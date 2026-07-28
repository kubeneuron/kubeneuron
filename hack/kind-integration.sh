#!/usr/bin/env bash
# shellcheck disable=SC2016 # Single-quoted jq programs expand jq, not shell, variables.
set -Eeuo pipefail

# Repeatable, CPU-only Kubernetes admission and operator integration smoke.
# This validates API admission and Kubernetes reconciliation. It does not
# validate NVIDIA drivers, NVML, DCGM, GPU telemetry/actions, or remediation.

usage() {
	cat <<'EOF'
Usage: hack/kind-integration.sh

Creates a dedicated, digest-pinned kind cluster by default, runs the 63-case
CEL admission matrix, installs locally built KubeNeuron images and the
operator, and verifies mTLS/token node identity, readiness, ownership
collisions, non-adoption, recovery, ordered certificate rotation/rollback, and
no-churn reconciliation.

Configuration is through environment variables:
  CLUSTER_NAME          kind cluster name (default: kubeneuron-integration)
  KUBECONFIG_PATH       new path for the generated kubeconfig; the harness
                        refuses any path that already exists
  REUSE_CLUSTER         1 to reuse an existing, correctly pinned cluster
  KEEP_CLUSTER          1 to retain a cluster created by this script
  KEEP_RESOURCES        1 to retain smoke fixtures for inspection
  BUILD_IMAGES          0 to use three already-present local images; the
                        default packages binaries previously built under bin/
  OPERATOR_IMAGE        local operator image name/tag
  CONTROLLER_IMAGE      local controller image name/tag
  AGENT_IMAGE           local agent image name/tag
  WORKER_NODES          worker nodes beside the control plane (default: 2;
                        0 reproduces the old single-node shape)
  TIMEOUT_SECONDS       wait timeout in seconds (default: 240)
  KIND_BIN, KUBECTL_BIN, DOCKER_BIN, JQ_BIN
                        command paths (each must be one executable)

The cluster always uses kind v0.32.0 and this immutable node image:
  kindest/node:v1.33.12@sha256:3f5c8443c620245e4d355cfe09e96a91ead32ceaa569d3f1ca9edf0cb2fe2ff4

If the current login predates membership in the docker group, invoke it as:
  sg docker -c './hack/kind-integration.sh'

Examples:
  KEEP_CLUSTER=1 ./hack/kind-integration.sh
  REUSE_CLUSTER=1 KEEP_CLUSTER=1 ./hack/kind-integration.sh
EOF
}

if [[ ${1:-} == -h || ${1:-} == --help ]]; then
	usage
	exit 0
fi
[[ $# -eq 0 ]] || {
	usage >&2
	exit 2
}

readonly EXPECTED_KIND_VERSION=v0.32.0
readonly EXPECTED_KUBECTL_VERSION=v1.33.12
readonly NODE_IMAGE='kindest/node:v1.33.12@sha256:3f5c8443c620245e4d355cfe09e96a91ead32ceaa569d3f1ca9edf0cb2fe2ff4'
readonly EXPECTED_SERVER_VERSION=v1.33.12
readonly OPERATOR_NAMESPACE=kube-neuron
readonly OPERATOR_DEPLOYMENT=kubeneuron-operator
readonly ROOT_NAME=integration-smoke
readonly TARGET_NAMESPACE=kubeneuron-integration-smoke
readonly PLAYBOOK_NAME=integration-smoke-observe
readonly POLICY_NAME=integration-smoke-policy
readonly LADDER_PLAYBOOK_NAME=integration-smoke-ladder
readonly RESTART_POLICY_NAME=integration-restart-policy
readonly controllerPort=8080
readonly agentIngressPort=8443
readonly agentTokenAudience=kubeneuron-controller

SCRIPT_DIR=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)
REPO_ROOT=$(cd -- "$SCRIPT_DIR/.." && pwd -P)
CEL_SCRIPT="$REPO_ROOT/test/integration/cel-admission.sh"
ROTATION_SCRIPT="$REPO_ROOT/hack/tls-rotate.sh"
EMERGENCY_TLS_SCRIPT="$REPO_ROOT/hack/tls-emergency-recover.sh"
SMOKE_TEMPLATE="$REPO_ROOT/test/integration/operator-smoke.yaml.tmpl"
IMAGE_DOCKERFILE="$REPO_ROOT/test/integration/Dockerfile"

KIND_BIN=${KIND_BIN:-kind}
KUBECTL_BIN=${KUBECTL_BIN:-kubectl}
DOCKER_BIN=${DOCKER_BIN:-docker}
JQ_BIN=${JQ_BIN:-jq}
CLUSTER_NAME=${CLUSTER_NAME:-kubeneuron-integration}
KUBECONFIG_PATH=${KUBECONFIG_PATH:-/tmp/${CLUSTER_NAME}.kubeconfig}
REUSE_CLUSTER=${REUSE_CLUSTER:-0}
KEEP_CLUSTER=${KEEP_CLUSTER:-0}
KEEP_RESOURCES=${KEEP_RESOURCES:-0}
BUILD_IMAGES=${BUILD_IMAGES:-1}
WORKER_NODES=${WORKER_NODES:-2}
TIMEOUT_SECONDS=${TIMEOUT_SECONDS:-240}
RUN_ID=${RUN_ID:-$(date -u +%Y%m%d%H%M%S)-$$}
OPERATOR_IMAGE=${OPERATOR_IMAGE:-kubeneuron-operator:kind-integration-${RUN_ID}}
CONTROLLER_IMAGE=${CONTROLLER_IMAGE:-kubeneuron-controller:kind-integration-${RUN_ID}}
AGENT_IMAGE=${AGENT_IMAGE:-kubeneuron-agent:kind-integration-${RUN_ID}}

created_cluster=0
fixture_applied=0
operator_scaled_down=0
operator_log_sequence=0
kubeconfig_created=0
kubeconfig_identity=
kubeconfig_temp_identity=
kubeconfig_temp=
work_dir=
port_forward_pid=
declare -A tls_secret_uids=()
declare -A tls_secret_hashes=()

note() {
	printf 'integration: %s\n' "$*"
}

die() {
	printf 'integration FAIL: %s\n' "$*" >&2
	exit 1
}

require_boolean() {
	local name=$1
	local value=$2
	[[ $value == 0 || $value == 1 ]] || die "$name must be 0 or 1 (got $value)"
}

require_positive_integer() {
	local name=$1
	local value=$2
	[[ $value =~ ^[1-9][0-9]*$ ]] || die "$name must be a positive integer (got $value)"
}

cluster_exists() {
	"$KIND_BIN" get clusters 2>/dev/null | grep -Fxq "$CLUSTER_NAME"
}

api_available() {
	((kubeconfig_created)) && [[ -f $KUBECONFIG_PATH && ! -L $KUBECONFIG_PATH ]] && \
		[[ $(stat --format='%d:%i' -- "$KUBECONFIG_PATH" 2>/dev/null) == "$kubeconfig_identity" ]] && \
		"$KUBECTL_BIN" --kubeconfig="$KUBECONFIG_PATH" --request-timeout=5s \
		get --raw=/readyz >/dev/null 2>&1
}

capture_operator_logs() {
	local phase=$1
	local log_file
	operator_log_sequence=$((operator_log_sequence + 1))
	log_file="$work_dir/operator-before-${operator_log_sequence}-${phase}.log"
	if ! "$KUBECTL_BIN" -n "$OPERATOR_NAMESPACE" logs \
		deployment/"$OPERATOR_DEPLOYMENT" --all-containers --tail=-1 \
		>"$log_file" 2>&1; then
		printf '%s\n' "$(<"$log_file")" >&2
		die "could not capture operator logs before $phase"
	fi
	if grep -Eiq 'forbidden|object has been modified|panic|fatal' "$log_file"; then
		printf '%s\n' "$(<"$log_file")" >&2
		die "operator logs before $phase contain an unexpected RBAC/concurrency/fatal error"
	fi
	note "captured and checked operator logs before $phase"
}

scale_operator() {
	local replicas=$1
	local phase=${2:-}
	if ((replicas == 0)); then
		[[ -n $phase ]] || die "scale_operator requires a phase name when scaling to zero"
		capture_operator_logs "$phase"
	fi
	"$KUBECTL_BIN" -n "$OPERATOR_NAMESPACE" scale \
		deployment "$OPERATOR_DEPLOYMENT" --replicas="$replicas" >/dev/null
	"$KUBECTL_BIN" -n "$OPERATOR_NAMESPACE" rollout status \
		deployment "$OPERATOR_DEPLOYMENT" --timeout="${TIMEOUT_SECONDS}s" >/dev/null
	if ((replicas == 0)); then
		operator_scaled_down=1
	else
		operator_scaled_down=0
	fi
}

cleanup_fixtures() {
	"$KUBECTL_BIN" delete kubeneuron "$ROOT_NAME" \
		--ignore-not-found --wait=true --timeout=90s >/dev/null 2>&1 || true
	"$KUBECTL_BIN" delete gpuremediationpolicy "$POLICY_NAME" "$RESTART_POLICY_NAME" \
		--ignore-not-found --wait=true --timeout=60s >/dev/null 2>&1 || true
	"$KUBECTL_BIN" delete gpuplaybook "$PLAYBOOK_NAME" "$LADDER_PLAYBOOK_NAME" \
		--ignore-not-found --wait=true --timeout=60s >/dev/null 2>&1 || true
	"$KUBECTL_BIN" delete namespace "$TARGET_NAMESPACE" \
		--ignore-not-found --wait=true --timeout=90s >/dev/null 2>&1 || true
	"$KUBECTL_BIN" wait --for=delete "clusterrole/${ROOT_NAME}-controller" \
		--timeout=60s >/dev/null 2>&1 || true
	"$KUBECTL_BIN" wait --for=delete "clusterrolebinding/${ROOT_NAME}-controller" \
		--timeout=60s >/dev/null 2>&1 || true
}

dump_diagnostics() {
	local log_file
	note "failure diagnostics follow"
	for log_file in "$work_dir"/operator-before-*.log; do
		[[ -f $log_file ]] || continue
		note "saved operator phase log: ${log_file##*/}"
		printf '%s\n' "$(<"$log_file")"
	done
	"$KUBECTL_BIN" get kubeneuron "$ROOT_NAME" -o yaml 2>&1 || true
	"$KUBECTL_BIN" -n "$TARGET_NAMESPACE" get \
		pods,configmaps,services,persistentvolumeclaims,deployments,daemonsets,poddisruptionbudgets \
		-o wide 2>&1 || true
	"$KUBECTL_BIN" -n "$OPERATOR_NAMESPACE" get pods -o wide 2>&1 || true
	"$KUBECTL_BIN" -n "$OPERATOR_NAMESPACE" logs \
		deployment/"$OPERATOR_DEPLOYMENT" --all-containers --tail=200 2>&1 || true
}

cleanup() {
	local rc=$?
	trap - EXIT
	set +e
	if [[ -n $port_forward_pid ]]; then
		kill "$port_forward_pid" >/dev/null 2>&1 || true
		wait "$port_forward_pid" >/dev/null 2>&1 || true
	fi
	if ((rc != 0)) && api_available; then
		dump_diagnostics
	fi
	if ((operator_scaled_down)) && api_available; then
		"$KUBECTL_BIN" -n "$OPERATOR_NAMESPACE" scale \
			deployment "$OPERATOR_DEPLOYMENT" --replicas=1 >/dev/null 2>&1 || true
	fi
	if ((fixture_applied)) && ((KEEP_RESOURCES == 0)) && api_available; then
		cleanup_fixtures
	fi
	if ((created_cluster)) && ((KEEP_CLUSTER == 0)) && cluster_exists; then
		"$KIND_BIN" delete cluster --name "$CLUSTER_NAME" >/dev/null 2>&1 || true
	elif ((created_cluster)); then
		note "retained cluster $CLUSTER_NAME; KUBECONFIG=$KUBECONFIG_PATH"
	fi
	if ((KEEP_CLUSTER == 0)) && ((kubeconfig_created)); then
		if [[ -f $KUBECONFIG_PATH && ! -L $KUBECONFIG_PATH ]] && \
			[[ $(stat --format='%d:%i' -- "$KUBECONFIG_PATH" 2>/dev/null) == "$kubeconfig_identity" ]]; then
			rm -f -- "$KUBECONFIG_PATH"
		else
			note "refusing to remove replaced kubeconfig path $KUBECONFIG_PATH"
		fi
	fi
	[[ -z $kubeconfig_temp ]] || rm -f -- "$kubeconfig_temp"
	[[ -z $work_dir ]] || rm -rf -- "$work_dir"
	if ((rc != 0)); then
		note "set KEEP_CLUSTER=1 KEEP_RESOURCES=1 to retain a failing run for inspection"
	fi
	exit "$rc"
}
trap cleanup EXIT
trap 'exit 130' INT TERM

wait_root_condition() {
	local expected_status=$1
	local expected_reason=$2
	local expected_message=${3:-}
	local deadline=$((SECONDS + TIMEOUT_SECONDS))
	local root_json=
	while ((SECONDS < deadline)); do
		root_json=$("$KUBECTL_BIN" get kubeneuron "$ROOT_NAME" -o json 2>/dev/null || true)
		if [[ -n $root_json ]] && "$JQ_BIN" -e \
			--arg status "$expected_status" \
			--arg reason "$expected_reason" \
			--arg message "$expected_message" '
              .metadata.generation as $generation |
              .status.observedGeneration == $generation and
              any(.status.conditions[]?;
                .type == "Ready" and
                .status == $status and
                .reason == $reason and
                .observedGeneration == $generation and
                ($message == "" or .message == $message))
            ' <<<"$root_json" >/dev/null; then
			return 0
		fi
		sleep 2
	done
	printf '%s\n' "$root_json" | "$JQ_BIN" '.status // {}' >&2 || true
	die "timed out waiting for Ready=$expected_status/$expected_reason"
}

wait_observed_generation() {
	local expected=$1
	local deadline=$((SECONDS + TIMEOUT_SECONDS))
	local actual=
	while ((SECONDS < deadline)); do
		actual=$("$KUBECTL_BIN" get kubeneuron "$ROOT_NAME" \
			-o jsonpath='{.status.observedGeneration}' 2>/dev/null || true)
		[[ $actual == "$expected" ]] && return 0
		sleep 2
	done
	die "timed out waiting for status.observedGeneration=$expected (got $actual)"
}

assert_owner() {
	local resource=$1
	local namespace=$2
	local root_uid=$3
	local object_json
	if [[ -n $namespace ]]; then
		object_json=$("$KUBECTL_BIN" -n "$namespace" get "$resource" -o json)
	else
		object_json=$("$KUBECTL_BIN" get "$resource" -o json)
	fi
	"$JQ_BIN" -e --arg uid "$root_uid" --arg name "$ROOT_NAME" '
      (.metadata.ownerReferences | length) == 1 and
      .metadata.ownerReferences[0].apiVersion == "kubeneuron.io/v1alpha1" and
      .metadata.ownerReferences[0].kind == "KubeNeuron" and
      .metadata.ownerReferences[0].name == $name and
      .metadata.ownerReferences[0].uid == $uid and
      .metadata.ownerReferences[0].controller == true and
      .metadata.ownerReferences[0].blockOwnerDeletion == true
    ' <<<"$object_json" >/dev/null || die "$resource does not have exactly the expected controller owner"
}

assert_all_owners() {
	local root_uid=$1
	local resource
	local -a namespaced_resources=(
		"configmap/${ROOT_NAME}-runtime"
		"configmap/${ROOT_NAME}-playbooks"
		"serviceaccount/${ROOT_NAME}-controller"
		"serviceaccount/${ROOT_NAME}-agent"
		"service/${ROOT_NAME}-controller"
		"persistentvolumeclaim/${ROOT_NAME}-controller-state"
		"deployment/${ROOT_NAME}-controller"
		"daemonset/${ROOT_NAME}-agent"
		"poddisruptionbudget/${ROOT_NAME}-controller"
	)
	for resource in "${namespaced_resources[@]}"; do
		assert_owner "$resource" "$TARGET_NAMESPACE" "$root_uid"
	done
	assert_owner "clusterrole/${ROOT_NAME}-controller" "" "$root_uid"
	assert_owner "clusterrolebinding/${ROOT_NAME}-controller" "" "$root_uid"
	note "all 11 managed resources have the expected controller owner"
}

assert_child_configuration_statuses() {
	local deadline=$((SECONDS + TIMEOUT_SECONDS))
	local playbook_json=
	local policy_json=
	while ((SECONDS < deadline)); do
		playbook_json=$("$KUBECTL_BIN" get gpuplaybook "$PLAYBOOK_NAME" -o json 2>/dev/null || true)
		policy_json=$("$KUBECTL_BIN" get gpuremediationpolicy "$POLICY_NAME" -o json 2>/dev/null || true)
		if "$JQ_BIN" -e '
          .metadata.generation as $generation |
          .status.observedGeneration == $generation and
          (.status.digest | type == "string" and length == 64) and
          any(.status.conditions[]?;
            .type == "Ready" and .status == "True" and .reason == "Compiled" and
            .observedGeneration == $generation)
        ' <<<"$playbook_json" >/dev/null &&
			"$JQ_BIN" -e --arg playbook "$PLAYBOOK_NAME" '
          .metadata.generation as $generation |
          .status.observedGeneration == $generation and
          .status.resolvedPlaybook == $playbook and
          (.status.digest | type == "string" and length == 64) and
          any(.status.conditions[]?;
            .type == "Ready" and .status == "True" and .reason == "Resolved" and
            .observedGeneration == $generation)
        ' <<<"$policy_json" >/dev/null; then
			note "playbook and policy publish current compiled digests and resolved status"
			return 0
		fi
		sleep 2
	done
	printf '%s\n' "$playbook_json" >&2
	printf '%s\n' "$policy_json" >&2
	die "timed out waiting for current compiled child-CR statuses"
}

assert_runtime_ready() {
	local root_json deployment_json daemonset_json pvc_json generation observed
	"$KUBECTL_BIN" -n "$TARGET_NAMESPACE" rollout status \
		deployment/"${ROOT_NAME}-controller" --timeout="${TIMEOUT_SECONDS}s" >/dev/null
	"$KUBECTL_BIN" -n "$TARGET_NAMESPACE" rollout status \
		daemonset/"${ROOT_NAME}-agent" --timeout="${TIMEOUT_SECONDS}s" >/dev/null
	"$KUBECTL_BIN" -n "$TARGET_NAMESPACE" wait \
		"persistentvolumeclaim/${ROOT_NAME}-controller-state" \
		"--for=jsonpath={.status.phase}=Bound" --timeout="${TIMEOUT_SECONDS}s" >/dev/null

	root_json=$("$KUBECTL_BIN" get kubeneuron "$ROOT_NAME" -o json)
	generation=$("$JQ_BIN" -r '.metadata.generation' <<<"$root_json")
	observed=$("$JQ_BIN" -r '.status.observedGeneration' <<<"$root_json")
	[[ $generation == "$observed" ]] || die "root generation/observedGeneration is $generation/$observed"
	"$JQ_BIN" -e --argjson generation "$generation" '
      .status.configDigest != null and .status.configDigest != "" and
      all(.status.conditions[]?; .observedGeneration == $generation)
    ' <<<"$root_json" >/dev/null || die "root digest or condition observed generations are stale"

	deployment_json=$("$KUBECTL_BIN" -n "$TARGET_NAMESPACE" get \
		deployment "${ROOT_NAME}-controller" -o json)
	"$JQ_BIN" -e '
      .metadata.generation == .status.observedGeneration and
      .status.replicas == 1 and .status.updatedReplicas == 1 and
      .status.readyReplicas == 1 and .status.availableReplicas == 1 and
      (.status.unavailableReplicas // 0) == 0
    ' <<<"$deployment_json" >/dev/null || die "controller Deployment is not current and fully available"

	daemonset_json=$("$KUBECTL_BIN" -n "$TARGET_NAMESPACE" get \
		daemonset "${ROOT_NAME}-agent" -o json)
	"$JQ_BIN" -e '
      .metadata.generation == .status.observedGeneration and
      .status.desiredNumberScheduled > 0 and
      .status.currentNumberScheduled == .status.desiredNumberScheduled and
      .status.updatedNumberScheduled == .status.desiredNumberScheduled and
      .status.numberReady == .status.desiredNumberScheduled and
      .status.numberAvailable == .status.desiredNumberScheduled and
      (.status.numberUnavailable // 0) == 0
    ' <<<"$daemonset_json" >/dev/null || die "agent DaemonSet is not current and fully available"

	pvc_json=$("$KUBECTL_BIN" -n "$TARGET_NAMESPACE" get \
		persistentvolumeclaim "${ROOT_NAME}-controller-state" -o json)
	"$JQ_BIN" -e '
      .status.phase == "Bound" and
      .spec.resources.requests.storage == "1Gi" and
      .status.capacity.storage == "1Gi"
    ' <<<"$pvc_json" >/dev/null || die "SQLite PVC is not Bound at requested/capacity 1Gi"
}

wait_agent_registration_readiness() {
	local expected=$1
	local deadline=$((SECONDS + TIMEOUT_SECONDS))
	local daemonset_json=
	while ((SECONDS < deadline)); do
		daemonset_json=$("$KUBECTL_BIN" -n "$TARGET_NAMESPACE" get \
			daemonset "${ROOT_NAME}-agent" -o json 2>/dev/null || true)
		if [[ -n $daemonset_json ]]; then
			if [[ $expected == ready ]] && "$JQ_BIN" -e '
              .status.desiredNumberScheduled > 0 and
              .status.numberReady == .status.desiredNumberScheduled
            ' <<<"$daemonset_json" >/dev/null; then
				return 0
			fi
			if [[ $expected == stale ]] && "$JQ_BIN" -e '
              .status.desiredNumberScheduled > 0 and
              (.status.numberReady // 0) == 0
            ' <<<"$daemonset_json" >/dev/null; then
				return 0
			fi
			# degraded: at least one agent lost readiness. A broken rollout
			# stalls at maxUnavailable on a multi-node DaemonSet — the other
			# agents keep their previous working identity by design.
			if [[ $expected == degraded ]] && "$JQ_BIN" -e '
              .status.desiredNumberScheduled > 0 and
              (.status.numberReady // 0) < .status.desiredNumberScheduled
            ' <<<"$daemonset_json" >/dev/null; then
				return 0
			fi
		fi
		sleep 2
	done
	printf '%s\n' "$daemonset_json" | "$JQ_BIN" '.status // {}' >&2 || true
	die "timed out waiting for agent registration readiness to become $expected"
}

wait_agent_log() {
	local pattern=$1
	local deadline=$((SECONDS + TIMEOUT_SECONDS))
	local logs= pod=
	while ((SECONDS < deadline)); do
		# Aggregate all agent pods: the daemonset/ selector reads one
		# arbitrary pod, which is not enough on a multi-node cluster.
		logs=$(while read -r pod; do
			[[ -n $pod ]] || continue
			"$KUBECTL_BIN" -n "$TARGET_NAMESPACE" logs "$pod" \
				--all-containers --tail=-1 2>&1 || true
		done < <("$KUBECTL_BIN" -n "$TARGET_NAMESPACE" get pods \
			-l "app.kubernetes.io/instance=${ROOT_NAME},app.kubernetes.io/component=agent" \
			-o name 2>/dev/null))
		if grep -Fq "$pattern" <<<"$logs"; then
			return 0
		fi
		sleep 2
	done
	printf '%s\n' "$logs" >&2
	die "timed out waiting for agent log transition: $pattern"
}

assert_can_i() {
	local expected=$1
	local verb=$2
	local resource=$3
	local namespace=${4:-}
	local subresource=${5:-}
	local subject="system:serviceaccount:${OPERATOR_NAMESPACE}:kubeneuron-operator"
	local -a command=("$KUBECTL_BIN" auth can-i "$verb" "$resource" --as="$subject")
	local actual
	if [[ -n $namespace ]]; then
		command+=(--namespace "$namespace")
	fi
	if [[ -n $subresource ]]; then
		command+=(--subresource="$subresource")
	fi
	actual=$("${command[@]}" 2>/dev/null || true)
	[[ $actual == "$expected" ]] || \
		die "auth can-i $verb $resource in ${namespace:-cluster scope} = ${actual:-empty}, want $expected"
}

assert_operator_rbac() {
	local verb resource
	for verb in create get update; do
		assert_can_i yes "$verb" leases.coordination.k8s.io "$OPERATOR_NAMESPACE"
	done
	for verb in list watch patch; do
		assert_can_i no "$verb" leases.coordination.k8s.io "$OPERATOR_NAMESPACE"
	done
	for verb in create patch; do
		assert_can_i yes "$verb" events "$OPERATOR_NAMESPACE"
		# Events for the cluster-scoped KubeNeuron object land in "default";
		# the grant is a namespace-scoped Role there, nowhere else.
		assert_can_i yes "$verb" events default
		assert_can_i no "$verb" events "$TARGET_NAMESPACE"
	done
	assert_can_i yes get namespaces
	assert_can_i no list namespaces
	assert_can_i no watch namespaces
	assert_can_i yes create tokenreviews.authentication.k8s.io
	for verb in get list watch update patch delete; do
		assert_can_i no "$verb" tokenreviews.authentication.k8s.io
	done
	for resource in gpuremediationpolicies.kubeneuron.io gpuplaybooks.kubeneuron.io; do
		for verb in get patch update; do
			assert_can_i yes "$verb" "$resource" "" status
		done
		for verb in create list watch delete; do
			assert_can_i no "$verb" "$resource" "" status
		done
	done
	note "operator Lease, Event, namespace, and child-status RBAC is minimal and correctly scoped"
}

generate_tls_fixtures() {
	local installation_uid=$1
	local pki_dir="$work_dir/pki"
	local service_name="${ROOT_NAME}-controller"
	local service_dns="${service_name}.${TARGET_NAMESPACE}.svc"
	mkdir -m 0700 -- "$pki_dir"

	openssl genpkey -algorithm EC -pkeyopt ec_paramgen_curve:P-256 \
		-out "$pki_dir/server-ca.key" >/dev/null 2>&1
	openssl req -new -x509 -sha256 -days 3650 \
		-key "$pki_dir/server-ca.key" -out "$pki_dir/server-ca.crt" \
		-subj "/CN=KubeNeuron integration server CA" >/dev/null 2>&1
	generate_server_leaf "$pki_dir" server "$pki_dir/server-ca.crt" "$pki_dir/server-ca.key"

	openssl genpkey -algorithm EC -pkeyopt ec_paramgen_curve:P-256 \
		-out "$pki_dir/client-ca.key" >/dev/null 2>&1
	openssl req -new -x509 -sha256 -days 3650 \
		-key "$pki_dir/client-ca.key" -out "$pki_dir/client-ca.crt" \
		-subj "/CN=KubeNeuron integration agent CA" >/dev/null 2>&1
	generate_client_leaf "$pki_dir" client "$installation_uid" "$pki_dir/client-ca.crt" "$pki_dir/client-ca.key"
	generate_client_leaf "$pki_dir" other-client other-installation "$pki_dir/client-ca.crt" "$pki_dir/client-ca.key"

	openssl genpkey -algorithm EC -pkeyopt ec_paramgen_curve:P-256 \
		-out "$pki_dir/rogue-ca.key" >/dev/null 2>&1
	openssl req -new -x509 -sha256 -days 3650 \
		-key "$pki_dir/rogue-ca.key" -out "$pki_dir/rogue-ca.crt" \
		-subj "/CN=KubeNeuron integration rogue CA" >/dev/null 2>&1
	generate_client_leaf "$pki_dir" rogue-client "$installation_uid" "$pki_dir/rogue-ca.crt" "$pki_dir/rogue-ca.key"

	openssl verify -CAfile "$pki_dir/server-ca.crt" "$pki_dir/server.crt" >/dev/null
	openssl verify -CAfile "$pki_dir/client-ca.crt" "$pki_dir/client.crt" >/dev/null
	openssl x509 -in "$pki_dir/server.crt" -noout -text | \
		grep -Fq "DNS:${service_dns}" || die "server certificate is missing its Service DNS SAN"
	openssl x509 -in "$pki_dir/client.crt" -noout -text | \
		grep -Fq "URI:spiffe://kubeneuron.io/installation/${installation_uid}/agent" || \
		die "client certificate is missing its installation URI SAN"
}

generate_server_leaf() {
	local pki_dir=$1
	local name=$2
	local ca_cert=$3
	local ca_key=$4
	local service_name="${ROOT_NAME}-controller"
	local service_dns="${service_name}.${TARGET_NAMESPACE}.svc"
	openssl genpkey -algorithm EC -pkeyopt ec_paramgen_curve:P-256 \
		-out "$pki_dir/${name}.key" >/dev/null 2>&1
	openssl req -new -sha256 -key "$pki_dir/${name}.key" -out "$pki_dir/${name}.csr" \
		-subj "/CN=$service_dns" >/dev/null 2>&1
	openssl x509 -req -sha256 -days 2 -in "$pki_dir/${name}.csr" \
		-CA "$ca_cert" -CAkey "$ca_key" -CAcreateserial \
		-out "$pki_dir/${name}.crt" -extfile <(printf '%s\n' \
			'basicConstraints=critical,CA:FALSE' \
			'keyUsage=critical,digitalSignature' \
			'extendedKeyUsage=serverAuth' \
			"subjectAltName=DNS:${service_name},DNS:${service_name}.${TARGET_NAMESPACE},DNS:${service_dns},DNS:${service_dns}.cluster.local") \
		>/dev/null 2>&1
}

generate_client_leaf() {
	local pki_dir=$1
	local name=$2
	local installation_uid=$3
	local ca_cert=$4
	local ca_key=$5
	openssl genpkey -algorithm EC -pkeyopt ec_paramgen_curve:P-256 \
		-out "$pki_dir/${name}.key" >/dev/null 2>&1
	openssl req -new -sha256 -key "$pki_dir/${name}.key" \
		-out "$pki_dir/${name}.csr" -subj "/CN=KubeNeuron integration agent" >/dev/null 2>&1
	openssl x509 -req -sha256 -days 2 -in "$pki_dir/${name}.csr" \
		-CA "$ca_cert" -CAkey "$ca_key" -CAcreateserial \
		-out "$pki_dir/${name}.crt" -extfile <(printf '%s\n' \
			'basicConstraints=critical,CA:FALSE' \
			'keyUsage=critical,digitalSignature' \
			'extendedKeyUsage=clientAuth' \
			"subjectAltName=URI:spiffe://kubeneuron.io/installation/${installation_uid}/agent") \
		>/dev/null 2>&1
}

create_tls_secrets() {
	local pki_dir="$work_dir/pki"
	"$KUBECTL_BIN" -n "$TARGET_NAMESPACE" create secret tls "${ROOT_NAME}-controller-tls" \
		--cert="$pki_dir/server.crt" --key="$pki_dir/server.key" >/dev/null
	"$KUBECTL_BIN" -n "$TARGET_NAMESPACE" create secret generic "${ROOT_NAME}-agent-client-ca" \
		--from-file=ca.crt="$pki_dir/client-ca.crt" >/dev/null
	"$KUBECTL_BIN" -n "$TARGET_NAMESPACE" create secret tls "${ROOT_NAME}-agent-tls" \
		--cert="$pki_dir/client.crt" --key="$pki_dir/client.key" >/dev/null
	"$KUBECTL_BIN" -n "$TARGET_NAMESPACE" create secret generic "${ROOT_NAME}-controller-server-ca" \
		--from-file=ca.crt="$pki_dir/server-ca.crt" >/dev/null
	"$KUBECTL_BIN" -n "$TARGET_NAMESPACE" create secret generic "${ROOT_NAME}-operator-api-token" \
		--from-literal=token="integration-operator-token" >/dev/null
	"$KUBECTL_BIN" -n "$TARGET_NAMESPACE" create secret generic "${ROOT_NAME}-webhook-token" \
		--from-literal=token="integration-webhook-token" >/dev/null

	local secret
	for secret in \
		"${ROOT_NAME}-controller-tls" \
		"${ROOT_NAME}-agent-client-ca" \
		"${ROOT_NAME}-agent-tls" \
		"${ROOT_NAME}-controller-server-ca"; do
		record_tls_secret "$secret"
	done
	note "created four unowned installation-local TLS input Secrets and two unowned API/webhook token Secrets"
}

record_tls_secret() {
	local secret=$1
	local secret_json owners data_hash
	secret_json=$("$KUBECTL_BIN" -n "$TARGET_NAMESPACE" get secret "$secret" -o json)
	owners=$("$JQ_BIN" -c '.metadata.ownerReferences // []' <<<"$secret_json")
	data_hash=$("$JQ_BIN" -cS '.data' <<<"$secret_json" | sha256sum | awk '{print $1}')
	[[ $owners == '[]' ]] || die "input TLS Secret $secret unexpectedly has an owner reference"
	"$JQ_BIN" -e '.metadata.annotations["kubectl.kubernetes.io/last-applied-configuration"] == null' \
		<<<"$secret_json" >/dev/null || die "input TLS Secret $secret duplicates its data in a last-applied annotation"
	tls_secret_uids["$secret"]=$("$JQ_BIN" -r '.metadata.uid' <<<"$secret_json")
	tls_secret_hashes["$secret"]=$data_hash
}

create_immutable_secret() {
	local name=$1
	local kind=$2
	shift 2
	"$KUBECTL_BIN" -n "$TARGET_NAMESPACE" create secret "$kind" "$name" "$@" \
		--dry-run=client -o json | "$JQ_BIN" '.immutable = true' | \
		"$KUBECTL_BIN" create -f - >/dev/null
	record_tls_secret "$name"
}

generate_rotated_tls_fixtures() {
	local installation_uid=$1
	local pki_dir="$work_dir/pki"
	local service_dns="${ROOT_NAME}-controller.${TARGET_NAMESPACE}.svc"

	openssl genpkey -algorithm EC -pkeyopt ec_paramgen_curve:P-256 \
		-out "$pki_dir/server-ca-v2.key" >/dev/null 2>&1
	openssl req -new -x509 -sha256 -days 3650 \
		-key "$pki_dir/server-ca-v2.key" -out "$pki_dir/server-ca-v2.crt" \
		-subj "/CN=KubeNeuron integration server CA v2" >/dev/null 2>&1
	generate_server_leaf "$pki_dir" server-v2 "$pki_dir/server-ca-v2.crt" "$pki_dir/server-ca-v2.key"
	generate_server_leaf "$pki_dir" server-emergency-v2 "$pki_dir/server-ca-v2.crt" "$pki_dir/server-ca-v2.key"
	openssl genpkey -algorithm EC -pkeyopt ec_paramgen_curve:P-256 \
		-out "$pki_dir/server-ca-retire-probe.key" >/dev/null 2>&1
	openssl req -new -x509 -sha256 -days 3650 \
		-key "$pki_dir/server-ca-retire-probe.key" -out "$pki_dir/server-ca-retire-probe.crt" \
		-subj "/CN=KubeNeuron integration server retirement probe CA" >/dev/null 2>&1
	generate_server_leaf "$pki_dir" server-retire-probe \
		"$pki_dir/server-ca-retire-probe.crt" "$pki_dir/server-ca-retire-probe.key"

	openssl genpkey -algorithm EC -pkeyopt ec_paramgen_curve:P-256 \
		-out "$pki_dir/client-ca-v2.key" >/dev/null 2>&1
	openssl req -new -x509 -sha256 -days 3650 \
		-key "$pki_dir/client-ca-v2.key" -out "$pki_dir/client-ca-v2.crt" \
		-subj "/CN=KubeNeuron integration agent CA v2" >/dev/null 2>&1
	generate_client_leaf "$pki_dir" client-v2 "$installation_uid" \
		"$pki_dir/client-ca-v2.crt" "$pki_dir/client-ca-v2.key"
	generate_client_leaf "$pki_dir" client-emergency-v2 "$installation_uid" \
		"$pki_dir/client-ca-v2.crt" "$pki_dir/client-ca-v2.key"

	{
		awk 'NF {print}' "$pki_dir/server-ca.crt"
		awk 'NF {print}' "$pki_dir/server-ca-v2.crt"
	} >"$pki_dir/server-ca-overlap.crt"
	{
		awk 'NF {print}' "$pki_dir/server-ca.crt"
		awk 'NF {print}' "$pki_dir/server-ca-retire-probe.crt"
	} >"$pki_dir/server-ca-overlap-retire-probe.crt"
	{
		awk 'NF {print}' "$pki_dir/client-ca.crt"
		awk 'NF {print}' "$pki_dir/client-ca-v2.crt"
	} >"$pki_dir/client-ca-overlap.crt"

	openssl verify -purpose sslserver -verify_hostname "$service_dns" \
		-CAfile "$pki_dir/server-ca-v2.crt" "$pki_dir/server-v2.crt" >/dev/null
	openssl verify -purpose sslserver -verify_hostname "$service_dns" \
		-CAfile "$pki_dir/server-ca-v2.crt" "$pki_dir/server-emergency-v2.crt" >/dev/null
	openssl verify -purpose sslserver -verify_hostname "$service_dns" \
		-CAfile "$pki_dir/server-ca-overlap.crt" "$pki_dir/server.crt" "$pki_dir/server-v2.crt" >/dev/null
	openssl verify -purpose sslserver -verify_hostname "$service_dns" \
		-CAfile "$pki_dir/server-ca-overlap-retire-probe.crt" \
		"$pki_dir/server.crt" "$pki_dir/server-retire-probe.crt" >/dev/null
	openssl verify -purpose sslclient \
		-CAfile "$pki_dir/client-ca-v2.crt" "$pki_dir/client-v2.crt" >/dev/null
	openssl verify -purpose sslclient \
		-CAfile "$pki_dir/client-ca-v2.crt" "$pki_dir/client-emergency-v2.crt" >/dev/null
	openssl verify -purpose sslclient \
		-CAfile "$pki_dir/client-ca-overlap.crt" "$pki_dir/client.crt" "$pki_dir/client-v2.crt" >/dev/null

	create_immutable_secret "${ROOT_NAME}-controller-tls-v2" tls \
		--cert="$pki_dir/server-v2.crt" --key="$pki_dir/server-v2.key"
	create_immutable_secret "${ROOT_NAME}-controller-tls-emergency-v2" tls \
		--cert="$pki_dir/server-emergency-v2.crt" --key="$pki_dir/server-emergency-v2.key"
	create_immutable_secret "${ROOT_NAME}-controller-tls-emergency-bad-v2" generic \
		--type=kubernetes.io/tls \
		--from-file=tls.crt="$pki_dir/server-v2.crt" \
		--from-file=tls.key="$pki_dir/client-v2.key"
	create_immutable_secret "${ROOT_NAME}-controller-tls-bad-v2" generic \
		--type=kubernetes.io/tls \
		--from-file=tls.crt="$pki_dir/server-v2.crt" \
		--from-file=tls.key="$pki_dir/client-v2.key"
	create_immutable_secret "${ROOT_NAME}-controller-server-ca-overlap-bad-leaf" generic \
		--from-file=ca.crt="$pki_dir/server-ca-overlap.crt"
	create_immutable_secret "${ROOT_NAME}-controller-server-ca-final-bad-leaf" generic \
		--from-file=ca.crt="$pki_dir/server-ca-v2.crt"
	create_immutable_secret "${ROOT_NAME}-controller-tls-retire-probe" tls \
		--cert="$pki_dir/server-retire-probe.crt" --key="$pki_dir/server-retire-probe.key"
	create_immutable_secret "${ROOT_NAME}-controller-server-ca-overlap-retire-probe" generic \
		--from-file=ca.crt="$pki_dir/server-ca-overlap-retire-probe.crt"
	create_immutable_secret "${ROOT_NAME}-controller-server-ca-bad-retire-probe" generic \
		--from-file=ca.crt="$pki_dir/rogue-ca.crt"
	create_immutable_secret "${ROOT_NAME}-agent-client-ca-overlap-v2" generic \
		--from-file=ca.crt="$pki_dir/client-ca-overlap.crt"
	create_immutable_secret "${ROOT_NAME}-agent-client-ca-v2" generic \
		--from-file=ca.crt="$pki_dir/client-ca-v2.crt"
	create_immutable_secret "${ROOT_NAME}-agent-tls-v2" tls \
		--cert="$pki_dir/client-v2.crt" --key="$pki_dir/client-v2.key"
	create_immutable_secret "${ROOT_NAME}-agent-tls-emergency-v2" tls \
		--cert="$pki_dir/client-emergency-v2.crt" --key="$pki_dir/client-emergency-v2.key"
	create_immutable_secret "${ROOT_NAME}-controller-server-ca-overlap-v2" generic \
		--from-file=ca.crt="$pki_dir/server-ca-overlap.crt"
	create_immutable_secret "${ROOT_NAME}-controller-server-ca-v2" generic \
		--from-file=ca.crt="$pki_dir/server-ca-v2.crt"
	note "created twelve compatible and three deliberately incompatible immutable, versioned, unowned TLS rotation/recovery Secrets"
}

assert_tls_secrets_unchanged() {
	local secret secret_json owners data_hash
	for secret in "${!tls_secret_uids[@]}"; do
		secret_json=$("$KUBECTL_BIN" -n "$TARGET_NAMESPACE" get secret "$secret" -o json)
		owners=$("$JQ_BIN" -c '.metadata.ownerReferences // []' <<<"$secret_json")
		data_hash=$("$JQ_BIN" -cS '.data' <<<"$secret_json" | sha256sum | awk '{print $1}')
		[[ $owners == '[]' ]] || die "input TLS Secret $secret acquired an owner reference"
		[[ $("$JQ_BIN" -r '.metadata.uid' <<<"$secret_json") == "${tls_secret_uids[$secret]}" ]] || \
			die "input TLS Secret $secret was replaced"
		[[ $data_hash == "${tls_secret_hashes[$secret]}" ]] || \
			die "input TLS Secret $secret data changed"
	done
	note "all TLS input Secrets retained their UIDs, data hashes, and unowned state"
}

write_bearer_header() {
	local token_file=$1
	local header_file=$2
	printf 'Authorization: Bearer ' >"$header_file"
	tr -d '\r\n' <"$token_file" >>"$header_file"
	printf '\n' >>"$header_file"
	chmod 0600 "$token_file" "$header_file"
}

assert_tls_curl_failure() {
	local label=$1
	local expected_pattern=$2
	local log_file=$3
	shift 3
	local rc
	set +e
	"$@" >/dev/null 2>"$log_file"
	rc=$?
	set -e
	((rc != 0)) || die "$label unexpectedly completed a TLS request"
	if ! grep -Eiq "$expected_pattern" "$log_file"; then
		printf '%s\n' "$(<"$log_file")" >&2
		die "$label failed without the expected TLS error class"
	fi
	kill -0 "$port_forward_pid" >/dev/null 2>&1 || die "$label coincided with a dead port-forward"
}

exercise_agent_bad_certificate_readiness() {
	local rogue_secret="${ROOT_NAME}-rogue-agent-tls"
	local pki_dir="$work_dir/pki"
	local generation deadline pods_json agent_logs=

	"$KUBECTL_BIN" -n "$TARGET_NAMESPACE" create secret tls "$rogue_secret" \
		--cert="$pki_dir/rogue-client.crt" --key="$pki_dir/rogue-client.key" >/dev/null
	[[ $("$KUBECTL_BIN" -n "$TARGET_NAMESPACE" get secret "$rogue_secret" \
		-o json | "$JQ_BIN" -c '.metadata.ownerReferences // []') == '[]' ]] || \
		die "rogue auth-probe Secret unexpectedly has an owner"

	"$KUBECTL_BIN" patch kubeneuron "$ROOT_NAME" --type=merge \
		-p "{\"spec\":{\"tls\":{\"clientSecretRef\":{\"name\":\"${rogue_secret}\"}}}}" >/dev/null
	generation=$("$KUBECTL_BIN" get kubeneuron "$ROOT_NAME" -o jsonpath='{.metadata.generation}')
	wait_observed_generation "$generation"
	# The rogue-cert rollout stalls fail-closed at maxUnavailable: only the
	# replaced agent loses readiness while the rest keep their previous
	# working identity, so a bad rotation cannot silently break the fleet.
	wait_agent_registration_readiness degraded

	deadline=$((SECONDS + TIMEOUT_SECONDS))
	while ((SECONDS < deadline)); do
		pods_json=$("$KUBECTL_BIN" -n "$TARGET_NAMESPACE" get pods \
			-l "app.kubernetes.io/instance=${ROOT_NAME},app.kubernetes.io/component=agent" -o json)
		# Aggregate every agent pod's logs: on a multi-node cluster the
		# daemonset/ log selector picks one arbitrary pod, which is usually a
		# healthy agent that never saw the rogue certificate.
		agent_logs=$(while read -r pod; do
			[[ -n $pod ]] || continue
			"$KUBECTL_BIN" -n "$TARGET_NAMESPACE" logs "$pod" \
				--all-containers --tail=-1 2>&1 || true
		done < <("$JQ_BIN" -r '.items[].metadata.name' <<<"$pods_json"))
		if "$JQ_BIN" -e '
          any(.items[];
            .status.phase == "Running" and
            (.status.containerStatuses | length) > 0 and
            all(.status.containerStatuses[];
              .restartCount == 0 and .state.running != null) and
            ([.status.conditions[]? | select(.type == "Ready" and .status == "True")] | length) == 0)
        ' <<<"$pods_json" >/dev/null && \
			grep -Eiq 'initial registration failed.*(tls|certificate)|tls.*(unknown|certificate)' <<<"$agent_logs"; then
			break
		fi
		sleep 2
	done
	if ((SECONDS >= deadline)); then
		printf '%s\n' "$agent_logs" >&2
		die "rogue client certificate did not leave the managed agent Running, unready, and restart-free"
	fi

	"$KUBECTL_BIN" patch kubeneuron "$ROOT_NAME" --type=merge \
		-p "{\"spec\":{\"tls\":{\"clientSecretRef\":{\"name\":\"${ROOT_NAME}-agent-tls\"}}}}" >/dev/null
	generation=$("$KUBECTL_BIN" get kubeneuron "$ROOT_NAME" -o jsonpath='{.metadata.generation}')
	wait_observed_generation "$generation"
	"$KUBECTL_BIN" -n "$TARGET_NAMESPACE" rollout status \
		daemonset/"${ROOT_NAME}-agent" --timeout="${TIMEOUT_SECONDS}s" >/dev/null
	wait_root_condition True RuntimeAvailable
	"$KUBECTL_BIN" -n "$TARGET_NAMESPACE" delete secret "$rogue_secret" \
		--wait=true --timeout=60s >/dev/null
	note "a managed agent with a rogue client certificate stayed Running/restart-free but unready, then recovered after restoring valid identity"
}

exercise_agent_authentication() {
	local service_dns="${ROOT_NAME}-controller.${TARGET_NAMESPACE}.svc"
	local agent_pod token_file header_file wrong_audience_token wrong_audience_header wrong_sa_token wrong_sa_header
	local port_forward_log public_code operator_unauth_code operator_auth_code webhook_unauth_code webhook_auth_code
	local plaintext_code plaintext_rc no_token_code malformed_code wrong_audience_code wrong_sa_code
	local valid_code mismatch_code event_code other_cert_code final_valid_code stale_code
	local agent_local_port='' public_local_port='' agent_base=''
	local -a valid_tls=()
	local pki_dir="$work_dir/pki"

	agent_pod=$("$KUBECTL_BIN" -n "$TARGET_NAMESPACE" get pods \
		-l "app.kubernetes.io/instance=${ROOT_NAME},app.kubernetes.io/component=agent" \
		-o jsonpath='{.items[0].metadata.name}')
	[[ -n $agent_pod ]] || die "could not find a managed agent Pod for the authentication test"
	token_file="$work_dir/agent-token"
	"$KUBECTL_BIN" -n "$TARGET_NAMESPACE" create token "${ROOT_NAME}-agent" \
		--audience="$agentTokenAudience" --duration=10m \
		--bound-object-kind=Pod --bound-object-name="$agent_pod" >"$token_file"
	header_file="$work_dir/agent-authorization-header"
	write_bearer_header "$token_file" "$header_file"

	wrong_audience_token="$work_dir/wrong-audience-token"
	"$KUBECTL_BIN" -n "$TARGET_NAMESPACE" create token "${ROOT_NAME}-agent" \
		--audience=kubeneuron-wrong-audience --duration=10m \
		--bound-object-kind=Pod --bound-object-name="$agent_pod" >"$wrong_audience_token"
	wrong_audience_header="$work_dir/wrong-audience-header"
	write_bearer_header "$wrong_audience_token" "$wrong_audience_header"

	wrong_sa_token="$work_dir/wrong-service-account-token"
	"$KUBECTL_BIN" -n "$TARGET_NAMESPACE" create token default \
		--audience="$agentTokenAudience" --duration=10m >"$wrong_sa_token"
	wrong_sa_header="$work_dir/wrong-service-account-header"
	write_bearer_header "$wrong_sa_token" "$wrong_sa_header"

	port_forward_log="$work_dir/controller-port-forward.log"
	start_auth_port_forward() {
		local deadline
		if [[ -n ${port_forward_pid:-} ]]; then
			kill "$port_forward_pid" >/dev/null 2>&1 || true
			wait "$port_forward_pid" >/dev/null 2>&1 || true
			port_forward_pid=
		fi
		agent_local_port=
		public_local_port=
		: >"$port_forward_log"
		"$KUBECTL_BIN" -n "$TARGET_NAMESPACE" port-forward \
			"service/${ROOT_NAME}-controller" ":${agentIngressPort}" ":${controllerPort}" \
			>"$port_forward_log" 2>&1 &
		port_forward_pid=$!
		deadline=$((SECONDS + 30))
		while ((SECONDS < deadline)); do
			agent_local_port=$(sed -n "s/^Forwarding from 127\\.0\\.0\\.1:\\([0-9][0-9]*\\) -> ${agentIngressPort}$/\\1/p" "$port_forward_log")
			public_local_port=$(sed -n "s/^Forwarding from 127\\.0\\.0\\.1:\\([0-9][0-9]*\\) -> ${controllerPort}$/\\1/p" "$port_forward_log")
			[[ -n $agent_local_port && -n $public_local_port ]] && break
			kill -0 "$port_forward_pid" >/dev/null 2>&1 || {
				printf '%s\n' "$(<"$port_forward_log")" >&2
				die "controller port-forward exited early"
			}
			sleep 1
		done
		[[ -n $agent_local_port && -n $public_local_port ]] || die "controller port-forward did not allocate both local ports"
		agent_base="https://${service_dns}:${agent_local_port}"
		valid_tls=(
			--silent --show-error --noproxy '*' --max-time 10
			--resolve "${service_dns}:${agent_local_port}:127.0.0.1"
			--cacert "$pki_dir/server-ca.crt"
			--cert "$pki_dir/client.crt"
			--key "$pki_dir/client.key"
		)
	}
	start_auth_port_forward

	public_code=$(curl --silent --show-error --noproxy '*' --max-time 10 \
		-o /dev/null -w '%{http_code}' \
		"http://127.0.0.1:${public_local_port}/api/v1/events")
	[[ $public_code == 404 ]] || die "public listener agent route returned $public_code, want 404"
	operator_unauth_code=$(curl --silent --show-error --noproxy '*' --max-time 10 \
		-o /dev/null -w '%{http_code}' \
		"http://127.0.0.1:${public_local_port}/api/v1/incidents")
	[[ $operator_unauth_code == 401 ]] || die "unauthenticated public operator API returned $operator_unauth_code, want 401"
	operator_auth_code=$(curl --silent --show-error --noproxy '*' --max-time 10 \
		-H 'Authorization: Bearer integration-operator-token' -o /dev/null -w '%{http_code}' \
		"http://127.0.0.1:${public_local_port}/api/v1/incidents")
	[[ $operator_auth_code == 200 ]] || die "authenticated public operator API returned $operator_auth_code, want 200"
	webhook_unauth_code=$(curl --silent --show-error --noproxy '*' --max-time 10 \
		-H 'Content-Type: application/json' --data-binary '{"alerts":[]}' -o /dev/null -w '%{http_code}' \
		"http://127.0.0.1:${public_local_port}/api/v1/webhooks/alertmanager")
	[[ $webhook_unauth_code == 401 ]] || die "unauthenticated Alertmanager webhook returned $webhook_unauth_code, want 401"
	webhook_auth_code=$(curl --silent --show-error --noproxy '*' --max-time 10 \
		-H 'Authorization: Bearer integration-webhook-token' -H 'Content-Type: application/json' \
		--data-binary '{"alerts":[]}' -o /dev/null -w '%{http_code}' \
		"http://127.0.0.1:${public_local_port}/api/v1/webhooks/alertmanager")
	[[ $webhook_auth_code == 202 ]] || die "authenticated Alertmanager webhook returned $webhook_auth_code, want 202"

	set +e
	plaintext_code=$(curl --silent --show-error --noproxy '*' --max-time 10 \
		-o /dev/null -w '%{http_code}' \
		"http://127.0.0.1:${agent_local_port}/api/v1/agents/register/narrow-v1" \
		2>"$work_dir/plaintext-agent-port.log")
	plaintext_rc=$?
	set -e
	if ((plaintext_rc == 0)); then
		[[ $plaintext_code == 400 ]] || die "plaintext request to mTLS port returned $plaintext_code, want TLS rejection"
	else
		[[ $plaintext_code == 000 ]] || die "failed plaintext request reported unexpected HTTP code $plaintext_code"
	fi
	kill -0 "$port_forward_pid" >/dev/null 2>&1 || die "plaintext check coincided with a dead port-forward"

	no_token_code=$(curl "${valid_tls[@]}" -o /dev/null -w '%{http_code}' \
		"${agent_base}/api/v1/agents/register/narrow-v1")
	[[ $no_token_code == 401 ]] || die "valid mTLS without a token returned $no_token_code, want 401"
	malformed_code=$(curl "${valid_tls[@]}" -H 'Authorization: Basic invalid' -o /dev/null -w '%{http_code}' \
		"${agent_base}/api/v1/agents/register/narrow-v1")
	[[ $malformed_code == 401 ]] || die "malformed agent credential returned $malformed_code, want 401"
	wrong_audience_code=$(curl "${valid_tls[@]}" -H "@$wrong_audience_header" -o /dev/null -w '%{http_code}' \
		"${agent_base}/api/v1/agents/register/narrow-v1")
	[[ $wrong_audience_code == 401 ]] || die "wrong-audience Pod token returned $wrong_audience_code, want 401"
	wrong_sa_code=$(curl "${valid_tls[@]}" -H "@$wrong_sa_header" -o /dev/null -w '%{http_code}' \
		"${agent_base}/api/v1/agents/register/narrow-v1")
	[[ $wrong_sa_code == 403 ]] || die "wrong-ServiceAccount Pod token returned $wrong_sa_code, want 403"

	valid_code=$(curl "${valid_tls[@]}" -H "@$header_file" -o "$work_dir/capability-response" -w '%{http_code}' \
		"${agent_base}/api/v1/agents/register/narrow-v1")
	[[ $valid_code == 200 ]] || die "valid mTLS and bound token returned $valid_code, want 200"
	[[ $(<"$work_dir/capability-response") == kubeneuron-agent-registration/v1 ]] || \
		die "authenticated capability response was not exact"

	mismatch_code=$(curl "${valid_tls[@]}" -H "@$header_file" -H 'Content-Type: application/json' \
		--data-binary '{"name":"spoofed-node"}' -o /dev/null -w '%{http_code}' \
		"${agent_base}/api/v1/agents/register/narrow-v1")
	[[ $mismatch_code == 403 ]] || die "spoofed-node registration returned $mismatch_code, want 403"
	event_code=$(curl "${valid_tls[@]}" -H "@$header_file" -H 'Content-Type: application/json' \
		--data-binary '{"node":"spoofed-node","xid":79}' -o /dev/null -w '%{http_code}' \
		"${agent_base}/api/v1/events")
	[[ $event_code == 403 ]] || die "spoofed-node event returned $event_code, want 403"

	other_cert_code=$(curl --silent --show-error --noproxy '*' --max-time 10 \
		--resolve "${service_dns}:${agent_local_port}:127.0.0.1" \
		--cacert "$pki_dir/server-ca.crt" --cert "$pki_dir/other-client.crt" --key "$pki_dir/other-client.key" \
		-H "@$header_file" -o /dev/null -w '%{http_code}' \
		"${agent_base}/api/v1/agents/register/narrow-v1")
	[[ $other_cert_code == 403 ]] || die "wrong-installation client certificate returned $other_cert_code, want 403"

	# Kubernetes port-forward can tear down after a client-certificate handshake
	# alert. Start an independent forwarding session for every negative TLS
	# probe so one expected reset cannot make the next assertion a TCP failure.
	start_auth_port_forward
	assert_tls_curl_failure "missing client certificate" \
		'certificate required|alert certificate' "$work_dir/no-client-cert.log" \
		curl --silent --show-error --noproxy '*' --max-time 10 \
		--resolve "${service_dns}:${agent_local_port}:127.0.0.1" --cacert "$pki_dir/server-ca.crt" \
		"${agent_base}/api/v1/agents/register/narrow-v1"
	start_auth_port_forward
	assert_tls_curl_failure "rogue client certificate" \
		'unknown ca|bad certificate|certificate unknown' "$work_dir/rogue-client-cert.log" \
		curl --silent --show-error --noproxy '*' --max-time 10 \
		--resolve "${service_dns}:${agent_local_port}:127.0.0.1" --cacert "$pki_dir/server-ca.crt" \
		--cert "$pki_dir/rogue-client.crt" --key "$pki_dir/rogue-client.key" \
		"${agent_base}/api/v1/agents/register/narrow-v1"
	start_auth_port_forward
	assert_tls_curl_failure "wrong controller CA" \
		'certificate problem|unable to get local issuer certificate|self-signed certificate|unknown ca' \
		"$work_dir/wrong-server-ca.log" \
		curl --silent --show-error --noproxy '*' --max-time 10 \
		--resolve "${service_dns}:${agent_local_port}:127.0.0.1" --cacert "$pki_dir/rogue-ca.crt" \
		--cert "$pki_dir/client.crt" --key "$pki_dir/client.key" \
		"${agent_base}/api/v1/agents/register/narrow-v1"

	start_auth_port_forward
	final_valid_code=$(curl "${valid_tls[@]}" -H "@$header_file" -o /dev/null -w '%{http_code}' \
		"${agent_base}/api/v1/agents/register/narrow-v1")
	[[ $final_valid_code == 200 ]] || die "post-negative valid identity returned $final_valid_code, want 200"

	"$KUBECTL_BIN" -n "$TARGET_NAMESPACE" delete pod "$agent_pod" \
		--wait=true --timeout="${TIMEOUT_SECONDS}s" >/dev/null
	deadline=$((SECONDS + 30))
	stale_code=
	while ((SECONDS < deadline)); do
		stale_code=$(curl "${valid_tls[@]}" -H "@$header_file" -o /dev/null -w '%{http_code}' \
			"${agent_base}/api/v1/agents/register/narrow-v1")
		[[ $stale_code == 401 ]] && break
		sleep 1
	done
	[[ $stale_code == 401 ]] || die "token bound to a deleted Pod returned $stale_code, want 401"
	"$KUBECTL_BIN" -n "$TARGET_NAMESPACE" rollout status \
		daemonset/"${ROOT_NAME}-agent" --timeout="${TIMEOUT_SECONDS}s" >/dev/null

	kill "$port_forward_pid" >/dev/null 2>&1 || true
	wait "$port_forward_pid" >/dev/null 2>&1 || true
	port_forward_pid=
	note "public API and Alertmanager webhook required their distinct bearer tokens; the agent boundary rejected public/plaintext access, malformed/wrong tokens, missing/rogue certificates, wrong installation identity, deleted-Pod tokens, and arbitrary node payloads"
}

# exercise_controller_restart_mid_playbook drives an approval-gated dry-run
# ladder to AWAITING_APPROVAL, kills the controller Pod, and proves the
# SQLite workflow state survives: the incident is still parked, no dry-run
# step re-executes, and the post-restart approval resumes the ladder.
exercise_controller_restart_mid_playbook() {
	note "exercising a controller restart in the middle of an approval-gated playbook"
	local pf_log="$work_dir/restart-port-forward.log"
	local pf_pid= public_port= node_name incident_json incident_id state
	local audit_json cordon_count controller_pod deadline

	node_name=$("$KUBECTL_BIN" get nodes -o jsonpath='{.items[0].metadata.name}')
	[[ -n $node_name ]] || die "no node name for the restart scenario"

	start_restart_port_forward() {
		if [[ -n $pf_pid ]]; then
			kill "$pf_pid" >/dev/null 2>&1 || true
			wait "$pf_pid" >/dev/null 2>&1 || true
			pf_pid=
		fi
		public_port=
		: >"$pf_log"
		"$KUBECTL_BIN" -n "$TARGET_NAMESPACE" port-forward \
			"service/${ROOT_NAME}-controller" ":${controllerPort}" >"$pf_log" 2>&1 &
		pf_pid=$!
		local pf_deadline=$((SECONDS + 30))
		while ((SECONDS < pf_deadline)); do
			public_port=$(sed -n "s/^Forwarding from 127\\.0\\.0\\.1:\\([0-9][0-9]*\\) -> ${controllerPort}$/\\1/p" "$pf_log")
			[[ -n $public_port ]] && break
			kill -0 "$pf_pid" >/dev/null 2>&1 || {
				printf '%s\n' "$(<"$pf_log")" >&2
				die "restart-phase port-forward exited early"
			}
			sleep 1
		done
		[[ -n $public_port ]] || die "restart-phase port-forward allocated no local port"
	}
	stop_restart_port_forward() {
		if [[ -n $pf_pid ]]; then
			kill "$pf_pid" >/dev/null 2>&1 || true
			wait "$pf_pid" >/dev/null 2>&1 || true
			pf_pid=
		fi
	}
	api_curl() {
		curl --silent --show-error --noproxy '*' --max-time 10 \
			-H 'Authorization: Bearer integration-operator-token' "$@"
	}
	incident_state() {
		api_curl "http://127.0.0.1:${public_port}/api/v1/incidents?node=${node_name}" |
			"$JQ_BIN" -r --arg id "$incident_id" '.[] | select(.id == $id) | .state'
	}
	fetch_audit() {
		api_curl "http://127.0.0.1:${public_port}/api/v1/incidents/${incident_id}" |
			"$JQ_BIN" '.audit'
	}
	dry_run_step_count() {
		local step=$1
		"$JQ_BIN" --arg step "$step" \
			'[.[] | select(.action == $step and .dry_run == true and (.result | tostring | contains("DRY-RUN")))] | length' \
			<<<"$audit_json"
	}

	start_restart_port_forward
	local trigger_code
	trigger_code=$(api_curl -H 'Content-Type: application/json' \
		--data-binary "{\"node\":\"${node_name}\",\"class\":\"integration-restart-test\",\"actor\":\"harness\"}" \
		-o /dev/null -w '%{http_code}' \
		"http://127.0.0.1:${public_port}/api/v1/incidents")
	[[ $trigger_code == 202 ]] || die "manual restart-test incident returned $trigger_code, want 202"

	incident_id=$(api_curl "http://127.0.0.1:${public_port}/api/v1/incidents?node=${node_name}" |
		"$JQ_BIN" -r '[.[] | select(.class == "integration-restart-test")][0].id // empty')
	deadline=$((SECONDS + TIMEOUT_SECONDS))
	while [[ -z $incident_id ]] && ((SECONDS < deadline)); do
		sleep 1
		incident_id=$(api_curl "http://127.0.0.1:${public_port}/api/v1/incidents?node=${node_name}" |
			"$JQ_BIN" -r '[.[] | select(.class == "integration-restart-test")][0].id // empty')
	done
	[[ -n $incident_id ]] || die "restart-test incident never appeared"

	deadline=$((SECONDS + TIMEOUT_SECONDS))
	state=
	while ((SECONDS < deadline)); do
		state=$(incident_state)
		[[ $state == AWAITING_APPROVAL ]] && break
		sleep 1
	done
	[[ $state == AWAITING_APPROVAL ]] || die "incident state is ${state:-unknown}, want AWAITING_APPROVAL before the restart"
	audit_json=$(fetch_audit)
	cordon_count=$(dry_run_step_count cordon)
	[[ $cordon_count == 1 ]] || die "cordon dry-run recorded $cordon_count times before restart, want 1"

	controller_pod=$("$KUBECTL_BIN" -n "$TARGET_NAMESPACE" get pods \
		-l "app.kubernetes.io/instance=${ROOT_NAME},app.kubernetes.io/component=controller" \
		-o jsonpath='{.items[0].metadata.name}')
	[[ -n $controller_pod ]] || die "no controller Pod to restart"
	stop_restart_port_forward
	"$KUBECTL_BIN" -n "$TARGET_NAMESPACE" delete pod "$controller_pod" \
		--wait=true --timeout="${TIMEOUT_SECONDS}s" >/dev/null
	"$KUBECTL_BIN" -n "$TARGET_NAMESPACE" rollout status \
		deployment/"${ROOT_NAME}-controller" --timeout="${TIMEOUT_SECONDS}s" >/dev/null
	start_restart_port_forward

	deadline=$((SECONDS + 30))
	state=
	while ((SECONDS < deadline)); do
		state=$(incident_state 2>/dev/null || true)
		[[ -n $state ]] && break
		sleep 1
	done
	[[ $state == AWAITING_APPROVAL ]] || \
		die "incident state after restart is ${state:-unreachable}, want the durable AWAITING_APPROVAL"
	audit_json=$(fetch_audit)
	cordon_count=$(dry_run_step_count cordon)
	[[ $cordon_count == 1 ]] || die "cordon dry-run recorded $cordon_count times after restart, want exactly 1 (no re-execution)"

	local approve_code
	approve_code=$(api_curl -H 'Content-Type: application/json' \
		--data-binary '{"actor":"harness-approver"}' -o /dev/null -w '%{http_code}' \
		"http://127.0.0.1:${public_port}/api/v1/incidents/${incident_id}/approve")
	[[ $approve_code == 204 ]] || die "post-restart approval returned $approve_code, want 204"

	deadline=$((SECONDS + TIMEOUT_SECONDS))
	state=
	while ((SECONDS < deadline)); do
		state=$(incident_state)
		[[ $state == VERIFYING || $state == RESOLVED ]] && break
		sleep 1
	done
	[[ $state == VERIFYING || $state == RESOLVED ]] || \
		die "incident state after approval is ${state:-unknown}, want VERIFYING or RESOLVED"
	audit_json=$(fetch_audit)
	local reboot_count approver_entries
	reboot_count=$(dry_run_step_count reboot)
	[[ $reboot_count == 1 ]] || die "reboot dry-run recorded $reboot_count times, want exactly 1"
	approver_entries=$("$JQ_BIN" '[.[] | select(.actor == "token:harness-approver")] | length' <<<"$audit_json")
	((approver_entries >= 1)) || die "audit trail does not record the post-restart approver identity"

	stop_restart_port_forward
	note "controller restart preserved the parked approval, re-executed nothing, and the post-restart approval resumed the ladder to ${state}"
}

rotation_phase() {
	local direction=$1
	local phase=$2
	local rotation_id=$3
	local timeout=${4:-$TIMEOUT_SECONDS}
	local approve=${5:-0}
	local -a material_args
	if [[ $direction == server ]]; then
		material_args=(
			--from-leaf-secret "${ROOT_NAME}-controller-tls"
			--from-ca-secret "${ROOT_NAME}-controller-server-ca"
			--new-leaf-secret "${ROOT_NAME}-controller-tls-v2"
			--overlap-ca-secret "${ROOT_NAME}-controller-server-ca-overlap-v2"
			--final-ca-secret "${ROOT_NAME}-controller-server-ca-v2"
		)
	else
		material_args=(
			--from-leaf-secret "${ROOT_NAME}-agent-tls"
			--from-ca-secret "${ROOT_NAME}-agent-client-ca"
			--new-leaf-secret "${ROOT_NAME}-agent-tls-v2"
			--overlap-ca-secret "${ROOT_NAME}-agent-client-ca-overlap-v2"
			--final-ca-secret "${ROOT_NAME}-agent-client-ca-v2"
		)
	fi
	local -a command=(
		"$ROTATION_SCRIPT" "$direction" --phase "$phase" --root "$ROOT_NAME"
		--rotation-id "$rotation_id" "${material_args[@]}"
		--kubeconfig "$KUBECONFIG_PATH" --timeout-seconds "$timeout"
	)
	if ((approve)); then
		command+=(--approve-retire-old-trust)
	fi
	KUBECTL_BIN="$KUBECTL_BIN" JQ_BIN="$JQ_BIN" "${command[@]}"
}

assert_rotation_rollout_scope() {
	local direction=$1
	local phase=$2
	local rotation_id=$3
	local approve=${4:-0}
	local controller_before controller_after agent_before agent_after
	controller_before=$("$KUBECTL_BIN" -n "$TARGET_NAMESPACE" get \
		deployment "${ROOT_NAME}-controller" -o jsonpath='{.metadata.generation}')
	agent_before=$("$KUBECTL_BIN" -n "$TARGET_NAMESPACE" get \
		daemonset "${ROOT_NAME}-agent" -o jsonpath='{.metadata.generation}')
	rotation_phase "$direction" "$phase" "$rotation_id" "$TIMEOUT_SECONDS" "$approve"
	controller_after=$("$KUBECTL_BIN" -n "$TARGET_NAMESPACE" get \
		deployment "${ROOT_NAME}-controller" -o jsonpath='{.metadata.generation}')
	agent_after=$("$KUBECTL_BIN" -n "$TARGET_NAMESPACE" get \
		daemonset "${ROOT_NAME}-agent" -o jsonpath='{.metadata.generation}')

	local expected=consumer
	[[ $phase == activate-leaf || $phase == rollback-leaf ]] && expected=producer
	if [[ $direction == server && $expected == consumer || $direction == client && $expected == producer ]]; then
		((agent_after > agent_before)) || die "$direction $phase did not advance the agent DaemonSet generation"
		[[ $controller_after == "$controller_before" ]] || \
			die "$direction $phase unexpectedly changed the controller Deployment generation"
	else
		((controller_after > controller_before)) || die "$direction $phase did not advance the controller Deployment generation"
		[[ $agent_after == "$agent_before" ]] || \
			die "$direction $phase unexpectedly changed the agent DaemonSet generation"
	fi
	note "$direction $phase rolled only its intended workload"
}

exercise_failed_server_rotation_and_rollback() {
	local rotation_id=server-failure-probe
	local root_json generation_before generation_after controller_logs deadline
	local -a rollback_args=(
		--from-leaf-secret "${ROOT_NAME}-controller-tls"
		--from-ca-secret "${ROOT_NAME}-controller-server-ca"
		--new-leaf-secret "${ROOT_NAME}-controller-tls-bad-v2"
		--overlap-ca-secret "${ROOT_NAME}-controller-server-ca-overlap-bad-leaf"
		--final-ca-secret "${ROOT_NAME}-controller-server-ca-final-bad-leaf"
	)
	KUBECTL_BIN="$KUBECTL_BIN" JQ_BIN="$JQ_BIN" "$ROTATION_SCRIPT" server \
		--phase expand-trust --root "$ROOT_NAME" --rotation-id "$rotation_id" \
		"${rollback_args[@]}" --kubeconfig "$KUBECONFIG_PATH" \
		--timeout-seconds "$TIMEOUT_SECONDS" >/dev/null

	generation_before=$("$KUBECTL_BIN" get kubeneuron "$ROOT_NAME" -o jsonpath='{.metadata.generation}')
	set +e
	rotation_phase client expand-trust "$rotation_id" 60 0 \
		>"$work_dir/same-id-direction-collision.log" 2>&1
	local collision_rc=$?
	set -e
	((collision_rc != 0)) || die "same rotation ID hijacked an active opposite-direction transaction"
	generation_after=$("$KUBECTL_BIN" get kubeneuron "$ROOT_NAME" -o jsonpath='{.metadata.generation}')
	[[ $generation_after == "$generation_before" ]] || die "rejected same-ID direction collision changed root generation"

	local -a inconsistent_command=(
		"$ROTATION_SCRIPT" server --phase activate-leaf --root "$ROOT_NAME"
		--rotation-id "$rotation_id"
		--from-leaf-secret "${ROOT_NAME}-controller-tls"
		--from-ca-secret "${ROOT_NAME}-controller-server-ca"
		--new-leaf-secret "${ROOT_NAME}-controller-tls-v2"
		--overlap-ca-secret "${ROOT_NAME}-controller-server-ca-overlap-bad-leaf"
		--final-ca-secret "${ROOT_NAME}-controller-server-ca-final-bad-leaf"
		--kubeconfig "$KUBECONFIG_PATH" --timeout-seconds 60
	)
	set +e
	KUBECTL_BIN="$KUBECTL_BIN" JQ_BIN="$JQ_BIN" "${inconsistent_command[@]}" \
		>"$work_dir/inconsistent-rotation-plan.log" 2>&1
	local inconsistent_rc=$?
	set -e
	((inconsistent_rc != 0)) || die "active rotation accepted a substituted new-leaf Secret"
	generation_after=$("$KUBECTL_BIN" get kubeneuron "$ROOT_NAME" -o jsonpath='{.metadata.generation}')
	[[ $generation_after == "$generation_before" ]] || die "rejected inconsistent plan changed root generation"

	local -a bad_command=(
		"$ROTATION_SCRIPT" server --phase activate-leaf --root "$ROOT_NAME"
		--rotation-id "$rotation_id" "${rollback_args[@]}"
		--kubeconfig "$KUBECONFIG_PATH" --timeout-seconds 60
	)
	set +e
	KUBECTL_BIN="$KUBECTL_BIN" JQ_BIN="$JQ_BIN" "${bad_command[@]}" \
		>"$work_dir/failed-server-rotation.log" 2>&1
	local rc=$?
	set -e
	((rc != 0)) || die "mismatched server key unexpectedly completed a rotation"
	wait_root_condition False RuntimeUnavailable
	root_json=$("$KUBECTL_BIN" get kubeneuron "$ROOT_NAME" -o json)
	"$JQ_BIN" -e '
      .metadata.annotations["kubeneuron.io/tls-rotation-phase"] == "LeafActivated" and
      .spec.tls.serverSecretRef.name == "integration-smoke-controller-tls-bad-v2"
	    ' <<<"$root_json" >/dev/null || die "failed server rotation phase was not durably recorded"

	deadline=$((SECONDS + 30))
	while ((SECONDS < deadline)); do
		controller_logs=$("$KUBECTL_BIN" -n "$TARGET_NAMESPACE" logs \
			deployment/"${ROOT_NAME}-controller" --all-containers --tail=-1 2>&1 || true)
		grep -Eiq 'private key.*(does not match|mismatch)|tls.*private key' <<<"$controller_logs" && break
		sleep 2
	done
	grep -Eiq 'private key.*(does not match|mismatch)|tls.*private key' <<<"$controller_logs" || {
		printf '%s\n' "$controller_logs" >&2
		die "invalid server rollout did not expose the expected certificate/key mismatch"
	}

	local removed
	for removed in \
		"${ROOT_NAME}-controller-tls-bad-v2" \
		"${ROOT_NAME}-controller-server-ca-final-bad-leaf"; do
		"$KUBECTL_BIN" -n "$TARGET_NAMESPACE" delete secret "$removed" \
			--wait=true --timeout=60s >/dev/null
		unset 'tls_secret_uids['"$removed"']'
		unset 'tls_secret_hashes['"$removed"']'
	done
	KUBECTL_BIN="$KUBECTL_BIN" JQ_BIN="$JQ_BIN" "$ROTATION_SCRIPT" server \
		--phase rollback-leaf --root "$ROOT_NAME" --rotation-id "$rotation_id" \
		"${rollback_args[@]}" --kubeconfig "$KUBECONFIG_PATH" \
		--timeout-seconds "$TIMEOUT_SECONDS" >/dev/null
	KUBECTL_BIN="$KUBECTL_BIN" JQ_BIN="$JQ_BIN" "$ROTATION_SCRIPT" server \
		--phase rollback-trust --root "$ROOT_NAME" --rotation-id "$rotation_id" \
		"${rollback_args[@]}" --kubeconfig "$KUBECONFIG_PATH" \
		--timeout-seconds "$TIMEOUT_SECONDS" >/dev/null
	wait_root_condition True RuntimeAvailable
	assert_runtime_ready
	note "same-ID/plan collisions were rejected; invalid server material produced RuntimeUnavailable and rollback succeeded after failed/unused candidates were quarantined"
}

exercise_failed_trust_retirement_and_rollback() {
	local rotation_id=server-retirement-failure-probe
	local root_json
	local -a material_args=(
		--from-leaf-secret "${ROOT_NAME}-controller-tls"
		--from-ca-secret "${ROOT_NAME}-controller-server-ca"
		--new-leaf-secret "${ROOT_NAME}-controller-tls-retire-probe"
		--overlap-ca-secret "${ROOT_NAME}-controller-server-ca-overlap-retire-probe"
		--final-ca-secret "${ROOT_NAME}-controller-server-ca-bad-retire-probe"
	)
	local phase
	for phase in expand-trust activate-leaf; do
		KUBECTL_BIN="$KUBECTL_BIN" JQ_BIN="$JQ_BIN" "$ROTATION_SCRIPT" server \
			--phase "$phase" --root "$ROOT_NAME" --rotation-id "$rotation_id" \
			"${material_args[@]}" --kubeconfig "$KUBECONFIG_PATH" \
			--timeout-seconds "$TIMEOUT_SECONDS" >/dev/null
	done

	set +e
	KUBECTL_BIN="$KUBECTL_BIN" JQ_BIN="$JQ_BIN" "$ROTATION_SCRIPT" server \
		--phase retire-old-trust --root "$ROOT_NAME" --rotation-id "$rotation_id" \
		"${material_args[@]}" --approve-retire-old-trust \
		--kubeconfig "$KUBECONFIG_PATH" --timeout-seconds 60 \
		>"$work_dir/failed-trust-retirement.log" 2>&1
	local rc=$?
	set -e
	((rc != 0)) || die "incompatible final server CA unexpectedly completed trust retirement"
	wait_root_condition False RuntimeUnavailable
	root_json=$("$KUBECTL_BIN" get kubeneuron "$ROOT_NAME" -o json)
	"$JQ_BIN" -e '
	  .metadata.annotations["kubeneuron.io/tls-rotation-phase"] == "OldTrustRetired" and
	  .spec.tls.serverCASecretRef.name == "integration-smoke-controller-server-ca-bad-retire-probe" and
	  .spec.tls.serverSecretRef.name == "integration-smoke-controller-tls-retire-probe"
	' <<<"$root_json" >/dev/null || die "failed trust retirement was not durably recorded"

	KUBECTL_BIN="$KUBECTL_BIN" JQ_BIN="$JQ_BIN" "$ROTATION_SCRIPT" server \
		--phase rollback-retirement --root "$ROOT_NAME" --rotation-id "$rotation_id" \
		"${material_args[@]}" --kubeconfig "$KUBECONFIG_PATH" \
		--timeout-seconds "$TIMEOUT_SECONDS" >/dev/null
	wait_root_condition True RuntimeAvailable
	KUBECTL_BIN="$KUBECTL_BIN" JQ_BIN="$JQ_BIN" "$ROTATION_SCRIPT" server \
		--phase rollback-leaf --root "$ROOT_NAME" --rotation-id "$rotation_id" \
		"${material_args[@]}" --kubeconfig "$KUBECONFIG_PATH" \
		--timeout-seconds "$TIMEOUT_SECONDS" >/dev/null
	KUBECTL_BIN="$KUBECTL_BIN" JQ_BIN="$JQ_BIN" "$ROTATION_SCRIPT" server \
		--phase rollback-trust --root "$ROOT_NAME" --rotation-id "$rotation_id" \
		"${material_args[@]}" --kubeconfig "$KUBECONFIG_PATH" \
		--timeout-seconds "$TIMEOUT_SECONDS" >/dev/null
	wait_root_condition True RuntimeAvailable
	assert_runtime_ready
	note "incompatible trust contraction produced RuntimeUnavailable and recovered through final-to-overlap, leaf, then trust rollback"
}

assert_post_rotation_identity() {
	local service_dns="${ROOT_NAME}-controller.${TARGET_NAMESPACE}.svc"
	local pki_dir="$work_dir/pki"
	local agent_pod token_file header_file port_forward_log deadline valid_code
	local agent_local_port=''
	agent_pod=$("$KUBECTL_BIN" -n "$TARGET_NAMESPACE" get pods \
		-l "app.kubernetes.io/instance=${ROOT_NAME},app.kubernetes.io/component=agent" \
		-o jsonpath='{.items[0].metadata.name}')
	[[ -n $agent_pod ]] || die "could not find the rotated managed agent Pod"
	token_file="$work_dir/rotated-agent-token"
	"$KUBECTL_BIN" -n "$TARGET_NAMESPACE" create token "${ROOT_NAME}-agent" \
		--audience="$agentTokenAudience" --duration=10m \
		--bound-object-kind=Pod --bound-object-name="$agent_pod" >"$token_file"
	header_file="$work_dir/rotated-agent-header"
	write_bearer_header "$token_file" "$header_file"

	port_forward_log="$work_dir/rotated-controller-port-forward.log"
	local agent_base=''
	start_rotated_port_forward() {
		if [[ -n ${port_forward_pid:-} ]]; then
			kill "$port_forward_pid" >/dev/null 2>&1 || true
			wait "$port_forward_pid" >/dev/null 2>&1 || true
			port_forward_pid=
		fi
		agent_local_port=
		: >"$port_forward_log"
		"$KUBECTL_BIN" -n "$TARGET_NAMESPACE" port-forward \
			"service/${ROOT_NAME}-controller" ":${agentIngressPort}" >"$port_forward_log" 2>&1 &
		port_forward_pid=$!
		deadline=$((SECONDS + 30))
		while ((SECONDS < deadline)); do
			agent_local_port=$(sed -n "s/^Forwarding from 127\\.0\\.0\\.1:\\([0-9][0-9]*\\) -> ${agentIngressPort}$/\\1/p" "$port_forward_log")
			[[ -n $agent_local_port ]] && break
			kill -0 "$port_forward_pid" >/dev/null 2>&1 || die "rotated controller port-forward exited early"
			sleep 1
		done
		[[ -n $agent_local_port ]] || die "rotated controller port-forward did not allocate a local port"
		agent_base="https://${service_dns}:${agent_local_port}"
	}
	start_rotated_port_forward
	valid_code=$(curl --silent --show-error --noproxy '*' --max-time 10 \
		--resolve "${service_dns}:${agent_local_port}:127.0.0.1" \
		--cacert "$pki_dir/server-ca-v2.crt" \
		--cert "$pki_dir/client-v2.crt" --key "$pki_dir/client-v2.key" \
		-H "@$header_file" -o /dev/null -w '%{http_code}' \
		"${agent_base}/api/v1/agents/register/narrow-v1")
	[[ $valid_code == 200 ]] || die "rotated certificate pair returned $valid_code, want 200"

	# Each expected handshake alert may terminate kubectl port-forward on this
	# host. Use a fresh forwarding session per negative TLS assertion so the
	# assertion remains about TLS rather than a follow-on TCP refusal.
	start_rotated_port_forward
	assert_tls_curl_failure "retired client certificate" \
		'unknown ca|bad certificate|certificate unknown' "$work_dir/retired-client-cert.log" \
		curl --silent --show-error --noproxy '*' --max-time 10 \
		--resolve "${service_dns}:${agent_local_port}:127.0.0.1" \
		--cacert "$pki_dir/server-ca-v2.crt" \
		--cert "$pki_dir/client.crt" --key "$pki_dir/client.key" \
		-H "@$header_file" "${agent_base}/api/v1/agents/register/narrow-v1"
	start_rotated_port_forward
	assert_tls_curl_failure "retired server CA" \
		'certificate problem|unable to get local issuer certificate|self-signed certificate|unknown ca' \
		"$work_dir/retired-server-ca.log" \
		curl --silent --show-error --noproxy '*' --max-time 10 \
		--resolve "${service_dns}:${agent_local_port}:127.0.0.1" \
		--cacert "$pki_dir/server-ca.crt" \
		--cert "$pki_dir/client-v2.crt" --key "$pki_dir/client-v2.key" \
		-H "@$header_file" "${agent_base}/api/v1/agents/register/narrow-v1"

	kill "$port_forward_pid" >/dev/null 2>&1 || true
	wait "$port_forward_pid" >/dev/null 2>&1 || true
	port_forward_pid=
	note "new server/client identities succeeded and both retired trust directions were rejected"
}

exercise_routine_tls_rotation() {
	local digest_before digest_after controller_before controller_after agent_before agent_after
	digest_before=$("$KUBECTL_BIN" get kubeneuron "$ROOT_NAME" -o jsonpath='{.status.configDigest}')
	set +e
	"$KUBECTL_BIN" -n "$TARGET_NAMESPACE" patch secret \
		"${ROOT_NAME}-controller-server-ca-v2" --type=merge \
		-p '{"data":{"mutation-probe":"cmVqZWN0"}}' >/dev/null 2>&1
	local immutable_rc=$?
	set -e
	((immutable_rc != 0)) || die "immutable TLS candidate unexpectedly accepted a data mutation"

	exercise_failed_server_rotation_and_rollback
	exercise_failed_trust_retirement_and_rollback
	assert_rotation_rollout_scope server expand-trust server-routine-v2
	assert_rotation_rollout_scope server activate-leaf server-routine-v2
	set +e
	rotation_phase server retire-old-trust server-routine-v2 "$TIMEOUT_SECONDS" 0 \
		>"$work_dir/unapproved-trust-retirement.log" 2>&1
	local unapproved_rc=$?
	set -e
	((unapproved_rc != 0)) || die "old server trust retired without explicit approval"
	assert_rotation_rollout_scope server retire-old-trust server-routine-v2 1

	controller_before=$("$KUBECTL_BIN" -n "$TARGET_NAMESPACE" get \
		deployment "${ROOT_NAME}-controller" -o jsonpath='{.metadata.generation}')
	agent_before=$("$KUBECTL_BIN" -n "$TARGET_NAMESPACE" get \
		daemonset "${ROOT_NAME}-agent" -o jsonpath='{.metadata.generation}')
	rotation_phase server retire-old-trust server-routine-v2 "$TIMEOUT_SECONDS" 1
	controller_after=$("$KUBECTL_BIN" -n "$TARGET_NAMESPACE" get \
		deployment "${ROOT_NAME}-controller" -o jsonpath='{.metadata.generation}')
	agent_after=$("$KUBECTL_BIN" -n "$TARGET_NAMESPACE" get \
		daemonset "${ROOT_NAME}-agent" -o jsonpath='{.metadata.generation}')
	[[ $controller_after == "$controller_before" && $agent_after == "$agent_before" ]] || \
		die "idempotent completed server phase changed a workload generation"

	assert_rotation_rollout_scope client expand-trust client-routine-v2
	assert_rotation_rollout_scope client activate-leaf client-routine-v2
	assert_rotation_rollout_scope client retire-old-trust client-routine-v2 1
	assert_post_rotation_identity

	digest_after=$("$KUBECTL_BIN" get kubeneuron "$ROOT_NAME" -o jsonpath='{.status.configDigest}')
	[[ $digest_after == "$digest_before" ]] || die "TLS rotation changed the compiled configuration digest"
	"$KUBECTL_BIN" get kubeneuron "$ROOT_NAME" -o json | "$JQ_BIN" -e '
      .metadata.annotations["kubeneuron.io/tls-rotation-id"] == "client-routine-v2" and
      .metadata.annotations["kubeneuron.io/tls-rotation-direction"] == "client" and
	      .metadata.annotations["kubeneuron.io/tls-rotation-phase"] == "OldTrustRetired" and
	      .metadata.annotations["kubeneuron.io/tls-rotation-from-leaf"] == "integration-smoke-agent-tls" and
	      .metadata.annotations["kubeneuron.io/tls-rotation-new-leaf"] == "integration-smoke-agent-tls-v2" and
	      .metadata.annotations["kubeneuron.io/tls-rotation-overlap-ca"] == "integration-smoke-agent-client-ca-overlap-v2" and
	      .metadata.annotations["kubeneuron.io/tls-rotation-final-ca"] == "integration-smoke-agent-client-ca-v2" and
	      (.metadata.annotations["kubeneuron.io/tls-rotation-from-leaf-uid"] | length) > 0 and
	      (.metadata.annotations["kubeneuron.io/tls-rotation-new-leaf-uid"] | length) > 0 and
	      (.metadata.annotations["kubeneuron.io/tls-rotation-overlap-ca-uid"] | length) > 0 and
	      (.metadata.annotations["kubeneuron.io/tls-rotation-final-ca-uid"] | length) > 0 and
      .spec.tls.serverSecretRef.name == "integration-smoke-controller-tls-v2" and
      .spec.tls.serverCASecretRef.name == "integration-smoke-controller-server-ca-v2" and
      .spec.tls.clientSecretRef.name == "integration-smoke-agent-tls-v2" and
      .spec.tls.clientCASecretRef.name == "integration-smoke-agent-client-ca-v2"
	    ' >/dev/null || die "root did not retain the final versioned TLS references and current transaction annotations"
	assert_runtime_ready
	note "routine server and client rotations completed through expand/activate/retire with fresh post-controller acknowledgments, scoped rollouts, plan/UID binding, and an idempotent terminal phase"
}

exercise_emergency_tls_leaf_recovery() {
	local bad_server="${ROOT_NAME}-controller-tls-emergency-bad-v2"
	local server_candidate="${ROOT_NAME}-controller-tls-emergency-v2"
	local client_candidate="${ROOT_NAME}-agent-tls-emergency-v2"
	local generation root_json

	"$KUBECTL_BIN" patch kubeneuron "$ROOT_NAME" --type=merge \
		-p "{\"spec\":{\"tls\":{\"serverSecretRef\":{\"name\":\"${bad_server}\"}}}}" >/dev/null
	generation=$("$KUBECTL_BIN" get kubeneuron "$ROOT_NAME" -o jsonpath='{.metadata.generation}')
	wait_observed_generation "$generation"
	wait_root_condition False RuntimeUnavailable

	KUBECTL_BIN="$KUBECTL_BIN" JQ_BIN="$JQ_BIN" "$EMERGENCY_TLS_SCRIPT" \
		--root "$ROOT_NAME" --recovery-id server-client-leaf-emergency-v2 \
		--server-leaf-secret "$server_candidate" --client-leaf-secret "$client_candidate" \
		--approve-emergency-leaf-recovery --kubeconfig "$KUBECONFIG_PATH" \
		--timeout-seconds "$TIMEOUT_SECONDS" >/dev/null
	wait_root_condition True RuntimeAvailable
	assert_runtime_ready
	root_json=$("$KUBECTL_BIN" get kubeneuron "$ROOT_NAME" -o json)
	"$JQ_BIN" -e --arg server "$server_candidate" --arg client "$client_candidate" '
      .metadata.annotations["kubeneuron.io/tls-emergency-recovery-id"] == "server-client-leaf-emergency-v2" and
      .metadata.annotations["kubeneuron.io/tls-emergency-recovery-server-leaf"] == $server and
      (.metadata.annotations["kubeneuron.io/tls-emergency-recovery-server-leaf-uid"] | length) > 0 and
      .metadata.annotations["kubeneuron.io/tls-emergency-recovery-client-leaf"] == $client and
      (.metadata.annotations["kubeneuron.io/tls-emergency-recovery-client-leaf-uid"] | length) > 0 and
      .metadata.annotations["kubeneuron.io/tls-emergency-recovery-previous-server-leaf"] == "integration-smoke-controller-tls-emergency-bad-v2" and
      .metadata.annotations["kubeneuron.io/tls-emergency-recovery-previous-client-leaf"] == "integration-smoke-agent-tls-v2" and
      .spec.tls.serverSecretRef.name == $server and .spec.tls.clientSecretRef.name == $client and
      .spec.tls.serverCASecretRef.name == "integration-smoke-controller-server-ca-v2" and
      .spec.tls.clientCASecretRef.name == "integration-smoke-agent-client-ca-v2"
    ' <<<"$root_json" >/dev/null || die "emergency recovery annotations or retained CA references are incomplete"
	note "bad server leaf produced RuntimeUnavailable; explicit dual-leaf emergency recovery retained both CAs, rolled both workloads, and restored Ready"
}

for name in REUSE_CLUSTER KEEP_CLUSTER KEEP_RESOURCES BUILD_IMAGES; do
	require_boolean "$name" "${!name}"
done
require_positive_integer TIMEOUT_SECONDS "$TIMEOUT_SECONDS"
[[ $CLUSTER_NAME =~ ^[a-zA-Z0-9][a-zA-Z0-9._-]*$ ]] || die "unsafe CLUSTER_NAME: $CLUSTER_NAME"
[[ $RUN_ID =~ ^[a-zA-Z0-9][a-zA-Z0-9._-]*$ ]] || die "unsafe RUN_ID: $RUN_ID"
for image in "$OPERATOR_IMAGE" "$CONTROLLER_IMAGE" "$AGENT_IMAGE"; do
	[[ $image =~ ^[a-zA-Z0-9][a-zA-Z0-9._/:@-]*$ ]] || die "unsafe image reference: $image"
done

for command in "$KIND_BIN" "$KUBECTL_BIN" "$DOCKER_BIN" "$JQ_BIN" awk curl openssl sed grep sha256sum stat; do
	command -v "$command" >/dev/null 2>&1 || die "required command not found: $command"
done
[[ -r $CEL_SCRIPT ]] || die "missing CEL harness: $CEL_SCRIPT"
[[ -x $ROTATION_SCRIPT ]] || die "missing executable TLS rotation helper: $ROTATION_SCRIPT"
[[ -x $EMERGENCY_TLS_SCRIPT ]] || die "missing executable emergency TLS helper: $EMERGENCY_TLS_SCRIPT"
[[ -r $SMOKE_TEMPLATE ]] || die "missing smoke template: $SMOKE_TEMPLATE"
[[ -r $IMAGE_DOCKERFILE ]] || die "missing integration image Dockerfile: $IMAGE_DOCKERFILE"

kind_version=$("$KIND_BIN" version | awk 'NR == 1 {print $2}')
[[ $kind_version == "$EXPECTED_KIND_VERSION" ]] || \
	die "kind $EXPECTED_KIND_VERSION is required (found ${kind_version:-unknown})"
kubectl_version=$("$KUBECTL_BIN" version --client=true -o json | \
	"$JQ_BIN" -r '.clientVersion.gitVersion')
[[ $kubectl_version == "$EXPECTED_KUBECTL_VERSION" ]] || \
	die "kubectl $EXPECTED_KUBECTL_VERSION is required (found ${kubectl_version:-unknown})"
if ! docker_version=$("$DOCKER_BIN" info --format '{{.ServerVersion}}' 2>&1); then
	die "Docker is unavailable: $docker_version (a stale login may need: sg docker -c '$0')"
fi
note "using kind $kind_version, kubectl $kubectl_version, and Docker $docker_version"

umask 077
work_dir=$(mktemp -d "${TMPDIR:-/tmp}/kubeneuron-integration.XXXXXX")
mkdir -p -- "$(dirname -- "$KUBECONFIG_PATH")"
if [[ -e $KUBECONFIG_PATH || -L $KUBECONFIG_PATH ]]; then
	die "refusing to truncate pre-existing KUBECONFIG_PATH $KUBECONFIG_PATH"
fi
kubeconfig_temp=$(mktemp "${KUBECONFIG_PATH}.tmp.XXXXXX")

if cluster_exists; then
	((REUSE_CLUSTER == 1)) || \
		die "cluster $CLUSTER_NAME already exists; set REUSE_CLUSTER=1 to use it"
	"$KIND_BIN" get kubeconfig --name "$CLUSTER_NAME" >"$kubeconfig_temp"
	note "reusing cluster $CLUSTER_NAME"
else
	# Multi-node by default: a per-node GPU remediation product must not be
	# integration-tested only on single-node clusters. Every node pins the
	# same digest-pinned image.
	[[ $WORKER_NODES =~ ^[0-9]+$ ]] || die "WORKER_NODES must be a non-negative integer"
	kind_config="$work_dir/kind-cluster.yaml"
	{
		printf 'kind: Cluster\napiVersion: kind.x-k8s.io/v1alpha4\nnodes:\n'
		printf -- '- role: control-plane\n  image: %s\n' "$NODE_IMAGE"
		for ((i = 0; i < WORKER_NODES; i++)); do
			printf -- '- role: worker\n  image: %s\n' "$NODE_IMAGE"
		done
	} >"$kind_config"
	"$KIND_BIN" create cluster --name "$CLUSTER_NAME" --config "$kind_config" \
		--kubeconfig "$kubeconfig_temp" --wait "${TIMEOUT_SECONDS}s"
	created_cluster=1
	note "created cluster $CLUSTER_NAME with 1 control-plane and $WORKER_NODES worker node(s)"
fi
kubeconfig_temp_identity=$(stat --format='%d:%i' -- "$kubeconfig_temp")
if ! ln --no-target-directory -- "$kubeconfig_temp" "$KUBECONFIG_PATH"; then
	die "refusing to replace KUBECONFIG_PATH created concurrently at $KUBECONFIG_PATH"
fi
kubeconfig_identity=$(stat --format='%d:%i' -- "$KUBECONFIG_PATH")
if [[ $kubeconfig_identity != "$kubeconfig_temp_identity" ]]; then
	die "KUBECONFIG_PATH changed while it was being installed: $KUBECONFIG_PATH"
fi
kubeconfig_created=1
rm -f -- "$kubeconfig_temp"
kubeconfig_temp=
export KUBECONFIG=$KUBECONFIG_PATH

mapfile -t kind_nodes < <("$KIND_BIN" get nodes --name "$CLUSTER_NAME")
((${#kind_nodes[@]} > 0)) || die "kind returned no nodes for $CLUSTER_NAME"
for node in "${kind_nodes[@]}"; do
	actual_node_image=$("$DOCKER_BIN" inspect "$node" --format '{{.Config.Image}}')
	[[ $actual_node_image == "$NODE_IMAGE" ]] || \
		die "node $node uses $actual_node_image, want pinned $NODE_IMAGE"
	"$DOCKER_BIN" exec "$node" test -c /dev/kmsg || \
		die "kind node $node does not expose /dev/kmsg as a character device"
done
server_version=$("$KUBECTL_BIN" version -o json | "$JQ_BIN" -r '.serverVersion.gitVersion')
[[ $server_version == "$EXPECTED_SERVER_VERSION" ]] || \
	die "server is $server_version, want $EXPECTED_SERVER_VERSION"
"$KUBECTL_BIN" wait --for=condition=Ready nodes --all --timeout="${TIMEOUT_SECONDS}s" >/dev/null
"$KUBECTL_BIN" -n local-path-storage rollout status \
	deployment/local-path-provisioner --timeout="${TIMEOUT_SECONDS}s" >/dev/null
note "verified $server_version from the pinned node image"

if "$KUBECTL_BIN" get namespace "$TARGET_NAMESPACE" >/dev/null 2>&1; then
	die "refusing to replace existing fixture namespace $TARGET_NAMESPACE"
fi
if "$KUBECTL_BIN" get crd/kubeneurons.kubeneuron.io >/dev/null 2>&1 && \
	"$KUBECTL_BIN" get kubeneuron "$ROOT_NAME" >/dev/null 2>&1; then
	die "refusing to replace existing KubeNeuron fixture $ROOT_NAME"
fi
if "$KUBECTL_BIN" get crd/gpuplaybooks.kubeneuron.io >/dev/null 2>&1 && \
	"$KUBECTL_BIN" get gpuplaybook "$PLAYBOOK_NAME" >/dev/null 2>&1; then
	die "refusing to replace existing GPUPlaybook fixture $PLAYBOOK_NAME"
fi
if "$KUBECTL_BIN" get crd/gpuremediationpolicies.kubeneuron.io >/dev/null 2>&1 && \
	"$KUBECTL_BIN" get gpuremediationpolicy "$POLICY_NAME" >/dev/null 2>&1; then
	die "refusing to replace existing GPURemediationPolicy fixture $POLICY_NAME"
fi

note "installing seven generated CRDs"
"$KUBECTL_BIN" apply -k "$REPO_ROOT/config/crd" >/dev/null
mapfile -t crd_names < <(
	for file in "$REPO_ROOT"/config/crd/bases/*.yaml; do
		sed -n 's/^  name: \(.*\.kubeneuron\.io\)$/\1/p' "$file"
	done
)
((${#crd_names[@]} == 7)) || die "found ${#crd_names[@]} generated CRDs, want 7"
for crd in "${crd_names[@]}"; do
	"$KUBECTL_BIN" wait --for=condition=Established "crd/$crd" \
		--timeout="${TIMEOUT_SECONDS}s" >/dev/null
done

note "running the 63-case CEL admission matrix"
CEL_ALLOW_CLUSTER_MUTATION=1 KUBECTL_BIN="$KUBECTL_BIN" JQ_BIN="$JQ_BIN" bash "$CEL_SCRIPT"

if ((BUILD_IMAGES)); then
	for binary in kubeneuron-operator kubeneuron-controller kubeneuron-agent; do
		[[ -x $REPO_ROOT/bin/$binary ]] || \
			die "missing static binary bin/$binary; run 'make build' first"
	done
	for target_and_image in \
		"operator=$OPERATOR_IMAGE" \
		"controller=$CONTROLLER_IMAGE" \
		"agent=$AGENT_IMAGE"; do
		target=${target_and_image%%=*}
		image=${target_and_image#*=}
		note "packaging static integration target $target as $image"
		"$DOCKER_BIN" build --target "$target" --tag "$image" \
			--file "$IMAGE_DOCKERFILE" "$REPO_ROOT"
	done
else
	for image in "$OPERATOR_IMAGE" "$CONTROLLER_IMAGE" "$AGENT_IMAGE"; do
		"$DOCKER_BIN" image inspect "$image" >/dev/null 2>&1 || \
			die "BUILD_IMAGES=0 but local image is absent: $image"
	done
fi

note "loading local images into kind"
"$KIND_BIN" load docker-image --name "$CLUSTER_NAME" \
	"$OPERATOR_IMAGE" "$CONTROLLER_IMAGE" "$AGENT_IMAGE"

operator_manifest="$work_dir/operator-deployment.yaml"
# Tag-agnostic: config/default pins a released tag that changes per release.
sed -E "s|image: ghcr\.io/kubeneuron/kube-neuron/operator:[A-Za-z0-9._-]+(@sha256:[a-f0-9]+)?|image: $OPERATOR_IMAGE|" \
	"$REPO_ROOT/config/default/operator_deployment.yaml" >"$operator_manifest"
grep -Fq "image: $OPERATOR_IMAGE" "$operator_manifest" || die "operator image substitution failed"
if grep -Fq 'ghcr.io/kubeneuron/kube-neuron/operator:latest' "$operator_manifest"; then
	die "operator manifest retained its published image placeholder"
fi

note "installing operator namespace, RBAC, and local Deployment"
"$KUBECTL_BIN" apply -f "$REPO_ROOT/config/default/namespace.yaml" >/dev/null
"$KUBECTL_BIN" apply -k "$REPO_ROOT/config/rbac" >/dev/null
"$KUBECTL_BIN" apply -f "$operator_manifest" >/dev/null
"$KUBECTL_BIN" -n "$OPERATOR_NAMESPACE" patch deployment "$OPERATOR_DEPLOYMENT" \
	--type=strategic \
	-p '{"spec":{"template":{"spec":{"containers":[{"name":"operator","imagePullPolicy":"Never"}]}}}}' >/dev/null
"$KUBECTL_BIN" -n "$OPERATOR_NAMESPACE" rollout status \
	deployment/"$OPERATOR_DEPLOYMENT" --timeout="${TIMEOUT_SECONDS}s" >/dev/null
actual_operator_image=$("$KUBECTL_BIN" -n "$OPERATOR_NAMESPACE" get \
	deployment "$OPERATOR_DEPLOYMENT" -o jsonpath='{.spec.template.spec.containers[?(@.name=="operator")].image}')
[[ $actual_operator_image == "$OPERATOR_IMAGE" ]] || \
	die "operator Deployment uses $actual_operator_image, want $OPERATOR_IMAGE"
assert_operator_rbac

smoke_manifest="$work_dir/operator-smoke.yaml"
sed -e "s|@CONTROLLER_IMAGE@|$CONTROLLER_IMAGE|g" \
	-e "s|@AGENT_IMAGE@|$AGENT_IMAGE|g" \
	"$SMOKE_TEMPLATE" >"$smoke_manifest"
if grep -Eq '@(CONTROLLER|AGENT)_IMAGE@' "$smoke_manifest"; then
	die "smoke image substitution failed"
fi

note "creating the operator smoke fixtures"
fixture_applied=1
"$KUBECTL_BIN" apply -f "$smoke_manifest" >/dev/null
root_uid=$("$KUBECTL_BIN" get kubeneuron "$ROOT_NAME" -o jsonpath='{.metadata.uid}')
[[ -n $root_uid ]] || die "KubeNeuron fixture has no UID"
generate_tls_fixtures "$root_uid"
create_tls_secrets
generate_rotated_tls_fixtures "$root_uid"
wait_root_condition True RuntimeAvailable
assert_runtime_ready
assert_all_owners "$root_uid"
assert_child_configuration_statuses

controller_subject="system:serviceaccount:${TARGET_NAMESPACE}:${ROOT_NAME}-controller"
controller_tokenreview=$("$KUBECTL_BIN" auth can-i create tokenreviews.authentication.k8s.io \
	--as="$controller_subject" 2>/dev/null || true)
[[ $controller_tokenreview == yes ]] || \
	die "managed controller cannot create TokenReviews (got ${controller_tokenreview:-empty})"
for target in \
	"serviceaccount/${ROOT_NAME}-agent" \
	"daemonset.apps/${ROOT_NAME}-agent"; do
	controller_target_get=$("$KUBECTL_BIN" -n "$TARGET_NAMESPACE" auth can-i get "$target" \
		--as="$controller_subject" 2>/dev/null || true)
	[[ $controller_target_get == yes ]] || die "managed controller cannot get $target"
done
for other in \
	"serviceaccount/default" \
	"daemonset.apps/not-the-managed-agent"; do
	controller_other_get=$("$KUBECTL_BIN" -n "$TARGET_NAMESPACE" auth can-i get "$other" \
		--as="$controller_subject" 2>/dev/null || true)
	[[ $controller_other_get == no ]] || die "managed controller unexpectedly can get $other"
done
note "managed controller TokenReview and named ServiceAccount/DaemonSet reads are least-privilege scoped"

deployment_generation=$("$KUBECTL_BIN" -n "$TARGET_NAMESPACE" get \
	deployment "${ROOT_NAME}-controller" -o jsonpath='{.metadata.generation}')
digest_before=$("$KUBECTL_BIN" get kubeneuron "$ROOT_NAME" -o jsonpath='{.status.configDigest}')
root_generation_before=$("$KUBECTL_BIN" get kubeneuron "$ROOT_NAME" -o jsonpath='{.metadata.generation}')
"$KUBECTL_BIN" patch kubeneuron "$ROOT_NAME" --type=merge \
	-p '{"spec":{"approvals":{}}}' >/dev/null
root_generation_after=$("$KUBECTL_BIN" get kubeneuron "$ROOT_NAME" -o jsonpath='{.metadata.generation}')
((root_generation_after > root_generation_before)) || \
	die "explicit-default approvals patch did not advance the root generation"
wait_observed_generation "$root_generation_after"
wait_root_condition True RuntimeAvailable
digest_after=$("$KUBECTL_BIN" get kubeneuron "$ROOT_NAME" -o jsonpath='{.status.configDigest}')
deployment_generation_after=$("$KUBECTL_BIN" -n "$TARGET_NAMESPACE" get \
	deployment "${ROOT_NAME}-controller" -o jsonpath='{.metadata.generation}')
[[ $digest_after == "$digest_before" ]] || die "explicit default changed compiled digest"
[[ $deployment_generation_after == "$deployment_generation" ]] || \
	die "acknowledged no-op reconcile changed Deployment generation $deployment_generation -> $deployment_generation_after"
note "acknowledged explicit-default reconcile left digest and Deployment generation unchanged"

wait_agent_log 'controller registration acknowledged'
exercise_agent_bad_certificate_readiness
exercise_agent_authentication
wait_root_condition True RuntimeAvailable
assert_runtime_ready
exercise_controller_restart_mid_playbook
wait_root_condition True RuntimeAvailable
assert_runtime_ready
exercise_routine_tls_rotation
exercise_emergency_tls_leaf_recovery

note "exercising registration-readiness loss and recovery during controller outage"
scale_operator 0 registration-outage
"$KUBECTL_BIN" -n "$TARGET_NAMESPACE" scale \
	deployment "${ROOT_NAME}-controller" --replicas=0 >/dev/null
"$KUBECTL_BIN" -n "$TARGET_NAMESPACE" rollout status \
	deployment/"${ROOT_NAME}-controller" --timeout="${TIMEOUT_SECONDS}s" >/dev/null
wait_agent_registration_readiness stale
wait_agent_log 'controller registration acknowledgment lost'

"$KUBECTL_BIN" -n "$TARGET_NAMESPACE" scale \
	deployment "${ROOT_NAME}-controller" --replicas=1 >/dev/null
"$KUBECTL_BIN" -n "$TARGET_NAMESPACE" rollout status \
	deployment/"${ROOT_NAME}-controller" --timeout="${TIMEOUT_SECONDS}s" >/dev/null
wait_agent_registration_readiness ready
wait_agent_log 'controller registration acknowledgment recovered'
scale_operator 1
wait_root_condition True RuntimeAvailable
assert_runtime_ready
note "agent readiness became stale during controller outage and recovered after a durable acknowledgment"

note "inducing an unowned runtime ConfigMap collision"
scale_operator 0 collision-setup
"$KUBECTL_BIN" -n "$TARGET_NAMESPACE" delete \
	configmap "${ROOT_NAME}-runtime" --wait=true --timeout=60s >/dev/null
"$KUBECTL_BIN" -n "$TARGET_NAMESPACE" create configmap "${ROOT_NAME}-runtime" \
	--from-literal=owner=someone-else >/dev/null
sentinel_uid=$("$KUBECTL_BIN" -n "$TARGET_NAMESPACE" get \
	configmap "${ROOT_NAME}-runtime" -o jsonpath='{.metadata.uid}')
sentinel_json=$("$KUBECTL_BIN" -n "$TARGET_NAMESPACE" get \
	configmap "${ROOT_NAME}-runtime" -o json)
"$JQ_BIN" -e '
  .data == {"owner":"someone-else"} and
  ((.metadata.ownerReferences // []) | length) == 0
' <<<"$sentinel_json" >/dev/null || die "collision sentinel was not created unowned and unchanged"
collision_generation=$("$KUBECTL_BIN" -n "$TARGET_NAMESPACE" get \
	deployment "${ROOT_NAME}-controller" -o jsonpath='{.metadata.generation}')

scale_operator 1
expected_collision_message="reconcile runtime ConfigMap: ConfigMap \"${TARGET_NAMESPACE}/${ROOT_NAME}-runtime\" already exists and is not controlled by KubeNeuron \"${ROOT_NAME}\""
wait_root_condition False ReconciliationFailed "$expected_collision_message"
sentinel_after=$("$KUBECTL_BIN" -n "$TARGET_NAMESPACE" get \
	configmap "${ROOT_NAME}-runtime" -o json)
"$JQ_BIN" -e --arg uid "$sentinel_uid" '
  .metadata.uid == $uid and
  .data == {"owner":"someone-else"} and
  ((.metadata.ownerReferences // []) | length) == 0
' <<<"$sentinel_after" >/dev/null || die "operator adopted or mutated the collision sentinel"
collision_generation_after=$("$KUBECTL_BIN" -n "$TARGET_NAMESPACE" get \
	deployment "${ROOT_NAME}-controller" -o jsonpath='{.metadata.generation}')
[[ $collision_generation_after == "$collision_generation" ]] || \
	die "collision changed Deployment generation $collision_generation -> $collision_generation_after"
note "collision produced Ready=False/ReconciliationFailed without adoption or Deployment churn"

note "removing only the sentinel and restarting the operator for deterministic recovery"
scale_operator 0 collision-recovery
"$KUBECTL_BIN" -n "$TARGET_NAMESPACE" delete \
	configmap "${ROOT_NAME}-runtime" --wait=true --timeout=60s >/dev/null
scale_operator 1
wait_root_condition True RuntimeAvailable
assert_runtime_ready
recovered_json=$("$KUBECTL_BIN" -n "$TARGET_NAMESPACE" get \
	configmap "${ROOT_NAME}-runtime" -o json)
"$JQ_BIN" -e --arg old_uid "$sentinel_uid" --arg root_uid "$root_uid" '
  .metadata.uid != $old_uid and
  .data["config-digest"] != null and
  .data["policies.yaml"] != null and
  .data.owner == null and
  (.metadata.ownerReferences | length) == 1 and
  .metadata.ownerReferences[0].uid == $root_uid and
  .metadata.ownerReferences[0].controller == true
' <<<"$recovered_json" >/dev/null || die "recovered runtime ConfigMap is not freshly owned configuration"
recovery_generation=$("$KUBECTL_BIN" -n "$TARGET_NAMESPACE" get \
	deployment "${ROOT_NAME}-controller" -o jsonpath='{.metadata.generation}')
[[ $recovery_generation == "$collision_generation" ]] || \
	die "recovery changed Deployment generation $collision_generation -> $recovery_generation"
assert_all_owners "$root_uid"
assert_tls_secrets_unchanged

operator_logs=$("$KUBECTL_BIN" -n "$OPERATOR_NAMESPACE" logs \
	deployment/"$OPERATOR_DEPLOYMENT" --all-containers --tail=-1 2>&1)
if grep -Eiq 'forbidden|object has been modified|panic|fatal' <<<"$operator_logs"; then
	die "operator logs contain an unexpected RBAC/concurrency/fatal error"
fi

if "$KUBECTL_BIN" get nodes -o json | "$JQ_BIN" -e \
	'any(.items[]; .status.capacity["nvidia.com/gpu"] != null)' >/dev/null; then
	note "GPU capacity is advertised, but this harness deliberately makes no NVIDIA/DCGM claim"
else
	note "kind advertises no nvidia.com/gpu capacity, as expected for this CPU-only smoke"
fi
agent_logs=$("$KUBECTL_BIN" -n "$TARGET_NAMESPACE" logs \
	daemonset/"${ROOT_NAME}-agent" --all-containers --tail=100 2>&1 || true)
if grep -Fq 'real NVML driver not wired yet; using fake driver (skeleton)' <<<"$agent_logs"; then
	note "agent explicitly reports its fake NVML skeleton"
fi

	note "PASS: 63 CEL checks, scoped RBAC, mTLS plus Pod/node identity rejection, authenticated public API and Alertmanager webhook, manual immutable/versioned routine TLS rotation, explicit dual-leaf emergency recovery, stale-state/plan rejection, failed-leaf and failed-contraction rollback, fresh registration proof, durable readiness loss/recovery, 11 owners, ownership collision/non-adoption/recovery, and acknowledged no-op reconciliation, plus a controller restart mid-playbook with durable approval state and no re-executed step"
note "CPU-only boundary: this validates transport, the tested manual TLS-rotation and leaf-recovery contracts, and Kubernetes workload identity; it does not validate automatic issuance/expiry detection, emergency CA revocation, NVIDIA, NVML, DCGM, GPU actions, or remediation"
