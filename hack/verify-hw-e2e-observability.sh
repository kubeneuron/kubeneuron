#!/usr/bin/env bash
# verify-hw-e2e-observability.sh — prove the hardware harness's v0.7 GPU
# operator profile wiring and test-observability phase without AWS.
#
# No cluster, no Docker, a second. It runs the REAL hack/hw-e2e.sh up-finish
# with scripted aws/eksctl/kubectl/helm first on PATH to prove the pre-Helm
# gate fails closed (wrong GPU_OPERATOR_VERSION, missing profile) — reaching
# the scripted helm is the failure. Then it reads the script for the wiring a
# live run could only disprove at the cost of a GPU cluster: the pinned
# default, the --values argument on the Helm install, the EKS-only overrides,
# the command dispatch, the target guard before any kubectl call, bounded
# curl, the two witness fields, the label-contract and node_pool assertions,
# the Tesla T4 absence checks, and the VMSingle port read from the Service.
# The port helper itself is executed against a scripted kubectl: named http,
# the single-port fallback, and an ambiguous Service it must refuse.
#
# Then the half that makes the phase reachable at all: deploy-observability.
# Statically, the workflow must dispatch both phases after deploy and before
# test-destructive with teardown still last and always; the operator pin the
# script repeats must equal versions.lock.yaml (version, URL, SHA-256) and
# nothing may apply a remote URL; the phase must install in the README's
# order (operator, CRDs, namespace, VMSingle, token copy, vmagent,
# Alertmanager, vmalert, rules, pipeline live, root repointed) with every
# wait bounded and no swallowed failure; the StatefulSet wait must not go
# through kubectl rollout status (the operator's VMAlertmanager StatefulSet is
# OnDelete, which kubectl refuses: the 2026-10-18 G3 attempt died there) but
# through the status fields, with the strategy's own meaning of rolled out
# (RollingUpdate: every pod on the update revision; OnDelete: the controller
# never rolls a pod, so Ready at the observed generation is all it promises;
# anything else: never ready); the VMSingle claim's EBS volume must be tied
# to this run (PVC -> PV -> volume, tagged with the run identity) as soon as
# the claim is Bound, and the sweep must never scope volumes by namespace
# (hack/verify-hw-e2e-sweep.sh proves the sweep side). The StatefulSet wait
# is also driven against fixtures: a ready
# OnDelete object with old-revision pods, a ready RollingUpdate object and a
# ready object with the API's defaulted strategy pass; zero desired replicas,
# a stale generation, an unready pod, a RollingUpdate or defaulted object with
# an old-revision pod, an unsupported strategy, an unwritten status and an
# absent object fail, naming the status. Dynamically, the REAL
# deploy-observability runs against
# scripted aws/kubectl/curl: a lock that disagrees with the script is refused
# before any download, a download that does not match the lock is refused
# before any apply (real sha256sum), a cluster without KubeNeuron is refused
# before anything is installed, a VMSingle Service on the wrong port stops
# the phase before Alertmanager and before the root is repointed, and the
# green path applies everything in order, copies the token byte-for-byte and
# patches both endpoints. The last two use a scripted sha256sum, because no
# offline fixture can match the real artifact's hash; the checksum refusal
# is proven by the scenario that uses the real one.
#
# Last, the workflow's own kubectl: its pinned minor must equal the EKS
# control-plane minor the script creates (K8S_VERSION), its version and
# SHA-256 must be EXACTLY the reviewed pair recorded below (a well-formed
# but different digest fails, and the comparison is self-tested against
# one), the install step must verify the download before it is made
# executable or put on PATH, and the step itself is run with a scripted
# curl: a download that does not match is refused (real sha256sum) with
# nothing exported, and a verified one reaches PATH.
#
# Usage: hack/verify-hw-e2e-observability.sh
# shellcheck disable=SC2016 # the \$ in single-quoted patterns and fake-tool bodies are literal, by design.
set -euo pipefail

repo_root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd -P)
cd "$repo_root"

script=hack/hw-e2e.sh
profile=deploy/kubernetes/dependencies/gpu-operator/values.yaml
pin=v26.3.3

fail=0
note() { printf '\n=== %s\n' "$*"; }
bad() {
	echo "OBSERVABILITY CHECK FAILED: $*" >&2
	fail=1
}
# has <description> <grep -E pattern> — the pattern must match the script.
has() {
	grep -Eq -- "$2" "$script" || bad "$1 (pattern: $2)"
}
# fn_body <function> — print the function body from its definition line to
# the next top-level '}'.
fn_body() {
	awk -v fn="$1" '$0 ~ "^"fn"\\(\\) \\{" {p=1} p {print} p && /^\}/ {exit}' "$script"
}
# in_fn <function> <description> <grep -E pattern> — pattern must occur inside
# the named function body.
in_fn() {
	# grep without -q: an early exit would SIGPIPE awk and fail the pipeline.
	fn_body "$1" | grep -E -- "$3" >/dev/null || bad "$1: $2 (pattern: $3)"
}

for tool in jq bash; do
	command -v "$tool" >/dev/null 2>&1 || {
		echo "verify-hw-e2e-observability: $tool is required" >&2
		exit 1
	}
done

work=$(mktemp -d)
trap '[ -n "${KEEP_WORK:-}" ] || rm -rf "$work"' EXIT

note "static: pinned profile and chart version"
[ -f "$profile" ] || bad "profile missing: $profile"
grep -Eq "^[[:space:]]*version:[[:space:]]*${pin}[[:space:]]*$" "$profile" ||
	bad "$profile does not pin $pin"
grep -Eq "version: ${pin}$" deploy/kubernetes/dependencies/versions.lock.yaml ||
	bad "versions.lock.yaml does not pin $pin"
has "profile path constant" "^readonly GPU_OPERATOR_PROFILE_REL=\"${profile}\"$"
has "pinned version constant" "^readonly GPU_OPERATOR_PINNED_VERSION=\"${pin}\"$"
has "default GPU_OPERATOR_VERSION is the pin" '^GPU_OPERATOR_VERSION="\$\{GPU_OPERATOR_VERSION:-\$GPU_OPERATOR_PINNED_VERSION\}"$'
! grep -Eq 'GPU_OPERATOR_VERSION:-v25\.3\.4|GPU_OPERATOR_VERSION:-v24\.' "$script" ||
	bad "old GPU operator default still present"
for field in DCGM_FI_DEV_POWER_USAGE DCGM_FI_DEV_ECC_SBE_VOL_TOTAL DCGM_FI_DEV_GPU_TEMP DCGM_FI_DEV_MEMORY_UNREPAIRABLE_FLAG; do
	grep -Eq "^[[:space:]]*${field}," "$profile" || bad "$profile does not list $field"
done
# Field 1507 is DCGM_FI_DEV_MEMORY_UNREPAIRABLE in dcgm_fields.h, but the
# go-dcgm the pinned exporter is built against only knows the id as
# DCGM_FI_DEV_MEMORY_UNREPAIRABLE_FLAG, and one unknown CSV name terminates
# the whole exporter (the 2026-10-18 G4dn run: Service present, no endpoints,
# pipeline wait timed out 300s later). The profile and both rule files must
# spell the series the exporter's way; the catalog test pins the catalog.
for f in "$profile" configs/vmalert/gpu-recording-rules.yaml deploy/kubernetes/dependencies/observability/rules.yaml; do
	! grep -vE '^[[:space:]]*#' "$f" | grep -E 'DCGM_FI_DEV_MEMORY_UNREPAIRABLE([^_]|$)' >/dev/null ||
		bad "$f spells field 1507 DCGM_FI_DEV_MEMORY_UNREPAIRABLE; the pinned exporter only accepts DCGM_FI_DEV_MEMORY_UNREPAIRABLE_FLAG and exits on an unknown name"
done
grep -Eq '^[[:space:]]*expr: DCGM_FI_DEV_MEMORY_UNREPAIRABLE_FLAG[[:space:]]*$' configs/vmalert/gpu-recording-rules.yaml ||
	bad "gpu-recording-rules.yaml does not record kubeneuron_gpu_memory_unrepairable from DCGM_FI_DEV_MEMORY_UNREPAIRABLE_FLAG"
grep -Eq '^[[:space:]]*expr: DCGM_FI_DEV_MEMORY_UNREPAIRABLE_FLAG[[:space:]]*$' deploy/kubernetes/dependencies/observability/rules.yaml ||
	bad "the deployed rules.yaml does not record kubeneuron_gpu_memory_unrepairable from DCGM_FI_DEV_MEMORY_UNREPAIRABLE_FLAG"

note "static: up-finish installs the profile"
in_fn cmd_up_finish "gate runs before helm" '^[[:space:]]*assert_gpu_operator_profile$'
in_fn cmd_up_finish "--values on the Helm install" '--values "\$GPU_OPERATOR_PROFILE"'
in_fn cmd_up_finish "EKS driver override" '--set driver\.enabled=false'
in_fn cmd_up_finish "EKS dcgm override" '--set dcgm\.enabled=true'
# The gate must precede the helm upgrade line within the function.
gate_line=$(fn_body cmd_up_finish | grep -nE 'assert_gpu_operator_profile$' | head -n1 | cut -d: -f1)
helm_line=$(fn_body cmd_up_finish | grep -nE 'helm upgrade --install gpu-operator' | head -n1 | cut -d: -f1)
if [ -z "$gate_line" ] || [ -z "$helm_line" ] || [ "$gate_line" -ge "$helm_line" ]; then
	bad "assert_gpu_operator_profile (line ${gate_line:-none}) must precede the helm install (line ${helm_line:-none})"
fi

note "static: test-observability wiring"
has "usage lists test-observability" '^  test-observability '
has "dispatch routes test-observability" '^[[:space:]]*test-observability\) cmd_test_observability "\$@" ;;$'
in_fn cmd_test_observability "target guard before any kubectl call" '^[[:space:]]*assert_kube_target$'
in_fn cmd_test_observability "requires curl" '^[[:space:]]*require_cmd curl$'
in_fn cmd_test_observability "requires jq" '^[[:space:]]*require_cmd jq$'
in_fn cmd_test_observability "reads the profile ConfigMap" 'get configmap "\$DCGM_METRICS_CONFIGMAP"'
has "T0 witness field" '^readonly OBS_T0_FIELD="DCGM_FI_DEV_POWER_USAGE"$'
has "T1 witness field" '^readonly OBS_T1_FIELD="DCGM_FI_DEV_ECC_SBE_VOL_TOTAL"$'
in_fn cmd_test_observability "both witness fields checked in the ConfigMap" 'for field in "\$OBS_T0_FIELD" "\$OBS_T1_FIELD"'
in_fn cmd_test_observability "exporter rollout check" 'rollout status daemonset/nvidia-dcgm-exporter --timeout='
in_fn cmd_test_observability "exporter via bounded port-forward" 'obs_portforward "\$GPU_OPERATOR_NAMESPACE" service/nvidia-dcgm-exporter 9400'
in_fn cmd_test_observability "UUID label asserted" 'UUID="GPU-'
in_fn cmd_test_observability "modelName label asserted" 'modelName='
in_fn cmd_test_observability "stack prerequisite stated" 'PREREQUISITE: VMSingle'
# The VMSingle port comes from the Service, never from a constant.
in_fn cmd_test_observability "VMSingle port resolved from the Service" 'vm_port=\$\(obs_service_http_port "\$OBSERVABILITY_NAMESPACE" vmsingle-kubeneuron\)'
in_fn cmd_test_observability "VMSingle via bounded port-forward on the resolved port" 'obs_portforward "\$OBSERVABILITY_NAMESPACE" service/vmsingle-kubeneuron "\$vm_port"'
! fn_body cmd_test_observability | grep -E 'vmsingle-kubeneuron 8429' >/dev/null ||
	bad "VMSingle port is hard-coded to 8429 instead of read from the Service"
in_fn obs_service_http_port "prefers the port named http" 'select\(\.name == "http"\)'
in_fn obs_service_http_port "single-port fallback" 'if length == 1 then \.\[0\]\.port'
in_fn obs_service_http_port "ambiguous service is refused" 'refusing to guess'
for series in kubeneuron_gpu_temperature_celsius kubeneuron_gpu_power_watts kubeneuron_gpu_memory_used_bytes; do
	in_fn cmd_test_observability "canonical series $series" "$series"
done
in_fn cmd_test_observability "MiB->bytes factor" '\* 1048576'
# Label contract on the selected canonical samples (v0.7 M2).
in_fn cmd_test_observability "node/gpu_uuid/gpu_model/node_pool asserted non-empty" '^[[:space:]]*for lbl in node gpu_uuid gpu_model node_pool; do$'
in_fn cmd_test_observability "gpu_uuid equals the exporter UUID" '\.\[0\]\.metric\.gpu_uuid == \$v'
in_fn cmd_test_observability "gpu_model equals the exporter modelName" '\.\[0\]\.metric\.gpu_model == \$v'
in_fn cmd_test_observability "modelName captured from the exporter" 'model=\$\(printf .*modelName='
# node_pool: read the EKS nodegroup label from the node, fail with an EKS error
# when missing, and compare the canonical sample against it.
in_fn cmd_test_observability "EKS nodegroup label read from the GPU node" "jsonpath='\{\.metadata\.labels\.eks\\\\\.amazonaws\\\\\.com/nodegroup\}'"
in_fn cmd_test_observability "missing nodegroup label is an EKS-label error" 'die "EKS-label: '
in_fn cmd_test_observability "canonical node_pool equals the nodegroup" '\.\[0\]\.metric\.node_pool == \$v'
! grep -En 'node_pool' deploy/kubernetes/dependencies/observability/vmagent.yaml | grep -Ei 'required|must' >/dev/null ||
	bad "node_pool must stay optional in the scrape contract; only the EKS phase may expect it"
# Tesla T4 absence checks: zero results, never a value compared to zero.
in_fn cmd_test_observability "absence checks gated on g4dn" '^[[:space:]]*g4dn\.\*\)$'
in_fn cmd_test_observability "absence checks gated on Tesla T4" '\*"Tesla T4"\*\) ;;'
for absent in kubeneuron_gpu_remapped_rows_total kubeneuron_gpu_row_remap_failed kubeneuron_gpu_row_remap_pending \
	kubeneuron_gpu_nvlink_errors_total kubeneuron_gpu_nvlink_errors_per_second kubeneuron_gpu_fail_slow_score_ratio \
	'mig_profile!=' 'GPU_I_PROFILE!='; do
	in_fn cmd_test_observability "absence query for $absent" "$absent"
done
in_fn cmd_test_observability "absence means an empty result set" '-eq 0 \]'
in_fn cmd_test_observability "unexpected samples are reported" 'expected NO samples for'
! fn_body cmd_test_observability | grep -vE '^[[:space:]]*#' | grep -E '== *0\b|value\[1\][^|]*== *"?0' >/dev/null ||
	bad "test-observability compares a sample value to zero; absence must be an empty result, not a zero sample"
# Read-only: no injection, no mutating kubectl verb, no wrapper bypass.
! fn_body cmd_test_observability | grep -E 'inject_xid|kubectl (apply|patch|delete|cordon|drain|scale)' >/dev/null ||
	bad "test-observability must be read-only"
! fn_body cmd_test_observability | grep 'command kubectl' >/dev/null ||
	bad "test-observability bypasses the kubectl wrapper (E2E_KUBECONFIG endpoint check)"
# The target guard must precede the first kubectl call in the function.
guard_line=$(fn_body cmd_test_observability | grep -nE '^[[:space:]]*assert_kube_target$' | head -n1 | cut -d: -f1)
first_kubectl=$(fn_body cmd_test_observability | grep -nE '(^|[^_a-z])kubectl ' | head -n1 | cut -d: -f1)
if [ -z "$guard_line" ] || [ -z "$first_kubectl" ] || [ "$guard_line" -ge "$first_kubectl" ]; then
	bad "assert_kube_target (line ${guard_line:-none}) must precede the first kubectl call (line ${first_kubectl:-none}) in cmd_test_observability"
fi
# Every curl in the observability block is bounded.
! awk '/^obs_portforward\(\) \{/,/^cmd_teardown\(\) \{/' "$script" | grep -Ew curl |
	grep -vE '^[[:space:]]*#|require_cmd' | grep -v -- '--max-time' | grep . >/dev/null ||
	bad "unbounded curl in the observability block"
in_fn cleanup "EXIT cleanup reaps the observability port-forward through the close helper" '^[[:space:]]*obs_portforward_close$'
! fn_body cleanup | grep -E 'kill "\$_OBS_PF_PID"' >/dev/null ||
	bad "cleanup kills _OBS_PF_PID directly instead of reaping it through obs_portforward_close"
in_fn obs_portforward_close "close helper waits on the child" '^[[:space:]]*wait "\$_OBS_PF_PID"'
in_fn obs_portforward "port-forward pid recorded for cleanup" '^[[:space:]]*_OBS_PF_PID=\$!$'
# $! must be the kubectl process itself: exec'd in the background subshell
# after the target guard, not the wrapper function run in the background
# (whose kill leaves the real forward orphaned on the port).
in_fn obs_portforward "target guard runs inside the background subshell" '^[[:space:]]*assert_kube_target$'
in_fn obs_portforward "the forward is exec'd so \$! is kubectl" '^[[:space:]]*exec kubectl --kubeconfig "\$KUBECONFIG" -n "\$ns" port-forward "\$target" "\$\{local_port\}:\$\{remote\}"$'
! fn_body obs_portforward | grep -E '^[[:space:]]*kubectl -n "\$ns" port-forward' >/dev/null ||
	bad "obs_portforward backgrounds the wrapper function; killing \$! would orphan the real port-forward"
in_fn obs_portforward "port-forward readiness is bounded" 'wait_for 60 '
has "OBS_MAX_TIME bounds the phase's curls" '^OBS_MAX_TIME="\$\{OBS_MAX_TIME:-[0-9]+\}"$'

# --- dynamic: the pre-Helm gate fails closed ---------------------------------
# Scripted tools: aws answers the endpoint check, eksctl writes a kubeconfig
# the fake kubectl agrees with, helm records every call. Reaching helm is the
# failure these two scenarios exist to catch.
fakebin="$work/bin"
mkdir -p "$fakebin"
# The volume answers are for deploy-observability's tagging of the VMSingle
# claim: the by-id read (--output json) returns the CSI driver's created-for
# tags naming the fake PV and claim; the read-back after create-tags
# (--output text) returns the id. The resolver's own refusals are proven in
# hack/verify-hw-e2e-sweep.sh; here it only has to be reached, in order, with
# the right arguments, which FAKE_AWS_LOG records.
cat >"$fakebin/aws" <<'EOF'
#!/usr/bin/env bash
[ -z "${FAKE_AWS_LOG:-}" ] || printf 'aws %s\n' "$*" >>"$FAKE_AWS_LOG"
case "$*" in
*sts\ get-caller-identity*) echo "arn:aws:iam::000000000000:user/fake" ;;
*describe-cluster*oidc.issuer*) echo "https://oidc.fake.local/id/FAKE" ;;
*describe-cluster*) echo "https://fake.eks.local" ;;
*iam\ list-role-tags*) echo "$CLUSTER_NAME" ;;
*iam\ get-role*) echo "arn:aws:iam::000000000000:role/fake" ;;
*ec2\ describe-volumes*--volume-ids\ vol-0b5e0000000000001*--output\ json*)
	echo '{"Volumes":[{"VolumeId":"vol-0b5e0000000000001","State":"in-use","Tags":[{"Key":"kubernetes.io/created-for/pv/name","Value":"pvc-vmsingle"},{"Key":"kubernetes.io/created-for/pvc/name","Value":"vmsingle-kubeneuron"},{"Key":"kubernetes.io/created-for/pvc/namespace","Value":"kubeneuron-monitoring"}]}]}' ;;
*ec2\ describe-volumes*--volume-ids\ vol-0b5e0000000000001*--output\ text*) echo vol-0b5e0000000000001 ;;
*ec2\ create-tags*) ;;
*) exit 0 ;;
esac
EOF
printf '%s\n' '#!/usr/bin/env bash' \
	'out=""' \
	'while [ $# -gt 0 ]; do [ "$1" = --kubeconfig ] && out="$2"; shift; done' \
	'printf "%s\n" "apiVersion: v1" "kind: Config" "current-context: fake" >"$out"' >"$fakebin/eksctl"
# The fake kubectl also satisfies up-finish's post-install waits (a schedulable
# GPU, a Running DCGM engine) so the success scenario terminates.
printf '%s\n' '#!/usr/bin/env bash' \
	'case "$*" in' \
	'*"config current-context"*) echo fake ;;' \
	'*"config view"*) printf "https://fake.eks.local" ;;' \
	'*allocatable*) echo 1 ;;' \
	'*status.phase*) echo Running ;;' \
	'*) exit 0 ;;' \
	'esac' >"$fakebin/kubectl"
printf '%s\n' '#!/usr/bin/env bash' \
	'printf "helm %s\n" "$*" >>"$FAKE_HELM_LOG"' \
	'exit 0' >"$fakebin/helm"
chmod +x "$fakebin"/*

# run_gate <label> <stderr-substring> <script-path> [VAR=value...]
run_gate() {
	local label="$1" needle="$2" target="$3"
	shift 3
	local log="$work/helm.$label.log" err="$work/err.$label"
	: >"$log"
	set +e
	env PATH="$fakebin:$PATH" FAKE_HELM_LOG="$log" \
		CLUSTER_NAME=kubeneuron-e2e-verify E2E_STATE_DIR="$work/state.$label" \
		RECYCLE_ROLE_ARN=arn:aws:iam::000000000000:role/fake "$@" \
		timeout 120 bash "$target" up-finish >/dev/null 2>"$err"
	local rc=$?
	set -e
	[ "$rc" -ne 0 ] || bad "$label: up-finish exited 0, expected refusal"
	grep -q -- "$needle" "$err" || bad "$label: stderr lacks '$needle': $(tr '\n' ' ' <"$err")"
	! grep -q 'helm upgrade' "$log" || bad "$label: helm upgrade was reached"
}

note "dynamic: wrong GPU_OPERATOR_VERSION is refused before helm"
run_gate version-mismatch "differs from the checked-in pin" "$script" GPU_OPERATOR_VERSION=v25.3.4

note "dynamic: missing profile is refused before helm"
mkdir -p "$work/tree/hack"
cp "$script" "$work/tree/hack/hw-e2e.sh"
run_gate missing-profile "GPU operator profile not found" "$work/tree/hack/hw-e2e.sh"

note "dynamic: the pinned version with the profile present reaches helm with --values"
log="$work/helm.ok.log"
err="$work/err.ok"
: >"$log"
set +e
env PATH="$fakebin:$PATH" FAKE_HELM_LOG="$log" \
	CLUSTER_NAME=kubeneuron-e2e-verify E2E_STATE_DIR="$work/state.ok" \
	RECYCLE_ROLE_ARN=arn:aws:iam::000000000000:role/fake \
	timeout 120 bash "$script" up-finish >/dev/null 2>"$err"
rc=$?
set -e
[ "$rc" -eq 0 ] || bad "pinned version: up-finish exited $rc against the fakes: $(tr '\n' ' ' <"$err")"
grep -Eq -- "helm upgrade --install gpu-operator nvidia/gpu-operator .*--version ${pin} .*--values ${repo_root}/${profile} .*--set driver.enabled=false --set dcgm.enabled=true" "$log" ||
	bad "helm install line lacks the expected --version/--values/--set arguments: $(grep 'helm upgrade' "$log" || echo '(helm upgrade never ran)')"

# --- dynamic: obs_service_http_port against a scripted kubectl ---------------
# The helper is extracted from the script and run in a subshell with a fake
# kubectl that serves one of three Services. The ambiguous case MUST fail:
# a helper that guesses would pass the live run right up to the port-forward
# that never answers.
note "dynamic: obs_service_http_port reads the port from the Service"
printf '%s\n' '#!/usr/bin/env bash' \
	'case "$FAKE_SVC" in' \
	'named) echo "{\"spec\":{\"ports\":[{\"name\":\"metrics\",\"port\":9999},{\"name\":\"http\",\"port\":8429}]}}" ;;' \
	'single) echo "{\"spec\":{\"ports\":[{\"port\":7777}]}}" ;;' \
	'ambiguous) echo "{\"spec\":{\"ports\":[{\"name\":\"a\",\"port\":1},{\"name\":\"b\",\"port\":2}]}}" ;;' \
	'*) exit 1 ;;' \
	'esac' >"$fakebin/kubectl-svc"
chmod +x "$fakebin/kubectl-svc"
port_helper="$work/port-helper.sh"
{
	echo 'set -euo pipefail'
	printf '%s\n' 'die() { printf "hw-e2e: %s\n" "$*" >&2; exit 1; }'
	echo 'kubectl() { kubectl-svc "$@"; }'
	fn_body obs_service_http_port
	echo 'obs_service_http_port ns svc'
} >"$port_helper"
# run_port <scenario> <expected-stdout|REFUSE>
run_port() {
	local scenario="$1" want="$2" got rc
	set +e
	got=$(env PATH="$fakebin:$PATH" FAKE_SVC="$scenario" timeout 30 bash "$port_helper" 2>"$work/port.$scenario.err")
	rc=$?
	set -e
	if [ "$want" = REFUSE ]; then
		[ "$rc" -ne 0 ] || bad "port helper: $scenario service accepted (printed '$got'), expected a refusal"
		grep -q 'refusing to guess' "$work/port.$scenario.err" ||
			bad "port helper: $scenario refusal lacks 'refusing to guess': $(tr '\n' ' ' <"$work/port.$scenario.err")"
	else
		[ "$rc" -eq 0 ] || bad "port helper: $scenario exited $rc: $(tr '\n' ' ' <"$work/port.$scenario.err")"
		[ "$got" = "$want" ] || bad "port helper: $scenario printed '$got', expected $want"
	fi
}
run_port named 8429
run_port single 7777
run_port ambiguous REFUSE

# =============================================================================
# deploy-observability: the stack test-observability reads
# =============================================================================
workflow=.github/workflows/hw-e2e.yaml
lock=deploy/kubernetes/dependencies/versions.lock.yaml
profile_dir=deploy/kubernetes/dependencies/observability

# lock_field <artifact> <key> — the same flat awk read the script uses, kept
# separate here so a change to the script's reader is checked, not trusted.
lock_field() {
	awk -v block="  $1:" -v key="$2" '
		$0 == block { p = 1; next }
		p && /^  [^ ]/ { p = 0 }
		p { sub(/^[ \t]+/, ""); if (index($0, key ":") == 1) { sub("^" key ":[ \t]*", ""); print; exit } }
	' "$lock"
}
# script_const <NAME> — the value of a `readonly NAME="..."` line.
script_const() {
	sed -nE "s/^readonly $1=\"([^\"]*)\"\$/\1/p" "$script" | head -n1
}
# line_of <text> — first line number of a fixed string in the function body on
# stdin; empty when absent.
line_of() {
	# `|| true`: under pipefail an absent label would otherwise exit the
	# verifier through set -e instead of being reported.
	grep -nF -- "$1" | head -n1 | cut -d: -f1 || true
}
# ordered <function> <label>... — each label must occur in the function body,
# and in the given order. Labels are fixed strings.
ordered() {
	local fn="$1" prev=0 prev_label="(start)" line label body
	shift
	body=$(fn_body "$fn")
	for label in "$@"; do
		line=$(printf '%s\n' "$body" | line_of "$label")
		if [ -z "$line" ]; then
			bad "$fn: missing '$label'"
			continue
		fi
		if [ "$line" -le "$prev" ]; then
			bad "$fn: '$label' (line $line) must come after '$prev_label' (line $prev)"
		fi
		prev="$line"
		prev_label="$label"
	done
}

note "static: the workflow dispatches deploy-observability and test-observability in order"
[ -f "$workflow" ] || bad "workflow missing: $workflow"
wf_line() { grep -nE "^[[:space:]]+run: hack/hw-e2e\.sh $1\$" "$workflow" | head -n1 | cut -d: -f1 || true; }
wf_deploy=$(wf_line deploy)
wf_obs=$(wf_line deploy-observability)
wf_test=$(wf_line test-observability)
wf_destructive=$(wf_line test-destructive)
wf_teardown=$(wf_line teardown)
for step in deploy deploy-observability test-observability test-destructive teardown; do
	[ -n "$(wf_line "$step")" ] || bad "workflow has no step running hack/hw-e2e.sh $step"
done
if [ -n "$wf_deploy" ] && [ -n "$wf_obs" ] && [ -n "$wf_test" ] && [ -n "$wf_destructive" ] && [ -n "$wf_teardown" ]; then
	[ "$wf_deploy" -lt "$wf_obs" ] || bad "deploy-observability (line $wf_obs) must follow deploy (line $wf_deploy): the token copy needs KubeNeuron"
	[ "$wf_obs" -lt "$wf_test" ] || bad "test-observability (line $wf_test) must follow deploy-observability (line $wf_obs)"
	[ "$wf_test" -lt "$wf_destructive" ] || bad "test-observability (line $wf_test) must precede test-destructive (line $wf_destructive): ReplaceNode terminates the only GPU node"
	[ "$wf_destructive" -lt "$wf_teardown" ] || bad "teardown must remain the last phase"
	# The step before teardown's run line must still be unconditional.
	sed -n "$((wf_teardown - 1))p" "$workflow" | grep -Eq '^[[:space:]]+if: always\(\)$' ||
		bad "teardown step is no longer 'if: always()'"
fi
# Exactly one step each: a duplicated phase would double the bounded waits.
for step in deploy-observability test-observability; do
	n=$(grep -cE "^[[:space:]]+run: hack/hw-e2e\.sh $step\$" "$workflow" || true)
	[ "$n" -eq 1 ] || bad "workflow runs $step $n times, expected once"
done
# The job ceiling must stay safely below the reaper's default lifetime: the
# reaper deletes the cluster at expires-at, and teardown needs 10-15 minutes
# after the ceiling fires.
wf_timeout=$(sed -nE 's/^[[:space:]]+timeout-minutes:[[:space:]]*([0-9]+)[[:space:]]*$/\1/p' "$workflow" | head -n1)
max_lifetime=$(sed -nE 's/^MAX_LIFETIME_MINUTES="\$\{MAX_LIFETIME_MINUTES:-([0-9]+)\}"$/\1/p' "$script" | head -n1)
[ -n "$wf_timeout" ] || bad "workflow has no timeout-minutes"
[ -n "$max_lifetime" ] || bad "script has no MAX_LIFETIME_MINUTES default"
if [ -n "$wf_timeout" ] && [ -n "$max_lifetime" ] && [ $((wf_timeout + 20)) -gt "$max_lifetime" ]; then
	bad "timeout-minutes $wf_timeout leaves less than 20 minutes before MAX_LIFETIME_MINUTES $max_lifetime; teardown would race the reaper"
fi

note "static: the VictoriaMetrics operator pin repeats versions.lock.yaml exactly"
vm_version=$(script_const VM_OPERATOR_VERSION)
vm_url=$(script_const VM_OPERATOR_MANIFEST_URL)
vm_sha=$(script_const VM_OPERATOR_MANIFEST_SHA256)
[ -n "$vm_version" ] || bad "script has no VM_OPERATOR_VERSION constant"
[ -n "$vm_url" ] || bad "script has no VM_OPERATOR_MANIFEST_URL constant"
printf '%s' "$vm_sha" | grep -Eq '^[0-9a-f]{64}$' || bad "VM_OPERATOR_MANIFEST_SHA256 is not a SHA-256 hex digest: '$vm_sha'"
[ "$(lock_field victoriaMetricsOperatorManifest version)" = "$vm_version" ] ||
	bad "lock version '$(lock_field victoriaMetricsOperatorManifest version)' != script VM_OPERATOR_VERSION '$vm_version'"
[ "$(lock_field victoriaMetricsOperatorManifest url)" = "$vm_url" ] ||
	bad "lock url '$(lock_field victoriaMetricsOperatorManifest url)' != script VM_OPERATOR_MANIFEST_URL '$vm_url'"
[ "$(lock_field victoriaMetricsOperatorManifest sha256)" = "$vm_sha" ] ||
	bad "lock sha256 '$(lock_field victoriaMetricsOperatorManifest sha256)' != script VM_OPERATOR_MANIFEST_SHA256 '$vm_sha'"
printf '%s' "$vm_url" | grep -Fq "/${vm_version}/" || bad "VM_OPERATOR_MANIFEST_URL does not carry the pinned version $vm_version"
grep -Fq "$vm_sha" deploy/kubernetes/dependencies/README.md || bad "README does not state the locked manifest SHA-256 $vm_sha"
has "the script constant is what the lock field reader is compared against" '^[[:space:]]*locked=\$\(lock_field victoriaMetricsOperatorManifest sha256\)$'
in_fn assert_observability_profile "lock version compared" 'lock_field victoriaMetricsOperatorManifest version'
in_fn assert_observability_profile "lock url compared" 'lock_field victoriaMetricsOperatorManifest url'
in_fn assert_observability_profile "lock sha256 compared" 'lock_field victoriaMetricsOperatorManifest sha256'
in_fn assert_observability_profile "every profile file required" 'for f in namespace vmsingle vmagent vmalertmanager vmalert rules; do'
in_fn assert_observability_profile "vmalert reads the VMSingle endpoint constant" 'VMSINGLE_ENDPOINT\}\[\[:space:\]\]\*\$" "\$OBSERVABILITY_PROFILE/vmalert\.yaml"'
in_fn assert_observability_profile "vmalert notifies the Alertmanager endpoint constant" 'VMALERTMANAGER_ENDPOINT\}\[\[:space:\]\]\*\$" "\$OBSERVABILITY_PROFILE/vmalert\.yaml"'
in_fn assert_observability_profile "Alertmanager mounts the token Secret constant" 'ALERTMANAGER_WEBHOOK_TOKEN_SECRET\}\[\[:space:\]\]\*\$" "\$OBSERVABILITY_PROFILE/vmalertmanager\.yaml"'
# The constants agree with the checked-in profile right now, not only by
# the script's own gate.
vmsingle_ep=$(script_const VMSINGLE_ENDPOINT)
am_ep=$(script_const VMALERTMANAGER_ENDPOINT)
token_secret=$(script_const ALERTMANAGER_WEBHOOK_TOKEN_SECRET)
grep -Eq "^[[:space:]]*url:[[:space:]]*${vmsingle_ep}[[:space:]]*$" "$profile_dir/vmalert.yaml" || bad "vmalert.yaml does not read $vmsingle_ep"
grep -Eq "^[[:space:]]*url:[[:space:]]*${am_ep}[[:space:]]*$" "$profile_dir/vmalert.yaml" || bad "vmalert.yaml does not notify $am_ep"
grep -Eq "^[[:space:]]*-[[:space:]]*${token_secret}[[:space:]]*$" "$profile_dir/vmalertmanager.yaml" || bad "vmalertmanager.yaml does not mount $token_secret"
grep -Eq "endpoint: ${vmsingle_ep}$" deploy/kubernetes/dependencies/README.md || bad "README's KubeNeuron example does not use $vmsingle_ep"
grep -Eq "endpoint: ${am_ep}$" deploy/kubernetes/dependencies/README.md || bad "README's KubeNeuron example does not use $am_ep"
# The port is the one the VictoriaMetrics operator actually gives the VMSingle
# Service (8429, observed on EKS 2026-10-18), not the standalone binary's
# 8428; the constant, the README table, the sample and vmagent's remote write
# all name it, so the live assertion below cannot be satisfied by a wrong
# constant that the profile merely agrees with.
[ "${vmsingle_ep##*:}" = 8429 ] || bad "VMSINGLE_ENDPOINT names port ${vmsingle_ep##*:}; the operator-managed VMSingle Service serves 8429"
grep -Eq "^\| VictoriaMetrics \| \`${vmsingle_ep}\` \|$" deploy/kubernetes/dependencies/README.md || bad "README's endpoint table does not list $vmsingle_ep"
grep -Eq "^[[:space:]]*endpoint: ${vmsingle_ep}$" config/samples/kubeneuron_v1alpha1_kubeneuron.yaml || bad "the repository sample does not use $vmsingle_ep"
grep -Eq "^[[:space:]]*- url: ${vmsingle_ep}/api/v1/write$" "$profile_dir/vmagent.yaml" || bad "vmagent.yaml does not remote-write to $vmsingle_ep"

note "static: nothing is applied from a mutable remote"
# Every https:// in the script is either the NGC Helm repository the GPU
# operator install already used or the locked manifest URL. http:// is
# in-cluster or loopback only.
while IFS= read -r url; do
	case "$url" in
	"https://helm.ngc.nvidia.com/nvidia") ;;
	"$vm_url") ;;
	*) bad "unexpected remote URL in the script: $url" ;;
	esac
done < <(grep -vE '^[[:space:]]*#' "$script" | grep -oE 'https://[^"'"'"' )}]+' | sort -u)
! grep -vE '^[[:space:]]*#' "$script" | grep -oE 'http://[^"'"'"' )}]+' | grep -vE '^http://(127\.0\.0\.1|\$\{ip|[a-z0-9-]+\.[a-z0-9-]+\.svc)(:|/|$)' | grep . ||
	bad "http:// URL in the script that is neither loopback, the pod ip, nor an in-cluster .svc name"
# No apply of a URL, no apply -k of the remote kustomization entry point.
! grep -vE '^[[:space:]]*#' "$script" | grep -E 'kubectl.*apply.*(-f|-k)[[:space:]]+"?https?://' | grep . ||
	bad "kubectl apply of a remote URL in the script"
! grep -vE '^[[:space:]]*#' "$script" | grep -E 'apply[[:space:]]+-k[[:space:]]+.*victoria-metrics-operator' | grep . ||
	bad "the remote victoria-metrics-operator kustomization is applied instead of the verified manifest"
in_fn fetch_vm_operator_manifest "manifest downloaded with a bounded, retried curl" 'curl -fsSL --max-time [0-9]+ --retry [0-9]+ -o "\$\{dest\}\.download" "\$VM_OPERATOR_MANIFEST_URL"'
in_fn fetch_vm_operator_manifest "unverified download is deleted" '^[[:space:]]*rm -f "\$\{dest\}\.download"$'
in_fn fetch_vm_operator_manifest "unverified download is refused" 'refusing to apply an unverified manifest'
in_fn vm_operator_manifest_verified "verification is sha256sum -c of the pinned digest" 'printf .*"\$VM_OPERATOR_MANIFEST_SHA256" "\$1" \| sha256sum -c --status -'
in_fn cmd_deploy_observability "sha256sum required" '^[[:space:]]*require_cmd sha256sum$'
in_fn cmd_deploy_observability "server-side apply of the verified file with the README's field manager" 'kubectl apply --server-side --field-manager="\$VM_OPERATOR_FIELD_MANAGER" -f "\$manifest"'
has "field manager is the README's" '^readonly VM_OPERATOR_FIELD_MANAGER="kubeneuron-dependencies"$'
grep -Fq -- '--field-manager=kubeneuron-dependencies' deploy/kubernetes/dependencies/README.md || bad "README no longer uses field manager kubeneuron-dependencies"

note "static: deploy-observability installs in the README's order with bounded gates"
has "dispatch routes deploy-observability" '^[[:space:]]*deploy-observability\) cmd_deploy_observability "\$@" ;;$'
has "usage lists deploy-observability" '^  deploy-observability$'
in_fn cmd_deploy_observability "target guard before any kubectl call" '^[[:space:]]*assert_kube_target$'
guard_line=$(fn_body cmd_deploy_observability | grep -nE '^[[:space:]]*assert_kube_target$' | head -n1 | cut -d: -f1)
first_kubectl=$(fn_body cmd_deploy_observability | grep -nE '(^|[^_a-z])kubectl ' | head -n1 | cut -d: -f1)
if [ -z "$guard_line" ] || [ -z "$first_kubectl" ] || [ "$guard_line" -ge "$first_kubectl" ]; then
	bad "assert_kube_target (line ${guard_line:-none}) must precede the first kubectl call (line ${first_kubectl:-none}) in cmd_deploy_observability"
fi
ordered cmd_deploy_observability \
	'assert_observability_profile' \
	'get kubeneuron "$ROOT_NAME" >/dev/null 2>&1 ||' \
	'get service nvidia-dcgm-exporter >/dev/null 2>&1 ||' \
	'obs_assert_exporter_live' \
	'manifest=$(fetch_vm_operator_manifest)' \
	'kubectl apply --server-side --field-manager="$VM_OPERATOR_FIELD_MANAGER" -f "$manifest"' \
	'kubectl wait --for=condition=Established --timeout=120s "crd/${crd}.operator.victoriametrics.com"' \
	'rollout status deployment/vm-operator --timeout=300s' \
	'kubectl apply -f "$OBSERVABILITY_PROFILE/namespace.yaml"' \
	'kubectl apply -f "$OBSERVABILITY_PROFILE/vmsingle.yaml"' \
	'get pvc vmsingle-kubeneuron -o jsonpath="{.status.phase}" | grep -qx Bound' \
	'obs_wait_rollout deployment/vmsingle-kubeneuron 300' \
	'obs_assert_service_port vmsingle-kubeneuron "$VMSINGLE_ENDPOINT"' \
	'copy_webhook_token' \
	'kubectl apply -f "$OBSERVABILITY_PROFILE/vmagent.yaml"' \
	'obs_wait_rollout deployment/vmagent-kubeneuron 300' \
	'kubectl apply -f "$OBSERVABILITY_PROFILE/vmalertmanager.yaml"' \
	'obs_wait_rollout statefulset/vmalertmanager-kubeneuron 300' \
	'obs_assert_service_port vmalertmanager-kubeneuron "$VMALERTMANAGER_ENDPOINT"' \
	'kubectl apply -f "$OBSERVABILITY_PROFILE/vmalert.yaml"' \
	'obs_wait_rollout deployment/vmalert-kubeneuron 300' \
	'kubectl apply -f "$OBSERVABILITY_PROFILE/rules.yaml"' \
	'for rule in kubeneuron-gpu kubeneuron-gpu-recording; do' \
	'vm_port=$(obs_service_http_port "$OBSERVABILITY_NAMESPACE" vmsingle-kubeneuron)' \
	'obs_portforward "$OBSERVABILITY_NAMESPACE" service/vmsingle-kubeneuron "$vm_port"' \
	'wait_for 300 '"'"'obs_vm_has_samples "$OBS_PIPELINE_FIELD"'"'" \
	'wait_for 300 '"'"'obs_vm_has_samples "kubeneuron_gpu_temperature_celsius"'"'" \
	'obs_portforward_close' \
	'patch kubeneuron "$ROOT_NAME" --type merge' \
	'wait_for_installation_ready' \
	'jsonpath='"'"'{.spec.observability.victoriaMetrics.endpoint}'"'" \
	'jsonpath='"'"'{.spec.observability.alertmanager.endpoint}'"'"
# The exporter gate (v0.7 G3, 2026-10-18): before the operator manifest is
# even fetched, the DaemonSet must be rolled out, the Service must have ready
# endpoints, and /metrics, read through the bounded port-forward on the port
# the Service declares, must carry the raw series the pipeline wait looks
# for. Each refusal prints the exporter diagnosis (pods, container state, the
# last log lines, the ConfigMap's field names), which is where an unknown CSV
# field name shows up.
has "pipeline field constant" '^readonly OBS_PIPELINE_FIELD="DCGM_FI_DEV_GPU_TEMP"$'
in_fn obs_assert_exporter_live "DaemonSet rollout is a bounded wait" '^[[:space:]]*kubectl -n "\$GPU_OPERATOR_NAMESPACE" rollout status daemonset/nvidia-dcgm-exporter --timeout=300s \|\| \{$'
in_fn obs_assert_exporter_live "ready endpoints are a bounded wait" '^[[:space:]]*\(wait_for 60 obs_exporter_has_endpoints\) \|\| \{$'
in_fn obs_exporter_has_endpoints "endpoint addresses are read from the Endpoints object" "get endpoints nvidia-dcgm-exporter --request-timeout=30s"
in_fn obs_assert_exporter_live "exporter port resolved from the Service" '^[[:space:]]*port=\$\(obs_service_http_port "\$GPU_OPERATOR_NAMESPACE" nvidia-dcgm-exporter\)$'
in_fn obs_assert_exporter_live "exporter via bounded port-forward on the resolved port" '^[[:space:]]*obs_portforward "\$GPU_OPERATOR_NAMESPACE" service/nvidia-dcgm-exporter "\$port" 19400 /metrics$'
! fn_body obs_assert_exporter_live | grep -E 'nvidia-dcgm-exporter 9400' >/dev/null ||
	bad "obs_assert_exporter_live hard-codes the exporter port instead of reading it from the Service"
in_fn obs_assert_exporter_live "/metrics read is bounded" '^[[:space:]]*metrics=\$\(curl -fsS --max-time "\$OBS_MAX_TIME" "http://127\.0\.0\.1:19400/metrics"\) \|\| \{$'
in_fn obs_assert_exporter_live "the pipeline field must be served as a sample" 'grep -Eq "\^\$\{OBS_PIPELINE_FIELD\}\\\{" \|\| \{$'
in_fn obs_assert_exporter_live "a missing sample names the series the exporter does serve" 'DCGM series served: \$\{served:-none\}'
# grep exits 1 on no match; under pipefail that would end the function
# before the diagnosis and the refusal when the body has no DCGM_ series.
in_fn obs_assert_exporter_live "the served-series extraction survives a body with no DCGM_ series" "^[[:space:]]*served=.*\\{ grep -oE '\\^DCGM_\\[A-Z0-9_\\]\\+' \\|\\| true; \\} \\| sort -u"
n_die=$(fn_body obs_assert_exporter_live | grep -c '^[[:space:]]*die "' || true)
n_diag=$(fn_body obs_assert_exporter_live | grep -c '^[[:space:]]*obs_exporter_diagnose$' || true)
if [ "$n_die" -ne 4 ] || [ "$n_diag" -ne 4 ]; then
	bad "obs_assert_exporter_live must print obs_exporter_diagnose before each of its four refusals (dies: $n_die, diagnoses: $n_diag)"
fi
for diag in 'get pods -l app=nvidia-dcgm-exporter' 'logs -l app=nvidia-dcgm-exporter --all-containers --prefix --previous --tail=40' 'logs -l app=nvidia-dcgm-exporter --all-containers --prefix --tail=40' 'get configmap "$DCGM_METRICS_CONFIGMAP"'; do
	in_fn obs_exporter_diagnose "diagnosis reads: $diag" "$(printf '%s' "$diag" | sed 's/[][\.*^$]/\\&/g')"
done
! fn_body obs_exporter_diagnose | grep -E '(^|[^_a-z])kubectl ' | grep -v -- '--request-timeout=' | grep . >/dev/null ||
	bad "obs_exporter_diagnose has a kubectl call without --request-timeout"
! fn_body obs_exporter_diagnose | grep -E 'get secret|\.data\.token' >/dev/null ||
	bad "obs_exporter_diagnose reads a Secret"
# Every CRD the profile's CRs need is waited on, by name.
in_fn cmd_deploy_observability "the five CRDs the profile needs" '^[[:space:]]*for crd in vmsingles vmagents vmalertmanagers vmalerts vmrules; do$'
in_fn obs_wait_rollout "object existence is a bounded wait" "^[[:space:]]*wait_for \"\\\$seconds\" 'kubectl -n '\"\\\$OBSERVABILITY_NAMESPACE\"' get '\"\\\$target\"' >/dev/null'$"
in_fn obs_wait_rollout "deployment rollout wait is bounded" 'rollout status "\$target" --timeout="\$\{seconds\}s"'
in_fn obs_wait_rollout "rollout failure is a failure" 'die "\$\{OBSERVABILITY_NAMESPACE\}/\$\{target\} did not roll out'
# The StatefulSet arm: no rollout status (kubectl refuses it for the OnDelete
# StatefulSet the operator generates), a status poll through the bounded
# helper, the status in the timeout message, and an unknown kind refused.
in_fn obs_wait_rollout "StatefulSets are polled on status through the bounded helper" "^[[:space:]]*\(wait_for \"\\\$seconds\" 'obs_statefulset_ready '\"\\\$target\"\) \|\|$"
in_fn obs_wait_rollout "StatefulSet timeout names the status" 'did not roll out within \$\{seconds\}s: \$\(obs_statefulset_status "\$target"\)"'
in_fn obs_wait_rollout "unknown kinds are refused, not rolled out" 'die "obs_wait_rollout: \$\{target\} is neither deployment/<name> nor statefulset/<name>"'
rs_count=$(fn_body obs_wait_rollout | grep -c 'rollout status' || true)
[ "$rs_count" -eq 1 ] || bad "obs_wait_rollout must call rollout status exactly once, in the deployment arm (found $rs_count)"
# The only waiting obs_wait_rollout does is the bounded wait_for and rollout
# status: no sleep, no fixed delay standing in for a readiness decision.
! fn_body obs_wait_rollout | grep -E '(^|[^a-zA-Z_])sleep([^a-zA-Z_]|$)' >/dev/null ||
	bad "obs_wait_rollout sleeps; a StatefulSet must be polled through wait_for on obs_statefulset_ready, never waited for by a fixed delay"
dep_arm=$(fn_body obs_wait_rollout | grep -nE '^[[:space:]]*deployment/\*\)$' | head -n1 | cut -d: -f1 || true)
sts_arm=$(fn_body obs_wait_rollout | grep -nE '^[[:space:]]*statefulset/\*\)$' | head -n1 | cut -d: -f1 || true)
rs_line=$(fn_body obs_wait_rollout | grep -n 'rollout status' | head -n1 | cut -d: -f1 || true)
if [ -z "$dep_arm" ] || [ -z "$sts_arm" ] || [ -z "$rs_line" ] || [ "$rs_line" -le "$dep_arm" ] || [ "$rs_line" -ge "$sts_arm" ]; then
	bad "obs_wait_rollout: rollout status (line ${rs_line:-none}) must sit in the deployment/*) arm (line ${dep_arm:-none}) before the statefulset/*) arm (line ${sts_arm:-none}): kubectl refuses it for an OnDelete StatefulSet"
fi
# The readiness predicate: current generation observed, at least one replica
# wanted, every wanted replica created and Ready, and the revision check the
# strategy itself promises: RollingUpdate rolls every pod to updateRevision,
# so updatedReplicas must reach spec.replicas; OnDelete never rolls a pod and
# deliberately keeps old-revision pods Ready, so requiring updatedReplicas
# there would wait the full bound for a healthy Alertmanager (the G3 blockage
# in a new shape); any other strategy is never ready.
in_fn obs_statefulset_ready "reads the object as JSON through the wrapper" '^[[:space:]]*kubectl -n "\$OBSERVABILITY_NAMESPACE" get "\$1" -o json \|$'
in_fn obs_statefulset_ready "decides with jq -e (false and null are failures)" "jq -e '"
in_fn obs_statefulset_ready "an unset strategy is the API's RollingUpdate default" '^[[:space:]]*\| \(\.spec\.updateStrategy\.type // "RollingUpdate"\) as \$strategy$'
in_fn obs_statefulset_ready "zero desired replicas is not ready" '^[[:space:]]*\| \$want >= 1$'
in_fn obs_statefulset_ready "the object must carry a generation" 'and \(\.metadata\.generation // 0\) >= 1$'
in_fn obs_statefulset_ready "controller observed the current generation" 'and \.status\.observedGeneration == \.metadata\.generation$'
in_fn obs_statefulset_ready "all desired replicas created" 'and \(\.status\.replicas // 0\) == \$want$'
in_fn obs_statefulset_ready "all desired replicas ready" 'and \(\.status\.readyReplicas // 0\) == \$want$'
in_fn obs_statefulset_ready "RollingUpdate: every desired replica on the update revision" 'and \(if \$strategy == "RollingUpdate" then \(\.status\.updatedReplicas // 0\) == \$want$'
in_fn obs_statefulset_ready "OnDelete: Ready at the observed generation is the whole promise" '^[[:space:]]*elif \$strategy == "OnDelete" then true$'
in_fn obs_statefulset_ready "any other strategy is never ready" "^[[:space:]]*else false end\)' >/dev/null$"
# The readiness conditions are joined by "and" only: no "or" may let one of
# them stand in for another.
! fn_body obs_statefulset_ready | grep -E '(^|[^a-zA-Z_])or([^a-zA-Z_]|$)' >/dev/null ||
	bad "obs_statefulset_ready joins conditions with or; every condition must be required"
# The revision check is the only place the strategy is consulted: the shared
# conditions (generation, created, Ready) must not be weakened per strategy.
strategy_uses=$(fn_body obs_statefulset_ready | grep -c '\$strategy ==' || true)
[ "$strategy_uses" -eq 2 ] || bad "obs_statefulset_ready consults \$strategy $strategy_uses times, expected exactly 2 (the RollingUpdate and OnDelete arms of the revision check)"
in_fn obs_statefulset_status "an unsupported strategy is called out in the timeout message" 'unsupported updateStrategy: obs_statefulset_ready understands only RollingUpdate and OnDelete and never holds for'
in_fn obs_assert_service_port "port read through the shared resolver" 'got=\$\(obs_service_http_port "\$OBSERVABILITY_NAMESPACE" "\$svc"\)'
in_fn obs_assert_service_port "port drift refused" 'refusing to hand KubeNeuron an endpoint the Service does not answer on'
# Both endpoints are patched as External, from the constants.
in_fn cmd_deploy_observability "VMSingle endpoint patched External" 'victoriaMetrics: \{mode: "External", endpoint: \$vm\}'
in_fn cmd_deploy_observability "Alertmanager endpoint patched External" 'alertmanager: \{mode: "External", endpoint: \$am\}'
in_fn cmd_deploy_observability "patch arguments are the constants" '--arg vm "\$VMSINGLE_ENDPOINT" --arg am "\$VMALERTMANAGER_ENDPOINT"'
# No swallowed failure anywhere in the new block, and no wrapper bypass.
for fn in cmd_deploy_observability copy_webhook_token fetch_vm_operator_manifest obs_wait_rollout obs_statefulset_ready obs_statefulset_status obs_assert_service_port assert_observability_profile; do
	! fn_body "$fn" | grep -vE '^[[:space:]]*#' | grep -E '\|\| *(true|:|log )' >/dev/null ||
		bad "$fn swallows a failure (|| true / || log)"
	! fn_body "$fn" | grep 'command kubectl' >/dev/null ||
		bad "$fn bypasses the kubectl wrapper (E2E_KUBECONFIG endpoint check)"
done
# Every curl from the new block through the observability block is bounded.
! awk '/^lock_field\(\) \{/,/^cmd_teardown\(\) \{/' "$script" | grep -Ew curl |
	grep -vE '^[[:space:]]*#|require_cmd' | grep -v -- '--max-time' | grep . >/dev/null ||
	bad "unbounded curl in the deploy-observability or test-observability block"

note "static: the webhook token copy is namespace-correct and fail-closed"
in_fn copy_webhook_token "root must exist (names the phase to run first)" 'run deploy before deploy-observability'
in_fn copy_webhook_token "root's webhookToken reference is read, not assumed" "jsonpath='\{\.spec\.notifications\.webhookToken\.name\}'"
in_fn copy_webhook_token "reference must be the install.sh Secret" '\[ "\$ref" = "\$src" \] \|\|'
in_fn copy_webhook_token "source is the installation namespace" '^[[:space:]]*kubectl -n "\$RUNTIME_NAMESPACE" get secret "\$src" -o json \|$'
in_fn copy_webhook_token "copy is applied from a pipe" '^[[:space:]]*kubectl apply -f - >/dev/null$'
in_fn copy_webhook_token "copy lands under the mounted name in the monitoring namespace" '--arg name "\$ALERTMANAGER_WEBHOOK_TOKEN_SECRET" --arg ns "\$OBSERVABILITY_NAMESPACE"'
in_fn copy_webhook_token "copy carries only the token key" 'data: \{token: \.data\.token\}'
in_fn copy_webhook_token "copy is read back and compared" '\[ "\$dst_b64" = "\$src_b64" \] \|\|'
! fn_body copy_webhook_token | grep -E -- '--from-literal|--arg (tok|token)' >/dev/null ||
	bad "copy_webhook_token passes the token through an argument"
has "ROOT_NAME is the install.sh --name" '^readonly ROOT_NAME=kubeneuron$'
grep -Eq '^[[:space:]]*- kubeneuron-alertmanager-webhook-token$' "$profile_dir/vmalertmanager.yaml" ||
	bad "vmalertmanager.yaml no longer mounts kubeneuron-alertmanager-webhook-token"
grep -Eq 'create secret generic "\$NAME-webhook-token"' deploy/install.sh ||
	bad "deploy/install.sh no longer generates <name>-webhook-token; the copy source moved"
in_fn cmd_test_observability "prerequisite message names the installing phase" 'hack/hw-e2e\.sh deploy-observability installs'

note "static: the VMSingle claim's volume is tied to this run once Bound; the sweep never scopes volumes by namespace"
# This check used to REQUIRE the sweep to filter volumes by the two PVC
# namespaces. The EBS CSI driver writes no cluster identity, so that filter
# matched every cluster in the account using those namespaces, and the sweep
# could have deleted another cluster's state. The requirement is now the
# opposite, and the claim's volume is tagged for the run at the moment it
# can first be proven: Bound, before the rollout wait.
ordered cmd_deploy_observability \
	'get pvc vmsingle-kubeneuron -o jsonpath="{.status.phase}" | grep -qx Bound' \
	'tag_run_volume "$OBSERVABILITY_NAMESPACE" "$VMSINGLE_CLAIM"' \
	'obs_wait_rollout deployment/vmsingle-kubeneuron 300'
! fn_body cmd_sweep | grep -vE '^[[:space:]]*#' | grep -E 'created-for|OBSERVABILITY_NAMESPACE|RUNTIME_NAMESPACE' >/dev/null ||
	bad "cmd_sweep names a PVC namespace or the CSI created-for tags; volumes are swept by this run's tag only (see hack/verify-hw-e2e-sweep.sh)"
! grep -E 'Name=tag:[^ ]*created-for' "$script" >/dev/null ||
	bad "a volume filter on the CSI created-for tags is back in the harness"
[ "$(script_const VMSINGLE_CLAIM)" = 'vmsingle-${ROOT_NAME}' ] ||
	bad "VMSINGLE_CLAIM is '$(script_const VMSINGLE_CLAIM)', not vmsingle-<VMSingle name>; the profile's VMSingle is named kubeneuron"

# StatefulSet fixtures, shaped like the operator's VMAlertmanager object (one
# replica, OnDelete). Shared by the deploy-observability green path (the
# ready one) and the StatefulSet-wait scenarios below.
sts_fixtures="$work/sts"
mkdir -p "$sts_fixtures"
# sts_fixture <name> <strategy|""> <generation> <observedGeneration|null> <desired> <replicas> <ready> <updated> <currentRevision> <updateRevision>
# An empty strategy leaves spec.updateStrategy out, as an object the API
# server has not defaulted would look (it defaults to RollingUpdate).
sts_fixture() {
	jq -n --arg strategy "$2" --argjson gen "$3" --argjson obs "$4" --argjson want "$5" \
		--argjson replicas "$6" --argjson ready "$7" --argjson updated "$8" --arg cur "$9" --arg upd "${10}" '{
		apiVersion: "apps/v1", kind: "StatefulSet",
		metadata: {name: "vmalertmanager-kubeneuron", namespace: "kubeneuron-monitoring", generation: $gen},
		spec: ({replicas: $want, serviceName: "vmalertmanager-kubeneuron"}
			+ (if $strategy == "" then {} else {updateStrategy: {type: $strategy}} end)),
		status: (if $obs == null then {} else {observedGeneration: $obs, replicas: $replicas, readyReplicas: $ready,
			updatedReplicas: $updated, currentRevision: $cur, updateRevision: $upd} end)}' >"$sts_fixtures/$1.json"
}
# ondelete-ready is the operator's StatefulSet after it has rotated the pod
# once: a stale currentRevision (the controller reconciles it with
# updateRevision only after a RollingUpdate), pod on the update revision.
# ondelete-old-revision is the same StatefulSet right after a spec change the
# operator has not acted on: generation observed, the pod Ready but still on
# the previous revision, updatedReplicas 0. OnDelete permits exactly that for
# as long as the operator likes, with Alertmanager serving the whole time, so
# the wait MUST accept it; requiring updatedReplicas there replays the G3
# blockage as a 300s timeout on a healthy StatefulSet. The identical shape
# under RollingUpdate (rollingupdate-old-revision), or with the strategy
# unset and therefore defaulted to RollingUpdate (nostrategy-old-revision),
# is a rollout in progress and must NOT be accepted.
sts_fixture ondelete-ready OnDelete 3 3 1 1 1 1 vmalertmanager-kubeneuron-5c6d7 vmalertmanager-kubeneuron-7f8a9
sts_fixture ondelete-old-revision OnDelete 2 2 1 1 1 0 vmalertmanager-kubeneuron-5c6d7 vmalertmanager-kubeneuron-7f8a9
sts_fixture rollingupdate-ready RollingUpdate 1 1 1 1 1 1 vmalertmanager-kubeneuron-5c6d7 vmalertmanager-kubeneuron-5c6d7
sts_fixture rollingupdate-old-revision RollingUpdate 2 2 1 1 1 0 vmalertmanager-kubeneuron-5c6d7 vmalertmanager-kubeneuron-7f8a9
sts_fixture nostrategy-ready "" 1 1 1 1 1 1 vmalertmanager-kubeneuron-5c6d7 vmalertmanager-kubeneuron-5c6d7
sts_fixture nostrategy-old-revision "" 2 2 1 1 1 0 vmalertmanager-kubeneuron-5c6d7 vmalertmanager-kubeneuron-7f8a9
sts_fixture unknown-strategy Partitioned 1 1 1 1 1 1 vmalertmanager-kubeneuron-5c6d7 vmalertmanager-kubeneuron-5c6d7
sts_fixture zero-desired OnDelete 1 1 0 0 0 0 vmalertmanager-kubeneuron-5c6d7 vmalertmanager-kubeneuron-5c6d7
sts_fixture stale-generation OnDelete 4 3 1 1 1 1 vmalertmanager-kubeneuron-5c6d7 vmalertmanager-kubeneuron-5c6d7
sts_fixture not-ready OnDelete 1 1 1 1 0 1 vmalertmanager-kubeneuron-5c6d7 vmalertmanager-kubeneuron-5c6d7
sts_fixture empty-status OnDelete 1 null 1 0 0 0 "" ""
for f in ondelete-ready ondelete-old-revision rollingupdate-ready rollingupdate-old-revision nostrategy-ready nostrategy-old-revision unknown-strategy zero-desired stale-generation not-ready empty-status; do
	jq -e '.kind == "StatefulSet"' "$sts_fixtures/$f.json" >/dev/null || bad "StatefulSet fixture $f is not valid JSON"
done
jq -e '.spec | has("updateStrategy") | not' "$sts_fixtures/nostrategy-ready.json" >/dev/null ||
	bad "StatefulSet fixture nostrategy-ready carries an updateStrategy; it must not"
jq -e '.spec.updateStrategy.type == "OnDelete" and .status.updatedReplicas == 0 and .status.readyReplicas == 1' "$sts_fixtures/ondelete-old-revision.json" >/dev/null ||
	bad "StatefulSet fixture ondelete-old-revision is not a Ready OnDelete object on the old revision"

# --- dynamic: the real deploy-observability against scripted tools ----------
obsbin="$work/obsbin"
mkdir -p "$obsbin"
cp "$fakebin/aws" "$obsbin/aws"
# kubectl: logs every call, answers the reads the phase makes, keeps the
# applied Secret and the patched endpoints as state so the script's own
# read-backs are real comparisons, not echoes of what it asked for.
cat >"$obsbin/kubectl" <<'EOF'
#!/usr/bin/env bash
# Each call is logged with a closing ' <' so an exact argument tail can be
# matched (`-o json <` is not a prefix of `-o jsonpath=...`).
printf 'kubectl %s <\n' "$*" >>"$FAKE_KUBECTL_LOG"
state="$FAKE_KUBECTL_STATE"
args="$*"
case "$args" in
*"config current-context"*) echo fake ;;
*"config view"*) printf 'https://fake.eks.local' ;;
*"get kubeneuron kubeneuron -o jsonpath={.spec.notifications.webhookToken.name}"*)
	if [ "${FAKE_NO_KUBENEURON:-0}" = 1 ]; then exit 1; fi
	printf 'kubeneuron-webhook-token' ;;
*"get kubeneuron kubeneuron -o jsonpath={.spec.observability.victoriaMetrics.endpoint}"*)
	cat "$state/vm-endpoint" 2>/dev/null || printf 'http://vmsingle-unset.kube-neuron.svc:8428' ;;
*"get kubeneuron kubeneuron -o jsonpath={.spec.observability.alertmanager.endpoint}"*)
	cat "$state/am-endpoint" 2>/dev/null || printf 'http://alertmanager-unset.kube-neuron.svc:9093' ;;
*"get kubeneuron kubeneuron -o json"*)
	echo '{"metadata":{"generation":2},"status":{"observedGeneration":2,"conditions":[{"type":"Ready","status":"True"}]}}' ;;
*"get kubeneuron kubeneuron"*) if [ "${FAKE_NO_KUBENEURON:-0}" = 1 ]; then exit 1; fi ;;
*"get secret kubeneuron-webhook-token -o jsonpath={.data.token}"*) printf '%s' "$FAKE_TOKEN_B64" ;;
*"get secret kubeneuron-webhook-token -o json"*)
	printf '{"apiVersion":"v1","kind":"Secret","type":"Opaque","metadata":{"name":"kubeneuron-webhook-token","namespace":"kube-neuron","uid":"u","resourceVersion":"7"},"data":{"token":"%s"}}' "$FAKE_TOKEN_B64" ;;
*"get secret kubeneuron-alertmanager-webhook-token -o jsonpath={.data.token}"*)
	[ -f "$state/applied-secret.json" ] || exit 1
	jq -r '.data.token' "$state/applied-secret.json" | tr -d '\n' ;;
*"apply -f -"*)
	n=$(ls "$state" | grep -c '^stdin-' || true)
	cat >"$state/stdin-$n.json"
	if jq -e '.kind == "Secret"' "$state/stdin-$n.json" >/dev/null 2>&1; then
		cp "$state/stdin-$n.json" "$state/applied-secret.json"
	fi ;;
*"get pvc vmsingle-kubeneuron -o jsonpath={.status.phase}"*) printf 'Bound' ;;
# The Bound claim and the EBS CSI PV it names, for the run-volume tagging
# that follows the Bound wait: claimRef agrees on namespace, name and uid.
*"get pvc vmsingle-kubeneuron -o json"*)
	echo '{"metadata":{"uid":"uid-vmsingle"},"spec":{"volumeName":"pvc-vmsingle"},"status":{"phase":"Bound"}}' ;;
*"get pv pvc-vmsingle -o json"*)
	echo '{"spec":{"claimRef":{"namespace":"kubeneuron-monitoring","name":"vmsingle-kubeneuron","uid":"uid-vmsingle"},"csi":{"driver":"ebs.csi.aws.com","volumeHandle":"vol-0b5e0000000000001"}}}' ;;
*"get service vmsingle-kubeneuron -o json"*)
	# 8429 is what the VictoriaMetrics operator generates for VMSingle (observed
	# on EKS 2026-10-18); FAKE_VMSINGLE_PORT overrides it for the drift case.
	printf '{"spec":{"ports":[{"name":"http","port":%s}]}}' "${FAKE_VMSINGLE_PORT:-8429}" ;;
*"get service vmalertmanager-kubeneuron -o json"*)
	echo '{"spec":{"ports":[{"name":"http","port":9093},{"name":"tcp-mesh","port":9094},{"name":"udp-mesh","port":9094}]}}' ;;
*"rollout status statefulset/"*)
	# Verbatim what kubectl 1.33 answered on EKS (2026-10-18) for the OnDelete
	# StatefulSet the operator generates for VMAlertmanager.
	echo 'error: rollout status is only available for RollingUpdate strategy type' >&2
	exit 1 ;;
*"get statefulset/vmalertmanager-kubeneuron -o json"*)
	cat "$FAKE_STS_FIXTURES/${FAKE_STS:-ondelete-ready}.json" ;;
*"patch kubeneuron kubeneuron --type merge -p "*)
	patch="${args##*--type merge -p }"
	printf '%s' "$patch" >"$state/patch.json"
	jq -r '.spec.observability.victoriaMetrics.endpoint' "$state/patch.json" | tr -d '\n' >"$state/vm-endpoint"
	jq -r '.spec.observability.alertmanager.endpoint' "$state/patch.json" | tr -d '\n' >"$state/am-endpoint" ;;
# The exporter gate. FAKE_EXPORTER=crashloop is the 2026-10-18 G4dn
# cluster: the DaemonSet never rolls out (the exporter exits on an unknown CSV
# field name) and the Service has no endpoints. FAKE_EXPORTER=silent rolls
# out and serves, but without the pipeline field (see the fake curl).
*"rollout status daemonset/nvidia-dcgm-exporter --timeout=300s"*)
	if [ "${FAKE_EXPORTER:-ok}" = crashloop ]; then
		echo 'error: timed out waiting for the condition' >&2
		exit 1
	fi
	echo 'daemon set "nvidia-dcgm-exporter" successfully rolled out' ;;
*"get endpoints nvidia-dcgm-exporter"*)
	[ "${FAKE_EXPORTER:-ok}" = crashloop ] || printf '10.0.0.7' ;;
*"get service nvidia-dcgm-exporter -o json"*)
	# The operator's exporter Service: one port, named gpu-metrics, not http.
	echo '{"spec":{"ports":[{"name":"gpu-metrics","port":9400}]}}' ;;
*"get pods -l app=nvidia-dcgm-exporter"*)
	echo 'nvidia-dcgm-exporter-fake   0/1   CrashLoopBackOff   7   fake-node' ;;
*"logs -l app=nvidia-dcgm-exporter"*)
	echo 'FAKE-EXPORTER-LOG level=fatal msg="field DCGM_FI_DEV_MEMORY_UNREPAIRABLE is not a valid field"' ;;
*"get configmap kubeneuron-dcgm-metrics"*)
	printf '# profile\nDCGM_FI_DEV_GPU_TEMP, gauge, GPU temperature.\nDCGM_FI_DEV_MEMORY_UNREPAIRABLE_FLAG, gauge, x.\n' ;;
*port-forward*)
	# Record the pid, then exec: the pid stays the sleep's, so the verifier
	# can ask whether the phase's close helper really killed the forward.
	printf '%s' "$$" >"$state/port-forward.pid"
	exec sleep 6007 ;;
*) exit 0 ;;
esac
EOF
# curl: the manifest download writes FAKE_MANIFEST_CONTENT; VMSingle's
# /health answers once the fake port-forward has recorded its pid, as the
# real one can only answer once the forward is up (an instant answer let the
# phase close the forward before its subshell had reached kubectl, and the
# "port-forward not reached" assertion below then failed by timing alone);
# its /api/v1/query returns one sample; the exporter's /metrics answers the
# same way, with the pipeline field unless FAKE_EXPORTER=silent, and with no
# DCGM_ series at all under FAKE_EXPORTER=empty.
cat >"$obsbin/curl" <<'EOF'
#!/usr/bin/env bash
printf 'curl %s\n' "$*" >>"$FAKE_CURL_LOG"
out="" url=""
while [ $# -gt 0 ]; do
	case "$1" in
	-o) out="$2"; shift ;;
	http://*|https://*) url="$1" ;;
	esac
	shift
done
case "$url" in
https://*) printf '%s\n' "${FAKE_MANIFEST_CONTENT:-not the manifest}" >"$out" ;;
*/health) [ -f "${FAKE_KUBECTL_STATE:-/nonexistent}/port-forward.pid" ] || exit 7 ;;
*/metrics)
	[ -f "${FAKE_KUBECTL_STATE:-/nonexistent}/port-forward.pid" ] || exit 7
	# FAKE_EXPORTER=empty: a body with no DCGM_ series at all (exporter up,
	# DCGM engine attached to nothing), only Go runtime noise.
	echo '# HELP go_goroutines Number of goroutines that currently exist.'
	echo 'go_goroutines 9'
	[ "${FAKE_EXPORTER:-ok}" = empty ] && exit 0
	echo 'DCGM_FI_DEV_POWER_USAGE{gpu="0",UUID="GPU-fake",modelName="Tesla T4"} 28.5'
	[ "${FAKE_EXPORTER:-ok}" = silent ] || echo 'DCGM_FI_DEV_GPU_TEMP{gpu="0",UUID="GPU-fake",modelName="Tesla T4"} 41' ;;
*/api/v1/query) echo '{"status":"success","data":{"result":[{"metric":{"UUID":"GPU-fake"},"value":[1,"42"]}]}}' ;;
*) exit 22 ;;
esac
EOF
# sha256sum that accepts anything: ONLY for the scenarios that must get past
# the download, since no offline fixture can match the real artifact's hash.
okhash="$work/okhash"
mkdir -p "$okhash"
printf '%s\n' '#!/usr/bin/env bash' 'cat >/dev/null' 'exit 0' >"$okhash/sha256sum"
chmod +x "$obsbin"/* "$okhash"/*

# run_obs <label> <expected-rc> <script-path> <extra-PATH-prefix-or-""> [VAR=value...]
# Runs deploy-observability; leaves logs at $work/obs.<label>.{kubectl,curl,err}
# and the fake kubectl state at $work/obs.<label>.state.
run_obs() {
	local label="$1" want_rc="$2" target="$3" prefix="$4"
	shift 4
	local klog="$work/obs.$label.kubectl" clog="$work/obs.$label.curl" err="$work/obs.$label.err"
	local alog="$work/obs.$label.aws" state="$work/obs.$label.state" kc="$work/obs.$label.kubeconfig"
	: >"$klog"
	: >"$clog"
	: >"$alog"
	mkdir -p "$state"
	printf '%s\n' "apiVersion: v1" "kind: Config" "current-context: fake" >"$kc"
	local path="$obsbin:$PATH"
	[ -z "$prefix" ] || path="$prefix:$path"
	set +e
	env PATH="$path" FAKE_KUBECTL_LOG="$klog" FAKE_CURL_LOG="$clog" FAKE_AWS_LOG="$alog" FAKE_KUBECTL_STATE="$state" \
		FAKE_TOKEN_B64="dGhlLXJlYWwtdG9rZW4=" FAKE_STS_FIXTURES="$sts_fixtures" \
		CLUSTER_NAME=kubeneuron-e2e-verify E2E_STATE_DIR="$work/obs.$label.e2e" E2E_KUBECONFIG="$kc" "$@" \
		timeout 120 bash "$target" deploy-observability >/dev/null 2>"$err"
	local rc=$?
	set -e
	[ "$rc" -eq "$want_rc" ] || bad "$label: deploy-observability exited $rc, expected $want_rc: $(tr '\n' ' ' <"$err")"
}

note "dynamic: a lock that disagrees with the script is refused before any download"
mkdir -p "$work/drift/hack" "$work/drift/deploy/kubernetes/dependencies"
cp "$script" "$work/drift/hack/hw-e2e.sh"
cp -R "$profile_dir" "$work/drift/deploy/kubernetes/dependencies/observability"
sed -E 's/^(    sha256: )28f0ab84/\1deadbeef/' "$lock" >"$work/drift/deploy/kubernetes/dependencies/versions.lock.yaml"
grep -q deadbeef "$work/drift/deploy/kubernetes/dependencies/versions.lock.yaml" || bad "lock-drift fixture did not alter the sha256"
run_obs lock-drift 1 "$work/drift/hack/hw-e2e.sh" ""
grep -q 'locks manifest SHA-256' "$work/obs.lock-drift.err" || bad "lock-drift: stderr lacks the lock disagreement: $(tr '\n' ' ' <"$work/obs.lock-drift.err")"
! grep -q . "$work/obs.lock-drift.curl" || bad "lock-drift: a download was attempted"
! grep -q ' apply ' "$work/obs.lock-drift.kubectl" || bad "lock-drift: something was applied"

note "dynamic: a download that does not match the lock is refused before any apply (real sha256sum)"
run_obs checksum 1 "$script" ""
grep -q 'does not match the locked SHA-256' "$work/obs.checksum.err" || bad "checksum: stderr lacks the mismatch: $(tr '\n' ' ' <"$work/obs.checksum.err")"
grep -q "curl .*$vm_url" "$work/obs.checksum.curl" || bad "checksum: the locked URL was not what curl fetched: $(cat "$work/obs.checksum.curl")"
! grep -q ' apply ' "$work/obs.checksum.kubectl" || bad "checksum: something was applied after the mismatch"
for leftover in "$work/obs.checksum.e2e"/vm-operator-*; do
	if [ -e "$leftover" ]; then
		bad "checksum: the unverified download was kept: $leftover"
	fi
done

note "dynamic: a cluster without KubeNeuron is refused before anything is installed"
run_obs no-root 1 "$script" "" FAKE_NO_KUBENEURON=1
grep -q 'run deploy before deploy-observability' "$work/obs.no-root.err" || bad "no-root: stderr lacks the deploy hint: $(tr '\n' ' ' <"$work/obs.no-root.err")"
! grep -q ' apply ' "$work/obs.no-root.kubectl" || bad "no-root: something was applied"
! grep -q . "$work/obs.no-root.curl" || bad "no-root: a download was attempted"

note "dynamic: a VMSingle Service on the wrong port stops the phase before Alertmanager and the repoint"
# 8428 is the drift the first G3 hit in reverse (constant 8428, Service 8429);
# the refusal must name both ports so the operator can see which side moved.
run_obs port-drift 1 "$script" "$okhash" FAKE_VMSINGLE_PORT=8428
grep -q 'refusing to hand KubeNeuron an endpoint the Service does not answer on' "$work/obs.port-drift.err" ||
	bad "port-drift: stderr lacks the port refusal: $(tr '\n' ' ' <"$work/obs.port-drift.err")"
grep -q "serves port 8428 but the checked-in endpoint ${vmsingle_ep} names 8429" "$work/obs.port-drift.err" ||
	bad "port-drift: the refusal does not name the served port and the endpoint: $(tr '\n' ' ' <"$work/obs.port-drift.err")"
grep -q 'apply -f .*/vmsingle.yaml' "$work/obs.port-drift.kubectl" || bad "port-drift: VMSingle was never applied, so the port was never read from a live Service"
! grep -qE 'apply -f .*/(vmagent|vmalertmanager|vmalert|rules)\.yaml' "$work/obs.port-drift.kubectl" || bad "port-drift: the stack continued past the refused port"
! grep -q 'apply -f -' "$work/obs.port-drift.kubectl" || bad "port-drift: the token was copied past the refused port"
! grep -q 'patch kubeneuron' "$work/obs.port-drift.kubectl" || bad "port-drift: the root was repointed at a port the Service does not serve"

# The exporter gate against scripted tools. This proves the phase's order
# and that it fails closed; it says nothing about what any GPU serves (the
# hardware run is the only evidence for that, and the one on 2026-10-18 is
# why the gate exists).
note "dynamic: an exporter that never rolls out stops the phase before the manifest is fetched, with its diagnosis"
run_obs exporter-crashloop 1 "$script" "$okhash" FAKE_EXPORTER=crashloop
grep -q 'nvidia-dcgm-exporter DaemonSet is not rolled out' "$work/obs.exporter-crashloop.err" ||
	bad "exporter-crashloop: stderr lacks the rollout refusal: $(tr '\n' ' ' <"$work/obs.exporter-crashloop.err")"
for needle in 'CrashLoopBackOff' 'FAKE-EXPORTER-LOG' 'DCGM_FI_DEV_MEMORY_UNREPAIRABLE_FLAG' 'field names the profile ConfigMap'; do
	grep -q "$needle" "$work/obs.exporter-crashloop.err" ||
		bad "exporter-crashloop: the diagnosis lacks '$needle': $(tr '\n' ' ' <"$work/obs.exporter-crashloop.err")"
done
! grep -q . "$work/obs.exporter-crashloop.curl" || bad "exporter-crashloop: curl ran (download or scrape) after the exporter refusal: $(cat "$work/obs.exporter-crashloop.curl")"
! grep -q ' apply ' "$work/obs.exporter-crashloop.kubectl" || bad "exporter-crashloop: something was applied"
! grep -q 'port-forward' "$work/obs.exporter-crashloop.kubectl" || bad "exporter-crashloop: a port-forward was opened to an exporter with no endpoints"

note "dynamic: an exporter that serves no ${OBS_PIPELINE_FIELD:-DCGM_FI_DEV_GPU_TEMP} stops the phase before the manifest is fetched, naming what it serves"
run_obs exporter-silent 1 "$script" "$okhash" FAKE_EXPORTER=silent
grep -q 'exporter serves no DCGM_FI_DEV_GPU_TEMP sample' "$work/obs.exporter-silent.err" ||
	bad "exporter-silent: stderr lacks the missing-sample refusal: $(tr '\n' ' ' <"$work/obs.exporter-silent.err")"
grep -q 'DCGM series served: DCGM_FI_DEV_POWER_USAGE' "$work/obs.exporter-silent.err" ||
	bad "exporter-silent: the refusal does not name the series the exporter does serve: $(tr '\n' ' ' <"$work/obs.exporter-silent.err")"
grep -q 'curl .*--max-time .*http://127.0.0.1:19400/metrics' "$work/obs.exporter-silent.curl" ||
	bad "exporter-silent: /metrics was not read through a bounded curl: $(cat "$work/obs.exporter-silent.curl")"
! grep -q 'https://' "$work/obs.exporter-silent.curl" || bad "exporter-silent: the operator manifest was downloaded after the exporter refusal"
! grep -q ' apply ' "$work/obs.exporter-silent.kubectl" || bad "exporter-silent: something was applied"
grep -q 'port-forward service/nvidia-dcgm-exporter 19400:9400' "$work/obs.exporter-silent.kubectl" ||
	bad "exporter-silent: the exporter was not forwarded on the port its Service declares"
if [ -f "$work/obs.exporter-silent.state/port-forward.pid" ]; then
	pf_pid=$(cat "$work/obs.exporter-silent.state/port-forward.pid")
	if kill -0 "$pf_pid" 2>/dev/null; then
		bad "exporter-silent: the exporter port-forward (pid $pf_pid) outlived the refusal"
		kill "$pf_pid" 2>/dev/null || true
	fi
else
	bad "exporter-silent: the exporter port-forward never recorded its pid"
fi

# The same gate against a body with no DCGM_ series at all: the message must
# still be the refusal, with "none" for the served list, after the diagnosis.
note "dynamic: an exporter whose /metrics carries no DCGM_ series at all stops the phase the same way, saying it serves none"
run_obs exporter-empty 1 "$script" "$okhash" FAKE_EXPORTER=empty
grep -q 'exporter serves no DCGM_FI_DEV_GPU_TEMP sample (DCGM series served: none)' "$work/obs.exporter-empty.err" ||
	bad "exporter-empty: stderr lacks the missing-sample refusal with an empty served list: $(tr '\n' ' ' <"$work/obs.exporter-empty.err")"
for needle in 'CrashLoopBackOff' 'FAKE-EXPORTER-LOG' 'field names the profile ConfigMap'; do
	grep -q "$needle" "$work/obs.exporter-empty.err" ||
		bad "exporter-empty: the diagnosis lacks '$needle': $(tr '\n' ' ' <"$work/obs.exporter-empty.err")"
done
grep -q 'curl .*--max-time .*http://127.0.0.1:19400/metrics' "$work/obs.exporter-empty.curl" ||
	bad "exporter-empty: /metrics was not read through a bounded curl: $(cat "$work/obs.exporter-empty.curl")"
! grep -q 'https://' "$work/obs.exporter-empty.curl" || bad "exporter-empty: the operator manifest was downloaded after the exporter refusal"
! grep -q ' apply ' "$work/obs.exporter-empty.kubectl" || bad "exporter-empty: something was applied"
if [ -f "$work/obs.exporter-empty.state/port-forward.pid" ]; then
	pf_pid=$(cat "$work/obs.exporter-empty.state/port-forward.pid")
	if kill -0 "$pf_pid" 2>/dev/null; then
		bad "exporter-empty: the exporter port-forward (pid $pf_pid) outlived the refusal"
		kill "$pf_pid" 2>/dev/null || true
	fi
else
	bad "exporter-empty: the exporter port-forward never recorded its pid"
fi

note "dynamic: the green path installs in order, copies the token byte-for-byte and repoints both endpoints"
run_obs green 0 "$script" "$okhash"
klog="$work/obs.green.kubectl"
state="$work/obs.green.state"
# Order of the mutating calls, as logged.
expect_order() {
	local prev=0 prev_label="(start)" line label
	for label in "$@"; do
		line=$(grep -nF -- "$label" "$klog" | head -n1 | cut -d: -f1 || true)
		if [ -z "$line" ]; then
			bad "green: kubectl never ran '$label'"
			continue
		fi
		[ "$line" -gt "$prev" ] || bad "green: '$label' (call $line) ran before '$prev_label' (call $prev)"
		prev="$line"
		prev_label="$label"
	done
}
expect_order \
	'rollout status daemonset/nvidia-dcgm-exporter --timeout=300s' \
	'get endpoints nvidia-dcgm-exporter' \
	'get service nvidia-dcgm-exporter -o json' \
	'port-forward service/nvidia-dcgm-exporter 19400:9400' \
	"apply --server-side --field-manager=kubeneuron-dependencies -f $work/obs.green.e2e/vm-operator-${vm_version}-no-webhook.yaml" \
	'wait --for=condition=Established --timeout=120s crd/vmsingles.operator.victoriametrics.com' \
	'wait --for=condition=Established --timeout=120s crd/vmrules.operator.victoriametrics.com' \
	'-n vm rollout status deployment/vm-operator --timeout=300s' \
	"apply -f $repo_root/$profile_dir/namespace.yaml" \
	"apply -f $repo_root/$profile_dir/vmsingle.yaml" \
	'get pvc vmsingle-kubeneuron -o jsonpath={.status.phase}' \
	'get pvc vmsingle-kubeneuron -o json <' \
	'get pv pvc-vmsingle -o json <' \
	'rollout status deployment/vmsingle-kubeneuron --timeout=300s' \
	'get service vmsingle-kubeneuron -o json' \
	'get secret kubeneuron-webhook-token -o json' \
	'apply -f -' \
	'get secret kubeneuron-alertmanager-webhook-token -o jsonpath={.data.token}' \
	"apply -f $repo_root/$profile_dir/vmagent.yaml" \
	'rollout status deployment/vmagent-kubeneuron --timeout=300s' \
	"apply -f $repo_root/$profile_dir/vmalertmanager.yaml" \
	'get statefulset/vmalertmanager-kubeneuron -o json <' \
	'get service vmalertmanager-kubeneuron -o json' \
	"apply -f $repo_root/$profile_dir/vmalert.yaml" \
	'rollout status deployment/vmalert-kubeneuron --timeout=300s' \
	"apply -f $repo_root/$profile_dir/rules.yaml" \
	'get vmrule kubeneuron-gpu' \
	'get vmrule kubeneuron-gpu-recording' \
	'port-forward service/vmsingle-kubeneuron 18429:8429' \
	'patch kubeneuron kubeneuron --type merge -p ' \
	'get kubeneuron kubeneuron -o json <'
# The StatefulSet never goes through rollout status (the fake refuses it the
# way kubectl does, so the phase would also have exited 1 above); the
# Deployments still do.
! grep -q 'rollout status statefulset/' "$klog" ||
	bad "green: rollout status was run against the Alertmanager StatefulSet, which kubectl refuses for the operator's OnDelete strategy"
for dep in vmsingle-kubeneuron vmagent-kubeneuron vmalert-kubeneuron; do
	grep -q "rollout status deployment/$dep --timeout=300s" "$klog" ||
		bad "green: deployment/$dep was not waited on with rollout status"
done
# The VMSingle claim's volume was tagged with exactly this run's identity,
# read by its id first and read back through the sweep's filter after, and
# recorded in the run's state dir.
alog="$work/obs.green.aws"
grep -qF 'aws ec2 describe-volumes --region us-east-1 --volume-ids vol-0b5e0000000000001 --output json' "$alog" ||
	bad "green: the VMSingle volume was not read by its id before tagging: $(grep -c . "$alog") aws calls"
grep -qF 'aws ec2 create-tags --region us-east-1 --resources vol-0b5e0000000000001 --tags Key=kubeneuron:e2e,Value=true Key=kubeneuron:e2e-run,Value=kubeneuron-e2e-verify Key=kubeneuron:e2e-claim,Value=kubeneuron-monitoring/vmsingle-kubeneuron' "$alog" ||
	bad "green: the VMSingle volume was not tagged with exactly this run's identity: $(grep create-tags "$alog" || echo 'no create-tags at all')"
grep -qF 'aws ec2 describe-volumes --region us-east-1 --volume-ids vol-0b5e0000000000001 --filters Name=tag:kubeneuron:e2e,Values=true Name=tag:kubeneuron:e2e-run,Values=kubeneuron-e2e-verify Name=tag:kubeneuron:e2e-claim,Values=kubeneuron-monitoring/vmsingle-kubeneuron' "$alog" ||
	bad "green: the run tags were not read back after create-tags"
! grep -E 'describe-volumes.*created-for' "$alog" >/dev/null || bad "green: a volume was looked up by the CSI created-for tags"
grep -qsxF 'kubeneuron-e2e-verify vol-0b5e0000000000001 kubeneuron-monitoring/vmsingle-kubeneuron' "$work/obs.green.e2e/run-volumes" ||
	bad "green: the tagged VMSingle volume was not recorded in the run's state dir: $(cat "$work/obs.green.e2e/run-volumes" 2>/dev/null || echo 'no run-volumes file')"
[ ! -e "$work/obs.green.e2e/unproven-claims" ] || bad "green: an unproven-claims record was written: $(cat "$work/obs.green.e2e/unproven-claims")"
# Every apply and wait went through the wrapper's dedicated kubeconfig.
! grep -E '^kubectl (apply|wait|patch|rollout)' "$klog" | grep -v -- "--kubeconfig $work/obs.green.kubeconfig" | grep . >/dev/null ||
	bad "green: a mutating kubectl call ran without the dedicated kubeconfig"
# The verified manifest is the file applied, and it is what curl fetched.
grep -q "curl .* -o $work/obs.green.e2e/vm-operator-${vm_version}-no-webhook.yaml.download $vm_url" "$work/obs.green.curl" ||
	bad "green: the manifest was not fetched from the locked URL into the state dir: $(cat "$work/obs.green.curl")"
# The exporter's /metrics was read, bounded, before the manifest download.
scrape_line=$(grep -n 'curl .*--max-time .*http://127.0.0.1:19400/metrics' "$work/obs.green.curl" | head -n1 | cut -d: -f1 || true)
fetch_line=$(grep -n "curl .*$vm_url" "$work/obs.green.curl" | head -n1 | cut -d: -f1 || true)
if [ -z "$scrape_line" ] || [ -z "$fetch_line" ] || [ "$scrape_line" -ge "$fetch_line" ]; then
	bad "green: the exporter /metrics read (curl call ${scrape_line:-none}) must precede the manifest download (curl call ${fetch_line:-none})"
fi
# The pipeline-live queries, bounded, against the forwarded VMSingle.
grep -q 'curl .*--max-time .*query=DCGM_FI_DEV_GPU_TEMP .*http://127.0.0.1:18429/api/v1/query' "$work/obs.green.curl" ||
	bad "green: no bounded raw DCGM query against VMSingle"
grep -q 'curl .*--max-time .*query=kubeneuron_gpu_temperature_celsius .*http://127.0.0.1:18429/api/v1/query' "$work/obs.green.curl" ||
	bad "green: no bounded canonical-series query against VMSingle"
# The Secret copy: name, namespace, the same token, and nothing else from
# the source's metadata.
[ -f "$state/applied-secret.json" ] || bad "green: no Secret was applied from stdin"
if [ -f "$state/applied-secret.json" ]; then
	jq -e '.kind == "Secret" and .metadata.name == "kubeneuron-alertmanager-webhook-token" and .metadata.namespace == "kubeneuron-monitoring"' "$state/applied-secret.json" >/dev/null ||
		bad "green: the copied Secret is not kubeneuron-monitoring/kubeneuron-alertmanager-webhook-token: $(cat "$state/applied-secret.json")"
	jq -e '.data == {"token": "dGhlLXJlYWwtdG9rZW4="}' "$state/applied-secret.json" >/dev/null ||
		bad "green: the copied Secret does not carry exactly the source token: $(jq -c .data "$state/applied-secret.json")"
	jq -e '.metadata | has("uid") or has("resourceVersion") | not' "$state/applied-secret.json" >/dev/null ||
		bad "green: the copied Secret carries the source's uid/resourceVersion"
fi
! grep -q 'dGhlLXJlYWwtdG9rZW4=' "$klog" || bad "green: the token appeared on a kubectl command line"
# Both endpoints patched to the constants, External.
[ -f "$state/patch.json" ] || bad "green: the root was never patched"
if [ -f "$state/patch.json" ]; then
	jq -e --arg vm "$vmsingle_ep" --arg am "$am_ep" '.spec.observability.victoriaMetrics == {mode: "External", endpoint: $vm} and .spec.observability.alertmanager == {mode: "External", endpoint: $am}' "$state/patch.json" >/dev/null ||
		bad "green: the root patch is not the two External profile endpoints: $(cat "$state/patch.json")"
fi
# No port-forward left behind by the phase: the fake recorded the forward's
# own pid before exec'ing, and that process must be gone.
if [ -f "$state/port-forward.pid" ]; then
	pf_pid=$(cat "$state/port-forward.pid")
	if kill -0 "$pf_pid" 2>/dev/null; then
		bad "green: the fake port-forward (pid $pf_pid) outlived the phase: the close helper killed a subshell, not the forward"
		kill "$pf_pid" 2>/dev/null || true
	fi
else
	bad "green: the fake port-forward never recorded its pid (port-forward not reached)"
fi

# --- dynamic: obs_wait_rollout against StatefulSet fixtures -----------------
# The real helpers (wait_for, obs_wait_rollout, obs_statefulset_ready,
# obs_statefulset_status) are extracted and run with a timeout of 0, so each
# scenario is a single poll: the readiness decision is what is under test,
# not the clock. The fake kubectl refuses rollout status on a StatefulSet
# verbatim as kubectl 1.33 did on EKS, and that refusal is self-tested first.
note "dynamic: the StatefulSet wait decides on status, for both strategies, and fails closed"
printf '%s\n' '#!/usr/bin/env bash' \
	'printf "kubectl %s <\n" "$*" >>"$FAKE_KUBECTL_LOG"' \
	'case "$*" in' \
	'*"rollout status statefulset/"*)' \
	'	echo "error: rollout status is only available for RollingUpdate strategy type" >&2' \
	'	exit 1 ;;' \
	'*"rollout status deployment/"*) exit 0 ;;' \
	'*"get statefulset/"*" -o json"*)' \
	'	[ -f "$FAKE_STS_FIXTURES/$FAKE_STS.json" ] || exit 1' \
	'	cat "$FAKE_STS_FIXTURES/$FAKE_STS.json" ;;' \
	'*"get statefulset/"*|*"get deployment/"*|*"get daemonset/"*)' \
	'	[ "$FAKE_STS" != absent ] || exit 1 ;;' \
	'*) exit 1 ;;' \
	'esac' >"$fakebin/kubectl-sts"
chmod +x "$fakebin/kubectl-sts"
: >"$work/sts.selftest.kubectl"
if env PATH="$fakebin:$PATH" FAKE_KUBECTL_LOG="$work/sts.selftest.kubectl" FAKE_STS_FIXTURES="$sts_fixtures" FAKE_STS=ondelete-ready \
	kubectl-sts -n kubeneuron-monitoring rollout status statefulset/vmalertmanager-kubeneuron --timeout=300s 2>"$work/sts.selftest.err"; then
	bad "sts wait self-test: the fake kubectl accepted rollout status on a StatefulSet; the scenarios below would prove nothing"
fi
grep -q 'only available for RollingUpdate strategy type' "$work/sts.selftest.err" ||
	bad "sts wait self-test: the fake kubectl does not answer with kubectl's refusal"
grep -q 'rollout status statefulset/' "$work/sts.selftest.kubectl" ||
	bad "sts wait self-test: the fake kubectl did not log the refused rollout status call, so the refusal above did not come from it"
# The self-test's log deliberately holds that one rollout status call; it is
# not a harness scenario and is kept out of the scenario-wide assertion below.
mv "$work/sts.selftest.kubectl" "$work/sts-selftest.kubectl"
mv "$work/sts.selftest.err" "$work/sts-selftest.err"
sts_helper="$work/sts-helper.sh"
{
	echo 'set -Eeuo pipefail'
	echo 'OBSERVABILITY_NAMESPACE=kubeneuron-monitoring'
	printf '%s\n' 'die() { printf "hw-e2e: %s\n" "$*" >&2; exit 1; }'
	echo 'kubectl() { kubectl-sts "$@"; }'
	fn_body wait_for
	fn_body obs_wait_rollout
	fn_body obs_statefulset_ready
	fn_body obs_statefulset_status
	echo 'obs_wait_rollout "$1" 0'
} >"$sts_helper"
for fn in wait_for obs_wait_rollout obs_statefulset_ready obs_statefulset_status; do
	grep -q "^$fn() {" "$sts_helper" || bad "sts wait: $fn could not be extracted from the script"
done
# run_sts <scenario> <target> <READY|FAIL> [stderr-substring]
sts_scenarios=()
run_sts() {
	local scenario="$1" target="$2" want="$3" needle="${4-}" rc
	local klog="$work/sts.$scenario.kubectl" err="$work/sts.$scenario.err"
	sts_scenarios+=("$scenario")
	: >"$klog"
	set +e
	env PATH="$fakebin:$PATH" FAKE_KUBECTL_LOG="$klog" FAKE_STS_FIXTURES="$sts_fixtures" FAKE_STS="$scenario" \
		timeout 30 bash "$sts_helper" "$target" >/dev/null 2>"$err"
	rc=$?
	set -e
	if [ "$want" = READY ]; then
		[ "$rc" -eq 0 ] || bad "sts wait: $scenario ($target) exited $rc, expected ready: $(tr '\n' ' ' <"$err")"
	else
		[ "$rc" -ne 0 ] || bad "sts wait: $scenario ($target) was accepted as ready; the wait must fail closed"
	fi
	[ -z "$needle" ] || grep -qF -- "$needle" "$err" ||
		bad "sts wait: $scenario stderr lacks '$needle': $(tr '\n' ' ' <"$err")"
	! grep -q 'rollout status statefulset/' "$klog" ||
		bad "sts wait: $scenario ran rollout status against a StatefulSet"
}
sts=statefulset/vmalertmanager-kubeneuron
# Ready under each strategy. ondelete-old-revision is the P1 case: the
# operator's OnDelete StatefulSet after a spec change, pod Ready on the
# previous revision, updatedReplicas 0; it is healthy and must pass.
run_sts ondelete-ready "$sts" READY
run_sts ondelete-old-revision "$sts" READY
run_sts rollingupdate-ready "$sts" READY
run_sts nostrategy-ready "$sts" READY
# The same old-revision shape is an unfinished rollout under RollingUpdate,
# named or defaulted, and must fail with the revision counts in the message.
run_sts rollingupdate-old-revision "$sts" FAIL 'strategy=RollingUpdate generation=2 observedGeneration=2 desired=1 replicas=1 ready=1 updated=0 currentRevision=vmalertmanager-kubeneuron-5c6d7 updateRevision=vmalertmanager-kubeneuron-7f8a9'
run_sts nostrategy-old-revision "$sts" FAIL 'strategy=RollingUpdate(default) generation=2 observedGeneration=2 desired=1 replicas=1 ready=1 updated=0'
# A strategy the predicate does not understand is never ready, even fully
# Ready and converged, and the message says why.
run_sts unknown-strategy "$sts" FAIL 'strategy=Partitioned generation=1 observedGeneration=1 desired=1 replicas=1 ready=1 updated=1'
grep -qF 'unsupported updateStrategy: obs_statefulset_ready understands only RollingUpdate and OnDelete and never holds for Partitioned' "$work/sts.unknown-strategy.err" ||
	bad "sts wait: unknown-strategy failed without naming the strategy as unsupported: $(tr '\n' ' ' <"$work/sts.unknown-strategy.err")"
# The strategy-independent conditions fail closed under OnDelete too.
run_sts zero-desired "$sts" FAIL 'did not roll out within 0s: strategy=OnDelete generation=1 observedGeneration=1 desired=0 replicas=0 ready=0 updated=0'
run_sts stale-generation "$sts" FAIL 'generation=4 observedGeneration=3 desired=1'
run_sts not-ready "$sts" FAIL 'desired=1 replicas=1 ready=0 updated=1'
run_sts empty-status "$sts" FAIL 'generation=1 observedGeneration=? desired=1 replicas=0 ready=0 updated=0'
run_sts absent "$sts" FAIL 'condition never met within 0s'
! grep -q -- '-o json' "$work/sts.absent.kubectl" || bad "sts wait: absent object was read for status before it existed"
run_sts deployment deployment/vmagent-kubeneuron READY
grep -q 'rollout status deployment/vmagent-kubeneuron --timeout=0s' "$work/sts.deployment.kubectl" ||
	bad "sts wait: a Deployment no longer goes through rollout status"
run_sts unknown-kind daemonset/nvidia-dcgm-exporter FAIL 'is neither deployment/<name> nor statefulset/<name>'
for s in ondelete-ready ondelete-old-revision rollingupdate-ready nostrategy-ready; do
	grep -q "get statefulset/vmalertmanager-kubeneuron -o json <" "$work/sts.$s.kubectl" ||
		bad "sts wait: $s was accepted without reading the StatefulSet status"
done
# No harness scenario, passing or failing, ever reached rollout status on a
# StatefulSet: every run_sts scenario (run_sts checks each as it goes) and
# every deploy-observability run above, the green path included. The fake's
# self-test log is not in this set: it alone holds a rollout status call, the
# one it was made to refuse.
[ "${#sts_scenarios[@]}" -ge 14 ] || bad "sts wait: only ${#sts_scenarios[@]} scenarios ran; the scenario-wide assertion would cover too little"
for s in "${sts_scenarios[@]}"; do
	[ -s "$work/sts.$s.kubectl" ] || bad "sts wait: scenario $s left no kubectl log; the scenario-wide assertion has nothing to check for it"
	! grep -q 'rollout status statefulset/' "$work/sts.$s.kubectl" ||
		bad "sts wait: scenario $s ran rollout status against a StatefulSet"
done
obs_logs=("$work"/obs.*.kubectl)
if [ "${#obs_logs[@]}" -lt 5 ] || [ ! -e "${obs_logs[0]}" ]; then
	bad "sts wait: expected the deploy-observability kubectl logs (lock-drift, checksum, no-root, port-drift, green), found ${#obs_logs[@]}"
fi
! cat "${obs_logs[@]}" | grep -q 'rollout status statefulset/' ||
	bad "sts wait: a deploy-observability run ran rollout status against a StatefulSet"

# =============================================================================
# the workflow's kubectl: no version skew against EKS, checksum-locked
# =============================================================================
note "static: the workflow's kubectl pin matches the EKS control-plane minor and is checksum-locked"
# wf_env <NAME> — the value of an `env:` entry in the tool-install step.
wf_env() {
	sed -nE "s/^[[:space:]]+$1:[[:space:]]*([^[:space:]#]+)[[:space:]]*$/\1/p" "$workflow" | head -n1
}
kubectl_version=$(wf_env KUBECTL_VERSION)
kubectl_sha=$(wf_env KUBECTL_SHA256)
k8s_version=$(sed -nE 's/^K8S_VERSION="\$\{K8S_VERSION:-([0-9]+\.[0-9]+)\}"$/\1/p' "$script" | head -n1)
[ -n "$k8s_version" ] || bad "script has no K8S_VERSION default to compare the kubectl pin against"
printf '%s' "$kubectl_version" | grep -Eq '^v[0-9]+\.[0-9]+\.[0-9]+$' ||
	bad "workflow KUBECTL_VERSION is not an exact vMAJOR.MINOR.PATCH pin: '${kubectl_version:-missing}'"
kubectl_minor=$(printf '%s' "$kubectl_version" | sed -E 's/^v([0-9]+\.[0-9]+)\..*$/\1/')
[ "$kubectl_minor" = "$k8s_version" ] ||
	bad "workflow kubectl ${kubectl_version:-missing} (minor ${kubectl_minor:-?}) does not match the EKS control plane K8S_VERSION ${k8s_version}; kubectl must stay within the supported skew and the harness is reviewed against this minor"
# The reviewed pin. The digest is the one published at
# dl.k8s.io/release/v1.33.12/bin/linux/amd64/kubectl.sha256, compared against
# the downloaded binary at review time (2026-10-17). Moving kubectl means
# changing this pair here and in the workflow in the same commit; a workflow
# that names the version with ANY other digest, well-formed or not, fails.
reviewed_kubectl_version=v1.33.12
reviewed_kubectl_sha=fe80ae4133b44fa2077db4af144e80765eb1b3b2eede55fbff6933c4374d8c6e
# kubectl_pin_ok <version> <sha256> — true only for the reviewed pair.
kubectl_pin_ok() {
	printf '%s' "$2" | grep -Eq '^[0-9a-f]{64}$' || return 1
	[ "$1" = "$reviewed_kubectl_version" ] || return 1
	[ "$2" = "$reviewed_kubectl_sha" ] || return 1
}
# Self-test of the comparison before it is trusted: the reviewed pair passes,
# a different well-formed digest fails, the digest under another version
# fails, and a malformed or missing digest fails. A comparison that cannot
# be made to fail proves nothing about the workflow.
kubectl_pin_ok "$reviewed_kubectl_version" "$reviewed_kubectl_sha" ||
	bad "kubectl pin self-test: the reviewed pair is rejected"
! kubectl_pin_ok "$reviewed_kubectl_version" "00000000000000000000000000000000000000000000000000000000deadbeef" ||
	bad "kubectl pin self-test: a different well-formed digest is accepted"
! kubectl_pin_ok "$reviewed_kubectl_version" "$(printf '%s' "$reviewed_kubectl_sha" | tr '0123456789abcdef' '123456789abcdef0')" ||
	bad "kubectl pin self-test: a one-rotation-away digest is accepted"
! kubectl_pin_ok v1.33.13 "$reviewed_kubectl_sha" ||
	bad "kubectl pin self-test: the reviewed digest under another version is accepted"
! kubectl_pin_ok "$reviewed_kubectl_version" "fe80ae41" ||
	bad "kubectl pin self-test: a truncated digest is accepted"
! kubectl_pin_ok "$reviewed_kubectl_version" "" ||
	bad "kubectl pin self-test: an empty digest is accepted"
# Now the workflow itself.
printf '%s' "$kubectl_sha" | grep -Eq '^[0-9a-f]{64}$' ||
	bad "workflow KUBECTL_SHA256 is missing or not a SHA-256 hex digest: '${kubectl_sha:-missing}'"
[ "$kubectl_version" = "$reviewed_kubectl_version" ] ||
	bad "workflow KUBECTL_VERSION '${kubectl_version:-missing}' is not the reviewed ${reviewed_kubectl_version}; re-review the pin (version AND digest) and update both here and in the workflow in one commit"
[ "$kubectl_sha" = "$reviewed_kubectl_sha" ] ||
	bad "workflow KUBECTL_SHA256 '${kubectl_sha:-missing}' is not the reviewed digest ${reviewed_kubectl_sha} for ${reviewed_kubectl_version}; a well-formed digest that differs is a different binary, not a typo to wave through"
kubectl_pin_ok "$kubectl_version" "$kubectl_sha" ||
	bad "workflow kubectl pin (${kubectl_version:-missing}, ${kubectl_sha:-missing}) is not the reviewed pair"
# The exact digest must also be what the step compares against: the env
# value, not a second copy that could drift from it.
grep -Eq "^[[:space:]]+KUBECTL_SHA256:[[:space:]]*${reviewed_kubectl_sha}[[:space:]]*$" "$workflow" ||
	bad "workflow does not carry the reviewed digest as KUBECTL_SHA256"
# The install step's script, extracted without a YAML parser: the `run: |`
# block after the step name, i.e. every following line indented ten spaces.
install_step=$(awk '
	/^      - name: Install eksctl and kubectl$/ { s = 1; next }
	s && /^        run: \|$/ { r = 1; next }
	r && /^          / { sub(/^          /, ""); print; next }
	r { exit }
' "$workflow")
[ -n "$install_step" ] || bad "cannot extract the run block of the 'Install eksctl and kubectl' step"
step_line() { printf '%s\n' "$install_step" | grep -nF -- "$1" | head -n1 | cut -d: -f1 || true; }
dl_line=$(step_line '--output "$tools/kubectl.download"')
url_line=$(step_line 'https://dl.k8s.io/release/${KUBECTL_VERSION}/bin/linux/amd64/kubectl')
sum_line=$(step_line '"$KUBECTL_SHA256" "$tools/kubectl.download" | sha256sum -c --status -')
rm_line=$(step_line 'rm -f "$tools/kubectl.download"')
exit_line=$(step_line 'exit 1')
mv_line=$(step_line 'mv "$tools/kubectl.download" "$tools/kubectl"')
chmod_line=$(step_line 'chmod 0755 "$tools/eksctl" "$tools/kubectl"')
path_line=$(step_line '>> "$GITHUB_PATH"')
[ -n "$dl_line" ] || bad "install step does not download kubectl to a .download file"
[ -n "$url_line" ] || bad "install step does not download kubectl from dl.k8s.io at \${KUBECTL_VERSION}"
[ -n "$sum_line" ] || bad "install step does not verify the kubectl download with sha256sum -c against \$KUBECTL_SHA256"
[ -n "$rm_line" ] || bad "install step does not delete a kubectl download that fails verification"
[ -n "$exit_line" ] || bad "install step does not exit non-zero on a kubectl checksum mismatch"
[ -n "$mv_line" ] || bad "install step does not move the verified download into place"
[ -n "$chmod_line" ] || bad "install step does not chmod kubectl"
[ -n "$path_line" ] || bad "install step does not export the tools dir to GITHUB_PATH"
if [ -n "$dl_line" ] && [ -n "$sum_line" ] && [ -n "$rm_line" ] && [ -n "$exit_line" ] && [ -n "$mv_line" ] && [ -n "$chmod_line" ] && [ -n "$path_line" ]; then
	if ! { [ "$dl_line" -lt "$sum_line" ] && [ "$sum_line" -lt "$rm_line" ] && [ "$rm_line" -lt "$exit_line" ] &&
		[ "$exit_line" -lt "$mv_line" ] && [ "$mv_line" -lt "$chmod_line" ] && [ "$chmod_line" -lt "$path_line" ]; }; then
		bad "install step order must be download -> sha256sum -c -> rm+exit on mismatch -> mv -> chmod -> GITHUB_PATH (got $dl_line/$sum_line/$rm_line/$exit_line/$mv_line/$chmod_line/$path_line)"
	fi
fi
printf '%s\n' "$install_step" | head -n1 | grep -q 'set -euo pipefail' || bad "install step does not start with set -euo pipefail"
! printf '%s\n' "$install_step" | grep -E '^chmod .*\$tools/kubectl\.download' >/dev/null ||
	bad "install step makes the unverified download executable"

# --- dynamic: the install step against a scripted curl ---------------------
# The eksctl half needs a real tarball on stdout; the kubectl half writes
# FAKE_KUBECTL_CONTENT to --output. RUNNER_TEMP and GITHUB_PATH are the
# runner's contract and are pointed into the work dir.
wfbin="$work/wfbin"
mkdir -p "$wfbin" "$work/eksctl-src"
printf '%s\n' '#!/usr/bin/env bash' 'echo fake-eksctl' >"$work/eksctl-src/eksctl"
tar -czf "$work/eksctl.tgz" -C "$work/eksctl-src" eksctl
cat >"$wfbin/curl" <<'EOF'
#!/usr/bin/env bash
printf 'curl %s\n' "$*" >>"$FAKE_CURL_LOG"
out="" url=""
while [ $# -gt 0 ]; do
	case "$1" in
	--output) out="$2"; shift ;;
	http://*|https://*) url="$1" ;;
	esac
	shift
done
case "$url" in
*eksctl*) cat "$FAKE_EKSCTL_TGZ" ;;
*dl.k8s.io/release/*/bin/linux/amd64/kubectl) printf '%s\n' "${FAKE_KUBECTL_CONTENT:-not kubectl}" >"$out" ;;
*) exit 22 ;;
esac
EOF
chmod +x "$wfbin/curl"
# run_install <label> <expected-rc> <extra-PATH-prefix-or-"">
run_install() {
	local label="$1" want_rc="$2" prefix="$3"
	local dir="$work/wf.$label" path="$wfbin:$PATH"
	[ -z "$prefix" ] || path="$prefix:$path"
	mkdir -p "$dir/runner-temp"
	: >"$dir/github-path"
	: >"$dir/curl.log"
	printf '%s\n' "$install_step" >"$dir/step.sh"
	set +e
	env PATH="$path" FAKE_CURL_LOG="$dir/curl.log" FAKE_EKSCTL_TGZ="$work/eksctl.tgz" \
		RUNNER_TEMP="$dir/runner-temp" GITHUB_PATH="$dir/github-path" \
		EKSCTL_VERSION="$(wf_env EKSCTL_VERSION)" KUBECTL_VERSION="$kubectl_version" KUBECTL_SHA256="$kubectl_sha" \
		timeout 60 bash "$dir/step.sh" >"$dir/out" 2>"$dir/err"
	local rc=$?
	set -e
	[ "$rc" -eq "$want_rc" ] || bad "$label: install step exited $rc, expected $want_rc: $(tr '\n' ' ' <"$dir/err")"
}

note "dynamic: a kubectl download that does not match the pin is refused and nothing is exported (real sha256sum)"
run_install mismatch 1 ""
dir="$work/wf.mismatch"
grep -q 'does not match the pinned SHA-256' "$dir/err" || bad "mismatch: stderr lacks the checksum refusal: $(tr '\n' ' ' <"$dir/err")"
grep -q "curl .*https://dl.k8s.io/release/${kubectl_version}/bin/linux/amd64/kubectl" "$dir/curl.log" ||
	bad "mismatch: kubectl was not fetched at the pinned version: $(cat "$dir/curl.log")"
[ ! -e "$dir/runner-temp/hw-e2e-tools/kubectl" ] || bad "mismatch: an unverified kubectl was put in place"
[ ! -e "$dir/runner-temp/hw-e2e-tools/kubectl.download" ] || bad "mismatch: the unverified download was kept"
! grep -q . "$dir/github-path" || bad "mismatch: the tools dir was exported to GITHUB_PATH after a refused download"

note "dynamic: a verified kubectl reaches PATH, executable, with the download name gone"
run_install verified 0 "$okhash"
dir="$work/wf.verified"
[ -x "$dir/runner-temp/hw-e2e-tools/kubectl" ] || bad "verified: kubectl is not in place and executable"
[ -x "$dir/runner-temp/hw-e2e-tools/eksctl" ] || bad "verified: eksctl is not in place and executable"
[ ! -e "$dir/runner-temp/hw-e2e-tools/kubectl.download" ] || bad "verified: the .download name was left behind"
[ "$(cat "$dir/github-path")" = "$dir/runner-temp/hw-e2e-tools" ] || bad "verified: GITHUB_PATH does not carry exactly the tools dir: $(cat "$dir/github-path")"

if [ "$fail" -ne 0 ]; then
	echo "verify-hw-e2e-observability: FAILED" >&2
	exit 1
fi
echo
echo "verify-hw-e2e-observability: OK (profile pin $pin, --values wiring, test-observability dispatch, label contract, EKS node_pool, T4 absence checks, Service-resolved VMSingle port, reaped cleanup, bounded checks, fail-closed gate; deploy-observability: workflow order, VM operator pin $vm_version = lock, no remote apply, README order with bounded gates, fail-closed token copy, VMSingle volume tied to the run once Bound and no namespace-scoped sweep, refused lock drift / checksum mismatch / missing root / port drift, green path in order incl. the VMSingle volume tagged with the run identity and recorded, StatefulSet wait on status with the strategy's own revision check (OnDelete accepts Ready old-revision pods, RollingUpdate and the defaulted strategy do not, an unsupported strategy never passes), rollout status refused (self-tested fake), four ready and seven fail-closed fixtures; workflow kubectl $kubectl_version = EKS $k8s_version minor, exact reviewed digest ${reviewed_kubectl_sha:0:12}… (self-tested against a different digest), refused mismatch, verified install on PATH)"
