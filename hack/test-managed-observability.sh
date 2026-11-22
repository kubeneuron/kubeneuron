#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Real-API smoke of the Managed metrics path the installer writes: the
# operator's Collector, store, vmalert and Grafana on a kind cluster, fed by
# hack/dcgmfixture (synthetic DCGM exposition, CPU-only) behind a Service
# named like the NVIDIA one. Uses an already provisioned kind cluster and
# operator; called by kind-integration.sh with local candidate images.
#
# Asserts:
#   * every Observability* condition and AlertDetectionConfigured is True;
#   * the store pod runs with fsGroup 65532 on its claim;
#   * every series the six dashboards query is in the store, except the
#     ones listed in expected_absent below with the reason they are absent;
#   * no stored DCGM series carries a workload label;
#   * the fixture's throttled GPU fires GpuThermalThrottle in vmalert, which
#     the controller accepts (firstAlertAcceptedAt in the inventory summary).
# Synthetic values, no GPU: wiring, names and delivery, not hardware.
set -euo pipefail
REPO_ROOT=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
: "${KUBECONFIG:?isolated test kubeconfig required}"
: "${CONTROLLER_IMAGE:?candidate controller image required}"
: "${AGENT_IMAGE:?candidate agent image required}"
: "${CLUSTER_NAME:?kind cluster name required}"
KUBECTL_BIN=${KUBECTL_BIN:-kubectl}
KIND_BIN=${KIND_BIN:-kind}
DOCKER_BIN=${DOCKER_BIN:-docker}
GO_BIN=${GO_BIN:-go}
JQ_BIN=${JQ_BIN:-jq}
name=${MANAGED_TEST_NAME:-integration-managed}
namespace=$name
fixture_namespace=$name-gpu
fixture_image=dcgm-fixture:$name-$$
# One throttled A10G per node (device 1): the input for GpuThermalThrottle.
fixture_spec='NVIDIA A10G:2:1'
work=$(mktemp -d)
chmod 700 "$work"
pf_pid=''
cleanup() {
	[[ -z $pf_pid ]] || { kill "$pf_pid" 2>/dev/null || true; wait "$pf_pid" 2>/dev/null || true; }
	"$KUBECTL_BIN" delete kubeneuron "$name" --ignore-not-found --timeout=120s >/dev/null 2>&1 || true
	"$KUBECTL_BIN" delete gpuplaybooks,gpuremediationpolicies -l "kubeneuron.io/installation=$name" --ignore-not-found >/dev/null 2>&1 || true
	"$KUBECTL_BIN" delete namespace "$namespace" "$fixture_namespace" --ignore-not-found --timeout=120s >/dev/null 2>&1 || true
	"$DOCKER_BIN" image rm -f "$fixture_image" >/dev/null 2>&1 || true
	rm -rf "$work"
}
trap cleanup EXIT
mkdir "$work/bin"
ln -s "$(command -v "$KUBECTL_BIN")" "$work/bin/kubectl"
export PATH="$work/bin:$PATH"
fail() { echo "managed observability FAIL: $*" >&2; exit 1; }
note() { echo "managed observability: $*"; }

# Dashboard series that the smoke cannot produce, each with its reason. A
# name listed here that no board queries any more is an error too.
declare -A expected_absent=(
	# Fixture omits these DCGM sources on purpose (hack/dcgmfixture).
	[kubeneuron_gpu_row_remap_failed]="fixture omits DCGM field 395"
	[kubeneuron_gpu_row_remap_pending]="fixture omits DCGM field 396"
	[kubeneuron_gpu_nvlink_errors_per_second]="fixture omits the NVLink fields"
)

# Fixture image: FROM scratch, statically linked on the host.
case $("$DOCKER_BIN" info --format '{{.Architecture}}') in
amd64 | x86_64) arch=amd64 ;;
arm64 | aarch64) arch=arm64 ;;
*) fail "unsupported container engine architecture" ;;
esac
mkdir "$work/image"
(cd "$REPO_ROOT" && CGO_ENABLED=0 GOOS=linux GOARCH=$arch "$GO_BIN" build -trimpath -ldflags='-s -w' -o "$work/image/dcgmfixture" ./hack/dcgmfixture)
(cd "$REPO_ROOT" && CGO_ENABLED=0 GOOS=linux GOARCH=$arch "$GO_BIN" build -trimpath -ldflags='-s -w' -o "$work/image/alertsink" ./hack/alertsink)
# The image runs as 65532 and the harness's umask may leave the binaries
# owner-only.
chmod 0755 "$work/image/dcgmfixture" "$work/image/alertsink"
cp "$REPO_ROOT/test/integration/gpu-observability/Dockerfile" "$work/image/"
"$DOCKER_BIN" build --quiet --pull=false -t "$fixture_image" "$work/image" >/dev/null
"$KIND_BIN" load docker-image --name "$CLUSTER_NAME" "$fixture_image" >/dev/null

# The exporter stand-in first, so its series and the throttle alert's hold
# run while the stack comes up.
"$KUBECTL_BIN" apply -f - >/dev/null <<YAML
apiVersion: v1
kind: Namespace
metadata:
  name: $fixture_namespace
---
apiVersion: apps/v1
kind: DaemonSet
metadata:
  name: nvidia-dcgm-exporter
  namespace: $fixture_namespace
spec:
  selector:
    matchLabels: {app: nvidia-dcgm-exporter}
  template:
    metadata:
      labels: {app: nvidia-dcgm-exporter}
    spec:
      automountServiceAccountToken: false
      securityContext:
        runAsNonRoot: true
        seccompProfile: {type: RuntimeDefault}
      containers:
        - name: exporter
          image: $fixture_image
          imagePullPolicy: Never
          args: ["-listen=:9400", "-spec=$fixture_spec"]
          env:
            - name: NODE_NAME
              valueFrom: {fieldRef: {fieldPath: spec.nodeName}}
          ports:
            - {name: metrics, containerPort: 9400}
          readinessProbe:
            httpGet: {path: /metrics, port: metrics}
          securityContext:
            allowPrivilegeEscalation: false
            readOnlyRootFilesystem: true
            capabilities: {drop: [ALL]}
---
apiVersion: v1
kind: Service
metadata:
  name: nvidia-dcgm-exporter
  namespace: $fixture_namespace
spec:
  selector: {app: nvidia-dcgm-exporter}
  ports:
    - {name: gpu-metrics, port: 9400, targetPort: metrics}
YAML
"$KUBECTL_BIN" -n "$fixture_namespace" rollout status daemonset/nvidia-dcgm-exporter --timeout=180s >/dev/null ||
	fail "fixture exporter did not become ready"

# CRDs/operator are installed by the parent harness; reapplying only its
# namespace keeps its candidate Deployment.
bash "$REPO_ROOT/deploy/install.sh" --name "$name" --namespace "$namespace" \
	--manifest "$REPO_ROOT/config/default/namespace.yaml" --admin '' \
	--controller-image "$CONTROLLER_IMAGE" --agent-image "$AGENT_IMAGE" >"$work/install.log" 2>&1 || {
	# The installer log holds generated credentials: do not print it.
	"$KUBECTL_BIN" get kubeneuron "$name" -o jsonpath='{.status.conditions}' >&2 || true
	fail "installation failed (credential-bearing log withheld)"
}
grep -Fq 'Installing third-party components, unmodified, pulled from their publishers:' "$work/install.log" ||
	fail "installer did not name the third-party components"

for condition in ObservabilityCollectorReady ObservabilityStoreReady ObservabilityRulesReady \
	ObservabilityGrafanaReady ObservabilityGPUMetricsReady AlertDetectionConfigured; do
	"$KUBECTL_BIN" wait --for="condition=$condition=True" "kubeneuron/$name" --timeout=300s >/dev/null || {
		"$KUBECTL_BIN" get kubeneuron "$name" -o jsonpath='{.status.conditions}' >&2 || true
		fail "$condition is not True"
	}
done

# The store writes to its claim as 65532: fsGroup makes a root-owned CSI
# volume writable. kind's local-path volume is world-writable and would hide
# a missing fsGroup, so the pod spec is checked too.
store_pod=$("$KUBECTL_BIN" -n "$namespace" get pod -l "app.kubernetes.io/instance=$name,app.kubernetes.io/component=metrics-store" -o jsonpath='{.items[0].metadata.name}')
[[ -n $store_pod ]] || fail "no metrics store pod"
[[ $("$KUBECTL_BIN" -n "$namespace" get pod "$store_pod" -o jsonpath='{.spec.securityContext.fsGroup}/{.spec.securityContext.runAsUser}/{.spec.volumes[?(@.name=="storage")].persistentVolumeClaim.claimName}') == "65532/65532/$name-metrics-store" ]] ||
	fail "metrics store pod does not run as 65532 with fsGroup 65532 on its claim"

"$KUBECTL_BIN" -n "$namespace" port-forward "pod/$store_pod" :8428 >"$work/forward.log" 2>&1 &
pf_pid=$!
port=''
deadline=$((SECONDS + 60))
while ((SECONDS < deadline)); do
	port=$(sed -n 's/.*127.0.0.1:\([0-9]*\) -> 8428.*/\1/p' "$work/forward.log" | head -1)
	[[ -n $port ]] && break
	sleep 1
done
[[ -n $port ]] || fail "port-forward to the store did not start"
# count of series with that name, 0 when there is none
series_count() {
	curl --silent --show-error --fail --noproxy '*' --connect-timeout 5 --max-time 30 \
		--get --data-urlencode "query=count({__name__=\"$1\"}) or on() vector(0)" \
		"http://127.0.0.1:$port/api/v1/query" | "$JQ_BIN" -r '.data.result[0].value[1] // "0"'
}

mapfile -t dashboard_series < <(python3 -I "$REPO_ROOT/hack/dashboard-series.py")
((${#dashboard_series[@]} > 0)) || fail "no dashboard series"
for series in "${!expected_absent[@]}"; do
	printf '%s\n' "${dashboard_series[@]}" | grep -qxF "$series" ||
		fail "expected_absent lists $series, which no dashboard queries"
done
# Recording rules run every 30s and the agents push every 30s; give the
# path five minutes to fill, then name what is still missing.
missing=()
deadline=$((SECONDS + 300))
while :; do
	missing=()
	for series in "${dashboard_series[@]}"; do
		[[ -n ${expected_absent[$series]:-} ]] && continue
		[[ $(series_count "$series") != 0 ]] || missing+=("$series")
	done
	((${#missing[@]} == 0 || SECONDS >= deadline)) && break
	sleep 15
done
# A series still missing must not be one the components already export:
# that would be the path losing it. The rest are vectors that appear on the
# first event of their kind (an incident, a step, an approval), which this
# run does not cause; internal/metrics/dashboard_test.go checks their names.
exported="$work/exported"
: >"$exported"
mapfile -t sources < <("$KUBECTL_BIN" -n "$namespace" get pod -l "app.kubernetes.io/instance=$name" \
	-o jsonpath='{range .items[*]}{.metadata.name}{" "}{.metadata.labels.app\.kubernetes\.io/component}{"\n"}{end}' |
	awk '$2 == "controller" {print $1 " 8080"} $2 == "agent" {print $1 " 9402"}')
((${#sources[@]} > 1)) || fail "no controller and agent pods to read /metrics from"
for source in "${sources[@]}"; do
	read -r pod remote <<<"$source"
	"$KUBECTL_BIN" -n "$namespace" port-forward "pod/$pod" ":$remote" >"$work/forward-$pod.log" 2>&1 &
	source_pid=$!
	local_port=''
	deadline=$((SECONDS + 30))
	while ((SECONDS < deadline)); do
		local_port=$(sed -n "s/.*127.0.0.1:\([0-9]*\) -> $remote.*/\1/p" "$work/forward-$pod.log" | head -1)
		[[ -n $local_port ]] && break
		sleep 1
	done
	[[ -n $local_port ]] || fail "port-forward to $pod did not start"
	curl --silent --show-error --fail --noproxy '*' --max-time 30 "http://127.0.0.1:$local_port/metrics" |
		awk '!/^#/ && NF {sub(/[{ ].*/, ""); print}' >>"$exported" || fail "could not read /metrics of $pod"
	kill "$source_pid" 2>/dev/null || true
	wait "$source_pid" 2>/dev/null || true
done
grep -q '^kubeneuron_' "$exported" || fail "the components export no kubeneuron_* series on /metrics"
lost=()
not_emitted=()
for series in "${missing[@]}"; do
	if grep -qxF "$series" "$exported"; then lost+=("$series"); else not_emitted+=("$series"); fi
done
((${#lost[@]} == 0)) || fail "exported on /metrics but missing from the Managed store: ${lost[*]}"
((${#not_emitted[@]} == 0)) || note "not emitted in this run (no event of their kind): ${not_emitted[*]}"
present=()
for series in "${!expected_absent[@]}"; do
	[[ $(series_count "$series") == 0 ]] || present+=("$series")
done
((${#present[@]} == 0)) || fail "in the store, so drop them from expected_absent: ${present[*]}"
note "$((${#dashboard_series[@]} - ${#expected_absent[@]} - ${#not_emitted[@]})) of ${#dashboard_series[@]} dashboard series in the store; ${#expected_absent[@]} the fixture cannot produce, ${#not_emitted[@]} not emitted yet"

# The fixture serves pod/namespace/container for busy devices; the Managed
# Collector drops them before the store.
leaked=$(curl --silent --show-error --fail --noproxy '*' --max-time 30 --get \
	--data-urlencode 'query=count({__name__=~"DCGM_FI_.*", pod!=""} or {__name__=~"DCGM_FI_.*", namespace!=""} or {__name__=~"DCGM_FI_.*", container!=""} or {__name__=~"DCGM_FI_.*", pod_uid!=""}) or on() vector(0)' \
	"http://127.0.0.1:$port/api/v1/query" | "$JQ_BIN" -r '.data.result[0].value[1] // "0"')
[[ $leaked == 0 ]] || fail "$leaked stored DCGM series carry a workload label"

# GpuThermalThrottle holds for 10m from the fixture's start; the controller
# notes the first alert it accepts.
deadline=$((SECONDS + 900))
accepted=''
while ((SECONDS < deadline)); do
	accepted=$("$KUBECTL_BIN" -n "$namespace" get configmap "$name-inventory-summary" -o jsonpath='{.data.firstAlertAcceptedAt}' 2>/dev/null || true)
	[[ -n $accepted ]] && break
	sleep 15
done
[[ -n $accepted ]] || fail "the controller accepted no metric alert from the managed vmalert"
# Which alerts: vmalert's own view. The throttled GPUs fire, one per node;
# the liveness alerts do not, since every node and the controller report.
rules_pod=$("$KUBECTL_BIN" -n "$namespace" get pod -l "app.kubernetes.io/instance=$name,app.kubernetes.io/component=metrics-rules" -o jsonpath='{.items[0].metadata.name}')
[[ -n $rules_pod ]] || fail "no vmalert pod"
"$KUBECTL_BIN" -n "$namespace" port-forward "pod/$rules_pod" :8880 >"$work/forward-rules.log" 2>&1 &
rules_pid=$!
rules_port=''
deadline=$((SECONDS + 30))
while ((SECONDS < deadline)); do
	rules_port=$(sed -n 's/.*127.0.0.1:\([0-9]*\) -> 8880.*/\1/p' "$work/forward-rules.log" | head -1)
	[[ -n $rules_port ]] && break
	sleep 1
done
[[ -n $rules_port ]] || fail "port-forward to vmalert did not start"
# The two hold from about the same start; give the second its evaluation.
throttled=0
deadline=$((SECONDS + 180))
while :; do
	curl --silent --show-error --fail --noproxy '*' --max-time 30 "http://127.0.0.1:$rules_port/api/v1/alerts" >"$work/alerts.json" ||
		fail "could not read vmalert's alerts"
	throttled=$("$JQ_BIN" '[.data.alerts[] | select(.name == "GpuThermalThrottle" and .state == "firing")] | length' "$work/alerts.json")
	((throttled >= 2 || SECONDS >= deadline)) && break
	sleep 15
done
kill "$rules_pid" 2>/dev/null || true
wait "$rules_pid" 2>/dev/null || true
[[ $throttled == 2 ]] || fail "GpuThermalThrottle firing for $throttled GPUs, want the 2 throttled ones: $("$JQ_BIN" -c '[.data.alerts[] | {name, state, node: .labels.node, gpu: .labels.gpu}]' "$work/alerts.json")"
liveness=$("$JQ_BIN" -r '[.data.alerts[] | select(.name == "GpuExporterDown" or .name == "KubeNeuronAgentDown") | .name] | join(" ")' "$work/alerts.json")
[[ -z $liveness ]] || fail "liveness alerts fire with every node reporting: $liveness"
note "controller accepted its first metric alert at $accepted; vmalert fires GpuThermalThrottle for both throttled GPUs and no liveness alert"
echo 'managed observability PASS: conditions, store fsGroup, dashboard series, workload labels dropped, alert delivered to the controller'
