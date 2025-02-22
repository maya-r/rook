/*
Copyright 2018 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package bucket

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/aws/aws-sdk-go/aws/awserr"
	"github.com/ceph/go-ceph/rgw/admin"
	"github.com/coreos/pkg/capnslog"
	bktv1alpha1 "github.com/kube-object-storage/lib-bucket-provisioner/pkg/apis/objectbucket.io/v1alpha1"
	apibkt "github.com/kube-object-storage/lib-bucket-provisioner/pkg/provisioner/api"
	opcontroller "github.com/rook/rook/pkg/operator/ceph/controller"
	"github.com/rook/rook/pkg/operator/ceph/object"
	storagev1 "k8s.io/api/storage/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/pkg/errors"
	"github.com/rook/rook/pkg/clusterd"
	"github.com/rook/rook/pkg/daemon/ceph/client"
	cephutil "github.com/rook/rook/pkg/daemon/ceph/util"
	cephObject "github.com/rook/rook/pkg/operator/ceph/object"
)

type Provisioner struct {
	context         *clusterd.Context
	objectContext   *cephObject.Context
	clusterInfo     *client.ClusterInfo
	bucketName      string
	storeDomainName string
	storePort       int32
	region          string
	// access keys for acct for the bucket *owner*
	cephUserName         string
	accessKeyID          string
	secretAccessKey      string
	objectStoreName      string
	endpoint             string
	additionalConfigData map[string]string
	tlsCert              []byte
	adminOpsClient       *admin.API
}

var _ apibkt.Provisioner = &Provisioner{}

func NewProvisioner(context *clusterd.Context, clusterInfo *client.ClusterInfo) *Provisioner {
	return &Provisioner{context: context, clusterInfo: clusterInfo}
}

// Provision creates an s3 bucket and returns a connection info
// representing the bucket's endpoint and user access credentials.
func (p Provisioner) Provision(options *apibkt.BucketOptions) (*bktv1alpha1.ObjectBucket, error) {
	logger.Debugf("Provision event for OB options: %+v", options)

	err := p.initializeCreateOrGrant(options)
	if err != nil {
		return nil, err
	}
	logger.Infof("Provision: creating bucket %q for OBC %q", p.bucketName, options.ObjectBucketClaim.Name)

	// dynamically create a new ceph user
	p.accessKeyID, p.secretAccessKey, err = p.createCephUser("")
	if err != nil {
		return nil, errors.Wrap(err, "Provision: can't create ceph user")
	}

	s3svc, err := cephObject.NewS3Agent(p.accessKeyID, p.secretAccessKey, p.getObjectStoreEndpoint(), logger.LevelAt(capnslog.DEBUG), p.tlsCert)
	if err != nil {
		p.deleteOBCResourceLogError("")
		return nil, err
	}

	// create the bucket
	err = s3svc.CreateBucket(p.bucketName)
	if err != nil {
		err = errors.Wrapf(err, "error creating bucket %q", p.bucketName)
		logger.Errorf(err.Error())
		p.deleteOBCResourceLogError("")
		return nil, err
	}

	singleBucketQuota := 1
	_, err = p.adminOpsClient.ModifyUser(context.TODO(), admin.User{ID: p.cephUserName, MaxBuckets: &singleBucketQuota})
	if err != nil {
		p.deleteOBCResourceLogError(p.bucketName)
		return nil, err
	}
	logger.Infof("set user %q bucket max to %d", p.cephUserName, singleBucketQuota)

	// setting quota limit if it is enabled
	err = p.setAdditionalSettings(options)
	if err != nil {
		p.deleteOBCResourceLogError(p.bucketName)
		return nil, err
	}

	return p.composeObjectBucket(), nil
}

// Grant attaches to an existing rgw bucket and returns a connection info
// representing the bucket's endpoint and user access credentials.
func (p Provisioner) Grant(options *apibkt.BucketOptions) (*bktv1alpha1.ObjectBucket, error) {
	logger.Debugf("Grant event for OB options: %+v", options)

	// initialize and set the AWS services and commonly used variables
	err := p.initializeCreateOrGrant(options)
	if err != nil {
		return nil, err
	}
	logger.Infof("Grant: allowing access to bucket %q for OBC %q", p.bucketName, options.ObjectBucketClaim.Name)

	// check and make sure the bucket exists
	logger.Infof("Checking for existing bucket %q", p.bucketName)
	if exists, err := p.bucketExists(p.bucketName); !exists {
		return nil, errors.Wrapf(err, "bucket %s does not exist", p.bucketName)
	}

	p.accessKeyID, p.secretAccessKey, err = p.createCephUser("")
	if err != nil {
		return nil, err
	}

	// need to quota into -1 for restricting creation of new buckets in rgw
	restrictBucketCreation := -1
	_, err = p.adminOpsClient.ModifyUser(context.TODO(), admin.User{ID: p.cephUserName, MaxBuckets: &restrictBucketCreation})
	if err != nil {
		p.deleteOBCResourceLogError("")
		return nil, err
	}

	// get the bucket's owner via the bucket metadata
	stats, err := p.adminOpsClient.GetBucketInfo(context.TODO(), admin.Bucket{Bucket: p.bucketName})
	if err != nil {
		p.deleteOBCResourceLogError("")
		return nil, errors.Wrapf(err, "failed to get bucket %q stats", p.bucketName)
	}

	objectUser, err := p.adminOpsClient.GetUser(context.TODO(), admin.User{ID: stats.Owner})
	if err != nil {
		p.deleteOBCResourceLogError("")
		return nil, errors.Wrapf(err, "failed to get user %q", stats.Owner)
	}

	s3svc, err := cephObject.NewS3Agent(objectUser.Keys[0].AccessKey, objectUser.Keys[0].SecretKey, p.getObjectStoreEndpoint(), logger.LevelAt(capnslog.DEBUG), p.tlsCert)
	if err != nil {
		p.deleteOBCResourceLogError("")
		return nil, err
	}

	// if the policy does not exist, we'll create a new and append the statement to it
	policy, err := s3svc.GetBucketPolicy(p.bucketName)
	if err != nil {
		if aerr, ok := err.(awserr.Error); ok {
			if aerr.Code() != "NoSuchBucketPolicy" {
				p.deleteOBCResourceLogError("")
				return nil, err
			}
		}
	}

	statement := cephObject.NewPolicyStatement().
		WithSID(p.cephUserName).
		ForPrincipals(p.cephUserName).
		ForResources(p.bucketName).
		ForSubResources(p.bucketName).
		Allows().
		Actions(cephObject.AllowedActions...)
	if policy == nil {
		policy = cephObject.NewBucketPolicy(*statement)
	} else {
		policy = policy.ModifyBucketPolicy(*statement)
	}
	out, err := s3svc.PutBucketPolicy(p.bucketName, *policy)

	logger.Infof("PutBucketPolicy output: %v", out)
	if err != nil {
		p.deleteOBCResourceLogError("")
		return nil, err
	}

	// setting quota limit if it is enabled
	err = p.setAdditionalSettings(options)
	if err != nil {
		p.deleteOBCResourceLogError("")
		return nil, err
	}

	// returned ob with connection info
	return p.composeObjectBucket(), nil
}

// Delete is called when the ObjectBucketClaim (OBC) is deleted and the associated
// storage class' reclaimPolicy is "Delete". Or, if a Provision() error occurs and
// the bucket controller needs to clean up before retrying.
func (p Provisioner) Delete(ob *bktv1alpha1.ObjectBucket) error {
	logger.Debugf("Delete event for OB: %+v", ob)

	err := p.initializeDeleteOrRevoke(ob)
	if err != nil {
		return err
	}
	logger.Infof("Delete: deleting bucket %q for OB %q", p.bucketName, ob.Name)

	if err := p.deleteOBCResource(p.bucketName); err != nil {
		return errors.Wrapf(err, "failed to delete OBCResource bucket %q", p.bucketName)
	}
	return nil
}

// Revoke removes a user and creds from an existing bucket.
// Note: cleanup order below matters.
func (p Provisioner) Revoke(ob *bktv1alpha1.ObjectBucket) error {
	logger.Debugf("Revoke event for OB: %+v", ob)

	err := p.initializeDeleteOrRevoke(ob)
	if err != nil {
		return err
	}
	logger.Infof("Revoke: denying access to bucket %q for OB %q", p.bucketName, ob.Name)

	bucket, err := p.adminOpsClient.GetBucketInfo(context.TODO(), admin.Bucket{Bucket: p.bucketName})
	if err != nil {
		logger.Errorf("%v", err)
	} else {
		if bucket.Owner == "" {
			return errors.Errorf("failed to find bucket %q owner", p.bucketName)
		}

		user, err := p.adminOpsClient.GetUser(context.TODO(), admin.User{ID: bucket.Owner})
		if err != nil {
			if errors.Is(err, admin.ErrNoSuchUser) {
				// The user may not exist. Ignore this in order to ensure the PolicyStatement does not contain the
				// stale user.
				return nil
			}

			return err
		}

		s3svc, err := cephObject.NewS3Agent(user.Keys[0].AccessKey, user.Keys[0].SecretKey, p.getObjectStoreEndpoint(), logger.LevelAt(capnslog.DEBUG), p.tlsCert)
		if err != nil {
			return err
		}

		// Ignore cases where there is no bucket policy. This may have occurred if an error ended a Grant()
		// call before the policy was attached to the bucket
		policy, err := s3svc.GetBucketPolicy(p.bucketName)
		if err != nil {
			if aerr, ok := err.(awserr.Error); ok && aerr.Code() == "NoSuchBucketPolicy" {
				policy = nil
				logger.Errorf("no bucket policy for bucket %q, so no need to drop policy", p.bucketName)

			} else {
				logger.Errorf("error getting policy for bucket %q. %v", p.bucketName, err)
				return err
			}
		}

		if bucket.Owner == p.cephUserName {
			statement := cephObject.NewPolicyStatement().
				WithSID(p.cephUserName).
				ForPrincipals(p.cephUserName).
				ForResources(p.bucketName).
				ForSubResources(p.bucketName).
				Denies().
				Actions(cephObject.AllowedActions...)
			if policy == nil {
				policy = cephObject.NewBucketPolicy(*statement)
			} else {
				policy = policy.ModifyBucketPolicy(*statement)
			}
			out, err := s3svc.PutBucketPolicy(p.bucketName, *policy)
			logger.Infof("PutBucketPolicy output: %v", out)
			if err != nil {
				return errors.Wrap(err, "failed to update policy")
			} else {
				return nil
			}
		}

		// drop policy if present
		if policy != nil {
			policy = policy.DropPolicyStatements(p.cephUserName)
			_, err := s3svc.PutBucketPolicy(p.bucketName, *policy)
			if err != nil {
				return err
			}
			logger.Infof("principal %q ejected from bucket %q policy", p.cephUserName, p.bucketName)
		}
	}

	// finally, delete the user
	p.deleteOBCResourceLogError("")
	return nil
}

// Return the OB struct with minimal fields filled in.
// initializeCreateOrGrant sets common provisioner receiver fields and
// the services and sessions needed to provision.
func (p *Provisioner) initializeCreateOrGrant(options *apibkt.BucketOptions) error {
	logger.Info("initializing and setting CreateOrGrant services")

	// set the bucket name
	obc := options.ObjectBucketClaim
	scName := options.ObjectBucketClaim.Spec.StorageClassName
	sc, err := p.getStorageClassWithBackoff(scName)
	if err != nil {
		logger.Errorf("failed to get storage class for OBC %q in namespace %q. %v", obc.Name, obc.Namespace, err)
		return err
	}

	// In most cases we assume the bucket is to be generated dynamically.  When a storage class
	// defines the bucket in the parameters, it's assumed to be a request to connect to a statically
	// created bucket.  In these cases, we forego generating a bucket.  Instead we connect a newly generated
	// user to the existing bucket.
	p.setBucketName(options.BucketName)
	if bucketName, isStatic := isStaticBucket(sc); isStatic {
		p.setBucketName(bucketName)
	}

	p.setObjectStoreName(sc)
	p.setRegion(sc)
	p.setAdditionalConfigData(obc.Spec.AdditionalConfig)
	p.setEndpoint(sc)
	err = p.setObjectContext()
	if err != nil {
		return err
	}

	// If an endpoint is declared let's use it
	err = p.populateDomainAndPort(sc)
	if err != nil {
		return errors.Wrap(err, "failed to set domain and port")
	}
	err = p.setTlsCaCert()
	if err != nil {
		return errors.Wrapf(err, "failed to set CA cert for the OBC %q to connect with object store %q via TLS", obc.Name, p.objectStoreName)
	}

	// Set admin ops api client
	err = p.setAdminOpsAPIClient()
	if err != nil {
		// Replace the error with a nicer more comprehensive one
		// If the ceph config is not initialized yet, the radosgw-admin command will fail to retrieve the user
		if strings.Contains(err.Error(), opcontroller.OperatorNotInitializedMessage) {
			return errors.New(opcontroller.OperatorNotInitializedMessage)
		}
		return errors.Wrap(err, "failed to set admin ops api client")
	}

	return nil
}

func (p *Provisioner) initializeDeleteOrRevoke(ob *bktv1alpha1.ObjectBucket) error {

	sc, err := p.getStorageClassWithBackoff(ob.Spec.StorageClassName)
	if err != nil {
		return errors.Wrapf(err, "failed to get storage class for OB %q", ob.Name)
	}

	// set receiver fields from OB data
	p.setBucketName(getBucketName(ob))
	p.cephUserName = getCephUser(ob)
	p.objectStoreName = getObjectStoreName(sc)
	p.setEndpoint(sc)
	err = p.setObjectContext()
	if err != nil {
		return err
	}

	err = p.populateDomainAndPort(sc)
	if err != nil {
		return err
	}

	err = p.setTlsCaCert()
	if err != nil {
		return errors.Wrapf(err, "failed to set CA cert for the OB %q to connect with object store %q via TLS", ob.Name, p.objectStoreName)
	}

	// Set admin ops api client
	err = p.setAdminOpsAPIClient()
	if err != nil {
		// Replace the error with a nicer more comprehensive one
		// If the ceph config is not initialized yet, the radosgw-admin command will fail to retrieve the user
		if strings.Contains(err.Error(), opcontroller.OperatorNotInitializedMessage) {
			return errors.New(opcontroller.OperatorNotInitializedMessage)
		}
		return errors.Wrap(err, "failed to set admin ops api client")
	}

	return nil
}

// Return the OB struct with minimal fields filled in.
func (p *Provisioner) composeObjectBucket() *bktv1alpha1.ObjectBucket {

	conn := &bktv1alpha1.Connection{
		Endpoint: &bktv1alpha1.Endpoint{
			BucketHost:           p.storeDomainName,
			BucketPort:           int(p.storePort),
			BucketName:           p.bucketName,
			Region:               p.region,
			AdditionalConfigData: p.additionalConfigData,
		},
		Authentication: &bktv1alpha1.Authentication{
			AccessKeys: &bktv1alpha1.AccessKeys{
				AccessKeyID:     p.accessKeyID,
				SecretAccessKey: p.secretAccessKey,
			},
		},
		AdditionalState: map[string]string{
			cephUser: p.cephUserName,
		},
	}

	return &bktv1alpha1.ObjectBucket{
		Spec: bktv1alpha1.ObjectBucketSpec{
			Connection: conn,
		},
	}
}

func (p *Provisioner) setObjectContext() error {
	msg := "error building object.Context: store %s cannot be empty"
	// p.endpoint means we point to an external cluster
	if p.objectStoreName == "" && p.endpoint == "" {
		return errors.Errorf(msg, "name")
	}

	// We don't need the CephObjectStore if an endpoint is provided
	// In 1.3, OBC external is working with an Endpoint (from the SC param) and in 1.4 we have a CephObjectStore but we must keep backward compatibility
	// In 1.4, the Endpoint from the SC is not expected and never used so we will enter the "else" condition which gets a CephObjectStore and it is present
	if p.endpoint != "" {
		p.objectContext = cephObject.NewContext(p.context, p.clusterInfo, p.objectStoreName)
	} else {
		// Get CephObjectStore
		store, err := p.getObjectStore()
		if err != nil {
			return errors.Wrap(err, "failed to get cephObjectStore")
		}

		// Set multisite context
		p.objectContext, err = cephObject.NewMultisiteContext(p.context, p.clusterInfo, store)
		if err != nil {
			return errors.Wrap(err, "failed to set multisite on provisioner's objectContext")
		}
	}

	return nil
}

// setObjectStoreDomainName sets the provisioner.storeDomainName and provisioner.port
// must be called after setObjectStoreName and setObjectStoreNamespace
func (p *Provisioner) setObjectStoreDomainName(sc *storagev1.StorageClass) error {

	name := getObjectStoreName(sc)
	namespace := getObjectStoreNameSpace(sc)
	// make sure the object store actually exists
	_, err := p.getObjectStore()
	if err != nil {
		return err
	}
	p.storeDomainName = cephObject.BuildDomainName(name, namespace)
	return nil
}

func (p *Provisioner) setObjectStorePort() error {
	store, err := p.getObjectStore()
	if err != nil {
		return errors.Wrap(err, "failed to get cephObjectStore")
	}
	p.storePort, err = store.Spec.GetPort()
	return err
}

func (p *Provisioner) setObjectStoreName(sc *storagev1.StorageClass) {
	p.objectStoreName = sc.Parameters[objectStoreName]
}

func (p *Provisioner) setBucketName(name string) {
	p.bucketName = name
}

func (p *Provisioner) setAdditionalConfigData(additionalConfigData map[string]string) {
	if len(additionalConfigData) == 0 {
		additionalConfigData = make(map[string]string)
	}
	p.additionalConfigData = additionalConfigData
}

func (p *Provisioner) setEndpoint(sc *storagev1.StorageClass) {
	p.endpoint = sc.Parameters[objectStoreEndpoint]
}

func (p *Provisioner) setRegion(sc *storagev1.StorageClass) {
	const key = "region"
	p.region = sc.Parameters[key]
}

func (p Provisioner) getObjectStoreEndpoint() string {
	return fmt.Sprintf("%s:%d", p.storeDomainName, p.storePort)
}

func (p *Provisioner) populateDomainAndPort(sc *storagev1.StorageClass) error {
	endpoint := getObjectStoreEndpoint(sc)
	// if endpoint is present, let's introspect it
	if endpoint != "" {
		p.storeDomainName = cephutil.GetIPFromEndpoint(endpoint)
		if p.storeDomainName == "" {
			return errors.New("failed to discover endpoint IP (is empty)")
		}
		p.storePort = cephutil.GetPortFromEndpoint(endpoint)
		if p.storePort == 0 {
			return errors.New("failed to discover endpoint port (is empty)")
		}
		// If no endpoint exists let's see if CephObjectStore exists
	} else {
		if err := p.setObjectStoreDomainName(sc); err != nil {
			return errors.Wrap(err, "failed to set object store domain name")
		}
		if err := p.setObjectStorePort(); err != nil {
			return errors.Wrap(err, "failed to set object store port")
		}
	}

	return nil
}

func (p *Provisioner) deleteOBCResourceLogError(bucketname string) {
	if err := p.deleteOBCResource(bucketname); err != nil {
		logger.Warningf("failed to delete OBC resource. %v", err)
	}
}

// Check for additional options mentioned in OBC and set them accordingly
func (p Provisioner) setAdditionalSettings(options *apibkt.BucketOptions) error {
	quotaEnabled := true
	maxObjects := MaxObjectQuota(options.ObjectBucketClaim.Spec.AdditionalConfig)
	maxSize := MaxSizeQuota(options.ObjectBucketClaim.Spec.AdditionalConfig)
	if maxObjects == "" && maxSize == "" {
		return nil
	}

	// Enabling quota for the user
	err := p.adminOpsClient.SetUserQuota(context.TODO(), admin.QuotaSpec{UID: p.cephUserName, Enabled: &quotaEnabled})
	if err != nil {
		return errors.Wrapf(err, "failed to enable user %q quota for obc", p.cephUserName)
	}

	if maxObjects != "" {
		maxObjectsInt, err := strconv.Atoi(maxObjects)
		if err != nil {
			return errors.Wrap(err, "failed to convert maxObjects to integer")
		}
		maxObjectsInt64 := int64(maxObjectsInt)
		err = p.adminOpsClient.SetUserQuota(context.TODO(), admin.QuotaSpec{UID: p.cephUserName, MaxObjects: &maxObjectsInt64})
		if err != nil {
			return errors.Wrapf(err, "failed to set MaxObject to user %q", p.cephUserName)
		}
	}
	if maxSize != "" {
		maxSizeInt, err := maxSizeToInt64(maxSize)
		if err != nil {
			return errors.Wrapf(err, "failed to parse maxSize quota for user %q", p.cephUserName)
		}
		err = p.adminOpsClient.SetUserQuota(context.TODO(), admin.QuotaSpec{UID: p.cephUserName, MaxSize: &maxSizeInt})
		if err != nil {
			return errors.Wrapf(err, "failed to set MaxSize to user %q", p.cephUserName)
		}
	}

	return nil
}

func maxSizeToInt64(maxSize string) (int64, error) {
	maxSizeInt, err := resource.ParseQuantity(maxSize)
	if err != nil {
		return 0, errors.Wrap(err, "failed to parse quantity")
	}

	return maxSizeInt.Value(), nil
}

func (p *Provisioner) setTlsCaCert() error {
	objStore, err := p.getObjectStore()
	if err != nil {
		return err
	}
	p.tlsCert = make([]byte, 0)
	if objStore.Spec.Gateway.SecurePort == p.storePort {
		p.tlsCert, err = cephObject.GetTlsCaCert(p.objectContext, &objStore.Spec)
		if err != nil {
			return err
		}
	}

	return nil
}

func (p *Provisioner) setAdminOpsAPIClient() error {
	// Build TLS transport for the HTTP client if needed
	httpClient := &http.Client{
		Timeout: cephObject.HttpTimeOut,
	}
	if p.tlsCert != nil {
		httpClient.Transport = cephObject.BuildTransportTLS(p.tlsCert)
	}

	// Fetch the ceph object store
	cephObjectStore, err := p.getObjectStore()
	if err != nil {
		return errors.Wrapf(err, "failed to get ceph object store %q", p.objectStoreName)
	}

	cephCluster, err := p.getCephCluster()
	if err != nil {
		return errors.Wrapf(err, "failed to get ceph cluster in namespace %q", p.clusterInfo.Namespace)
	}
	if cephCluster == nil {
		return errors.Errorf("failed to read ceph cluster in namespace %q, it's nil", p.clusterInfo.Namespace)
	}
	// Set the Ceph Cluster Spec so that we can fetch the admin ops key properly when multus is enabled
	p.objectContext.CephClusterSpec = cephCluster.Spec

	// Fetch the object store admin ops user
	accessKey, secretKey, err := cephObject.GetAdminOPSUserCredentials(p.objectContext, &cephObjectStore.Spec)
	if err != nil {
		return errors.Wrap(err, "failed to retrieve rgw admin ops user")
	}

	// Build endpoint
	s3endpoint := cephObject.BuildDNSEndpoint(cephObject.BuildDomainName(p.objectContext.Name, cephObjectStore.Namespace), p.storePort, cephObjectStore.Spec.IsTLSEnabled())

	// If DEBUG level is set we will mutate the HTTP client for printing request and response
	if logger.LevelAt(capnslog.DEBUG) {
		p.adminOpsClient, err = admin.New(s3endpoint, accessKey, secretKey, object.NewDebugHTTPClient(httpClient, logger))
		if err != nil {
			return errors.Wrap(err, "failed to build admin ops API connection")
		}
	} else {
		p.adminOpsClient, err = admin.New(s3endpoint, accessKey, secretKey, httpClient)
		if err != nil {
			return errors.Wrap(err, "failed to build admin ops API connection")
		}
	}

	return nil
}
func (p Provisioner) updateAdditionalSettings(ob *bktv1alpha1.ObjectBucket) error {
	var maxObjectsInt64 int64
	var maxSizeInt64 int64
	var err error
	var quotaEnabled bool
	maxObjects := MaxObjectQuota(ob.Spec.Endpoint.AdditionalConfigData)
	maxSize := MaxSizeQuota(ob.Spec.Endpoint.AdditionalConfigData)
	if maxObjects != "" {
		maxObjectsInt, err := strconv.Atoi(maxObjects)
		if err != nil {
			return errors.Wrap(err, "failed to convert maxObjects to integer")
		}
		maxObjectsInt64 = int64(maxObjectsInt)
	}
	if maxSize != "" {
		maxSizeInt64, err = maxSizeToInt64(maxSize)
		if err != nil {
			return errors.Wrapf(err, "failed to parse maxSize quota for user %q", p.cephUserName)
		}
	}
	objectUser, err := p.adminOpsClient.GetUser(context.TODO(), admin.User{ID: ob.Spec.Connection.AdditionalState[cephUser]})
	if err != nil {
		return errors.Wrapf(err, "failed to fetch user %q", p.cephUserName)
	}
	if *objectUser.UserQuota.Enabled &&
		(maxObjects == "" || maxObjectsInt64 < 0) &&
		(maxSize == "" || maxSizeInt64 < 0) {
		quotaEnabled = false
		err = p.adminOpsClient.SetUserQuota(context.TODO(), admin.QuotaSpec{UID: p.cephUserName, Enabled: &quotaEnabled})
		if err != nil {
			return errors.Wrapf(err, "failed to disable quota to user %q", p.cephUserName)
		}
		return nil
	}

	quotaEnabled = true
	quotaSpec := admin.QuotaSpec{UID: p.cephUserName, Enabled: &quotaEnabled}

	//MaxObject is modified
	if maxObjects != "" && (maxObjectsInt64 != *objectUser.UserQuota.MaxObjects) {
		quotaSpec.MaxObjects = &maxObjectsInt64
	}

	//MaxSize is modified
	if maxSize != "" && (maxSizeInt64 != *objectUser.UserQuota.MaxSize) {
		quotaSpec.MaxSize = &maxSizeInt64
	}
	err = p.adminOpsClient.SetUserQuota(context.TODO(), quotaSpec)
	if err != nil {
		return errors.Wrapf(err, "failed to update quota to user %q", p.cephUserName)
	}

	return nil
}

// Update is sent when only there is modification to AdditionalConfig field in OBC
func (p Provisioner) Update(ob *bktv1alpha1.ObjectBucket) error {
	logger.Debugf("Update event for OB: %+v", ob)

	err := p.initializeDeleteOrRevoke(ob)
	if err != nil {
		return err
	}

	return p.updateAdditionalSettings(ob)
}
