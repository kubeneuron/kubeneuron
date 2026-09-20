#!/usr/bin/env bash
# shellcheck disable=SC2016 # Single-quoted jq programs expand jq, not shell, variables.
set -Eeuo pipefail

# v0.4.0 -> HEAD -> v0.4.0 (images only) -> HEAD lifecycle rehearsal on kind,
# for BOTH workflow stores, with the v0.5 runtime contract qualification rows
# and their hash-chained audit history as the durable evidence under test.
#
# One disposable kind cluster hosts two v0.4.0 installations side by side —
# `upgrade-sqlite` on the SQLite PVC store and `upgrade-postgres` on a
# throwaway in-cluster PostgreSQL — under the one cluster-scoped operator, so
# every step below is performed once for the operator and once per store:
#
#   1. install the released BASELINE (official install manifest and the
#      digest-pinned images from images.txt), seed a v0.4 incident per store;
#   2. upgrade in the documented order (CRDs -> operator -> controller/agent
#      images) to the locally built HEAD;
#   3. drive the v0.5 runtime contract lifecycle per store through the public
#      API: a profile selects that store's agent node, the real agent identity
#      posts a synthetic report, coverage reads Exact/FreshCompatible/Full, a
#      qualification is created and observed to ReadyForApproval, and its
#      audit chain (create, observe, observe, ready) is snapshotted;
#   4. roll the controller/agent images back to BASELINE (images only, no
#      store restore — docs/upgrade.md "Rolling back") and prove the old
#      binary serves 404 for every runtime-contract route, still serves the
#      v0.4 incident, and still returns the qualification's audit rows
#      through its own audit explorer (the rows are in the store, not gone);
#   5. upgrade to HEAD again and prove the qualification reads back
#      byte-identical (except the read-time evaluation clock), its audit
#      chain is byte-identical, and a fresh observation extends that same
#      chain (prev_hash links to the pre-rollback head) instead of forking.
#
# Node layout. The agent DaemonSet runs with hostPID and owns host state under
# hostPath /var/lib/kube-neuron (its spool), so two installations' agents on
# ONE node share that state and are not isolated from each other: the second
# agent crash-loops. The cluster therefore has one control-plane node plus one
# dedicated worker PER STORE, labeled kubeneuron.io/upgrade-store=<store>;
# every root object pins its agent there with spec.agent.nodeSelector (a
# field the v0.4.0 baseline already honours), and that worker is the node
# identity used for the profile, report, coverage, and qualification of that
# store. The controllers carry no node constraint on kind (no GPU labels).
#
# CPU-only. No kind node has a GPU: the accelerator report is synthetic
# evidence signed by the real agent Pod identity over real mTLS, which proves
# store, API, and upgrade wiring, not NVIDIA/NVML/DCGM or any GPU action.
#
# Requirements: docker, kind, kubectl, jq, curl, openssl, gh. The baseline
# images are pulled from the real release, so this cannot run air-gapped.
# Docker group membership is required: `sg docker -c 'hack/kind-upgrade-rollback.sh'`
# if the current login predates it.
#
# Env:
#   BASELINE          released tag to start from (default: v0.4.0)
#   STORES            space-separated subset of "sqlite postgres" (default: both)
#   CLUSTER_NAME      kind cluster (default: kubeneuron-upgrade-rollback;
#                     created if absent, deleted on exit unless KEEP_CLUSTER=1)
#   KEEP_CLUSTER=1    retain the cluster for inspection
#   TIMEOUT_SECONDS   per-wait timeout (default: 300)
#   POSTGRES_IMAGE    throwaway database image (default: docker.io/library/postgres:16-alpine)
#   KIND_BIN, KUBECTL_BIN, DOCKER_BIN, JQ_BIN  command paths

BASELINE=${BASELINE:-v0.4.0}
RELEASE_REPO=${RELEASE_REPO:-kubeneuron/kubeneuron}
LOAD_PLATFORM=${LOAD_PLATFORM:-linux/$(go env GOARCH 2>/dev/null || echo amd64)}
CLUSTER_NAME=${CLUSTER_NAME:-kubeneuron-upgrade-rollback}
STORES=${STORES:-sqlite postgres}
POSTGRES_IMAGE=${POSTGRES_IMAGE:-docker.io/library/postgres:16-alpine}
KIND_BIN=${KIND_BIN:-kind}
KUBECTL_BIN=${KUBECTL_BIN:-kubectl}
JQ_BIN=${JQ_BIN:-jq}
DOCKER_BIN=${DOCKER_BIN:-docker}
TIMEOUT_SECONDS=${TIMEOUT_SECONDS:-300}
RUN_ID=${RUN_ID:-$(date -u +%Y%m%d%H%M%S)-$$}
# The same immutable node image hack/kind-integration.sh pins.
readonly NODE_IMAGE='kindest/node:v1.33.12@sha256:3f5c8443c620245e4d355cfe09e96a91ead32ceaa569d3f1ca9edf0cb2fe2ff4'

REPO_ROOT=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
readonly OPERATOR_NS=kube-neuron
readonly BASELINE_REGISTRY=ghcr.io/kubeneuron/kubeneuron
readonly controllerPort=8080
readonly agentIngressPort=8443
readonly agentTokenAudience=kubeneuron-controller
readonly QUAL_MIN_DURATION_SECONDS=5
readonly PROFILE_DIGEST='sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef'
readonly PROFILE_DRIVER=570.1
readonly PROFILE_RUNTIME=dcgm-4.1

note() { printf 'upgrade-rollback: %s\n' "$*"; }
die() {
	printf 'upgrade-rollback FAIL: %s\n' "$*" >&2
	exit 1
}

for store in $STORES; do
	[[ $store == sqlite || $store == postgres ]] || die "STORES may only contain sqlite and postgres (got $store)"
done

root_name() { printf 'upgrade-%s' "$1"; }
store_ns() { printf 'kubeneuron-upgrade-%s' "$1"; }
operator_token() { printf 'upgrade-%s-operator-token' "$1"; }
# store_node_label is the node label that dedicates one kind worker to one
# store: the agent nodeSelector and the runtime profile both select on it.
readonly STORE_NODE_LABEL=kubeneuron.io/upgrade-store
# store_node / store_node_uid: the dedicated worker per store (filled in once
# the cluster is up).
declare -A store_node=() store_node_uid=()

umask 077
work_dir=$(mktemp -d "${TMPDIR:-/tmp}/kubeneuron-upgrade-rollback.XXXXXX")
port_forward_pid=
created_cluster=0
baseline_tree=
head_tag="upgrade-rollback-head-$RUN_ID"
head_images_built=0
stop_port_forward() {
	if [[ -n $port_forward_pid ]]; then
		kill "$port_forward_pid" >/dev/null 2>&1 || true
		wait "$port_forward_pid" >/dev/null 2>&1 || true
		port_forward_pid=
	fi
}
cleanup() {
	local status=$?
	stop_port_forward
	# Diagnostics only once this run's cluster exists and its kubeconfig is in
	# use; before that, kubectl would just dial localhost and print
	# connection-refused noise unrelated to the real failure.
	if ((status != 0)) && ((created_cluster)) && [[ ${KUBECONFIG:-} == "$work_dir/kubeconfig" && -s $KUBECONFIG ]]; then
		note "failure diagnostics follow"
		"$KUBECTL_BIN" get nodes -L "$STORE_NODE_LABEL" 2>&1 || true
		for store in $STORES; do
			"$KUBECTL_BIN" get kubeneuron "$(root_name "$store")" -o yaml 2>&1 | sed -n '/^status:/,$p' || true
			"$KUBECTL_BIN" -n "$(store_ns "$store")" get pods -o wide 2>&1 || true
			"$KUBECTL_BIN" -n "$(store_ns "$store")" logs "deployment/$(root_name "$store")-controller" --all-containers --tail=60 2>&1 || true
		done
		"$KUBECTL_BIN" -n "$OPERATOR_NS" logs deployment/kubeneuron-operator --all-containers --tail=60 2>&1 || true
	fi
	if [[ -n $baseline_tree ]]; then
		git -C "$REPO_ROOT" worktree remove --force "$baseline_tree" >/dev/null 2>&1 || true
	fi
	if ((created_cluster)) && [[ ${KEEP_CLUSTER:-0} != 1 ]]; then
		"$KIND_BIN" delete cluster --name "$CLUSTER_NAME" >/dev/null 2>&1 || true
	fi
	if ((head_images_built)); then
		# Only this run's uniquely tagged HEAD images; the retagged baseline
		# pulls are ordinary local copies of the published release.
		"$DOCKER_BIN" rmi "kubeneuron-operator:$head_tag" "kubeneuron-controller:$head_tag" \
			"kubeneuron-agent:$head_tag" >/dev/null 2>&1 || true
	fi
	rm -rf -- "$work_dir"
}
trap cleanup EXIT

for command in "$KIND_BIN" "$KUBECTL_BIN" "$JQ_BIN" "$DOCKER_BIN" curl openssl gh git sed grep; do
	command -v "$command" >/dev/null 2>&1 || die "required command not found: $command"
done
if ! docker_version=$("$DOCKER_BIN" info --format '{{.ServerVersion}}' 2>&1); then
	die "Docker is unavailable: $docker_version (a stale login may need: sg docker -c '$0')"
fi
note "run $RUN_ID: baseline $BASELINE, stores: $STORES, Docker $docker_version, kind $("$KIND_BIN" version | awk 'NR == 1 {print $2}')"

# --- baseline artifacts ------------------------------------------------------

note "downloading the $BASELINE release manifest and image digests"
install_manifest="$work_dir/kubeneuron-install-$BASELINE.yaml"
if ! gh release download "$BASELINE" -R "$RELEASE_REPO" \
	-p "kubeneuron-install-$BASELINE.yaml" -p images.txt \
	--dir "$work_dir" --clobber 2>/dev/null || [[ ! -s $install_manifest ]]; then
	note "release assets unavailable for $BASELINE; building the manifest from the tag's tree"
	baseline_tree="$work_dir/baseline-tree"
	rm -rf "$baseline_tree"
	if ! git -C "$REPO_ROOT" rev-parse --verify "refs/tags/$BASELINE" >/dev/null 2>&1; then
		git -C "$REPO_ROOT" fetch --quiet "https://github.com/${RELEASE_REPO}.git" \
			"refs/tags/${BASELINE}:refs/tags/${BASELINE}" 2>/dev/null || true
	fi
	git -C "$REPO_ROOT" worktree add --detach "$baseline_tree" "$BASELINE" >/dev/null 2>&1 ||
		die "cannot check out baseline $BASELINE to build its manifest"
	("$KUBECTL_BIN" kustomize "$baseline_tree/config/default" >"$install_manifest")
	git -C "$REPO_ROOT" worktree remove --force "$baseline_tree" >/dev/null 2>&1 || true
	baseline_tree=
	: >"$work_dir/images.txt"
fi
[[ -s $install_manifest ]] || die "release install manifest is empty"

declare -A baseline_image=()
while IFS= read -r ref; do
	[[ -n $ref ]] || continue
	name=${ref%%@*}
	baseline_image[${name##*/}]=$ref
done <"$work_dir/images.txt"

pull_baseline() {
	local component ref
	for component in operator controller agent; do
		ref=${baseline_image[$component]:-}
		[[ -n $ref ]] || return 1
		# ONE platform, explicitly: see hack/kind-upgrade.sh pull_baseline for
		# why a multi-arch index cannot be handed to `kind load docker-image`.
		if ! "$DOCKER_BIN" pull --quiet --platform "$LOAD_PLATFORM" "$ref" >/dev/null 2>&1; then
			gh auth token | "$DOCKER_BIN" login ghcr.io \
				--username "$(gh api user -q .login)" --password-stdin >/dev/null 2>&1 || true
			"$DOCKER_BIN" pull --quiet --platform "$LOAD_PLATFORM" "$ref" >/dev/null 2>&1 || return 1
		fi
		"$DOCKER_BIN" tag "$ref" "${BASELINE_REGISTRY}/${component}:${BASELINE}"
	done
	return 0
}

build_baseline_from_tag() {
	note "building baseline images from git tag $BASELINE (no registry access)"
	baseline_tree="$work_dir/baseline-src"
	git -C "$REPO_ROOT" worktree add --detach "$baseline_tree" "$BASELINE" >/dev/null
	local target
	for target in operator controller agent; do
		"$DOCKER_BIN" build --target "$target" \
			--tag "${BASELINE_REGISTRY}/${target}:${BASELINE}" \
			--file "$baseline_tree/build/Dockerfile" "$baseline_tree" >/dev/null
	done
}

# load_image hands ONE single-platform image archive to kind (see
# hack/kind-upgrade.sh load_baseline_images for the multi-arch failure this
# avoids).
load_image() {
	local ref=$1 archive="$work_dir/image-load.tar"
	"$DOCKER_BIN" save --platform "$LOAD_PLATFORM" -o "$archive" "$ref" ||
		die "cannot export image $ref"
	"$KIND_BIN" load image-archive "$archive" --name "$CLUSTER_NAME" >/dev/null ||
		die "cannot load image $ref into kind"
	rm -f "$archive"
}

note "obtaining the $BASELINE baseline images"
if ! pull_baseline; then
	build_baseline_from_tag
fi
if [[ " $STORES " == *" postgres "* ]]; then
	note "obtaining the throwaway database image $POSTGRES_IMAGE"
	"$DOCKER_BIN" pull --quiet --platform "$LOAD_PLATFORM" "$POSTGRES_IMAGE" >/dev/null ||
		die "cannot pull $POSTGRES_IMAGE"
fi

# --- cluster -------------------------------------------------------------------

if ! "$KIND_BIN" get clusters 2>/dev/null | grep -Fxq "$CLUSTER_NAME"; then
	note "creating kind cluster $CLUSTER_NAME (pinned control-plane + one dedicated agent worker per store: $STORES)"
	{
		printf 'kind: Cluster\napiVersion: kind.x-k8s.io/v1alpha4\nnodes:\n- role: control-plane\n  image: %s\n' "$NODE_IMAGE"
		for store in $STORES; do
			printf -- '- role: worker\n  image: %s\n  labels:\n    %s: %s\n' "$NODE_IMAGE" "$STORE_NODE_LABEL" "$store"
		done
	} >"$work_dir/kind.yaml"
	"$KIND_BIN" create cluster --name "$CLUSTER_NAME" --config "$work_dir/kind.yaml" \
		--wait "${TIMEOUT_SECONDS}s" --kubeconfig "$work_dir/kubeconfig" >/dev/null
	created_cluster=1
else
	die "cluster $CLUSTER_NAME already exists; delete it or choose another CLUSTER_NAME (this rehearsal needs a fresh store)"
fi
export KUBECONFIG="$work_dir/kubeconfig"
# Resolve each store's dedicated worker by label: exactly one node per store,
# so the agent nodeSelector below can only ever land on that node.
for store in $STORES; do
	nodes=$("$KUBECTL_BIN" get nodes -l "${STORE_NODE_LABEL}=${store}" -o jsonpath='{range .items[*]}{.metadata.name} {.metadata.uid}{"\n"}{end}')
	[[ $(printf '%s' "$nodes" | grep -c .) -eq 1 ]] || die "[$store] want exactly one node labeled ${STORE_NODE_LABEL}=${store}, got: ${nodes:-none}"
	store_node[$store]=${nodes%% *}
	store_node_uid[$store]=${nodes#* }
	[[ -n ${store_node[$store]} && -n ${store_node_uid[$store]} ]] || die "[$store] dedicated node has no name/UID"
	note "[$store] dedicated agent node: ${store_node[$store]} (uid ${store_node_uid[$store]})"
done

note "loading baseline images into kind"
for component in operator controller agent; do
	load_image "${BASELINE_REGISTRY}/${component}:${BASELINE}"
done
if [[ " $STORES " == *" postgres "* ]]; then
	load_image "$POSTGRES_IMAGE"
fi

# The official manifest pins the operator by digest. The image loaded into
# kind is exactly that digest's single-platform bytes, retagged, so the only
# rewrite is the reference style; imagePullPolicy Never then guarantees the
# cluster can never silently substitute a registry pull for the loaded bytes.
rewrite_operator_image() {
	local source=$1 image=$2 target=$3
	sed -E "s|image: ghcr\.io/kubeneuron/kubeneuron/operator[:@][A-Za-z0-9._:-]+|image: $image|" "$source" >"$target"
	grep -Fq "image: $image" "$target" || die "operator image substitution failed for $image"
}
pin_operator_pull_policy() {
	"$KUBECTL_BIN" -n "$OPERATOR_NS" patch deployment kubeneuron-operator --type=strategic \
		-p '{"spec":{"template":{"spec":{"containers":[{"name":"operator","imagePullPolicy":"Never"}]}}}}' >/dev/null
	"$KUBECTL_BIN" -n "$OPERATOR_NS" rollout status deployment/kubeneuron-operator \
		--timeout="${TIMEOUT_SECONDS}s" >/dev/null
}

note "installing baseline $BASELINE (official install manifest: CRDs + RBAC + operator)"
baseline_manifest="$work_dir/install-baseline.yaml"
rewrite_operator_image "$install_manifest" "${BASELINE_REGISTRY}/operator:${BASELINE}" "$baseline_manifest"
"$KUBECTL_BIN" apply -f "$baseline_manifest" >/dev/null
pin_operator_pull_policy

# --- per-store installation ------------------------------------------------------

install_postgres() {
	local ns=$1
	local password="upgrade-postgres-$RUN_ID"
	cat >"$work_dir/postgres-$ns.yaml" <<EOF
apiVersion: apps/v1
kind: Deployment
metadata:
  name: postgres
  namespace: $ns
spec:
  replicas: 1
  strategy:
    type: Recreate
  selector:
    matchLabels:
      app: upgrade-postgres
  template:
    metadata:
      labels:
        app: upgrade-postgres
    spec:
      containers:
        - name: postgres
          image: $POSTGRES_IMAGE
          imagePullPolicy: Never
          env:
            - name: POSTGRES_USER
              value: kubeneuron
            - name: POSTGRES_PASSWORD
              value: "$password"
            - name: POSTGRES_DB
              value: kubeneuron
          ports:
            - containerPort: 5432
          readinessProbe:
            exec:
              command: ["pg_isready", "-U", "kubeneuron", "-d", "kubeneuron"]
            periodSeconds: 2
          volumeMounts:
            - name: data
              mountPath: /var/lib/postgresql/data
      volumes:
        - name: data
          emptyDir: {}
---
apiVersion: v1
kind: Service
metadata:
  name: postgres
  namespace: $ns
spec:
  selector:
    app: upgrade-postgres
  ports:
    - port: 5432
      targetPort: 5432
EOF
	"$KUBECTL_BIN" apply -f "$work_dir/postgres-$ns.yaml" >/dev/null
	"$KUBECTL_BIN" -n "$ns" create secret generic upgrade-postgres-dsn \
		--from-literal=dsn="postgres://kubeneuron:${password}@postgres.${ns}.svc:5432/kubeneuron?sslmode=disable" >/dev/null
	"$KUBECTL_BIN" -n "$ns" rollout status deployment/postgres --timeout="${TIMEOUT_SECONDS}s" >/dev/null
}

generate_pki() {
	local store=$1 root=$2 ns=$3 root_uid=$4
	local pki="$work_dir/pki-$store"
	mkdir -m 0700 -- "$pki"
	openssl ecparam -name prime256v1 -genkey -noout -out "$pki/server-ca.key" 2>/dev/null
	openssl req -x509 -new -key "$pki/server-ca.key" -out "$pki/server-ca.crt" \
		-subj "/CN=upgrade-server-ca-$store" -days 30 >/dev/null 2>&1
	openssl ecparam -name prime256v1 -genkey -noout -out "$pki/client-ca.key" 2>/dev/null
	openssl req -x509 -new -key "$pki/client-ca.key" -out "$pki/client-ca.crt" \
		-subj "/CN=upgrade-client-ca-$store" -days 30 >/dev/null 2>&1
	openssl ecparam -name prime256v1 -genkey -noout -out "$pki/server.key" 2>/dev/null
	openssl req -new -key "$pki/server.key" -out "$pki/server.csr" \
		-subj "/CN=${root}-controller.${ns}.svc" >/dev/null 2>&1
	# The same four name forms hack/kind-integration.sh issues: the agent dials
	# <root>-controller.<ns>.svc, but the short and cluster.local forms cost
	# nothing and keep any in-cluster client that uses them from failing TLS.
	openssl x509 -req -in "$pki/server.csr" -CA "$pki/server-ca.crt" -CAkey "$pki/server-ca.key" \
		-CAcreateserial -out "$pki/server.crt" -days 20 \
		-extfile <(printf 'extendedKeyUsage=serverAuth\nsubjectAltName=DNS:%s-controller,DNS:%s-controller.%s,DNS:%s-controller.%s.svc,DNS:%s-controller.%s.svc.cluster.local\n' \
			"$root" "$root" "$ns" "$root" "$ns" "$root" "$ns") \
		>/dev/null 2>&1
	openssl verify -CAfile "$pki/server-ca.crt" -verify_hostname "${root}-controller.${ns}.svc" \
		"$pki/server.crt" >/dev/null 2>&1 || die "[$store] server certificate does not verify for ${root}-controller.${ns}.svc"
	openssl ecparam -name prime256v1 -genkey -noout -out "$pki/client.key" 2>/dev/null
	openssl req -new -key "$pki/client.key" -out "$pki/client.csr" -subj "/" >/dev/null 2>&1
	openssl x509 -req -in "$pki/client.csr" -CA "$pki/client-ca.crt" -CAkey "$pki/client-ca.key" \
		-CAcreateserial -out "$pki/client.crt" -days 20 \
		-extfile <(printf 'extendedKeyUsage=clientAuth\nsubjectAltName=URI:spiffe://kubeneuron.io/installation/%s/agent\n' "$root_uid") \
		>/dev/null 2>&1
	"$KUBECTL_BIN" -n "$ns" create secret tls "${root}-controller-tls" \
		--cert="$pki/server.crt" --key="$pki/server.key" >/dev/null
	"$KUBECTL_BIN" -n "$ns" create secret generic "${root}-controller-server-ca" \
		--from-file=ca.crt="$pki/server-ca.crt" >/dev/null
	"$KUBECTL_BIN" -n "$ns" create secret tls "${root}-agent-tls" \
		--cert="$pki/client.crt" --key="$pki/client.key" >/dev/null
	"$KUBECTL_BIN" -n "$ns" create secret generic "${root}-agent-client-ca" \
		--from-file=ca.crt="$pki/client-ca.crt" >/dev/null
}

install_store() {
	local store=$1
	local root ns
	root=$(root_name "$store")
	ns=$(store_ns "$store")
	note "[$store] installing baseline root object $root in namespace $ns"
	"$KUBECTL_BIN" create namespace "$ns" >/dev/null
	"$KUBECTL_BIN" -n "$ns" create secret generic "${root}-operator-api-token" \
		--from-literal=token="$(operator_token "$store")" >/dev/null
	"$KUBECTL_BIN" -n "$ns" create secret generic "${root}-webhook-token" \
		--from-literal=token="upgrade-$store-webhook-token" >/dev/null
	local store_spec
	if [[ $store == postgres ]]; then
		install_postgres "$ns"
		store_spec=$'    type: Postgres\n    secretRef:\n      name: upgrade-postgres-dsn'
	else
		store_spec=$'    type: SQLite\n    sqlite:\n      size: 1Gi'
	fi
	cat >"$work_dir/root-$store.yaml" <<ROOT
apiVersion: kubeneuron.io/v1alpha1
kind: GPUPlaybook
metadata:
  name: ${root}-observe
spec:
  kubeNeuronRef: $root
  target: GPU
  steps:
    - name: observe
      action: Observe
---
apiVersion: kubeneuron.io/v1alpha1
kind: GPURemediationPolicy
metadata:
  name: ${root}-policy
spec:
  kubeNeuronRef: $root
  priority: 1
  match:
    class: upgrade-test
  playbookRef: ${root}-observe
---
apiVersion: kubeneuron.io/v1alpha1
kind: KubeNeuron
metadata:
  name: $root
spec:
  namespace: $ns
  controller:
    image: ${BASELINE_REGISTRY}/controller:${BASELINE}
  agent:
    image: ${BASELINE_REGISTRY}/agent:${BASELINE}
    # Pin this store's agent to its own worker: the agent owns host state
    # under /var/lib/kube-neuron, which two installations cannot share.
    nodeSelector:
      ${STORE_NODE_LABEL}: ${store}
    tolerations:
      - operator: Exists
  safety:
    executionMode: DryRun
  notifications:
    operatorAPIToken:
      name: ${root}-operator-api-token
    webhookToken:
      name: ${root}-webhook-token
  workflowStore:
${store_spec}
  observability:
    victoriaMetrics:
      mode: External
      endpoint: http://vmsingle-unused.${ns}.svc:8428
    alertmanager:
      mode: External
      endpoint: http://alertmanager-unused.${ns}.svc:9093
  tls:
    serverSecretRef:
      name: ${root}-controller-tls
    clientCASecretRef:
      name: ${root}-agent-client-ca
    clientSecretRef:
      name: ${root}-agent-tls
    serverCASecretRef:
      name: ${root}-controller-server-ca
ROOT
	"$KUBECTL_BIN" apply -f "$work_dir/root-$store.yaml" >/dev/null
	local root_uid
	root_uid=$("$KUBECTL_BIN" get kubeneuron "$root" -o jsonpath='{.metadata.uid}')
	[[ -n $root_uid ]] || die "[$store] root object has no UID"
	generate_pki "$store" "$root" "$ns" "$root_uid"
}

# wait_converged waits until the operator has observed the root object's
# current generation and compiled its configuration, the controller
# Deployment is fully rolled to the expected image (spec generation observed,
# every replica updated), every controller/agent Pod runs the expected image,
# and exactly one agent is Ready on the store's dedicated worker (a second
# agent, or one on another node, would mean the nodeSelector was lost and the
# host-state isolation with it). Ready on the root alone is not enough right after
# an image patch: the operator may not have observed the new generation yet,
# and a stale Ready=True would pass.
#
# SQLite additionally requires Ready=True at the current generation.
# PostgreSQL cannot: the operator renders an active/standby pair whose
# standby answers 503 on /readyz by design (internal/operator/resources.go
# controllerReplicas + the /readyz readiness override), yet its
# workloadsReady (internal/operator/reconciler.go) demands every replica
# Available, so a Postgres root reports Ready=False/RuntimeUnavailable
# forever. That is an operator gap this rehearsal records rather than hides:
# for Postgres the converged state is exactly one Ready (leader) controller
# Pod out of the rendered replica count, and Ready=True is accepted too
# should the operator ever be fixed. Never `rollout status`: it would wait
# on the standby.
wait_converged() {
	local store=$1 label=$2 controller_image=$3 agent_image=$4
	local root ns node deadline root_json pods_json deploy_json ok
	root=$(root_name "$store")
	ns=$(store_ns "$store")
	node=${store_node[$store]}
	deadline=$((SECONDS + TIMEOUT_SECONDS))
	while ((SECONDS < deadline)); do
		root_json=$("$KUBECTL_BIN" get kubeneuron "$root" -o json 2>/dev/null || true)
		pods_json=$("$KUBECTL_BIN" -n "$ns" get pods -l "app.kubernetes.io/instance=$root" -o json 2>/dev/null || true)
		deploy_json=$("$KUBECTL_BIN" -n "$ns" get deployment "${root}-controller" -o json 2>/dev/null || true)
		ok=$(
			"$JQ_BIN" -n --argjson root "${root_json:-null}" --argjson pods "${pods_json:-null}" \
				--argjson deploy "${deploy_json:-null}" --arg store "$store" --arg node "$node" \
				--arg controller "$controller_image" --arg agent "$agent_image" '
				def cond($obj; $type): any($obj.status.conditions[]?; .type == $type and .status == "True");
				($root != null and $pods != null and $deploy != null) and
				($root.metadata.generation as $g |
					$root.status.observedGeneration == $g and
					any($root.status.conditions[]?; .type == "ConfigurationValid" and .status == "True" and .observedGeneration == $g) and
					any($root.status.conditions[]?; .type == "Ready" and .observedGeneration == $g)) and
				(($deploy.spec.replicas // 1) as $want |
					$deploy.status.observedGeneration >= $deploy.metadata.generation and
					$deploy.spec.template.spec.containers[0].image == $controller and
					($deploy.status.replicas // 0) == $want and ($deploy.status.updatedReplicas // 0) == $want and
					([$pods.items[] | select(.metadata.labels["app.kubernetes.io/component"] == "controller")] as $c |
						($c | length) == $want and
						all($c[]; .spec.containers[0].image == $controller and .status.phase == "Running" and .metadata.deletionTimestamp == null) and
						([$c[] | select(cond(.; "Ready"))] | length) as $ready |
						if $store == "sqlite" then
							$ready == $want and cond($root; "Ready")
						else
							$ready == 1 or ($ready == $want and cond($root; "Ready"))
						end)) and
				([$pods.items[] | select(.metadata.labels["app.kubernetes.io/component"] == "agent")] as $a |
					($a | length) == 1 and all($a[]; .spec.nodeName == $node and
						.spec.containers[0].image == $agent and .status.phase == "Running" and
						.metadata.deletionTimestamp == null and cond(.; "Ready")))
			' 2>/dev/null || echo false
		)
		if [[ $ok == true ]]; then
			if [[ $store == sqlite ]]; then
				note "[$store] $label: root $root is Ready on controller $controller_image"
			else
				note "[$store] $label: root $root converged on controller $controller_image (root Ready=$(printf '%s' "$root_json" | "$JQ_BIN" -r '[.status.conditions[]? | select(.type == "Ready")][0].status // "absent"'); one Ready leader of $(printf '%s' "$deploy_json" | "$JQ_BIN" -r '.spec.replicas // 1') replicas)"
			fi
			return 0
		fi
		sleep 3
	done
	printf '%s\n' "$root_json" | "$JQ_BIN" '.status // {}' >&2 || true
	printf '%s\n' "$deploy_json" | "$JQ_BIN" '.status // {}' >&2 || true
	"$KUBECTL_BIN" -n "$ns" get pods -o wide >&2 || true
	die "[$store] $label: root $root never converged on $controller_image"
}

# wait_config_digest waits until the operator has recompiled the root's
# configuration to a digest other than $2 AND the live leader reports that
# digest on /readyz through the current port-forward. This is what "the
# profile is applied" means; the Deployment does not roll for a config change
# (the ConfigMap is reloaded in place), so image/Ready checks would pass
# trivially and prove nothing about the profile being live.
wait_config_digest() {
	local store=$1 previous=$2 label=$3
	local root deadline digest='' live=''
	root=$(root_name "$store")
	deadline=$((SECONDS + TIMEOUT_SECONDS))
	while ((SECONDS < deadline)); do
		digest=$("$KUBECTL_BIN" get kubeneuron "$root" -o jsonpath='{.status.configDigest}' 2>/dev/null || true)
		if [[ -n $digest && $digest != "$previous" ]]; then
			live=$(curl --silent --noproxy '*' --max-time 5 "http://127.0.0.1:${public_port}/readyz" 2>/dev/null || true)
			if [[ $live == *"config=${digest}"* ]]; then
				note "[$store] $label: config digest $digest is compiled and live on the leader"
				return 0
			fi
		fi
		sleep 2
	done
	die "[$store] $label: config digest never advanced from ${previous:-<none>} and went live (status $digest, /readyz '$live')"
}

# leader_pod prints the one Ready controller Pod (the elected leader under
# PostgreSQL, the only Pod under SQLite).
leader_pod() {
	local store=$1 root ns
	root=$(root_name "$store")
	ns=$(store_ns "$store")
	"$KUBECTL_BIN" -n "$ns" get pods -l "app.kubernetes.io/instance=$root,app.kubernetes.io/component=controller" -o json |
		"$JQ_BIN" -r '[.items[] | select(any(.status.conditions[]?; .type == "Ready" and .status == "True"))][0].metadata.name // ""'
}

public_port=
agent_port=
start_port_forward() {
	local store=$1 pod ns log deadline
	stop_port_forward
	ns=$(store_ns "$store")
	pod=$(leader_pod "$store")
	[[ -n $pod ]] || die "[$store] no Ready controller Pod to port-forward to"
	log="$work_dir/port-forward-$store.log"
	: >"$log"
	"$KUBECTL_BIN" -n "$ns" port-forward "pod/$pod" ":${controllerPort}" ":${agentIngressPort}" >"$log" 2>&1 &
	port_forward_pid=$!
	deadline=$((SECONDS + 30))
	public_port='' agent_port=''
	while ((SECONDS < deadline)); do
		public_port=$(sed -n "s/^Forwarding from 127\\.0\\.0\\.1:\\([0-9][0-9]*\\) -> ${controllerPort}$/\\1/p" "$log")
		agent_port=$(sed -n "s/^Forwarding from 127\\.0\\.0\\.1:\\([0-9][0-9]*\\) -> ${agentIngressPort}$/\\1/p" "$log")
		[[ -n $public_port && -n $agent_port ]] && return 0
		kill -0 "$port_forward_pid" >/dev/null 2>&1 || die "[$store] port-forward to $pod exited early"
		sleep 1
	done
	die "[$store] port-forward to $pod allocated no local ports"
}
# api runs one bounded public-API request: api <store> <curl args...>
api() {
	local store=$1
	shift
	curl --silent --show-error --noproxy '*' --max-time 10 \
		-H "Authorization: Bearer $(operator_token "$store")" "$@"
}
api_code() {
	local store=$1 out=$2
	shift 2
	api "$store" -o "$out" -w '%{http_code}' "$@"
}
qual_url() { printf 'http://127.0.0.1:%s/api/v1/runtime-contract-qualifications' "$public_port"; }

# --- baseline install and v0.4 seed ----------------------------------------------

for store in $STORES; do
	install_store "$store"
done
for store in $STORES; do
	wait_converged "$store" "baseline install" "${BASELINE_REGISTRY}/controller:${BASELINE}" "${BASELINE_REGISTRY}/agent:${BASELINE}"
done

declare -A incident_id=() incident_audit=()
for store in $STORES; do
	note "[$store] seeding a v0.4 incident through the $BASELINE operator API"
	start_port_forward "$store"
	code=$(api_code "$store" /dev/null -H 'Content-Type: application/json' \
		--data-binary '{"node":"upgrade-node","class":"upgrade-test","actor":"upgrade-harness"}' \
		"http://127.0.0.1:${public_port}/api/v1/incidents")
	[[ $code == 202 ]] || die "[$store] manual incident returned $code, want 202"
	id=''
	deadline=$((SECONDS + 60))
	while ((SECONDS < deadline)); do
		id=$(api "$store" "http://127.0.0.1:${public_port}/api/v1/incidents?node=upgrade-node" |
			"$JQ_BIN" -r '[.[] | select(.class == "upgrade-test")][0].id // empty')
		[[ -n $id ]] && break
		sleep 1
	done
	[[ -n $id ]] || die "[$store] seed incident never appeared"
	incident_id[$store]=$id
	incident_audit[$store]=$(api "$store" "http://127.0.0.1:${public_port}/api/v1/incidents/${id}" | "$JQ_BIN" '.audit | length')
	((incident_audit[$store] >= 1)) || die "[$store] seed incident has no audit trail"
	# The v0.5 routes must NOT exist on the baseline; otherwise the 404s
	# asserted after rollback would prove nothing about the rollback.
	code=$(api_code "$store" /dev/null "$(qual_url)")
	[[ $code == 404 ]] || die "[$store] baseline $BASELINE already serves runtime-contract-qualifications ($code); this rehearsal needs a pre-v0.5 baseline"
	note "[$store] seeded incident $id with ${incident_audit[$store]} audit entries; baseline answers 404 on the v0.5 routes"
done
stop_port_forward

# --- upgrade to HEAD ----------------------------------------------------------------

note "building HEAD images from build/Dockerfile"
for target in operator controller agent; do
	"$DOCKER_BIN" build --target "$target" --tag "kubeneuron-$target:$head_tag" \
		--file "$REPO_ROOT/build/Dockerfile" "$REPO_ROOT" >/dev/null
done
head_images_built=1
for target in operator controller agent; do
	load_image "kubeneuron-$target:$head_tag"
done

upgrade_to_head() {
	local label=$1
	note "$label step 1/2: HEAD CRDs, RBAC, and operator (docs/upgrade.md order)"
	"$KUBECTL_BIN" apply -k "$REPO_ROOT/config/crd" >/dev/null
	"$KUBECTL_BIN" apply -k "$REPO_ROOT/config/rbac" >/dev/null
	rewrite_operator_image "$REPO_ROOT/config/default/operator_deployment.yaml" \
		"kubeneuron-operator:$head_tag" "$work_dir/operator-head.yaml"
	"$KUBECTL_BIN" apply -f "$work_dir/operator-head.yaml" >/dev/null
	pin_operator_pull_policy
	note "$label step 2/2: controller/agent images on every root object"
	local store
	for store in $STORES; do
		"$KUBECTL_BIN" patch kubeneuron "$(root_name "$store")" --type=merge -p "{
  \"spec\": {
    \"controller\": {\"image\": \"kubeneuron-controller:$head_tag\"},
    \"agent\":      {\"image\": \"kubeneuron-agent:$head_tag\"}
  }
}" >/dev/null
	done
	for store in $STORES; do
		wait_converged "$store" "$label" "kubeneuron-controller:$head_tag" "kubeneuron-agent:$head_tag"
	done
}

rollback_images_to_baseline() {
	# docs/upgrade.md "Rolling back — Images only": patch the root object back
	# to the previous images; the store is untouched. The operator and CRDs are
	# deliberately left at HEAD: v0.5 changed neither (config/, api/, and
	# internal/operator are identical to the baseline tag), and an images-only
	# rollback is exactly the rollback the release notes promise.
	note "rollback: controller/agent images back to $BASELINE on every root object (store untouched)"
	local store
	for store in $STORES; do
		"$KUBECTL_BIN" patch kubeneuron "$(root_name "$store")" --type=merge -p "{
  \"spec\": {
    \"controller\": {\"image\": \"${BASELINE_REGISTRY}/controller:${BASELINE}\"},
    \"agent\":      {\"image\": \"${BASELINE_REGISTRY}/agent:${BASELINE}\"}
  }
}" >/dev/null
	done
	for store in $STORES; do
		wait_converged "$store" "rollback" "${BASELINE_REGISTRY}/controller:${BASELINE}" "${BASELINE_REGISTRY}/agent:${BASELINE}"
	done
}

upgrade_to_head "upgrade"

# --- v0.5 lifecycle per store ----------------------------------------------------------

# post_report signs a synthetic accelerator report with the real agent Pod
# identity (Pod-bound ServiceAccount token + installation client certificate
# over mTLS) acknowledging the live profile. No node_uid on the wire: the
# controller stamps it from the identity and rejects a supplied one.
post_report() {
	local store=$1 profile_uid=$2 profile_generation=$3
	local root ns agent_pod token_file header_file report_file code
	root=$(root_name "$store")
	ns=$(store_ns "$store")
	agent_pod=$("$KUBECTL_BIN" -n "$ns" get pods -l "app.kubernetes.io/instance=$root,app.kubernetes.io/component=agent" \
		--field-selector=status.phase=Running -o jsonpath='{.items[0].metadata.name}')
	[[ -n $agent_pod ]] || die "[$store] no running agent Pod"
	token_file="$work_dir/agent-token-$store"
	"$KUBECTL_BIN" -n "$ns" create token "${root}-agent" --audience="$agentTokenAudience" --duration=10m \
		--bound-object-kind=Pod --bound-object-name="$agent_pod" >"$token_file"
	header_file="$work_dir/agent-header-$store"
	printf 'Authorization: Bearer %s\n' "$(tr -d '\r\n' <"$token_file")" >"$header_file"
	report_file="$work_dir/report-$store.json"
	cat >"$report_file" <<EOF
{
  "node": "${store_node[$store]}",
  "vendor": "nvidia",
  "observed_at": "$(date -u +%Y-%m-%dT%H:%M:%SZ)",
  "devices": [
    {"id": "GPU-upgrade-00000000-0000-0000-0000-000000000000", "kind": "physical", "family": "gpu", "model": "kind-synthetic-cpu-only"}
  ],
  "driver_version": "${PROFILE_DRIVER}",
  "runtime_version": "${PROFILE_RUNTIME}",
  "topology_safety": "verified-unpartitioned",
  "capabilities": [{"action": "reset-device", "scopes": ["physical-device"]}],
  "readiness": "ready",
  "profile_digest": "${PROFILE_DIGEST}",
  "profile_uid": "${profile_uid}",
  "profile_generation": ${profile_generation},
  "device_holders": []
}
EOF
	local service_dns="${root}-controller.${ns}.svc" pki="$work_dir/pki-$store"
	code=$(curl --silent --show-error --noproxy '*' --max-time 10 \
		--resolve "${service_dns}:${agent_port}:127.0.0.1" \
		--cacert "$pki/server-ca.crt" --cert "$pki/client.crt" --key "$pki/client.key" \
		-H "@$header_file" -H 'Content-Type: application/json' --data-binary "@$report_file" \
		-o "$work_dir/report-response-$store" -w '%{http_code}' \
		"https://${service_dns}:${agent_port}/api/v1/agents/accelerators/report-v1")
	[[ $code == 204 ]] || {
		sed -n '1,20p' "$work_dir/report-response-$store" >&2
		die "[$store] synthetic accelerator report returned $code, want 204"
	}
}

# wait_full_coverage polls the read-only coverage route until the node reads
# Exact/FreshCompatible/Full and leaves the response in $1.
wait_full_coverage() {
	local store=$1 out=$2 label=$3 node deadline code=''
	node=${store_node[$store]}
	deadline=$((SECONDS + TIMEOUT_SECONDS))
	while ((SECONDS < deadline)); do
		code=$(api_code "$store" "$out" "http://127.0.0.1:${public_port}/api/v1/nodes/${node}/runtime-contract?vendor=nvidia")
		if [[ $code == 200 ]] && "$JQ_BIN" -e '.selection == "Exact" and .attestation == "FreshCompatible" and .verification_depth == "Full" and (.reasons // [] | length) == 0' "$out" >/dev/null 2>&1; then
			return 0
		fi
		sleep 2
	done
	"$JQ_BIN" . "$out" >&2 2>/dev/null || sed -n '1,20p' "$out" >&2
	die "[$store] $label: coverage for $node never became Exact/FreshCompatible/Full (last HTTP $code)"
}

# observe posts one observation bound to the version in qual_version[store],
# leaves the response in $2, and advances qual_version.
declare -A qual_id=() qual_version=() profile_uid=() profile_generation=()
observe() {
	local store=$1 out=$2 key=$3 code
	code=$(api_code "$store" "$out" -H 'Content-Type: application/json' \
		-H "Idempotency-Key: upgrade-rollback-${RUN_ID}-${store}-observe-${key}" \
		--data-binary "{\"actor\":\"upgrade-harness\",\"resource_version\":${qual_version[$store]}}" \
		"$(qual_url)/${qual_id[$store]}/observe")
	[[ $code == 200 ]] || {
		sed -n '1,20p' "$out" >&2
		die "[$store] observe ($key) returned $code, want 200"
	}
	qual_version[$store]=$("$JQ_BIN" -r '.resource_version // 0' "$out")
	"$JQ_BIN" -e '.observations[-1].successful == true' "$out" >/dev/null || {
		"$JQ_BIN" '.observations[-1]' "$out" >&2
		die "[$store] observation ($key) was not a successful Full sample"
	}
}
audit_events() {
	local store=$1 out=$2 code
	code=$(api_code "$store" "$out" \
		"http://127.0.0.1:${public_port}/api/v1/audit-events?kind=runtime-contract-qualification&resource_id=${qual_id[$store]}&limit=50")
	[[ $code == 200 ]] || {
		sed -n '1,20p' "$out" >&2
		die "[$store] audit-events for qualification ${qual_id[$store]} returned $code, want 200"
	}
}
audit_actions() { "$JQ_BIN" -r '[.items[]? | .action] | join(",")' "$1"; }

for store in $STORES; do
	root=$(root_name "$store")
	profile_name="${root}-profile"
	node=${store_node[$store]}
	node_uid=${store_node_uid[$store]}
	# The profile selects on the same store label that pins the agent, so it
	# covers exactly this store's dedicated worker and no other node.
	note "[$store] v0.5 lifecycle: profile $profile_name selects $node via ${STORE_NODE_LABEL}=${store}"
	start_port_forward "$store"
	digest_before=$("$KUBECTL_BIN" get kubeneuron "$root" -o jsonpath='{.status.configDigest}')
	[[ -n $digest_before ]] || die "[$store] root $root has no compiled configDigest before the profile"
	cat >"$work_dir/profile-$store.yaml" <<EOF
apiVersion: kubeneuron.io/v1alpha1
kind: AcceleratorRuntimeProfile
metadata:
  name: ${profile_name}
spec:
  kubeNeuronRef: ${root}
  vendor: nvidia
  nodeSelector:
    matchLabels:
      ${STORE_NODE_LABEL}: ${store}
  profileDigest: ${PROFILE_DIGEST}
  driverVersion: "${PROFILE_DRIVER}"
  runtimeVersion: ${PROFILE_RUNTIME}
  maxReportAge: 10m
  allowedActions:
    - action: reset-device
      scopes:
        - physical-device
      requireVerifiedUnpartitionedTopology: true
EOF
	"$KUBECTL_BIN" apply -f "$work_dir/profile-$store.yaml" >/dev/null
	profile_uid[$store]=$("$KUBECTL_BIN" get acceleratorruntimeprofile "$profile_name" -o jsonpath='{.metadata.uid}')
	profile_generation[$store]=$("$KUBECTL_BIN" get acceleratorruntimeprofile "$profile_name" -o jsonpath='{.metadata.generation}')
	[[ -n ${profile_uid[$store]} && ${profile_generation[$store]:-0} -gt 0 ]] || die "[$store] profile $profile_name has no UID/generation"
	# The profile changes the compiled configuration without rolling any Pod;
	# wait until the recompiled digest is live on the leader before judging
	# coverage (or creating a qualification) against a stale snapshot.
	wait_config_digest "$store" "$digest_before" "profile applied"

	post_report "$store" "${profile_uid[$store]}" "${profile_generation[$store]}"
	coverage_file="$work_dir/coverage-$store.json"
	wait_full_coverage "$store" "$coverage_file" "post-upgrade"
	"$JQ_BIN" -e --arg node "$node" --arg uid "$node_uid" --arg p "$profile_name" --arg puid "${profile_uid[$store]}" \
		'.node_name == $node and .node_uid == $uid and .profile_name == $p and .profile_uid == $puid' "$coverage_file" >/dev/null ||
		die "[$store] coverage identity mismatch: $("$JQ_BIN" -c '{node_name, node_uid, profile_name, profile_uid}' "$coverage_file")"
	note "[$store] coverage: $("$JQ_BIN" -r '.summary' "$coverage_file")"

	create_file="$work_dir/qual-create-$store.json"
	# The expiry window must outlast the whole rehearsal with room to spare.
	# effective_state, expired, expiry_pending, ready_for_approval, and summary
	# are all computed from the clock against expires_at at read time, so an
	# expiry during the cycle would break the byte-identical comparison below
	# and turn the post-re-upgrade observation into an `expire` transition
	# (docs/upgrade.md: "the first observation after that records Expired if
	# the window has passed"). One hour is not enough margin for four image
	# rollouts across two stores; the ceiling is 30 days.
	expires_at=$(date -u -d '+12 hours' +%Y-%m-%dT%H:%M:%SZ)
	code=$(api_code "$store" "$create_file" -H 'Content-Type: application/json' \
		-H "Idempotency-Key: upgrade-rollback-${RUN_ID}-${store}-create" \
		--data-binary "{\"actor\":\"upgrade-harness\",\"nodes\":[\"${node}\"],\"vendor\":\"nvidia\",\"requirements\":{\"min_samples\":1,\"min_duration\":\"${QUAL_MIN_DURATION_SECONDS}s\"},\"expires_at\":\"${expires_at}\"}" \
		"$(qual_url)")
	[[ $code == 201 ]] || {
		sed -n '1,20p' "$create_file" >&2
		die "[$store] qualification create returned $code, want 201"
	}
	qual_id[$store]=$("$JQ_BIN" -r '.id // ""' "$create_file")
	qual_version[$store]=$("$JQ_BIN" -r '.resource_version // 0' "$create_file")
	[[ -n ${qual_id[$store]} && ${qual_version[$store]} -gt 0 ]] || die "[$store] qualification create returned no id/version"
	"$JQ_BIN" -e --arg uid "$node_uid" --arg puid "${profile_uid[$store]}" '
		.state == "Observing" and .effective_state == "Observing" and .ready_for_approval == false and
		.successful_samples == 0 and .total_observations == 0 and
		(.cohort | length) == 1 and .cohort[0].uid == $uid and .profile.uid == $puid and
		.initial_coverage[0].verification_depth == "Full"' "$create_file" >/dev/null || {
		"$JQ_BIN" . "$create_file" >&2
		die "[$store] created qualification ${qual_id[$store]} is not a fresh Observing record over $node"
	}
	sleep "$QUAL_MIN_DURATION_SECONDS"
	observe "$store" "$work_dir/qual-observe1-$store.json" first
	"$JQ_BIN" -e '.successful_samples == 1 and .state == "Observing"' "$work_dir/qual-observe1-$store.json" >/dev/null ||
		die "[$store] after the first sample: $("$JQ_BIN" -c '{successful_samples, state}' "$work_dir/qual-observe1-$store.json"), want 1/Observing"
	sleep "$QUAL_MIN_DURATION_SECONDS"
	observe "$store" "$work_dir/qual-observe2-$store.json" second
	"$JQ_BIN" -e '.successful_samples == 2 and .state == "ReadyForApproval" and .effective_state == "ReadyForApproval" and .ready_for_approval == true' \
		"$work_dir/qual-observe2-$store.json" >/dev/null ||
		die "[$store] after the second sample: $("$JQ_BIN" -c '{successful_samples, state, effective_state, ready_for_approval}' "$work_dir/qual-observe2-$store.json"), want 2/ReadyForApproval"

	# Snapshots the rollback and re-upgrade are judged against. evaluated_at
	# is the read-time clock and legitimately differs per read.
	code=$(api_code "$store" "$work_dir/qual-before-$store.json" "$(qual_url)/${qual_id[$store]}")
	[[ $code == 200 ]] || die "[$store] qualification GET returned $code, want 200"
	"$JQ_BIN" -S 'del(.evaluated_at)' "$work_dir/qual-before-$store.json" >"$work_dir/qual-before-$store.canonical.json"
	audit_events "$store" "$work_dir/audit-before-$store.json"
	actions=$(audit_actions "$work_dir/audit-before-$store.json")
	[[ $actions == "create,observe,observe,ready" ]] || {
		"$JQ_BIN" -c '[.items[]? | {action, actor, result}]' "$work_dir/audit-before-$store.json" >&2
		die "[$store] audit actions were '$actions', want create,observe,observe,ready"
	}
	"$JQ_BIN" -S '.items' "$work_dir/audit-before-$store.json" >"$work_dir/audit-before-$store.canonical.json"
	"$JQ_BIN" -e '[.items[] | .hash] | length == 4 and (unique | length) == 4' "$work_dir/audit-before-$store.json" >/dev/null ||
		die "[$store] audit chain hashes are not four distinct values"
	note "[$store] qualification ${qual_id[$store]} is ReadyForApproval at resource_version ${qual_version[$store]} with audit create,observe,observe,ready"
done
stop_port_forward

# --- images-only rollback to baseline -----------------------------------------------------

rollback_images_to_baseline

for store in $STORES; do
	start_port_forward "$store"
	note "[$store] rolled back: asserting the $BASELINE binary hides but keeps the v0.5 rows"
	for path in \
		"/api/v1/runtime-contract-qualifications" \
		"/api/v1/runtime-contract-qualifications/${qual_id[$store]}" \
		"/api/v1/nodes/${store_node[$store]}/runtime-contract?vendor=nvidia" \
		"/api/v1/runtime-contracts/coverage?vendor=nvidia"; do
		code=$(api_code "$store" /dev/null "http://127.0.0.1:${public_port}${path}")
		[[ $code == 404 ]] || die "[$store] rolled-back $BASELINE controller answered $code on $path, want 404"
	done
	code=$(api_code "$store" /dev/null -X POST -H 'Content-Type: application/json' \
		-H "Idempotency-Key: upgrade-rollback-${RUN_ID}-${store}-rollback-observe" \
		--data-binary "{\"actor\":\"upgrade-harness\",\"resource_version\":${qual_version[$store]}}" \
		"$(qual_url)/${qual_id[$store]}/observe")
	[[ $code == 404 ]] || die "[$store] rolled-back controller accepted an observe ($code), want 404"
	# The v0.4 incident is still served, with its audit intact.
	survived=$(api "$store" "http://127.0.0.1:${public_port}/api/v1/incidents/${incident_id[$store]}" |
		"$JQ_BIN" --arg id "${incident_id[$store]}" --argjson n "${incident_audit[$store]}" '(.incident.id == $id) and ((.audit | length) >= $n)')
	[[ $survived == true ]] || die "[$store] v0.4 incident ${incident_id[$store]} or its audit did not survive the rollback"
	# The old binary's audit explorer filters by kind string without knowing
	# the kind: the qualification's four hash-chained events are still in the
	# store and byte-identical, which is the "rows remain" promise made
	# concrete rather than inferred from a 404.
	audit_events "$store" "$work_dir/audit-rollback-$store.json"
	"$JQ_BIN" -S '.items' "$work_dir/audit-rollback-$store.json" >"$work_dir/audit-rollback-$store.canonical.json"
	cmp -s "$work_dir/audit-before-$store.canonical.json" "$work_dir/audit-rollback-$store.canonical.json" || {
		diff "$work_dir/audit-before-$store.canonical.json" "$work_dir/audit-rollback-$store.canonical.json" >&2 || true
		die "[$store] qualification audit chain read through the $BASELINE binary differs from the pre-rollback chain"
	}
	logs=$("$KUBECTL_BIN" -n "$(store_ns "$store")" logs "pod/$(leader_pod "$store")" --tail=-1 2>&1)
	if grep -Eiq 'panic|fatal' <<<"$logs"; then
		die "[$store] rolled-back controller logs contain a panic/fatal line"
	fi
	note "[$store] $BASELINE serves 404 on all four runtime-contract routes and the observe mutation, still serves incident ${incident_id[$store]}, and still returns the qualification's 4 audit events unchanged"
done
stop_port_forward

# --- re-upgrade to HEAD ------------------------------------------------------------------

upgrade_to_head "re-upgrade"

for store in $STORES; do
	start_port_forward "$store"
	note "[$store] re-upgraded: asserting the qualification and its audit chain are intact and still usable"
	code=$(api_code "$store" "$work_dir/qual-after-$store.json" "$(qual_url)/${qual_id[$store]}")
	[[ $code == 200 ]] || die "[$store] qualification GET after re-upgrade returned $code, want 200"
	"$JQ_BIN" -S 'del(.evaluated_at)' "$work_dir/qual-after-$store.json" >"$work_dir/qual-after-$store.canonical.json"
	cmp -s "$work_dir/qual-before-$store.canonical.json" "$work_dir/qual-after-$store.canonical.json" || {
		diff "$work_dir/qual-before-$store.canonical.json" "$work_dir/qual-after-$store.canonical.json" >&2 || true
		die "[$store] qualification ${qual_id[$store]} read back differently after the rollback cycle"
	}
	"$JQ_BIN" -e '.state == "ReadyForApproval" and .ready_for_approval == true and .successful_samples == 2' "$work_dir/qual-after-$store.json" >/dev/null ||
		die "[$store] qualification is no longer ReadyForApproval with 2 samples after re-upgrade"
	code=$(api_code "$store" "$work_dir/qual-list-after-$store.json" "$(qual_url)")
	[[ $code == 200 ]] || die "[$store] qualification list after re-upgrade returned $code, want 200"
	"$JQ_BIN" -e --arg id "${qual_id[$store]}" '[.items[]? | select(.id == $id)] | length == 1' "$work_dir/qual-list-after-$store.json" >/dev/null ||
		die "[$store] qualification ${qual_id[$store]} is not listed exactly once after re-upgrade"
	audit_events "$store" "$work_dir/audit-after-$store.json"
	"$JQ_BIN" -S '.items' "$work_dir/audit-after-$store.json" >"$work_dir/audit-after-$store.canonical.json"
	cmp -s "$work_dir/audit-before-$store.canonical.json" "$work_dir/audit-after-$store.canonical.json" || {
		diff "$work_dir/audit-before-$store.canonical.json" "$work_dir/audit-after-$store.canonical.json" >&2 || true
		die "[$store] qualification audit chain differs after the rollback cycle"
	}
	survived=$(api "$store" "http://127.0.0.1:${public_port}/api/v1/incidents/${incident_id[$store]}" |
		"$JQ_BIN" --arg id "${incident_id[$store]}" '.incident.id == $id')
	[[ $survived == true ]] || die "[$store] v0.4 incident ${incident_id[$store]} is gone after re-upgrade"

	# Still usable, not just readable: a fresh report from the (new) agent Pod
	# identity restores Full coverage, and one more observation extends the
	# SAME audit chain — its prev_hash is the pre-rollback head.
	post_report "$store" "${profile_uid[$store]}" "${profile_generation[$store]}"
	wait_full_coverage "$store" "$work_dir/coverage-after-$store.json" "post-re-upgrade"
	observe "$store" "$work_dir/qual-observe3-$store.json" third
	"$JQ_BIN" -e '.successful_samples == 3 and .total_observations == 3 and .state == "ReadyForApproval"' "$work_dir/qual-observe3-$store.json" >/dev/null ||
		die "[$store] third observation: $("$JQ_BIN" -c '{successful_samples, total_observations, state}' "$work_dir/qual-observe3-$store.json"), want 3/3 ReadyForApproval"
	audit_events "$store" "$work_dir/audit-final-$store.json"
	actions=$(audit_actions "$work_dir/audit-final-$store.json")
	[[ $actions == "create,observe,observe,ready,observe" ]] || die "[$store] final audit actions were '$actions', want create,observe,observe,ready,observe"
	chained=$("$JQ_BIN" -r '(.items[3].hash == .items[4].prev_hash) and (.items[4].hash != .items[3].hash)' "$work_dir/audit-final-$store.json")
	[[ $chained == true ]] || die "[$store] the post-re-upgrade observe event does not chain onto the pre-rollback audit head"
	logs=$("$KUBECTL_BIN" -n "$(store_ns "$store")" logs "pod/$(leader_pod "$store")" --tail=-1 2>&1)
	if grep -Eiq 'panic|fatal' <<<"$logs"; then
		die "[$store] re-upgraded controller logs contain a panic/fatal line"
	fi
	note "[$store] qualification ${qual_id[$store]} survived $BASELINE -> HEAD -> $BASELINE -> HEAD byte-identical, and its audit chain grew to 5 linked events"
done
stop_port_forward

operator_logs=$("$KUBECTL_BIN" -n "$OPERATOR_NS" logs deployment/kubeneuron-operator --all-containers --tail=-1 2>&1)
if grep -Eiq 'forbidden|panic|fatal' <<<"$operator_logs"; then
	die "operator logs contain an unexpected RBAC/fatal error"
fi

note "PASS: $BASELINE -> HEAD -> $BASELINE (images only) -> HEAD converged for: $STORES; runtime contract qualification rows and their hash-chained audit survived the cycle on every store (CPU-only kind, synthetic accelerator evidence, no hardware claim)"
