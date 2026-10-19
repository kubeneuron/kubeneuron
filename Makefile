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
GENERATED_PATHS := api/v1alpha1/zz_generated.deepcopy.go config/crd/bases deploy/helm/kubeneuron/crds config/gpu-metrics docs/reference-gpu-metrics.md
# The lock verify-generate serialises on. In .gitignore; created on first use.
GENERATE_LOCK := .make-generate.lock

.PHONY: all build test test-integration-kind test-upgrade-rollback-kind kind-clean lint clean tidy generate verify-generate verify-generate-locked generate-gpucatalog verify-gpucatalog proto web docs docs-serve docker gates gates-full verify-docs verify-image verify-hw-e2e-sweep verify-hw-e2e-observability verify-tls-scripts mirror test-gpu-observability-kind verify-gpu-observability-fixture kind-gpu-observability-clean

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
	$(MAKE) verify-hw-e2e-sweep
	$(MAKE) verify-hw-e2e-observability
	$(MAKE) verify-tls-scripts
	@echo
	@echo "gates: OK (generate, build, lint, race tests, docs, hw-e2e sweep, hw-e2e observability, TLS helpers)"
	@echo "note: the PostgreSQL conformance suite and the kind/image/release"
	@echo "      gates are in 'make gates-full' — this tier proves the code,"
	@echo "      not the artifact."

## gates-full: gates plus everything that needs Docker or a cluster.
# Order: the offline image check first, then the three disposable kind
# clusters from cheapest to dearest. test-integration-kind is fully offline.
# test-gpu-observability-kind is the v0.7 GPU observability fixture: its own
# CPU-only kind cluster with a SYNTHETIC dcgm-exporter stand-in, scraped and
# evaluated by the pinned VictoriaMetrics v1.147.0 pipeline, routed through
# the shipped Alertmanager route tree into recorder sinks, and provisioned
# into the pinned Grafana; it pulls those images from Docker Hub. It covers
# the G2 rows for recording rules, label contract, missing data, fail-slow,
# shadow routing and dashboard provisioning on fabricated series, never
# hardware or G2 as a whole.
# test-upgrade-rollback-kind pulls the published baseline images from GHCR
# and is the slowest, so it goes last.
gates-full: gates
	$(MAKE) verify-image
	$(MAKE) test-integration-kind
	$(MAKE) test-gpu-observability-kind
	$(MAKE) test-upgrade-rollback-kind
	@echo
	@echo "gates-full: OK (code gates, published images, kind integration, synthetic GPU observability fixture, upgrade/rollback rehearsal)"
	@echo "note: the GPU observability fixture is a separate CPU-only kind validation"
	@echo "      with fabricated DCGM series; it is not a hardware result and not G2."

verify-docs:
	bash hack/verify-docs.sh

verify-image:
	bash hack/verify-image.sh

# The hardware sweep's networking cleanup, run against a scripted aws/eksctl
# (hack/hw-e2e-fake-aws.py). No AWS, no cluster, a few seconds: it is the only
# deterministic proof that the sweep removes exactly this run's orphaned VPC-CNI
# interface and EKS cluster security group, and refuses everything else — a live
# cluster's interface, another cluster's group, an ambient or default VPC.
verify-hw-e2e-sweep:
	bash hack/verify-hw-e2e-sweep.sh

# Offline check of the hardware harness's v0.7 GPU operator profile wiring
# (pinned --values, v26.3.3 default, fail-closed pre-Helm gate), the
# read-only test-observability phase (dispatch, target guard, bounded curl,
# T0/T1 witness fields, node/gpu_uuid/gpu_model/node_pool label contract,
# Tesla T4 absence checks, VMSingle port read from the Service, reaped EXIT
# cleanup) and the deploy-observability phase that installs the stack it
# reads (workflow order, VictoriaMetrics operator pin equal to
# versions.lock.yaml, no remote apply, the README's install order behind
# bounded gates, the fail-closed webhook-token copy, the swept VMSingle
# claim; the REAL phase run against scripted aws/kubectl/curl refuses lock
# drift, a checksum mismatch, a missing root and a Service port drift, and
# applies everything in order on the green path), plus the workflow's own
# kubectl pin (minor equal to the EKS K8S_VERSION, version and SHA-256
# equal to the reviewed pair the verifier records, verified before
# chmod/PATH; the install step is run against a scripted curl). No cluster,
# seconds. It proves wiring, not hardware behaviour.
verify-hw-e2e-observability:
	bash hack/verify-hw-e2e-observability.sh

# Offline regression coverage for the TLS helpers' optimistic-concurrency path
# (hack/tls-rotate.sh, hack/tls-emergency-recover.sh) against a scripted kubectl
# that enforces the resourceVersion compare-and-swap and injects a concurrent
# writer: status-only conflicts are retried, exact already-applied state is
# accepted, and every TLS-relevant drift (foreign transaction, UID lookalike,
# spec change, root turned Ready mid-recovery) fails closed. A live kind run
# lost this race once; no cluster, a few seconds.
verify-tls-scripts:
	bash hack/verify-tls-scripts.sh

## mirror: publish this tree to the public repository. See hack/mirror.sh.
mirror:
	bash hack/mirror.sh

lint:
	$(GO) vet ./...
# An explicit if/else, not `command -v X && X || echo skipped`. The && chain
# returns X's exit status into `|| echo`, so a real lint FAILURE printed
# "not installed, skipped" and the gate passed — golangci-lint reported four
# issues under a green `make lint`. Same fix applies to shellcheck below.
	@if command -v golangci-lint >/dev/null 2>&1; then \
		golangci-lint run ./...; \
	else \
		echo "golangci-lint not installed, skipped"; \
	fi
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
	@if command -v shellcheck >/dev/null 2>&1; then \
		shellcheck hack/*.sh deploy/install.sh; \
	else \
		echo "shellcheck not installed, skipped"; \
	fi
# No unbounded network call in the scripts that spend money.
#
# The hardware stand's cost guarantee has now died twice on a cleanup step that
# could not fail: once on a teardown piped through tee, and once on a curl with
# no --max-time, which hung an EXIT trap for two hours with a GPU cluster still
# billing. A port-forward keeps its local listener up after the remote end dies,
# so the connect succeeds and the read never returns — the failure looks exactly
# like a slow test.
#
# Scoped to hw-e2e.sh, and the scope is the honest limit of a grep.
#
# That is the only script that holds a PAID cluster, and every curl in it is
# written with its flags inline, so a line-oriented check can actually prove
# something there. The kind scripts build their flags in arrays
# (valid_tls=(... --max-time 10)) and kind-upgrade.sh wraps curl in api_curl —
# both bounded, neither visible to grep on the invocation line. Widening this to
# hack/*.sh produced five false positives and zero real ones, which is how a
# guard gets deleted six months later.
#
# deploy/install.sh is bounded too (a hung install costs patience, not dollars)
# but is not what this guards.
#
# Cheap, mechanical, and it fails the build rather than a paid run.
	@! grep -nEw 'curl' hack/hw-e2e.sh \
		| grep -v -- '--max-time' | grep -vE ':[[:space:]]*#|require_cmd|curlimages/' | grep . \
		|| (echo "curl without --max-time in hack/hw-e2e.sh, which holds a paid cluster:" >&2; \
		    grep -nEw 'curl' hack/hw-e2e.sh \
		      | grep -v -- '--max-time' | grep -vE ':[[:space:]]*#|require_cmd|curlimages/' >&2; \
		    exit 1)

tidy:
	$(GO) mod tidy

generate:
	$(GO) tool controller-gen object paths=./api/v1alpha1
	$(GO) tool controller-gen crd:allowDangerousTypes=true paths=./api/v1alpha1 output:crd:artifacts:config=config/crd/bases
	cp config/crd/bases/*.yaml deploy/helm/kubeneuron/crds/
	$(MAKE) generate-gpucatalog

# Serialised, because it REWRITES tracked files and then asserts they are
# unchanged. Two of these on one working tree can in principle interleave one's
# rewrite with the other's `git diff`.
#
# Read that as removing a hazard, NOT as the fix for a known bug. `make gates`
# has failed three times unreproducibly on this machine and the cause is still
# unknown. What has been ruled out: 32 clean runs of `go test -race ./...` (25
# in one sitting, 7 by a reviewer) reproduced nothing in the tests; actionlint's
# network fetch is gone; the 10-minute per-package timeout is gone; no OOM kill
# appears in the kernel log. This hypothesis fitted — every observed failure
# coincided with a second agent working in the tree — but six concurrent
# unserialised pairs did not reproduce it either, so it is a hypothesis that
# survives rather than one that was confirmed.
#
# The lock is cheap and removes the class without changing what generation does.
# controller-gen's `object` mode writes zz_generated.deepcopy.go beside its
# source and takes no output directory, so generating into a scratch tree and
# comparing would mean copying the module — more machinery than the problem is
# worth. Serialising costs nothing when nobody else is running.
#
# flock is not everywhere (macOS ships none), so its absence degrades to the
# old behaviour with a warning rather than failing the gate.
verify-generate:
	@if command -v flock >/dev/null 2>&1; then \
		flock "$(GENERATE_LOCK)" $(MAKE) verify-generate-locked; \
	else \
		echo "flock not found; running verify-generate unserialised (do not run two at once)" >&2; \
		$(MAKE) verify-generate-locked; \
	fi

verify-generate-locked: generate
	@git diff --exit-code -- $(GENERATED_PATHS)
	@test -z "$$(git ls-files --others --exclude-standard -- $(GENERATED_PATHS))" || \
		(echo "untracked generated files found; run 'make generate' and commit the result" >&2; \
		 git ls-files --others --exclude-standard -- $(GENERATED_PATHS) >&2; \
		 exit 1)

# --- GPU metric catalog -------------------------------------------------------
#
# internal/gpucatalog is the source of truth; config/gpu-metrics/ and
# docs/reference-gpu-metrics.md are rendered from it. `make generate` runs
# generate-gpucatalog and both artifacts are in GENERATED_PATHS, so the
# standard verify-generate / gates path fails on catalog drift. The render is
# a pure function of the Go tables, so verify-gpucatalog needs no lock: it
# compares an in-memory render against the checked-in files and rewrites
# nothing.
generate-gpucatalog:
	$(GO) run ./hack/gpucatalog

verify-gpucatalog:
	$(GO) run ./hack/gpucatalog -check

test-integration-kind: build
	CLUSTER_NAME="$(KIND_CLUSTER_NAME)" \
	KUBECONFIG_PATH="$(KIND_KUBECONFIG)" \
	KIND_BIN="$(KIND)" KUBECTL_BIN="$(KUBECTL)" \
	./hack/kind-integration.sh

# The v0.6.1 -> HEAD -> v0.6.1 (images only) -> HEAD rehearsal from
# docs/upgrade.md, on one disposable kind cluster, for BOTH stores at once
# (SQLite PVC and a throwaway in-cluster PostgreSQL). It seeds a baseline
# incident, drives a v0.5 runtime contract qualification to ReadyForApproval
# on HEAD, enables and disables the v0.6 checkpoint coordination policy and
# proves the managed controller ClusterRole holds pods patch only while it is
# enabled, rolls the images back with the policy off, proves the v0.6.1
# binary reads the rows back byte-identical, re-upgrades, and proves the rows
# and their hash-chained audit read back byte-identical and still accept
# observations. CPU-only: synthetic accelerator evidence over
# the real agent identity, no GPU claim. Pulls the published baseline images
# from GHCR, so it is the one kind gate that is not offline. Deletes its
# cluster on exit unless KEEP_CLUSTER=1. Docker group membership required
# (sg docker -c 'make test-upgrade-rollback-kind').
test-upgrade-rollback-kind:
	KIND_BIN="$(KIND)" KUBECTL_BIN="$(KUBECTL)" \
	./hack/kind-upgrade-rollback.sh

# v0.7 GPU observability fixture on its own disposable kind cluster
# (kubeneuron-gpu-observability): a synthetic dcgm-exporter stand-in
# (hack/dcgmfixture, CPU-only, no DCGM/NVML) scraped by vmagent with the
# checked-in M2 relabeling into vmsingle; vmalert evaluating
# configs/vmalert/gpu-recording-rules.yaml and gpu-shadow-rules.yaml
# byte-for-byte; Alertmanager v0.32.1 running the route tree of the
# checked-in VMAlertmanager verbatim (the integration-less human-default
# receiver gets one test-only webhook) in front of two recorder sinks
# (hack/alertsink), the controller one under the shipped receiver's own
# Service name, namespace, port and token file; and Grafana (tag+digest from
# versions.lock.yaml) provisioning the five deploy/grafana boards against a
# vmsingle datasource. Asserts through the query API that the
# canonical/derived series, units, node_pool/cluster presence, absent-not-zero
# for omitted sources and the fail-slow peer score are wired as the catalog
# promises; that ShadowGpuMemoryBeyondRepair fires on the synthetic
# unrepairable device and is delivered to human-default only, with exactly
# zero requests at the controller webhook sink (then API-injected probes
# prove both sinks and the route order are live); and through the Grafana
# API that every board is provisioned with its datasource bound and every
# variable and panel query executes without a query error (a wiring check on
# synthetic data, not panel semantics). G2 rows on fabricated series, not G2
# and no evidence about real GPUs, a real pager or the controller. Pulls the
# VictoriaMetrics, Alertmanager and Grafana images from Docker Hub. Deletes
# its cluster on exit unless KEEP_CLUSTER=1. Docker group membership
# required (sg docker -c 'make test-gpu-observability-kind').
test-gpu-observability-kind:
	KIND_BIN="$(KIND)" KUBECTL_BIN="$(KUBECTL)" GO_BIN="$(GO)" \
	./hack/kind-gpu-observability.sh

# Offline half of the above: bash -n, shellcheck, go vet/test of the fixture
# exporter and recorder sink, pinned tag/digest checks against
# versions.lock.yaml, scrape-config and Alertmanager-config extraction from
# the checked-in VMAgent/VMAlertmanager (route tree, inhibit rules and
# kubeneuron receiver must come back unchanged), shadow-rule and fixture-spec
# drift checks, dashboard placeholder binding, Grafana provisioning
# consistency, manifest rendering; amtool route tests when AMTOOL_BIN is
# set. No cluster, no Docker, no network.
verify-gpu-observability-fixture:
	GO_BIN="$(GO)" ./hack/kind-gpu-observability.sh --verify

kind-gpu-observability-clean:
	$(KIND) delete cluster --name kubeneuron-gpu-observability

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
