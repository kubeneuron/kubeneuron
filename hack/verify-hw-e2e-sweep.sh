#!/usr/bin/env bash
# verify-hw-e2e-sweep.sh — prove the hardware sweep's networking cleanup
# without AWS.
#
# Why this exists: run kubeneuron-e2e-v5-20260913-a17 passed every phase and
# then could not be swept. Its cluster stack sat in DELETE_FAILED behind the
# detached VPC-CNI interface of the node ReplaceNode had terminated, and then
# behind the EKS cluster security group that interface had pinned; both were
# removed by hand. The sweep now clears that class of orphan itself, and this
# script is the only place that behaviour is exercised deterministically — a
# live run costs a GPU cluster and proves one path once.
#
# It runs the REAL `hack/hw-e2e.sh sweep` against a scripted `aws`
# (hack/hw-e2e-fake-aws.py) and a scripted `eksctl`, both placed first on
# PATH, and asserts three things per scenario: the sweep's real exit status,
# what it deleted (from the fake's call log and end state), and what it
# refused to touch. The refusals matter more than the cleanup: the same code
# that removes this run's orphan must leave a live cluster's interface, another
# cluster's security group, an ambient VPC, and a default group exactly where
# they are.
#
# EBS volumes, second: the sweep once found the controller's and VMSingle's
# volumes by kubernetes.io/created-for/pvc/namespace, which the EBS CSI
# driver writes without anything naming the cluster, so one sweep could have
# deleted another cluster's state in the same two namespaces (found reviewing
# a real G3 teardown, 2026-10-18). Now each claim's volume is resolved
# PVC -> PV -> CSI handle while the cluster is live, cross-checked against the
# driver's own tags, tagged with the run identity, and swept by that tag
# alone. Proven here: a volume in the same namespace with the same claim name
# but no run tag is never deleted and never even queried for; a live-cluster
# sweep resolves and tags exactly the two named claims (and their PV) before
# the delete; every link of the resolution chain refuses on its own, records
# the claim as unproven, and the sweep then refuses to report clean; a
# recorded volume that lost its tag is named and left; the kubeconfig the
# live-cluster path reads the claims through (written there, or left by an
# earlier phase) is validated against the EKS endpoint of exactly this cluster
# before any claim lookup or tag action, and one that does not validate
# records every claim as unproven, reads nothing, tags nothing, and still
# lets the cluster delete proceed; and, structurally, every describe-volumes
# in the harness is scoped by id, run tag or exact cluster tag, and the sweep
# names no namespace at all. A legacy in-tree PV (.spec.awsElasticBlockStore)
# is resolved too, on its own explicit schema: its volume must also carry
# KubernetesCluster=<this cluster> exactly, a PV that sets neither source,
# both sources or a malformed one is refused before any AWS call, and the CSI
# chain is unchanged.
#
# Usage: hack/verify-hw-e2e-sweep.sh
#
# shellcheck disable=SC2016 # the '$c' '$o' '$s' in single quotes are jq variables, bound by scenario().
set -euo pipefail

repo_root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd -P)
cd "$repo_root"

fail=0
note() { printf '\n=== %s\n' "$*"; }
bad() {
	echo "SWEEP CHECK FAILED: $*" >&2
	fail=1
}

for tool in python3 jq bash; do
	command -v "$tool" >/dev/null 2>&1 || {
		echo "verify-hw-e2e-sweep: $tool is required" >&2
		exit 1
	}
done

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

# --- the fakes ----------------------------------------------------------------
fakebin="$work/bin"
mkdir -p "$fakebin"
cat >"$fakebin/aws" <<EOF
#!/usr/bin/env bash
exec python3 "$repo_root/hack/hw-e2e-fake-aws.py" "\$@"
EOF
# eksctl: `get cluster` answers from the same state as the fake aws; `delete
# cluster` returns non-zero and removes nothing, which is the case the sweep
# has to cope with (teardown's own delete has already failed by the time the
# sweep matters); `utils write-kubeconfig` writes a stub the fake kubectl
# accepts, for a live cluster only.
cat >"$fakebin/eksctl" <<'EOF'
#!/usr/bin/env bash
printf 'eksctl %s\n' "$*" >>"$FAKE_AWS_LOG"
sub="${1:-} ${2:-}"
name="" kubeconfig=""
while [ $# -gt 0 ]; do
	case "$1" in
	--name | --cluster) name=$2; shift ;;
	--kubeconfig) kubeconfig=$2; shift ;;
	esac
	shift
done
if [ -n "$name" ] && aws eks describe-cluster --name "$name" >/dev/null 2>&1; then
	if [ "$sub" = "utils write-kubeconfig" ] && [ -n "$kubeconfig" ]; then
		printf '%s\n' "apiVersion: v1" "kind: Config" "current-context: fake" >"$kubeconfig"
	fi
	exit 0
fi
exit 1
EOF
# kubectl: answers `get pvc/pv <name>` from FAKE_KUBE_STATE ({"pvcs":
# {"<ns>/<name>": doc}, "pvs": {"<name>": doc}}); a missing file is an empty
# cluster, and every miss is a NotFound, as the API server words it. Its
# server is FAKE_KUBE_ENDPOINT, the endpoint the fake aws reports for the run
# cluster, so the harness's target guard passes for exactly that cluster; a
# scenario that points it elsewhere (kube_endpoint) is a kubeconfig the guard
# must refuse. Anything unmodelled exits 99 with "unsupported call", which
# fails the run.
cat >"$fakebin/kubectl" <<'EOF'
#!/usr/bin/env bash
printf 'kubectl %s\n' "$*" >>"$FAKE_AWS_LOG"
ns="" verb="" kind="" name="" out=""
while [ $# -gt 0 ]; do
	case "$1" in
	--kubeconfig) shift ;;
	-n) ns=$2; shift ;;
	-o) out=$2; shift ;;
	config)
		case "${2:-}" in
		current-context) printf 'fake' ;;
		*) printf '%s' "${FAKE_KUBE_ENDPOINT:-https://nowhere.eks.fake}" ;;
		esac
		exit 0 ;;
	get) verb=get; kind=$2; name=${3:-}; shift 2 ;;
	esac
	shift
done
[ "$verb" = get ] && [ -n "$name" ] || { echo "fake kubectl: unsupported call: $*" >&2; exit 99; }
case "$kind" in
pvc) key="$ns/$name"; table=pvcs; plural=persistentvolumeclaims ;;
pv) key="$name"; table=pvs; plural=persistentvolumes ;;
*) echo "fake kubectl: unsupported call: get $kind" >&2; exit 99 ;;
esac
doc=""
if [ -f "${FAKE_KUBE_STATE:-}" ]; then
	doc=$(jq -c --arg k "$key" ".${table}[\$k] // empty" "$FAKE_KUBE_STATE")
fi
if [ -z "$doc" ]; then
	echo "Error from server (NotFound): ${plural} \"${name}\" not found" >&2
	exit 1
fi
case "$out" in
name) echo "${kind}/${name}" ;;
json) printf '%s\n' "$doc" ;;
*) echo "fake kubectl: unsupported call: -o $out" >&2; exit 99 ;;
esac
EOF
chmod +x "$fakebin/aws" "$fakebin/eksctl" "$fakebin/kubectl"

readonly CLUSTER="kubeneuron-e2e-verify"
readonly OTHER="kubeneuron-e2e-other"
readonly STACK="eksctl-${CLUSTER}-cluster"
# The two claims the harness creates, and a well-formed EBS id for the one
# the live-cluster scenarios resolve (the resolver insists on vol-<hex>).
readonly CTRL_CLAIM="kube-neuron/kubeneuron-controller-state"
readonly VM_CLAIM="kubeneuron-monitoring/vmsingle-kubeneuron"
readonly VOL_LIVE="vol-0123456789abcdef0"

# --- running a scenario -------------------------------------------------------
#
# The state is written to a file FIRST and the sweep is run from the current
# shell, never `producer | run_sweep`: a function on the right of a pipe runs
# in a subshell, so its exit-status variable and its failure flag are lost, and
# every "expected non-zero" assertion silently reads the previous value.

# run_sweep <scenario> runs the real sweep with the fakes first on PATH against
# $work/<scenario>/state.json, leaving output.log, calls.log, the mutated
# state.json and the sweep's exit status in rc. Never fails the script itself:
# every scenario's exit status is an assertion, not an accident.
run_sweep() {
	local name="$1" dir="$work/$1" rc
	: >"$dir/calls.log"
	set +e
	env -i PATH="$fakebin:$PATH" HOME="$dir" \
		FAKE_AWS_STATE="$dir/state.json" FAKE_AWS_LOG="$dir/calls.log" \
		AWS_CONFIG_FILE=/dev/null AWS_SHARED_CREDENTIALS_FILE=/dev/null \
		CLUSTER_NAME="$CLUSTER" AWS_REGION=us-east-1 \
		E2E_STATE_DIR="$dir/e2e-state" RUNNER_TEMP="$dir" \
		FAKE_KUBE_STATE="$dir/kube.json" FAKE_KUBE_ENDPOINT="$(cat "$dir/kube-endpoint" 2>/dev/null || printf 'https://%s.eks.fake' "$CLUSTER")" \
		bash hack/hw-e2e.sh sweep >"$dir/output.log" 2>&1
	rc=$?
	set -e
	printf '%s' "$rc" >"$dir/rc"
	if grep -q 'unsupported call' "$dir/output.log"; then
		bad "$name: the sweep made a call the fake does not model:"
		grep 'unsupported call' "$dir/output.log" >&2
	fi
	if ! grep -q '^aws ' "$dir/calls.log"; then
		bad "$name: the fake aws was never called; the real CLI may have been"
	fi
}

# scenario <name> [jq filter] derives the state from the a17 state through the
# filter ($c, $o, $s are bound to the cluster, the other cluster and the exact
# stack) and runs the sweep. A filter that does not compile fails the script.
scenario() {
	local name="$1" filter="${2:-.}" dir="$work/$1"
	mkdir -p "$dir"
	a17_state | jq --arg c "$CLUSTER" --arg o "$OTHER" --arg s "$STACK" "$filter" >"$dir/state.json"
	run_sweep "$name"
}

# kube_state <name> <jq program> seeds the fake kubectl's cluster for a
# scenario ($c, $o, $v bound to the cluster, the other cluster and the live
# volume id); run_record <name> <file> <line> appends a line to one of the
# run's records (run-volumes, unproven-claims) in its state dir. Both go
# before scenario().
kube_state() {
	mkdir -p "$work/$1"
	jq -n --arg c "$CLUSTER" --arg o "$OTHER" --arg v "$VOL_LIVE" "$2" >"$work/$1/kube.json"
}
run_record() {
	mkdir -p "$work/$1/e2e-state"
	printf '%s\n' "$3" >>"$work/$1/e2e-state/$2"
}
# kube_endpoint <name> <url> makes the fake kubectl report that server for the
# scenario, whatever kubeconfig it is handed: the default is the endpoint the
# fake aws reports for the run cluster. run_kubeconfig <name> leaves a
# non-empty kubeconfig at the run's dedicated path before the sweep, as the
# deploy phases do on a run whose teardown then runs on the same runner; the
# sweep must reuse it rather than write one, and must still validate it.
kube_endpoint() {
	mkdir -p "$work/$1"
	printf '%s' "$2" >"$work/$1/kube-endpoint"
}
run_kubeconfig() {
	mkdir -p "$work/$1/e2e-state"
	printf '%s\n' "apiVersion: v1" "kind: Config" "current-context: left-by-deploy" >"$work/$1/e2e-state/kubeconfig"
}
# The controller claim, Bound to a PV that is the EBS CSI volume $VOL_LIVE,
# with claimRef agreeing on namespace, name and uid: the chain tag_run_volume
# walks on a live cluster. The VMSingle claim is deliberately absent, as it is
# on a run that never reached deploy-observability.
ctrl_kube_state='{
  "pvcs": {"kube-neuron/kubeneuron-controller-state": {"metadata": {"uid": "uid-ctrl"}, "spec": {"volumeName": "pvc-ctrl"}, "status": {"phase": "Bound"}}},
  "pvs":  {"pvc-ctrl": {"spec": {"claimRef": {"namespace": "kube-neuron", "name": "kubeneuron-controller-state", "uid": "uid-ctrl"},
                                 "csi": {"driver": "ebs.csi.aws.com", "volumeHandle": $v}}}}
}'
# ...and the volume as the CSI driver leaves it: in use, its created-for tags
# naming that PV and claim, no run identity yet. ($v is bound by kube_state;
# the aws-side copy takes the id literally, below.)
live_volume_tags='{"kubernetes.io/created-for/pv/name": "pvc-ctrl", "kubernetes.io/created-for/pvc/name": "kubeneuron-controller-state", "kubernetes.io/created-for/pvc/namespace": "kube-neuron", "CSIVolumeName": "pvc-ctrl"}'

# scenario_from <name> <state file> runs the sweep over a copy of an existing
# state, typically the end state another scenario left behind.
scenario_from() {
	local name="$1" dir="$work/$1"
	mkdir -p "$dir"
	cp -- "$2" "$dir/state.json"
	run_sweep "$name"
}

# Assertions over one scenario directory.
expect_rc() { # <scenario> <rc>
	local rc
	rc=$(cat "$work/$1/rc")
	[ "$rc" -eq "$2" ] || {
		bad "$1: sweep exited $rc, expected $2"
		sed 's/^/    /' "$work/$1/output.log" >&2
	}
}
expect_output() { # <scenario> <fixed string>
	grep -Fq -- "$2" "$work/$1/output.log" || bad "$1: expected the sweep to log: $2"
}
expect_no_output() { # <scenario> <fixed string>
	if grep -Fq -- "$2" "$work/$1/output.log"; then
		bad "$1: the sweep must not have logged: $2"
	fi
}
expect_called() { # <scenario> <regex>
	grep -Eq -- "$2" "$work/$1/calls.log" || bad "$1: expected an aws call matching: $2"
}
expect_not_called() { # <scenario> <regex>
	if grep -Eq -- "$2" "$work/$1/calls.log"; then
		bad "$1: forbidden aws call was made: $2"
		grep -E -- "$2" "$work/$1/calls.log" | sed 's/^/    /' >&2
	fi
}
expect_call_count() { # <scenario> <regex> <n>
	local n
	n=$(grep -Ec -- "$2" "$work/$1/calls.log" || true)
	[ "$n" -eq "$3" ] || bad "$1: expected $3 aws call(s) matching '$2', saw $n"
}
# expect_call_before <scenario> <regex A> <regex B>: the first call matching A
# was made before the first call matching B. Both must have been made.
expect_call_before() {
	local a b
	a=$(grep -En -- "$2" "$work/$1/calls.log" | head -n1 | cut -d: -f1 || true)
	b=$(grep -En -- "$3" "$work/$1/calls.log" | head -n1 | cut -d: -f1 || true)
	if [ -z "$a" ] || [ -z "$b" ]; then
		bad "$1: expected calls matching both '$2' and '$3' (found: '${a:-none}' and '${b:-none}')"
	elif [ "$a" -ge "$b" ]; then
		bad "$1: expected a call matching '$2' (line $a) before the first matching '$3' (line $b)"
	fi
}
expect_present() { # <scenario> <table> <id>
	jq -e --arg id "$3" ".$2[\$id]" "$work/$1/state.json" >/dev/null ||
		bad "$1: $2/$3 should have been left alone but is gone"
}
expect_absent() { # <scenario> <table> <id>
	if jq -e --arg id "$3" ".$2[\$id]" "$work/$1/state.json" >/dev/null; then
		bad "$1: $2/$3 should have been deleted but still exists"
	fi
}
expect_tag() { # <scenario> <table> <id> <key> <value>
	jq -e --arg id "$3" --arg k "$4" --arg v "$5" ".$2[\$id].tags[\$k] == \$v" "$work/$1/state.json" >/dev/null ||
		bad "$1: $2/$3 should carry $4=$5; tags: $(jq -c --arg id "$3" ".$2[\$id].tags" "$work/$1/state.json")"
}
expect_no_tag() { # <scenario> <table> <id> <key>
	if jq -e --arg id "$3" --arg k "$4" ".$2[\$id].tags | has(\$k)" "$work/$1/state.json" >/dev/null; then
		bad "$1: $2/$3 must not carry $4; tags: $(jq -c --arg id "$3" ".$2[\$id].tags" "$work/$1/state.json")"
	fi
}
expect_state_line() { # <scenario> <file under the run's state dir> <exact line>
	grep -qsxF -- "$3" "$work/$1/e2e-state/$2" || bad "$1: e2e-state/$2 lacks the line '$3' (has: $(tr '\n' '|' <"$work/$1/e2e-state/$2" 2>/dev/null))"
}
expect_no_state_file() { # <scenario> <file under the run's state dir>
	[ ! -e "$work/$1/e2e-state/$2" ] || bad "$1: e2e-state/$2 should not exist (has: $(tr '\n' '|' <"$work/$1/e2e-state/$2"))"
}
# expect_volume_queries_scoped <scenario>: every describe-volumes the sweep
# made was by volume id, by kubeneuron:e2e + this run's tag, or by the exact
# legacy cluster tag — and none by the CSI driver's created-for tags, which
# is what made the old sweep namespace-wide. The log is the proof: a filter
# the fake would happily answer is still forbidden.
expect_volume_queries_scoped() {
	local s="$1" line n=0
	while IFS= read -r line; do
		n=$((n + 1))
		case "$line" in
		*created-for*) bad "$s: a volume query scoped by the CSI created-for tags (namespace-wide): $line" ;;
		*--volume-ids*) ;;
		*"tag:kubeneuron:e2e,Values=true"*"tag:kubeneuron:e2e-run,Values=${CLUSTER}"*) ;;
		*"tag:KubernetesCluster,Values=${CLUSTER}"*) ;;
		*) bad "$s: a volume query not scoped by id, this run's tag or the exact cluster tag: $line" ;;
		esac
	done < <(grep -E '^aws ec2 describe-volumes' "$work/$s/calls.log" || true)
	[ "$n" -gt 0 ] || bad "$s: the sweep never queried volumes at all"
}
# expect_kube_reads_named <scenario>: the only Kubernetes reads were the two
# claims by name and a PV by name — no listing, no other namespace.
expect_kube_reads_named() {
	local stray
	stray=$(grep -E '^kubectl ' "$work/$1/calls.log" |
		grep -vE -- '-n kube-neuron get pvc kubeneuron-controller-state -o (name|json)$' |
		grep -vE -- '-n kubeneuron-monitoring get pvc vmsingle-kubeneuron -o (name|json)$' |
		grep -vE -- ' get pv pvc-[a-z]+ -o json$' |
		grep -vE -- ' config (current-context|view)' || true)
	[ -z "$stray" ] || bad "$1: kubectl read something other than the two named claims and their PV:
$(printf '%s\n' "$stray" | sed 's/^/    /')"
}
# The mutations that cost money or scope: everything the sweep can do to
# EC2, CloudFormation and IAM. ECR image deletion is deliberately NOT in this
# set — the sweep deletes this run's image tags best-effort on every run,
# including a clean one, and that is the documented behaviour, not a leak.
readonly INFRA_MUTATION='(delete-network-interface|delete-security-group|delete-stack|delete-volume|delete-vpc|terminate-instances|delete-role)'
expect_no_infra_mutation() { # <scenario>
	expect_not_called "$1" "$INFRA_MUTATION"
}

# --- the a17 state ------------------------------------------------------------
# Cluster gone, the exact cluster stack DELETE_FAILED, and in its VPC: the
# detached CNI interface of the terminated run node, carrying both the EKS
# cluster group and the stack's shared-node group (VPC-CNI secondary
# interfaces copy the node's groups, and eksctl attaches both to a managed
# node); the EKS cluster group, referenced by a rule in the shared-node group;
# the VPC default group. That is why the group needs the second pass: the
# shared-node group cannot go while the interface holds it, and the cluster
# group cannot go while the shared-node group's rule references it. Next to
# it: another cluster's VPC with the same shapes, and the account's default
# VPC. Nothing outside the run VPC may change.
#
# Two volumes sit in the account throughout, and both must survive every
# scenario: the other run's VMSingle volume, tagged for THAT run; and a
# stranger's volume carrying exactly this stand's namespace and claim name in
# the CSI driver's created-for tags and no run identity at all — what another
# cluster's KubeNeuron install leaves, and what the namespace filter deleted.
a17_state() {
	cat <<EOF
{
  "clusters": [],
  "stacks": {
    "$STACK": {"status": "DELETE_FAILED", "cluster": "$CLUSTER", "vpcs": ["vpc-run"]}
  },
  "vpcs": {
    "vpc-run":     {"default": false, "tags": {"alpha.eksctl.io/cluster-name": "$CLUSTER", "Name": "$STACK/VPC"}},
    "vpc-ambient": {"default": false, "tags": {"alpha.eksctl.io/cluster-name": "$OTHER"}},
    "vpc-default": {"default": true,  "tags": {}}
  },
  "enis": {
    "eni-orphan":  {"vpc": "vpc-run", "status": "available", "attached": false, "requester_managed": false,
                    "interface_type": "interface",
                    "description": "aws-K8S-i-0aaa000000000000a", "groups": ["sg-cluster", "sg-shared"],
                    "tags": {"cluster.k8s.amazonaws.com/name": "$CLUSTER", "node.k8s.amazonaws.com/instance_id": "i-0aaa000000000000a"}},
    "eni-ambient": {"vpc": "vpc-ambient", "status": "available", "attached": false, "requester_managed": false,
                    "description": "aws-K8S-i-0bbb000000000000b", "groups": ["sg-ambient-cluster"],
                    "tags": {"cluster.k8s.amazonaws.com/name": "$OTHER", "node.k8s.amazonaws.com/instance_id": "i-0bbb000000000000b"}}
  },
  "sgs": {
    "sg-cluster":         {"vpc": "vpc-run", "name": "eks-cluster-sg-$CLUSTER-1234", "referenced_by": ["sg-shared"],
                           "tags": {"aws:eks:cluster-name": "$CLUSTER", "kubernetes.io/cluster/$CLUSTER": "owned"}},
    "sg-shared":          {"vpc": "vpc-run", "name": "$STACK-ClusterSharedNodeSecurityGroup-X",
                           "tags": {"alpha.eksctl.io/cluster-name": "$CLUSTER"}},
    "sg-run-default":     {"vpc": "vpc-run", "name": "default", "tags": {}},
    "sg-ambient-cluster": {"vpc": "vpc-ambient", "name": "eks-cluster-sg-$OTHER-1",
                           "tags": {"aws:eks:cluster-name": "$OTHER", "kubernetes.io/cluster/$OTHER": "owned"}},
    "sg-ambient-default": {"vpc": "vpc-ambient", "name": "default", "tags": {}}
  },
  "instances": {
    "i-0aaa000000000000a": {"state": "terminated", "tags": {"aws:eks:cluster-name": "$CLUSTER", "kubeneuron:e2e": "true", "kubeneuron:e2e-run": "$CLUSTER"}},
    "i-0bbb000000000000b": {"state": "terminated", "tags": {"aws:eks:cluster-name": "$OTHER"}}
  },
  "volumes": {
    "vol-other":  {"status": "available", "tags": {"kubeneuron:e2e": "true", "kubeneuron:e2e-run": "$OTHER", "kubeneuron:e2e-claim": "$VM_CLAIM",
                   "kubernetes.io/created-for/pvc/namespace": "kubeneuron-monitoring", "kubernetes.io/created-for/pvc/name": "vmsingle-kubeneuron"}},
    "vol-nsonly": {"status": "available", "tags": {"kubernetes.io/created-for/pvc/namespace": "kube-neuron", "kubernetes.io/created-for/pvc/name": "kubeneuron-controller-state",
                   "kubernetes.io/created-for/pv/name": "pvc-stranger", "CSIVolumeName": "pvc-stranger"}}
  }
}
EOF
}

# What every scenario must leave alone, whatever else it does.
expect_ambient_untouched() {
	local s="$1"
	expect_present "$s" vpcs vpc-ambient
	expect_present "$s" vpcs vpc-default
	expect_present "$s" enis eni-ambient
	expect_present "$s" sgs sg-ambient-cluster
	expect_present "$s" sgs sg-ambient-default
	expect_not_called "$s" 'delete-[a-z-]+ .*(eni-ambient|sg-ambient|vpc-ambient|vpc-default)'
	expect_not_called "$s" 'delete-security-group .*sg-(run-)?default'
	expect_not_called "$s" 'delete-security-group .*sg-shared'
	expect_not_called "$s" 'delete-vpc'
	expect_not_called "$s" 'terminate-instances .*i-0bbb000000000000b'
	expect_present "$s" volumes vol-other
	expect_present "$s" volumes vol-nsonly
	expect_not_called "$s" 'delete-volume .*vol-(other|nsonly)'
	expect_not_called "$s" 'create-tags .*vol-(other|nsonly)'
	expect_volume_queries_scoped "$s"
}

# --- positive: the a17 sequence, two passes -----------------------------------
# Pass 1 removes the interface and TRIES the cluster group, which AWS refuses
# (the shared-node group's rule still references it); the stack retry then
# removes the shared-node group but fails on the VPC because the cluster group
# is still in it. Pass 2 removes the cluster group; the second retry succeeds.
# The stack starts DELETE_FAILED, so the sweep must not spend a retry before
# the first cleanup: exactly two delete-stack calls, and the interface goes
# before the first of them.
note "a17: detached CNI interface, then the cluster group, then the stack"
scenario a17
expect_rc a17 0
expect_output a17 "sweep: $STACK is DELETE_FAILED; clearing networking orphans in its VPC before retrying the delete (pass 1 of 2)"
expect_output a17 "sweep: deleting orphaned VPC-CNI interface eni-orphan (aws-K8S-i-0aaa000000000000a)"
expect_output a17 "sweep: could not delete sg-cluster yet (a stack-owned rule may still reference it); the stack retry decides"
expect_output a17 "(pass 2 of 2)"
expect_output a17 "sweep: deleting orphaned EKS cluster security group sg-cluster"
expect_output a17 "sweep: clean (verified against AWS"
expect_call_count a17 'delete-network-interface .*eni-orphan' 1
expect_call_count a17 'delete-security-group .*sg-cluster' 2
expect_call_count a17 "delete-stack .*$STACK" 2
expect_call_before a17 'delete-network-interface .*eni-orphan' "delete-stack .*$STACK"
expect_called a17 'describe-network-interfaces .*--network-interface-ids eni-orphan'
expect_called a17 'describe-security-groups .*--group-ids sg-cluster'
expect_called a17 'describe-instances .*--instance-ids i-0aaa000000000000a'
expect_absent a17 enis eni-orphan
expect_absent a17 sgs sg-cluster
expect_absent a17 sgs sg-shared
expect_absent a17 vpcs vpc-run
expect_absent a17 stacks "$STACK"
expect_ambient_untouched a17

# --- positive: one pass is enough when nothing references the group ---------
note "one pass: the group has no referencing rule, so the first retry succeeds"
scenario onepass '.sgs["sg-cluster"].referenced_by = [] | del(.sgs["sg-shared"]) | .enis["eni-orphan"].groups = ["sg-cluster"]'
expect_rc onepass 0
expect_output onepass "sweep: clean"
expect_no_output onepass "(pass 2 of 2)"
expect_call_count onepass "delete-stack .*$STACK" 1
expect_call_count onepass 'delete-security-group .*sg-cluster' 1
expect_absent onepass stacks "$STACK"
expect_ambient_untouched onepass

# --- positive: a delete still in progress is waited out, not raced -----------
# eksctl's (or the reaper's) delete is still running. The sweep must not clear
# anything underneath it: the first thing it does to the stack is wait, and
# every mutation comes after that wait has settled into DELETE_FAILED.
note "in progress: wait for the running delete to settle before clearing anything"
scenario inprogress '.stacks[$s].status = "DELETE_IN_PROGRESS"'
expect_rc inprogress 0
expect_output inprogress "sweep: $STACK is DELETE_IN_PROGRESS; waiting for that delete to settle rather than racing it"
expect_output inprogress "sweep: clean"
expect_call_before inprogress 'cloudformation wait stack-delete-complete' "$INFRA_MUTATION"
expect_call_count inprogress "delete-stack .*$STACK" 2
expect_absent inprogress stacks "$STACK"
expect_ambient_untouched inprogress

# --- positive: idempotent -----------------------------------------------------
note "idempotent: a second sweep over the cleaned account mutates nothing"
scenario_from again "$work/a17/state.json"
expect_rc again 0
expect_output again "sweep: clean"
expect_no_infra_mutation again
expect_not_called again 'describe-stack-resources'
expect_ambient_untouched again

# --- positive: an already-clean account never looks for a VPC ----------------
note "clean: no stack means no VPC derivation and no candidate queries"
scenario clean '.stacks = {} | .vpcs = {"vpc-ambient": .vpcs["vpc-ambient"], "vpc-default": .vpcs["vpc-default"]}
	| .enis = {"eni-ambient": .enis["eni-ambient"]}
	| .sgs = {"sg-ambient-cluster": .sgs["sg-ambient-cluster"], "sg-ambient-default": .sgs["sg-ambient-default"]}'
expect_rc clean 0
expect_output clean "sweep: clean"
expect_no_infra_mutation clean
expect_not_called clean 'describe-stack-resources'
expect_not_called clean 'describe-network-interfaces'
expect_not_called clean 'describe-security-groups'
expect_ambient_untouched clean

# --- positive edge: the terminated instance has aged out of the EC2 API ------
note "aged out: the instance is gone, the interface's own cluster tag ties it"
scenario agedout 'del(.instances["i-0aaa000000000000a"])'
expect_rc agedout 0
expect_call_count agedout 'delete-network-interface .*eni-orphan' 1
expect_absent agedout stacks "$STACK"
expect_ambient_untouched agedout

# --- positive edge: a node of this cluster still running -----------------------
# The sweep's instance stage terminates any non-terminated EC2 tagged for
# exactly this cluster before the stacks are touched — that is pre-existing,
# intentional, and it is what makes the node's interface a legitimate orphan
# by the time the networking helper sees it. Recorded here so the near-miss
# scenario below is read correctly: a candidate meant to test the helper's
# refusal of a LIVE instance must not be one the instance stage terminates.
note "leaked node: the instance stage terminates this cluster's node first, then its interface is an orphan"
scenario leakednode '.instances["i-0aaa000000000000a"].state = "running"'
expect_rc leakednode 0
expect_called leakednode 'terminate-instances .*i-0aaa000000000000a'
expect_call_before leakednode 'terminate-instances .*i-0aaa000000000000a' 'delete-network-interface .*eni-orphan'
expect_call_count leakednode 'delete-network-interface .*eni-orphan' 1
expect_absent leakednode stacks "$STACK"
expect_ambient_untouched leakednode

# --- refusal: the EKS cluster still exists ------------------------------------
note "refusal: a live cluster of this name means nothing in the VPC is ours to remove"
scenario livecluster '.clusters = [$c]'
expect_rc livecluster 1
expect_output livecluster "sweep: EKS cluster $CLUSTER still exists; refusing to clear networking orphans"
expect_output livecluster "these still exist after the deletions"
expect_not_called livecluster 'delete-network-interface'
expect_not_called livecluster 'delete-security-group'
expect_not_called livecluster 'delete-stack'
# Live cluster, no kubeconfig, no claims: the sweep writes a kubeconfig, asks
# for exactly the two claims by name, finds neither, tags nothing, records
# nothing — and then retries the delete.
expect_called livecluster "eksctl utils write-kubeconfig --cluster $CLUSTER "
expect_output livecluster "run-volume: claim $CTRL_CLAIM does not exist; nothing to tag"
expect_output livecluster "run-volume: claim $VM_CLAIM does not exist; nothing to tag"
expect_call_before livecluster '^kubectl .*get pvc' 'eksctl delete cluster'
expect_not_called livecluster 'create-tags'
expect_not_called livecluster 'delete-volume'
expect_kube_reads_named livecluster
expect_no_state_file livecluster run-volumes
expect_no_state_file livecluster unproven-claims
expect_present livecluster enis eni-orphan
expect_present livecluster sgs sg-cluster
expect_ambient_untouched livecluster

# --- refusal: the stack's VPC is not tagged for this cluster -----------------
note "refusal: a VPC not tagged for exactly this cluster is out of scope"
scenario mistagged '.vpcs["vpc-run"].tags["alpha.eksctl.io/cluster-name"] = $o'
expect_rc mistagged 1
expect_output mistagged "sweep: vpc-run is not tagged alpha.eksctl.io/cluster-name=$CLUSTER; refusing"
expect_no_infra_mutation mistagged
expect_not_called mistagged 'describe-network-interfaces'
expect_present mistagged enis eni-orphan
expect_present mistagged sgs sg-cluster
expect_present mistagged stacks "$STACK"
expect_ambient_untouched mistagged

# --- refusal: the stack points at more than one VPC --------------------------
note "refusal: an ambiguous VPC scope is no scope"
scenario ambiguous '.stacks[$s].vpcs = ["vpc-run", "vpc-ambient"]'
expect_rc ambiguous 1
expect_output ambiguous "reports more than one VPC"
expect_no_infra_mutation ambiguous
expect_not_called ambiguous 'describe-vpcs'
expect_present ambiguous enis eni-orphan
expect_present ambiguous stacks "$STACK"
expect_ambient_untouched ambiguous

# --- refusal: the stack owns no VPC at all -----------------------------------
note "refusal: a stack without a VPC of its own touches no VPC"
scenario novpc '.stacks[$s].vpcs = [] | .stacks[$s].wedged = true'
expect_rc novpc 1
expect_output novpc "has no VPC resource of its own; refusing to touch any VPC"
expect_no_infra_mutation novpc
expect_not_called novpc 'describe-network-interfaces'
expect_present novpc stacks "$STACK"
expect_ambient_untouched novpc

# --- refusal: the stack's VPC is the account default -------------------------
note "refusal: the default VPC is never in scope, whatever it is tagged"
scenario defaultvpc '.stacks[$s].vpcs = ["vpc-default"] | .stacks[$s].wedged = true
	| .vpcs["vpc-default"].tags["alpha.eksctl.io/cluster-name"] = $c'
expect_rc defaultvpc 1
expect_output defaultvpc "is the account's default VPC (IsDefault=True); refusing"
expect_no_infra_mutation defaultvpc
expect_present defaultvpc vpcs vpc-default
expect_present defaultvpc stacks "$STACK"
expect_ambient_untouched defaultvpc

# --- refusal: candidates that fail revalidation ------------------------------
# All in the run VPC, cluster gone, stack wedged. Only the aged-out, cluster-
# tagged interface is a true orphan; everything else must be left, and named
# in the log with the reason. The ambient resources stay in the state so the
# usual untouched check still applies.
#
# eni-live names a RUNNING instance. That instance is tagged for the other
# cluster on purpose: an instance tagged for this cluster would be terminated
# by the sweep's instance stage before the helper ever ran (see "leaked node"
# above), and the candidate would no longer test what it is here to test — the
# helper's own refusal of an interface whose instance is alive.
#
# eni-mismatch is the regression guard for the field-shift bug: it has NO
# cluster tag and an instance tag naming a different instance from the one in
# its description. Read with a whitespace IFS the empty cluster tag collapsed,
# the instance tag slid into the cluster-tag slot, the mismatch check saw an
# empty instance tag and passed, and the interface was deleted on the strength
# of its terminated, cluster-tagged instance. It must be refused by name.
#
# eni-efa, eni-trunk and eni-untyped are the a17 orphan in every respect the
# sweep otherwise checks — detached, available, not requester-managed, the
# CNI description naming the terminated run node, the cluster tag and the
# instance tag both right, the instance itself tagged for this cluster — and
# differ only in InterfaceType: efa, trunk, and absent from the document. The
# observed orphan was InterfaceType=interface; anything else must be refused
# by that field alone, before any of the matching identifiers can carry it.
note "revalidation: every near-miss in the run VPC is left alone and explained"
scenario nearmiss '
	del(.enis["eni-orphan"]) | del(.sgs["sg-cluster"]) | del(.sgs["sg-shared"])
	| .enis += {
	  "eni-efa":      {"vpc": "vpc-run", "status": "available", "attached": false, "requester_managed": false,
	                   "interface_type": "efa", "description": "aws-K8S-i-0aaa000000000000a", "groups": [],
	                   "tags": {"cluster.k8s.amazonaws.com/name": $c, "node.k8s.amazonaws.com/instance_id": "i-0aaa000000000000a"}},
	  "eni-trunk":    {"vpc": "vpc-run", "status": "available", "attached": false, "requester_managed": false,
	                   "interface_type": "trunk", "description": "aws-K8S-i-0aaa000000000000a", "groups": [],
	                   "tags": {"cluster.k8s.amazonaws.com/name": $c, "node.k8s.amazonaws.com/instance_id": "i-0aaa000000000000a"}},
	  "eni-untyped":  {"vpc": "vpc-run", "status": "available", "attached": false, "requester_managed": false,
	                   "interface_type": null, "description": "aws-K8S-i-0aaa000000000000a", "groups": [],
	                   "tags": {"cluster.k8s.amazonaws.com/name": $c, "node.k8s.amazonaws.com/instance_id": "i-0aaa000000000000a"}},
	  "eni-live":     {"vpc": "vpc-run", "status": "available", "attached": false, "requester_managed": false,
	                   "description": "aws-K8S-i-0ccc000000000000c", "groups": [], "tags": {"cluster.k8s.amazonaws.com/name": $c}},
	  "eni-foreign":  {"vpc": "vpc-run", "status": "available", "attached": false, "requester_managed": false,
	                   "description": "aws-K8S-i-0ddd000000000000d", "groups": [], "tags": {"node.k8s.amazonaws.com/instance_id": "i-0ddd000000000000d"}},
	  "eni-unknown":  {"vpc": "vpc-run", "status": "available", "attached": false, "requester_managed": false,
	                   "description": "aws-K8S-i-0eee000000000000e", "groups": [], "tags": {}},
	  "eni-mismatch": {"vpc": "vpc-run", "status": "available", "attached": false, "requester_managed": false,
	                   "description": "aws-K8S-i-0aaa000000000000a", "groups": [], "tags": {"node.k8s.amazonaws.com/instance_id": "i-0zzz000000000000z"}},
	  "eni-attached": {"vpc": "vpc-run", "status": "in-use", "attached": true, "requester_managed": false,
	                   "description": "aws-K8S-i-0aaa000000000000a", "groups": ["sg-referenced"], "tags": {"cluster.k8s.amazonaws.com/name": $c}},
	  "eni-eks":      {"vpc": "vpc-run", "status": "available", "attached": false, "requester_managed": true,
	                   "description": ("Amazon EKS " + $c), "groups": [], "tags": {}},
	  "eni-aged":     {"vpc": "vpc-run", "status": "available", "attached": false, "requester_managed": false,
	                   "description": "aws-K8S-i-0fff000000000000f", "groups": [], "tags": {"cluster.k8s.amazonaws.com/name": $c}}
	}
	| .sgs += {
	  "sg-referenced":  {"vpc": "vpc-run", "name": ("eks-cluster-sg-" + $c + "-777"),
	                     "tags": {"aws:eks:cluster-name": $c, ("kubernetes.io/cluster/" + $c): "owned"}},
	  "sg-foreign-tag": {"vpc": "vpc-run", "name": ("eks-cluster-sg-" + $c + "-888"),
	                     "tags": {"aws:eks:cluster-name": $o, ("kubernetes.io/cluster/" + $o): "owned"}},
	  "sg-not-owned":   {"vpc": "vpc-run", "name": ("eks-cluster-sg-" + $c + "-999"),
	                     "tags": {"aws:eks:cluster-name": $c}},
	  "sg-wrong-name":  {"vpc": "vpc-run", "name": ("eksctl-" + $c + "-cluster-ControlPlaneSecurityGroup-Y"),
	                     "tags": {"aws:eks:cluster-name": $c, ("kubernetes.io/cluster/" + $c): "owned"}}
	}
	| .instances += {
	  "i-0aaa000000000000a": {"state": "terminated", "tags": {"aws:eks:cluster-name": $c}},
	  "i-0ccc000000000000c": {"state": "running",    "tags": {"aws:eks:cluster-name": $o}},
	  "i-0ddd000000000000d": {"state": "terminated", "tags": {"aws:eks:cluster-name": $o}}
	}'
expect_rc nearmiss 1
expect_not_called nearmiss 'terminate-instances'
expect_call_count nearmiss 'delete-network-interface' 1
expect_called nearmiss 'delete-network-interface .*eni-aged'
expect_absent nearmiss enis eni-aged
expect_not_called nearmiss 'delete-security-group'
expect_output nearmiss "eni-live names i-0ccc000000000000c, which is running, not terminated; leaving it"
expect_output nearmiss "eni-foreign names terminated i-0ddd000000000000d, but neither it nor the interface is tagged for $CLUSTER"
expect_output nearmiss "eni-unknown names i-0eee000000000000e, which no longer exists, and the interface is tagged '' rather than $CLUSTER; leaving it"
expect_output nearmiss "eni-mismatch names i-0aaa000000000000a but is tagged for i-0zzz000000000000z; leaving it"
expect_output nearmiss "eni-efa has InterfaceType 'efa', not 'interface'; leaving it"
expect_output nearmiss "eni-trunk has InterfaceType 'trunk', not 'interface'; leaving it"
expect_output nearmiss "eni-untyped has InterfaceType '', not 'interface'; leaving it"
# The type refusal must be the reason, not a later identifier check: the
# three are the only candidates here naming i-0aaa… that could reach the
# instance lookup (eni-mismatch is refused on its tag first, eni-attached is
# never a candidate), so that lookup must not happen at all.
expect_not_called nearmiss 'describe-instances .*--instance-ids i-0aaa000000000000a'
expect_output nearmiss "sg-referenced is still referenced by network interface(s) eni-attached; leaving it"
expect_output nearmiss "sg-not-owned is tagged aws:eks:cluster-name='$CLUSTER' kubernetes.io/cluster/$CLUSTER=''; not exactly this cluster's; leaving it"
expect_not_called nearmiss 'describe-network-interfaces .*--network-interface-ids eni-attached'
expect_not_called nearmiss 'describe-network-interfaces .*--network-interface-ids eni-eks'
expect_not_called nearmiss 'describe-security-groups .*--group-ids sg-(foreign-tag|wrong-name|run-default)'
for left in eni-live eni-foreign eni-unknown eni-mismatch eni-attached eni-eks eni-efa eni-trunk eni-untyped; do
	expect_present nearmiss enis "$left"
done
for left in sg-referenced sg-foreign-tag sg-not-owned sg-wrong-name sg-run-default; do
	expect_present nearmiss sgs "$left"
done
expect_present nearmiss instances i-0ccc000000000000c
expect_output nearmiss "these still exist after the deletions"
expect_ambient_untouched nearmiss

# =============================================================================
# EBS volumes: this run's identity, nothing wider
# =============================================================================

# --- positive: run-tagged and exact-cluster-tagged volumes go; nothing else --
# vol-half carries kubeneuron:e2e=true without a run tag: not enough. The
# stranger's volume shares our namespace and claim name: never touched, never
# queried for (expect_ambient_untouched checks the query log).
note "volumes: delete by this run's tag and the exact legacy cluster tag only"
scenario volumes '.volumes += {
	"vol-run":    {"status": "available", "tags": {"kubeneuron:e2e": "true", "kubeneuron:e2e-run": $c, "kubeneuron:e2e-claim": "kube-neuron/kubeneuron-controller-state",
	               "kubernetes.io/created-for/pvc/namespace": "kube-neuron", "kubernetes.io/created-for/pvc/name": "kubeneuron-controller-state"}},
	"vol-vm":     {"status": "available", "tags": {"kubeneuron:e2e": "true", "kubeneuron:e2e-run": $c, "kubeneuron:e2e-claim": "kubeneuron-monitoring/vmsingle-kubeneuron",
	               "kubernetes.io/created-for/pvc/namespace": "kubeneuron-monitoring", "kubernetes.io/created-for/pvc/name": "vmsingle-kubeneuron"}},
	"vol-legacy": {"status": "available", "tags": {"KubernetesCluster": $c}},
	"vol-half":   {"status": "available", "tags": {"kubeneuron:e2e": "true", "kubernetes.io/created-for/pvc/namespace": "kube-neuron"}}
}'
expect_rc volumes 0
expect_output volumes "sweep: clean"
expect_call_count volumes 'delete-volume' 3
expect_absent volumes volumes vol-run
expect_absent volumes volumes vol-vm
expect_absent volumes volumes vol-legacy
expect_present volumes volumes vol-half
expect_not_called volumes 'delete-volume .*vol-half'
expect_ambient_untouched volumes

# --- positive: run records are read for this cluster only --------------------
# vol-gone was recorded and is already deleted; vol-other is recorded by the
# OTHER cluster and must not even be looked up under this one.
note "recorded: this run's records are checked by id; another cluster's are ignored"
run_record recorded run-volumes "$CLUSTER vol-run $CTRL_CLAIM"
run_record recorded run-volumes "$CLUSTER vol-gone $VM_CLAIM"
run_record recorded run-volumes "$OTHER vol-other $VM_CLAIM"
scenario recorded '.volumes += {"vol-run": {"status": "available", "tags": {"kubeneuron:e2e": "true", "kubeneuron:e2e-run": $c, "kubeneuron:e2e-claim": "kube-neuron/kubeneuron-controller-state"}}}'
expect_rc recorded 0
expect_output recorded "sweep: clean"
expect_absent recorded volumes vol-run
expect_called recorded 'describe-volumes .*--volume-ids vol-gone'
expect_not_called recorded 'describe-volumes .*--volume-ids vol-other'
expect_ambient_untouched recorded

# --- refusal: a recorded volume that lost its run tag is named, not deleted --
note "recorded-untagged: a recorded volume without the run tag is reported by name and left"
run_record untagged run-volumes "$CLUSTER vol-stripped $CTRL_CLAIM"
scenario untagged '.volumes += {"vol-stripped": {"status": "available", "tags": {"kubernetes.io/created-for/pvc/namespace": "kube-neuron", "kubernetes.io/created-for/pvc/name": "kubeneuron-controller-state"}}}'
expect_rc untagged 1
expect_output untagged "sweep: recorded volume vol-stripped ($CTRL_CLAIM) still exists but no longer carries kubeneuron:e2e-run=$CLUSTER; refusing to delete it"
expect_output untagged "volumes (recorded for this run, no longer run-tagged): vol-stripped"
expect_output untagged "these still exist after the deletions"
expect_not_called untagged 'delete-volume'
expect_present untagged volumes vol-stripped
expect_absent untagged stacks "$STACK"
expect_ambient_untouched untagged

# --- refusal: a claim never tied to this run keeps the sweep from "clean" ----
# The rest of the cleanup still runs (the stack goes); only the verdict is
# withheld, with the record and how to clear it in the message.
note "unproven: a claim recorded as unproven fails the sweep by name; another cluster's record does not"
run_record unproven unproven-claims "$CLUSTER $VM_CLAIM the claim is 'Pending', not Bound"
run_record unproven unproven-claims "$OTHER $CTRL_CLAIM the claim could not be read"
scenario unproven
expect_rc unproven 1
expect_output unproven "claims never tied to this run (find the volume by hand, then remove $work/unproven/e2e-state/unproven-claims): $VM_CLAIM the claim is 'Pending', not Bound"
expect_no_output unproven "the claim could not be read"
expect_output unproven "these still exist after the deletions"
expect_not_called unproven 'delete-volume'
expect_absent unproven stacks "$STACK"
expect_ambient_untouched unproven

note "unproven-other: only another cluster's unproven record present; this sweep is clean"
run_record unprovenother unproven-claims "$OTHER $CTRL_CLAIM the claim could not be read"
scenario unprovenother
expect_rc unprovenother 0
expect_output unprovenother "sweep: clean"
expect_ambient_untouched unprovenother

# --- positive: a live cluster's claim is resolved and tagged before delete ---
# The cluster is still up (teardown's or the reaper's delete failed), no
# kubeconfig survived, the controller claim is Bound to an EBS CSI PV, and
# the VMSingle claim never existed. The sweep must write a kubeconfig, read
# exactly those two claims by name, walk PVC -> PV -> handle, find the
# volume's created-for tags agreeing, tag it with this run's identity, record
# it, and only then retry the delete. The volume is in use and the cluster is
# live, so nothing is deleted and the sweep ends unclean, as it must.
note "livetag: a live cluster's controller claim is tied to this run before the delete retry"
kube_state livetag "$ctrl_kube_state"
scenario livetag '.clusters = [$c] | .volumes += {"'"$VOL_LIVE"'": {"status": "in-use", "tags": '"$live_volume_tags"'}}'
expect_rc livetag 1
expect_called livetag "eksctl utils write-kubeconfig --cluster $CLUSTER "
expect_output livetag "run-volume: claim $CTRL_CLAIM is EBS volume $VOL_LIVE (PV pvc-ctrl); tagged kubeneuron:e2e-run=$CLUSTER"
expect_output livetag "run-volume: claim $VM_CLAIM does not exist; nothing to tag"
expect_call_count livetag "ec2 create-tags --region us-east-1 --resources $VOL_LIVE --tags Key=kubeneuron:e2e,Value=true Key=kubeneuron:e2e-run,Value=$CLUSTER Key=kubeneuron:e2e-claim,Value=$CTRL_CLAIM\$" 1
expect_call_before livetag "^kubectl .*get pvc kubeneuron-controller-state -o json" "ec2 describe-volumes .*--volume-ids $VOL_LIVE"
expect_call_before livetag "ec2 describe-volumes .*--volume-ids $VOL_LIVE" 'ec2 create-tags'
expect_call_before livetag 'ec2 create-tags' 'eksctl delete cluster'
expect_tag livetag volumes "$VOL_LIVE" kubeneuron:e2e true
expect_tag livetag volumes "$VOL_LIVE" kubeneuron:e2e-run "$CLUSTER"
expect_tag livetag volumes "$VOL_LIVE" kubeneuron:e2e-claim "$CTRL_CLAIM"
expect_state_line livetag run-volumes "$CLUSTER $VOL_LIVE $CTRL_CLAIM"
expect_no_state_file livetag unproven-claims
expect_kube_reads_named livetag
expect_not_called livetag 'delete-volume'
expect_present livetag volumes "$VOL_LIVE"
expect_output livetag "these still exist after the deletions"
expect_ambient_untouched livetag

# The same live cluster, but the controller claim is Bound to a legacy
# in-tree PV: resolved through .spec.awsElasticBlockStore, the volume's
# exact KubernetesCluster tag and created-for tags, then tagged and recorded
# like the CSI one, with the same by-id query and no wider lookup.
note "legacylivetag: a live cluster's legacy in-tree claim is tied to this run before the delete retry"
kube_state legacylivetag "$ctrl_kube_state"' | .pvs["pvc-ctrl"].spec |= (del(.csi) | .awsElasticBlockStore = {volumeID: ("aws://us-east-1a/" + $v)})'
scenario legacylivetag '.clusters = [$c] | .volumes += {"'"$VOL_LIVE"'": {"status": "in-use", "tags": ('"$live_volume_tags"' + {"KubernetesCluster": $c})}}'
expect_rc legacylivetag 1
expect_output legacylivetag "run-volume: PV pvc-ctrl is a legacy in-tree awsElasticBlockStore volume; KubernetesCluster=$CLUSTER on $VOL_LIVE proves its cluster"
expect_output legacylivetag "run-volume: claim $CTRL_CLAIM is EBS volume $VOL_LIVE (PV pvc-ctrl); tagged kubeneuron:e2e-run=$CLUSTER"
expect_output legacylivetag "run-volume: claim $VM_CLAIM does not exist; nothing to tag"
expect_call_count legacylivetag "ec2 create-tags --region us-east-1 --resources $VOL_LIVE --tags Key=kubeneuron:e2e,Value=true Key=kubeneuron:e2e-run,Value=$CLUSTER Key=kubeneuron:e2e-claim,Value=$CTRL_CLAIM\$" 1
expect_call_before legacylivetag "ec2 describe-volumes .*--volume-ids $VOL_LIVE" 'ec2 create-tags'
expect_call_before legacylivetag 'ec2 create-tags' 'eksctl delete cluster'
expect_tag legacylivetag volumes "$VOL_LIVE" kubeneuron:e2e true
expect_tag legacylivetag volumes "$VOL_LIVE" kubeneuron:e2e-run "$CLUSTER"
expect_tag legacylivetag volumes "$VOL_LIVE" kubeneuron:e2e-claim "$CTRL_CLAIM"
expect_state_line legacylivetag run-volumes "$CLUSTER $VOL_LIVE $CTRL_CLAIM"
expect_no_state_file legacylivetag unproven-claims
expect_kube_reads_named legacylivetag
expect_not_called legacylivetag 'delete-volume'
expect_present legacylivetag volumes "$VOL_LIVE"
expect_output legacylivetag "these still exist after the deletions"
expect_ambient_untouched legacylivetag

# ...and the same legacy PV whose volume is tagged for another cluster is
# refused on that tag, recorded, and left untagged.
note "legacylivewrong: a live legacy claim whose volume names another cluster is refused and recorded"
kube_state legacylivewrong "$ctrl_kube_state"' | .pvs["pvc-ctrl"].spec |= (del(.csi) | .awsElasticBlockStore = {volumeID: ("aws://us-east-1a/" + $v)})'
scenario legacylivewrong '.clusters = [$c] | .volumes += {"'"$VOL_LIVE"'": {"status": "in-use", "tags": ('"$live_volume_tags"' + {"KubernetesCluster": $o})}}'
expect_rc legacylivewrong 1
expect_output legacylivewrong "cannot tie the volume behind claim $CTRL_CLAIM to run $CLUSTER: EBS volume $VOL_LIVE is tagged KubernetesCluster='$OTHER', not $CLUSTER; a legacy in-tree volume must name this cluster"
expect_state_line legacylivewrong unproven-claims "$CLUSTER $CTRL_CLAIM EBS volume $VOL_LIVE is tagged KubernetesCluster='$OTHER', not $CLUSTER; a legacy in-tree volume must name this cluster"
expect_no_state_file legacylivewrong run-volumes
expect_called legacylivewrong "describe-volumes .*--volume-ids $VOL_LIVE"
expect_not_called legacylivewrong 'create-tags'
expect_not_called legacylivewrong 'delete-volume'
expect_no_tag legacylivewrong volumes "$VOL_LIVE" kubeneuron:e2e
expect_no_tag legacylivewrong volumes "$VOL_LIVE" kubeneuron:e2e-run
expect_no_tag legacylivewrong volumes "$VOL_LIVE" kubeneuron:e2e-claim
expect_called legacylivewrong 'eksctl delete cluster'
expect_kube_reads_named legacylivewrong
expect_present legacylivewrong volumes "$VOL_LIVE"
expect_ambient_untouched legacylivewrong

# --- refusal: a live claim that cannot be proven is recorded, left, reported -
# Same cluster, but the PV's claimRef carries a stale uid: the claim was
# re-created and this PV is someone else's history. No tag, no delete, an
# unproven record — and the delete retry still happens, with the record then
# withholding "clean".
note "liverefused: a live claim whose PV does not point back at it is refused, recorded, and still reported"
kube_state liverefused "$ctrl_kube_state"' | .pvs["pvc-ctrl"].spec.claimRef.uid = "uid-stale"'
scenario liverefused '.clusters = [$c] | .volumes += {"'"$VOL_LIVE"'": {"status": "in-use", "tags": '"$live_volume_tags"'}}'
expect_rc liverefused 1
expect_output liverefused "cannot tie the volume behind claim $CTRL_CLAIM to run $CLUSTER: PersistentVolume pvc-ctrl is claimed by kube-neuron/kubeneuron-controller-state uid uid-stale, not by this claim"
expect_output liverefused "run-volume: claim $CTRL_CLAIM could not be tied to this run; recorded as unproven, and the sweep will refuse to report clean"
expect_output liverefused "claims never tied to this run (find the volume by hand, then remove $work/liverefused/e2e-state/unproven-claims): $CTRL_CLAIM PersistentVolume pvc-ctrl is claimed by"
expect_state_line liverefused unproven-claims "$CLUSTER $CTRL_CLAIM PersistentVolume pvc-ctrl is claimed by kube-neuron/kubeneuron-controller-state uid uid-stale, not by this claim"
expect_no_state_file liverefused run-volumes
expect_not_called liverefused 'create-tags'
expect_not_called liverefused 'delete-volume'
expect_not_called liverefused "describe-volumes .*--volume-ids $VOL_LIVE"
expect_called liverefused 'eksctl delete cluster'
expect_kube_reads_named liverefused
expect_present liverefused volumes "$VOL_LIVE"
expect_ambient_untouched liverefused

# --- refusal: the kubeconfig does not target the live cluster -----------------
# The same live cluster and the same Bound, tag-ready claim as livetag, but
# the kubeconfig the sweep obtains points at ANOTHER cluster's API server:
# write-kubeconfig wrote it, the fake kubectl reports the other cluster's
# endpoint as its server, and the EKS endpoint for this cluster differs. The
# target guard must refuse before the first claim lookup: no kubectl read of
# any claim, no describe-volumes by id, no create-tags, no run-volumes
# record, both known claims recorded as unproven with the guard's own reason,
# the cluster delete still retried, and the sweep reporting the claims
# instead of clean. The volume keeps no run identity: a tag written through
# an unvalidated context is exactly the licence to delete the wrong volume.
wrong_target_reason="the dedicated kubeconfig was not proven to target the live cluster $CLUSTER before deletion: refusing kubectl against fake (https://$OTHER.eks.fake); expected EKS endpoint for $CLUSTER"
expect_wrong_target() { # <scenario>
	local s="$1"
	expect_rc "$s" 1
	expect_output "$s" "run-volume: the dedicated kubeconfig at $work/$s/e2e-state/kubeconfig is not proven to target the live cluster $CLUSTER; no claim is looked up and no volume is tagged on an unvalidated context"
	expect_output "$s" "cannot tie the volume behind claim $CTRL_CLAIM to run $CLUSTER: $wrong_target_reason"
	expect_output "$s" "cannot tie the volume behind claim $VM_CLAIM to run $CLUSTER: $wrong_target_reason"
	expect_state_line "$s" unproven-claims "$CLUSTER $CTRL_CLAIM $wrong_target_reason"
	expect_state_line "$s" unproven-claims "$CLUSTER $VM_CLAIM $wrong_target_reason"
	expect_no_state_file "$s" run-volumes
	expect_output "$s" "claims never tied to this run (find the volume by hand, then remove $work/$s/e2e-state/unproven-claims): $CTRL_CLAIM $wrong_target_reason;$VM_CLAIM $wrong_target_reason"
	expect_no_output "$s" "does not exist; nothing to tag"
	expect_no_output "$s" "could not be listed before deletion"
	expect_called "$s" '^kubectl --kubeconfig .* config (current-context|view)'
	expect_not_called "$s" '^kubectl .* get (pvc|pv) '
	expect_not_called "$s" "describe-volumes .*--volume-ids"
	expect_not_called "$s" 'create-tags'
	expect_not_called "$s" 'delete-volume'
	expect_call_before "$s" '^kubectl --kubeconfig .* config ' 'eksctl delete cluster'
	expect_called "$s" 'eksctl delete cluster'
	expect_no_tag "$s" volumes "$VOL_LIVE" kubeneuron:e2e
	expect_no_tag "$s" volumes "$VOL_LIVE" kubeneuron:e2e-run
	expect_no_tag "$s" volumes "$VOL_LIVE" kubeneuron:e2e-claim
	expect_tag "$s" volumes "$VOL_LIVE" kubernetes.io/created-for/pvc/name kubeneuron-controller-state
	expect_present "$s" volumes "$VOL_LIVE"
	expect_volume_queries_scoped "$s"
	expect_output "$s" "these still exist after the deletions"
	expect_ambient_untouched "$s"
}
note "wrongtarget: a kubeconfig written for the live cluster that targets another API server tags nothing and records every claim"
kube_state wrongtarget "$ctrl_kube_state"
kube_endpoint wrongtarget "https://$OTHER.eks.fake"
scenario wrongtarget '.clusters = [$c] | .volumes += {"'"$VOL_LIVE"'": {"status": "in-use", "tags": '"$live_volume_tags"'}}'
expect_called wrongtarget "eksctl utils write-kubeconfig --cluster $CLUSTER "
expect_call_before wrongtarget "eksctl utils write-kubeconfig" '^kubectl --kubeconfig .* config '
expect_wrong_target wrongtarget

# The kubeconfig an earlier phase left at the run's path is reused, not
# rewritten, and is held to the same proof: left by a deploy against some
# other cluster (or edited since), it fails the guard the same way, with
# nothing read and nothing tagged.
note "staletarget: a kubeconfig left at the run's path that targets another API server is reused, refused, and tags nothing"
kube_state staletarget "$ctrl_kube_state"
kube_endpoint staletarget "https://$OTHER.eks.fake"
run_kubeconfig staletarget
scenario staletarget '.clusters = [$c] | .volumes += {"'"$VOL_LIVE"'": {"status": "in-use", "tags": '"$live_volume_tags"'}}'
expect_not_called staletarget 'eksctl utils write-kubeconfig'
expect_no_output staletarget "writing one for the live cluster"
expect_wrong_target staletarget

# ...and the same reused kubeconfig, when it DOES target this cluster, is
# validated and then used: the claim is resolved and tagged exactly as in
# livetag, with no kubeconfig written and the guard's reads before the first
# claim lookup.
note "reusedtarget: a kubeconfig left at the run's path that targets this cluster is validated, reused, and the claim tagged"
kube_state reusedtarget "$ctrl_kube_state"
run_kubeconfig reusedtarget
scenario reusedtarget '.clusters = [$c] | .volumes += {"'"$VOL_LIVE"'": {"status": "in-use", "tags": '"$live_volume_tags"'}}'
expect_rc reusedtarget 1
expect_not_called reusedtarget 'eksctl utils write-kubeconfig'
expect_no_output reusedtarget "is not proven to target the live cluster"
expect_output reusedtarget "run-volume: claim $CTRL_CLAIM is EBS volume $VOL_LIVE (PV pvc-ctrl); tagged kubeneuron:e2e-run=$CLUSTER"
expect_output reusedtarget "run-volume: claim $VM_CLAIM does not exist; nothing to tag"
expect_call_before reusedtarget '^kubectl --kubeconfig .* config ' '^kubectl .* get pvc'
expect_call_before reusedtarget '^kubectl .* get pvc' 'ec2 create-tags'
expect_call_before reusedtarget 'ec2 create-tags' 'eksctl delete cluster'
expect_call_count reusedtarget "ec2 create-tags --region us-east-1 --resources $VOL_LIVE --tags Key=kubeneuron:e2e,Value=true Key=kubeneuron:e2e-run,Value=$CLUSTER Key=kubeneuron:e2e-claim,Value=$CTRL_CLAIM\$" 1
expect_tag reusedtarget volumes "$VOL_LIVE" kubeneuron:e2e-run "$CLUSTER"
expect_tag reusedtarget volumes "$VOL_LIVE" kubeneuron:e2e-claim "$CTRL_CLAIM"
expect_state_line reusedtarget run-volumes "$CLUSTER $VOL_LIVE $CTRL_CLAIM"
expect_no_state_file reusedtarget unproven-claims
expect_kube_reads_named reusedtarget
expect_volume_queries_scoped reusedtarget
expect_not_called reusedtarget 'delete-volume'
expect_present reusedtarget volumes "$VOL_LIVE"
expect_ambient_untouched reusedtarget

# --- the resolution chain, link by link ----------------------------------------
# tag_run_volume itself, extracted from the script and run against the fake
# kubectl and the fake aws, once per link that must refuse. The kubectl
# wrapper is replaced by the plain fake: the target guard is exercised by the
# live scenarios above; this is about the chain. The constants are the
# script's own readonly lines, so a renamed tag here is a renamed tag there.
note "resolution chain: every link refuses on its own, records the claim, and never tags"
fn_body() {
	awk -v fn="$1" '$0 ~ "^"fn"\\(\\) \\{" {p=1} p {print} p && /^\}/ {exit}' hack/hw-e2e.sh
}
tag_helper="$work/tag-helper.sh"
{
	echo 'set -Eeuo pipefail'
	grep -E '^readonly (E2E_RUN_TAG|E2E_CLAIM_TAG|EBS_CSI_DRIVER|CSI_PV_NAME_TAG|CSI_PVC_NAME_TAG|CSI_PVC_NAMESPACE_TAG|LEGACY_CLUSTER_TAG|RUN_VOLUMES_FILE|UNPROVEN_CLAIMS_FILE)=' hack/hw-e2e.sh
	printf '%s\n' 'log() { printf "=== %s\n" "$*" >&2; }' 'die() { printf "hw-e2e: %s\n" "$*" >&2; exit 1; }'
	echo 'kubectl() { command kubectl "$@"; }'
	fn_body claim_unproven
	fn_body volume_tag
	fn_body tag_run_volume
	echo 'tag_run_volume kube-neuron kubeneuron-controller-state'
} >"$tag_helper"
[ "$(grep -c '^readonly ' "$tag_helper")" -eq 9 ] || bad "resolution chain: the script no longer defines all nine run-volume constants"
[ "$(sed -nE 's/^readonly LEGACY_CLUSTER_TAG="([^"]*)"$/\1/p' "$tag_helper")" = KubernetesCluster ] ||
	bad "resolution chain: LEGACY_CLUSTER_TAG is not the KubernetesCluster literal the sweep filters on"
[ -n "$(fn_body tag_run_volume)" ] || bad "resolution chain: tag_run_volume is not defined in the script"

# tagcase <name> <kube jq> <aws jq> <OK|REFUSE> [needle]: run the helper once
# over the live fixture with the two mutations applied; a refusal must exit
# non-zero, name the reason, write the unproven record and no run-volumes
# record; a success must tag, read back, record, and refuse nothing.
tagcase() {
	local name="$1" kube_jq="$2" aws_jq="$3" want="$4" needle="${5:-}" dir="$work/tag.$1" rc
	mkdir -p "$dir/e2e-state"
	jq -n --arg c "$CLUSTER" --arg o "$OTHER" --arg v "$VOL_LIVE" "$ctrl_kube_state | $kube_jq" >"$dir/kube.json"
	jq -n --arg c "$CLUSTER" --arg o "$OTHER" --arg v "$VOL_LIVE" --argjson t "$live_volume_tags" \
		'{clusters: [$c], stacks: {}, vpcs: {}, enis: {}, sgs: {}, instances: {}, volumes: {($v): {status: "in-use", tags: $t}}} | '"$aws_jq" >"$dir/state.json"
	: >"$dir/calls.log"
	set +e
	env -i PATH="$fakebin:$PATH" HOME="$dir" \
		FAKE_AWS_STATE="$dir/state.json" FAKE_AWS_LOG="$dir/calls.log" FAKE_KUBE_STATE="$dir/kube.json" \
		AWS_CONFIG_FILE=/dev/null AWS_SHARED_CREDENTIALS_FILE=/dev/null \
		CLUSTER_NAME="$CLUSTER" AWS_REGION=us-east-1 E2E_STATE_DIR="$dir/e2e-state" \
		timeout 60 bash "$tag_helper" >"$dir/output.log" 2>&1
	rc=$?
	set -e
	if grep -q 'unsupported call' "$dir/output.log"; then
		bad "tag.$name: the helper made a call a fake does not model: $(grep 'unsupported call' "$dir/output.log")"
	fi
	if [ "$want" = OK ]; then
		[ "$rc" -eq 0 ] || bad "tag.$name: tag_run_volume exited $rc, expected 0: $(tr '\n' ' ' <"$dir/output.log")"
		grep -Fq -- "run-volume: claim $CTRL_CLAIM is EBS volume $VOL_LIVE (PV pvc-ctrl); tagged kubeneuron:e2e-run=$CLUSTER" "$dir/output.log" ||
			bad "tag.$name: no success line: $(tr '\n' ' ' <"$dir/output.log")"
		jq -e --arg v "$VOL_LIVE" --arg c "$CLUSTER" '.volumes[$v].tags["kubeneuron:e2e"] == "true" and .volumes[$v].tags["kubeneuron:e2e-run"] == $c and .volumes[$v].tags["kubeneuron:e2e-claim"] == "kube-neuron/kubeneuron-controller-state"' "$dir/state.json" >/dev/null ||
			bad "tag.$name: the volume does not carry the three run tags: $(jq -c --arg v "$VOL_LIVE" '.volumes[$v].tags' "$dir/state.json")"
		grep -qsxF "$CLUSTER $VOL_LIVE $CTRL_CLAIM" "$dir/e2e-state/run-volumes" || bad "tag.$name: no run-volumes record"
		[ ! -e "$dir/e2e-state/unproven-claims" ] || bad "tag.$name: an unproven record was written on success: $(cat "$dir/e2e-state/unproven-claims")"
	else
		[ "$rc" -ne 0 ] || bad "tag.$name: tag_run_volume exited 0, expected a refusal"
		grep -Fq -- "cannot tie the volume behind claim $CTRL_CLAIM to run $CLUSTER: $needle" "$dir/output.log" ||
			bad "tag.$name: refusal does not say '$needle': $(tr '\n' ' ' <"$dir/output.log")"
		grep -qs -- "^$CLUSTER $CTRL_CLAIM " "$dir/e2e-state/unproven-claims" || bad "tag.$name: no unproven record"
		grep -qsF -- "$needle" "$dir/e2e-state/unproven-claims" || bad "tag.$name: the unproven record lacks the reason '$needle'"
		[ ! -e "$dir/e2e-state/run-volumes" ] || bad "tag.$name: a run-volumes record was written on refusal: $(cat "$dir/e2e-state/run-volumes")"
		jq -e --arg v "$VOL_LIVE" '.volumes[$v].tags["kubeneuron:e2e-run"] // "" | . == "" or . == "kubeneuron-e2e-other"' "$dir/state.json" >/dev/null ||
			bad "tag.$name: the volume was tagged for this run despite the refusal"
	fi
	! grep -q 'delete-volume' "$dir/calls.log" || bad "tag.$name: the resolver deleted something"
}
tag_called() { grep -Eq -- "$2" "$work/tag.$1/calls.log" || bad "tag.$1: expected a call matching: $2"; }
tag_not_called() { ! grep -Eq -- "$2" "$work/tag.$1/calls.log" || bad "tag.$1: forbidden call was made: $2"; }

tagcase ok '.' '.' OK
tag_called ok "ec2 create-tags --region us-east-1 --resources $VOL_LIVE --tags Key=kubeneuron:e2e,Value=true Key=kubeneuron:e2e-run,Value=$CLUSTER Key=kubeneuron:e2e-claim,Value=$CTRL_CLAIM\$"
tag_called ok "ec2 describe-volumes --region us-east-1 --volume-ids $VOL_LIVE --filters Name=tag:kubeneuron:e2e,Values=true Name=tag:kubeneuron:e2e-run,Values=$CLUSTER Name=tag:kubeneuron:e2e-claim,Values=$CTRL_CLAIM "
# Idempotent: already ours is fine, and the record is not duplicated.
tagcase again '.' '.volumes[$v].tags += {"kubeneuron:e2e": "true", "kubeneuron:e2e-run": $c, "kubeneuron:e2e-claim": "kube-neuron/kubeneuron-controller-state"}' OK
set +e
env -i PATH="$fakebin:$PATH" HOME="$work/tag.again" \
	FAKE_AWS_STATE="$work/tag.again/state.json" FAKE_AWS_LOG="$work/tag.again/calls.log" FAKE_KUBE_STATE="$work/tag.again/kube.json" \
	AWS_CONFIG_FILE=/dev/null AWS_SHARED_CREDENTIALS_FILE=/dev/null \
	CLUSTER_NAME="$CLUSTER" AWS_REGION=us-east-1 E2E_STATE_DIR="$work/tag.again/e2e-state" \
	timeout 60 bash "$tag_helper" >>"$work/tag.again/output.log" 2>&1
again_rc=$?
set -e
[ "$again_rc" -eq 0 ] || bad "tag.again: a second resolution of the same claim exited $again_rc"
[ "$(wc -l <"$work/tag.again/e2e-state/run-volumes")" -eq 1 ] || bad "tag.again: the run-volumes record was duplicated: $(tr '\n' '|' <"$work/tag.again/e2e-state/run-volumes")"
# Kubernetes-side refusals: nothing is asked of AWS at all.
tagcase pending '.pvcs["kube-neuron/kubeneuron-controller-state"].status.phase = "Pending"' '.' REFUSE "the claim is 'Pending', not Bound"
tag_not_called pending 'describe-volumes'
tagcase nopv '.pvcs["kube-neuron/kubeneuron-controller-state"].spec.volumeName = ""' '.' REFUSE "the claim has no uid or no volumeName"
tagcase pvgone '.pvcs["kube-neuron/kubeneuron-controller-state"].spec.volumeName = "pvc-elsewhere"' '.' REFUSE "its PersistentVolume pvc-elsewhere could not be read"
tagcase uid '.pvs["pvc-ctrl"].spec.claimRef.uid = "uid-stale"' '.' REFUSE "PersistentVolume pvc-ctrl is claimed by kube-neuron/kubeneuron-controller-state uid uid-stale, not by this claim"
tagcase otherns '.pvs["pvc-ctrl"].spec.claimRef.namespace = "other-ns"' '.' REFUSE "PersistentVolume pvc-ctrl is claimed by other-ns/kubeneuron-controller-state uid uid-ctrl, not by this claim"
tagcase driver '.pvs["pvc-ctrl"].spec.csi.driver = "efs.csi.aws.com"' '.' REFUSE "PersistentVolume pvc-ctrl is provisioned by 'efs.csi.aws.com', not ebs.csi.aws.com"
tagcase handle '.pvs["pvc-ctrl"].spec.csi.volumeHandle = "fs-0123456789abcdef0"' '.' REFUSE "PersistentVolume pvc-ctrl has CSI volumeHandle 'fs-0123456789abcdef0', not an EBS volume id"
tagcase csistring '.pvs["pvc-ctrl"].spec.csi = "ebs.csi.aws.com"' '.' REFUSE "PersistentVolume pvc-ctrl is provisioned by '', not ebs.csi.aws.com"
# The PV source is decided by what the spec sets, never by what it lacks: no
# EBS source, a foreign source, or both EBS sources at once (well-formed or
# not) is refused before any AWS call.
tagcase nosource '.pvs["pvc-ctrl"].spec |= del(.csi)' '.' REFUSE "PersistentVolume pvc-ctrl has neither a csi nor an awsElasticBlockStore source; not a volume this harness can name"
tagcase nfs '.pvs["pvc-ctrl"].spec |= (del(.csi) | .nfs = {server: "10.0.0.1", path: "/"})' '.' REFUSE "PersistentVolume pvc-ctrl has neither a csi nor an awsElasticBlockStore source; not a volume this harness can name"
tagcase dual '.pvs["pvc-ctrl"].spec.awsElasticBlockStore = {volumeID: ("aws://us-east-1a/" + $v)}' '.' REFUSE "PersistentVolume pvc-ctrl has more than one volume source (csi+awsElasticBlockStore); refusing to pick one"
tagcase dualmalformed '.pvs["pvc-ctrl"].spec |= (.csi = "garbage" | .awsElasticBlockStore = {volumeID: ("aws://us-east-1a/" + $v)})' '.' REFUSE "PersistentVolume pvc-ctrl has more than one volume source (csi+awsElasticBlockStore); refusing to pick one"
# Legacy in-tree PV: only the two documented volumeID forms are accepted, and
# only the anchored vol- id is handed to aws. Anything else is refused on the
# Kubernetes side.
legacy_pv() { printf '.pvs["pvc-ctrl"].spec |= (del(.csi) | .awsElasticBlockStore = %s)' "$1"; }
tagcase legacyzone "$(legacy_pv '{volumeID: ("aws://us-east-1a/" + $v)}')" '.volumes[$v].tags.KubernetesCluster = $c' OK
tag_called legacyzone "ec2 describe-volumes --region us-east-1 --volume-ids $VOL_LIVE --output json\$"
tag_called legacyzone "ec2 create-tags --region us-east-1 --resources $VOL_LIVE --tags Key=kubeneuron:e2e,Value=true Key=kubeneuron:e2e-run,Value=$CLUSTER Key=kubeneuron:e2e-claim,Value=$CTRL_CLAIM\$"
grep -Fq -- "run-volume: PV pvc-ctrl is a legacy in-tree awsElasticBlockStore volume; KubernetesCluster=$CLUSTER on $VOL_LIVE proves its cluster" "$work/tag.legacyzone/output.log" ||
	bad "tag.legacyzone: the legacy source and its cluster proof are not logged"
tagcase legacynozone "$(legacy_pv '{volumeID: ("aws:///" + $v)}')" '.volumes[$v].tags.KubernetesCluster = $c' OK
tagcase legacybare "$(legacy_pv '{volumeID: $v}')" '.volumes[$v].tags.KubernetesCluster = $c' OK
tagcase legacystring "$(legacy_pv '("aws://us-east-1a/" + $v)')" '.volumes[$v].tags.KubernetesCluster = $c' REFUSE "PersistentVolume pvc-ctrl has awsElasticBlockStore volumeID '', not an EBS volume id"
tagcase legacynoid "$(legacy_pv '{fsType: "ext4"}')" '.volumes[$v].tags.KubernetesCluster = $c' REFUSE "PersistentVolume pvc-ctrl has awsElasticBlockStore volumeID '', not an EBS volume id"
tagcase legacynumber "$(legacy_pv '{volumeID: 42}')" '.volumes[$v].tags.KubernetesCluster = $c' REFUSE "PersistentVolume pvc-ctrl has awsElasticBlockStore volumeID '', not an EBS volume id"
tagcase legacyfs "$(legacy_pv '{volumeID: "aws://us-east-1a/fs-0123456789abcdef0"}')" '.volumes[$v].tags.KubernetesCluster = $c' REFUSE "PersistentVolume pvc-ctrl has awsElasticBlockStore volumeID 'aws://us-east-1a/fs-0123456789abcdef0', not an EBS volume id"
tagcase legacytrailing "$(legacy_pv '{volumeID: ("aws://us-east-1a/" + $v + "/x")}')" '.volumes[$v].tags.KubernetesCluster = $c' REFUSE "PersistentVolume pvc-ctrl has awsElasticBlockStore volumeID 'aws://us-east-1a/$VOL_LIVE/x', not an EBS volume id"
tagcase legacyscheme "$(legacy_pv '{volumeID: ("ebs://us-east-1a/" + $v)}')" '.volumes[$v].tags.KubernetesCluster = $c' REFUSE "PersistentVolume pvc-ctrl has awsElasticBlockStore volumeID 'ebs://us-east-1a/$VOL_LIVE', not an EBS volume id"
tagcase legacyinjected "$(legacy_pv '{volumeID: ($v + " --filters Name=tag:kubeneuron:e2e,Values=true")}')" '.volumes[$v].tags.KubernetesCluster = $c' REFUSE "PersistentVolume pvc-ctrl has awsElasticBlockStore volumeID '$VOL_LIVE --filters Name=tag:kubeneuron:e2e,Values=true', not an EBS volume id"
for k in pending nopv pvgone uid otherns driver handle csistring nosource nfs dual dualmalformed legacystring legacynoid legacynumber legacyfs legacytrailing legacyscheme legacyinjected; do
	tag_not_called "$k" 'aws ec2'
done
# Legacy, AWS side: the volume is read by id; the exact KubernetesCluster tag
# is required first, then the same three created-for tags and the other-run
# guard; a refusal tags nothing.
legacy_zone_pv=$(legacy_pv '{volumeID: ("aws://us-east-1a/" + $v)}')
tagcase legacynocluster "$legacy_zone_pv" '.' REFUSE "EBS volume $VOL_LIVE is tagged KubernetesCluster='', not $CLUSTER; a legacy in-tree volume must name this cluster"
tagcase legacyothercluster "$legacy_zone_pv" '.volumes[$v].tags.KubernetesCluster = $o' REFUSE "EBS volume $VOL_LIVE is tagged KubernetesCluster='$OTHER', not $CLUSTER; a legacy in-tree volume must name this cluster"
tagcase legacyprefixcluster "$legacy_zone_pv" '.volumes[$v].tags.KubernetesCluster = ($c + "-2")' REFUSE "EBS volume $VOL_LIVE is tagged KubernetesCluster='$CLUSTER-2', not $CLUSTER; a legacy in-tree volume must name this cluster"
tagcase legacypvtag "$legacy_zone_pv" '.volumes[$v].tags.KubernetesCluster = $c | .volumes[$v].tags["kubernetes.io/created-for/pv/name"] = "pvc-someone"' REFUSE "EBS volume $VOL_LIVE is tagged kubernetes.io/created-for/pv/name='pvc-someone', not pvc-ctrl"
tagcase legacypvctag "$legacy_zone_pv" '.volumes[$v].tags.KubernetesCluster = $c | .volumes[$v].tags["kubernetes.io/created-for/pvc/name"] = "kubeneuron-controller-state-2"' REFUSE "EBS volume $VOL_LIVE is tagged kubernetes.io/created-for/pvc/name='kubeneuron-controller-state-2', not kubeneuron-controller-state"
tagcase legacynstag "$legacy_zone_pv" '.volumes[$v].tags.KubernetesCluster = $c | .volumes[$v].tags["kubernetes.io/created-for/pvc/namespace"] = "other-ns"' REFUSE "EBS volume $VOL_LIVE is tagged kubernetes.io/created-for/pvc/namespace='other-ns', not kube-neuron"
tagcase legacyclusteronly "$legacy_zone_pv" '.volumes[$v].tags = {KubernetesCluster: $c}' REFUSE "EBS volume $VOL_LIVE is tagged kubernetes.io/created-for/pv/name='', not pvc-ctrl"
tagcase legacyforeign "$legacy_zone_pv" '.volumes[$v].tags.KubernetesCluster = $c | .volumes[$v].tags["kubeneuron:e2e-run"] = $o' REFUSE "EBS volume $VOL_LIVE is already tagged kubeneuron:e2e-run='$OTHER', another run's"
tagcase legacymissing "$legacy_zone_pv" '.volumes = {}' REFUSE "EBS volume $VOL_LIVE does not exist in us-east-1"
for k in legacynocluster legacyothercluster legacyprefixcluster legacypvtag legacypvctag legacynstag legacyclusteronly legacyforeign legacymissing; do
	tag_called "$k" "ec2 describe-volumes --region us-east-1 --volume-ids $VOL_LIVE --output json\$"
	tag_not_called "$k" 'create-tags'
	tag_not_called "$k" 'describe-volumes .*--filters'
	jq -e --arg v "$VOL_LIVE" '(.volumes[$v].tags // {}) | has("kubeneuron:e2e") or has("kubeneuron:e2e-claim") | not' "$work/tag.$k/state.json" >/dev/null ||
		bad "tag.$k: a refused legacy volume carries a run tag: $(jq -c --arg v "$VOL_LIVE" '.volumes[$v].tags' "$work/tag.$k/state.json")"
done
tagcase legacyfrozen "$legacy_zone_pv" '.volumes[$v].tags.KubernetesCluster = $c | .volumes[$v].tags_frozen = true' REFUSE "EBS volume $VOL_LIVE does not carry the run tags after create-tags"
tag_called legacyfrozen 'create-tags'
# AWS-side refusals: the volume is read by id, and nothing is tagged.
tagcase missing '.' '.volumes = {}' REFUSE "EBS volume $VOL_LIVE does not exist in us-east-1"
tagcase pvtag '.' '.volumes[$v].tags["kubernetes.io/created-for/pv/name"] = "pvc-someone"' REFUSE "EBS volume $VOL_LIVE is tagged kubernetes.io/created-for/pv/name='pvc-someone', not pvc-ctrl"
tagcase pvctag '.' '.volumes[$v].tags["kubernetes.io/created-for/pvc/name"] = "kubeneuron-controller-state-2"' REFUSE "EBS volume $VOL_LIVE is tagged kubernetes.io/created-for/pvc/name='kubeneuron-controller-state-2', not kubeneuron-controller-state"
tagcase nstag '.' '.volumes[$v].tags["kubernetes.io/created-for/pvc/namespace"] = "other-ns"' REFUSE "EBS volume $VOL_LIVE is tagged kubernetes.io/created-for/pvc/namespace='other-ns', not kube-neuron"
tagcase untagged '.' '.volumes[$v].tags = {}' REFUSE "EBS volume $VOL_LIVE is tagged kubernetes.io/created-for/pv/name='', not pvc-ctrl"
tagcase foreign '.' '.volumes[$v].tags["kubeneuron:e2e-run"] = $o' REFUSE "EBS volume $VOL_LIVE is already tagged kubeneuron:e2e-run='$OTHER', another run's"
for k in missing pvtag pvctag nstag untagged foreign; do
	tag_not_called "$k" 'create-tags'
	tag_not_called "$k" 'describe-volumes .*--filters'
done
# A create-tags that returns 0 and lands nothing: the read-back is the proof,
# not the exit status, and a volume the sweep's filter cannot see is unproven.
tagcase frozen '.' '.volumes[$v].tags_frozen = true' REFUSE "EBS volume $VOL_LIVE does not carry the run tags after create-tags"
tag_called frozen 'create-tags'

# --- structural: the scope cannot quietly widen again -------------------------
note "static: every volume query in the harness is scoped by id, run tag or exact cluster tag; the sweep names no namespace"
script=hack/hw-e2e.sh
# Continuation lines joined, so one aws call is one record.
joined=$(sed -e ':a' -e '/\\$/N; s/\\\n[[:space:]]*/ /; ta' "$script")
n_queries=0
while IFS= read -r line; do
	n_queries=$((n_queries + 1))
	case "$line" in
	*created-for* | *CSI_PVC_NAMESPACE_TAG* | *RUNTIME_NAMESPACE* | *OBSERVABILITY_NAMESPACE*)
		bad "a describe-volumes is scoped by PVC namespace or the CSI created-for tags: $line" ;;
	*--volume-ids*) ;;
	*'Name=tag:kubeneuron:e2e,Values=true'*'Name=tag:${E2E_RUN_TAG},Values=${CLUSTER_NAME}'*) ;;
	*'Name=tag:KubernetesCluster,Values=${CLUSTER_NAME}'*) ;;
	*) bad "a describe-volumes is not scoped by volume id, this run's tag or the exact cluster tag: $line" ;;
	esac
done < <(printf '%s\n' "$joined" | grep -E 'aws ec2 describe-volumes' || true)
[ "$n_queries" -ge 6 ] || bad "expected at least six describe-volumes calls in the harness (delete pass, audit, resolver, records), found $n_queries"
! grep -E 'Name=tag:[^ ]*created-for' "$script" >/dev/null ||
	bad "a tag filter on the CSI created-for tags is back: $(grep -nE 'Name=tag:[^ ]*created-for' "$script")"
# The namespace tag constant is read off a by-id volume document inside the
# resolver and nowhere else.
outside=$(grep -n 'CSI_PVC_NAMESPACE_TAG' "$script" | grep -vc '^[0-9]*:readonly ' || true)
inside=$(fn_body tag_run_volume | grep -c 'CSI_PVC_NAMESPACE_TAG' || true)
if [ "$outside" -ne "$inside" ] || [ "$inside" -lt 1 ]; then
	bad "CSI_PVC_NAMESPACE_TAG is used outside tag_run_volume ($outside uses, $inside inside the resolver)"
fi
# Code lines only: the comments in cmd_sweep say why the filter is gone.
code_of() { fn_body "$1" | grep -vE '^[[:space:]]*#'; }
! code_of cmd_sweep | grep -E 'NAMESPACE|created-for|get pvc' >/dev/null ||
	bad "cmd_sweep names a namespace, the CSI created-for tags or a claim listing: $(code_of cmd_sweep | grep -nE 'NAMESPACE|created-for|get pvc')"
# Every claim read in the script names its claim.
stray=$(grep -vE '^[[:space:]]*#' "$script" | grep -oE 'get pvc( [^ ]+)?' | grep -vE 'get pvc ("\$pvc"|vmsingle-kubeneuron)$' || true)
[ -z "$stray" ] || bad "a PVC read without a claim name: $stray"
! grep -E 'get (pvc|pv|persistentvolume[a-z]*) .*(-A|--all-namespaces)' "$script" >/dev/null ||
	bad "a cluster-wide claim listing is in the script"

note "static: the claims are the operator's and the profile's, by name"
script_const() { sed -nE "s/^readonly $1=\"([^\"]*)\"\$/\1/p" "$script" | head -n1; }
[ "$(script_const CONTROLLER_STATE_CLAIM)" = '${ROOT_NAME}-controller-state' ] ||
	bad "CONTROLLER_STATE_CLAIM is '$(script_const CONTROLLER_STATE_CLAIM)', not the operator's <name>-controller-state"
grep -qF 'installation.Name + "-controller-state"' internal/operator/resources.go ||
	bad "the operator no longer names the SQLite claim <name>-controller-state; CONTROLLER_STATE_CLAIM is stale"
[ "$(script_const VMSINGLE_CLAIM)" = 'vmsingle-${ROOT_NAME}' ] ||
	bad "VMSINGLE_CLAIM is '$(script_const VMSINGLE_CLAIM)', not vmsingle-<VMSingle name>"
grep -qE '^readonly ROOT_NAME=kubeneuron$' "$script" || bad "ROOT_NAME is no longer kubeneuron; VMSINGLE_CLAIM must follow the VMSingle name"
grep -qE '^  name: kubeneuron$' deploy/kubernetes/dependencies/observability/vmsingle.yaml ||
	bad "the profile's VMSingle is no longer named kubeneuron; VMSINGLE_CLAIM (vmsingle-<name>) is stale"
[ "$(script_const E2E_CLAIM_TAG)" = 'kubeneuron:e2e-claim' ] || bad "E2E_CLAIM_TAG changed; the fakes and the sweep's audit read kubeneuron:e2e-claim"

note "static: tagging is wired at the readiness points, and before every cluster delete"
ordered() { # <function> <label>...: each label in the body's CODE lines, in order
	local fn="$1" prev=0 prev_label="(start)" line label body
	shift
	body=$(code_of "$fn")
	[ -n "$body" ] || { bad "$fn: not defined"; return; }
	for label in "$@"; do
		line=$(printf '%s\n' "$body" | grep -nF -- "$label" | head -n1 | cut -d: -f1 || true)
		if [ -z "$line" ]; then
			bad "$fn: missing '$label'"
			continue
		fi
		[ "$line" -gt "$prev" ] || bad "$fn: '$label' (line $line) must come after '$prev_label' (line $prev)"
		prev="$line"
		prev_label="$label"
	done
}
ordered cmd_deploy 'wait_for_installation_ready' 'tag_run_volume "$RUNTIME_NAMESPACE" "$CONTROLLER_STATE_CLAIM"' 'assert_real_driver'
ordered cmd_deploy_observability 'get pvc vmsingle-kubeneuron -o jsonpath="{.status.phase}" | grep -qx Bound' \
	'tag_run_volume "$OBSERVABILITY_NAMESPACE" "$VMSINGLE_CLAIM"' 'obs_wait_rollout deployment/vmsingle-kubeneuron 300'
ordered cmd_teardown 'tag_run_volumes_if_live' 'eksctl delete cluster' 'cmd_sweep'
ordered cmd_sweep 'tag_run_volumes_if_live' 'eksctl delete cluster' 'describe-volumes' 'delete-volume' \
	'add_remaining "volumes (run-tagged)"' 'add_remaining "volumes (legacy tag)"' 'recorded_volumes_untagged' 'unproven_claims_for_run' \
	'these still exist after the deletions'
ordered tag_run_volume 'get pvc "$pvc" -o json' '"Bound"' 'get pv "$pv_name" -o json' 'claimRef' '.spec.csi != null' 'EBS_CSI_DRIVER' 'volumeHandle' \
	'.spec.awsElasticBlockStore | objects | .volumeID' 'BASH_REMATCH[2]' \
	'describe-volumes --region "$AWS_REGION" --volume-ids "$vol"' 'LEGACY_CLUSTER_TAG' 'CSI_PV_NAME_TAG' 'CSI_PVC_NAME_TAG' 'CSI_PVC_NAMESPACE_TAG' \
	"another run's" 'create-tags' 'Name=tag:${E2E_CLAIM_TAG},Values=${claim}' 'RUN_VOLUMES_FILE'
ordered tag_run_volumes_if_live 'CONTROLLER_STATE_CLAIM' 'describe-cluster' 'write-kubeconfig' 'claim_unproven' '(assert_kube_target)' \
	'not proven to target the live cluster' 'get pvc "$pvc" -o name' 'tag_run_volume "$ns" "$pvc"' 'NotFound'
in_fn() { fn_body "$1" | grep -E -- "$3" >/dev/null || bad "$1: $2 (pattern: $3)"; }
# The legacy id reaches aws only through the anchored match, and the source
# is classified by what the spec sets, with no fallback branch.
in_fn tag_run_volume 'anchors the legacy volumeID before handing it to aws' '\[\[ "\$vol_id" =~ \^\(aws://\[a-z0-9-\]\*/\)\?\(vol-\[0-9a-f\]\{8,17\}\)\$ \]\]'
in_fn tag_run_volume 'refuses a PV with no EBS source' 'has neither a csi nor an awsElasticBlockStore source'
in_fn tag_run_volume 'refuses a PV with both EBS sources' 'has more than one volume source'
in_fn tag_run_volumes_if_live 'resolves exactly the two known claims' 'claims=\("\$\{RUNTIME_NAMESPACE\}/\$\{CONTROLLER_STATE_CLAIM\}" "\$\{OBSERVABILITY_NAMESPACE\}/\$\{VMSINGLE_CLAIM\}"\)'
in_fn tag_run_volumes_if_live 'never stops the deletion that follows' '^\s*return 0$'
# The target guard is the gate between the kubeconfig and the claims: it runs
# exactly once, in a subshell (it dies), its failure records EVERY known
# claim and returns, and no kubectl or tag action sits between the kubeconfig
# being obtained and that guard.
[ "$(code_of tag_run_volumes_if_live | grep -c 'assert_kube_target')" -eq 1 ] || bad "tag_run_volumes_if_live must validate the kube target exactly once, explicitly"
in_fn tag_run_volumes_if_live 'validates the target in a subshell and fails closed on it' '^\s*if ! why=\$\( \(assert_kube_target\) 2>&1\); then$'
guard_block=$(code_of tag_run_volumes_if_live | sed -n '/(assert_kube_target)/,/return 0/p')
printf '%s\n' "$guard_block" | grep -q 'for claim in "${claims\[@\]}"; do' || bad "tag_run_volumes_if_live: a refused target must record every known claim"
printf '%s\n' "$guard_block" | grep -q 'claim_unproven "$claim" "the dedicated kubeconfig was not proven to target the live cluster' || bad "tag_run_volumes_if_live: a refused target must record each claim with the guard's reason"
between=$(code_of tag_run_volumes_if_live | sed -n '/write-kubeconfig/,/(assert_kube_target)/p' | grep -E 'kubectl|tag_run_volume|create-tags' || true)
[ -z "$between" ] || bad "tag_run_volumes_if_live reads or tags before the target is validated:
$(printf '%s\n' "$between" | sed 's/^/    /')"
# The resolver has one way to fail and no way to shrug.
! fn_body tag_run_volume | grep -E '\|\| true|\bdie ' >/dev/null || bad "tag_run_volume swallows a failure or dies without recording the claim"
[ "$(fn_body tag_run_volume | grep -c 'claim_unproven')" -ge 20 ] || bad "tag_run_volume has fewer refusal points than the chain has links"
[ "$(fn_body tag_run_volume | grep -c 'aws ec2 create-tags')" -eq 1 ] || bad "tag_run_volume must tag exactly once"
in_fn claim_unproven 'records before dying' 'UNPROVEN_CLAIMS_FILE'
in_fn claim_unproven 'dies' '^\s*die '
in_fn recorded_volumes_untagged 'reads each recorded volume by id' -- '--volume-ids "\$vol"'
in_fn recorded_volumes_untagged 'filters records by this cluster' '"\$cluster" = "\$CLUSTER_NAME"'
! fn_body recorded_volumes_untagged | grep -q 'delete-volume' || bad "recorded_volumes_untagged deletes; it must only report"
in_fn unproven_claims_for_run 'filters records by this cluster' '\$1 == c'

# --- refusal: the sweep's own name guard still holds --------------------------
note "guard: a CLUSTER_NAME outside the e2e prefix is refused before any call"
set +e
env -i PATH="$fakebin:$PATH" HOME="$work" FAKE_AWS_STATE=/dev/null FAKE_AWS_LOG="$work/guard.log" \
	CLUSTER_NAME=production AWS_REGION=us-east-1 E2E_STATE_DIR="$work/guard-state" \
	bash hack/hw-e2e.sh sweep >"$work/guard.out" 2>&1
guard_rc=$?
set -e
[ "$guard_rc" -ne 0 ] || bad "guard: sweep accepted CLUSTER_NAME=production"
grep -q "CLUSTER_NAME must start with" "$work/guard.out" || bad "guard: no prefix refusal in the output"
[ ! -s "$work/guard.log" ] || bad "guard: aws was called before the name guard"

# --- verdict ------------------------------------------------------------------
echo
if [ "$fail" -ne 0 ]; then
	echo "verify-hw-e2e-sweep: FAILED — the sweep's networking cleanup or one of its refusals regressed (see above)" >&2
	exit 1
fi
echo "verify-hw-e2e-sweep: OK (a17 orphan cleanup in two passes, one-pass, delete-in-progress waited out, idempotent, clean account, aged-out instance, leaked node; refusals: live cluster, mistagged VPC, ambiguous VPC, no VPC, default VPC, revalidation near-misses incl. field-shift mismatch and non-'interface' InterfaceType, name guard; EBS volumes: swept by this run's tag and the exact cluster tag only, a same-namespace same-claim volume never deleted nor queried, run records read per cluster, a de-tagged record reported and left, unproven claims withhold clean, a live cluster's claim resolved PVC->PV->EBS and tagged before the delete (CSI, and a legacy in-tree PV on its exact KubernetesCluster tag), a stale PV refused and recorded, a PV with no, a foreign, both or a malformed EBS source refused before any AWS call, the live cluster's kubeconfig (written or reused) validated against this cluster's EKS endpoint before any claim lookup and a wrong target recording both claims with nothing read or tagged, every link of the resolution chain refused on its own incl. a tag that did not land; structurally no namespace-scoped volume query anywhere, tagging ordered at deploy/deploy-observability/teardown/sweep)"
