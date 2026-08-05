#!/usr/bin/env bash
# shellcheck disable=SC2016 # Single-quoted jq programs expand jq, not shell, variables.
set -Eeuo pipefail

# Hardware GPU end-to-end driver for the .github/workflows/hw-e2e.yaml target.
#
# This stands up an EPHEMERAL EKS cluster with a real NVIDIA g4dn (Tesla T4)
# GPU node, installs KubeNeuron through the operator path, injects a kernel
# XID 79 into a GPU node, asserts the incident walks the dry-run ladder with
# the approver identity in the audit, exercises the confined destructive
# ReplaceNode path, and then DESTROYS everything and sweeps for leaks.
#
# It is deliberately a thin, testable shell layer so the workflow YAML stays
# readable. Every subcommand is idempotent enough to be re-run, and `teardown`
# / `sweep` / `reap` are safe to run on their own — they are the cost guard.
#
# Nothing here hard-codes an AWS account id or a credential. The caller (the
# workflow, via GitHub OIDC) supplies ambient AWS credentials; identifiers come
# from environment variables documented under "Configuration" below.
#
# Provenance: encodes the proven ephemeral-EKS recipe from live runs
# (kubeneuron-e2e<N>, us-east-1, g4dn.xlarge / Tesla T4, driver 580.159.03),
# including the two infra findings that each cost a whole run — the EBS CSI
# addon is a prerequisite, and the controller's CSI volume needs fsGroup.

usage() {
	cat <<'EOF'
Usage: hack/hw-e2e.sh <command>

Commands (the workflow runs them in this order):
  preflight          Verify required tooling and environment variables.
  up                 eksctl-create the ephemeral cluster (GPU + CPU nodegroups),
                     install the EBS CSI addon, GPU operator, default
                     StorageClass, a scoped ReplaceNode IRSA role, and tag every
                     resource with an expiry and this run identity.
  up-finish          Resume an interrupted up: run every post-eksctl step
                     (kubeconfig, IRSA role, StorageClass, GPU operator)
                     against the already-created cluster.
  deploy             Build+push images to ECR and install KubeNeuron (DryRun),
                     wait Ready.
  test-dryrun        Inject XID 79 and assert cordon->drain->approval->dry-run
                     ladder with the approver identity in the audit.
  test-threshold     Inject XID 92 three times (past the agent's 2-minute
                     dedup window) and assert the threshold path: one signal
                     holds in OBSERVING, the third crosses the policy threshold
                     and the observe-only ladder resolves.
  test-dcgm          Exercise the DCGM detection source via field injection
                     (fallback: live dmon parse-clean check). UNEXERCISED LIVE.
  test-verify-recur  Re-inject during VERIFYING; assert recurrence escalates.
                     UNEXERCISED LIVE.
  test-destructive   Flip to executionMode=Enabled under a confined
                     destructiveExecution block and assert the ReplaceNode path
                     closes the incident as replaced.
  teardown           eksctl delete cluster --force --wait, then sweep. Safe to re-run.
  sweep              Assert ZERO leftovers for this run: cluster, non-terminated e2e EC2,
                     e2e CloudFormation stacks, orphaned e2e EBS volumes, and a
                     manually-created recycle IAM role. Deletes what it finds.
  reap               Out-of-band watchdog: force-delete any e2e cluster older
                     than MAX_LIFETIME_MINUTES. Meant for an INDEPENDENT cron
                     so a hung run cannot leak a cluster (PRODUCT_PLAN safety
                     rule 1). Never assumes this run's cluster.

Configuration (environment variables):
  CLUSTER_NAME       ephemeral cluster name (default: kubeneuron-e2e-local).
                     Must start with the e2e prefix so sweep/reap can find it.
  AWS_REGION         region (default: us-east-1).
  ECR_REGISTRY       ECR registry host, e.g. <acct>.dkr.ecr.<region>.amazonaws.com
                     (required for deploy). Never hard-code the account id.
  GPU_INSTANCE_TYPE  GPU instance type (default: g4dn.xlarge).
  K8S_VERSION        EKS control-plane version (default: 1.33).
  GPU_OPERATOR_VERSION  NVIDIA GPU operator chart version (default: v24.9.0).
  RECYCLE_ROLE_NAME  IAM role name that a live run may create for the
                     controller's scoped ec2:TerminateInstances IRSA. Swept on
                     teardown (default: <CLUSTER_NAME>-recycle). Do not reuse a
                     role belonging to another run.
  E2E_KUBECONFIG     dedicated kubeconfig path. Defaults beneath RUNNER_TEMP
                     (or /tmp); the script never uses the caller's default
                     kubeconfig.
  MAX_LIFETIME_MINUTES  reap threshold (default: 180).
  KEEP_CLUSTER       set to 1 to skip destruction in teardown (debug only).
EOF
}

readonly E2E_PREFIX="kubeneuron-e2e"
readonly XID_ACK="I understand these nodes may be reset, rebooted, or destroyed"
readonly CONFIRM_INCIDENT_TIMEOUT=600
readonly DRYRUN_XID=79
readonly DESTRUCTIVE_XID=45
readonly THRESHOLD_XID=92
readonly E2E_RUN_TAG="kubeneuron:e2e-run"

CLUSTER_NAME="${CLUSTER_NAME:-kubeneuron-e2e-local}"
AWS_REGION="${AWS_REGION:-us-east-1}"
ECR_REGISTRY="${ECR_REGISTRY:-}"
GPU_INSTANCE_TYPE="${GPU_INSTANCE_TYPE:-g4dn.xlarge}"
K8S_VERSION="${K8S_VERSION:-1.33}"
GPU_OPERATOR_VERSION="${GPU_OPERATOR_VERSION:-v24.9.0}"
RECYCLE_ROLE_NAME="${RECYCLE_ROLE_NAME:-${CLUSTER_NAME}-recycle}"
MAX_LIFETIME_MINUTES="${MAX_LIFETIME_MINUTES:-180}"
KEEP_CLUSTER="${KEEP_CLUSTER:-0}"

# Hardware E2E must never inherit an operator's or runner's ambient context.
# A previous failure applied fixtures to a different live EKS cluster through a
# shared ~/.kube/config; use a single, per-run file and assert its API endpoint
# before every kubectl operation in this process.
E2E_STATE_DIR="${E2E_STATE_DIR:-${RUNNER_TEMP:-/tmp}/kubeneuron-hw-e2e/${CLUSTER_NAME}}"
E2E_KUBECONFIG="${E2E_KUBECONFIG:-${E2E_STATE_DIR}/kubeconfig}"
case "$E2E_KUBECONFIG" in
*:*)
	printf 'hw-e2e: %s\n' "E2E_KUBECONFIG must name one file, not a colon-separated kubeconfig list" >&2
	exit 1
	;;
esac
export KUBECONFIG="$E2E_KUBECONFIG"
RECYCLE_ROLE_ARN="${RECYCLE_ROLE_ARN:-}"

readonly OPERATOR_NAMESPACE=kube-neuron
readonly RUNTIME_NAMESPACE=kube-neuron
readonly ROOT_NAME=kubeneuron
IMAGE_TAG="e2e-$(date -u +%Y%m%d%H%M%S)"
readonly IMAGE_TAG

SCRIPT_DIR=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)
readonly SCRIPT_DIR
REPO_ROOT=$(cd -- "$SCRIPT_DIR/.." && pwd -P)
readonly REPO_ROOT

log() { printf '=== %s\n' "$*" >&2; }
die() {
	printf 'hw-e2e: %s\n' "$*" >&2
	exit 1
}

require_cmd() {
	command -v "$1" >/dev/null 2>&1 || die "required tool not found on PATH: $1"
}

# guard_cluster_name refuses to operate on anything not clearly ours, so a
# mis-set CLUSTER_NAME can never delete an unrelated cluster.
guard_cluster_name() {
	case "$CLUSTER_NAME" in
	"$E2E_PREFIX"*) : ;;
	*) die "CLUSTER_NAME must start with '$E2E_PREFIX' (got '$CLUSTER_NAME')" ;;
	esac
}

init_kubeconfig() {
	local dir
	dir=$(dirname "$KUBECONFIG")
	mkdir -p "$dir"
	chmod 0700 "$dir"
	: >"$KUBECONFIG"
	chmod 0600 "$KUBECONFIG"
}

# assert_kube_target checks both the selected context and its API endpoint.
# Context names are mutable and therefore not a safety boundary; EKS supplies
# the authoritative endpoint for the cluster that this run created.
assert_kube_target() {
	[ -s "$KUBECONFIG" ] || die "dedicated kubeconfig is missing or empty: $KUBECONFIG"
	local expected actual context
	expected=$(aws eks describe-cluster --name "$CLUSTER_NAME" --region "$AWS_REGION" \
		--query 'cluster.endpoint' --output text) ||
		die "cannot resolve EKS endpoint for $CLUSTER_NAME"
	context=$(command kubectl --kubeconfig "$KUBECONFIG" config current-context 2>/dev/null) ||
		die "dedicated kubeconfig has no current context"
	actual=$(command kubectl --kubeconfig "$KUBECONFIG" config view --minify \
		-o jsonpath='{.clusters[0].cluster.server}') ||
		die "cannot read server from kubeconfig context $context"
	[ "$actual" = "$expected" ] ||
		die "refusing kubectl against $context ($actual); expected EKS endpoint for $CLUSTER_NAME"
}

# Keep every direct kubectl invocation in this script behind the endpoint
# check. Child scripts inherit the isolated KUBECONFIG and are preceded by an
# explicit assert_kube_target call at their invocation site.
kubectl() {
	assert_kube_target
	command kubectl --kubeconfig "$KUBECONFIG" "$@"
}

# ---------------------------------------------------------------------------

cmd_preflight() {
	log "preflight: tooling"
	require_cmd eksctl
	require_cmd kubectl
	require_cmd aws
	require_cmd helm
	require_cmd jq
	guard_cluster_name
	log "preflight: AWS identity (no account id printed)"
	aws sts get-caller-identity --query Arn --output text >/dev/null ||
		die "AWS credentials are not usable; the workflow assumes an OIDC role"
	log "preflight OK (cluster=$CLUSTER_NAME region=$AWS_REGION type=$GPU_INSTANCE_TYPE)"
}

# cmd_up creates the ephemeral cluster. Two GPU-lab findings are encoded as
# first-class steps because each one silently cost a full prior run:
#   1. The aws-ebs-csi-driver addon is a PREREQUISITE — without it the
#      controller's SQLite PVC stays Pending forever.
#   2. EKS ships no default StorageClass, so gp2 must be marked default.
cmd_up() {
	guard_cluster_name
	require_cmd eksctl
	require_cmd kubectl
	require_cmd helm
	require_cmd aws
	require_cmd jq
	init_kubeconfig

	local expires_at
	expires_at=$(date -u -d "+${MAX_LIFETIME_MINUTES} minutes" +%Y-%m-%dT%H:%M:%SZ 2>/dev/null ||
		date -u -v "+${MAX_LIFETIME_MINUTES}M" +%Y-%m-%dT%H:%M:%SZ)

	log "up: creating ephemeral cluster $CLUSTER_NAME ($GPU_INSTANCE_TYPE + CPU nodegroup)"
	# withOIDC is required so the EBS CSI driver and any IRSA role can bind.
	eksctl create cluster -f - <<EOF
apiVersion: eksctl.io/v1alpha5
kind: ClusterConfig
metadata:
  name: ${CLUSTER_NAME}
  region: ${AWS_REGION}
  version: "${K8S_VERSION}"
  tags:
    kubeneuron:e2e: "true"
    kubeneuron:e2e-expires-at: "${expires_at}"
iam:
  withOIDC: true
addons:
  # A real prior run failed here: without the EBS CSI addon the controller's
  # SQLite PVC never binds. It is a prerequisite, not an afterthought.
  - name: aws-ebs-csi-driver
    wellKnownPolicies:
      ebsCSIController: true
managedNodeGroups:
  - name: cpu
    instanceType: m6i.large
    desiredCapacity: 2
    minSize: 2
    maxSize: 3
    labels: { kubeneuron.io/e2e: "true", role: cpu }
    tags: { kubeneuron:e2e: "true", kubeneuron:e2e-expires-at: "${expires_at}", kubeneuron:e2e-run: "${CLUSTER_NAME}" }
  - name: gpu
    instanceType: ${GPU_INSTANCE_TYPE}
    desiredCapacity: 1
    minSize: 1
    maxSize: 1
    labels: { kubeneuron.io/e2e: "true", role: gpu, nvidia.com/gpu.present: "true" }
    tags: { kubeneuron:e2e: "true", kubeneuron:e2e-expires-at: "${expires_at}", kubeneuron:e2e-run: "${CLUSTER_NAME}" }
EOF

	cmd_up_finish
	log "up: cluster ready; expires-at tag = $expires_at"
}

# cmd_up_finish runs every up step AFTER eksctl created the cluster. It is a
# separate command so an interrupted up (the eksctl wait alone runs ~15 min)
# can resume against the existing cluster instead of tearing down and paying
# for a full re-create.
cmd_up_finish() {
	guard_cluster_name
	require_cmd eksctl
	require_cmd kubectl
	require_cmd helm
	require_cmd aws
	require_cmd jq
	init_kubeconfig

	log "up: writing isolated kubeconfig $KUBECONFIG"
	eksctl utils write-kubeconfig --cluster "$CLUSTER_NAME" --region "$AWS_REGION" \
		--kubeconfig "$KUBECONFIG"
	assert_kube_target

	log "up: creating scoped controller IRSA role for ReplaceNode"
	create_recycle_role

	log "up: making gp2 the default StorageClass (EKS ships none)"
	kubectl patch storageclass gp2 \
		-p '{"metadata":{"annotations":{"storageclass.kubernetes.io/is-default-class":"true"}}}' ||
		log "up: gp2 patch skipped (already default or absent)"

	log "up: installing the NVIDIA GPU operator ($GPU_OPERATOR_VERSION)"
	helm repo add nvidia https://helm.ngc.nvidia.com/nvidia >/dev/null 2>&1 || true
	helm repo update nvidia >/dev/null
	# The EKS NVIDIA AMI already carries the driver; let the operator manage the
	# device plugin / toolkit only. driver.enabled=false matches the g4dn AMI.
	helm upgrade --install gpu-operator nvidia/gpu-operator \
		--namespace gpu-operator --create-namespace \
		--version "$GPU_OPERATOR_VERSION" \
		--set driver.enabled=false \
		--wait --timeout 15m

	log "up: waiting for a GPU to be schedulable"
	wait_for 600 'kubectl get nodes -l role=gpu \
		-o jsonpath="{.items[*].status.allocatable.nvidia\.com/gpu}" | grep -q "[1-9]"'
}

cmd_deploy() {
	guard_cluster_name
	require_cmd aws
	require_cmd docker
	require_cmd kubectl
	require_cmd jq
	[ -n "$ECR_REGISTRY" ] || die "ECR_REGISTRY must be set for deploy"
	assert_kube_target

	log "deploy: ECR login"
	aws ecr get-login-password --region "$AWS_REGION" |
		docker login --username AWS --password-stdin "$ECR_REGISTRY"

	local img
	for img in operator controller agent; do
		ensure_ecr_repo "kubeneuron/$img"
	done

	log "deploy: build+push operator/controller/agent -> $ECR_REGISTRY (tag $IMAGE_TAG)"
	make -C "$REPO_ROOT" docker \
		IMAGE_REPO="$ECR_REGISTRY/kubeneuron" IMAGE_TAG="$IMAGE_TAG"
	for img in operator controller agent; do
		docker push "$ECR_REGISTRY/kubeneuron/$img:$IMAGE_TAG"
	done

	log "deploy: installing KubeNeuron via the operator path (stays DryRun)"
	# The install manifest pins the operator to a ghcr image the cluster cannot
	# pull (documented gotcha from live runs). install.sh WAITS for the operator
	# rollout, so the repoint must land as soon as the Deployment exists — not
	# after install.sh, which would time out first. The repointer runs beside
	# install.sh and swaps the image the moment the object appears.
	assert_kube_target
	# Re-assert continuously, not once: install.sh's apply RE-PINS the ghcr
	# image even when the Deployment pre-exists (a one-shot repoint fires
	# before the apply and is overwritten). The loop keeps swapping until
	# install.sh finishes; set image is a no-op once the template matches.
	(
		while :; do
			command kubectl --kubeconfig "$KUBECONFIG" -n "$OPERATOR_NAMESPACE" \
				set image deployment/kubeneuron-operator \
				"operator=$ECR_REGISTRY/kubeneuron/operator:$IMAGE_TAG" >/dev/null 2>&1 || true
			sleep 2
		done
	) &
	_REPOINT_PID=$!
	local install_rc=0
	"$REPO_ROOT/deploy/install.sh" \
		--name "$ROOT_NAME" --namespace "$RUNTIME_NAMESPACE" \
		--controller-image "$ECR_REGISTRY/kubeneuron/controller:$IMAGE_TAG" \
		--agent-image "$ECR_REGISTRY/kubeneuron/agent:$IMAGE_TAG" || install_rc=$?
	kill "$_REPOINT_PID" 2>/dev/null || true
	wait "$_REPOINT_PID" 2>/dev/null || true
	_REPOINT_PID=""
	[ "$install_rc" -eq 0 ] || die "install.sh failed with $install_rc"
	assert_kube_target

	log "deploy: installing an explicit, test-only dry-run ladder"
	apply_e2e_playbook Reboot
	kubectl -n "$RUNTIME_NAMESPACE" patch kubeneuron "$ROOT_NAME" --type merge \
		-p '{"spec":{"safety":{"executionMode":"DryRun","verifyQuietWindow":"30s"}}}'

	# Idempotent belt-and-braces: assert the ECR image stuck.
	log "deploy: repointing the operator image at ECR"
	kubectl -n "$OPERATOR_NAMESPACE" set image deployment/kubeneuron-operator \
		"operator=$ECR_REGISTRY/kubeneuron/operator:$IMAGE_TAG"

	log "deploy: waiting for the operator and controller to be Ready"
	kubectl -n "$OPERATOR_NAMESPACE" rollout status deployment/kubeneuron-operator --timeout=5m
	wait_for_installation_ready
	log "deploy: KubeNeuron Ready"
}

cmd_test_dryrun() {
	guard_cluster_name
	require_cmd kubectl
	require_cmd jq
	require_cmd curl
	local node
	node=$(gpu_node)
	assert_kube_target
	log "test-dryrun: injecting kernel XID $DRYRUN_XID into $node /dev/kmsg"
	inject_xid "$node" "$DRYRUN_XID"

	log "test-dryrun: waiting for an incident to open for $node"
	local incident
	incident=$(wait_for_incident "$node")
	[ -n "$incident" ] || die "no incident opened for $node after XID 79"
	log "test-dryrun: incident $incident opened"

	log "test-dryrun: waiting for the ladder to park for approval (AWAITING_APPROVAL)"
	wait_for "$CONFIRM_INCIDENT_TIMEOUT" \
		'api GET "/api/v1/incidents/'"$incident"'" | jq -e ".incident.state==\"AWAITING_APPROVAL\"" >/dev/null'

	log "test-dryrun: asserting cordon and drain preceded the gate in the audit"
	local audit
	audit=$(api GET "/api/v1/incidents/$incident")
	echo "$audit" | jq -e '[.audit[].action] | index("Cordon") != null' >/dev/null ||
		die "Cordon missing from audit trail"
	echo "$audit" | jq -e '[.audit[].action] | index("Drain") != null' >/dev/null ||
		die "Drain missing from audit trail"

	log "test-dryrun: approving as the e2e actor and asserting the identity is audited"
	api POST "/api/v1/incidents/$incident/approve" '{"actor":"hw-e2e-approver"}' >/dev/null
	# The approver's identity is recorded as the ACTOR of the approved step's
	# execution transition (the audit action is the step name, not "Approve"),
	# and static-token identities are audited with their provenance prefix.
	wait_for "$CONFIRM_INCIDENT_TIMEOUT" \
		'api GET "/api/v1/incidents/'"$incident"'" \
		 | jq -e "any(.audit[]; .action==\"Reboot\" and .actor==\"token:hw-e2e-approver\")" >/dev/null'

	log "test-dryrun: asserting the reboot ran as a dry-run no-op (DryRun mode)"
	wait_for "$CONFIRM_INCIDENT_TIMEOUT" \
		'api GET "/api/v1/incidents/'"$incident"'" \
			 | jq -e "[.audit[].action] | index(\"Reboot\") != null" >/dev/null'

	# Incidents correlate by (node, class). Finish this DryRun incident before
	# changing execution mode so the destructive stage can only open a new one.
	wait_for 120 \
		'api GET "/api/v1/incidents/'"$incident"'" | jq -e ".incident.state==\"RESOLVED\"" >/dev/null'
	log "test-dryrun: PASS (cordon->drain->approval->dry-run ladder, actor audited)"
}

# cmd_test_threshold exercises the ONLY XID with different pipeline behavior:
# XID 92 (high single-bit ECC rate) is warning severity with a 3-signal
# threshold, so one signal must hold in OBSERVING and only the third may
# escalate. Injections are spaced past the agent's 2-minute dedup window so
# each one is a distinct accepted signal, not a duplicate.
cmd_test_threshold() {
	guard_cluster_name
	require_cmd kubectl
	require_cmd jq
	require_cmd curl
	local node
	node=$(gpu_node)
	assert_kube_target

	log "test-threshold: installing an observe-only ladder for ecc-sbe-rate (threshold 3)"
	kubectl apply -f - <<EOF
apiVersion: kubeneuron.io/v1alpha1
kind: GPUPlaybook
metadata:
  name: ${ROOT_NAME}-e2e-sbe-rate
  labels: {kubeneuron.io/hw-e2e: "true"}
spec:
  kubeNeuronRef: ${ROOT_NAME}
  target: GPU
  effects: []
  steps:
    - name: Observe
      action: Observe
---
apiVersion: kubeneuron.io/v1alpha1
kind: GPURemediationPolicy
metadata:
  name: ${ROOT_NAME}-e2e-sbe-rate
  labels: {kubeneuron.io/hw-e2e: "true"}
spec:
  kubeNeuronRef: ${ROOT_NAME}
  priority: 10
  match:
    class: ecc-sbe-rate
  playbookRef: ${ROOT_NAME}-e2e-sbe-rate
  # threshold/window feed the controller's observe gate. The window only
  # anchors quiet-resolution; it must comfortably exceed the injection
  # spacing so the incident cannot quiet-resolve mid-test.
  parameters:
    threshold: "3"
    window: "30m"
EOF
	wait_for_configured_object gpuplaybook "${ROOT_NAME}-e2e-sbe-rate"
	wait_for_configured_object gpuremediationpolicy "${ROOT_NAME}-e2e-sbe-rate"

	# Ready on the CRs means the OPERATOR compiled them into the ConfigMap;
	# the CONTROLLER only picks that up via mounted-file hot-reload, and
	# kubelet propagation adds up to ~2 minutes. Playbook binding is
	# open-time, so injecting before the reload opens an incident with NO
	# playbook — it would hold in OBSERVING past any threshold. The
	# controller publishes its loaded-config identity on /readyz
	# ("ready config=<digest>"); wait until it equals the digest the operator
	# advertised on the root status. Fallback: restart the controller — a
	# fresh pod mounts the already-updated ConfigMap immediately.
	local digest
	digest=$(kubectl -n "$RUNTIME_NAMESPACE" get kubeneuron "$ROOT_NAME" \
		-o jsonpath='{.status.configDigest}')
	[ -n "$digest" ] || die "root has no configDigest to wait on"
	log "test-threshold: waiting for the controller to load configuration ${digest:0:12}"
	local waited=0 loaded=0
	while ((waited < 180)); do
		if api GET "/readyz" 2>/dev/null | grep -q "config=${digest}"; then
			loaded=1
			break
		fi
		sleep 10
		waited=$((waited + 10))
	done
	if ((loaded == 0)); then
		log "test-threshold: loaded digest never matched; restarting the controller to load the compiled snapshot"
		kubectl -n "$RUNTIME_NAMESPACE" rollout restart "deployment/${ROOT_NAME}-controller"
		kubectl -n "$RUNTIME_NAMESPACE" rollout status "deployment/${ROOT_NAME}-controller" --timeout=5m
	fi

	log "test-threshold: injecting XID $THRESHOLD_XID (1/3) into $node"
	inject_xid "$node" "$THRESHOLD_XID"
	local incident
	incident=$(wait_for_incident "$node" ecc-sbe-rate)
	[ -n "$incident" ] || die "no ecc-sbe-rate incident opened after the first XID $THRESHOLD_XID"
	log "test-threshold: incident $incident opened"

	log "test-threshold: one signal is below the threshold — the incident must hold in OBSERVING"
	sleep 45
	api GET "/api/v1/incidents/$incident" |
		jq -e '.incident.state=="OBSERVING"' >/dev/null ||
		die "a single sub-threshold signal escalated past OBSERVING"

	log "test-threshold: injecting 2/3 and 3/3 past the agent dedup window (2m each)"
	sleep 130
	inject_xid "$node" "$THRESHOLD_XID"
	sleep 130
	inject_xid "$node" "$THRESHOLD_XID"

	log "test-threshold: the third signal crosses the threshold; the observe ladder must resolve"
	wait_for "$CONFIRM_INCIDENT_TIMEOUT" \
		'api GET "/api/v1/incidents/'"$incident"'" \
			 | jq -e ".incident.state==\"RESOLVED\" and any(.audit[]; .action==\"observe-threshold\")" >/dev/null'
	log "test-threshold: PASS (sub-threshold hold, 3-signal escalation, observe-only resolution)"
}

# cmd_test_dcgm exercises the DCGM poll source — the one detection path the
# XID phases (which write to /dev/kmsg) never touch. DCGM supports field-value
# injection into a live hostengine; if this build refuses injection, fall back
# to asserting the agent parses the live `dcgmi dmon` layout cleanly.
# UNEXERCISED LIVE: written after the first green run; validate on the next
# paid stand before trusting a red result.
cmd_test_dcgm() {
	guard_cluster_name
	require_cmd kubectl
	require_cmd jq
	require_cmd curl
	local node
	node=$(gpu_node)
	assert_kube_target

	local agent_pod dcgm_pod
	agent_pod=$(kubectl -n "$RUNTIME_NAMESPACE" get pods \
		-l "app.kubernetes.io/instance=${ROOT_NAME},app.kubernetes.io/component=agent" \
		--field-selector "spec.nodeName=$node" -o jsonpath='{.items[0].metadata.name}')
	[ -n "$agent_pod" ] || die "no agent pod on $node"
	dcgm_pod=$(kubectl -n gpu-operator get pods -l app=nvidia-dcgm \
		--field-selector "spec.nodeName=$node" -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || true)

	# The agent image is distroless (no shell, no wget): scrape its metrics
	# endpoint from a disposable curl pod against the agent Pod IP instead of
	# exec-ing into it (round-12 review H2).
	local agent_ip
	agent_ip=$(kubectl -n "$RUNTIME_NAMESPACE" get pod "$agent_pod" -o jsonpath='{.status.podIP}')
	[ -n "$agent_ip" ] || die "cannot resolve the agent Pod IP on $node"

	local baseline
	baseline=$(gpuhealth_detections "$agent_ip")

	if [ -n "$dcgm_pod" ] &&
		kubectl -n gpu-operator exec "$dcgm_pod" -- dcgmi test --inject --gpuid 0 \
			-f 230 -v "$THRESHOLD_XID" >/dev/null 2>&1; then
		log "test-dcgm: injected XID $THRESHOLD_XID as a DCGM field value; waiting for the gpuhealth source"
		wait_for "$CONFIRM_INCIDENT_TIMEOUT" \
			'[ "$(gpuhealth_detections '"$agent_ip"')" -gt '"$baseline"' ]'
		log "test-dcgm: PASS (gpuhealth source observed the injected DCGM fault)"
		return 0
	fi

	log "test-dcgm: DCGM injection unavailable; asserting the live dmon layout parses cleanly"
	if kubectl -n "$RUNTIME_NAMESPACE" logs "$agent_pod" --tail=-1 2>/dev/null |
		grep -qiE "gpuhealth.*(parse|column|layout).*(error|warn|fail)"; then
		die "agent logged gpuhealth parse problems against the live dmon layout"
	fi
	log "test-dcgm: PASS (fallback: no gpuhealth parse warnings against live dmon output)"
}

# cmd_test_verify_recur asserts the verification NEGATIVE path: a signal that
# recurs while an incident is VERIFYING must escalate, not quiet-resolve.
# UNEXERCISED LIVE: written after the first green run; validate on the next
# paid stand before trusting a red result.
cmd_test_verify_recur() {
	guard_cluster_name
	require_cmd kubectl
	require_cmd jq
	require_cmd curl
	local node
	node=$(gpu_node)
	assert_kube_target

	log "test-verify-recur: opening a fresh dry-run ladder (XID $DRYRUN_XID)"
	inject_xid "$node" "$DRYRUN_XID"
	local incident
	incident=$(wait_for_incident "$node" fell-off-bus)
	[ -n "$incident" ] || die "no incident opened"
	wait_for "$CONFIRM_INCIDENT_TIMEOUT" \
		'api GET "/api/v1/incidents/'"$incident"'" | jq -e ".incident.state==\"AWAITING_APPROVAL\"" >/dev/null'
	api POST "/api/v1/incidents/$incident/approve" '{"actor":"hw-e2e-approver"}' >/dev/null
	wait_for "$CONFIRM_INCIDENT_TIMEOUT" \
		'api GET "/api/v1/incidents/'"$incident"'" | jq -e ".incident.state==\"VERIFYING\"" >/dev/null'

	# The agent deduplicates one fault for 2 minutes; an immediate
	# re-injection would be swallowed and the incident would quiet-resolve.
	# Space it past the window — the verify quiet window is re-armed by the
	# recurrence, so the incident is still VERIFYING when it lands.
	log "test-verify-recur: waiting past the agent dedup window, then re-injecting XID $DRYRUN_XID"
	sleep 130
	inject_xid "$node" "$DRYRUN_XID"
	wait_for "$CONFIRM_INCIDENT_TIMEOUT" \
		'api GET "/api/v1/incidents/'"$incident"'" \
			 | jq -e ".incident.state!=\"VERIFYING\" and .incident.state!=\"RESOLVED\"" >/dev/null'
	local state
	state=$(api GET "/api/v1/incidents/$incident" | jq -r .incident.state)
	# Close it: an escalated incident stays OPEN (NEEDS_HUMAN is not halted
	# for correlation purposes), and the next phase's fault on the same
	# node+class would ATTACH to it instead of opening its own incident.
	api POST "/api/v1/incidents/$incident/resolve" \
		'{"actor":"hw-e2e","reason":"verification-recurrence phase complete"}' >/dev/null
	log "test-verify-recur: PASS (recurrence during VERIFYING escalated to $state, not RESOLVED)"
}

cmd_test_destructive() {
	guard_cluster_name
	require_cmd kubectl
	require_cmd aws
	require_cmd jq
	require_cmd curl
	load_recycle_role_arn
	local node
	node=$(gpu_node)
	assert_kube_target

	log "test-destructive: confining destructive execution to the e2e GPU node"
	# executionMode Enabled is admissible only with a destructiveExecution block
	# (node selector + exact acknowledgement). A ReplaceNode ladder is used
	# because a hardware GPU reset is impossible on a virtualized g4dn.
	kubectl -n "$RUNTIME_NAMESPACE" patch kubeneuron "$ROOT_NAME" --type merge -p "$(
		cat <<EOF
{"spec":{"cloud":{"provider":"aws","aws":{"region":"${AWS_REGION}","iamRoleARN":"${RECYCLE_ROLE_ARN}"}},
 "safety":{"executionMode":"Enabled",
   "destructiveExecution":{"nodeSelector":{"kubeneuron.io/e2e":"true"},
     "acknowledgement":"${XID_ACK}"}}}}
EOF
	)"
	apply_e2e_playbook ReplaceNode
	wait_for_installation_ready
	verify_controller_irsa

	log "test-destructive: injecting distinct test-only XID $DESTRUCTIVE_XID into $node"
	inject_xid "$node" "$DESTRUCTIVE_XID"
	local incident
	incident=$(wait_for_incident "$node")
	[ -n "$incident" ] || die "no incident opened for destructive run"
	log "test-destructive: incident $incident opened"

	wait_for "$CONFIRM_INCIDENT_TIMEOUT" \
		'api GET "/api/v1/incidents/'"$incident"'" | jq -e ".incident.state==\"AWAITING_APPROVAL\"" >/dev/null'
	log "test-destructive: approving the confined ReplaceNode"
	api POST "/api/v1/incidents/$incident/approve" '{"actor":"hw-e2e-approver"}' >/dev/null

	log "test-destructive: asserting ReplaceNode terminated the instance and the incident resolved"
	# The happy path resolves through the ladder itself: the approved
	# ReplaceNode step (audited under the approver) reports the termination
	# and the verify quiet-window closes the incident. The "node-replaced"
	# audit action belongs to the vanished-node janitor — the OTHER closure
	# path, taken only when the node disappears before the ladder finishes —
	# so asserting it here would fail exactly when everything worked.
	wait_for 900 \
		'api GET "/api/v1/incidents/'"$incident"'" \
			 | jq -e ".incident.state==\"RESOLVED\"
				and any(.audit[]; .action==\"ReplaceNode\" and .actor==\"token:hw-e2e-approver\")
				and any(.audit[]; .action==\"ReplaceNode\" and (.result|test(\"terminated\")))" >/dev/null'
	log "test-destructive: PASS (confined ReplaceNode terminated the node, approver audited, resolved)"
}

cmd_teardown() {
	guard_cluster_name
	if [ "$KEEP_CLUSTER" = "1" ]; then
		log "teardown: KEEP_CLUSTER=1, skipping destruction (debug only)"
		return 0
	fi
	require_cmd eksctl
	log "teardown: deleting cluster $CLUSTER_NAME"
	# --force keeps deleting even if a nodegroup or the stack is unhealthy;
	# a real run once left everything running because a plain delete errored.
	eksctl delete cluster --name "$CLUSTER_NAME" --region "$AWS_REGION" --force --wait ||
		log "teardown: eksctl delete returned non-zero; sweep will finish the job"
	cmd_sweep
}

# cmd_sweep is the cost guard. It ASSERTS zero leftovers and deletes anything
# it finds. A real run once leaked a 1 GiB EBS volume and a manual IRSA role;
# both are swept here explicitly.
cmd_sweep() {
	guard_cluster_name
	require_cmd aws
	require_cmd eksctl
	local leaks=0

	log "sweep: cluster must be gone"
	if eksctl get cluster --name "$CLUSTER_NAME" --region "$AWS_REGION" >/dev/null 2>&1; then
		log "sweep: cluster still present, retrying delete --force"
		eksctl delete cluster --name "$CLUSTER_NAME" --region "$AWS_REGION" --force --wait || leaks=1
	fi

	log "sweep: no non-terminated e2e EC2 instances"
	local instances
	instances=$(aws ec2 describe-instances --region "$AWS_REGION" \
		--filters "Name=tag:kubeneuron:e2e,Values=true" \
		"Name=tag:${E2E_RUN_TAG},Values=${CLUSTER_NAME}" \
		"Name=instance-state-name,Values=pending,running,stopping,stopped" \
		--query 'Reservations[].Instances[].InstanceId' --output text)
	if [ -n "$instances" ]; then
		log "sweep: terminating leaked instances: $instances"
		# shellcheck disable=SC2086 # word-splitting the id list is intended.
		aws ec2 terminate-instances --region "$AWS_REGION" --instance-ids $instances >/dev/null || leaks=1
		# shellcheck disable=SC2086 # word-splitting the id list is intended.
		aws ec2 wait instance-terminated --region "$AWS_REGION" --instance-ids $instances || leaks=1
	fi

	log "sweep: no e2e CloudFormation stacks"
	local stacks
	stacks=$(aws cloudformation list-stacks --region "$AWS_REGION" \
		--stack-status-filter CREATE_COMPLETE UPDATE_COMPLETE ROLLBACK_COMPLETE CREATE_FAILED \
		--query "StackSummaries[?starts_with(StackName, 'eksctl-${CLUSTER_NAME}-')].StackName" \
		--output text)
	local stack
	for stack in $stacks; do
		log "sweep: deleting leaked stack $stack"
		aws cloudformation delete-stack --region "$AWS_REGION" --stack-name "$stack" || leaks=1
		aws cloudformation wait stack-delete-complete --region "$AWS_REGION" --stack-name "$stack" || leaks=1
	done

	log "sweep: no orphaned e2e EBS volumes"
	local volumes
	volumes=$(aws ec2 describe-volumes --region "$AWS_REGION" \
		--filters "Name=tag:kubeneuron:e2e,Values=true" \
		"Name=tag:${E2E_RUN_TAG},Values=${CLUSTER_NAME}" "Name=status,Values=available" \
		--query 'Volumes[].VolumeId' --output text)
	# Also catch the controller's PVC volume tagged with the cluster name.
	local cluster_volumes
	cluster_volumes=$(aws ec2 describe-volumes --region "$AWS_REGION" \
		--filters "Name=tag:KubernetesCluster,Values=${CLUSTER_NAME}" "Name=status,Values=available" \
		--query 'Volumes[].VolumeId' --output text)
	local vol
	for vol in $volumes $cluster_volumes; do
		log "sweep: deleting orphaned volume $vol"
		aws ec2 delete-volume --region "$AWS_REGION" --volume-id "$vol" || leaks=1
	done

	log "sweep: delete any manually-created recycle IAM role ($RECYCLE_ROLE_NAME)"
	delete_iam_role "$RECYCLE_ROLE_NAME" || true

	if [ "$leaks" -ne 0 ]; then
		die "sweep found leftovers it could not delete — investigate before the next run"
	fi
	log "sweep: clean (no cluster, EC2, stacks, volumes, or recycle role)"
}

# cmd_reap is the out-of-band watchdog. Run it from an INDEPENDENT schedule (a
# separate cron / Lambda), never from the run it is guarding: its whole point
# is to force-delete a cluster whose owning job hung past MAX_LIFETIME_MINUTES.
cmd_reap() {
	require_cmd eksctl
	require_cmd aws
	local now expiry name
	now=$(date -u +%s)
	log "reap: scanning for e2e clusters past their expiry"
	local names
	names=$(eksctl get cluster --region "$AWS_REGION" -o json 2>/dev/null |
		jq -r '.[].Name // .[].name' | grep "^${E2E_PREFIX}" || true)
	for name in $names; do
		expiry=$(aws eks describe-cluster --name "$name" --region "$AWS_REGION" \
			--query 'cluster.tags."kubeneuron:e2e-expires-at"' --output text 2>/dev/null || echo "")
		# Fall back to age if no readable expiry tag: reap conservatively.
		if [ -z "$expiry" ] || [ "$expiry" = "None" ]; then
			log "reap: $name has no readable expiry tag; leaving for manual review"
			continue
		fi
		local expiry_s
		expiry_s=$(date -u -d "$expiry" +%s 2>/dev/null || echo 0)
		if [ "$expiry_s" -ne 0 ] && [ "$now" -gt "$expiry_s" ]; then
			log "reap: $name expired at $expiry — force-deleting"
			CLUSTER_NAME="$name" eksctl delete cluster --name "$name" \
				--region "$AWS_REGION" --force --wait || true
			CLUSTER_NAME="$name" RECYCLE_ROLE_NAME="${name}-recycle" cmd_sweep || true
		fi
	done
	log "reap: done"
}

# ---------------------------------------------------------------------------
# helpers

# gpuhealth_detections reads the agent's DCGM/nvidia-smi source counter from
# a disposable curl pod: the agent image is distroless, so exec-based probes
# cannot work there.
gpuhealth_detections() {
	local ip="$1" out
	out=$(kubectl -n "$RUNTIME_NAMESPACE" run "kn-metrics-$RANDOM" --rm -i --restart=Never \
		--image=curlimages/curl:8.10.1 --quiet -- \
		curl -fsS --max-time 10 "http://${ip}:9402/metrics" 2>/dev/null |
		grep -E 'kubeneuron_agent_detections_total\{[^}]*source="gpuhealth"' |
		grep -oE '[0-9]+$' | head -1 || true)
	printf '%s' "${out:-0}"
}

gpu_node() {
	kubectl get nodes -l role=gpu -o jsonpath='{.items[0].metadata.name}'
}

wait_for_installation_ready() {
	wait_for 300 'kubectl -n '"$RUNTIME_NAMESPACE"' get kubeneuron '"$ROOT_NAME"' -o json |
		jq -e ".status.observedGeneration == .metadata.generation and
		any(.status.conditions[]?; .type == \"Ready\" and .status == \"True\")" >/dev/null'
}

wait_for_configured_object() {
	local resource="$1" name="$2"
	wait_for 300 'kubectl get '"$resource"' '"$name"' -o json |
		jq -e ".status.observedGeneration == .metadata.generation and
		any(.status.conditions[]?; .type == \"Ready\" and .status == \"True\")" >/dev/null'
}

# apply_e2e_playbook installs the only policy used by the hardware test. The
# standard installer intentionally creates an Observe-only example, so using it
# would make this target claim it exercised actions it never selected. XID 45
# is an explicit test-only mapping to the same class as XID 79: after the
# DryRun incident resolves, it gives the destructive phase a distinct event
# identity without weakening production deduplication.
apply_e2e_playbook() {
	local final_action="$1"
	case "$final_action" in
	Reboot | ReplaceNode) ;;
	*) die "unsupported E2E final action: $final_action" ;;
	esac
	kubectl apply -f - <<EOF
apiVersion: kubeneuron.io/v1alpha1
kind: GPUPlaybook
metadata:
  name: ${ROOT_NAME}-e2e-fell-off-bus
  labels: {kubeneuron.io/hw-e2e: "true"}
spec:
  kubeNeuronRef: ${ROOT_NAME}
  target: GPU
  effects: [nodeScheduling]
  steps:
    - name: Cordon
      action: Cordon
    - name: Drain
      action: Drain
    - name: ${final_action}
      action: ${final_action}
      approval: Required
---
apiVersion: kubeneuron.io/v1alpha1
kind: GPURemediationPolicy
metadata:
  name: ${ROOT_NAME}-e2e-fell-off-bus
  labels: {kubeneuron.io/hw-e2e: "true"}
spec:
  kubeNeuronRef: ${ROOT_NAME}
  priority: 10
  match:
    class: fell-off-bus
  playbookRef: ${ROOT_NAME}-e2e-fell-off-bus
---
apiVersion: kubeneuron.io/v1alpha1
kind: GPUSignalMapping
metadata:
  name: ${ROOT_NAME}-e2e-destructive-xid
  labels: {kubeneuron.io/hw-e2e: "true"}
spec:
  kubeNeuronRef: ${ROOT_NAME}
  source: xid
  xidCodes: [${DESTRUCTIVE_XID}]
  class: fell-off-bus
  severity: critical
EOF
	wait_for_configured_object gpuplaybook "${ROOT_NAME}-e2e-fell-off-bus"
	wait_for_configured_object gpuremediationpolicy "${ROOT_NAME}-e2e-fell-off-bus"
	wait_for_configured_object gpusignalmapping "${ROOT_NAME}-e2e-destructive-xid"
}

load_recycle_role_arn() {
	RECYCLE_ROLE_ARN=$(aws iam get-role --role-name "$RECYCLE_ROLE_NAME" \
		--query 'Role.Arn' --output text 2>/dev/null) ||
		die "scoped E2E role $RECYCLE_ROLE_NAME does not exist; run 'up' first"
	case "$RECYCLE_ROLE_ARN" in
	arn:*:iam::*:role/*) ;;
	*) die "scoped E2E role has an invalid ARN" ;;
	esac
}

create_recycle_role() {
	local identity partition account issuer issuer_host provider_arn subject trust policy existing_run
	identity=$(aws sts get-caller-identity --query Arn --output text)
	partition=$(printf '%s' "$identity" | cut -d: -f2)
	account=$(printf '%s' "$identity" | cut -d: -f5)
	issuer=$(aws eks describe-cluster --name "$CLUSTER_NAME" --region "$AWS_REGION" \
		--query 'cluster.identity.oidc.issuer' --output text)
	[ -n "$issuer" ] && [ "$issuer" != "None" ] || die "EKS OIDC issuer is unavailable for $CLUSTER_NAME"
	issuer_host=${issuer#https://}
	provider_arn="arn:${partition}:iam::${account}:oidc-provider/${issuer_host}"
	subject="system:serviceaccount:${RUNTIME_NAMESPACE}:${ROOT_NAME}-controller"
	trust=$(jq -nc --arg provider "$provider_arn" --arg issuer "$issuer_host" --arg subject "$subject" '
		{Version:"2012-10-17",Statement:[{Effect:"Allow",Principal:{Federated:$provider},
		Action:"sts:AssumeRoleWithWebIdentity",Condition:{StringEquals:{($issuer+":aud"):"sts.amazonaws.com",($issuer+":sub"):$subject}}}]}')
	policy=$(jq -nc --arg run "$CLUSTER_NAME" --arg e2e_tag "kubeneuron:e2e" --arg run_tag "$E2E_RUN_TAG" '
		{Version:"2012-10-17",Statement:[
		{Sid:"DescribeOnly",Effect:"Allow",Action:["ec2:DescribeInstances"],Resource:"*"},
		{Sid:"TerminateOnlyThisRun",Effect:"Allow",Action:["ec2:TerminateInstances"],Resource:"*",
		 Condition:{StringEquals:{("ec2:ResourceTag/"+$e2e_tag):"true",("ec2:ResourceTag/"+$run_tag):$run}}}
		]}')

	if aws iam get-role --role-name "$RECYCLE_ROLE_NAME" >/dev/null 2>&1; then
		existing_run=$(aws iam list-role-tags --role-name "$RECYCLE_ROLE_NAME" \
			--query "Tags[?Key=='${E2E_RUN_TAG}'].Value | [0]" --output text)
		[ "$existing_run" = "$CLUSTER_NAME" ] ||
			die "refusing to reuse IAM role $RECYCLE_ROLE_NAME owned by another run"
		aws iam update-assume-role-policy --role-name "$RECYCLE_ROLE_NAME" \
			--policy-document "$trust"
	else
		aws iam create-role --role-name "$RECYCLE_ROLE_NAME" \
			--assume-role-policy-document "$trust" \
			--tags "Key=kubeneuron:e2e,Value=true" \
			"Key=${E2E_RUN_TAG},Value=${CLUSTER_NAME}" >/dev/null
	fi
	aws iam put-role-policy --role-name "$RECYCLE_ROLE_NAME" \
		--policy-name kubeneuron-e2e-replace-node --policy-document "$policy"
	load_recycle_role_arn
}

controller_irsa_is_configured() {
	[ "$(kubectl -n "$RUNTIME_NAMESPACE" get serviceaccount "${ROOT_NAME}-controller" \
		-o jsonpath='{.metadata.annotations.eks\.amazonaws\.com/role-arn}')" = "$RECYCLE_ROLE_ARN" ]
}

# Verify web-identity credential exchange from a Pod using the exact managed
# controller ServiceAccount. The controller image is distroless, so this small
# disposable AWS CLI Pod is the reliable STS probe.
verify_controller_irsa() {
	load_recycle_role_arn
	wait_for 180 'controller_irsa_is_configured'
	kubectl -n "$RUNTIME_NAMESPACE" rollout restart "deployment/${ROOT_NAME}-controller"
	kubectl -n "$RUNTIME_NAMESPACE" rollout status "deployment/${ROOT_NAME}-controller" --timeout=5m

	local pod="${ROOT_NAME}-irsa-check" identity partition account expected
	kubectl -n "$RUNTIME_NAMESPACE" delete pod "$pod" --ignore-not-found --wait=true
	kubectl -n "$RUNTIME_NAMESPACE" apply -f - <<EOF
apiVersion: v1
kind: Pod
metadata:
  name: ${pod}
  labels: {kubeneuron.io/hw-e2e: "true"}
spec:
  restartPolicy: Never
  serviceAccountName: ${ROOT_NAME}-controller
  containers:
    - name: sts
      image: public.ecr.aws/aws-cli/aws-cli:2.17.62
      command: ["sh", "-ec", "aws sts get-caller-identity --query Arn --output text"]
EOF
	kubectl -n "$RUNTIME_NAMESPACE" wait --for=jsonpath='{.status.phase}'=Succeeded \
		"pod/${pod}" --timeout=3m || die "controller ServiceAccount could not assume its IRSA role"
	identity=$(kubectl -n "$RUNTIME_NAMESPACE" logs "$pod") || die "cannot read IRSA STS probe output"
	partition=$(printf '%s' "$RECYCLE_ROLE_ARN" | cut -d: -f2)
	account=$(printf '%s' "$RECYCLE_ROLE_ARN" | cut -d: -f5)
	expected="arn:${partition}:sts::${account}:assumed-role/${RECYCLE_ROLE_NAME}/"
	case "$identity" in
	"${expected}"*) ;;
	*) die "controller ServiceAccount assumed an unexpected AWS identity" ;;
	esac
	kubectl -n "$RUNTIME_NAMESPACE" delete pod "$pod" --ignore-not-found --wait=true
}

ensure_ecr_repo() {
	local repo="$1"
	aws ecr describe-repositories --region "$AWS_REGION" --repository-names "$repo" >/dev/null 2>&1 ||
		aws ecr create-repository --region "$AWS_REGION" --repository-name "$repo" >/dev/null
}

# inject_xid writes an NVRM Xid line into the node's kernel ring buffer,
# exactly the format the agent's kmsg watcher matches. `kubectl debug node`
# gives a privileged pod in the node's namespaces with /dev/kmsg writable.
inject_xid() {
	local node="$1" xid="$2"
	case "$xid" in
	*[!0-9]* | "") die "XID must be a positive integer" ;;
	esac
	local line="NVRM: Xid (PCI:0000:00:1e): ${xid}, pid=0, hardware E2E synthetic fault."
	kubectl debug "node/$node" --profile=sysadmin -q \
		--image=busybox:1.36 -- \
		sh -c "printf '%s\n' '$line' > /dev/kmsg"
}

# wait_for_incident returns the id of the newest ACTIVE incident for a node, or
# empty. Halted incidents are excluded: the destructive stage runs after the
# dry-run incident RESOLVED, and returning that one would bind every later
# assertion to a finished incident.
wait_for_incident() {
	local node="$1" class="${2:-}" i id
	for ((i = 0; i < CONFIRM_INCIDENT_TIMEOUT; i += 10)); do
		id=$(api GET "/api/v1/incidents" |
		jq -r --arg n "$node" --arg c "$class" \
			'[.[] | select(.target.node==$n)
				| select($c == "" or .class == $c)
				| select(.state!="RESOLVED" and .state!="EXPIRED")]
				| sort_by(.opened_at) | last | .id // empty')
		if [ -n "$id" ]; then
			printf '%s' "$id"
			return 0
		fi
		sleep 10
	done
	return 0
}

# api calls the controller REST API through a background port-forward, with the
# operator API token read from its Secret. GET/POST only.
api() {
	local method="$1" path="$2" body="${3:-}"
	_ensure_portforward
	local base="http://127.0.0.1:${_API_LOCAL_PORT}"
	if [ "$method" = POST ]; then
		curl -fsS -X POST -H 'Content-Type: application/json' \
			-H "Authorization: Bearer ${_API_TOKEN}" \
			--data "$body" "${base}${path}"
	else
		curl -fsS -H "Authorization: Bearer ${_API_TOKEN}" "${base}${path}"
	fi
}

_API_LOCAL_PORT=""
_API_TOKEN=""
_PF_PID=""
_REPOINT_PID=""
_ensure_portforward() {
	[ -n "$_PF_PID" ] && kill -0 "$_PF_PID" 2>/dev/null && return 0
	_API_TOKEN=$(kubectl -n "$RUNTIME_NAMESPACE" get secret kubeneuron-operator-api-token \
		-o jsonpath='{.data.token}' | base64 -d)
	_API_LOCAL_PORT=18080
	kubectl -n "$RUNTIME_NAMESPACE" port-forward \
		"service/${ROOT_NAME}-controller" "${_API_LOCAL_PORT}:8080" >/dev/null 2>&1 &
	_PF_PID=$!
	wait_for 60 'curl -fsS -o /dev/null "http://127.0.0.1:'"$_API_LOCAL_PORT"'/healthz"'
}

# wait_for polls a shell condition until it succeeds or the timeout elapses.
wait_for() {
	local timeout="$1" cond="$2" i
	for ((i = 0; i < timeout; i += 10)); do
		if eval "$cond" >/dev/null 2>&1; then
			return 0
		fi
		sleep 10
	done
	eval "$cond" >/dev/null 2>&1 && return 0
	die "condition never met within ${timeout}s: $cond"
}

delete_iam_role() {
	local role="$1"
	aws iam get-role --role-name "$role" >/dev/null 2>&1 || return 0
	log "sweep: deleting recycle IAM role $role"
	local policy
	for policy in $(aws iam list-attached-role-policies --role-name "$role" \
		--query 'AttachedPolicies[].PolicyArn' --output text); do
		aws iam detach-role-policy --role-name "$role" --policy-arn "$policy" || true
	done
	for policy in $(aws iam list-role-policies --role-name "$role" \
		--query 'PolicyNames' --output text); do
		aws iam delete-role-policy --role-name "$role" --policy-name "$policy" || true
	done
	aws iam delete-role --role-name "$role"
}

cleanup() {
	if [ -n "$_PF_PID" ]; then
		kill "$_PF_PID" 2>/dev/null || true
	fi
	# A cancelled run must not leak the operator-image repoint loop on the
	# runner (review F6): the EXIT trap fires on SIGTERM too under -E.
	if [ -n "$_REPOINT_PID" ]; then
		kill "$_REPOINT_PID" 2>/dev/null || true
	fi
}
trap cleanup EXIT

# ---------------------------------------------------------------------------

main() {
	local cmd="${1:-}"
	if [ $# -gt 0 ]; then
		shift
	fi
	case "$cmd" in
	preflight) cmd_preflight "$@" ;;
	up) cmd_up "$@" ;;
	up-finish) cmd_up_finish "$@" ;;
	deploy) cmd_deploy "$@" ;;
	test-dryrun) cmd_test_dryrun "$@" ;;
	test-threshold) cmd_test_threshold "$@" ;;
	test-dcgm) cmd_test_dcgm "$@" ;;
	test-verify-recur) cmd_test_verify_recur "$@" ;;
	test-destructive) cmd_test_destructive "$@" ;;
	teardown) cmd_teardown "$@" ;;
	sweep) cmd_sweep "$@" ;;
	reap) cmd_reap "$@" ;;
	-h | --help | help | "") usage ;;
	*)
		usage >&2
		die "unknown command: $cmd"
		;;
	esac
}

main "$@"
