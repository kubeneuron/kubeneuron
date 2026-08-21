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

.PHONY: all build test test-integration-kind kind-clean lint clean tidy generate verify-generate proto web docs docs-serve docker gates gates-full verify-docs verify-image mirror

all: build

build: $(BINARIES)

$(BINARIES): | $(BIN_DIR)
	CGO_ENABLED=0 $(GO) build -ldflags '$(LDFLAGS)' -o $(BIN_DIR)/$@ ./cmd/$@

$(BIN_DIR):
	mkdir -p $(BIN_DIR)

test:
	$(GO) test ./...

# --- gates -------------------------------------------------------------------
#
# The gate set used to live only as CI job steps and as prose in a checkpoint
# document. That was survivable while every push ran CI; it stopped being so
# when Actions was disabled on the private development repository, because the
# public repository now sees one squashed commit per release — CI runs after
# every decision has already been made.
#
# Two tiers, deliberately. Bundling a four-second check with a forty-minute one
# is how the four-second check stops being run: `gates` is fast enough to run
# before every commit, `gates-full` is what a release deserves.

## gates: everything that finishes in minutes. Run before every commit.
gates:
	$(MAKE) verify-generate
	$(GO) build ./...
	$(MAKE) lint
# -timeout 30m, not the default 10m. internal/controller under -race takes
# ~185s on an idle box; this one also runs kind clusters, docker builds and
# hardware E2E, and a package that crosses 600s dies with "test timed out" and
# passes on a quiet re-run. That is the signature of the two unreproducible
# gate failures nobody could explain — 3.2x headroom is not headroom here.
	$(GO) test -race -timeout 30m ./...
	$(MAKE) verify-docs
	@echo
	@echo "gates: OK (generate, build, lint, race tests, docs)"
	@echo "note: the PostgreSQL conformance suite and the kind/image/release"
	@echo "      gates are in 'make gates-full' — this tier proves the code,"
	@echo "      not the artifact."

## gates-full: gates plus everything that needs Docker or a cluster.
gates-full: gates
	$(MAKE) verify-image
	$(MAKE) test-integration-kind
	@echo
	@echo "gates-full: OK (code gates, published images, kind integration)"

verify-docs:
	bash hack/verify-docs.sh

verify-image:
	bash hack/verify-image.sh

## mirror: publish this tree to the public repository. See hack/mirror.sh.
mirror:
	bash hack/mirror.sh

lint:
	$(GO) vet ./...
	@command -v golangci-lint >/dev/null 2>&1 && golangci-lint run ./... || echo "golangci-lint not installed, skipped"
# actionlint (with its embedded shellcheck) is the one CI linter that golangci-lint
# does not cover, and workflow YAML is the one place a defect cannot be caught by
# running the code. It is pinned to the same version CI uses.
#
# Run from the module's own tool dependency, not `go run …@v1.7.7`: the @version
# form is the only step in `gates` that reaches the network, so a cold module
# cache or a proxy hiccup fails the whole gate and a retry passes. That is the
# other half of the unreproducible-failure signature.
	$(GO) tool actionlint
# actionlint's shellcheck reaches `run:` blocks only. Everything load-bearing in
# hack/ is a standalone script it never sees — the mirror, the release check,
# the image check, the hardware stand — and those are exactly the scripts whose
# job is to refuse. hack/mirror.sh carried a dead array for as long as it
# existed (SC2034), so its drift assertion checked one direction while its
# header claimed two; nothing reported it because nothing ran shellcheck here.
	@command -v shellcheck >/dev/null 2>&1 && shellcheck hack/*.sh deploy/install.sh \
		|| echo "shellcheck not installed, skipped"

tidy:
	$(GO) mod tidy

generate:
	$(GO) tool controller-gen object paths=./api/v1alpha1
	$(GO) tool controller-gen crd:allowDangerousTypes=true paths=./api/v1alpha1 output:crd:artifacts:config=config/crd/bases
	cp config/crd/bases/*.yaml deploy/helm/kubeneuron/crds/

# NOT SAFE TO RUN CONCURRENTLY WITH ITSELF, and that is the best explanation
# anyone has for the unreproducible `make gates` failures.
#
# It runs `generate`, which REWRITES tracked files, and then asserts they are
# unchanged. Two `make gates` on one working tree — a reviewer's and yours, say
# — interleave a rewrite with the other's `git diff`, and one of them fails on
# a file the other was mid-write. It passes on a re-run because by then nothing
# else is writing.
#
# 32 clean runs of `go test -race ./...` (25 here, 7 by a reviewer) failed to
# reproduce anything in the tests, while every observed gate failure coincided
# with a second agent working in this tree. One `make gates` at a time.
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
