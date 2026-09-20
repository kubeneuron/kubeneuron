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
# sweep matters).
cat >"$fakebin/eksctl" <<'EOF'
#!/usr/bin/env bash
printf 'eksctl %s\n' "$*" >>"$FAKE_AWS_LOG"
name=""
while [ $# -gt 0 ]; do
	case "$1" in
	--name) name=$2; shift ;;
	esac
	shift
done
if [ -n "$name" ] && aws eks describe-cluster --name "$name" >/dev/null 2>&1; then
	exit 0
fi
exit 1
EOF
chmod +x "$fakebin/aws" "$fakebin/eksctl"

readonly CLUSTER="kubeneuron-e2e-verify"
readonly OTHER="kubeneuron-e2e-other"
readonly STACK="eksctl-${CLUSTER}-cluster"

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
  "volumes": {}
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
echo "verify-hw-e2e-sweep: OK (a17 orphan cleanup in two passes, one-pass, delete-in-progress waited out, idempotent, clean account, aged-out instance, leaked node; refusals: live cluster, mistagged VPC, ambiguous VPC, no VPC, default VPC, revalidation near-misses incl. field-shift mismatch and non-'interface' InterfaceType, name guard)"
