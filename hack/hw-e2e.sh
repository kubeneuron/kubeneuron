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
  test-drain         Real drain over a real pod list: refuse a node carrying
                     a pod with no controller having evicted NOTHING, then
                     drain it for real once that pod is gone
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
# RECUR_XID exists because a recurrence must be a DIFFERENT fault identity.
# The agent deduplicates one (gpu, xid) pair for two minutes, while the
# verification quiet window on this stand is thirty seconds — so re-injecting
# the SAME code during VERIFYING is swallowed by dedup and the incident
# quiet-resolves before dedup would ever let it through. A second test-only
# code mapped to the same class is accepted immediately and attaches to the
# open incident, which is exactly what a real recurrence looks like.
readonly RECUR_XID=44
readonly THRESHOLD_XID=92
# DRAIN_XID gives the drain phase its own fault identity, for the same reason
# every other phase has one: incidents correlate by (node, class), so sharing a
# class would attach this phase's fault to a previous phase's incident.
readonly DRAIN_XID=48
# The second drain incident needs a different fault identity for the same
# reason RECUR_XID exists — a recurrence must not deduplicate into the first —
# but it must map to THIS phase's class, which RECUR_XID does not.
readonly DRAIN_RECUR_XID=13
readonly E2E_RUN_TAG="kubeneuron:e2e-run"

CLUSTER_NAME="${CLUSTER_NAME:-kubeneuron-e2e-local}"
AWS_REGION="${AWS_REGION:-us-east-1}"
ECR_REGISTRY="${ECR_REGISTRY:-}"
GPU_INSTANCE_TYPE="${GPU_INSTANCE_TYPE:-g4dn.xlarge}"
K8S_VERSION="${K8S_VERSION:-1.33}"
# The GPU operator version decides the DCGM HOST ENGINE version, and the agent
# image ships its own dcgmi CLIENT. A 4.x client cannot talk to a 3.x engine —
# every call returns "API version mismatch" — and the first real hardware run
# hit exactly that: agent dcgmi 4.6.1 against v24.9.0's engine 3.3.8, so the
# DCGM detection source failed on every poll and the agent silently served the
# narrower nvidia-smi one. v24.9.x ships 3.3.x; v25.3.4 ships 4.3.1, which is
# the same major as the client. assert_dcgm_versions_agree below enforces the
# pairing rather than trusting this line to stay correct.
GPU_OPERATOR_VERSION="${GPU_OPERATOR_VERSION:-v25.3.4}"
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

# Read the tag this cluster was deployed with, or mint one and record it.
#
# The default is set HERE and not further down: the assignment that used to
# hold it sits below this block, so under `set -u` the first read of
# $IMAGE_TAG was an unbound variable and every command died at startup.
IMAGE_TAG="${IMAGE_TAG:-}"
image_tag_file="${E2E_STATE_DIR}/image-tag"
if [ -z "$IMAGE_TAG" ] && [ -r "$image_tag_file" ]; then
	IMAGE_TAG=$(cat "$image_tag_file")
fi
if [ -z "$IMAGE_TAG" ]; then
	IMAGE_TAG="e2e-$(date -u +%Y%m%d%H%M%S)"
	mkdir -p "$E2E_STATE_DIR"
	printf '%s' "$IMAGE_TAG" >"$image_tag_file"
fi
readonly IMAGE_TAG
RECYCLE_ROLE_ARN="${RECYCLE_ROLE_ARN:-}"

readonly OPERATOR_NAMESPACE=kube-neuron
readonly RUNTIME_NAMESPACE=kube-neuron
readonly ROOT_NAME=kubeneuron
# IMAGE_TAG must survive across invocations. deploy and teardown are separate
# `hack/hw-e2e.sh <cmd>` processes — that is how the workflow runs them — so a
# tag computed here was a FRESH timestamp in the sweep, and the ECR cleanup
# deleted a tag that had never existed. Proved on the first real run: teardown
# reported success and left three images behind. Persist it beside the
# kubeconfig, which is already per-cluster and already survives the same way.
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

	# Free disk, checked before anything is created.
	#
	# A run builds four images from a ~375 MB context and pushes them; run 3
	# exhausted the host disk mid-build. That alone would be an inconvenience —
	# the failure was that TEARDOWN could not run either, because the driver
	# pipes it through `tee` and tee could not write. A full disk therefore
	# broke the one guarantee this target makes, and left a live cluster and
	# three instances billing until a human noticed.
	#
	# 25 GiB is measured, not guessed: the four image builds plus the build
	# context peaked around 20 GiB on the run that failed.
	local free_kb
	free_kb=$(df -Pk "$REPO_ROOT" | awk 'NR==2 {print $4}')
	if [ "${free_kb:-0}" -lt $((25 * 1024 * 1024)) ]; then
		die "only $((free_kb / 1024 / 1024)) GiB free on $(df -Ph "$REPO_ROOT" | awk 'NR==2 {print $6}'); a run needs about 25 GiB for the image builds, and running out mid-build has already cost one cluster that teardown could not remove. Free space (docker image prune -af) before dispatching."
	fi

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
	#
	# dcgm.enabled=true is what makes the DCGM phase mean anything. The operator
	# ships the standalone host engine DISABLED, because its exporter embeds a
	# private one that nothing outside the exporter can reach. Left at the
	# default there is no nvidia-dcgm pod and no nvidia-dcgm Service, so the
	# agent's dcgmEndpoint resolves to nothing, runtime attestation stays
	# degraded, and test-dcgm can only ever take its fallback branch — against a
	# source that was never serving the node in the first place.
	helm upgrade --install gpu-operator nvidia/gpu-operator \
		--namespace gpu-operator --create-namespace \
		--version "$GPU_OPERATOR_VERSION" \
		--set driver.enabled=false \
		--set dcgm.enabled=true \
		--wait --timeout 15m

	log "up: waiting for a GPU to be schedulable"
	wait_for 600 'kubectl get nodes -l role=gpu \
		-o jsonpath="{.items[*].status.allocatable.nvidia\.com/gpu}" | grep -q "[1-9]"'

	# Assert the engine the agent is about to be pointed at exists. --wait above
	# covers the release, not this one subchart's rollout, and an absent engine
	# is indistinguishable at the agent from a healthy one that reports nothing.
	log "up: waiting for the standalone DCGM host engine"
	wait_for 600 'kubectl -n gpu-operator get pods -l app=nvidia-dcgm \
		-o jsonpath="{.items[*].status.phase}" | grep -q Running'
	kubectl -n gpu-operator get svc nvidia-dcgm >/dev/null 2>&1 ||
		die "no nvidia-dcgm Service; spec.agent.hostTooling.dcgmEndpoint would point at nothing"
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

	# Arm the agent's host tooling. THIS IS THE STEP THAT MAKES THIS TARGET
	# ABOUT NVIDIA AT ALL, and it was missing for every run this stand has ever
	# done.
	#
	# install.sh writes an agent block of image + tolerations and nothing else,
	# because it also installs onto CPU-only clusters. The shipped agent image is
	# distroless and carries exactly three executables — its own binary, dcgmi
	# and nsenter — so with no host mounts there is no nvidia-smi on PATH, and
	# cmd/kubeneuron-agent/main.go takes its default branch: warn once, run on
	# nvml.Fake. Everything this target claims to prove then unravels quietly:
	# the DCGM/nvidia-smi watcher is only built for a real driver, NVIDIA
	# observation is switched off so no accelerator report is ever produced,
	# GPUByPCIAddr finds nothing so every incident is node-scoped rather than
	# device-scoped, and the agent refuses the controller's armed answer. The run
	# stays green throughout, on a real T4, having exercised a simulator.
	#
	# Declaring hostTooling is also what arms --require-real-driver, which turns
	# that silent fallback into a startup failure. That is the real assertion
	# here; assert_real_driver below is the second one, for a regression in the
	# wiring itself rather than in the node.
	# The nodeSelector is not optional here, and leaving it out breaks the run
	# before the assertion below can say anything useful.
	#
	# hostTooling is a property of the ONE agent DaemonSet, and install.sh gives
	# that DaemonSet no nodeSelector at all — it tolerates everything, because it
	# also installs onto CPU-only clusters. This stand's cluster has a two-node
	# m6i.large CPU nodegroup beside the GPU one. Arming host tooling fleet-wide
	# therefore hands --require-real-driver to the CPU agents too, and that flag
	# does exactly what it promises: no nvidia-smi, exit 1, CrashLoopBackOff.
	#
	# The operator's Ready condition requires every scheduled agent to be
	# available, so wait_for_installation_ready below would time out after five
	# minutes with "condition never met" — after paying for a cluster and three
	# image builds, and without ever reaching assert_real_driver, whose whole
	# job is to name this class of failure.
	log "deploy: arming host tooling on the GPU nodes so the agent runs on a real driver"
	kubectl -n "$RUNTIME_NAMESPACE" patch kubeneuron "$ROOT_NAME" --type merge \
		-p '{"spec":{"agent":{"nodeSelector":{"nvidia.com/gpu.present":"true"},"hostTooling":{"binDir":"/usr/bin","libDirs":["/usr/lib64"],"dcgmEndpoint":"nvidia-dcgm.gpu-operator.svc:5555"}}}}'

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
	assert_real_driver
	log "deploy: KubeNeuron Ready"
}

# assert_real_driver refuses to let the NVIDIA phases run against nvml.Fake.
#
# Two independent checks, because they fail for different reasons:
#
#   1. The DaemonSet rolls out. With hostTooling declared the agent runs with
#      --require-real-driver, so an agent that cannot find nvidia-smi crash-loops
#      instead of falling back. If the node's NVIDIA userspace is not where
#      binDir/libDirs say it is, this is where the run stops — at deploy, for a
#      nameable reason, rather than fifty minutes later in an assertion about
#      something else.
#   2. No pod logged the fallback. Check (1) cannot see a regression in the
#      operator's own wiring: if agentHostToolingWiring stopped emitting
#      --require-real-driver, the agent would start happily on Fake and roll out
#      clean. The warning is the only evidence of that, and it is one grep.
assert_real_driver() {
	local node agent_pod
	node=$(gpu_node)
	[ -n "$node" ] || die "no GPU node to assert a driver on"

	log "deploy: asserting the agent on $node is on the node's real driver"
	kubectl -n "$RUNTIME_NAMESPACE" rollout status "daemonset/${ROOT_NAME}-agent" --timeout=5m ||
		die "the agent DaemonSet did not roll out after host tooling was armed; --require-real-driver refuses to start without nvidia-smi, so check binDir/libDirs against this AMI's NVIDIA userspace layout"

	agent_pod=$(kubectl -n "$RUNTIME_NAMESPACE" get pods \
		-l "app.kubernetes.io/instance=${ROOT_NAME},app.kubernetes.io/component=agent" \
		--field-selector "spec.nodeName=$node" -o jsonpath='{.items[0].metadata.name}')
	[ -n "$agent_pod" ] || die "no agent pod on the GPU node $node"

	if kubectl -n "$RUNTIME_NAMESPACE" logs "$agent_pod" --tail=-1 2>/dev/null |
		grep -q "using fake GPU driver"; then
		die "the agent on $node fell back to the fake GPU driver on real hardware; every NVIDIA assertion after this point would pass against a simulator"
	fi
	log "deploy: the agent on $node is on a real driver"
	assert_dcgm_versions_agree "$agent_pod"
}

# assert_dcgm_versions_agree refuses to run the DCGM phases against an engine
# the agent's own client cannot speak to.
#
# Without it the run reaches test-dcgm forty minutes later and fails there for a
# reason nothing in the output names, having proved nothing. The first real run
# did exactly that. Majors only: DCGM keeps wire compatibility within a major
# and breaks it across one, which is the failure that was actually observed
# (client 4.6.1, engine 3.3.8, "API version mismatch" on every call).
assert_dcgm_versions_agree() {
	local agent_pod="$1" client engine
	client=$(kubectl -n "$RUNTIME_NAMESPACE" exec "$agent_pod" -- /usr/bin/dcgmi --version 2>/dev/null |
		grep -oE '[0-9]+\.[0-9]+\.[0-9]+' | head -1)
	engine=$(kubectl -n gpu-operator get pods -l app=nvidia-dcgm \
		-o jsonpath='{.items[0].spec.containers[0].image}' 2>/dev/null |
		grep -oE '[0-9]+\.[0-9]+\.[0-9]+' | head -1)
	if [ -z "$client" ] || [ -z "$engine" ]; then
		die "cannot read the dcgmi client (${client:-unknown}) or the DCGM engine (${engine:-unknown}) version; the DCGM phases would be unattributable"
	fi
	# The engine must be NO OLDER than the client, not merely the same major.
	#
	# Requiring only a matching major was this assertion's first form, and the
	# second hardware run walked straight through it: client 4.6.1 against
	# engine 4.3.1 passed, then dcgmi connected, reported the GPU as found, and
	# failed every field read with "Return -20: The requested function was not
	# found". DCGM tolerates an engine newer than the client and not the
	# reverse, and the reverse is the worse failure because it looks like it is
	# working.
	if ! printf '%s\n%s\n' "$client" "$engine" | sort -V -C; then
		die "the agent's dcgmi client is ${client} and the DCGM host engine is ${engine}: DCGM tolerates an engine NEWER than the client and not the reverse, so the client would connect, find the GPU, and fail every field read with \"function was not found\" while the agent quietly served nvidia-smi. Raise GPU_OPERATOR_VERSION until its engine is at least ${client}, or lower DCGM_VERSION in build/Dockerfile."
	fi
	log "deploy: dcgmi client ${client} is not newer than DCGM engine ${engine}"
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

	# Assert the mode, not the audit entry.
	#
	# This used to re-assert that a Reboot entry exists in the audit — which the
	# approval assertion four lines above already proves, and which is written
	# identically whether the step simulated or rebooted a T4. The phase called
	# "dry-run" could not distinguish DryRun from Enabled. That is the same
	# shape of defect as the inert executionMode found in round 14, and this
	# phase was structurally unable to catch it.
	#
	# The controller's own answer can fail: it reports what the loaded Gate is
	# doing, not what the CR asks for or what digest the operator stamped.
	log "test-dryrun: asserting the controller was in dry-run while that step ran"
	local mode
	mode=$(api GET /api/v1/runtime-config | jq -r '.execution_mode')
	[ "$mode" = "dry-run" ] ||
		die "the controller reports execution_mode='${mode}', not dry-run; this phase just approved a destructive ladder in ${mode} mode on real hardware"

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
# dcgm_diagnostics prints what the run needs to name a DCGM failure: the
# agent's own account, and the two version numbers that have to agree. The
# first hardware run failed here for a reason nothing in the output stated —
# the agent's dcgmi client was 4.6.1 and the operator's host engine 3.3.8, so
# every poll returned "API version mismatch" and the agent quietly served the
# narrower nvidia-smi source instead.
dcgm_diagnostics() {
	local agent_pod="$1"
	log "test-dcgm: the agent's own account of the DCGM path:"
	kubectl -n "$RUNTIME_NAMESPACE" logs "$agent_pod" --tail=-1 2>/dev/null |
		grep -iE 'DCGM health probe|dmon printed rows' | tail -3 >&2 || true
	log "test-dcgm: client vs engine version (these must agree on major):"
	kubectl -n "$RUNTIME_NAMESPACE" exec "$agent_pod" -- /usr/bin/dcgmi --version 2>&1 | tail -1 >&2 || true
	kubectl -n gpu-operator get pods -l app=nvidia-dcgm \
		-o jsonpath='{.items[0].spec.containers[0].image}' 2>/dev/null >&2 || true
	echo >&2
}

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
		# The injection runs the OPERATOR's dcgmi against its own engine, so it
		# succeeds even when the AGENT's client cannot read the same engine —
		# which is exactly what the first hardware run found. Without the trap
		# below this branch times out with no diagnosis at all.
		if ! wait_for "$CONFIRM_INCIDENT_TIMEOUT" \
			'[ "$(gpuhealth_detections '"$agent_ip"')" -gt '"$baseline"' ]'; then
			dcgm_diagnostics "$agent_pod"
			die "the agent never observed an XID injected straight into the DCGM engine on its own node"
		fi
		# The detection this phase just proved opens an INCIDENT, and every
		# later phase shares this node. Close it before leaving, or the next
		# phase inherits it — which is exactly what happened the first time
		# this phase passed.
		local opened
		opened=$(wait_for_incident "$node" ecc-sbe-rate 2>/dev/null || true)
		if [ -n "$opened" ]; then
			close_incident "$opened" "dcgm phase complete"
		fi
		log "test-dcgm: PASS (gpuhealth source observed the injected DCGM fault)"
		return 0
	fi

	# The fallback. Two assertions, because the first version of this branch
	# asserted only the absence of a parse warning — and parseDCGMXID had no
	# code path that could emit one, so it passed without proving anything and
	# the release notes then claimed the DCGM source had been exercised.
	log "test-dcgm: DCGM injection unavailable; asserting the agent actually reads the live dmon layout"

	# 1. The warning now exists, so its absence means something. An agent that
	#    cannot parse this DCGM build's layout says so once, loudly.
	if kubectl -n "$RUNTIME_NAMESPACE" logs "$agent_pod" --tail=-1 2>/dev/null |
		grep -qiE "dmon printed rows this build cannot parse"; then
		die "the agent cannot parse this DCGM build's dmon layout; the DCGM detection source is dark"
	fi

	# 2. Positive evidence that the DCGM path is the one serving this node. A
	#    parser that is never invoked also emits no warning, which is the hole
	#    the assertion above cannot cover alone: an agent quietly falling back
	#    to the narrower nvidia-smi source would pass it.
	local active
	active=$(agent_health_source "$agent_ip")
	if [ "$active" != "dcgm" ]; then
		# Print the agent's own diagnosis before dying. The first run to reach
		# this assertion failed for a reason nothing in the output named: the
		# dcgmi client in the agent image and the operator's host engine
		# disagreed on API version, so every poll failed and the agent fell
		# back to the narrower source. Reading it out of the log here is the
		# difference between "the source is wrong" and "here is why".
		dcgm_diagnostics "$agent_pod"
		die "the agent's active health source is '${active}', not dcgm; this phase would prove nothing about DCGM"
	fi
	log "test-dcgm: PASS (fallback: the DCGM source is serving this node and parses its dmon layout)"
}

# cmd_test_verify_recur asserts the verification NEGATIVE path: a signal that
# recurs while an incident is VERIFYING must escalate, not quiet-resolve.
# UNEXERCISED LIVE: written after the first green run; validate on the next
# paid stand before trusting a red result.
# close_incident resolves an incident so the NEXT phase cannot inherit it.
#
# Incidents correlate by (node, class), and NEEDS_HUMAN is deliberately not
# halted for correlation purposes — so an incident this phase leaves open
# ATTACHES the next phase's fault to itself instead of opening a fresh one.
# The first real run showed what that costs: a phase died on a bad assertion
# before reaching its cleanup line, and the destructive phase then spent ten
# minutes waiting for AWAITING_APPROVAL on an incident that was already parked
# in NEEDS_HUMAN. One phase's bug became three phases' failures.
#
# Registered as a trap by its callers, so it runs whether the phase passes,
# fails an assertion, or dies.
close_incident() {
	local incident="$1" reason="$2"
	[ -n "$incident" ] || return 0
	api POST "/api/v1/incidents/${incident}/resolve" \
		"{\"actor\":\"hw-e2e\",\"reason\":\"${reason}\"}" >/dev/null 2>&1 || true
}

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
	# Recorded HERE, not after the assertions: the point is that the cleanup
	# runs when an assertion FAILS, which is the case that poisoned the next
	# phase.
	#
	# A `trap ... RETURN` was the obvious way and it was wrong twice. Bash runs
	# a RETURN trap when a function returns, not when the shell exits — and
	# every assertion below leaves through `die`, which exits — so it never
	# fired for the case it was written for. It then fired somewhere it had no
	# business firing, on main's own return, where $incident is out of scope:
	# under `set -u` the phase PASSED its assertions and the script died anyway
	# with "incident: unbound variable".
	#
	# The script already has one EXIT trap that runs on every path. Use it.
	_OPEN_INCIDENT="$incident"
	wait_for "$CONFIRM_INCIDENT_TIMEOUT" \
		'api GET "/api/v1/incidents/'"$incident"'" | jq -e ".incident.state==\"AWAITING_APPROVAL\"" >/dev/null'
	api POST "/api/v1/incidents/$incident/approve" '{"actor":"hw-e2e-approver"}' >/dev/null
	wait_for "$CONFIRM_INCIDENT_TIMEOUT" \
		'api GET "/api/v1/incidents/'"$incident"'" | jq -e ".incident.state==\"VERIFYING\"" >/dev/null'

	# Recur with a DIFFERENT code mapped to the same class: dedup keys on
	# (gpu, xid), so this is accepted at once and attaches to the open
	# incident, while the same code would be swallowed for two minutes —
	# far longer than the thirty-second quiet window it must land inside.
	log "test-verify-recur: injecting XID $RECUR_XID (same class) inside the verification quiet window"
	inject_xid "$node" "$RECUR_XID"
	# Diagnose before dying, rather than leaving the next reader to guess from
	# a bare timeout. Run 4 failed here with nothing but "condition never met",
	# and the cluster was gone by the time anyone looked.
	if ! wait_for "$CONFIRM_INCIDENT_TIMEOUT" \
		'api GET "/api/v1/incidents/'"$incident"'" \
			 | jq -e ".incident.state!=\"VERIFYING\" and .incident.state!=\"RESOLVED\"" >/dev/null'; then
		log "test-verify-recur: the incident never left VERIFYING; here is what the controller saw"
		api GET "/api/v1/incidents/$incident" |
			jq -r '"state=\(.incident.state) signals=\(.incident.signal_seen) updated=\(.incident.updated_at)"' >&2 || true
		api GET "/api/v1/incidents/$incident" |
			jq -r '.audit[] | "  \(.time) \(.action): \(.result // "")"' | tail -8 >&2 || true
		log "test-verify-recur: every open incident on this node, in case the recurrence landed on another one"
		api GET "/api/v1/incidents" |
			jq -r '.[] | select(.target.node=="'"$node"'") | "  \(.id) \(.state) \(.class)"' >&2 || true
		die "the recurrence injected inside the quiet window did not move the incident out of VERIFYING"
	fi
	local state
	state=$(api GET "/api/v1/incidents/$incident" | jq -r .incident.state)

	# Assert WHY it left VERIFYING, not merely that it did.
	#
	# Leaving VERIFYING for anything other than RESOLVED satisfies the wait
	# above — an approval timeout, an unrelated escalation, a store error. The
	# phase would report PASS for a recurrence it never detected. The audit
	# names the cause, so require it.
	# `.result // ""` because an audit row may legitimately carry no result —
	# jq's test() errors on null and takes the whole filter down with it, so the
	# assertion failed on rows it was not even asking about. Found on the first
	# hardware run: the product had done exactly the right thing (the audit read
	# "verification failed: signal recurred during quiet window") and the phase
	# still reported failure.
	api GET "/api/v1/incidents/$incident" |
		jq -e '[.audit[] | select((.result // "") | test("signal recurred during quiet window"))] | length > 0' >/dev/null ||
		die "the incident left VERIFYING as $state, but no audit entry attributes it to a recurrence; this phase proved nothing about the quiet window"
	# Close it: an escalated incident stays OPEN (NEEDS_HUMAN is not halted
	# for correlation purposes), and the next phase's fault on the same
	# node+class would ATTACH to it instead of opening its own incident.
	close_incident "$incident" "verification-recurrence phase complete"
	_OPEN_INCIDENT=""
	log "test-verify-recur: PASS (recurrence during VERIFYING escalated to $state, not RESOLVED)"
}

# cmd_test_drain exercises the one destructive step this stand has been running
# for eight paid runs without ever loading it.
#
# The ReplaceNode ladder does contain a real Drain, and it runs in Enabled mode
# — but the stand puts no workload on the GPU node, so that drain has always
# walked an empty pod list, and nothing has ever asserted anything about it. The
# most expensive defect found in round 26 lived in exactly that unexercised
# code, which is why it survived four green runs.
#
# What this asserts, in order:
#
#   1. A node carrying a pod with no controller is REFUSED, and refused with
#      NOTHING EVICTED. That second half is the whole point: the first version
#      of this refusal collected the unmanaged names inside the eviction loop,
#      so it evicted every managed pod, waited for them to terminate, and only
#      then refused. Doing the disruption and then declining is worse than
#      either choice on its own, and only a real drain over a real pod list can
#      tell the two implementations apart.
#   2. Once the unmanaged pod is gone, the same ladder drains the node for real
#      and the tenant workload is actually evicted.
#
# UNEXERCISED LIVE: written after run 8; validate on the next paid stand before
# trusting a red result from it.
cmd_test_drain() {
	guard_cluster_name
	require_cmd kubectl
	require_cmd jq
	require_cmd curl
	local node
	node=$(gpu_node)
	assert_kube_target

	log "test-drain: placing a managed tenant workload and one bare pod on $node"
	kubectl apply -f - <<EOF
apiVersion: apps/v1
kind: Deployment
metadata:
  name: e2e-tenant
  namespace: default
  labels: {kubeneuron.io/hw-e2e: "true"}
spec:
  replicas: 1
  selector: {matchLabels: {app: e2e-tenant}}
  template:
    metadata: {labels: {app: e2e-tenant}}
    spec:
      nodeName: ${node}
      tolerations: [{operator: Exists}]
      terminationGracePeriodSeconds: 5
      containers:
        - name: sleep
          image: busybox:1.36
          command: ["sh", "-c", "sleep 3600"]
EOF
	# A bare pod: no controller, so nothing would recreate it elsewhere. This is
	# what `kubectl debug node/...` leaves behind, and what an engineer's
	# `kubectl run` creates.
	kubectl apply -f - <<EOF
apiVersion: v1
kind: Pod
metadata:
  name: e2e-bare
  namespace: default
  labels: {kubeneuron.io/hw-e2e: "true"}
spec:
  nodeName: ${node}
  tolerations: [{operator: Exists}]
  terminationGracePeriodSeconds: 5
  containers:
    - name: sleep
      image: busybox:1.36
      command: ["sh", "-c", "sleep 3600"]
EOF
	wait_for 180 'kubectl -n default get pod e2e-bare -o jsonpath="{.status.phase}" | grep -q Running'
	wait_for 180 'kubectl -n default get pods -l app=e2e-tenant \
		-o jsonpath="{.items[0].status.phase}" | grep -q Running'

	log "test-drain: confining destructive execution to the e2e GPU node"
	kubectl -n "$RUNTIME_NAMESPACE" patch kubeneuron "$ROOT_NAME" --type merge -p "$(
		cat <<EOF
{"spec":{"safety":{"executionMode":"Enabled",
   "destructiveExecution":{"nodeSelector":{"kubeneuron.io/e2e":"true"},
     "acknowledgement":"${XID_ACK}"}}}}
EOF
	)"
	apply_drain_playbook
	wait_for_installation_ready
	# Wait for the CONTROLLER, not the operator — dry_run is stamped on the
	# incident at OPEN and never revisited, so an incident born one tick early
	# is a simulation for its whole life and every assertion below passes
	# vacuously. Assert the blast radius too: Enabled with the wrong
	# confinement is a destructive action aimed at the wrong machines.
	wait_for 300 'api GET /api/v1/runtime-config |
		jq -e ".execution_mode==\"enabled\" and .confinement.\"kubeneuron.io/e2e\"==\"true\"" >/dev/null'

	log "test-drain: injecting XID $DRAIN_XID; the drain must REFUSE while the bare pod is there"
	inject_xid "$node" "$DRAIN_XID"
	local incident
	incident=$(wait_for_incident "$node" drain-probe)
	[ -n "$incident" ] || die "no drain-probe incident opened"
	_OPEN_INCIDENT="$incident"
	log "test-drain: incident $incident opened"

	# The ladder has no escalation target, so a failed Drain parks it.
	wait_for 600 'api GET "/api/v1/incidents/'"$incident"'" |
		jq -e ".incident.state==\"NEEDS_HUMAN\"" >/dev/null' ||
		die "the drain did not refuse a node carrying a pod with no controller"

	# The refusal must NAME the pod, so an operator knows what to do about it.
	api GET "/api/v1/incidents/$incident" |
		jq -e 'any(.audit[]; .action=="Drain" and (.result|test("default/e2e-bare")))' >/dev/null ||
		die "the drain refused without naming the pod that blocked it"

	# And — the assertion this phase exists for — it refused having evicted
	# NOTHING. A tenant workload still running here is the difference between a
	# pre-flight refusal and a refusal issued after the damage.
	kubectl -n default get pods -l app=e2e-tenant \
		-o jsonpath='{.items[0].status.phase}' | grep -q Running ||
		die "the drain evicted the tenant workload and THEN refused; the refusal is not a pre-flight"
	log "test-drain: PASS (refused, named the pod, evicted nothing)"

	close_incident "$incident" "drain refusal asserted"
	_OPEN_INCIDENT=""
	kubectl -n default delete pod e2e-bare --wait=true --timeout=120s >/dev/null

	log "test-drain: with the bare pod gone, the same ladder must drain for real"
	kubectl uncordon "$node" >/dev/null 2>&1 || true
	inject_xid "$node" "$DRAIN_RECUR_XID"
	incident=$(wait_for_incident "$node" drain-probe)
	[ -n "$incident" ] || die "no second drain-probe incident opened"
	_OPEN_INCIDENT="$incident"

	wait_for 900 'kubectl -n default get pods -l app=e2e-tenant \
		-o jsonpath="{.items[0].spec.nodeName}" 2>/dev/null | grep -qv "'"$node"'"' ||
		die "the drain did not evict the tenant workload off $node"
	log "test-drain: PASS (real drain evicted the tenant workload)"

	close_incident "$incident" "drain success asserted"
	_OPEN_INCIDENT=""
	kubectl uncordon "$node" >/dev/null 2>&1 || true
	kubectl -n default delete deployment e2e-tenant --wait=false >/dev/null 2>&1 || true
}

# apply_drain_playbook installs a Cordon -> Drain ladder with NO escalation
# target, so a refused drain parks the incident in NEEDS_HUMAN instead of
# climbing to something more destructive. That is what makes the refusal
# observable as a state rather than inferred from a later step's absence.
apply_drain_playbook() {
	kubectl apply -f - <<EOF
apiVersion: kubeneuron.io/v1alpha1
kind: GPUPlaybook
metadata:
  name: ${ROOT_NAME}-e2e-drain-probe
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
      timeout: 5m
---
apiVersion: kubeneuron.io/v1alpha1
kind: GPURemediationPolicy
metadata:
  name: ${ROOT_NAME}-e2e-drain-probe
  labels: {kubeneuron.io/hw-e2e: "true"}
spec:
  kubeNeuronRef: ${ROOT_NAME}
  priority: 5
  match:
    class: drain-probe
  playbookRef: ${ROOT_NAME}-e2e-drain-probe
---
apiVersion: kubeneuron.io/v1alpha1
kind: GPUSignalMapping
metadata:
  name: ${ROOT_NAME}-e2e-drain-xid
  labels: {kubeneuron.io/hw-e2e: "true"}
spec:
  kubeNeuronRef: ${ROOT_NAME}
  source: xid
  xidCodes: [${DRAIN_XID}, ${DRAIN_RECUR_XID}]
  class: drain-probe
  severity: critical
EOF
	wait_for_configured_object gpuplaybook "${ROOT_NAME}-e2e-drain-probe"
	wait_for_configured_object gpuremediationpolicy "${ROOT_NAME}-e2e-drain-probe"
	wait_for_configured_object gpusignalmapping "${ROOT_NAME}-e2e-drain-xid"
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

	# Wait for the CONTROLLER, not the operator.
	#
	# wait_for_installation_ready above answers a different question: it says the
	# operator observed the new generation and stamped the root object Ready. The
	# controller reloads its configuration in place, so there is a window in
	# which the CR says Enabled, the digest has moved, the root object is Ready —
	# and the running Gate is still in DryRun.
	#
	# The cost of injecting inside that window is not a retry. dry_run is stamped
	# on the incident at OPEN and never revisited, so an incident born one tick
	# early is dry-run for its whole life, and this phase then spends fifteen
	# minutes waiting for a `terminated` result that can never arrive. Three kind
	# runs were lost to precisely this before the endpoint existed to prevent it.
	#
	# Assert the blast radius too. Enabled with the wrong confinement is not a
	# slower failure, it is a destructive action aimed at the wrong machines.
	log "test-destructive: waiting for the controller to actually load Enabled + the confinement"
	wait_for 300 'api GET /api/v1/runtime-config |
		jq -e ".execution_mode==\"enabled\" and .confinement.\"kubeneuron.io/e2e\"==\"true\"" >/dev/null'

	verify_controller_irsa

	log "test-destructive: injecting distinct test-only XID $DESTRUCTIVE_XID into $node"
	inject_xid "$node" "$DESTRUCTIVE_XID"
	local incident
	# Filtered by CLASS, like every other phase. This was the one that was not,
	# and it stopped mattering only because the phases before it used to fail.
	# Once test-dcgm started passing it left an ecc-sbe-rate incident behind,
	# and this unfiltered wait attached to THAT — so the destructive phase
	# waited for approval on somebody else's incident and timed out after ten
	# minutes on a paid cluster.
	incident=$(wait_for_incident "$node" fell-off-bus)
	[ -n "$incident" ] || die "no fell-off-bus incident opened for the destructive run"
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

	log "sweep: cluster must be gone"
	if eksctl get cluster --name "$CLUSTER_NAME" --region "$AWS_REGION" >/dev/null 2>&1; then
		log "sweep: cluster still present, retrying delete --force"
		eksctl delete cluster --name "$CLUSTER_NAME" --region "$AWS_REGION" --force --wait || true
	fi

	log "sweep: no non-terminated e2e EC2 instances"
	# Two independent queries, unioned, because the first one's premise is not
	# guaranteed. kubeneuron:e2e and the run tag are declared on the eksctl
	# managedNodeGroups; whether EKS propagates node-group tags down to the EC2
	# instances is version-dependent, and historically it did not. If it does
	# not, that filter matches nothing and this check reports "clean" while the
	# instances bill by the hour — a sweep that cannot see a leak is worse than
	# no sweep, because it is written down as proof.
	#
	# aws:eks:cluster-name is stamped by EKS itself on every managed node, so it
	# holds whether or not our tags propagated.
	local instances tagged_instances eks_instances
	tagged_instances=$(aws ec2 describe-instances --region "$AWS_REGION" \
		--filters "Name=tag:kubeneuron:e2e,Values=true" \
		"Name=tag:${E2E_RUN_TAG},Values=${CLUSTER_NAME}" \
		"Name=instance-state-name,Values=pending,running,stopping,stopped" \
		--query 'Reservations[].Instances[].InstanceId' --output text)
	eks_instances=$(aws ec2 describe-instances --region "$AWS_REGION" \
		--filters "Name=tag:aws:eks:cluster-name,Values=${CLUSTER_NAME}" \
		"Name=instance-state-name,Values=pending,running,stopping,stopped" \
		--query 'Reservations[].Instances[].InstanceId' --output text)
	# `|| true` because grep exits 1 when it selects nothing, and selecting
	# nothing is the ORDINARY case: a clean account. Under this file's `set -o
	# pipefail` that exit status failed the assignment and returned from
	# cmd_sweep — so on every successful run the teardown step reported failure
	# and the CloudFormation, EBS, IAM-role and ECR checks after it never ran.
	# A cost guard that only executes when there is already a leak is not one.
	instances=$(printf '%s %s' "$tagged_instances" "$eks_instances" | tr -s ' \t' '\n' |
		grep -v '^$' | sort -u | tr '\n' ' ' || true)
	instances=${instances% }
	if [ -n "$instances" ]; then
		log "sweep: terminating leaked instances: $instances"
		# shellcheck disable=SC2086 # word-splitting the id list is intended.
		aws ec2 terminate-instances --region "$AWS_REGION" --instance-ids $instances >/dev/null || true
		# shellcheck disable=SC2086 # word-splitting the id list is intended.
		aws ec2 wait instance-terminated --region "$AWS_REGION" --instance-ids $instances || true
	fi

	log "sweep: no e2e CloudFormation stacks"
	local stacks
	# DELETE_FAILED belongs at the front of this list, not off it. It is the
	# single most likely state for a leaked eksctl VPC or nodegroup stack —
	# an orphaned ENI or a security group still in use wedges the delete — and
	# it is precisely the state that means "something is stuck and costing
	# money". Omitting it made the one condition worth sweeping for invisible
	# to the sweep.
	stacks=$(aws cloudformation list-stacks --region "$AWS_REGION" \
		--stack-status-filter CREATE_COMPLETE UPDATE_COMPLETE ROLLBACK_COMPLETE \
		CREATE_FAILED DELETE_FAILED ROLLBACK_FAILED UPDATE_ROLLBACK_COMPLETE \
		UPDATE_ROLLBACK_FAILED \
		--query "StackSummaries[?starts_with(StackName, 'eksctl-${CLUSTER_NAME}-')].StackName" \
		--output text)
	local stack
	for stack in $stacks; do
		log "sweep: deleting leaked stack $stack"
		aws cloudformation delete-stack --region "$AWS_REGION" --stack-name "$stack" || true
		aws cloudformation wait stack-delete-complete --region "$AWS_REGION" --stack-name "$stack" || true
	done

	log "sweep: no orphaned e2e EBS volumes"
	local volumes
	volumes=$(aws ec2 describe-volumes --region "$AWS_REGION" \
		--filters "Name=tag:kubeneuron:e2e,Values=true" \
		"Name=tag:${E2E_RUN_TAG},Values=${CLUSTER_NAME}" "Name=status,Values=available" \
		--query 'Volumes[].VolumeId' --output text)
	# Also catch the controller's PVC volume — the 5Gi SQLite claim, which is
	# the leak this check was written for after a real run left one behind.
	#
	# KubernetesCluster is the LEGACY in-tree provisioner's tag. EKS provisions
	# through the EBS CSI driver, which does not write it; the tags it does
	# write are CSIVolumeName and kubernetes.io/created-for/pvc/*. So the filter
	# aimed at that volume could not match that volume. Query by what the CSI
	# driver actually writes, and keep the legacy filter for older clusters.
	local cluster_volumes csi_volumes
	cluster_volumes=$(aws ec2 describe-volumes --region "$AWS_REGION" \
		--filters "Name=tag:KubernetesCluster,Values=${CLUSTER_NAME}" "Name=status,Values=available" \
		--query 'Volumes[].VolumeId' --output text)
	csi_volumes=$(aws ec2 describe-volumes --region "$AWS_REGION" \
		--filters "Name=tag:kubernetes.io/created-for/pvc/namespace,Values=${RUNTIME_NAMESPACE}" \
		"Name=status,Values=available" \
		--query 'Volumes[].VolumeId' --output text)
	# Deduplicate. The three filters deliberately overlap — that is what makes
	# them a safety net — so a volume matched by two of them was deleted twice,
	# and the second delete's failure set the leak flag. The first real run
	# ended with "sweep found leftovers it could not delete" on an account that
	# was, in fact, clean.
	local vol
	for vol in $(printf '%s %s %s' "$volumes" "$cluster_volumes" "$csi_volumes" |
		tr -s ' \t' '\n' | grep -v '^$' | sort -u || true); do
		log "sweep: deleting orphaned volume $vol"
		aws ec2 delete-volume --region "$AWS_REGION" --volume-id "$vol" || true
	done

	log "sweep: delete any manually-created recycle IAM role ($RECYCLE_ROLE_NAME)"
	delete_iam_role "$RECYCLE_ROLE_NAME" || true

	# The images this run pushed. Every run pushes three at a fresh e2e-<stamp>
	# tag and nothing ever removed them, so "the sweep leaves nothing behind"
	# was false by construction — cheap, but this script's whole claim is that
	# it can be believed about cost.
	log "sweep: delete this run's ECR images (tag $IMAGE_TAG)"
	local repo
	for repo in operator controller agent; do
		aws ecr batch-delete-image --region "$AWS_REGION" \
			--repository-name "kubeneuron/$repo" \
			--image-ids "imageTag=$IMAGE_TAG" >/dev/null 2>&1 || true
	done

	# Judge the END STATE, not the exit codes of the deletions.
	#
	# Every delete above sets leaks=1 when its command returns non-zero, and
	# several of them legitimately do so while succeeding: `wait
	# stack-delete-complete` on a stack that is already gone, a second delete of
	# a volume two filters both matched, a role another pass removed. The fifth
	# hardware run ended with "sweep found leftovers it could not delete" on an
	# account that was, on inspection, completely empty — which is the same
	# false alarm in the opposite direction from the filters that reported clean
	# while resources ran.
	#
	# So the deletions above are best-effort, and this is the assertion: ask AWS
	# what is actually there. A leak is a resource that still exists, not a
	# command that returned non-zero.
	local remaining=""
	# An explicit `if`, not `[ … ] && [ … ] && assign`. The && chain returns 1
	# when the condition is false — the ordinary case, a resource that is
	# absent — and under `set -e` that killed the sweep at the first clean
	# check. The assertion written to replace a false alarm produced one.
	add_remaining() {
		if [ -n "$2" ] && [ "$2" != "None" ]; then
			remaining="${remaining}\n  ${1}: ${2}"
		fi
	}

	add_remaining "cluster" "$(aws eks list-clusters --region "$AWS_REGION" \
		--query "clusters[?@=='${CLUSTER_NAME}']|join(',',@)" --output text 2>/dev/null || true)"
	# The assertion must be at least as WIDE as the deletions it verifies, or a
	# resource the sweep deleted best-effort and missed is never checked — the
	# file's own words: a sweep that cannot see a leak is worse than no sweep,
	# because it is written down as proof. Both instance filters, all three
	# volume filters.
	add_remaining "instances" "$(aws ec2 describe-instances --region "$AWS_REGION" \
		--filters "Name=tag:aws:eks:cluster-name,Values=${CLUSTER_NAME}" \
		"Name=instance-state-name,Values=pending,running,stopping,stopped" \
		--query "Reservations[].Instances[].InstanceId|join(',',@)" --output text 2>/dev/null || true)"
	add_remaining "instances (run-tagged)" "$(aws ec2 describe-instances --region "$AWS_REGION" \
		--filters "Name=tag:kubeneuron:e2e,Values=true" \
		"Name=tag:${E2E_RUN_TAG},Values=${CLUSTER_NAME}" \
		"Name=instance-state-name,Values=pending,running,stopping,stopped" \
		--query "Reservations[].Instances[].InstanceId|join(',',@)" --output text 2>/dev/null || true)"
	add_remaining "stacks" "$(aws cloudformation list-stacks --region "$AWS_REGION" \
		--query "StackSummaries[?starts_with(StackName,'eksctl-${CLUSTER_NAME}-') && StackStatus!='DELETE_COMPLETE'].StackName|join(',',@)" \
		--output text 2>/dev/null || true)"
	# `status` is filtered, and `deleting` is deliberately excluded: delete-volume
	# is asynchronous, so a volume the sweep just removed sits in `deleting` for
	# up to a minute and only the three fast ECR deletes separate the loop from
	# this check. Without the filter a SUCCESSFUL sweep reports the account
	# unclean — the same false alarm this assertion replaced, reintroduced
	# inside it.
	local volume_states="Name=status,Values=available,in-use,error"
	add_remaining "volumes (csi)" "$(aws ec2 describe-volumes --region "$AWS_REGION" \
		--filters "Name=tag:kubernetes.io/created-for/pvc/namespace,Values=${RUNTIME_NAMESPACE}" \
		"$volume_states" \
		--query "Volumes[].VolumeId|join(',',@)" --output text 2>/dev/null || true)"
	add_remaining "volumes (run-tagged)" "$(aws ec2 describe-volumes --region "$AWS_REGION" \
		--filters "Name=tag:kubeneuron:e2e,Values=true" \
		"Name=tag:${E2E_RUN_TAG},Values=${CLUSTER_NAME}" "$volume_states" \
		--query "Volumes[].VolumeId|join(',',@)" --output text 2>/dev/null || true)"
	add_remaining "volumes (legacy tag)" "$(aws ec2 describe-volumes --region "$AWS_REGION" \
		--filters "Name=tag:KubernetesCluster,Values=${CLUSTER_NAME}" "$volume_states" \
		--query "Volumes[].VolumeId|join(',',@)" --output text 2>/dev/null || true)"
	add_remaining "recycle role" "$(aws iam get-role --role-name "$RECYCLE_ROLE_NAME" \
		--query "Role.RoleName" --output text 2>/dev/null || true)"

	if [ -n "$remaining" ]; then
		printf 'sweep: these still exist after the deletions:%b\n' "$remaining" >&2
		die "the account is not clean — investigate before the next run"
	fi
	# ECR is deliberately absent from the assertion and therefore from this
	# line: the images are deleted best-effort and cost cents, and probing them
	# would claim a verification this does not perform.
	log "sweep: clean (verified against AWS: no cluster, EC2, stacks, volumes or recycle role; run images deleted best-effort)"
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
	# Enumerate through the EKS API, not eksctl.
	#
	# This used to parse `eksctl get cluster -o json` with `.[].Name // .[].name`,
	# which assumes one of two flat shapes. eksctl has also emitted the nested
	# {"metadata":{"name":...}} form, and against that BOTH selectors are null,
	# jq prints "null", the prefix grep drops it, and the reaper reports success
	# having reaped nothing. A watchdog whose enumeration can silently return
	# empty is indistinguishable from a clean account — which is the exact
	# failure it exists to prevent, and it would only ever be discovered by the
	# bill.
	#
	# aws eks list-clusters has one documented shape and is the authority on
	# what exists. It also drops the jq dependency this function never declared.
	names=$(aws eks list-clusters --region "$AWS_REGION" \
		--query "clusters[?starts_with(@, '${E2E_PREFIX}')]" --output text 2>/dev/null |
		tr '\t' '\n' | grep -v '^$' || true)
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
# agent_health_source prints which second-source probe served the agent's last
# poll: dcgm, nvidia-smi, or none.
agent_health_source() {
	local ip="$1" out
	out=$(kubectl -n "$RUNTIME_NAMESPACE" run "kn-src-$RANDOM" --rm -i --restart=Never \
		--image=curlimages/curl:8.10.1 --quiet -- \
		curl -fsS --max-time 10 "http://${ip}:9402/metrics" 2>/dev/null |
		grep -E '^kubeneuron_agent_health_source\{source="[a-z-]+"\} 1$' |
		sed -E 's/.*source="([a-z-]+)".*/\1/' | head -1 || true)
	printf '%s' "${out:-none}"
}

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
  xidCodes: [${DESTRUCTIVE_XID}, ${RECUR_XID}]
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
	# shellcheck disable=SC2015 # both conjuncts failing must die; that is the intent.
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
#
# Every call is bounded. A port-forward keeps its LOCAL listener up after the
# remote end dies, so the TCP connect succeeds and nothing ever answers: an
# unbounded curl there waits forever. That is not hypothetical — it hung the
# EXIT trap of run 8 for two hours with the cluster still billing, which is the
# second time this stand's cost guarantee died on an unbounded cleanup call
# (the first was a teardown piped through tee).
#
# API_MAX_TIME is generous enough for the slowest real call and finite, which
# is the only property that matters here.
api() {
	local method="$1" path="$2" body="${3:-}"
	_ensure_portforward
	local base="http://127.0.0.1:${_API_LOCAL_PORT}"
	if [ "$method" = POST ]; then
		curl -fsS --max-time "$API_MAX_TIME" -X POST -H 'Content-Type: application/json' \
			-H "Authorization: Bearer ${_API_TOKEN}" \
			--data "$body" "${base}${path}"
	else
		curl -fsS --max-time "$API_MAX_TIME" \
			-H "Authorization: Bearer ${_API_TOKEN}" "${base}${path}"
	fi
}

# API_MAX_TIME bounds every controller API call. No network call in this script
# may be unbounded; see api().
API_MAX_TIME="${API_MAX_TIME:-30}"

_API_LOCAL_PORT=""
_API_TOKEN=""
_PF_PID=""
_REPOINT_PID=""
_ensure_portforward() {
	# A LIVE process is not a live tunnel. kubectl port-forward keeps its
	# listener bound after the remote connection breaks, so kill -0 alone
	# happily returned a forward that answered nothing — which is how run 8
	# came to hang. Probe it, cheaply, and rebuild it if it has gone deaf.
	if [ -n "$_PF_PID" ] && kill -0 "$_PF_PID" 2>/dev/null; then
		if curl -fsS --max-time 5 -o /dev/null \
			"http://127.0.0.1:${_API_LOCAL_PORT}/healthz" 2>/dev/null; then
			return 0
		fi
		kill "$_PF_PID" 2>/dev/null || true
		_PF_PID=""
	fi
	_API_TOKEN=$(kubectl -n "$RUNTIME_NAMESPACE" get secret kubeneuron-operator-api-token \
		-o jsonpath='{.data.token}' | base64 -d)
	_API_LOCAL_PORT=18080
	kubectl -n "$RUNTIME_NAMESPACE" port-forward \
		"service/${ROOT_NAME}-controller" "${_API_LOCAL_PORT}:8080" >/dev/null 2>&1 &
	_PF_PID=$!
	wait_for 60 'curl -fsS --max-time 5 -o /dev/null "http://127.0.0.1:'"$_API_LOCAL_PORT"'/healthz"'
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

# _OPEN_INCIDENT is the incident a phase must not leave behind. Incidents
# correlate by (node, class) and NEEDS_HUMAN is deliberately not halted for
# correlation, so an incident one phase leaves open ATTACHES the next phase's
# fault to itself. On the first hardware run that turned one phase's bad
# assertion into three failed phases.
_OPEN_INCIDENT="${_OPEN_INCIDENT:-}"

# cleanup must always terminate.
#
# It runs from the EXIT trap, which is the last thing standing between a failed
# phase and a cluster that keeps billing, so nothing in here may block forever.
# The only step that talks to the network is close_incident, and it is bounded
# by construction now: api() caps each curl at API_MAX_TIME, and the port-forward
# it may rebuild is capped by its own wait_for. Worst case is on the order of a
# minute and cannot become infinite.
#
# Deliberately NOT wrapped in an extra `timeout` subshell. The first version was,
# and it re-declared these functions into a child shell with the port-forward
# state interpolated into a string — more moving parts, on the one path that has
# to work when everything else has already gone wrong. Bounding the call itself
# is the smaller and more reliable fix.
cleanup() {
	if [ -n "${_OPEN_INCIDENT:-}" ]; then
		close_incident "$_OPEN_INCIDENT" "phase ended without closing its incident"
		_OPEN_INCIDENT=""
	fi
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
	test-drain) cmd_test_drain "$@" ;;
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
