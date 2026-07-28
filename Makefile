GO        ?= go
BIN_DIR   := bin
VERSION   ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS   := -s -w -X main.version=$(VERSION)
KIND      ?= kind
KUBECTL   ?= kubectl
KIND_CLUSTER_NAME ?= kubeneuron-integration
KIND_KUBECONFIG   ?= /tmp/$(KIND_CLUSTER_NAME).kubeconfig

BINARIES := kubeneuron-operator kubeneuron-controller kubeneuron-agent kubeneuronctl

IMAGE_REPO ?= ghcr.io/kubeneuron/kube-neuron
IMAGE_TAG  ?= $(VERSION)
IMAGE_TARGETS := operator controller agent kubeneuronctl
# Dockerfile stage per published image name.
image_stage = $(if $(filter kubeneuronctl,$(1)),cli,$(1))
GENERATED_PATHS := api/v1alpha1/zz_generated.deepcopy.go config/crd/bases deploy/helm/kubeneuron/crds

.PHONY: all build test test-integration-kind kind-clean lint clean tidy generate verify-generate proto web docs docs-serve docker

all: build

build: $(BINARIES)

$(BINARIES): | $(BIN_DIR)
	CGO_ENABLED=0 $(GO) build -ldflags '$(LDFLAGS)' -o $(BIN_DIR)/$@ ./cmd/$@

$(BIN_DIR):
	mkdir -p $(BIN_DIR)

test:
	$(GO) test ./...

lint:
	$(GO) vet ./...
	@command -v golangci-lint >/dev/null 2>&1 && golangci-lint run ./... || echo "golangci-lint not installed, skipped"

tidy:
	$(GO) mod tidy

generate:
	$(GO) tool controller-gen object paths=./api/v1alpha1
	$(GO) tool controller-gen crd:allowDangerousTypes=true paths=./api/v1alpha1 output:crd:artifacts:config=config/crd/bases
	cp config/crd/bases/*.yaml deploy/helm/kubeneuron/crds/

verify-generate: generate
	@git diff --exit-code -- $(GENERATED_PATHS)
	@test -z "$$(git ls-files --others --exclude-standard -- $(GENERATED_PATHS))" || \
		(echo "untracked generated files found; run 'make generate' and commit the result" >&2; \
		 git ls-files --others --exclude-standard -- $(GENERATED_PATHS) >&2; \
		 exit 1)

test-integration-kind: build
	CLUSTER_NAME="$(KIND_CLUSTER_NAME)" \
	KUBECONFIG_PATH="$(KIND_KUBECONFIG)" \
	KIND_BIN="$(KIND)" KUBECTL_BIN="$(KUBECTL)" \
	./hack/kind-integration.sh

kind-clean:
	$(KIND) delete cluster --name "$(KIND_CLUSTER_NAME)"
	@echo "left kubeconfig untouched at $(KIND_KUBECONFIG); remove it only if you created it"

docker:
	$(foreach t,$(IMAGE_TARGETS),docker build -f build/Dockerfile \
		--build-arg VERSION=$(VERSION) \
		--target $(call image_stage,$(t)) \
		-t $(IMAGE_REPO)/$(t):$(IMAGE_TAG) . &&) true

proto:
	protoc --go_out=. --go_opt=paths=source_relative \
	       --go-grpc_out=. --go-grpc_opt=paths=source_relative \
	       api/proto/agent/v1/agent.proto

web:
	@echo "panel v1 is a zero-dependency static file (web/dist/index.html); no build step required"

docs:
	@command -v mkdocs >/dev/null || { echo "install mkdocs-material first: pip install mkdocs-material" >&2; exit 1; }
	mkdocs build --strict

docs-serve:
	mkdocs serve

clean:
	rm -rf $(BIN_DIR)
