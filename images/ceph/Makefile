# Copyright 2016 The Rook Authors. All rights reserved.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

include ../image.mk

# ====================================================================================
# Image Build Options

ifeq ($(GOARCH),amd64)
CEPH_VERSION = v16.2.5-20210708
else
CEPH_VERSION = v16.2.5-20210708
endif
REGISTRY_NAME = quay.io
BASEIMAGE = $(REGISTRY_NAME)/ceph/ceph-$(GOARCH):$(CEPH_VERSION)
CEPH_IMAGE = $(BUILD_REGISTRY)/ceph-$(GOARCH)
OPERATOR_SDK_VERSION = v0.17.1
# TODO: update to yq v4 - v3 end of life in Aug 2021 ; v4 removes the 'yq delete' cmd and changes syntax
YQ_VERSION = 3.3.0
GOHOST := GOOS=$(GOHOSTOS) GOARCH=$(GOHOSTARCH) go

TEMP := $(shell mktemp -d)

ifeq ($(HOST_PLATFORM),linux_amd64)
OPERATOR_SDK_PLATFORM = x86_64-linux-gnu
INCLUDE_CSV_TEMPLATES = true
endif
ifeq ($(HOST_PLATFORM),darwin_amd64)
OPERATOR_SDK_PLATFORM = x86_64-apple-darwin
INCLUDE_CSV_TEMPLATES = true
endif
ifneq ($(INCLUDE_CSV_TEMPLATES),true)
$(info )
$(info NOT INCLUDING OLM/CSV TEMPLATES!)
$(info )
endif

OPERATOR_SDK := $(TOOLS_HOST_DIR)/operator-sdk-$(OPERATOR_SDK_VERSION)
YQ := $(TOOLS_HOST_DIR)/yq-$(YQ_VERSION)
export OPERATOR_SDK YQ

# ====================================================================================
# Build Rook

do.build:
	@echo === container build $(CEPH_IMAGE)
	@cp Dockerfile $(TEMP)
	@cp toolbox.sh $(TEMP)
	@cp set-ceph-debug-level $(TEMP)
	@cp $(OUTPUT_DIR)/bin/linux_$(GOARCH)/rook $(TEMP)
	@cp $(OUTPUT_DIR)/bin/linux_$(GOARCH)/rookflex $(TEMP)
	@cp -r ../../cluster/examples/kubernetes/ceph/monitoring $(TEMP)/ceph-monitoring
	@mkdir -p $(TEMP)/rook-external/test-data
	@cp ../../cluster/examples/kubernetes/ceph/create-external-cluster-resources.* $(TEMP)/rook-external/
	@cp ../../cluster/examples/kubernetes/ceph/test-data/ceph-status-out $(TEMP)/rook-external/test-data/
ifeq ($(INCLUDE_CSV_TEMPLATES),true)
	@$(MAKE) CSV_TEMPLATE_DIR=$(TEMP) generate-csv-templates
	@cp -r $(TEMP)/cluster/olm/ceph/templates $(TEMP)/ceph-csv-templates
else
	mkdir $(TEMP)/ceph-csv-templates
endif
	@cd $(TEMP) && $(SED_IN_PLACE) 's|BASEIMAGE|$(BASEIMAGE)|g' Dockerfile
	@if [ -z "$(BUILD_CONTAINER_IMAGE)" ]; then\
		$(DOCKERCMD) build $(BUILD_ARGS) \
		--build-arg ARCH=$(GOARCH) \
		--build-arg TINI_VERSION=$(TINI_VERSION) \
		-t $(CEPH_IMAGE) \
		$(TEMP);\
	fi
	@rm -fr $(TEMP)

# generate CSV template files into the directory defined by the env var CSV_TEMPLATE_DIR
# CSV_TEMPLATE_DIR will be created if it doesn't already exist
generate-csv-templates: $(OPERATOR_SDK) $(YQ) ## Generate CSV templates for OLM into CSV_TEMPLATE_DIR
	@if [[ -z "$(CSV_TEMPLATE_DIR)" ]]; then echo "CSV_TEMPLATE_DIR is not set"; exit 1; fi
	@# first, copy the existing CRDs and OLM catalog directory to CSV_TEMPLATE_DIR
	@# then, generate or copy all prerequisites into CSV_TEMPLATE_DIR (e.g., CRDs)
	@# finally, generate the templates in-place using CSV_TEMPLATE_DIR as a staging dir
	@mkdir -p $(CSV_TEMPLATE_DIR)
	@cp -a ../../cluster $(CSV_TEMPLATE_DIR)/cluster
	@set -eE;\
	BEFORE_GEN_CRD_SIZE=$$(wc -l < ../../cluster/examples/kubernetes/ceph/crds.yaml);\
	$(MAKE) -C ../.. NO_OB_OBC_VOL_GEN=true MAX_DESC_LEN=0 BUILD_CRDS_INTO_DIR=$(CSV_TEMPLATE_DIR) crds;\
	AFTER_GEN_CRD_SIZE=$$(wc -l < $(CSV_TEMPLATE_DIR)/cluster/examples/kubernetes/ceph/crds.yaml);\
	if [ "$$BEFORE_GEN_CRD_SIZE" -le "$$AFTER_GEN_CRD_SIZE" ]; then\
		echo "the new crd file must be smaller since the description fields were stripped!";\
		echo "length before $$BEFORE_GEN_CRD_SIZE";\
		echo "length after $$AFTER_GEN_CRD_SIZE";\
		exit 1;\
	fi
	@OLM_CATALOG_DIR=$(CSV_TEMPLATE_DIR)/cluster/olm/ceph ../../cluster/olm/ceph/generate-rook-csv-templates.sh
	@echo " === Generated CSV templates can be found at $(CSV_TEMPLATE_DIR)/cluster/olm/ceph/templates"

$(YQ):
	@echo === installing yq $(GOHOST)
	@mkdir -p $(TOOLS_HOST_DIR)
	@curl -JL https://github.com/mikefarah/yq/releases/download/$(YQ_VERSION)/yq_$(HOST_PLATFORM) -o $(YQ)
	@chmod +x $(YQ)

$(OPERATOR_SDK):
	@echo === installing operator-sdk $(GOHOST)
	@mkdir -p $(TOOLS_HOST_DIR)
	@curl -JL -o $(TOOLS_HOST_DIR)/operator-sdk-$(OPERATOR_SDK_VERSION) \
		https://github.com/operator-framework/operator-sdk/releases/download/$(OPERATOR_SDK_VERSION)/operator-sdk-$(OPERATOR_SDK_VERSION)-$(OPERATOR_SDK_PLATFORM)
	@chmod +x $(OPERATOR_SDK)

csv: $(OPERATOR_SDK) $(YQ) ## Generate a CSV file for OLM.
	@echo Generating CSV manifests
	@cd ../.. && cluster/olm/ceph/generate-rook-csv.sh $(CSV_VERSION) $(CSV_PLATFORM) $(ROOK_OP_VERSION)

csv-clean: $(OPERATOR_SDK) $(YQ) ## Remove existing OLM files.
	@rm -fr ../../cluster/olm/ceph/deploy/* ../../cluster/olm/ceph/templates/*
