#!/usr/bin/env bash
set -euo pipefail

# N-1 -> N upgrade scenario on kind, following docs/upgrade.md exactly:
# install the released BASELINE (CRDs + operator + workloads from GHCR),
# seed durable workflow state through the operator API, then upgrade in the
# documented order (CRDs -> operator -> controller/agent images) to the
# locally built HEAD and prove:
#   - the root object converges back to Ready/RuntimeAvailable,
#   - the pre-upgrade incident and its audit trail survive the SQLite
#     schema migration,
#   - the upgraded CRD actually serves new-schema fields (hostTooling),
#   - operator logs stay free of RBAC/concurrency/fatal errors.
#
# Requirements: docker (logged in to ghcr.io or `gh auth` available), kind,
# kubectl, jq, curl, openssl, gh. The baseline images are pulled from the
# real release, so this cannot run air-gapped.
#
# Env:
#   BASELINE          released tag to start from (default: v0.1.0)
#   CLUSTER_NAME      kind cluster (default: kubeneuron-upgrade; created if
#                     absent, deleted on exit unless KEEP_CLUSTER=1)
#   KEEP_CLUSTER=1    retain the cluster for inspection

BASELINE=${BASELINE:-v0.1.0}
CLUSTER_NAME=${CLUSTER_NAME:-kubeneuron-upgrade}
KIND_BIN=${KIND_BIN:-kind}
KUBECTL_BIN=${KUBECTL_BIN:-kubectl}
JQ_BIN=${JQ_BIN:-jq}
DOCKER_BIN=${DOCKER_BIN:-docker}
TIMEOUT_SECONDS=${TIMEOUT_SECONDS:-300}

REPO_ROOT=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
readonly ROOT_NAME=kubeneuron
readonly NS=kube-neuron
readonly OPERATOR_TOKEN=upgrade-test-operator-token

note() { printf 'upgrade: %s\n' "$*"; }
die() {
	printf 'upgrade FAIL: %s\n' "$*" >&2
	exit 1
}

work_dir=$(mktemp -d)
port_forward_pid=
created_cluster=0
baseline_tree=
cleanup() {
	local status=$?
	if [[ -n $port_forward_pid ]]; then
		kill "$port_forward_pid" >/dev/null 2>&1 || true
		wait "$port_forward_pid" >/dev/null 2>&1 || true
	fi
	if ((status != 0)); then
		note "failure diagnostics follow"
		"$KUBECTL_BIN" get kubeneuron "$ROOT_NAME" -o yaml 2>&1 || true
		"$KUBECTL_BIN" -n "$NS" get pods -o wide 2>&1 || true
		"$KUBECTL_BIN" -n "$NS" logs deployment/kubeneuron-operator --all-containers --tail=100 2>&1 || true
	fi
	if [[ -n $baseline_tree ]]; then
		git -C "$REPO_ROOT" worktree remove --force "$baseline_tree" >/dev/null 2>&1 || true
	fi
	if ((created_cluster)) && [[ ${KEEP_CLUSTER:-0} != 1 ]]; then
		"$KIND_BIN" delete cluster --name "$CLUSTER_NAME" >/dev/null 2>&1 || true
	fi
	rm -rf -- "$work_dir"
}
trap cleanup EXIT

for command in "$KIND_BIN" "$KUBECTL_BIN" "$JQ_BIN" "$DOCKER_BIN" curl openssl gh; do
	command -v "$command" >/dev/null 2>&1 || die "required command not found: $command"
done

note "downloading the $BASELINE release manifest and image digests"
gh release download "$BASELINE" -p "kubeneuron-install-$BASELINE.yaml" -p images.txt \
	--dir "$work_dir" --clobber
install_manifest="$work_dir/kubeneuron-install-$BASELINE.yaml"
[[ -s $install_manifest ]] || die "release install manifest is empty"

declare -A baseline_image=()
while IFS= read -r ref; do
	[[ -n $ref ]] || continue
	name=${ref%%@*}
	baseline_image[${name##*/}]=$ref
done <"$work_dir/images.txt"
for component in operator controller agent; do
	[[ -n ${baseline_image[$component]:-} ]] || die "release images.txt lacks $component"
done

pull_baseline() {
	local component ref
	for component in operator controller agent; do
		ref=${baseline_image[$component]}
		if ! "$DOCKER_BIN" pull --quiet "$ref" >/dev/null 2>&1; then
			gh auth token | "$DOCKER_BIN" login ghcr.io \
				--username "$(gh api user -q .login)" --password-stdin >/dev/null 2>&1 || true
			"$DOCKER_BIN" pull --quiet "$ref" >/dev/null 2>&1 || return 1
		fi
		# The install manifest and the root object reference tag form; retag
		# the digest-pinned pull so kind serves both reference styles.
		"$DOCKER_BIN" tag "$ref" "ghcr.io/kubeneuron/kube-neuron/${component}:${BASELINE}"
	done
}

build_baseline_from_tag() {
	# Registry pulls need a token with read:packages; without one, build the
	# baseline from the release tag instead — identical source, binaries,
	# and schema migrations, just not the published artifact bytes.
	note "building baseline images from git tag $BASELINE (no registry access)"
	baseline_tree="$work_dir/baseline-src"
	git -C "$REPO_ROOT" worktree add --detach "$baseline_tree" "$BASELINE" >/dev/null
	make -C "$baseline_tree" build >/dev/null
	local target
	for target in operator controller agent; do
		"$DOCKER_BIN" build --target "$target" \
			--tag "ghcr.io/kubeneuron/kube-neuron/${target}:${BASELINE}" \
			--file "$baseline_tree/test/integration/Dockerfile" "$baseline_tree" >/dev/null
	done
}

note "obtaining the $BASELINE baseline images"
if ! pull_baseline; then
	build_baseline_from_tag
fi

if ! "$KIND_BIN" get clusters 2>/dev/null | grep -Fxq "$CLUSTER_NAME"; then
	note "creating kind cluster $CLUSTER_NAME"
	"$KIND_BIN" create cluster --name "$CLUSTER_NAME" --wait "${TIMEOUT_SECONDS}s" \
		--kubeconfig "$work_dir/kubeconfig"
	created_cluster=1
else
	"$KIND_BIN" export kubeconfig --name "$CLUSTER_NAME" --kubeconfig "$work_dir/kubeconfig"
fi
export KUBECONFIG="$work_dir/kubeconfig"

note "loading baseline images into kind"
"$KIND_BIN" load docker-image --name "$CLUSTER_NAME" \
	"ghcr.io/kubeneuron/kube-neuron/operator:${BASELINE}" \
	"ghcr.io/kubeneuron/kube-neuron/controller:${BASELINE}" \
	"ghcr.io/kubeneuron/kube-neuron/agent:${BASELINE}"

note "installing baseline $BASELINE (CRDs + operator)"
"$KUBECTL_BIN" apply -f "$install_manifest" >/dev/null
"$KUBECTL_BIN" -n "$NS" rollout status deployment/kubeneuron-operator \
	--timeout="${TIMEOUT_SECONDS}s" >/dev/null

note "creating token Secrets and the baseline root object"
"$KUBECTL_BIN" -n "$NS" create secret generic kubeneuron-operator-api-token \
	--from-literal=token="$OPERATOR_TOKEN" >/dev/null
"$KUBECTL_BIN" -n "$NS" create secret generic kubeneuron-alertmanager-webhook-token \
	--from-literal=token=upgrade-test-webhook-token >/dev/null

config_fixture="$work_dir/config.yaml"
cat >"$config_fixture" <<CONFIG
apiVersion: kubeneuron.io/v1alpha1
kind: GPUPlaybook
metadata:
  name: upgrade-observe
spec:
  kubeNeuronRef: $ROOT_NAME
  target: GPU
  steps:
    - name: observe
      action: Observe
---
apiVersion: kubeneuron.io/v1alpha1
kind: GPURemediationPolicy
metadata:
  name: upgrade-policy
spec:
  kubeNeuronRef: $ROOT_NAME
  priority: 1
  match:
    class: upgrade-test
  playbookRef: upgrade-observe
CONFIG
"$KUBECTL_BIN" apply -f "$config_fixture" >/dev/null

root_fixture="$work_dir/kubeneuron.yaml"
cat >"$root_fixture" <<ROOT
apiVersion: kubeneuron.io/v1alpha1
kind: KubeNeuron
metadata:
  name: $ROOT_NAME
spec:
  namespace: $NS
  controller:
    image: ghcr.io/kubeneuron/kube-neuron/controller:$BASELINE
  agent:
    image: ghcr.io/kubeneuron/kube-neuron/agent:$BASELINE
    tolerations:
      - operator: Exists
  safety:
    executionMode: DryRun
  notifications:
    operatorAPIToken:
      name: kubeneuron-operator-api-token
    webhookToken:
      name: kubeneuron-alertmanager-webhook-token
  workflowStore:
    type: SQLite
    sqlite:
      size: 1Gi
  observability:
    victoriaMetrics:
      mode: External
      endpoint: http://vmsingle-unused.$NS.svc:8428
    alertmanager:
      mode: External
      endpoint: http://alertmanager-unused.$NS.svc:9093
  tls:
    serverSecretRef:
      name: kubeneuron-controller-tls
    clientCASecretRef:
      name: kubeneuron-agent-client-ca
    clientSecretRef:
      name: kubeneuron-agent-tls
    serverCASecretRef:
      name: kubeneuron-controller-server-ca
ROOT
"$KUBECTL_BIN" apply -f "$root_fixture" >/dev/null
root_uid=$("$KUBECTL_BIN" get kubeneuron "$ROOT_NAME" -o jsonpath='{.metadata.uid}')
[[ -n $root_uid ]] || die "root object has no UID"

note "generating the four TLS Secrets (install.md contract)"
pki="$work_dir/pki"
mkdir -p "$pki"
openssl ecparam -name prime256v1 -genkey -noout -out "$pki/server-ca.key" 2>/dev/null
openssl req -x509 -new -key "$pki/server-ca.key" -out "$pki/server-ca.crt" \
	-subj "/CN=upgrade-server-ca" -days 30 >/dev/null 2>&1
openssl ecparam -name prime256v1 -genkey -noout -out "$pki/client-ca.key" 2>/dev/null
openssl req -x509 -new -key "$pki/client-ca.key" -out "$pki/client-ca.crt" \
	-subj "/CN=upgrade-client-ca" -days 30 >/dev/null 2>&1
openssl ecparam -name prime256v1 -genkey -noout -out "$pki/server.key" 2>/dev/null
openssl req -new -key "$pki/server.key" -out "$pki/server.csr" \
	-subj "/CN=${ROOT_NAME}-controller.${NS}.svc" >/dev/null 2>&1
openssl x509 -req -in "$pki/server.csr" -CA "$pki/server-ca.crt" -CAkey "$pki/server-ca.key" \
	-CAcreateserial -out "$pki/server.crt" -days 20 \
	-extfile <(printf 'extendedKeyUsage=serverAuth\nsubjectAltName=DNS:%s-controller.%s.svc\n' "$ROOT_NAME" "$NS") \
	>/dev/null 2>&1
openssl ecparam -name prime256v1 -genkey -noout -out "$pki/client.key" 2>/dev/null
openssl req -new -key "$pki/client.key" -out "$pki/client.csr" -subj "/" >/dev/null 2>&1
openssl x509 -req -in "$pki/client.csr" -CA "$pki/client-ca.crt" -CAkey "$pki/client-ca.key" \
	-CAcreateserial -out "$pki/client.crt" -days 20 \
	-extfile <(printf 'extendedKeyUsage=clientAuth\nsubjectAltName=URI:spiffe://kubeneuron.io/installation/%s/agent\n' "$root_uid") \
	>/dev/null 2>&1
"$KUBECTL_BIN" -n "$NS" create secret tls kubeneuron-controller-tls \
	--cert="$pki/server.crt" --key="$pki/server.key" >/dev/null
"$KUBECTL_BIN" -n "$NS" create secret generic kubeneuron-controller-server-ca \
	--from-file=ca.crt="$pki/server-ca.crt" >/dev/null
"$KUBECTL_BIN" -n "$NS" create secret tls kubeneuron-agent-tls \
	--cert="$pki/client.crt" --key="$pki/client.key" >/dev/null
"$KUBECTL_BIN" -n "$NS" create secret generic kubeneuron-agent-client-ca \
	--from-file=ca.crt="$pki/client-ca.crt" >/dev/null

wait_ready() {
	local label=$1
	local deadline=$((SECONDS + TIMEOUT_SECONDS))
	while ((SECONDS < deadline)); do
		if "$KUBECTL_BIN" get kubeneuron "$ROOT_NAME" -o json 2>/dev/null | "$JQ_BIN" -e '
			any(.status.conditions[]?; .type == "Ready" and .status == "True")
		' >/dev/null; then
			note "$label: root object is Ready"
			return 0
		fi
		sleep 3
	done
	note "$label: final conditions:"
	"$KUBECTL_BIN" get kubeneuron "$ROOT_NAME" -o jsonpath='{.status.conditions}' 2>&1 || true
	printf '\n'
	die "$label: root object never became Ready"
}
wait_controller() {
	"$KUBECTL_BIN" -n "$NS" rollout status "deployment/${ROOT_NAME}-controller" \
		--timeout="${TIMEOUT_SECONDS}s" >/dev/null
}
wait_ready "baseline install"
wait_controller

start_port_forward() {
	if [[ -n $port_forward_pid ]]; then
		kill "$port_forward_pid" >/dev/null 2>&1 || true
		wait "$port_forward_pid" >/dev/null 2>&1 || true
		port_forward_pid=
	fi
	local log="$work_dir/port-forward.log"
	: >"$log"
	"$KUBECTL_BIN" -n "$NS" port-forward "service/${ROOT_NAME}-controller" :8080 >"$log" 2>&1 &
	port_forward_pid=$!
	local deadline=$((SECONDS + 30))
	public_port=
	while ((SECONDS < deadline)); do
		public_port=$(sed -n 's/^Forwarding from 127\.0\.0\.1:\([0-9][0-9]*\) -> 8080$/\1/p' "$log")
		[[ -n $public_port ]] && break
		kill -0 "$port_forward_pid" >/dev/null 2>&1 || die "port-forward exited early"
		sleep 1
	done
	[[ -n $public_port ]] || die "port-forward allocated no local port"
}
api_curl() {
	curl --silent --show-error --noproxy '*' --max-time 10 \
		-H "Authorization: Bearer $OPERATOR_TOKEN" "$@"
}

note "seeding durable workflow state through the $BASELINE operator API"
start_port_forward
trigger_code=$(api_curl -H 'Content-Type: application/json' \
	--data-binary '{"node":"upgrade-node","class":"upgrade-test","actor":"upgrade-harness"}' \
	-o /dev/null -w '%{http_code}' "http://127.0.0.1:${public_port}/api/v1/incidents")
[[ $trigger_code == 202 ]] || die "manual incident returned $trigger_code, want 202"
incident_id=
deadline=$((SECONDS + 60))
while ((SECONDS < deadline)); do
	incident_id=$(api_curl "http://127.0.0.1:${public_port}/api/v1/incidents?node=upgrade-node" |
		"$JQ_BIN" -r '[.[] | select(.class == "upgrade-test")][0].id // empty')
	[[ -n $incident_id ]] && break
	sleep 1
done
[[ -n $incident_id ]] || die "seed incident never appeared"
audit_before=$(api_curl "http://127.0.0.1:${public_port}/api/v1/incidents/${incident_id}" |
	"$JQ_BIN" '.audit | length')
((audit_before >= 1)) || die "seed incident has no audit trail"
note "seeded incident $incident_id with $audit_before audit entries"

note "building HEAD images"
for binary in kubeneuron-operator kubeneuron-controller kubeneuron-agent; do
	[[ -x $REPO_ROOT/bin/$binary ]] || die "missing static binary bin/$binary; run 'make build' first"
done
head_tag="upgrade-head-$$"
for target in operator controller agent; do
	"$DOCKER_BIN" build --target "$target" --tag "kubeneuron-$target:$head_tag" \
		--file "$REPO_ROOT/test/integration/Dockerfile" "$REPO_ROOT" >/dev/null
done
"$KIND_BIN" load docker-image --name "$CLUSTER_NAME" \
	"kubeneuron-operator:$head_tag" "kubeneuron-controller:$head_tag" "kubeneuron-agent:$head_tag"

note "upgrade step 1/2: HEAD CRDs, RBAC, and operator (docs/upgrade.md order)"
"$KUBECTL_BIN" apply -k "$REPO_ROOT/config/crd" >/dev/null
"$KUBECTL_BIN" apply -k "$REPO_ROOT/config/rbac" >/dev/null
operator_manifest="$work_dir/operator-head.yaml"
sed -E "s|image: ghcr\.io/kubeneuron/kube-neuron/operator:[A-Za-z0-9._-]+(@sha256:[a-f0-9]+)?|image: kubeneuron-operator:$head_tag|" \
	"$REPO_ROOT/config/default/operator_deployment.yaml" >"$operator_manifest"
grep -Fq "image: kubeneuron-operator:$head_tag" "$operator_manifest" || die "operator image substitution failed"
"$KUBECTL_BIN" apply -f "$operator_manifest" >/dev/null
"$KUBECTL_BIN" -n "$NS" patch deployment kubeneuron-operator --type=strategic \
	-p '{"spec":{"template":{"spec":{"containers":[{"name":"operator","imagePullPolicy":"Never"}]}}}}' >/dev/null
"$KUBECTL_BIN" -n "$NS" rollout status deployment/kubeneuron-operator \
	--timeout="${TIMEOUT_SECONDS}s" >/dev/null

note "upgrade step 2/2: controller/agent images on the root object"
"$KUBECTL_BIN" patch kubeneuron "$ROOT_NAME" --type=merge -p "{
  \"spec\": {
    \"controller\": {\"image\": \"kubeneuron-controller:$head_tag\"},
    \"agent\":      {\"image\": \"kubeneuron-agent:$head_tag\"}
  }
}" >/dev/null
wait_ready "post-upgrade"
wait_controller

note "asserting the upgraded CRD serves new-schema fields"
"$KUBECTL_BIN" get crd kubeneurons.kubeneuron.io -o json | "$JQ_BIN" -e '
	.spec.versions[0].schema.openAPIV3Schema.properties.spec.properties.agent.properties.hostTooling != null
' >/dev/null || die "upgraded CRD lacks spec.agent.hostTooling"

note "asserting the seeded incident survived the store migration"
start_port_forward
survived=$(api_curl "http://127.0.0.1:${public_port}/api/v1/incidents/${incident_id}" |
	"$JQ_BIN" --arg id "$incident_id" '(.incident.id == $id) and ((.audit | length) >= 1)')
[[ $survived == true ]] || die "seed incident/audit did not survive the upgrade"
audit_after=$(api_curl "http://127.0.0.1:${public_port}/api/v1/incidents/${incident_id}" |
	"$JQ_BIN" '.audit | length')
((audit_after >= audit_before)) || die "audit shrank across the upgrade: $audit_before -> $audit_after"

operator_logs=$("$KUBECTL_BIN" -n "$NS" logs deployment/kubeneuron-operator \
	--all-containers --tail=-1 2>&1)
if grep -Eiq 'forbidden|panic|fatal' <<<"$operator_logs"; then
	die "post-upgrade operator logs contain an unexpected RBAC/fatal error"
fi

note "PASS: $BASELINE -> HEAD upgrade converged in the documented order; the CRD gained new schema, and incident $incident_id with its audit trail survived the SQLite migration"
