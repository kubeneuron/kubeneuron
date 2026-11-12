#!/usr/bin/env bash
# SPDX-License-Identifier: LicenseRef-KubeNeuron

# shellcheck disable=SC2016 # Single-quoted jq programs expand jq, not shell, variables.
set -Eeuo pipefail

# The v0.8 examples fixture: examples/checkpoint/{plain-job,kueue-job,
# kuberay-ray-train} driven end to end through a real KubeNeuron installation,
# CPU-only. It proves that the three toys read the advisory and the request
# off their own Pods, checkpoint and acknowledge (by leaving, or for the Ray
# workers by writing the state key), that the impact preview names each Pod's
# owner, Kueue queue and Ray cluster, that the approve is bound to the preview
# digest, and that no Kueue or Ray object gained a KubeNeuron field manager.
#
# It is not evidence that Kueue, KubeRay or Ray Train integrate with
# KubeNeuron, and no hardware claim: a kind node without GPUs previews as
# topology unknown, device actions refused. That is the point of the toys.

usage() {
	cat <<'USAGE'
Usage: hack/kind-workload-safe.sh [--verify]

Runs the three examples under examples/checkpoint/ against an EXISTING
cluster where KubeNeuron is installed and its controller API reachable
(the cluster `KEEP_CLUSTER=1 make test-integration-kind` leaves behind fits
the defaults), installs the Kueue release manifest and the KubeRay operator
chart pinned in deploy/kubernetes/dependencies/versions.lock.yaml when their
CRDs are missing, and asserts, on one worker node:

  Phase A (parked for approval):
  * every opted-in example Pod carries the advisory for the incident, round
    1, naming platform.drain, and no request deadline; no other Pod does
  * the plain-job and kueue-job sidecars log the advisory and their trainers
    checkpoint; the Ray driver logs the workers' advisory checkpoints
  * the impact preview is a computed round-1 document parked at cordon with
    exactly the opted-in Pods eligible, one group per example with all its
    members on the node, owner_chain Job/<name> on the Job Pods,
    context.kueue {team-a, cpu-queue, workload} on the Kueue Pods and
    context.ray {worker, trainers, ray-train-checkpoint} on the Ray workers
  * approve without the digest is refused (409)

  Phase B (approved, drained):
  * approve bound to the digest is accepted; every opted-in Pod receives the
    request with a deadline and platform.drain
  * the Job Pods checkpoint and leave (exit 75, so the Job replaces the
    index); the Ray workers checkpoint and write checkpoint-state
  * the incident reaches VERIFYING/RESOLVED; the audit records every Job Pod
    as exited and every Ray worker as acknowledged; the node was cordoned
  * no Kueue Workload, RayJob or RayCluster carries a KubeNeuron field
    manager: the controller read them and wrote nothing

  Phase C (resume; plain-job only): the plain-job-pvc overlay runs one index with
  its checkpoint on a PersistentVolumeClaim, a request stamped on its Pod
  makes the trainer checkpoint and leave, and the replacement Pod the Job
  creates logs "resuming from step N" with the step the first Pod wrote

  --verify     offline checks only: bash -n and shellcheck of this script
               and the example scripts, manifest rendering and shape, lock
               pins, README disclaimers, the trainer and sidecar run locally
               through an advisory and a request, and every preview,
               annotation and audit assertion shown to FAIL on deliberate
               defects; no cluster, no docker, no network

Configuration is through environment variables:
  KUBECONFIG_PATH      kubeconfig of the target cluster
                       (default: /tmp/kubeneuron-integration.kubeconfig)
  ROOT_NAME            KubeNeuron root (default: integration-smoke)
  ROOT_NAMESPACE       its namespace (default: kubeneuron-integration-smoke)
  OPERATOR_TOKEN       controller API bearer token; the default reads the
                       Secret the root's notifications.operatorAPIToken names
  EXAMPLES             space-separated subset of
                       "plain-job kueue-job kuberay-ray-train" (default: all)
  INSTALL_DEPENDENCIES 0 to fail instead of installing missing Kueue/KubeRay
  REMOVE_DEPENDENCIES  1 to uninstall Kueue/KubeRay this run installed
  KEEP_RESOURCES       1 to retain the examples, playbook and policy on exit
  TIMEOUT_SECONDS      per-wait deadline in seconds (default: 420)
  KUBECTL_BIN, HELM_BIN, JQ_BIN, PYTHON_BIN, CURL_BIN
                       command paths (each must be one executable)

The root's spec.safety is saved before the run and restored exactly on exit.
USAGE
}

MODE=run
case "${1:-}" in
-h | --help)
	usage
	exit 0
	;;
--verify)
	MODE=verify
	shift
	;;
esac
[[ $# -eq 0 ]] || {
	usage >&2
	exit 2
}

SCRIPT_DIR=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)
REPO_ROOT=$(cd -- "$SCRIPT_DIR/.." && pwd -P)
readonly EXAMPLES_DIR="$REPO_ROOT/examples/checkpoint"
readonly LOCK_FILE="$REPO_ROOT/deploy/kubernetes/dependencies/versions.lock.yaml"
readonly DISCLAIMER='This example demonstrates the annotation protocol; it is not evidence that any framework integrates with KubeNeuron.'
readonly EXAMPLE_NAMESPACE=ml-training
readonly PLAYBOOK_NAME=workload-safe-example
readonly POLICY_NAME=workload-safe-example
readonly INCIDENT_CLASS=workload-safe-example
readonly NODE_LABEL=kubeneuron.io/workload-safe-example
readonly controllerPort=8080
readonly -a ALL_EXAMPLES=(plain-job kueue-job kuberay-ray-train)

KUBECTL_BIN=${KUBECTL_BIN:-kubectl}
HELM_BIN=${HELM_BIN:-helm}
JQ_BIN=${JQ_BIN:-jq}
PYTHON_BIN=${PYTHON_BIN:-python3}
CURL_BIN=${CURL_BIN:-curl}
KUBECONFIG_PATH=${KUBECONFIG_PATH:-/tmp/kubeneuron-integration.kubeconfig}
ROOT_NAME=${ROOT_NAME:-integration-smoke}
ROOT_NAMESPACE=${ROOT_NAMESPACE:-kubeneuron-integration-smoke}
OPERATOR_TOKEN=${OPERATOR_TOKEN:-}
EXAMPLES=${EXAMPLES:-${ALL_EXAMPLES[*]}}
INSTALL_DEPENDENCIES=${INSTALL_DEPENDENCIES:-1}
REMOVE_DEPENDENCIES=${REMOVE_DEPENDENCIES:-0}
KEEP_RESOURCES=${KEEP_RESOURCES:-0}
TIMEOUT_SECONDS=${TIMEOUT_SECONDS:-420}
RUN_ID=${RUN_ID:-$(date -u +%Y%m%d%H%M%S)-$$}

failures=0
verify_tmp=
note() { printf '==> %s\n' "$*"; }
fail() {
	printf 'ERROR: %s\n' "$*" >&2
	exit 1
}
check() {
	# check <description> <ok:0|1>
	if [[ $2 -eq 0 ]]; then
		printf 'ok   %s\n' "$1"
	else
		printf 'FAIL %s\n' "$1" >&2
		failures=$((failures + 1))
	fi
}
assert_true() {
	# assert_true <description> <command...>: records the status instead of
	# letting errexit end the run, so every assertion is reported.
	local desc=$1
	shift
	if "$@"; then check "$desc" 0; else check "$desc" 1; fi
}
test_fails() { ! "$@"; }
require_cmd() {
	local var=$1 value=$2
	[[ $(printf '%s' "$value" | wc -w) -eq 1 ]] || fail "$var must be a single executable path, got '$value'"
	command -v -- "$value" >/dev/null 2>&1 || fail "$var ($value) is not executable"
}
has_example() {
	local e
	for e in $EXAMPLES; do [[ $e == "$1" ]] && return 0; done
	return 1
}
for e in $EXAMPLES; do
	case $e in
	plain-job | kueue-job | kuberay-ray-train) ;;
	*) fail "EXAMPLES names unknown example '$e'" ;;
	esac
done

# ---------------------------------------------------------------------------
# Lock pins. Read once, by the same parser --verify uses, so the run and the
# verify cannot disagree about what is pinned.
read_pins() {
	"$PYTHON_BIN" -I - "$LOCK_FILE" <<'PY'
import re, sys, yaml
lock = yaml.safe_load(open(sys.argv[1], encoding="utf-8"))
a, i = lock["artifacts"], lock["images"]
out = {}
for key, name in (("kueueManifest", "KUEUE"), ("kuberayOperatorChart", "KUBERAY")):
    e = a[key]
    if not re.fullmatch(r"[0-9a-f]{64}", e["sha256"]):
        sys.exit(f"lock: artifacts.{key}.sha256 must be 64 hex")
    if not e["url"].startswith("https://"):
        sys.exit(f"lock: artifacts.{key}.url must be https")
    if e["version"].lstrip("v") not in e["url"]:
        sys.exit(f"lock: artifacts.{key}.url does not carry version {e['version']}")
    out[name + "_VERSION"], out[name + "_URL"], out[name + "_SHA256"] = e["version"], e["url"], e["sha256"]
ray = i["rayCpu"]
if not re.fullmatch(r"sha256:[0-9a-f]{64}", ray["digest"]):
    sys.exit("lock: images.rayCpu.digest must be sha256:<64 hex>")
if not re.fullmatch(r"\d+\.\d+\.\d+-py\d+-cpu", ray["tag"]):
    sys.exit(f"lock: images.rayCpu.tag {ray['tag']!r} is not a release CPU tag")
out["RAY_IMAGE"] = f"{ray['repository']}:{ray['tag']}@{ray['digest']}"
out["RAY_VERSION"] = ray["tag"].split("-")[0]
kq = i["kueue"]
if kq["tag"] != a["kueueManifest"]["version"]:
    sys.exit("lock: images.kueue.tag must equal artifacts.kueueManifest.version")
for k, v in out.items():
    print(f"{k}={v}")
PY
}
KUEUE_VERSION='' KUEUE_URL='' KUEUE_SHA256='' KUBERAY_VERSION='' KUBERAY_URL='' KUBERAY_SHA256='' RAY_IMAGE='' RAY_VERSION=''
load_pins() {
	local line
	while IFS= read -r line; do
		case $line in
		KUEUE_VERSION=*) KUEUE_VERSION=${line#*=} ;;
		KUEUE_URL=*) KUEUE_URL=${line#*=} ;;
		KUEUE_SHA256=*) KUEUE_SHA256=${line#*=} ;;
		KUBERAY_VERSION=*) KUBERAY_VERSION=${line#*=} ;;
		KUBERAY_URL=*) KUBERAY_URL=${line#*=} ;;
		KUBERAY_SHA256=*) KUBERAY_SHA256=${line#*=} ;;
		RAY_IMAGE=*) RAY_IMAGE=${line#*=} ;;
		RAY_VERSION=*) RAY_VERSION=${line#*=} ;;
		esac
	done < <(read_pins)
	[[ -n $KUEUE_URL && -n $KUBERAY_URL && -n $RAY_IMAGE ]] || fail "versions.lock.yaml does not pin Kueue, KubeRay and the Ray image"
}

# ---------------------------------------------------------------------------
# Assertions over documents. Each takes files, never the cluster, so --verify
# can feed them fixtures with deliberate defects and prove they fail.

# assert_preview_shape <preview.json> <round> <pending_step>
assert_preview_shape() {
	"$JQ_BIN" -e --argjson round "$2" --arg step "$3" '
		.version == "impact-preview/v1" and .unavailable == null and .round == $round
		and .pending_step == $step and ((.downstream_disruptions // []) | index("platform.drain") != null)
		and ((.digest // "") | test("^sha256:[0-9a-f]{64}$"))
		and .node.topology.topology == "unknown" and .node.topology.device_actions == "refused"' "$1" >/dev/null 2>&1
}
# assert_preview_eligible <preview.json> <count>
assert_preview_eligible() {
	"$JQ_BIN" -e --argjson n "$2" '.workloads.eligible_for_checkpoint == $n and .limits.items_truncated == false' "$1" >/dev/null 2>&1
}
# assert_preview_group <preview.json> <namespace> <group> <members_on_node> <members_total>
assert_preview_group() {
	"$JQ_BIN" -e --arg ns "$2" --arg name "$3" --argjson on "$4" --argjson total "$5" '
		[.workloads.groups[]? | select(.namespace == $ns and .name == $name)]
		| length == 1 and .[0].members_on_node == $on and .[0].members_total == $total and (.[0].too_large // false) == false' "$1" >/dev/null 2>&1
}
# assert_item_owner <preview.json> <namespace> <pod> <Kind/name>
assert_item_owner() {
	"$JQ_BIN" -e --arg ns "$2" --arg pod "$3" --arg owner "$4" '
		[.workloads.items[]? | select(.namespace == $ns and .name == $pod)]
		| length == 1 and ((.[0].owner_chain // []) | index($owner) != null)
		and .[0].checkpoint.opted_in == true and .[0].checkpoint.eligible == true' "$1" >/dev/null 2>&1
}
# assert_item_kueue <preview.json> <namespace> <pod> <local_queue> <cluster_queue>
assert_item_kueue() {
	"$JQ_BIN" -e --arg ns "$2" --arg pod "$3" --arg lq "$4" --arg cq "$5" '
		[.workloads.items[]? | select(.namespace == $ns and .name == $pod)]
		| length == 1 and .[0].context.kueue.local_queue == $lq and .[0].context.kueue.cluster_queue == $cq
		and ((.[0].context.kueue.workload // "") | length > 0)' "$1" >/dev/null 2>&1
}
# assert_item_ray <preview.json> <namespace> <pod> <ray_job> <group>
assert_item_ray() {
	"$JQ_BIN" -e --arg ns "$2" --arg pod "$3" --arg job "$4" --arg group "$5" '
		[.workloads.items[]? | select(.namespace == $ns and .name == $pod)]
		| length == 1 and .[0].context.ray.node_type == "worker" and .[0].context.ray.ray_job == $job
		and .[0].context.ray.group == $group and ((.[0].context.ray.cluster // "") | length > 0)' "$1" >/dev/null 2>&1
}
# assert_advisory_annotations <annotations.json> <incident> <round> <next_action>
assert_advisory_annotations() {
	"$JQ_BIN" -e --arg id "$2" --arg round "$3" --arg next "$4" '
		.["kubeneuron.io/checkpoint"] == "true"
		and .["kubeneuron.io/checkpoint-advisory"] == "true"
		and .["kubeneuron.io/checkpoint-advisory-incident"] == $id
		and .["kubeneuron.io/checkpoint-advisory-round"] == $round
		and .["kubeneuron.io/checkpoint-advisory-next-action"] == $next
		and has("kubeneuron.io/checkpoint-deadline-at") == false' "$1" >/dev/null 2>&1
}
# assert_request_annotations <annotations.json> <incident>
assert_request_annotations() {
	"$JQ_BIN" -e --arg id "$2" '
		.["kubeneuron.io/checkpoint-incident"] == $id
		and ((.["kubeneuron.io/checkpoint-deadline-at"] // "") | length > 0)
		and ((.["kubeneuron.io/checkpoint-requested-at"] // "") | length > 0)
		and .["kubeneuron.io/checkpoint-next-action"] == "platform.drain"' "$1" >/dev/null 2>&1
}
# assert_audit_outcome <audit.txt> <namespace> <pod> <outcome>: the completion
# row names the Pod with exactly this outcome (peer or not).
assert_audit_outcome() {
	# The controller names at most five workloads per row and counts the rest
	# as "(+N more)". A named Pod must carry the expected outcome; an unnamed
	# one is accepted only when the row says it was truncated, and the caller
	# reads that Pod's outcome off the Pod itself (exit, or the state key).
	local row
	row=$(grep -E "checkpoint: complete after " "$1") || return 1
	if grep -Eq "(^|[ ,;:])(peer )?$2/$3=" <<<"$row"; then
		grep -Eq "(^|[ ,;:])(peer )?$2/$3=$4([ ,;]|$)" <<<"$row"
	else
		grep -Eq '\(\+[0-9]+ more\)' <<<"$row"
	fi
}
# assert_no_kubeneuron_manager <object.json>: nothing KubeNeuron-named ever
# wrote a field of this object.
assert_no_kubeneuron_manager() {
	"$JQ_BIN" -e '[.metadata.managedFields[]? | (.manager // "") | ascii_downcase | select(test("kubeneuron"))] | length == 0' "$1" >/dev/null 2>&1
}

# ---------------------------------------------------------------------------
# --verify: everything that needs no cluster.
verify_offline() {
	require_cmd JQ_BIN "$JQ_BIN"
	require_cmd PYTHON_BIN "$PYTHON_BIN"
	require_cmd KUBECTL_BIN "$KUBECTL_BIN"
	# Global on purpose: the EXIT trap runs after this function's locals are gone.
	verify_tmp=$(mktemp -d "${TMPDIR:-/tmp}/kind-workload-safe-verify.XXXXXX")
	trap 'rm -rf -- "$verify_tmp"' EXIT
	local tmp=$verify_tmp

	note "static checks"
	assert_true "bash -n $(basename "${BASH_SOURCE[0]}")" bash -n "${BASH_SOURCE[0]}"
	if command -v shellcheck >/dev/null 2>&1; then
		assert_true "shellcheck $(basename "${BASH_SOURCE[0]}")" shellcheck "${BASH_SOURCE[0]}"
		assert_true "shellcheck -s sh scripts/{trainer,sidecar}.sh" shellcheck -s sh "$EXAMPLES_DIR/scripts/trainer.sh" "$EXAMPLES_DIR/scripts/sidecar.sh"
	else
		printf 'skip shellcheck (not installed)\n'
	fi

	note "lock pins"
	local pins_ok=0
	load_pins || pins_ok=1
	check "versions.lock.yaml pins Kueue $KUEUE_VERSION, KubeRay chart $KUBERAY_VERSION, Ray image $RAY_IMAGE" $pins_ok
	assert_true "rayjob.yaml runs exactly the pinned Ray image (head and workers)" \
		"$PYTHON_BIN" -I - "$EXAMPLES_DIR/kuberay-ray-train/rayjob.yaml" "$RAY_IMAGE" "$RAY_VERSION" <<'PY'
import sys, yaml
path, image, version = sys.argv[1:]
docs = [d for d in yaml.safe_load_all(open(path, encoding="utf-8")) if d]
rj = next(d for d in docs if d["kind"] == "RayJob")
spec = rj["spec"]["rayClusterSpec"]
assert spec["rayVersion"] == version, (spec["rayVersion"], version)
images = [c["image"] for c in spec["headGroupSpec"]["template"]["spec"]["containers"]]
for wg in spec["workerGroupSpecs"]:
    images += [c["image"] for c in wg["template"]["spec"]["containers"]]
assert images and all(i == image for i in images), images
PY

	note "example manifests"
	render_job() { "$KUBECTL_BIN" kustomize "$EXAMPLES_DIR/$1" >"$tmp/$1.yaml"; }
	assert_true "kubectl kustomize plain-job renders" render_job plain-job
	assert_true "kubectl kustomize plain-job-pvc renders" render_job plain-job-pvc
	pvc_shape() {
		"$PYTHON_BIN" -I - "$tmp/plain-job-pvc.yaml" <<'PY'
import sys, yaml
docs = [d for d in yaml.safe_load_all(open(sys.argv[1])) if d]
job = next(d for d in docs if d["kind"] == "Job")
claims = [d for d in docs if d["kind"] == "PersistentVolumeClaim"]
assert job["spec"]["completions"] == 1 and job["spec"]["parallelism"] == 1, job["spec"]
assert job["spec"]["maxFailedIndexes"] <= job["spec"]["completions"], job["spec"]
vols = {v["name"]: v for v in job["spec"]["template"]["spec"]["volumes"]}
assert "emptyDir" not in vols["ckpt"] and vols["ckpt"]["persistentVolumeClaim"]["claimName"] == "plain-job-ckpt", vols["ckpt"]
assert len(claims) == 1 and claims[0]["metadata"]["name"] == "plain-job-ckpt", claims
PY
	}
	assert_true "plain-job-pvc overlay: one index, the checkpoint on the claim, no emptyDir" pvc_shape
	assert_true "kubectl kustomize kueue-job renders" render_job kueue-job
	job_shape() {
		"$PYTHON_BIN" -I - "$tmp/$1.yaml" "$2" "$3" <<'PY'
import sys, yaml
path, name, kueue = sys.argv[1], sys.argv[2], sys.argv[3] == "1"
docs = [d for d in yaml.safe_load_all(open(path, encoding="utf-8")) if d]
job = next(d for d in docs if d["kind"] == "Job" and d["metadata"]["name"] == name)
cm = next(d for d in docs if d["kind"] == "ConfigMap" and d["metadata"]["name"] == "checkpoint-scripts")
assert set(cm["data"]) == {"trainer.sh", "sidecar.sh"}, sorted(cm["data"])
assert job["metadata"]["namespace"] == "ml-training"
assert job["spec"]["completionMode"] == "Indexed" and job["spec"]["completions"] == 2
assert "backoffLimitPerIndex" in job["spec"], "a Pod that leaves on the request must be replaced per index"
tpl = job["spec"]["template"]
ann = tpl["metadata"]["annotations"]
assert ann["kubeneuron.io/checkpoint"] == "true", ann
assert ann["kubeneuron.io/checkpoint-group"] == name, ann
assert ann["kubeneuron.io/checkpoint-max-wait"] == "3m", ann
assert tpl["spec"]["restartPolicy"] == "Never", "exit 75 must fail the Pod, not restart the container in place"
assert tpl["spec"]["shareProcessNamespace"] is True, "the sidecar signals the trainer"
vols = {v["name"]: v for v in tpl["spec"]["volumes"]}
assert vols["podinfo"]["downwardAPI"]["items"][0]["fieldRef"]["fieldPath"] == "metadata.annotations"
names = {c["name"]: c for c in tpl["spec"]["containers"]}
assert set(names) == {"trainer", "checkpoint-sidecar"}, sorted(names)
assert {m["name"] for m in names["checkpoint-sidecar"]["volumeMounts"]} >= {"podinfo", "ckpt", "scripts"}
for c in names.values():
    assert c["image"] == "busybox:1.37", c["image"]
if kueue:
    assert job["metadata"]["labels"]["kueue.x-k8s.io/queue-name"] == "team-a"
    assert job["spec"]["suspend"] is True, "Kueue admits a suspended Job"
    kinds = {d["kind"] for d in docs}
    assert {"ResourceFlavor", "ClusterQueue", "LocalQueue"} <= kinds, kinds
    lq = next(d for d in docs if d["kind"] == "LocalQueue")
    assert lq["metadata"]["name"] == "team-a" and lq["spec"]["clusterQueue"] == "cpu-queue"
else:
    assert "kueue.x-k8s.io/queue-name" not in (job["metadata"].get("labels") or {})
PY
	}
	assert_true "plain-job Job: Indexed, opted in, group = job name, downward API, sidecar + trainer, Never + per-index backoff" job_shape plain-job plain-job-run 0
	assert_true "kueue-job Job: the same shape, suspended, in LocalQueue team-a -> cpu-queue" job_shape kueue-job kueue-job-run 1
	ray_shape() {
		"$PYTHON_BIN" -I - "$EXAMPLES_DIR/kuberay-ray-train/rayjob.yaml" <<'PY'
import sys, yaml
docs = [d for d in yaml.safe_load_all(open(sys.argv[1], encoding="utf-8")) if d]
kinds = {d["kind"] for d in docs}
assert {"RayJob", "ConfigMap", "ServiceAccount", "Role", "RoleBinding"} <= kinds, kinds
role = next(d for d in docs if d["kind"] == "Role")
assert role["rules"] == [{"apiGroups": [""], "resources": ["pods"], "verbs": ["get", "patch"]}], "the workers' Role is get+patch pods and nothing else"
rj = next(d for d in docs if d["kind"] == "RayJob")
assert rj["metadata"]["name"] == "ray-train-checkpoint" and rj["metadata"]["namespace"] == "ml-training"
import ast
scripts = [d for d in docs if d.get("kind") == "ConfigMap" and d["metadata"]["name"] == "ray-train-checkpoint-example"]
assert len(scripts) == 1 and "train.py" in scripts[0]["data"], "the Ray example ships train.py in its ConfigMap"
ast.parse(scripts[0]["data"]["train.py"])  # a syntax error here would only surface on the cluster
spec = rj["spec"]["rayClusterSpec"]
head = spec["headGroupSpec"]["template"]
assert "kubeneuron.io/checkpoint" not in (head.get("metadata", {}).get("annotations") or {}), "the head is not opted in"
(wg,) = spec["workerGroupSpecs"]
assert wg["groupName"] == "trainers" and wg["replicas"] == 2
ann = wg["template"]["metadata"]["annotations"]
assert ann["kubeneuron.io/checkpoint"] == "true" and ann["kubeneuron.io/checkpoint-group"] == "ray-train-checkpoint", ann
assert wg["template"]["spec"]["serviceAccountName"] == "ray-train-checkpoint"
vols = {v["name"]: v for v in wg["template"]["spec"]["volumes"]}
assert vols["podinfo"]["downwardAPI"]["items"][0]["fieldRef"]["fieldPath"] == "metadata.annotations"
cm = next(d for d in docs if d["kind"] == "ConfigMap")
src = cm["data"]["train.py"]
for needle in ("kubeneuron.io/checkpoint-advisory-incident", "kubeneuron.io/checkpoint-incident",
               "kubeneuron.io/checkpoint-deadline-at", "kubeneuron.io/checkpoint-state", "complete:",
               "application/merge-patch+json", "FailureConfig", "Checkpoint.from_directory"):
    assert needle in src, needle
assert "use_gpu=False" in src
PY
	}
	assert_true "rayjob.yaml: workers opted in as group ray-train-checkpoint, head not, get+patch Role, train.py reads advisory/request and acknowledges" ray_shape
	local d ok=0
	for d in "$EXAMPLES_DIR"/plain-job "$EXAMPLES_DIR"/kueue-job "$EXAMPLES_DIR"/kuberay-ray-train "$EXAMPLES_DIR"; do
		tr -s '\n>' '  ' <"$d/README.md" | grep -qF -- "$DISCLAIMER" || { ok=1; printf 'missing disclaimer: %s\n' "$d/README.md" >&2; }
	done
	check "every examples README carries the disclaimer sentence" $ok
	ok=0
	grep -rEq 'image: [^@]*:(latest|main|master)\b' "$EXAMPLES_DIR" && ok=1
	check "no example names a mutable image tag" $ok

	note "trainer and sidecar, locally, through an advisory and a request"
	local work="$tmp/local" tpid spid deadline
	mkdir -p "$work/ckpt"
	: >"$work/annotations"
	cp "$EXAMPLES_DIR/scripts/trainer.sh" "$work/trainer.sh"
	cp "$EXAMPLES_DIR/scripts/sidecar.sh" "$work/sidecar.sh"
	CKPT_DIR="$work/ckpt" STEPS=600 CHECKPOINT_EVERY=1000 sh "$work/trainer.sh" >"$work/trainer.log" 2>&1 &
	tpid=$!
	ANNOTATIONS="$work/annotations" CKPT_DIR="$work/ckpt" POLL=1 TRAINER_SCRIPT="$work/trainer.sh" \
		sh "$work/sidecar.sh" >"$work/sidecar.log" 2>&1 &
	spid=$!
	local_cleanup() { kill "$tpid" "$spid" >/dev/null 2>&1 || true; }
	wait_log() {
		# wait_log <file> <pattern> <seconds>
		local end=$((SECONDS + $3))
		while ((SECONDS < end)); do
			grep -q -- "$2" "$1" 2>/dev/null && return 0
			sleep 1
		done
		return 1
	}
	sleep 2
	[[ ! -f "$work/ckpt/step" ]] || { check "the trainer checkpoints only when told (no checkpoint before the advisory)" 1; }
	printf '%s\n' 'kubeneuron.io/checkpoint="true"' 'kubeneuron.io/checkpoint-advisory="true"' \
		'kubeneuron.io/checkpoint-advisory-incident="inc-local-1"' 'kubeneuron.io/checkpoint-advisory-round="1"' \
		'kubeneuron.io/checkpoint-advisory-next-action="platform.drain"' >"$work/annotations"
	assert_true "sidecar logs the advisory (incident, round) and signals the trainer" wait_log "$work/sidecar.log" 'sidecar: advisory for incident inc-local-1 round 1' 10
	assert_true "trainer checkpoints on the advisory (not a periodic one) and keeps running" wait_log "$work/trainer.log" 'trainer: checkpoint at step [0-9]* (advisory)' 10
	assert_true "trainer is still alive after the advisory" kill -0 "$tpid"
	# A request without a deadline is not a request: the sidecar must ignore it.
	printf '%s\n' 'kubeneuron.io/checkpoint="true"' 'kubeneuron.io/checkpoint-incident="inc-local-1"' >"$work/annotations"
	sleep 3
	assert_true "sidecar ignores a checkpoint-incident key without a deadline" test_fails grep -q 'sidecar: request for' "$work/sidecar.log"
	assert_true "trainer is still alive after the deadline-less key" kill -0 "$tpid"
	printf '%s\n' 'kubeneuron.io/checkpoint="true"' 'kubeneuron.io/checkpoint-incident="inc-local-1"' \
		'kubeneuron.io/checkpoint-deadline-at="2026-01-01T00:03:00Z"' 'kubeneuron.io/checkpoint-next-action="platform.drain"' >"$work/annotations"
	assert_true "sidecar logs the request and signals the trainer" wait_log "$work/sidecar.log" 'sidecar: request for incident inc-local-1' 10
	local trc=0 src=0
	deadline=$((SECONDS + 15))
	while ((SECONDS < deadline)) && kill -0 "$tpid" 2>/dev/null; do sleep 1; done
	wait "$tpid" || trc=$?
	assert_true "trainer exits 75 after the requested checkpoint (got $trc)" test "$trc" -eq 75
	assert_true "trainer wrote checkpoint.done and its step" test -f "$work/ckpt/checkpoint.done" -a -s "$work/ckpt/step"
	deadline=$((SECONDS + 15))
	while ((SECONDS < deadline)) && kill -0 "$spid" 2>/dev/null; do sleep 1; done
	wait "$spid" || src=$?
	assert_true "sidecar exits 0 once the checkpoint is written (got $src)" test "$src" -eq 0
	local_cleanup

	note "assertion self-check: fixtures with deliberate defects must FAIL"
	local good="$tmp/preview.json" adv="$tmp/adv.json" req="$tmp/req.json" audit="$tmp/audit.txt" obj="$tmp/obj.json"
	cat >"$good" <<'JSON'
{"version":"impact-preview/v1","incident":"inc-1","round":1,"digest":"sha256:0000000000000000000000000000000000000000000000000000000000000000",
 "node":{"name":"worker","topology":{"topology":"unknown","device_actions":"refused"}},
 "pending_step":"cordon","downstream_disruptions":["platform.drain"],
 "workloads":{"total":7,"gpu_holders":0,"namespaces":1,"eligible_for_checkpoint":6,"ineligible":{"not-opted-in":1},
  "groups":[{"name":"plain-job-run","namespace":"ml-training","members_on_node":2,"members_total":2,"cross_node_requested":true},
            {"name":"kueue-job-run","namespace":"ml-training","members_on_node":2,"members_total":2,"cross_node_requested":true},
            {"name":"ray-train-checkpoint","namespace":"ml-training","members_on_node":2,"members_total":2,"cross_node_requested":true}],
  "items":[
   {"namespace":"ml-training","name":"plain-job-run-0-abc","kind":"Pod","uid":"u1","uses_gpu":false,"owner_chain":["Job/plain-job-run"],
    "context":{"kueue":{"local_queue":"","workload":"","active":false}},"checkpoint":{"opted_in":true,"eligible":true,"group":"plain-job-run"}},
   {"namespace":"ml-training","name":"kueue-job-run-0-def","kind":"Pod","uid":"u2","uses_gpu":false,"owner_chain":["Job/kueue-job-run"],
    "context":{"kueue":{"local_queue":"team-a","cluster_queue":"cpu-queue","workload":"job-kueue-job-run-1a2b3","priority":0,"active":true}},
    "checkpoint":{"opted_in":true,"eligible":true,"group":"kueue-job-run"}},
   {"namespace":"ml-training","name":"ray-train-checkpoint-raycluster-x-trainers-worker-ghi","kind":"Pod","uid":"u3","uses_gpu":false,
    "owner_chain":["RayCluster/ray-train-checkpoint-raycluster-x"],
    "context":{"ray":{"cluster":"ray-train-checkpoint-raycluster-x","node_type":"worker","group":"trainers","ray_job":"ray-train-checkpoint"}},
    "checkpoint":{"opted_in":true,"eligible":true,"group":"ray-train-checkpoint"}}]},
 "devices":{"whole":0,"mig_instances":0,"time_sliced_replicas":0,"attribution":"unknown"},
 "checkpoint_policy":{"enabled":true,"advisory":true,"groups":true},
 "limits":{"items_truncated":false,"max_items":100},"warnings":["topology-refused"],"unavailable":null}
JSON
	cat >"$adv" <<'JSON'
{"kubeneuron.io/checkpoint":"true","kubeneuron.io/checkpoint-group":"plain-job-run","kubeneuron.io/checkpoint-max-wait":"3m",
 "kubeneuron.io/checkpoint-advisory":"true","kubeneuron.io/checkpoint-advisory-at":"2026-11-05T10:00:00Z",
 "kubeneuron.io/checkpoint-advisory-incident":"inc-1","kubeneuron.io/checkpoint-advisory-round":"1",
 "kubeneuron.io/checkpoint-advisory-reason":"parked for approval","kubeneuron.io/checkpoint-advisory-next-action":"platform.drain"}
JSON
	cat >"$req" <<'JSON'
{"kubeneuron.io/checkpoint":"true","kubeneuron.io/checkpoint-incident":"inc-1","kubeneuron.io/checkpoint-requested-at":"2026-11-05T10:05:00Z",
 "kubeneuron.io/checkpoint-deadline-at":"2026-11-05T10:08:00Z","kubeneuron.io/checkpoint-reason":"drain","kubeneuron.io/checkpoint-next-action":"platform.drain"}
JSON
	printf '%s\n' $'checkpoint-requested\tcheckpoint: requested 6 workloads' \
		$'checkpoint-complete\tcheckpoint: complete after 41.2s: ml-training/plain-job-run-0-abc=exited, ml-training/kueue-job-run-0-def=exited, peer ml-training/ray-train-checkpoint-raycluster-x-trainers-worker-ghi=acknowledged; proceeding with platform.drain' >"$audit"
	cat >"$obj" <<'JSON'
{"kind":"Workload","metadata":{"name":"job-kueue-job-run-1a2b3","managedFields":[{"manager":"kueue","operation":"Update"},{"manager":"kueue-admission","operation":"Update"}]}}
JSON
	local ns=ml-training
	assert_true "good fixture: preview shape" assert_preview_shape "$good" 1 cordon
	assert_true "good fixture: eligible count" assert_preview_eligible "$good" 6
	assert_true "good fixture: group" assert_preview_group "$good" $ns plain-job-run 2 2
	assert_true "good fixture: owner chain" assert_item_owner "$good" $ns plain-job-run-0-abc Job/plain-job-run
	assert_true "good fixture: kueue context" assert_item_kueue "$good" $ns kueue-job-run-0-def team-a cpu-queue
	assert_true "good fixture: ray context" assert_item_ray "$good" $ns ray-train-checkpoint-raycluster-x-trainers-worker-ghi ray-train-checkpoint trainers
	assert_true "good fixture: advisory annotations" assert_advisory_annotations "$adv" inc-1 1 platform.drain
	assert_true "good fixture: request annotations" assert_request_annotations "$req" inc-1
	assert_true "good fixture: audit exited" assert_audit_outcome "$audit" $ns plain-job-run-0-abc exited
	assert_true "good fixture: audit peer acknowledged" assert_audit_outcome "$audit" $ns ray-train-checkpoint-raycluster-x-trainers-worker-ghi acknowledged
	assert_true "good fixture: no KubeNeuron manager" assert_no_kubeneuron_manager "$obj"

	mutate() { "$JQ_BIN" "$2" "$1" >"$tmp/mut.json"; }
	local mutations=0
	mutation() {
		# mutation <description> <source> <jq program> <assertion...>
		local desc=$1 src=$2 prog=$3
		shift 3
		mutate "$src" "$prog"
		mutations=$((mutations + 1))
		assert_true "defect $mutations fails: $desc" test_fails "$@" "$tmp/mut.json" "${EXTRA_ARGS[@]}"
	}
	local -a EXTRA_ARGS
	EXTRA_ARGS=(1 cordon)
	mutation "preview without a digest" "$good" 'del(.digest)' assert_preview_shape
	mutation "preview for round 2 offered as round 1" "$good" '.round = 2' assert_preview_shape
	mutation "preview marked unavailable" "$good" '.unavailable = {"reason":"budget-exhausted"}' assert_preview_shape
	mutation "preview parked at drain, not cordon" "$good" '.pending_step = "drain"' assert_preview_shape
	mutation "preview on a node that claims whole-device topology" "$good" '.node.topology = {"topology":"whole-device","device_actions":"allowed"}' assert_preview_shape
	EXTRA_ARGS=(6)
	mutation "one eligible Pod missing" "$good" '.workloads.eligible_for_checkpoint = 5' assert_preview_eligible
	mutation "listing truncated" "$good" '.limits.items_truncated = true' assert_preview_eligible
	EXTRA_ARGS=("$ns" plain-job-run 2 2)
	mutation "group with one member missing from the node" "$good" '.workloads.groups[0].members_on_node = 1' assert_preview_group
	mutation "group marked too large" "$good" '.workloads.groups[0].too_large = true' assert_preview_group
	mutation "group absent" "$good" 'del(.workloads.groups[0])' assert_preview_group
	EXTRA_ARGS=("$ns" plain-job-run-0-abc Job/plain-job-run)
	mutation "owner chain dropped" "$good" 'del(.workloads.items[0].owner_chain)' assert_item_owner
	mutation "item ineligible" "$good" '.workloads.items[0].checkpoint.eligible = false | .workloads.items[0].checkpoint.ineligibility = "not-gpu-holder"' assert_item_owner
	EXTRA_ARGS=("$ns" kueue-job-run-0-def team-a cpu-queue)
	mutation "kueue context names another LocalQueue" "$good" '.workloads.items[1].context.kueue.local_queue = "team-b"' assert_item_kueue
	mutation "kueue context without the Workload" "$good" '.workloads.items[1].context.kueue.workload = ""' assert_item_kueue
	mutation "kueue context not observed" "$good" 'del(.workloads.items[1].context)' assert_item_kueue
	EXTRA_ARGS=("$ns" ray-train-checkpoint-raycluster-x-trainers-worker-ghi ray-train-checkpoint trainers)
	mutation "ray context says head, not worker" "$good" '.workloads.items[2].context.ray.node_type = "head"' assert_item_ray
	mutation "ray context without the RayJob" "$good" 'del(.workloads.items[2].context.ray.ray_job)' assert_item_ray
	EXTRA_ARGS=(inc-1 1 platform.drain)
	mutation "advisory for another incident" "$adv" '.["kubeneuron.io/checkpoint-advisory-incident"] = "inc-2"' assert_advisory_annotations
	mutation "advisory carrying a request deadline" "$adv" '.["kubeneuron.io/checkpoint-deadline-at"] = "2026-11-05T10:08:00Z"' assert_advisory_annotations
	mutation "advisory naming no next action" "$adv" 'del(.["kubeneuron.io/checkpoint-advisory-next-action"])' assert_advisory_annotations
	mutation "advisory that overwrote the opt-in" "$adv" '.["kubeneuron.io/checkpoint"] = "false"' assert_advisory_annotations
	EXTRA_ARGS=(inc-1)
	mutation "request without a deadline" "$req" 'del(.["kubeneuron.io/checkpoint-deadline-at"])' assert_request_annotations
	mutation "request for another incident" "$req" '.["kubeneuron.io/checkpoint-incident"] = "inc-2"' assert_request_annotations
	EXTRA_ARGS=()
	mutations=$((mutations + 1))
	sed 's/plain-job-run-0-abc=exited/plain-job-run-0-abc=expired/' "$audit" >"$tmp/mut.txt"
	assert_true "defect $mutations fails: audit records the Job Pod as expired" test_fails assert_audit_outcome "$tmp/mut.txt" $ns plain-job-run-0-abc exited
	mutations=$((mutations + 1))
	sed 's/worker-ghi=acknowledged/worker-ghi=expired/' "$audit" >"$tmp/mut.txt"
	assert_true "defect $mutations fails: audit records the Ray worker as expired" test_fails assert_audit_outcome "$tmp/mut.txt" $ns ray-train-checkpoint-raycluster-x-trainers-worker-ghi acknowledged
	mutations=$((mutations + 1))
	grep -v 'complete after' "$audit" >"$tmp/mut.txt"
	assert_true "defect $mutations fails: no completion row at all" test_fails assert_audit_outcome "$tmp/mut.txt" $ns plain-job-run-0-abc exited
	mutation "Workload with a kubeneuron-controller field manager" "$obj" '.metadata.managedFields += [{"manager":"kubeneuron-controller","operation":"Update"}]' assert_no_kubeneuron_manager
	mutation "RayCluster patched by a KubeNeuron-named manager (case-insensitive)" "$obj" '.metadata.managedFields[0].manager = "KubeNeuron"' assert_no_kubeneuron_manager
	((mutations >= 6)) || check "at least 6 mutation self-checks ran (ran $mutations)" 1
	note "$mutations deliberate defects exercised"

	if ((failures > 0)); then
		fail "$failures check(s) failed"
	fi
	note "verify OK: static checks, pins, manifests, local trainer/sidecar run, $mutations mutation self-checks"
}

# ---------------------------------------------------------------------------
# The cluster run.
kc() { "$KUBECTL_BIN" --kubeconfig "$KUBECONFIG_PATH" --request-timeout=30s "$@"; }
api() {
	"$CURL_BIN" --silent --show-error --noproxy '*' --max-time 20 \
		-H "Authorization: Bearer $OPERATOR_TOKEN" "$@"
}
wait_until() {
	# wait_until <description> <command...>: polls until the command succeeds
	# or TIMEOUT_SECONDS pass, then fails the run.
	local desc=$1
	shift
	local end=$((SECONDS + TIMEOUT_SECONDS))
	while ((SECONDS < end)); do
		"$@" && return 0
		sleep 3
	done
	fail "timed out after ${TIMEOUT_SECONDS}s waiting for $desc"
}

work_dir=
pf_pid=
public_port=
target_node=
saved_safety=
root_armed=0
examples_applied=0
policy_applied=0
installed_kueue=0
installed_kuberay=0
incident_id=
declare -a cordoned_by_us=()

cleanup() {
	local rc=$?
	trap - EXIT
	set +e
	if ((rc != 0)) && [[ -n $work_dir ]]; then
		note "failure diagnostics"
		kc -n "$EXAMPLE_NAMESPACE" get pods -o wide 2>&1 | sed 's/^/    /'
		[[ -z $incident_id || -z $public_port ]] || api "http://127.0.0.1:${public_port}/api/v1/incidents/${incident_id}" |
			"$JQ_BIN" -r '.incident.state as $s | "    state=\($s)", (.audit[]? | "    \(.action)\t\(.result)")' 2>/dev/null
	fi
	if [[ -n $incident_id && -n $public_port ]]; then
		api -X POST -H 'Content-Type: application/json' -H "Idempotency-Key: workload-safe-${RUN_ID}-cleanup" \
			--data-binary '{"actor":"workload-safe-fixture","reason":"fixture teardown"}' -o /dev/null \
			"http://127.0.0.1:${public_port}/api/v1/incidents/${incident_id}/resolve" 2>/dev/null
	fi
	if ((KEEP_RESOURCES == 0)); then
		# Delete the examples by hand before the namespace: a namespace that
		# still holds the Jobs, the RayJob and the Kueue Workload (with the
		# operators' finalizers on them) has twice sat in Terminating for
		# longer than the next run's wait on this cluster, while the same
		# objects deleted explicitly let it finish within a minute.
		if ((examples_applied)); then
			local e
			for e in $EXAMPLES; do
				if [[ -f "$EXAMPLES_DIR/$e/kustomization.yaml" ]]; then
					kc delete -k "$EXAMPLES_DIR/$e" --ignore-not-found --wait=false >/dev/null 2>&1
				else
					kc delete -f "$EXAMPLES_DIR/$e" --ignore-not-found --wait=false >/dev/null 2>&1
				fi
			done
			kc delete namespace "$EXAMPLE_NAMESPACE" --ignore-not-found --wait=false >/dev/null 2>&1
		fi
		((examples_applied == 0)) || kc delete clusterqueue cpu-queue resourceflavor default-flavor --ignore-not-found >/dev/null 2>&1
		((policy_applied == 0)) || kc delete gpuremediationpolicy "$POLICY_NAME" --ignore-not-found >/dev/null 2>&1
		((policy_applied == 0)) || kc delete gpuplaybook "$PLAYBOOK_NAME" --ignore-not-found >/dev/null 2>&1
	fi
	if ((root_armed)); then
		if [[ $saved_safety == null ]]; then
			kc patch kubeneuron "$ROOT_NAME" --type=json -p '[{"op":"remove","path":"/spec/safety"}]' >/dev/null 2>&1
		else
			kc patch kubeneuron "$ROOT_NAME" --type=json -p "[{\"op\":\"replace\",\"path\":\"/spec/safety\",\"value\":$saved_safety}]" >/dev/null 2>&1
		fi
	fi
	if [[ -n $target_node ]]; then
		kc uncordon "$target_node" >/dev/null 2>&1
		kc label node "$target_node" "${NODE_LABEL}-" >/dev/null 2>&1
	fi
	local n
	for n in "${cordoned_by_us[@]}"; do kc uncordon "$n" >/dev/null 2>&1; done
	if ((REMOVE_DEPENDENCIES)); then
		((installed_kuberay == 0)) || "$HELM_BIN" --kubeconfig "$KUBECONFIG_PATH" uninstall kuberay-operator -n kuberay-system >/dev/null 2>&1
		((installed_kueue == 0)) || kc delete -f "$work_dir/kueue-manifests.yaml" --ignore-not-found --wait=false >/dev/null 2>&1
	fi
	if [[ -n $pf_pid ]]; then
		kill "$pf_pid" >/dev/null 2>&1
		wait "$pf_pid" >/dev/null 2>&1
	fi
	[[ -z $work_dir ]] || rm -rf -- "$work_dir"
	((rc == 0)) || note "set KEEP_RESOURCES=1 to retain the examples for inspection"
	exit "$rc"
}

download_pinned() {
	# download_pinned <url> <sha256> <dest>
	"$CURL_BIN" --silent --show-error --fail --location --max-time 300 -o "$3" "$1" || fail "download of $1 failed"
	printf '%s  %s\n' "$2" "$3" | sha256sum --check --quiet - || fail "$1 does not match the pinned sha256 $2"
}

controller_sa() { printf 'system:serviceaccount:%s:%s-controller' "$ROOT_NAMESPACE" "$ROOT_NAME"; }
can_i() { [[ "$(kc auth can-i "$1" "$2" --as="$(controller_sa)" 2>/dev/null || true)" == yes ]]; }

install_dependencies() {
	if has_example kueue-job; then
		if kc get crd workloads.kueue.x-k8s.io >/dev/null 2>&1; then
			note "Kueue CRDs present; not installing"
		else
			((INSTALL_DEPENDENCIES)) || fail "Kueue is not installed and INSTALL_DEPENDENCIES=0"
			note "installing Kueue $KUEUE_VERSION from the pinned release manifest"
			download_pinned "$KUEUE_URL" "$KUEUE_SHA256" "$work_dir/kueue-manifests.yaml"
			kc apply --server-side -f "$work_dir/kueue-manifests.yaml" >/dev/null
			installed_kueue=1
		fi
		kc -n kueue-system rollout status deployment/kueue-controller-manager --timeout="${TIMEOUT_SECONDS}s" >/dev/null ||
			fail "kueue-controller-manager never became available"
	fi
	if has_example kuberay-ray-train; then
		if kc get crd rayjobs.ray.io >/dev/null 2>&1; then
			note "KubeRay CRDs present; not installing"
		else
			((INSTALL_DEPENDENCIES)) || fail "KubeRay is not installed and INSTALL_DEPENDENCIES=0"
			require_cmd HELM_BIN "$HELM_BIN"
			note "installing the KubeRay operator chart $KUBERAY_VERSION from the pinned archive"
			download_pinned "$KUBERAY_URL" "$KUBERAY_SHA256" "$work_dir/kuberay-operator.tgz"
			"$HELM_BIN" --kubeconfig "$KUBECONFIG_PATH" upgrade --install kuberay-operator "$work_dir/kuberay-operator.tgz" \
				--namespace kuberay-system --create-namespace --wait --timeout "${TIMEOUT_SECONDS}s" >/dev/null ||
				fail "the KubeRay operator chart did not install"
			installed_kuberay=1
		fi
		kc -n kuberay-system rollout status deployment/kuberay-operator --timeout="${TIMEOUT_SECONDS}s" >/dev/null ||
			fail "kuberay-operator never became available"
	fi
}

namespace_absent_or_active() {
	# A namespace left Terminating by an earlier run refuses every create
	# until the namespace controller finishes with it; wait instead of
	# applying into it.
	local phase
	phase=$(kc get namespace "$EXAMPLE_NAMESPACE" -o jsonpath='{.status.phase}' 2>/dev/null) || return 0
	[[ $phase != Terminating ]]
}

apply_examples() {
	local e
	wait_until "namespace $EXAMPLE_NAMESPACE to finish terminating" namespace_absent_or_active
	kc create namespace "$EXAMPLE_NAMESPACE" --dry-run=client -o yaml | kc apply -f - >/dev/null
	examples_applied=1
	for e in $EXAMPLES; do
		case $e in
		plain-job)
			kc apply -k "$EXAMPLES_DIR/plain-job" >/dev/null
			;;
		kueue-job)
			# Kueue's webhook answers only once its certificate is in place.
			wait_until "the Kueue webhook to admit the kueue-job example" kc apply -k "$EXAMPLES_DIR/kueue-job" >/dev/null
			;;
		kuberay-ray-train)
			# The pinned image, from the lock, not from the file, so the run
			# never trusts a manifest edit over the lock.
			sed "s#^\(\s*image: \)rayproject/ray:.*\$#\1${RAY_IMAGE}#" "$EXAMPLES_DIR/kuberay-ray-train/rayjob.yaml" |
				kc apply -f - >/dev/null
			;;
		esac
	done
}

# resume_check: Phase C. The plain-job-pvc overlay (one index, the
# checkpoint on a PersistentVolumeClaim) is driven through a request the way
# KubeNeuron would stamp it — the annotations, not the controller, so this
# needs no incident — and the Pod the Job creates in its place must resume
# from the step the first one checkpointed. This is the only place the
# examples show a checkpoint surviving its Pod; the base manifests keep an
# emptyDir and restart from step 0, and say so.
resume_check() {
	local first second step
	note "phase C: the plain-job-pvc overlay must resume from its checkpoint after the request"
	kc -n "$EXAMPLE_NAMESPACE" delete job plain-job-run --ignore-not-found --wait=true >/dev/null 2>&1 || true
	kc apply -k "$EXAMPLES_DIR/plain-job-pvc" >/dev/null
	wait_until "the plain-job-pvc Pod Running (claim bound)" count_running job-name=plain-job-run 1
	first=$(running_pods job-name=plain-job-run | head -n 1)
	wait_until "$first to take its first periodic checkpoint" pod_logs_have "$first" trainer 'trainer: checkpoint at step [0-9]* (periodic)'
	kc -n "$EXAMPLE_NAMESPACE" annotate pod "$first" --overwrite \
		kubeneuron.io/checkpoint-incident=inc-resume-check \
		kubeneuron.io/checkpoint-requested-at="$(date -u +%FT%TZ)" \
		kubeneuron.io/checkpoint-deadline-at="$(date -u -d '+3 minutes' +%FT%TZ)" \
		kubeneuron.io/checkpoint-next-action=platform.drain >/dev/null
	wait_until "$first's trainer to checkpoint on the request" pod_logs_have "$first" trainer 'trainer: checkpoint at step [0-9]* (request)'
	step=$(kc -n "$EXAMPLE_NAMESPACE" logs "$first" -c trainer --tail=-1 2>/dev/null | sed -n 's/^trainer: checkpoint at step \([0-9]*\) (request)$/\1/p' | tail -n 1)
	[[ -n $step && $step -gt 0 ]] || fail "no request checkpoint step in $first's log"
	wait_until "$first to leave (exit 75)" pod_gone_or_terminal "$first"
	replacement_running() { running_pods job-name=plain-job-run | grep -qv "^$first\$"; }
	wait_until "the Job to replace the index" replacement_running
	second=$(running_pods job-name=plain-job-run | grep -v "^$first\$" | head -n 1)
	[[ -n $second && $second != "$first" ]] || fail "no replacement Pod for $first"
	wait_until "$second to resume from step $step" pod_logs_have "$second" trainer "trainer: resuming from step $step"
	check "replacement Pod $second resumed from step $step checkpointed by $first on the claim" 0
	kc -n "$EXAMPLE_NAMESPACE" delete job plain-job-run --ignore-not-found --wait=false >/dev/null 2>&1 || true
	kc -n "$EXAMPLE_NAMESPACE" delete pvc plain-job-ckpt --ignore-not-found --wait=false >/dev/null 2>&1 || true
}

running_pods() {
	# running_pods <label selector> -> names of Running Pods, one per line
	kc -n "$EXAMPLE_NAMESPACE" get pods -l "$1" --field-selector=status.phase=Running \
		-o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}' 2>/dev/null
}
count_running() { [[ "$(running_pods "$1" | grep -c .)" -ge "$2" ]]; }
pod_node() { kc -n "$EXAMPLE_NAMESPACE" get pod "$1" -o jsonpath='{.spec.nodeName}' 2>/dev/null; }
pod_annotations_json() { kc -n "$EXAMPLE_NAMESPACE" get pod "$1" -o json 2>/dev/null | "$JQ_BIN" '.metadata.annotations // {}'; }
pod_phase() { kc -n "$EXAMPLE_NAMESPACE" get pod "$1" -o jsonpath='{.status.phase}' 2>/dev/null || true; }
pod_gone_or_terminal() {
	local phase
	phase=$(pod_phase "$1")
	[[ -z $phase || $phase == Succeeded || $phase == Failed ]]
}
logs_have() {
	# logs_have <label selector> <container or ""> <pattern>
	local -a c=()
	[[ -z $2 ]] || c=(-c "$2")
	kc -n "$EXAMPLE_NAMESPACE" logs -l "$1" "${c[@]}" --tail=-1 2>/dev/null | grep -q -- "$3"
}
pod_logs_have() {
	local -a c=()
	[[ -z $2 ]] || c=(-c "$2")
	kc -n "$EXAMPLE_NAMESPACE" logs "$1" "${c[@]}" --tail=-1 2>/dev/null | grep -q -- "$3"
}
incident_state() {
	api "http://127.0.0.1:${public_port}/api/v1/incidents/${incident_id}" | "$JQ_BIN" -r '.incident.state // empty'
}
incident_in() {
	local s
	s=$(incident_state)
	case " $* " in *" $s "*) return 0 ;; esac
	return 1
}
audit_to() { api "http://127.0.0.1:${public_port}/api/v1/incidents/${incident_id}" | "$JQ_BIN" -r '.audit[]? | "\(.action)\t\(.result)"' >"$1"; }
runtime_mode_is() { [[ "$(api "http://127.0.0.1:${public_port}/api/v1/runtime-config" | "$JQ_BIN" -r '.execution_mode // empty')" == "$1" ]]; }
node_cordoned() { [[ "$(kc get node "$1" -o jsonpath='{.spec.unschedulable}')" == true ]]; }

# assert_acknowledged <annotations.json> <incident>: the state key reads complete:<incident>.
assert_acknowledged() { "$JQ_BIN" -e --arg v "complete:$2" '.["kubeneuron.io/checkpoint-state"] == $v' "$1" >/dev/null; }
# assert_not_advised <annotations.json>: no advisory key at all.
assert_not_advised() { "$JQ_BIN" -e 'has("kubeneuron.io/checkpoint-advisory") == false' "$1" >/dev/null; }

annotations_pass() {
	# annotations_pass <pod> <assertion> <args...>: fetch, then assert.
	local pod=$1 fn=$2
	shift 2
	pod_annotations_json "$pod" >"$work_dir/ann-$pod.json" || return 1
	"$fn" "$work_dir/ann-$pod.json" "$@"
}

run_cluster() {
	require_cmd KUBECTL_BIN "$KUBECTL_BIN"
	require_cmd JQ_BIN "$JQ_BIN"
	require_cmd PYTHON_BIN "$PYTHON_BIN"
	require_cmd CURL_BIN "$CURL_BIN"
	[[ -f $KUBECONFIG_PATH ]] || fail "KUBECONFIG_PATH $KUBECONFIG_PATH does not exist; run KEEP_CLUSTER=1 make test-integration-kind first or point at another cluster"
	load_pins
	work_dir=$(mktemp -d "${TMPDIR:-/tmp}/kind-workload-safe.XXXXXX")
	trap cleanup EXIT
	trap 'exit 130' INT TERM

	kc get --raw=/readyz >/dev/null || fail "the API server behind $KUBECONFIG_PATH is not reachable"
	kc get kubeneuron "$ROOT_NAME" >/dev/null || fail "KubeNeuron root $ROOT_NAME not found"
	if [[ -z $OPERATOR_TOKEN ]]; then
		local secret
		secret=$(kc get kubeneuron "$ROOT_NAME" -o jsonpath='{.spec.notifications.operatorAPIToken.name}')
		[[ -n $secret ]] || fail "the root names no operatorAPIToken Secret; set OPERATOR_TOKEN"
		OPERATOR_TOKEN=$(kc -n "$ROOT_NAMESPACE" get secret "$secret" -o jsonpath='{.data.token}' | base64 -d)
		[[ -n $OPERATOR_TOKEN ]] || fail "Secret $ROOT_NAMESPACE/$secret holds no token key"
	fi
	saved_safety=$(kc get kubeneuron "$ROOT_NAME" -o json | "$JQ_BIN" -c '.spec.safety // null')

	mapfile -t workers < <(kc get nodes -l '!node-role.kubernetes.io/control-plane' -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}')
	((${#workers[@]} >= 1)) || fail "no worker node"
	# Not the controller's own node: its PodDisruptionBudget refuses the
	# eviction and the drain would never finish.
	local controller_node w
	controller_node=$(kc -n "$ROOT_NAMESPACE" get pods -l app.kubernetes.io/component=controller -o jsonpath='{.items[0].spec.nodeName}' 2>/dev/null || true)
	target_node=''
	for w in "${workers[@]}"; do
		[[ $w == "$controller_node" ]] && continue
		target_node=$w
		break
	done
	[[ -n $target_node ]] || fail "every worker hosts the controller ($controller_node); the drain needs a node it can empty"
	note "target node $target_node (controller on ${controller_node:-?}); ${#workers[@]} worker(s); examples: $EXAMPLES"

	# The controller API, through a port-forward, before anything is changed.
	: >"$work_dir/pf.log"
	kc -n "$ROOT_NAMESPACE" port-forward "service/${ROOT_NAME}-controller" ":${controllerPort}" >"$work_dir/pf.log" 2>&1 &
	pf_pid=$!
	local end=$((SECONDS + 30))
	while ((SECONDS < end)); do
		public_port=$(sed -n "s/^Forwarding from 127\\.0\\.0\\.1:\\([0-9][0-9]*\\) -> ${controllerPort}$/\\1/p" "$work_dir/pf.log")
		[[ -n $public_port ]] && break
		sleep 1
	done
	[[ -n $public_port ]] || fail "port-forward to ${ROOT_NAME}-controller allocated no port"
	api -o /dev/null -w '%{http_code}' "http://127.0.0.1:${public_port}/api/v1/runtime-config" | grep -qx 200 ||
		fail "the controller API refused the operator token"

	install_dependencies

	# One playbook that parks at cordon and then drains, and one policy that
	# routes this run's class to it. Both are removed on exit.
	kc apply -f - >/dev/null <<-YAML
	apiVersion: kubeneuron.io/v1alpha1
	kind: GPUPlaybook
	metadata:
	  name: $PLAYBOOK_NAME
	spec:
	  kubeNeuronRef: $ROOT_NAME
	  target: Node
	  steps:
	    - name: cordon
	      action: Cordon
	      approval: Required
	    - name: drain
	      action: Drain
	---
	apiVersion: kubeneuron.io/v1alpha1
	kind: GPURemediationPolicy
	metadata:
	  name: $POLICY_NAME
	spec:
	  kubeNeuronRef: $ROOT_NAME
	  priority: 4
	  match:
	    class: $INCIDENT_CLASS
	  playbookRef: $PLAYBOOK_NAME
	YAML
	policy_applied=1

	# Every example Pod on the target node: the other workers are cordoned
	# while the examples roll out, then released, so the preview, the
	# advisory and the request can be asserted on one node.
	local n
	for n in "${workers[@]}"; do
		[[ $n == "$target_node" ]] && continue
		node_cordoned "$n" && continue
		kc cordon "$n" >/dev/null
		cordoned_by_us+=("$n")
	done
	apply_examples
	local -a opted=() job_pods=() ray_workers=()
	local -A group_of=()
	if has_example plain-job; then
		wait_until "plain-job Pods Running" count_running job-name=plain-job-run 2
		mapfile -t pods < <(running_pods job-name=plain-job-run)
		job_pods+=("${pods[@]}")
		for n in "${pods[@]}"; do group_of[$n]=plain-job-run; done
	fi
	if has_example kueue-job; then
		wait_until "kueue-job Pods admitted and Running" count_running job-name=kueue-job-run 2
		mapfile -t pods < <(running_pods job-name=kueue-job-run)
		job_pods+=("${pods[@]}")
		for n in "${pods[@]}"; do group_of[$n]=kueue-job-run; done
	fi
	if has_example kuberay-ray-train; then
		wait_until "Ray worker Pods Running (image pull included)" count_running ray.io/node-type=worker 2
		mapfile -t ray_workers < <(running_pods ray.io/node-type=worker)
		for n in "${ray_workers[@]}"; do group_of[$n]=ray-train-checkpoint; done
		wait_until "the Ray driver to start training" logs_have job-name=ray-train-checkpoint "" 'checkpoint-example'
	fi
	opted=("${job_pods[@]}" "${ray_workers[@]}")
	((${#opted[@]} > 0)) || fail "no example Pods"
	for n in "${opted[@]}"; do
		[[ "$(pod_node "$n")" == "$target_node" ]] || fail "$n runs on $(pod_node "$n"), not on $target_node"
	done
	for n in "${cordoned_by_us[@]}"; do kc uncordon "$n" >/dev/null; done
	cordoned_by_us=()
	note "${#opted[@]} opted-in example Pods Running on $target_node"

	# Snapshot the objects KubeNeuron must never write, before it reads them.
	snapshot_foreign() {
		: >"$work_dir/foreign-$1.list"
		if has_example kueue-job; then
			kc -n "$EXAMPLE_NAMESPACE" get workloads.kueue.x-k8s.io -o name >>"$work_dir/foreign-$1.list" 2>/dev/null || true
		fi
		if has_example kuberay-ray-train; then
			kc -n "$EXAMPLE_NAMESPACE" get rayjobs.ray.io,rayclusters.ray.io -o name >>"$work_dir/foreign-$1.list" 2>/dev/null || true
		fi
	}
	snapshot_foreign before
	[[ ! -s "$work_dir/foreign-before.list" ]] || note "foreign objects under watch: $(tr '\n' ' ' <"$work_dir/foreign-before.list")"

	# Arm the one node and switch the v0.8 blocks on, with every context
	# resolver the examples need.
	kc label node "$target_node" "${NODE_LABEL}=yes" --overwrite >/dev/null
	kc patch kubeneuron "$ROOT_NAME" --type=merge -p "$(
		cat <<-JSON
		{"spec":{"safety":{"executionMode":"Enabled","destructiveExecution":{
		  "nodeSelector":{"$NODE_LABEL":"yes"},
		  "acknowledgement":"I understand these nodes may be reset, rebooted, or destroyed"},
		 "checkpointCoordination":{"enabled":true,"namespaces":["$EXAMPLE_NAMESPACE"],"defaultWait":"3m","maxWait":"5m",
		  "advisory":{"enabled":true,"budget":"10s"},
		  "groups":{"enabled":true,"maxGroupSize":16,"crossNode":true}},
		 "impactPreview":{"enabled":true,"budget":"15s","maxItems":100,"requireForApproval":true,
		  "workloadContext":{"batchJobs":true,"kueue":$(has_example kueue-job && echo true || echo false),"kuberay":$(has_example kuberay-ray-train && echo true || echo false)}}}}}
		JSON
	)" >/dev/null
	root_armed=1
	wait_until "the controller to hold pods: patch" can_i patch pods
	! has_example kueue-job || wait_until "the controller to hold read-only Kueue access" can_i list workloads.kueue.x-k8s.io
	! has_example kuberay-ray-train || wait_until "the controller to hold read-only Ray access" can_i list rayclusters.ray.io
	[[ "$(kc auth can-i patch workloads.kueue.x-k8s.io --as="$(controller_sa)" 2>/dev/null || true)" != yes ]] ||
		fail "the controller can patch Kueue Workloads: the context grant is not read-only"
	[[ "$(kc auth can-i patch rayclusters.ray.io --as="$(controller_sa)" 2>/dev/null || true)" != yes ]] ||
		fail "the controller can patch RayClusters: the context grant is not read-only"
	# By digest, not by mode: a controller still running the previous snapshot
	# may already report enabled, and an incident parked under it would carry no
	# advisory and no preview.
	root_generation_observed() {
		local g o
		g=$(kc get kubeneuron "$ROOT_NAME" -o jsonpath='{.metadata.generation}')
		o=$(kc get kubeneuron "$ROOT_NAME" -o jsonpath='{.status.observedGeneration}')
		[[ -n $g && $g == "$o" ]]
	}
	wait_until "the operator to observe the armed root" root_generation_observed
	local armed_digest
	armed_digest=$(kc get kubeneuron "$ROOT_NAME" -o jsonpath='{.status.configDigest}')
	[[ -n $armed_digest ]] || fail "the root has no configDigest after arming"
	config_loaded() { [[ "$(api "http://127.0.0.1:${public_port}/api/v1/runtime-config" | "$JQ_BIN" -r '.source_digest // empty')" == "$armed_digest" ]]; }
	wait_until "the controller to load configuration ${armed_digest:0:12}" config_loaded
	runtime_mode_is enabled || fail "the controller reports a mode other than enabled under the armed configuration"
	note "armed: coordination with advisory and groups, preview required for approval, contexts on (config ${armed_digest:0:12} live)"

	# --- Phase A --------------------------------------------------------------
	local code
	code=$(api -H 'Content-Type: application/json' \
		--data-binary "{\"node\":\"${target_node}\",\"class\":\"${INCIDENT_CLASS}\",\"actor\":\"workload-safe-fixture\"}" \
		-o /dev/null -w '%{http_code}' "http://127.0.0.1:${public_port}/api/v1/incidents")
	[[ $code == 202 ]] || fail "opening the incident returned $code, want 202"
	find_incident() {
		incident_id=$(api "http://127.0.0.1:${public_port}/api/v1/incidents?node=${target_node}" |
			"$JQ_BIN" -r --arg c "$INCIDENT_CLASS" '[(. // [])[] | select(.class == $c and (.state | IN("RESOLVED","CLOSED") | not))][0].id // empty')
		[[ -n $incident_id ]]
	}
	wait_until "the incident to appear" find_incident
	wait_until "incident $incident_id to park at AWAITING_APPROVAL" incident_in AWAITING_APPROVAL
	note "incident $incident_id parked for approval"

	for n in "${opted[@]}"; do
		wait_until "the advisory on $n" annotations_pass "$n" assert_advisory_annotations "$incident_id" 1 platform.drain
	done
	local head
	for head in $(running_pods ray.io/node-type=head); do
		annotations_pass "$head" assert_not_advised ||
			fail "the Ray head, which never opted in, was advised"
	done
	note "advisory stamped on ${#opted[@]} opted-in Pods only"
	for n in "${job_pods[@]}"; do
		wait_until "$n's sidecar to log the advisory" pod_logs_have "$n" checkpoint-sidecar "sidecar: advisory for incident ${incident_id} round 1"
		wait_until "$n's trainer to checkpoint on the advisory" pod_logs_have "$n" trainer 'trainer: checkpoint at step [0-9]* (advisory)'
	done
	if ((${#ray_workers[@]} > 0)); then
		wait_until "the Ray driver to log a worker's advisory checkpoint" logs_have job-name=ray-train-checkpoint "" "advisory for incident ${incident_id}"
	fi
	note "the trainers checkpointed on the advisory"

	local preview="$work_dir/preview-a.json" digest
	code=$(api -o "$preview" -w '%{http_code}' "http://127.0.0.1:${public_port}/api/v1/incidents/${incident_id}/impact-preview")
	[[ $code == 200 ]] || fail "impact preview returned $code"
	assert_preview_shape "$preview" 1 cordon || { "$JQ_BIN" . "$preview" >&2; fail "the preview is not a computed round-1 document parked at cordon on an unknown/refused node"; }
	assert_preview_eligible "$preview" "${#opted[@]}" || { "$JQ_BIN" .workloads "$preview" >&2; fail "the preview does not count exactly ${#opted[@]} eligible Pods"; }
	local -A group_seen=()
	for n in "${opted[@]}"; do group_seen[${group_of[$n]}]=1; done
	for n in "${!group_seen[@]}"; do
		assert_preview_group "$preview" "$EXAMPLE_NAMESPACE" "$n" 2 2 || { "$JQ_BIN" .workloads.groups "$preview" >&2; fail "group $n is not two members, both on the node"; }
	done
	for n in "${job_pods[@]}"; do
		assert_item_owner "$preview" "$EXAMPLE_NAMESPACE" "$n" "Job/${group_of[$n]}" || { "$JQ_BIN" .workloads.items "$preview" >&2; fail "$n lacks owner_chain Job/${group_of[$n]}"; }
		if [[ ${group_of[$n]} == kueue-job-run ]]; then
			assert_item_kueue "$preview" "$EXAMPLE_NAMESPACE" "$n" team-a cpu-queue || { "$JQ_BIN" .workloads.items "$preview" >&2; fail "$n lacks context.kueue team-a/cpu-queue with its Workload"; }
		fi
	done
	for n in "${ray_workers[@]}"; do
		assert_item_ray "$preview" "$EXAMPLE_NAMESPACE" "$n" ray-train-checkpoint trainers || { "$JQ_BIN" .workloads.items "$preview" >&2; fail "$n lacks context.ray worker/trainers/ray-train-checkpoint"; }
	done
	digest=$("$JQ_BIN" -r .digest "$preview")
	note "preview round 1: ${#opted[@]} eligible, groups ${!group_seen[*]}, contexts resolved, digest $digest"
	code=$(api -H 'Content-Type: application/json' --data-binary '{"actor":"workload-safe-fixture","park_epoch":1}' \
		-o /dev/null -w '%{http_code}' "http://127.0.0.1:${public_port}/api/v1/incidents/${incident_id}/approve")
	[[ $code == 409 ]] || fail "approve without the digest returned $code, want 409"

	# --- Phase B --------------------------------------------------------------
	local -A uid_before=()
	for n in "${opted[@]}"; do uid_before[$n]=$(kc -n "$EXAMPLE_NAMESPACE" get pod "$n" -o jsonpath='{.metadata.uid}'); done
	code=$(api -H 'Content-Type: application/json' \
		--data-binary "{\"actor\":\"workload-safe-fixture\",\"park_epoch\":1,\"impact_digest\":\"$digest\"}" \
		-o /dev/null -w '%{http_code}' "http://127.0.0.1:${public_port}/api/v1/incidents/${incident_id}/approve")
	[[ $code == 204 ]] || fail "approve bound to $digest returned $code, want 204"
	for n in "${opted[@]}"; do
		wait_until "the request on $n" annotations_pass "$n" assert_request_annotations "$incident_id"
	done
	note "request stamped on ${#opted[@]} Pods after the digest-bound approve"
	for n in "${job_pods[@]}"; do
		wait_until "$n to checkpoint and leave" pod_gone_or_terminal "$n"
	done
	for n in "${ray_workers[@]}"; do
		wait_until "$n to acknowledge with checkpoint-state" annotations_pass "$n" assert_acknowledged "$incident_id"
	done
	wait_until "the incident to pass the drain (VERIFYING or RESOLVED)" incident_in VERIFYING RESOLVED
	audit_to "$work_dir/audit-b.txt"
	for n in "${job_pods[@]}"; do
		assert_audit_outcome "$work_dir/audit-b.txt" "$EXAMPLE_NAMESPACE" "$n" exited || { cat "$work_dir/audit-b.txt" >&2; fail "$n is not recorded as exited"; }
	done
	for n in "${ray_workers[@]}"; do
		assert_audit_outcome "$work_dir/audit-b.txt" "$EXAMPLE_NAMESPACE" "$n" acknowledged || { cat "$work_dir/audit-b.txt" >&2; fail "$n is not recorded as acknowledged"; }
	done
	grep -q "checkpoint-advisory"$'\t' "$work_dir/audit-b.txt" || fail "no checkpoint-advisory audit row"
	grep -q "impact-preview-computed"$'\t' "$work_dir/audit-b.txt" || fail "no impact-preview-computed audit row"
	node_cordoned "$target_node" || fail "$target_node is not cordoned after the drain"
	[[ "$(api "http://127.0.0.1:${public_port}/api/v1/incidents/${incident_id}" | "$JQ_BIN" -r '.impact_preview_digest // empty')" == "$digest" ]] ||
		fail "the incident detail does not carry the bound digest"
	for n in "${job_pods[@]}"; do
		[[ "$(kc -n "$EXAMPLE_NAMESPACE" get pod "$n" -o jsonpath='{.metadata.uid}' 2>/dev/null)" != "${uid_before[$n]}" ]] ||
			[[ "$(pod_phase "$n")" == Failed ]] || fail "$n neither left nor failed"
	done
	note "drained: Job Pods exited, Ray workers acknowledged, audit and digest consistent"

	# --- Nothing foreign was written ----------------------------------------
	local obj
	while IFS= read -r obj; do
		[[ -n $obj ]] || continue
		if ! kc -n "$EXAMPLE_NAMESPACE" get "$obj" -o json >"$work_dir/obj.json" 2>/dev/null; then
			# The Ray workers return from the training loop once they have
			# acknowledged, the RayJob finishes, and KubeRay (not KubeNeuron,
			# which holds no delete verb on ray.io) removes the RayCluster
			# because shutdownAfterJobFinishes is set. Anything else gone is
			# a failure.
			[[ $obj == raycluster.ray.io/* ]] || fail "$obj vanished during the run"
			[[ "$(kc -n "$EXAMPLE_NAMESPACE" get rayjob ray-train-checkpoint -o jsonpath='{.status.jobDeploymentStatus}' 2>/dev/null)" == @(Complete|Failed) ]] ||
				fail "$obj vanished while its RayJob had not finished"
			note "$obj was removed by KubeRay after its RayJob finished (shutdownAfterJobFinishes)"
			continue
		fi
		assert_no_kubeneuron_manager "$work_dir/obj.json" || { "$JQ_BIN" '.metadata.managedFields' "$work_dir/obj.json" >&2; fail "$obj carries a KubeNeuron field manager"; }
	done <"$work_dir/foreign-before.list"
	[[ ! -s "$work_dir/foreign-before.list" ]] || note "no Kueue or Ray object gained a KubeNeuron field manager"

	api -X POST -H 'Content-Type: application/json' -H "Idempotency-Key: workload-safe-${RUN_ID}-resolve" \
		--data-binary '{"actor":"workload-safe-fixture","reason":"fixture complete"}' -o /dev/null \
		"http://127.0.0.1:${public_port}/api/v1/incidents/${incident_id}/resolve" || true
	incident_id=
	if has_example plain-job; then
		resume_check
	fi
	note "workload-safe examples fixture PASSED on $target_node: $EXAMPLES"
	note "this proves the annotation protocol on CPU-only toys; it is not evidence that Kueue, KubeRay or Ray Train integrate with KubeNeuron, and no hardware claim"
}

if [[ $MODE == verify ]]; then
	verify_offline
else
	run_cluster
fi
