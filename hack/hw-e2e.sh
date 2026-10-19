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
                     wait Ready, then tie the controller's SQLite claim to
                     this run: resolve PVC -> PV -> EBS volume, cross-check
                     the CSI driver's own tags, and tag the exact volume with
                     the run identity the sweep deletes by.
  deploy-observability
                     Install the checked-in dependency profile's
                     VictoriaMetrics half (deploy/kubernetes/dependencies):
                     the checksum-locked operator manifest (server-side
                     apply, CRDs Established, operator rolled out), then
                     namespace, VMSingle (PVC Bound, its EBS volume tied to
                     this run exactly as deploy ties the controller's, then
                     rollout), the webhook
                     token copy, VMAgent, VMAlertmanager, VMAlert and the
                     rules, each behind a bounded readiness wait; wait for a
                     raw DCGM series and a canonical recording-rule series
                     in VMSingle; then point the KubeNeuron root at the
                     profile's VMSingle/Alertmanager endpoints and wait
                     Ready. Needs deploy (the token) and up (the exporter).
                     UNEXERCISED LIVE.
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
  test-observability Read-only check of the v0.7 GPU reliability profile on
                     EKS: the kubeneuron-dcgm-metrics ConfigMap carries the T0
                     and T1 fields, nvidia-dcgm-exporter is rolled out and
                     serves both tiers with UUID/modelName labels, and — on
                     the stack deploy-observability installed (VMSingle; a
                     missing stack is a stated prerequisite failure) —
                     the canonical kubeneuron_gpu_* recording rules exist with
                     the node/gpu_uuid/gpu_model contract labels, node_pool
                     equals the EKS nodegroup label, memory_used equals raw
                     FB_USED * 1048576, and on g4dn/Tesla T4 the signals the
                     hardware cannot produce (row remap, NVLink, fail-slow,
                     MIG) are ABSENT, not zero. Never mutates the cluster.
                     UNEXERCISED LIVE.
  teardown           Tie this run's claim volumes to the run while the cluster
                     is still live (last chance), then eksctl delete cluster
                     --force --wait, then sweep. Safe to re-run.
  sweep              Assert ZERO leftovers for this run: cluster, non-terminated e2e EC2,
                     e2e CloudFormation stacks, EBS volumes tagged for exactly
                     this run, and a manually-created recycle IAM role. Deletes
                     what it finds. Volumes are deleted and audited by this
                     run's own tag (and the legacy KubernetesCluster tag) and
                     never by PVC namespace: a namespace is shared by every
                     cluster in the account. A claim whose volume could not be
                     tied to this run fails the sweep by name instead of being
                     guessed at.
                     When the exact cluster stack is DELETE_FAILED, clears the
                     detached VPC-CNI ENIs of this run's terminated nodes and
                     the exact EKS cluster security group inside that stack's
                     own VPC BEFORE retrying the stack (at most twice); waits
                     out a DELETE_IN_PROGRESS rather than racing it.
  reap               Out-of-band watchdog: force-delete any e2e cluster older
                     than MAX_LIFETIME_MINUTES. Meant for an INDEPENDENT cron
                     so a hung run cannot leak a cluster (PRODUCT_PLAN safety
                     rule 1). Never assumes this run's cluster. Sweeps a
                     reaped cluster's volumes by its run tag only; a run that
                     hung before its claims were tagged leaves its volume for
                     manual review rather than a namespace-wide guess.

Configuration (environment variables):
  CLUSTER_NAME       ephemeral cluster name (default: kubeneuron-e2e-local).
                     Must start with the e2e prefix so sweep/reap can find it.
  AWS_REGION         region (default: us-east-1).
  ECR_REGISTRY       ECR registry host, e.g. <acct>.dkr.ecr.<region>.amazonaws.com
                     (required for deploy). Never hard-code the account id.
  GPU_INSTANCE_TYPE  GPU instance type (default: g4dn.xlarge).
  K8S_VERSION        EKS control-plane version (default: 1.33).
  GPU_OPERATOR_VERSION  NVIDIA GPU operator chart version (default: v26.3.3,
                     the version pinned by the checked-in profile; any other
                     value is refused before Helm runs).
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
# Run-scoped EBS identity. The EBS CSI driver tags the volume behind a claim
# with kubernetes.io/created-for/pvc/{name,namespace} and nothing that names
# the CLUSTER, so "the volumes in kube-neuron and kubeneuron-monitoring" is
# every cluster in the account that uses those namespaces. The sweep filtered
# on exactly that once, and could have deleted another cluster's state. Now
# each of the two claims this harness creates is resolved PVC -> PV -> EBS
# volume id while its cluster is live, cross-checked against the
# provisioner's own created-for tags, and stamped with E2E_RUN_TAG=CLUSTER_NAME
# plus E2E_CLAIM_TAG=<namespace>/<pvc>; the sweep deletes and audits volumes
# by that run identity and nothing wider (tag_run_volume, cmd_sweep).
#
# Two PV sources are accepted, each on its own explicit schema, and only
# when exactly one of them is set: an ebs.csi.aws.com PV (.spec.csi, id in
# volumeHandle) or a legacy in-tree PV (.spec.awsElasticBlockStore, id in
# volumeID as vol-<hex> or aws://<zone>/vol-<hex>). A legacy PV names no
# driver, so its volume must additionally carry LEGACY_CLUSTER_TAG=
# CLUSTER_NAME exactly, on top of the same three created-for tags.
readonly E2E_CLAIM_TAG="kubeneuron:e2e-claim"
readonly EBS_CSI_DRIVER="ebs.csi.aws.com"
readonly CSI_PV_NAME_TAG="kubernetes.io/created-for/pv/name"
readonly CSI_PVC_NAME_TAG="kubernetes.io/created-for/pvc/name"
readonly CSI_PVC_NAMESPACE_TAG="kubernetes.io/created-for/pvc/namespace"
# The in-tree provisioner's cluster tag, the same literal the sweep's legacy
# delete and audit filter on.
readonly LEGACY_CLUSTER_TAG="KubernetesCluster"
# The GPU operator install is the checked-in v0.7 reliability profile, not an
# ad-hoc chart default. Both the values file and the chart version are pinned
# in deploy/kubernetes/dependencies (versions.lock.yaml, values.yaml); the
# harness repeats the version so a stale GPU_OPERATOR_VERSION cannot install a
# chart the profile was never reviewed against. assert_gpu_operator_profile
# refuses any mismatch BEFORE helm runs — no silent drift in either direction.
readonly GPU_OPERATOR_PROFILE_REL="deploy/kubernetes/dependencies/gpu-operator/values.yaml"
readonly GPU_OPERATOR_PINNED_VERSION="v26.3.3"
# The ConfigMap the profile creates (values.yaml dcgmExporter.config.name) and
# the two fields test-observability uses as witnesses for the T0 and T1 tiers.
readonly DCGM_METRICS_CONFIGMAP="kubeneuron-dcgm-metrics"
readonly GPU_OPERATOR_NAMESPACE="gpu-operator"
readonly OBSERVABILITY_NAMESPACE="kubeneuron-monitoring"
readonly OBS_T0_FIELD="DCGM_FI_DEV_POWER_USAGE"
readonly OBS_T1_FIELD="DCGM_FI_DEV_ECC_SBE_VOL_TOTAL"
# The raw series the deploy-observability pipeline wait looks for in VMSingle
# and, before anything is installed, on the exporter itself; the canonical
# kubeneuron_gpu_temperature_celsius rule records from it.
readonly OBS_PIPELINE_FIELD="DCGM_FI_DEV_GPU_TEMP"
# The VictoriaMetrics half of the same profile, installed by
# deploy-observability. Version, release URL and manifest SHA-256 repeat
# versions.lock.yaml on purpose: assert_observability_profile refuses to run
# when the two disagree, so neither can move without the other, and the
# download is verified against this pin before anything is applied.
readonly DEPENDENCIES_LOCK_REL="deploy/kubernetes/dependencies/versions.lock.yaml"
readonly OBSERVABILITY_PROFILE_REL="deploy/kubernetes/dependencies/observability"
readonly VM_OPERATOR_VERSION="v0.73.1"
readonly VM_OPERATOR_MANIFEST_URL="https://github.com/VictoriaMetrics/operator/releases/download/v0.73.1/install-no-webhook.yaml"
readonly VM_OPERATOR_MANIFEST_SHA256="28f0ab848df7a00f9d46685cd2864024a0d998df4840bb9a1fce471dd4163628"
readonly VM_OPERATOR_NAMESPACE="vm"
readonly VM_OPERATOR_FIELD_MANAGER="kubeneuron-dependencies"
# The in-cluster endpoints the profile generates (README, "What this profile
# installs") and the KubeNeuron root is pointed at once the stack is live.
# vmalert.yaml reads and notifies the same two URLs; the gate checks that.
# VMSingle serves 8429: the VictoriaMetrics operator names the Service port
# http and gives it the VMSingle default listen port, 8429, not the
# victoria-metrics binary's standalone default 8428. The first G3 on EKS
# (2026-10-18) observed exactly that and obs_assert_service_port stopped the
# phase because this constant said 8428; the constant now records the
# observed contract and the assertion keeps guarding it.
readonly VMSINGLE_ENDPOINT="http://vmsingle-kubeneuron.kubeneuron-monitoring.svc:8429"
readonly VMALERTMANAGER_ENDPOINT="http://vmalertmanager-kubeneuron.kubeneuron-monitoring.svc:9093"
# The Secret vmalertmanager.yaml mounts for the controller webhook's bearer
# token: a namespace-local copy of RUNTIME_NAMESPACE/<ROOT_NAME>-webhook-token.
readonly ALERTMANAGER_WEBHOOK_TOKEN_SECRET="kubeneuron-alertmanager-webhook-token"

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
#
# Since v0.7 the default is the profile's locked chart release
# (GPU_OPERATOR_PINNED_VERSION); overriding it is refused by
# assert_gpu_operator_profile so the stand always installs what is reviewed.
GPU_OPERATOR_VERSION="${GPU_OPERATOR_VERSION:-$GPU_OPERATOR_PINNED_VERSION}"
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
# The two PersistentVolumeClaims this harness creates: the operator's SQLite
# state claim (deploy; the installation is Ready only once it is Bound, see
# internal/operator/reconciler.go sqliteClaimReady) and the profile's VMSingle
# claim (deploy-observability; the operator names it vmsingle-<VMSingle name>).
# These two names, never a namespace-wide listing, are what tag_run_volume
# resolves.
readonly CONTROLLER_STATE_CLAIM="${ROOT_NAME}-controller-state"
readonly VMSINGLE_CLAIM="vmsingle-${ROOT_NAME}"
# Per-run records beside the kubeconfig and image tag, written by
# tag_run_volume / claim_unproven and read by the sweep's end-state audit.
# Every line starts with the cluster name so a record is never read for the
# wrong run:
#   run-volumes:      <cluster> <volume-id> <namespace>/<pvc>
#   unproven-claims:  <cluster> <namespace>/<pvc> <reason...>
# The tags ON the volumes are what the sweep deletes by; these files add only
# to what it asserts, never to what it deletes.
readonly RUN_VOLUMES_FILE="${E2E_STATE_DIR}/run-volumes"
readonly UNPROVEN_CLAIMS_FILE="${E2E_STATE_DIR}/unproven-claims"
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

# assert_gpu_operator_profile is the pre-Helm gate for the GPU operator
# install: the checked-in profile must exist, the requested chart version must
# equal the harness pin, and the profile itself must agree with that pin (the
# file repeats the chart's operator image version). Fails closed.
GPU_OPERATOR_PROFILE="${REPO_ROOT}/${GPU_OPERATOR_PROFILE_REL}"
assert_gpu_operator_profile() {
	[ -f "$GPU_OPERATOR_PROFILE" ] ||
		die "GPU operator profile not found: $GPU_OPERATOR_PROFILE_REL (run from a checkout)"
	[ "$GPU_OPERATOR_VERSION" = "$GPU_OPERATOR_PINNED_VERSION" ] ||
		die "GPU_OPERATOR_VERSION=$GPU_OPERATOR_VERSION differs from the checked-in pin $GPU_OPERATOR_PINNED_VERSION ($GPU_OPERATOR_PROFILE_REL, versions.lock.yaml); refusing to install an unreviewed chart"
	grep -Eq "^[[:space:]]*version:[[:space:]]*${GPU_OPERATOR_PINNED_VERSION}[[:space:]]*$" "$GPU_OPERATOR_PROFILE" ||
		die "$GPU_OPERATOR_PROFILE_REL does not pin version ${GPU_OPERATOR_PINNED_VERSION}; update GPU_OPERATOR_PINNED_VERSION in this script together with the profile"
	grep -Eq "^[[:space:]]*name:[[:space:]]*${DCGM_METRICS_CONFIGMAP}[[:space:]]*$" "$GPU_OPERATOR_PROFILE" ||
		die "$GPU_OPERATOR_PROFILE_REL does not create the ${DCGM_METRICS_CONFIGMAP} ConfigMap test-observability depends on"
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

	log "up: installing the NVIDIA GPU operator ($GPU_OPERATOR_VERSION) from $GPU_OPERATOR_PROFILE_REL"
	assert_gpu_operator_profile
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
	#
	# --values is the reviewed reliability profile (field list, pinned images);
	# the two --set lines are the EKS-only overrides on top of it, in that
	# order so they win.
	helm upgrade --install gpu-operator nvidia/gpu-operator \
		--namespace "$GPU_OPERATOR_NAMESPACE" --create-namespace \
		--version "$GPU_OPERATOR_VERSION" \
		--values "$GPU_OPERATOR_PROFILE" \
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
	# Ready means the SQLite claim is Bound (the operator's runtimeReady checks
	# exactly that), so its EBS volume exists now. Tie it to this run before
	# anything after this point can stop the phase: a volume with no run
	# identity is one the sweep is not allowed to find.
	tag_run_volume "$RUNTIME_NAMESPACE" "$CONTROLLER_STATE_CLAIM"
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
	wait_for_controller_config

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
	local incident="$1" reason="$2" err
	[ -n "$incident" ] || return 0
	# Non-fatal, because this runs from the EXIT trap — but never silent. A
	# resolve the controller refused used to vanish into /dev/null, and the
	# phase after it then failed for a reason nothing in the output named.
	#
	# The error goes through a FILE, not a $(...) capture. api may rebuild the
	# port-forward, and a child left holding the write end of a capture pipe
	# keeps the substitution waiting for an EOF that never comes — inside the
	# EXIT trap, with the cluster still billing. That is the hang from the
	# memory note, and the first version of this function reproduced it.
	err="${E2E_STATE_DIR}/close-incident.err"
	if ! api POST "/api/v1/incidents/${incident}/resolve" \
		"{\"actor\":\"hw-e2e\",\"reason\":\"${reason}\"}" >/dev/null 2>"$err"; then
		log "close_incident: the controller refused to resolve ${incident} ($(tail -n 1 "$err" 2>/dev/null)); the next phase on this node may attach to it"
	fi
	rm -f "$err"
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

	_DRAIN_FIXTURE_NODE="$node"
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
      # nodeSelector, NEVER nodeName. nodeName bypasses the scheduler, so the
      # ReplicaSet puts every replacement straight back onto the node no matter
      # what — a cordon does not apply, the node can never be drained, and the
      # drain step correctly times out with "1 pods remaining". That is not a
      # test of the drain, it is a test of a node nobody could drain by hand
      # either.
      #
      # And no blanket toleration, for the same reason. A bare operator: Exists
      # also tolerates node.kubernetes.io/unschedulable — the taint a cordon
      # applies — so the scheduler put the evicted pod's replacement straight
      # back onto the cordoned node ten seconds after the Cordon step, and the
      # drain timed out with "1 pods remaining" having evicted correctly (run
      # 10). The stand's GPU node carries no taint of its own; tolerate only
      # the one an accelerator-tainted pool could add.
      nodeSelector: {role: gpu}
      tolerations: [{key: nvidia.com/gpu, operator: Exists}]
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
  nodeSelector: {role: gpu}
  tolerations: [{key: nvidia.com/gpu, operator: Exists}]
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
	# The mapping that makes XID 48 a drain-probe fault must be LOADED, not
	# merely compiled, before it is injected — see wait_for_controller_config.
	wait_for_controller_config
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

	# The node must stop running it. Not "it moved" — with one role=gpu node the
	# replacement has nowhere to go and sits Pending, which is a drained node,
	# and asserting a move would fail on a correct product.
	wait_for 900 '[ "$(kubectl -n default get pods -l app=e2e-tenant \
		-o jsonpath="{range .items[*]}{.spec.nodeName}{\"\n\"}{end}" 2>/dev/null |
		grep -c "^'"$node"'$")" = 0 ]' ||
		die "the drain left the tenant workload on $node"
	log "test-drain: PASS (real drain took the tenant workload off the node)"

	close_incident "$incident" "drain success asserted"
	_OPEN_INCIDENT=""
	drain_fixture_cleanup
}

# _DRAIN_FIXTURE_NODE names the node test-drain put a workload on. The EXIT trap
# clears it whichever way the phase ends; leaving a pod behind on a cordoned
# node is how this phase failed the destructive phase after it.
_DRAIN_FIXTURE_NODE="${_DRAIN_FIXTURE_NODE:-}"

drain_fixture_cleanup() {
	[ -n "${_DRAIN_FIXTURE_NODE:-}" ] || return 0
	kubectl -n default delete deployment e2e-tenant --ignore-not-found --wait=false >/dev/null 2>&1 || true
	kubectl -n default delete pod e2e-bare --ignore-not-found --wait=false >/dev/null 2>&1 || true
	# Uncordon too: this phase's ladder has no uncordon step, so a node left
	# cordoned here is a node the next phase cannot drain.
	kubectl uncordon "$_DRAIN_FIXTURE_NODE" >/dev/null 2>&1 || true
	_DRAIN_FIXTURE_NODE=""
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
	# Same open-time binding as test-drain: the ReplaceNode ladder must be the
	# one the controller has LOADED before the fault that binds to it lands.
	wait_for_controller_config

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

# ---------------------------------------------------------------------------
# deploy-observability — install the dependency profile's VictoriaMetrics half.
#
# This is the stack test-observability reads. Until it existed in the harness
# the phase could only ever stop at its own VMSingle prerequisite, so the
# canonical-series checks were never reachable from the workflow.
#
# It runs AFTER deploy, not inside up-finish with the GPU operator, because of
# one namespace-local object: the profile's VMAlertmanager mounts the
# installation's webhook bearer token under ALERTMANAGER_WEBHOOK_TOKEN_SECRET,
# which is a copy of the Secret deploy/install.sh generates in
# RUNTIME_NAMESPACE. The copy needs KubeNeuron to exist, and an Alertmanager
# applied before its Secret exists sits in ContainerCreating for the rest of
# the run. The README's own order (dependencies healthy, KubeNeuron pointed
# at them last) is kept for the part that matters: the root is repointed at
# the profile endpoints only after the pipeline is proven live.
#
# Nothing is applied from a mutable remote. The operator manifest is
# downloaded into the per-run state directory, checked against the SHA-256
# this script repeats from versions.lock.yaml (and the repeat itself is
# checked against the lock first), and only the verified file is applied —
# server-side, because the operator's CRDs exceed the client-side annotation
# limit. Every wait is bounded and every failure is a failure: a stack that
# does not come up stops the run here, with the component named, rather than
# letting a later phase pass against nothing.

OBSERVABILITY_PROFILE="${REPO_ROOT}/${OBSERVABILITY_PROFILE_REL}"
DEPENDENCIES_LOCK="${REPO_ROOT}/${DEPENDENCIES_LOCK_REL}"

# lock_field <artifact> <key> — print one scalar from the lock's
# artifacts.<artifact> block. awk, so the runner needs no YAML tool; the lock
# is a flat two-level file by design (schemaVersion 1).
lock_field() {
	awk -v block="  $1:" -v key="$2" '
		$0 == block { p = 1; next }
		p && /^  [^ ]/ { p = 0 }
		p { sub(/^[ \t]+/, ""); if (index($0, key ":") == 1) { sub("^" key ":[ \t]*", ""); print; exit } }
	' "$DEPENDENCIES_LOCK"
}

# assert_observability_profile is the gate before any download or apply: the
# lock must agree with this script's pins, every profile file must exist, and
# the endpoints this phase hands to KubeNeuron must be the ones the profile
# itself wires (vmalert reads VMSingle at the first and notifies the second).
# A profile that moved either without this script moving with it would leave
# the controller pointed at a name that resolves to nothing. Fails closed.
assert_observability_profile() {
	[ -f "$DEPENDENCIES_LOCK" ] ||
		die "dependency lock not found: $DEPENDENCIES_LOCK_REL (run from a checkout)"
	local locked
	locked=$(lock_field victoriaMetricsOperatorManifest version)
	[ "$locked" = "$VM_OPERATOR_VERSION" ] ||
		die "$DEPENDENCIES_LOCK_REL pins VictoriaMetrics operator '${locked}' but this script pins ${VM_OPERATOR_VERSION}; update VM_OPERATOR_VERSION together with the lock"
	locked=$(lock_field victoriaMetricsOperatorManifest url)
	[ "$locked" = "$VM_OPERATOR_MANIFEST_URL" ] ||
		die "$DEPENDENCIES_LOCK_REL locks manifest URL '${locked}' but this script would download ${VM_OPERATOR_MANIFEST_URL}; refusing an artifact the lock does not name"
	locked=$(lock_field victoriaMetricsOperatorManifest sha256)
	[ "$locked" = "$VM_OPERATOR_MANIFEST_SHA256" ] ||
		die "$DEPENDENCIES_LOCK_REL locks manifest SHA-256 '${locked}' but this script expects ${VM_OPERATOR_MANIFEST_SHA256}; refusing to verify against a stale pin"
	local f
	for f in namespace vmsingle vmagent vmalertmanager vmalert rules; do
		[ -f "$OBSERVABILITY_PROFILE/$f.yaml" ] ||
			die "observability profile file missing: ${OBSERVABILITY_PROFILE_REL}/${f}.yaml (run from a checkout)"
	done
	grep -Eq "^[[:space:]]*url:[[:space:]]*${VMSINGLE_ENDPOINT}[[:space:]]*$" "$OBSERVABILITY_PROFILE/vmalert.yaml" ||
		die "${OBSERVABILITY_PROFILE_REL}/vmalert.yaml does not read VMSingle at ${VMSINGLE_ENDPOINT}; update VMSINGLE_ENDPOINT together with the profile"
	grep -Eq "^[[:space:]]*url:[[:space:]]*${VMALERTMANAGER_ENDPOINT}[[:space:]]*$" "$OBSERVABILITY_PROFILE/vmalert.yaml" ||
		die "${OBSERVABILITY_PROFILE_REL}/vmalert.yaml does not notify ${VMALERTMANAGER_ENDPOINT}; update VMALERTMANAGER_ENDPOINT together with the profile"
	grep -Eq "^[[:space:]]*-[[:space:]]*${ALERTMANAGER_WEBHOOK_TOKEN_SECRET}[[:space:]]*$" "$OBSERVABILITY_PROFILE/vmalertmanager.yaml" ||
		die "${OBSERVABILITY_PROFILE_REL}/vmalertmanager.yaml does not mount Secret ${ALERTMANAGER_WEBHOOK_TOKEN_SECRET}; update ALERTMANAGER_WEBHOOK_TOKEN_SECRET together with the profile"
}

vm_operator_manifest_verified() {
	printf '%s  %s\n' "$VM_OPERATOR_MANIFEST_SHA256" "$1" | sha256sum -c --status -
}

# fetch_vm_operator_manifest prints the path of the checksum-verified operator
# manifest in the per-run state directory, downloading it when the copy there
# is absent or does not verify. A download that does not match the lock is
# deleted and the phase stops; it is never applied.
fetch_vm_operator_manifest() {
	local dest="${E2E_STATE_DIR}/vm-operator-${VM_OPERATOR_VERSION}-no-webhook.yaml"
	mkdir -p "$E2E_STATE_DIR"
	if [ ! -s "$dest" ] || ! vm_operator_manifest_verified "$dest"; then
		rm -f "$dest"
		log "deploy-observability: downloading the VictoriaMetrics operator manifest ${VM_OPERATOR_VERSION}"
		curl -fsSL --max-time 120 --retry 3 -o "${dest}.download" "$VM_OPERATOR_MANIFEST_URL" ||
			die "cannot download ${VM_OPERATOR_MANIFEST_URL}"
		if ! vm_operator_manifest_verified "${dest}.download"; then
			rm -f "${dest}.download"
			die "VictoriaMetrics operator manifest from ${VM_OPERATOR_MANIFEST_URL} does not match the locked SHA-256 ${VM_OPERATOR_MANIFEST_SHA256} (${DEPENDENCIES_LOCK_REL}); refusing to apply an unverified manifest"
		fi
		mv "${dest}.download" "$dest"
	fi
	printf '%s\n' "$dest"
}

# obs_wait_rollout <deployment|statefulset>/<name> <seconds> — the operator
# creates a CR's workload asynchronously, and rollout status on an object that
# does not exist yet fails at once; wait for the object, then for the rollout.
# Deployments go through kubectl rollout status. StatefulSets do not: the
# VictoriaMetrics operator generates VMAlertmanager's StatefulSet with
# updateStrategy OnDelete (it rotates the pods itself), and kubectl answers
# "error: rollout status is only available for RollingUpdate strategy type"
# — the 2026-10-18 G3 attempt died on that line with Alertmanager coming up
# fine. A StatefulSet is instead polled on its status fields
# (obs_statefulset_ready), with the strategy's own meaning of "rolled out",
# and the timeout prints that status so the failure names what was missing.
obs_wait_rollout() {
	local target="$1" seconds="$2"
	wait_for "$seconds" 'kubectl -n '"$OBSERVABILITY_NAMESPACE"' get '"$target"' >/dev/null'
	case "$target" in
	deployment/*)
		kubectl -n "$OBSERVABILITY_NAMESPACE" rollout status "$target" --timeout="${seconds}s" ||
			die "${OBSERVABILITY_NAMESPACE}/${target} did not roll out within ${seconds}s"
		;;
	statefulset/*)
		(wait_for "$seconds" 'obs_statefulset_ready '"$target") ||
			die "${OBSERVABILITY_NAMESPACE}/${target} did not roll out within ${seconds}s: $(obs_statefulset_status "$target")"
		;;
	*) die "obs_wait_rollout: ${target} is neither deployment/<name> nor statefulset/<name>" ;;
	esac
}

# obs_statefulset_ready <statefulset/name> — true once the StatefulSet
# controller has observed the current generation and every desired replica is
# created and Ready, plus what the update strategy itself promises about the
# revision the pods run:
#   RollingUpdate — the controller rolls every pod to updateRevision, so the
#     rollout is complete only when updatedReplicas equals spec.replicas
#     (what kubectl rollout status waits for).
#   OnDelete — the controller never rolls a pod; after a spec change it keeps
#     the old-revision pods running and Ready until something else deletes
#     them (the VictoriaMetrics operator decides if and when). updatedReplicas
#     therefore stays below spec.replicas indefinitely while Alertmanager
#     serves, and requiring it would make the harness wait the full bound for
#     a healthy StatefulSet — the G3 blockage in a new shape. The pods' being
#     Ready at the observed generation is the whole of what OnDelete promises,
#     so that is what is required. currentRevision is likewise not compared
#     with updateRevision under either strategy.
#   anything else — not ready, ever (fail closed); the timeout message names
#     the strategy as unsupported. An unset updateStrategy.type is the API
#     server's RollingUpdate default.
# A StatefulSet that asks for zero replicas is NOT ready: the harness is
# waiting for Alertmanager to serve, and nothing-to-serve would otherwise pass
# vacuously. Every count is compared with spec.replicas, so a status the
# controller has not written yet reads as 0 of N, never as 0 of 0.
obs_statefulset_ready() {
	kubectl -n "$OBSERVABILITY_NAMESPACE" get "$1" -o json |
		jq -e '(.spec.replicas // 1) as $want
			| (.spec.updateStrategy.type // "RollingUpdate") as $strategy
			| $want >= 1
			and (.metadata.generation // 0) >= 1
			and .status.observedGeneration == .metadata.generation
			and (.status.replicas // 0) == $want
			and (.status.readyReplicas // 0) == $want
			and (if $strategy == "RollingUpdate" then (.status.updatedReplicas // 0) == $want
				elif $strategy == "OnDelete" then true
				else false end)' >/dev/null
}

# obs_statefulset_status <statefulset/name> — one line of the fields
# obs_statefulset_ready decides on, for the timeout message. A strategy the
# predicate does not understand is called out, since it alone makes the wait
# unsatisfiable.
obs_statefulset_status() {
	kubectl -n "$OBSERVABILITY_NAMESPACE" get "$1" -o json 2>/dev/null |
		jq -er '(.spec.updateStrategy.type // "RollingUpdate(default)") as $strategy
			| "strategy=\($strategy) generation=\(.metadata.generation // "?") observedGeneration=\(.status.observedGeneration // "?") desired=\(.spec.replicas // "?") replicas=\(.status.replicas // 0) ready=\(.status.readyReplicas // 0) updated=\(.status.updatedReplicas // 0) currentRevision=\(.status.currentRevision // "?") updateRevision=\(.status.updateRevision // "?")"
			+ (if $strategy == "RollingUpdate" or $strategy == "RollingUpdate(default)" or $strategy == "OnDelete" then ""
				else " (unsupported updateStrategy: obs_statefulset_ready understands only RollingUpdate and OnDelete and never holds for \($strategy))" end)' 2>/dev/null ||
		printf 'status unreadable'
}

# obs_assert_service_port <service> <endpoint-url> — the Service the operator
# generated must listen where the checked-in endpoint says, read through the
# same resolver test-observability uses (the port named http, or the only
# port; anything else is refused there). Otherwise KubeNeuron would be handed
# a URL whose host answers and whose port does not.
obs_assert_service_port() {
	local svc="$1" endpoint="$2" want got
	want="${endpoint##*:}"
	got=$(obs_service_http_port "$OBSERVABILITY_NAMESPACE" "$svc")
	[ "$got" = "$want" ] ||
		die "service ${OBSERVABILITY_NAMESPACE}/${svc} serves port ${got} but the checked-in endpoint ${endpoint} names ${want}; refusing to hand KubeNeuron an endpoint the Service does not answer on"
}

# copy_webhook_token copies the installation's Alertmanager webhook bearer
# token into OBSERVABILITY_NAMESPACE under the name vmalertmanager.yaml
# mounts. Secrets are namespace-local, so this is the one point where the two
# halves of the profile meet. The root must reference the Secret install.sh
# generated (no guessing which token Alertmanager should present), the token
# travels through a pipe and never through an argument, and the copy is read
# back and compared before Alertmanager is applied.
copy_webhook_token() {
	local src="${ROOT_NAME}-webhook-token" ref src_b64 dst_b64
	ref=$(kubectl -n "$RUNTIME_NAMESPACE" get kubeneuron "$ROOT_NAME" \
		-o jsonpath='{.spec.notifications.webhookToken.name}' 2>/dev/null) ||
		die "KubeNeuron ${RUNTIME_NAMESPACE}/${ROOT_NAME} is not installed; run deploy before deploy-observability (Alertmanager presents the installation's webhook token, which does not exist yet)"
	[ "$ref" = "$src" ] ||
		die "KubeNeuron ${ROOT_NAME} references webhookToken '${ref}', expected ${src} (deploy/install.sh); refusing to guess which Secret Alertmanager should present"
	src_b64=$(kubectl -n "$RUNTIME_NAMESPACE" get secret "$src" -o jsonpath='{.data.token}') ||
		die "Secret ${RUNTIME_NAMESPACE}/${src} is absent although the root references it"
	[ -n "$src_b64" ] ||
		die "Secret ${RUNTIME_NAMESPACE}/${src} has no token key; the controller would refuse every webhook delivery"
	kubectl -n "$RUNTIME_NAMESPACE" get secret "$src" -o json |
		jq --arg name "$ALERTMANAGER_WEBHOOK_TOKEN_SECRET" --arg ns "$OBSERVABILITY_NAMESPACE" '{
			apiVersion: "v1", kind: "Secret", type: (.type // "Opaque"),
			metadata: {name: $name, namespace: $ns,
				labels: {"app.kubernetes.io/part-of": "kube-neuron-dependencies"}},
			data: {token: .data.token}}' |
		kubectl apply -f - >/dev/null
	dst_b64=$(kubectl -n "$OBSERVABILITY_NAMESPACE" get secret "$ALERTMANAGER_WEBHOOK_TOKEN_SECRET" -o jsonpath='{.data.token}') ||
		die "Secret ${OBSERVABILITY_NAMESPACE}/${ALERTMANAGER_WEBHOOK_TOKEN_SECRET} is absent after the copy"
	[ "$dst_b64" = "$src_b64" ] ||
		die "Secret ${OBSERVABILITY_NAMESPACE}/${ALERTMANAGER_WEBHOOK_TOKEN_SECRET} does not carry the installation's token after the copy"
}

# obs_vm_has_samples <promql> — one bounded instant query against the
# forwarded VMSingle at the current instant; true when a sample came back.
obs_vm_has_samples() {
	local n
	_OBS_QUERY_TIME=$(date -u +%s)
	n=$(obs_vm_query "$1" | jq 'length') || return 1
	[ "${n:-0}" -ge 1 ]
}

# obs_exporter_diagnose — what the exporter is doing, for a failure message.
# Pod phase, container state, and the last log lines of every exporter pod,
# current and previous container. One CSV field name the pinned exporter does
# not know terminates the whole exporter (the 2026-10-18 G4dn run: the
# profile listed DCGM_FI_DEV_MEMORY_UNREPAIRABLE, the pinned go-dcgm spells
# the id DCGM_FI_DEV_MEMORY_UNREPAIRABLE_FLAG, and the exporter served
# nothing), and from the outside that looks exactly like a DaemonSet that is
# slow to roll out: a Service with no ready endpoints. The reason is only in
# the container's exit and its log, so both are printed. Nothing here is a
# secret: the exporter mounts no credential and logs none. Every call is
# bounded by the API request timeout, and none of them can fail the phase.
obs_exporter_diagnose() {
	{
		echo "--- ${GPU_OPERATOR_NAMESPACE} pods app=nvidia-dcgm-exporter"
		kubectl -n "$GPU_OPERATOR_NAMESPACE" get pods -l app=nvidia-dcgm-exporter --request-timeout=30s -o wide 2>&1 || true
		kubectl -n "$GPU_OPERATOR_NAMESPACE" get pods -l app=nvidia-dcgm-exporter --request-timeout=30s \
			-o jsonpath='{range .items[*]}{.metadata.name}: phase={.status.phase} restarts={.status.containerStatuses[*].restartCount} state={.status.containerStatuses[*].state} last={.status.containerStatuses[*].lastState}{"\n"}{end}' 2>&1 || true
		echo "--- last 40 log lines per exporter container (previous container first, if any)"
		kubectl -n "$GPU_OPERATOR_NAMESPACE" logs -l app=nvidia-dcgm-exporter --all-containers --prefix --previous --tail=40 --request-timeout=30s 2>&1 || true
		kubectl -n "$GPU_OPERATOR_NAMESPACE" logs -l app=nvidia-dcgm-exporter --all-containers --prefix --tail=40 --request-timeout=30s 2>&1 || true
		echo "--- field names the profile ConfigMap ${DCGM_METRICS_CONFIGMAP} lists"
		kubectl -n "$GPU_OPERATOR_NAMESPACE" get configmap "$DCGM_METRICS_CONFIGMAP" --request-timeout=30s \
			-o jsonpath='{.data.dcgm-metrics\.csv}' 2>&1 | grep -vE '^[[:space:]]*(#|$)' | cut -d, -f1 | tr -d ' ' | tr '\n' ' ' || true
		echo
	} >&2
}

# obs_exporter_has_endpoints — true when the exporter Service has at least
# one ready address, which is what vmagent will scrape.
obs_exporter_has_endpoints() {
	kubectl -n "$GPU_OPERATOR_NAMESPACE" get endpoints nvidia-dcgm-exporter --request-timeout=30s \
		-o jsonpath='{.subsets[*].addresses[*].ip}' | grep -q .
}

# obs_assert_exporter_live — the exporter half of the pipeline, proven before
# the VictoriaMetrics half is installed. deploy-observability's pipeline wait
# at the end can only say "no DCGM_FI_DEV_GPU_TEMP in VMSingle after 300s",
# which on 2026-10-18 cost five minutes plus the whole stack install to learn
# that the exporter had never served a byte. So: the DaemonSet rolled out,
# the Service has ready endpoints, and /metrics, read through a bounded
# port-forward on the port the Service declares (obs_service_http_port: the
# operator names it gpu-metrics and it is the Service's only port; nothing
# here hard-codes 9400), carries the raw GPU temperature sample the pipeline
# wait and the canonical temperature rule both need. Each failure stops the
# phase before anything is installed and prints obs_exporter_diagnose.
obs_assert_exporter_live() {
	log "deploy-observability: nvidia-dcgm-exporter rollout"
	kubectl -n "$GPU_OPERATOR_NAMESPACE" rollout status daemonset/nvidia-dcgm-exporter --timeout=300s || {
		obs_exporter_diagnose
		die "nvidia-dcgm-exporter DaemonSet is not rolled out; nothing installed (vmagent would have no target). If the exporter container exits on start, check the profile ConfigMap ${DCGM_METRICS_CONFIGMAP} for a field name the pinned exporter does not know"
	}
	(wait_for 60 obs_exporter_has_endpoints) || {
		obs_exporter_diagnose
		die "Service ${GPU_OPERATOR_NAMESPACE}/nvidia-dcgm-exporter has no ready endpoints although the DaemonSet reports rolled out; nothing installed"
	}

	log "deploy-observability: reading the exporter's /metrics for ${OBS_PIPELINE_FIELD} through a bounded port-forward"
	local port metrics served
	port=$(obs_service_http_port "$GPU_OPERATOR_NAMESPACE" nvidia-dcgm-exporter)
	obs_portforward "$GPU_OPERATOR_NAMESPACE" service/nvidia-dcgm-exporter "$port" 19400 /metrics
	metrics=$(curl -fsS --max-time "$OBS_MAX_TIME" "http://127.0.0.1:19400/metrics") || {
		obs_portforward_close
		obs_exporter_diagnose
		die "exporter /metrics did not answer within ${OBS_MAX_TIME}s on Service port ${port}; nothing installed"
	}
	obs_portforward_close
	printf '%s\n' "$metrics" | grep -Eq "^${OBS_PIPELINE_FIELD}\{" || {
		# The series names are only for the message; a body with no DCGM_
		# series at all is the strongest form of this failure, and grep's
		# "no match" status must not stop the function before it is said.
		served=$(printf '%s\n' "$metrics" | { grep -oE '^DCGM_[A-Z0-9_]+' || true; } | sort -u | tr '\n' ' ')
		obs_exporter_diagnose
		die "exporter serves no ${OBS_PIPELINE_FIELD} sample (DCGM series served: ${served:-none}); the pipeline wait and the canonical temperature rule both need it, so nothing is installed. Check the profile ConfigMap ${DCGM_METRICS_CONFIGMAP} against the fields the pinned exporter knows"
	}
	log "deploy-observability: exporter OK (${OBS_PIPELINE_FIELD} served on Service port ${port})"
}

cmd_deploy_observability() {
	guard_cluster_name
	require_cmd kubectl
	require_cmd aws
	require_cmd curl
	require_cmd jq
	require_cmd sha256sum
	assert_kube_target
	assert_observability_profile

	# Both prerequisites are checked before anything is installed, so a
	# missing one costs nothing to undo: the token copy needs the root, and
	# without the exporter Service vmagent has no target and VMSingle stays
	# empty forever, which the pipeline wait below would report as its own
	# failure 300 seconds later instead of as what it is.
	kubectl -n "$RUNTIME_NAMESPACE" get kubeneuron "$ROOT_NAME" >/dev/null 2>&1 ||
		die "KubeNeuron ${RUNTIME_NAMESPACE}/${ROOT_NAME} is not installed; run deploy before deploy-observability (Alertmanager presents the installation's webhook token, which does not exist yet)"
	kubectl -n "$GPU_OPERATOR_NAMESPACE" get service nvidia-dcgm-exporter >/dev/null 2>&1 ||
		die "no ${GPU_OPERATOR_NAMESPACE}/nvidia-dcgm-exporter Service; run up before deploy-observability (the profile's vmagent scrapes it and nothing else fills VMSingle)"
	# A Service that exists but serves nothing is the same dead end.
	obs_assert_exporter_live

	local manifest
	manifest=$(fetch_vm_operator_manifest)
	log "deploy-observability: installing the VictoriaMetrics operator ${VM_OPERATOR_VERSION} (server-side apply of the verified manifest)"
	kubectl apply --server-side --field-manager="$VM_OPERATOR_FIELD_MANAGER" -f "$manifest" >/dev/null
	local crd
	for crd in vmsingles vmagents vmalertmanagers vmalerts vmrules; do
		kubectl wait --for=condition=Established --timeout=120s "crd/${crd}.operator.victoriametrics.com" >/dev/null ||
			die "CRD ${crd}.operator.victoriametrics.com never became Established"
	done
	kubectl -n "$VM_OPERATOR_NAMESPACE" rollout status deployment/vm-operator --timeout=300s ||
		die "deployment/vm-operator did not roll out in namespace ${VM_OPERATOR_NAMESPACE}"

	log "deploy-observability: namespace and VMSingle"
	kubectl apply -f "$OBSERVABILITY_PROFILE/namespace.yaml" >/dev/null
	kubectl apply -f "$OBSERVABILITY_PROFILE/vmsingle.yaml" >/dev/null
	# gp2 binds on first consumer, so the claim and the rollout land together;
	# the claim is asserted on its own because a claim that never binds is the
	# failure the EBS CSI prerequisite exists for, and it should name itself.
	wait_for 300 'kubectl -n '"$OBSERVABILITY_NAMESPACE"' get pvc vmsingle-kubeneuron -o jsonpath="{.status.phase}" | grep -qx Bound'
	# Bound means the 100Gi volume exists. Tie it to this run NOW, before the
	# rollout wait or anything after it can stop the phase and leave the
	# volume with no identity the sweep is allowed to delete by.
	tag_run_volume "$OBSERVABILITY_NAMESPACE" "$VMSINGLE_CLAIM"
	obs_wait_rollout deployment/vmsingle-kubeneuron 300
	obs_assert_service_port vmsingle-kubeneuron "$VMSINGLE_ENDPOINT"

	log "deploy-observability: copying the installation's webhook token for Alertmanager"
	copy_webhook_token

	log "deploy-observability: VMAgent"
	kubectl apply -f "$OBSERVABILITY_PROFILE/vmagent.yaml" >/dev/null
	obs_wait_rollout deployment/vmagent-kubeneuron 300

	log "deploy-observability: VMAlertmanager"
	kubectl apply -f "$OBSERVABILITY_PROFILE/vmalertmanager.yaml" >/dev/null
	obs_wait_rollout statefulset/vmalertmanager-kubeneuron 300
	obs_assert_service_port vmalertmanager-kubeneuron "$VMALERTMANAGER_ENDPOINT"

	log "deploy-observability: VMAlert"
	kubectl apply -f "$OBSERVABILITY_PROFILE/vmalert.yaml" >/dev/null
	obs_wait_rollout deployment/vmalert-kubeneuron 300

	log "deploy-observability: rules"
	kubectl apply -f "$OBSERVABILITY_PROFILE/rules.yaml" >/dev/null
	local rule
	for rule in kubeneuron-gpu kubeneuron-gpu-recording; do
		kubectl -n "$OBSERVABILITY_NAMESPACE" get vmrule "$rule" >/dev/null 2>&1 ||
			die "VMRule ${rule} is absent after applying ${OBSERVABILITY_PROFILE_REL}/rules.yaml"
	done

	# Live, not merely rolled out. The README's health criterion is a DCGM
	# series queryable in VMSingle (exporter -> vmagent -> VMSingle); the
	# canonical series on top of it proves vmalert loaded the recording
	# VMRule, evaluated it and wrote back. Both are polled at 30s intervals
	# upstream, so the first samples take a minute or two; the wait is
	# bounded and test-observability, which runs next, does not wait at all.
	log "deploy-observability: waiting for the pipeline (exporter -> vmagent -> VMSingle -> vmalert recording rules)"
	local vm_port
	vm_port=$(obs_service_http_port "$OBSERVABILITY_NAMESPACE" vmsingle-kubeneuron)
	obs_portforward "$OBSERVABILITY_NAMESPACE" service/vmsingle-kubeneuron "$vm_port" 18429 /health
	wait_for 300 'obs_vm_has_samples "$OBS_PIPELINE_FIELD"'
	wait_for 300 'obs_vm_has_samples "kubeneuron_gpu_temperature_celsius"'
	obs_portforward_close

	# Last, as the README orders it: the root's External endpoints become the
	# profile's. install.sh writes placeholder hosts (vmsingle-unset,
	# alertmanager-unset) because it also installs where no stack exists; the
	# operator validates the endpoints and records the new generation as
	# Ready, which is what is waited on and then read back.
	log "deploy-observability: pointing KubeNeuron at the profile endpoints"
	kubectl -n "$RUNTIME_NAMESPACE" patch kubeneuron "$ROOT_NAME" --type merge \
		-p "$(jq -nc --arg vm "$VMSINGLE_ENDPOINT" --arg am "$VMALERTMANAGER_ENDPOINT" \
			'{spec: {observability: {victoriaMetrics: {mode: "External", endpoint: $vm}, alertmanager: {mode: "External", endpoint: $am}}}}')" >/dev/null
	wait_for_installation_ready
	local got
	got=$(kubectl -n "$RUNTIME_NAMESPACE" get kubeneuron "$ROOT_NAME" -o jsonpath='{.spec.observability.victoriaMetrics.endpoint}')
	[ "$got" = "$VMSINGLE_ENDPOINT" ] ||
		die "KubeNeuron ${ROOT_NAME} victoriaMetrics endpoint is '${got}' after the patch, expected ${VMSINGLE_ENDPOINT}"
	got=$(kubectl -n "$RUNTIME_NAMESPACE" get kubeneuron "$ROOT_NAME" -o jsonpath='{.spec.observability.alertmanager.endpoint}')
	[ "$got" = "$VMALERTMANAGER_ENDPOINT" ] ||
		die "KubeNeuron ${ROOT_NAME} alertmanager endpoint is '${got}' after the patch, expected ${VMALERTMANAGER_ENDPOINT}"
	log "deploy-observability: stack live; KubeNeuron reads VMSingle at ${VMSINGLE_ENDPOINT} and Alertmanager at ${VMALERTMANAGER_ENDPOINT}"
}

# ---------------------------------------------------------------------------
# test-observability — v0.7 M6 read-only profile check.
#
# Nothing here mutates the cluster: it reads a ConfigMap, a DaemonSet rollout,
# the exporter's /metrics through a bounded port-forward, and (only when the
# stack is installed) VMSingle's query API through another. Every network call
# is capped (OBS_MAX_TIME) for the same reason api() is: a port-forward whose
# remote end died still accepts the connect and never answers.
#
# What it proves and what it does not: the profile's ConfigMap reached the
# cluster, dcgm-exporter serves the T0 and T1 witness fields with the labels
# the recording rules join on, and — if VMSingle is present — three canonical
# series exist for this GPU carrying the M2 label contract (node, gpu_uuid,
# gpu_model equal to the exporter's own values; node_pool equal to the EKS
# nodegroup label on the node), the MiB->bytes rule is arithmetically right
# for one GPU, and on g4dn/Tesla T4 the signals the hardware cannot produce
# (row remap, NVLink, the fail-slow peer score, MIG) are absent from VMSingle
# rather than present as zeros. It injects nothing, fires no alert, and does
# not look at Grafana.

OBS_MAX_TIME="${OBS_MAX_TIME:-20}"
_OBS_PF_PID=""
_OBS_LOCAL_PORT=""

# obs_portforward <namespace> <target> <remote-port> <local-port> <probe-path>
# Opens one bounded port-forward, recorded in _OBS_PF_PID so cleanup() and
# obs_portforward_close can always kill it. Exactly one is open at a time.
#
# The forward is exec'd inside the background subshell so that $! IS the
# kubectl process. Backgrounding the wrapper function instead forks a subshell
# that runs kubectl as its child, and killing $! then kills only the subshell:
# the real port-forward survives as an orphan holding the local port, which
# the next phase's forward cannot bind, while its probe is answered by the
# orphan — a check that passes against a tunnel nothing owns. The target
# guard the wrapper would have run is run here explicitly, before the exec
# (exec resolves an external binary, never the wrapper function).
obs_portforward() {
	local ns="$1" target="$2" remote="$3" local_port="$4" probe="$5"
	obs_portforward_close
	_OBS_LOCAL_PORT="$local_port"
	(
		assert_kube_target
		exec kubectl --kubeconfig "$KUBECONFIG" -n "$ns" port-forward "$target" "${local_port}:${remote}"
	) >/dev/null 2>&1 &
	_OBS_PF_PID=$!
	wait_for 60 'curl -fsS --max-time 5 -o /dev/null "http://127.0.0.1:'"$local_port"''"$probe"'"'
}

obs_portforward_close() {
	if [ -n "$_OBS_PF_PID" ]; then
		kill "$_OBS_PF_PID" 2>/dev/null || true
		wait "$_OBS_PF_PID" 2>/dev/null || true
		_OBS_PF_PID=""
	fi
}

# obs_service_http_port <namespace> <service> — print the HTTP port of a
# Service as Kubernetes declares it, so no caller hard-codes the VictoriaMetrics
# default. The operator names VMSingle's port "http"; that name is preferred.
# Documented fallback: a Service that declares exactly ONE port is that port,
# named or not. Anything else (no "http" port and several ports) is ambiguous
# and refused — guessing would forward to a port that accepts the connect and
# never answers /health, which is the hang obs_portforward's wait_for exists to
# bound, not to hide.
obs_service_http_port() {
	local ns="$1" svc="$2" ports port
	ports=$(kubectl -n "$ns" get service "$svc" -o json 2>/dev/null |
		jq -c '[.spec.ports[]? | {name: (.name // ""), port: .port}]') ||
		die "cannot read service ${ns}/${svc} to resolve its HTTP port"
	port=$(printf '%s' "$ports" | jq -r '
		(map(select(.name == "http")) | .[0].port) //
		(if length == 1 then .[0].port else empty end) // empty')
	[ -n "$port" ] ||
		die "service ${ns}/${svc} has no port named http and does not declare exactly one port (ports: ${ports}); refusing to guess"
	printf '%s\n' "$port"
}

# obs_vm_query <promql> — instant query against the forwarded VMSingle,
# printing the JSON result array. Bounded.
obs_vm_query() {
	curl -fsS --max-time "$OBS_MAX_TIME" -G \
		--data-urlencode "query=$1" --data-urlencode "time=${_OBS_QUERY_TIME}" \
		"http://127.0.0.1:${_OBS_LOCAL_PORT}/api/v1/query" | jq -c '.data.result'
}
_OBS_QUERY_TIME=""

cmd_test_observability() {
	guard_cluster_name
	require_cmd kubectl
	require_cmd aws
	require_cmd curl
	require_cmd jq
	assert_kube_target

	log "test-observability: profile ConfigMap ${GPU_OPERATOR_NAMESPACE}/${DCGM_METRICS_CONFIGMAP}"
	local csv
	csv=$(kubectl -n "$GPU_OPERATOR_NAMESPACE" get configmap "$DCGM_METRICS_CONFIGMAP" \
		-o jsonpath='{.data.dcgm-metrics\.csv}') ||
		die "ConfigMap ${DCGM_METRICS_CONFIGMAP} is absent: up-finish did not install the profile (--values ${GPU_OPERATOR_PROFILE_REL})"
	local field
	for field in "$OBS_T0_FIELD" "$OBS_T1_FIELD"; do
		printf '%s\n' "$csv" | grep -Eq "^[[:space:]]*${field}[[:space:]]*," ||
			die "ConfigMap ${DCGM_METRICS_CONFIGMAP} does not list ${field}; the cluster is not running the checked-in profile"
	done

	log "test-observability: nvidia-dcgm-exporter rollout"
	kubectl -n "$GPU_OPERATOR_NAMESPACE" rollout status daemonset/nvidia-dcgm-exporter --timeout=300s ||
		die "nvidia-dcgm-exporter DaemonSet is not rolled out"

	log "test-observability: scraping the exporter through a bounded port-forward"
	local metrics
	obs_portforward "$GPU_OPERATOR_NAMESPACE" service/nvidia-dcgm-exporter 9400 19400 /metrics
	metrics=$(curl -fsS --max-time "$OBS_MAX_TIME" "http://127.0.0.1:19400/metrics") ||
		die "exporter /metrics did not answer within ${OBS_MAX_TIME}s"
	obs_portforward_close

	# Fail closed: both tiers must be present as real samples, and the labels
	# the recording rules and dashboards key on must be on them. An exporter
	# that silently skipped the ECC field on this GPU is a finding, not a pass.
	local t0_line t1_line uuid
	t0_line=$(printf '%s\n' "$metrics" | grep -E "^${OBS_T0_FIELD}\{" | head -n1) ||
		die "exporter serves no ${OBS_T0_FIELD} sample (T0 tier absent)"
	t1_line=$(printf '%s\n' "$metrics" | grep -E "^${OBS_T1_FIELD}\{" | head -n1) ||
		die "exporter serves no ${OBS_T1_FIELD} sample (T1 tier absent)"
	local line
	for line in "$t0_line" "$t1_line"; do
		printf '%s\n' "$line" | grep -Eq 'UUID="GPU-[0-9a-f-]+"' ||
			die "exporter sample lacks the UUID label: $line"
		printf '%s\n' "$line" | grep -Eq 'modelName="[^"]+"' ||
			die "exporter sample lacks the modelName label: $line"
	done
	uuid=$(printf '%s\n' "$t0_line" | sed -nE 's/.*UUID="([^"]+)".*/\1/p')
	local model
	model=$(printf '%s\n' "$t0_line" | sed -nE 's/.*modelName="([^"]+)".*/\1/p')
	log "test-observability: exporter OK (T0 ${OBS_T0_FIELD}, T1 ${OBS_T1_FIELD}, UUID ${uuid}, modelName ${model})"

	# The EKS nodegroup label on the GPU node is what vmagent turns into
	# node_pool (deploy/kubernetes/dependencies/observability/vmagent.yaml).
	# This phase runs on an EKS managed node group by construction
	# (assert_kube_target proved the endpoint), so the label is expected, and a
	# managed node without it is an EKS-side finding, stated as such. This is
	# an EKS-phase expectation only: the scrape contract itself keeps node_pool
	# optional, and nothing outside this phase may require it.
	local node node_pool
	node=$(gpu_node)
	[ -n "$node" ] || die "no node labelled role=gpu; cannot locate the GPU node for the label checks"
	node_pool=$(kubectl get node "$node" -o jsonpath='{.metadata.labels.eks\.amazonaws\.com/nodegroup}')
	[ -n "$node_pool" ] ||
		die "EKS-label: managed GPU node ${node} carries no eks.amazonaws.com/nodegroup label; EKS stamps it on every managed node group member, so either this node is not from the managed node group this run created or EKS did not label it — node_pool cannot be derived"
	log "test-observability: GPU node ${node} is in EKS nodegroup ${node_pool}"

	# Canonical series are produced by the recording rules in
	# deploy/kubernetes/dependencies/observability, evaluated by vmalert into
	# VMSingle. That stack is an operator prerequisite, not something this phase
	# installs; without it there is nothing to query and the check is a
	# prerequisite failure, stated as such.
	log "test-observability: canonical recording rules (requires the observability stack)"
	kubectl -n "$OBSERVABILITY_NAMESPACE" get vmsingle kubeneuron >/dev/null 2>&1 ||
		die "PREREQUISITE: VMSingle ${OBSERVABILITY_NAMESPACE}/kubeneuron is not installed. The canonical kubeneuron_gpu_* checks need the stack in deploy/kubernetes/dependencies/observability (VictoriaMetrics operator, VMSingle, vmagent, vmalert + rules.yaml), which hack/hw-e2e.sh deploy-observability installs. Exporter checks above PASSED; canonical checks NOT RUN."
	kubectl -n "$OBSERVABILITY_NAMESPACE" get service vmsingle-kubeneuron >/dev/null 2>&1 ||
		die "PREREQUISITE: VMSingle CR exists but service vmsingle-kubeneuron is absent (operator not reconciled?); canonical checks NOT RUN"

	local vm_port
	vm_port=$(obs_service_http_port "$OBSERVABILITY_NAMESPACE" vmsingle-kubeneuron)
	log "test-observability: VMSingle service port ${vm_port} (resolved from the Service, not assumed)"
	obs_portforward "$OBSERVABILITY_NAMESPACE" service/vmsingle-kubeneuron "$vm_port" 18429 /health
	_OBS_QUERY_TIME=$(date -u +%s)
	local series result n
	for series in kubeneuron_gpu_temperature_celsius kubeneuron_gpu_power_watts kubeneuron_gpu_memory_used_bytes; do
		result=$(obs_vm_query "${series}{UUID=\"${uuid}\"}")
		n=$(printf '%s' "$result" | jq 'length')
		[ "${n:-0}" -ge 1 ] ||
			die "canonical series ${series} has no sample for UUID ${uuid} in VMSingle (vmalert rules not evaluated, or vmagent not scraping the exporter)"
		# The M2 label contract on the selected sample: node, gpu_uuid and
		# gpu_model must be present and non-empty, and the two renamed labels
		# must equal what the exporter itself stamped (UUID, modelName) —
		# vmagent copies them, it does not invent them. node_pool must equal
		# the EKS nodegroup label read from the node above. Each is asserted
		# separately so the failure names the label, not "a label".
		local lbl
		for lbl in node gpu_uuid gpu_model node_pool; do
			printf '%s' "$result" | jq -e --arg l "$lbl" '.[0].metric[$l] // "" | length > 0' >/dev/null ||
				die "canonical series ${series} sample for UUID ${uuid} lacks the ${lbl} label (labels: $(printf '%s' "$result" | jq -c '.[0].metric'))"
		done
		printf '%s' "$result" | jq -e --arg v "$node" '.[0].metric.node == $v' >/dev/null ||
			die "canonical series ${series}: node=$(printf '%s' "$result" | jq -r '.[0].metric.node') but the GPU node is ${node}"
		printf '%s' "$result" | jq -e --arg v "$uuid" '.[0].metric.gpu_uuid == $v' >/dev/null ||
			die "canonical series ${series}: gpu_uuid=$(printf '%s' "$result" | jq -r '.[0].metric.gpu_uuid') but the exporter's UUID is ${uuid}"
		printf '%s' "$result" | jq -e --arg v "$model" '.[0].metric.gpu_model == $v' >/dev/null ||
			die "canonical series ${series}: gpu_model=$(printf '%s' "$result" | jq -r '.[0].metric.gpu_model') but the exporter's modelName is ${model}"
		printf '%s' "$result" | jq -e --arg v "$node_pool" '.[0].metric.node_pool == $v' >/dev/null ||
			die "canonical series ${series}: node_pool=$(printf '%s' "$result" | jq -r '.[0].metric.node_pool') but node ${node} carries eks.amazonaws.com/nodegroup=${node_pool}"
	done
	log "test-observability: label contract OK (node=${node}, gpu_uuid=${uuid}, gpu_model=${model}, node_pool=${node_pool})"

	# Signals this hardware cannot produce must be ABSENT, not zero. Row
	# remapping is Ampere and later, the T4 has no NVLink and no MIG, and the
	# fail-slow score needs at least three devices of one gpu_model while this
	# stand has one physical GPU. A sample with value 0 would mean the exporter
	# emitted a field it does not have (and a rule then recorded a lie), so a
	# `== 0` comparison is exactly the wrong evidence here: the only acceptable
	# query result is the empty set, and any sample is printed as a finding.
	# Scoped to this node so the check is about this stand's hardware.
	case "$GPU_INSTANCE_TYPE" in
	g4dn.*)
		case "$model" in
		*"Tesla T4"*) ;;
		*) die "GPU_INSTANCE_TYPE=${GPU_INSTANCE_TYPE} is a g4dn but the exporter reports modelName=${model}, not a Tesla T4; the absence expectations below are written for the T4 and cannot be applied" ;;
		esac
		log "test-observability: g4dn/Tesla T4 — unavailable signals must be absent (row remap, NVLink, fail-slow, MIG)"
		local absent_query unexpected
		for absent_query in \
			"kubeneuron_gpu_remapped_rows_total{node=\"${node}\"}" \
			"kubeneuron_gpu_row_remap_failed{node=\"${node}\"}" \
			"kubeneuron_gpu_row_remap_pending{node=\"${node}\"}" \
			"kubeneuron_gpu_nvlink_errors_total{node=\"${node}\"}" \
			"kubeneuron_gpu_nvlink_errors_per_second{node=\"${node}\"}" \
			"kubeneuron_gpu_fail_slow_score_ratio{node=\"${node}\"}" \
			"{node=\"${node}\",mig_profile!=\"\"}" \
			"{node=\"${node}\",GPU_I_PROFILE!=\"\"}"; do
			unexpected=$(obs_vm_query "$absent_query") ||
				die "VMSingle did not answer the absence query ${absent_query} within ${OBS_MAX_TIME}s"
			n=$(printf '%s' "$unexpected" | jq 'length')
			[ "${n:-1}" -eq 0 ] ||
				die "expected NO samples for ${absent_query} on a Tesla T4, found ${n}: $(printf '%s' "$unexpected" | jq -c '.[] | {metric, value}')"
		done
		log "test-observability: absence OK (no row-remap, NVLink, fail-slow or MIG samples for node ${node})"
		;;
	*)
		log "test-observability: GPU_INSTANCE_TYPE=${GPU_INSTANCE_TYPE} is not g4dn; the Tesla T4 absence checks do not apply and were NOT run"
		;;
	esac

	# memory_used must equal the raw MiB sample times 1048576 for the same GPU
	# at the same instant. Both reads are instant queries at one shared
	# timestamp; VMSingle serves the latest sample at or before it.
	local raw canon expected
	raw=$(obs_vm_query "DCGM_FI_DEV_FB_USED{UUID=\"${uuid}\"}" | jq -r '.[0].value[1] // empty')
	canon=$(obs_vm_query "kubeneuron_gpu_memory_used_bytes{UUID=\"${uuid}\"}" | jq -r '.[0].value[1] // empty')
	obs_portforward_close
	[ -n "$raw" ] || die "no raw DCGM_FI_DEV_FB_USED sample for UUID ${uuid} in VMSingle"
	[ -n "$canon" ] || die "no kubeneuron_gpu_memory_used_bytes sample for UUID ${uuid} in VMSingle"
	expected=$(jq -n --arg r "$raw" '($r | tonumber) * 1048576')
	jq -en --arg c "$canon" --arg e "$expected" '($c | tonumber) == ($e | tonumber)' >/dev/null ||
		die "kubeneuron_gpu_memory_used_bytes=${canon} but DCGM_FI_DEV_FB_USED*1048576=${expected} for UUID ${uuid}"
	log "test-observability: canonical OK (memory_used_bytes ${canon} = FB_USED ${raw} MiB * 1048576)"
	log "test-observability: PASS"
}

cmd_teardown() {
	guard_cluster_name
	if [ "$KEEP_CLUSTER" = "1" ]; then
		log "teardown: KEEP_CLUSTER=1, skipping destruction (debug only)"
		return 0
	fi
	require_cmd eksctl
	require_cmd aws
	# Last chance to tie the claim volumes to this run while the API server
	# still answers; after the delete below only the tags remain to find
	# them by, and a run that failed between a claim binding and its tagging
	# point would otherwise leave a volume nothing is scoped to see.
	log "teardown: tying this run's claim volumes to the run while the cluster is live"
	tag_run_volumes_if_live
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
		log "sweep: cluster still present; tying its claim volumes to the run, then retrying delete --force"
		tag_run_volumes_if_live
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
		# The exact cluster stack is handled on its own below, and last: the
		# nodegroup stacks reference its security groups, so it can only go
		# once they have, and it is the one stack that wedges.
		[ "$stack" != "$(cluster_stack_name)" ] || continue
		log "sweep: deleting leaked stack $stack"
		aws cloudformation delete-stack --region "$AWS_REGION" --stack-name "$stack" || true
		aws cloudformation wait stack-delete-complete --region "$AWS_REGION" --stack-name "$stack" || true
	done
	# The cluster stack's delete is exactly what wedges. Run a17 passed every
	# phase, and its cluster stack then sat in DELETE_FAILED behind two things
	# nothing in this file could see: the VPC-CNI ENI of the node ReplaceNode
	# terminated, left detached and `available`, and after that the EKS cluster
	# security group that ENI had pinned. Neither is a stack resource, so
	# CloudFormation cannot delete them and cannot delete the VPC around them;
	# both had to be removed by hand before the sweep could report clean.
	sweep_wedged_cluster_stack

	log "sweep: no orphaned EBS volumes tagged for this run"
	# ONLY volumes that carry this run's identity. The controller's SQLite
	# claim and the VMSingle claim are tagged kubeneuron:e2e + E2E_RUN_TAG=
	# CLUSTER_NAME by tag_run_volume while their cluster is live (deploy,
	# deploy-observability, and once more at teardown), after the PVC -> PV ->
	# EBS id chain (CSI handle, or a legacy in-tree volumeID plus the exact
	# KubernetesCluster tag) and the provisioner's own created-for tags agreed
	# on the exact volume.
	#
	# There is deliberately NO filter on kubernetes.io/created-for/pvc/namespace
	# here or in the audit below. The EBS CSI driver writes nothing that names
	# the cluster, so a filter on this stand's two namespaces matched every
	# cluster in the account that uses them, and this sweep could have deleted
	# another cluster's state. A volume this run cannot prove is its own is
	# not its to delete, and not its to report either. KubernetesCluster is
	# the legacy in-tree provisioner's tag and names the exact cluster, so it
	# stays for older clusters.
	local volumes cluster_volumes
	volumes=$(aws ec2 describe-volumes --region "$AWS_REGION" \
		--filters "Name=tag:kubeneuron:e2e,Values=true" \
		"Name=tag:${E2E_RUN_TAG},Values=${CLUSTER_NAME}" "Name=status,Values=available" \
		--query 'Volumes[].VolumeId' --output text)
	cluster_volumes=$(aws ec2 describe-volumes --region "$AWS_REGION" \
		--filters "Name=tag:KubernetesCluster,Values=${CLUSTER_NAME}" "Name=status,Values=available" \
		--query 'Volumes[].VolumeId' --output text)
	# Deduplicate: the two filters may overlap, and a second delete's failure
	# once flagged a leak on an account that was clean.
	local vol
	for vol in $(printf '%s %s' "$volumes" "$cluster_volumes" |
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
	#
	# Run-scoped, like the deletions: the run tag, the legacy cluster tag, the
	# volumes this run RECORDED when it tagged them, and the claims it could
	# never tie to itself. No namespace filter (see the delete pass).
	local volume_states="Name=status,Values=available,in-use,error"
	add_remaining "volumes (run-tagged)" "$(aws ec2 describe-volumes --region "$AWS_REGION" \
		--filters "Name=tag:kubeneuron:e2e,Values=true" \
		"Name=tag:${E2E_RUN_TAG},Values=${CLUSTER_NAME}" "$volume_states" \
		--query "Volumes[].VolumeId|join(',',@)" --output text 2>/dev/null || true)"
	add_remaining "volumes (legacy tag)" "$(aws ec2 describe-volumes --region "$AWS_REGION" \
		--filters "Name=tag:KubernetesCluster,Values=${CLUSTER_NAME}" "$volume_states" \
		--query "Volumes[].VolumeId|join(',',@)" --output text 2>/dev/null || true)"
	# A recorded volume that still exists and still carries the run tag is
	# already in the run-tagged line; one that exists WITHOUT the tag is the
	# case nothing else can see. Tags do not vanish on their own, so it is
	# named on its own and deliberately not deleted: the identity that made it
	# this run's to delete is gone, and a human decides.
	add_remaining "volumes (recorded for this run, no longer run-tagged)" "$(recorded_volumes_untagged)"
	# Claims whose volume this run could never tie to itself. Nothing in this
	# sweep is scoped to find that volume, so the sweep cannot say the account
	# is clean; it says this instead, and how to clear it.
	add_remaining "claims never tied to this run (find the volume by hand, then remove ${UNPROVEN_CLAIMS_FILE})" "$(unproven_claims_for_run)"
	add_remaining "recycle role" "$(aws iam get-role --role-name "$RECYCLE_ROLE_NAME" \
		--query "Role.RoleName" --output text 2>/dev/null || true)"

	if [ -n "$remaining" ]; then
		printf 'sweep: these still exist after the deletions:%b\n' "$remaining" >&2
		die "the account is not clean — investigate before the next run"
	fi
	# ECR is deliberately absent from the assertion and therefore from this
	# line: the images are deleted best-effort and cost cents, and probing them
	# would claim a verification this does not perform.
	log "sweep: clean (verified against AWS: no cluster, EC2, stacks, run-tagged or recorded volumes, unproven claims or recycle role; run images deleted best-effort)"
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

# wait_for_controller_config blocks until the CONTROLLER has loaded the
# snapshot the operator advertises on the root status (/readyz reports
# "ready config=<digest>"). Ready on the CRs and on the root object means the
# operator compiled them; the controller only picks that up through the
# mounted-file hot-reload, and kubelet propagation adds up to ~2 minutes.
# Signal classification and playbook binding are both open-time, so a fault
# injected inside that window is classified by the PREVIOUS snapshot: run 10
# injected the drain phase's XID three seconds after its mapping went Ready,
# the controller opened the incident under the built-in class, and the phase
# waited ten minutes for an incident that could never open. Fallback: restart
# the controller — a fresh pod mounts the already-updated ConfigMap at once.
wait_for_controller_config() {
	local digest waited=0
	digest=$(kubectl -n "$RUNTIME_NAMESPACE" get kubeneuron "$ROOT_NAME" \
		-o jsonpath='{.status.configDigest}')
	[ -n "$digest" ] || die "root has no configDigest to wait on"
	log "waiting for the controller to load configuration ${digest:0:12}"
	while ((waited < 180)); do
		if api GET "/readyz" 2>/dev/null | grep -q "config=${digest}"; then
			return 0
		fi
		sleep 10
		waited=$((waited + 10))
	done
	log "loaded digest never matched; restarting the controller to load the compiled snapshot"
	kubectl -n "$RUNTIME_NAMESPACE" rollout restart "deployment/${ROOT_NAME}-controller"
	kubectl -n "$RUNTIME_NAMESPACE" rollout status "deployment/${ROOT_NAME}-controller" --timeout=5m
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
		# Every mutable operator route requires an Idempotency-Key
		# (docs/reference-api.md). A v0.4 controller answers a resolve without
		# one with 400, and close_incident used to swallow that: run 10 left
		# every phase's incident open, and the next phase on the node attached
		# to it. One fresh key per call; nothing here is ever retried.
		curl -fsS --max-time "$API_MAX_TIME" -X POST -H 'Content-Type: application/json' \
			-H "Authorization: Bearer ${_API_TOKEN}" \
			-H "Idempotency-Key: hw-e2e-$(date -u +%s%N)-$$-$RANDOM" \
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

# ---------------------------------------------------------------------------
# run-scoped EBS volumes
#
# claim_unproven <namespace>/<pvc> <reason> records that the volume behind one
# of this harness's claims could not be tied to this run, and dies. The record
# is the point: the sweep reads it and refuses to call the account clean, so a
# claim whose volume was never proven is a loud failure at teardown rather
# than an `available` volume nobody is scoped to see.
claim_unproven() {
	local claim="$1" reason="$2"
	mkdir -p "$E2E_STATE_DIR"
	printf '%s %s %s\n' "$CLUSTER_NAME" "$claim" "$reason" >>"$UNPROVEN_CLAIMS_FILE"
	die "cannot tie the volume behind claim ${claim} to run ${CLUSTER_NAME}: ${reason}; recorded in ${UNPROVEN_CLAIMS_FILE}, and the sweep will refuse to report clean until that record is resolved by hand"
}

# volume_tag <describe-volumes JSON> <key> prints the first volume's value for
# that tag key, or nothing.
volume_tag() {
	printf '%s' "$1" | jq -r --arg k "$2" '.Volumes[0].Tags[]? | select(.Key == $k) | .Value' | head -n1
}

# tag_run_volume <namespace> <pvc> proves which EBS volume backs ONE claim this
# harness created and stamps it with this run's identity, so the sweep can
# delete and audit it by that identity alone once the cluster is gone.
#
# Every link is checked and the chain fails closed: the claim must be Bound to
# a PV whose claimRef names this claim by namespace, name AND uid; the PV must
# set exactly one recognised EBS source, decided by what its spec SETS and
# never by what it lacks: an ebs.csi.aws.com .spec.csi object with a vol-
# handle, or a legacy in-tree .spec.awsElasticBlockStore object whose
# volumeID is vol-<hex> or aws://<zone>/vol-<hex> (no other source, and not
# both); the volume must exist in this region; a legacy volume must be tagged
# LEGACY_CLUSTER_TAG=CLUSTER_NAME exactly; the provisioner's created-for tags
# on it must name this PV and this claim; and it must not already carry
# another run's tag. A volume that passes is tagged kubeneuron:e2e,
# E2E_RUN_TAG=CLUSTER_NAME and E2E_CLAIM_TAG=<namespace>/<pvc>; the tags are
# read back through the filter the sweep uses; the id is recorded. Anything
# else is claim_unproven: recorded, then fatal. There is no fallback and no
# `|| true` in here.
tag_run_volume() {
	local ns="$1" pvc="$2" claim="$1/$2"
	local pvc_doc phase pvc_uid pv_name pv_doc source driver vol vol_id vol_doc have
	log "run-volume: resolving the EBS volume behind claim ${claim}"
	pvc_doc=$(kubectl -n "$ns" get pvc "$pvc" -o json 2>/dev/null) ||
		claim_unproven "$claim" "the claim could not be read"
	phase=$(printf '%s' "$pvc_doc" | jq -r '.status.phase // ""')
	[ "$phase" = "Bound" ] || claim_unproven "$claim" "the claim is '${phase}', not Bound"
	pvc_uid=$(printf '%s' "$pvc_doc" | jq -r '.metadata.uid // ""')
	pv_name=$(printf '%s' "$pvc_doc" | jq -r '.spec.volumeName // ""')
	if [ -z "$pvc_uid" ] || [ -z "$pv_name" ]; then
		claim_unproven "$claim" "the claim has no uid or no volumeName"
	fi
	pv_doc=$(kubectl get pv "$pv_name" -o json 2>/dev/null) ||
		claim_unproven "$claim" "its PersistentVolume ${pv_name} could not be read"
	printf '%s' "$pv_doc" | jq -e --arg ns "$ns" --arg name "$pvc" --arg uid "$pvc_uid" \
		'.spec.claimRef.namespace == $ns and .spec.claimRef.name == $name and .spec.claimRef.uid == $uid' >/dev/null ||
		claim_unproven "$claim" "PersistentVolume ${pv_name} is claimed by $(printf '%s' "$pv_doc" | jq -r '"\(.spec.claimRef.namespace)/\(.spec.claimRef.name) uid \(.spec.claimRef.uid)"'), not by this claim"
	# The source is whichever of the two EBS schemas the spec SETS (any
	# non-null value counts as set, so a malformed one is still seen). None
	# (nfs, hostPath, a stripped spec) or both is refused, not guessed; an
	# absent .spec.csi on its own proves nothing.
	source=$(printf '%s' "$pv_doc" | jq -r '[(if .spec.csi != null then "csi" else empty end),
		(if .spec.awsElasticBlockStore != null then "awsElasticBlockStore" else empty end)] | join("+")')
	case "$source" in
	csi)
		driver=$(printf '%s' "$pv_doc" | jq -r '(.spec.csi | objects | .driver | strings) // ""')
		[ "$driver" = "$EBS_CSI_DRIVER" ] ||
			claim_unproven "$claim" "PersistentVolume ${pv_name} is provisioned by '${driver}', not ${EBS_CSI_DRIVER}"
		vol=$(printf '%s' "$pv_doc" | jq -r '(.spec.csi.volumeHandle | strings) // ""')
		[[ "$vol" =~ ^vol-[0-9a-f]{8,17}$ ]] ||
			claim_unproven "$claim" "PersistentVolume ${pv_name} has CSI volumeHandle '${vol}', not an EBS volume id"
		;;
	awsElasticBlockStore)
		# Only the id captured by this anchored pattern is ever handed to aws:
		# the in-tree form aws://<zone>/vol-<hex> (the zone may be empty) or a
		# bare vol-<hex>. A non-object source yields '' and is refused here.
		vol_id=$(printf '%s' "$pv_doc" | jq -r '(.spec.awsElasticBlockStore | objects | .volumeID | strings) // ""')
		[[ "$vol_id" =~ ^(aws://[a-z0-9-]*/)?(vol-[0-9a-f]{8,17})$ ]] ||
			claim_unproven "$claim" "PersistentVolume ${pv_name} has awsElasticBlockStore volumeID '${vol_id}', not an EBS volume id"
		vol="${BASH_REMATCH[2]}"
		;;
	"")
		claim_unproven "$claim" "PersistentVolume ${pv_name} has neither a csi nor an awsElasticBlockStore source; not a volume this harness can name"
		;;
	*)
		claim_unproven "$claim" "PersistentVolume ${pv_name} has more than one volume source (${source}); refusing to pick one"
		;;
	esac
	vol_doc=$(aws ec2 describe-volumes --region "$AWS_REGION" --volume-ids "$vol" --output json 2>/dev/null) ||
		claim_unproven "$claim" "EBS volume ${vol} does not exist in ${AWS_REGION}"
	[ "$(printf '%s' "$vol_doc" | jq -r '.Volumes | length')" = "1" ] ||
		claim_unproven "$claim" "describe-volumes ${vol} did not return exactly one volume"
	# A legacy PV names no driver and nothing in its spec names the cluster,
	# so the in-tree provisioner's cluster tag must name THIS cluster exactly
	# before the created-for tags are consulted.
	if [ "$source" = awsElasticBlockStore ]; then
		have=$(volume_tag "$vol_doc" "$LEGACY_CLUSTER_TAG")
		[ "$have" = "$CLUSTER_NAME" ] ||
			claim_unproven "$claim" "EBS volume ${vol} is tagged ${LEGACY_CLUSTER_TAG}='${have}', not ${CLUSTER_NAME}; a legacy in-tree volume must name this cluster"
	fi
	# The provisioner's own tags are an independent witness to the same chain,
	# written when the volume was provisioned; they must agree with Kubernetes.
	have=$(volume_tag "$vol_doc" "$CSI_PV_NAME_TAG")
	[ "$have" = "$pv_name" ] ||
		claim_unproven "$claim" "EBS volume ${vol} is tagged ${CSI_PV_NAME_TAG}='${have}', not ${pv_name}"
	have=$(volume_tag "$vol_doc" "$CSI_PVC_NAME_TAG")
	[ "$have" = "$pvc" ] ||
		claim_unproven "$claim" "EBS volume ${vol} is tagged ${CSI_PVC_NAME_TAG}='${have}', not ${pvc}"
	have=$(volume_tag "$vol_doc" "$CSI_PVC_NAMESPACE_TAG")
	[ "$have" = "$ns" ] ||
		claim_unproven "$claim" "EBS volume ${vol} is tagged ${CSI_PVC_NAMESPACE_TAG}='${have}', not ${ns}"
	have=$(volume_tag "$vol_doc" "$E2E_RUN_TAG")
	if [ -n "$have" ] && [ "$have" != "$CLUSTER_NAME" ]; then
		claim_unproven "$claim" "EBS volume ${vol} is already tagged ${E2E_RUN_TAG}='${have}', another run's"
	fi
	aws ec2 create-tags --region "$AWS_REGION" --resources "$vol" \
		--tags "Key=kubeneuron:e2e,Value=true" \
		"Key=${E2E_RUN_TAG},Value=${CLUSTER_NAME}" \
		"Key=${E2E_CLAIM_TAG},Value=${claim}" >/dev/null ||
		claim_unproven "$claim" "create-tags on EBS volume ${vol} failed"
	# Read back through the exact filter the sweep will use, not the call's
	# exit status: a tag that did not land is a volume the sweep cannot see.
	have=$(aws ec2 describe-volumes --region "$AWS_REGION" --volume-ids "$vol" \
		--filters "Name=tag:kubeneuron:e2e,Values=true" \
		"Name=tag:${E2E_RUN_TAG},Values=${CLUSTER_NAME}" \
		"Name=tag:${E2E_CLAIM_TAG},Values=${claim}" \
		--query 'Volumes[].VolumeId' --output text 2>/dev/null) ||
		claim_unproven "$claim" "EBS volume ${vol} could not be read back after create-tags"
	[ "$have" = "$vol" ] ||
		claim_unproven "$claim" "EBS volume ${vol} does not carry the run tags after create-tags"
	mkdir -p "$E2E_STATE_DIR"
	if ! grep -qsxF "${CLUSTER_NAME} ${vol} ${claim}" "$RUN_VOLUMES_FILE"; then
		printf '%s %s %s\n' "$CLUSTER_NAME" "$vol" "$claim" >>"$RUN_VOLUMES_FILE"
	fi
	if [ "$source" = awsElasticBlockStore ]; then
		log "run-volume: PV ${pv_name} is a legacy in-tree awsElasticBlockStore volume; ${LEGACY_CLUSTER_TAG}=${CLUSTER_NAME} on ${vol} proves its cluster"
	fi
	log "run-volume: claim ${claim} is EBS volume ${vol} (PV ${pv_name}); tagged ${E2E_RUN_TAG}=${CLUSTER_NAME}"
}

# tag_run_volumes_if_live is the last chance to prove ownership while the API
# server still answers: teardown and a standalone sweep call it before any
# `eksctl delete cluster`. Each of the two known claims that exists is resolved
# and tagged; one that does not exist (its phase never ran) is skipped by
# name; one that exists and cannot be proven is recorded as unproven and
# left, and the sweep then refuses to report clean. It never stops the
# deletion that follows — the cluster is the cost, and goes either way — so
# every step that can die runs in its own subshell, and this returns 0.
#
# The kubeconfig it reads the claims through, whether an earlier phase left
# it or it is written here, is not trusted until assert_kube_target has
# proven that its selected context's server is the endpoint EKS reports for
# exactly CLUSTER_NAME. A context name is mutable and a file at the expected
# path proves nothing: a stale or foreign kubeconfig would resolve another
# cluster's claim to another cluster's volume and stamp THIS run's identity on
# it, which is the sweep's licence to delete it. So the target is validated
# once, explicitly, after the kubeconfig is obtained or reused and before any
# claim is looked up or any volume tagged; a validation that does not hold
# records every known claim as unproven (the sweep then refuses to report
# clean) and returns without a single kubectl read or create-tags. The
# kubectl wrapper repeats the same check on every call behind this, as
# defence in depth, not as the gate.
tag_run_volumes_if_live() {
	local claims=("${RUNTIME_NAMESPACE}/${CONTROLLER_STATE_CLAIM}" "${OBSERVABILITY_NAMESPACE}/${VMSINGLE_CLAIM}")
	local claim ns pvc out why
	if ! aws eks describe-cluster --name "$CLUSTER_NAME" --region "$AWS_REGION" >/dev/null 2>&1; then
		log "run-volume: cluster $CLUSTER_NAME is not live; no claim can be resolved now (the sweep works from the tags already written)"
		return 0
	fi
	if [ ! -s "$KUBECONFIG" ]; then
		log "run-volume: no dedicated kubeconfig at $KUBECONFIG; writing one for the live cluster"
		init_kubeconfig
		if ! eksctl utils write-kubeconfig --cluster "$CLUSTER_NAME" --region "$AWS_REGION" \
			--kubeconfig "$KUBECONFIG" >/dev/null 2>&1; then
			for claim in "${claims[@]}"; do
				(claim_unproven "$claim" "the live cluster's kubeconfig could not be written before deletion") || true
			done
			return 0
		fi
	fi
	# Fail closed on the target before the first claim lookup: no kubectl
	# read and no tag action happens on a context that is not proven to be
	# this run's live cluster. assert_kube_target dies on any defect (empty
	# kubeconfig, no current context, unreadable server, EKS endpoint
	# unresolvable, server differing from the endpoint), so it runs in a
	# subshell and its message becomes the reason on every claim's record.
	if ! why=$( (assert_kube_target) 2>&1); then
		why=$(printf '%s' "$why" | sed 's/^hw-e2e: //' | tr '\n' ' ')
		log "run-volume: the dedicated kubeconfig at $KUBECONFIG is not proven to target the live cluster ${CLUSTER_NAME}; no claim is looked up and no volume is tagged on an unvalidated context (${why})"
		for claim in "${claims[@]}"; do
			(claim_unproven "$claim" "the dedicated kubeconfig was not proven to target the live cluster ${CLUSTER_NAME} before deletion: ${why}") || true
		done
		return 0
	fi
	for claim in "${claims[@]}"; do
		ns=${claim%/*}
		pvc=${claim#*/}
		if out=$( (kubectl -n "$ns" get pvc "$pvc" -o name) 2>&1); then
			(tag_run_volume "$ns" "$pvc") ||
				log "run-volume: claim ${claim} could not be tied to this run; recorded as unproven, and the sweep will refuse to report clean"
		elif printf '%s' "$out" | grep -q 'NotFound'; then
			log "run-volume: claim ${claim} does not exist; nothing to tag"
		else
			(claim_unproven "$claim" "the claim could not be listed before deletion: $(printf '%s' "$out" | tr '\n' ' ')") || true
		fi
	done
	return 0
}

# recorded_volumes_untagged prints, comma-joined, every volume RUN_VOLUMES_FILE
# records for THIS cluster that still exists (any state but deleting) without
# E2E_RUN_TAG=CLUSTER_NAME on it. Each is looked up by its own id and nothing
# is deleted here.
recorded_volumes_untagged() {
	[ -s "$RUN_VOLUMES_FILE" ] || return 0
	local cluster vol claim state tagged out=""
	while read -r cluster vol claim; do
		[ "$cluster" = "$CLUSTER_NAME" ] || continue
		state=$(aws ec2 describe-volumes --region "$AWS_REGION" --volume-ids "$vol" \
			--query 'Volumes[0].State' --output text 2>/dev/null || true)
		case "$state" in "" | None | deleting | deleted) continue ;; esac
		tagged=$(aws ec2 describe-volumes --region "$AWS_REGION" --volume-ids "$vol" \
			--filters "Name=tag:${E2E_RUN_TAG},Values=${CLUSTER_NAME}" \
			--query 'Volumes[].VolumeId' --output text 2>/dev/null || true)
		if [ "$tagged" != "$vol" ]; then
			log "sweep: recorded volume ${vol} (${claim}) still exists but no longer carries ${E2E_RUN_TAG}=${CLUSTER_NAME}; refusing to delete it"
			out="${out:+$out,}${vol}"
		fi
	done <"$RUN_VOLUMES_FILE"
	printf '%s' "$out"
}

# unproven_claims_for_run prints, ';'-joined, the unproven-claims records for
# THIS cluster: "<namespace>/<pvc> <reason>" each.
unproven_claims_for_run() {
	[ -s "$UNPROVEN_CLAIMS_FILE" ] || return 0
	awk -v c="$CLUSTER_NAME" '$1 == c { $1 = ""; sub(/^ /, ""); print }' "$UNPROVEN_CLAIMS_FILE" |
		sort -u | paste -sd ';' -
}

# ---------------------------------------------------------------------------
# networking orphans that wedge the exact cluster stack
#
# Two resources outlive `eksctl delete cluster` after a ReplaceNode, and both
# sit in the run's VPC without belonging to any CloudFormation stack, so the
# stack delete fails on the VPC and the sweep can only report the stack as a
# leftover. The VPC-CNI plugin allocates a secondary ENI per node, described
# `aws-K8S-i-<instance>`; when the controller terminates that instance the ENI
# is detached and stays `available`, which pins the EKS cluster security group
# it carries, which pins the VPC. Observed on run a17 (2026-09-13), removed by
# hand, and the sweep reported clean only after that.
#
# Everything below is scoped from the one CloudFormation stack that carries
# the exact CLUSTER_NAME. The VPC is that stack's own `VPC` resource, verified
# to be tagged for the exact cluster; the EKS cluster must already be gone;
# every candidate is re-read individually and must match the exact cluster on
# its own tags before it is deleted. A resource that fails any of those checks
# is logged and left alone, and the end-state assertion then reports the stack
# it keeps wedged — a refusal here is a loud failure, never a quiet leak.
readonly CNI_ENI_DESCRIPTION_PREFIX="aws-K8S-i-"
readonly CNI_ENI_CLUSTER_TAG="cluster.k8s.amazonaws.com/name"
readonly CNI_ENI_INSTANCE_TAG="node.k8s.amazonaws.com/instance_id"
readonly EKSCTL_CLUSTER_TAG="alpha.eksctl.io/cluster-name"
readonly EKS_CLUSTER_SG_PREFIX="eks-cluster-sg-"

cluster_stack_name() { printf 'eksctl-%s-cluster' "$CLUSTER_NAME"; }

# cluster_stack_status prints the exact cluster stack's status, or nothing
# when the stack no longer exists. describe-stacks by NAME resolves only live
# stacks, so a DELETE_COMPLETE stack is "nothing" here, which is what we want.
cluster_stack_status() {
	aws cloudformation describe-stacks --region "$AWS_REGION" \
		--stack-name "$(cluster_stack_name)" \
		--query 'Stacks[0].StackStatus' --output text 2>/dev/null || true
}

# cluster_stack_gone is true for the statuses that mean there is nothing left
# to delete: no stack, or a stack that finished deleting.
cluster_stack_gone() {
	[ -z "$1" ] || [ "$1" = "None" ] || [ "$1" = "DELETE_COMPLETE" ]
}

delete_cluster_stack() {
	aws cloudformation delete-stack --region "$AWS_REGION" --stack-name "$(cluster_stack_name)" || true
	wait_cluster_stack_delete
}

wait_cluster_stack_delete() {
	aws cloudformation wait stack-delete-complete --region "$AWS_REGION" \
		--stack-name "$(cluster_stack_name)" || true
}

# sweep_wedged_cluster_stack deletes the exact cluster stack, clearing the
# orphans that wedge it BEFORE each retry rather than after a retry that is
# known to fail: a cluster stack delete runs for minutes and, with the orphans
# still in the VPC, ends in the same DELETE_FAILED it started from.
#
# Three starting states, handled in order:
#   DELETE_IN_PROGRESS  another delete (eksctl's, the reaper's) is running.
#                       Wait for it to settle; touching the VPC underneath a
#                       running delete would be a race, and the settled status
#                       decides what happens next.
#   anything live       (CREATE_COMPLETE, ROLLBACK_COMPLETE, …) the stack was
#                       never deleted. A plain delete first: the EKS cluster is
#                       a resource of this stack and sweep_run_vpc refuses to
#                       clear anything while it exists, so there is nothing to
#                       clear before this delete.
#   DELETE_FAILED       the known wedge. Clear the orphans, then retry.
#
# Bounded to two cleanup passes: the first clears the ENI, and the stack's own
# security groups (which hold rules referencing the cluster group) go away
# only once the stack retries, so the cluster group can need a second pass —
# that is the sequence a17 showed, and it is the only reason there is a second
# pass at all. Whatever is left after that, the end-state assertion reports.
sweep_wedged_cluster_stack() {
	local stack status vpc pass
	stack=$(cluster_stack_name)
	status=$(cluster_stack_status)
	cluster_stack_gone "$status" && return 0
	if [ "$status" = "DELETE_IN_PROGRESS" ]; then
		log "sweep: $stack is DELETE_IN_PROGRESS; waiting for that delete to settle rather than racing it"
		wait_cluster_stack_delete
		status=$(cluster_stack_status)
		cluster_stack_gone "$status" && return 0
	fi
	if [ "$status" != "DELETE_FAILED" ]; then
		log "sweep: deleting leaked stack $stack ($status)"
		delete_cluster_stack
		status=$(cluster_stack_status)
		cluster_stack_gone "$status" && return 0
	fi
	for pass in 1 2; do
		if [ "$status" != "DELETE_FAILED" ]; then
			log "sweep: $stack is $status; leaving it to the end-state assertion"
			return 0
		fi
		log "sweep: $stack is DELETE_FAILED; clearing networking orphans in its VPC before retrying the delete (pass $pass of 2)"
		if ! vpc=$(sweep_run_vpc); then
			log "sweep: no exact VPC scope for $stack; leaving it to the end-state assertion"
			return 0
		fi
		sweep_orphaned_cni_enis "$vpc"
		sweep_orphaned_cluster_security_groups "$vpc"
		log "sweep: retrying delete of $stack"
		delete_cluster_stack
		status=$(cluster_stack_status)
		cluster_stack_gone "$status" && return 0
	done
	log "sweep: $stack is still $status after 2 passes; the end-state assertion reports it"
}

# record_complete is the guard on every jq record read below. Fields are
# emitted one per line, an empty field as an empty line, and the record ends
# with the literal `end`; it is complete only when exactly <n> fields precede
# that sentinel. A tab-separated record read with IFS=$'\t' is NOT safe here:
# bash treats tab as IFS whitespace, so consecutive tabs collapse and every
# field after an empty one shifts left — which once moved an ENI's instance
# tag into its cluster-tag slot and let a mismatched interface through.
record_complete() { # <n> <field>...
	local n=$1
	shift
	[ "$#" -eq $((n + 1)) ] && [ "${!#}" = "end" ]
}

# sweep_run_vpc prints the VPC id that the exact cluster stack owns, or
# returns 1 having logged why the scope could not be established. It refuses
# whenever the answer is not exactly one VPC that eksctl created for exactly
# this cluster, and it refuses outright while the EKS cluster still exists —
# a live cluster's ENIs and security groups are never ours to remove.
sweep_run_vpc() {
	local stack vpc tagged is_default
	stack=$(cluster_stack_name)
	if aws eks describe-cluster --region "$AWS_REGION" --name "$CLUSTER_NAME" \
		--query 'cluster.name' --output text >/dev/null 2>&1; then
		log "sweep: EKS cluster $CLUSTER_NAME still exists; refusing to clear networking orphans"
		return 1
	fi
	vpc=$(aws cloudformation describe-stack-resources --region "$AWS_REGION" \
		--stack-name "$stack" --logical-resource-id VPC \
		--query "StackResources[?ResourceType=='AWS::EC2::VPC'].PhysicalResourceId" \
		--output text 2>/dev/null | tr -s ' \t' '\n' | grep -v '^$' | grep -v '^None$' || true)
	if [ -z "$vpc" ]; then
		log "sweep: $stack has no VPC resource of its own; refusing to touch any VPC"
		return 1
	fi
	if [ "$(printf '%s\n' "$vpc" | wc -l)" -ne 1 ]; then
		log "sweep: $stack reports more than one VPC ($(printf '%s' "$vpc" | tr '\n' ' ')); refusing an ambiguous scope"
		return 1
	fi
	case "$vpc" in
	vpc-*) : ;;
	*)
		log "sweep: $stack VPC resource is '$vpc', not a VPC id; refusing"
		return 1
		;;
	esac
	# The stack said so; now the VPC itself must say so. eksctl stamps every
	# stack resource with the cluster name, so a VPC that is not tagged for
	# exactly this cluster is not one this run created.
	tagged=$(aws ec2 describe-vpcs --region "$AWS_REGION" --vpc-ids "$vpc" \
		--filters "Name=tag:${EKSCTL_CLUSTER_TAG},Values=${CLUSTER_NAME}" \
		--query "Vpcs[].VpcId|join(',',@)" --output text 2>/dev/null || true)
	if [ "$tagged" != "$vpc" ]; then
		log "sweep: $vpc is not tagged ${EKSCTL_CLUSTER_TAG}=${CLUSTER_NAME}; refusing"
		return 1
	fi
	is_default=$(aws ec2 describe-vpcs --region "$AWS_REGION" --vpc-ids "$vpc" \
		--query 'Vpcs[0].IsDefault' --output text 2>/dev/null || true)
	if [ "$is_default" != "False" ] && [ "$is_default" != "false" ]; then
		log "sweep: $vpc is the account's default VPC (IsDefault=$is_default); refusing"
		return 1
	fi
	printf '%s' "$vpc"
}

# sweep_orphaned_cni_enis deletes the detached VPC-CNI interfaces of this
# run's terminated nodes, and nothing else. Candidates come from a scoped
# query; each is then re-read on its own and must still be detached and
# `available`, in the run VPC, not requester-managed, a plain `interface`
# by InterfaceType (the a17 orphan was; an efa, trunk, or any other kind
# is not something the CNI's secondary-ENI path allocates, whatever it is
# described or tagged), described `aws-K8S-i-<instance>` by the CNI, and
# tied to the exact run: the instance it names is terminated (or already
# aged out of the API) and either that instance or the ENI itself is tagged
# for exactly this cluster.
readonly CNI_ENI_INTERFACE_TYPE="interface"
sweep_orphaned_cni_enis() {
	local vpc="$1" candidates eni doc fields
	require_cmd jq
	candidates=$(aws ec2 describe-network-interfaces --region "$AWS_REGION" \
		--filters "Name=vpc-id,Values=${vpc}" "Name=status,Values=available" \
		"Name=description,Values=${CNI_ENI_DESCRIPTION_PREFIX}*" \
		--query 'NetworkInterfaces[].NetworkInterfaceId' --output text 2>/dev/null |
		tr -s ' \t' '\n' | grep -v '^$' | grep -v '^None$' || true)
	[ -n "$candidates" ] || return 0
	for eni in $candidates; do
		doc=$(aws ec2 describe-network-interfaces --region "$AWS_REGION" \
			--network-interface-ids "$eni" --output json 2>/dev/null) || {
			log "sweep: $eni disappeared before revalidation; skipping"
			continue
		}
		mapfile -t fields < <(printf '%s' "$doc" | jq -r --arg cluster_tag "$CNI_ENI_CLUSTER_TAG" \
			--arg instance_tag "$CNI_ENI_INSTANCE_TAG" '
			.NetworkInterfaces // [] |
			if length != 1 then "count=\(length)" else .[0] |
				[ .VpcId // "", .Status // "", (if .Attachment == null then "detached" else "attached" end),
				  ((.RequesterManaged // false) | tostring), .InterfaceType // "", .Description // "",
				  (((.TagSet // []) | map(select(.Key == $cluster_tag)) | .[0].Value) // ""),
				  (((.TagSet // []) | map(select(.Key == $instance_tag)) | .[0].Value) // ""),
				  "end" ] | .[]
			end' 2>/dev/null)
		case "${fields[0]-}" in
		count=*)
			log "sweep: $eni resolved to ${fields[0]} interfaces, not one; leaving it"
			continue
			;;
		esac
		if ! record_complete 8 "${fields[@]}"; then
			log "sweep: cannot parse the description of $eni; leaving it"
			continue
		fi
		local eni_vpc status attachment requester interface_type description cluster_tag instance_tag
		eni_vpc=${fields[0]} status=${fields[1]} attachment=${fields[2]} requester=${fields[3]}
		interface_type=${fields[4]} description=${fields[5]} cluster_tag=${fields[6]} instance_tag=${fields[7]}
		if [ "$eni_vpc" != "$vpc" ]; then
			log "sweep: $eni is in $eni_vpc, not the run VPC $vpc; leaving it"
			continue
		fi
		if [ "$status" != "available" ] || [ "$attachment" != "detached" ]; then
			log "sweep: $eni is $status/$attachment on re-read, not a detached orphan; leaving it"
			continue
		fi
		if [ "$requester" != "false" ]; then
			log "sweep: $eni is requester-managed (an AWS service owns it); leaving it"
			continue
		fi
		# The one kind the CNI allocates. An efa or trunk interface in the run
		# VPC, however it is described or tagged, is not the a17 orphan and is
		# not deleted on the strength of the identifiers that follow.
		if [ "$interface_type" != "$CNI_ENI_INTERFACE_TYPE" ]; then
			log "sweep: $eni has InterfaceType '${interface_type}', not '${CNI_ENI_INTERFACE_TYPE}'; leaving it"
			continue
		fi
		# The CNI writes `aws-K8S-<instance id>`; the prefix ends in `i-`, so
		# the remainder is the hexadecimal part of the id and nothing else.
		local instance
		instance="i-${description#"$CNI_ENI_DESCRIPTION_PREFIX"}"
		case "$description" in
		"$CNI_ENI_DESCRIPTION_PREFIX"[0-9a-f]*) : ;;
		*)
			log "sweep: $eni is described '$description', not a VPC-CNI interface; leaving it"
			continue
			;;
		esac
		if [ -n "$instance_tag" ] && [ "$instance_tag" != "$instance" ]; then
			log "sweep: $eni names $instance but is tagged for $instance_tag; leaving it"
			continue
		fi
		if ! cni_eni_instance_is_this_runs "$eni" "$instance" "$cluster_tag"; then
			continue
		fi
		log "sweep: deleting orphaned VPC-CNI interface $eni ($description)"
		aws ec2 delete-network-interface --region "$AWS_REGION" --network-interface-id "$eni" ||
			log "sweep: could not delete $eni; the end-state assertion will report the stack"
	done
}

# cni_eni_instance_is_this_runs decides whether the instance an ENI names is
# a terminated node of exactly this run. Terminated instances leave the EC2
# API after about an hour, and a reaper-driven sweep can arrive later than
# that, so an instance that is gone is accepted only when the ENI's own
# cluster tag names this cluster. An instance in any live state is refused
# whatever it is tagged: an interface a running node just released is not an
# orphan.
cni_eni_instance_is_this_runs() {
	local eni="$1" instance="$2" eni_cluster_tag="$3" doc state cluster_tag run_tag
	doc=$(aws ec2 describe-instances --region "$AWS_REGION" --instance-ids "$instance" \
		--output json 2>/dev/null) || doc='{"Reservations":[]}'
	local fields
	mapfile -t fields < <(printf '%s' "$doc" | jq -r --arg run_tag "$E2E_RUN_TAG" '
		[ .Reservations[]?.Instances[]? ] |
		if length == 0 then "absent" else .[0] |
			[ .State.Name // "",
			  (((.Tags // []) | map(select(.Key == "aws:eks:cluster-name")) | .[0].Value) // ""),
			  (((.Tags // []) | map(select(.Key == $run_tag)) | .[0].Value) // ""),
			  "end" ] | .[]
		end' 2>/dev/null)
	if [ "${fields[0]-}" = "absent" ]; then
		if [ "$eni_cluster_tag" = "$CLUSTER_NAME" ]; then
			return 0
		fi
		log "sweep: $eni names $instance, which no longer exists, and the interface is tagged '${eni_cluster_tag}' rather than ${CLUSTER_NAME}; leaving it"
		return 1
	fi
	if ! record_complete 3 "${fields[@]}"; then
		log "sweep: cannot parse instance $instance named by $eni; leaving it"
		return 1
	fi
	state=${fields[0]} cluster_tag=${fields[1]} run_tag=${fields[2]}
	if [ "$state" != "terminated" ]; then
		log "sweep: $eni names $instance, which is $state, not terminated; leaving it"
		return 1
	fi
	if [ "$cluster_tag" = "$CLUSTER_NAME" ] || [ "$run_tag" = "$CLUSTER_NAME" ] ||
		[ "$eni_cluster_tag" = "$CLUSTER_NAME" ]; then
		return 0
	fi
	log "sweep: $eni names terminated $instance, but neither it nor the interface is tagged for ${CLUSTER_NAME} (instance: aws:eks:cluster-name='${cluster_tag}' ${E2E_RUN_TAG}='${run_tag}'); leaving it"
	return 1
}

# sweep_orphaned_cluster_security_groups deletes the EKS cluster security
# group of exactly this cluster once nothing references it. EKS names it
# eks-cluster-sg-<cluster>-<n> and tags it with aws:eks:cluster-name and
# kubernetes.io/cluster/<cluster>=owned; every candidate is re-read and must
# carry all three, be in the run VPC, not be the VPC's default group, and have
# no network interface anywhere still attached to it. The stack's own groups
# (tagged by eksctl, not EKS) are not candidates: the stack deletes those.
sweep_orphaned_cluster_security_groups() {
	local vpc="$1" candidates sg doc fields
	require_cmd jq
	candidates=$(aws ec2 describe-security-groups --region "$AWS_REGION" \
		--filters "Name=vpc-id,Values=${vpc}" \
		"Name=tag:aws:eks:cluster-name,Values=${CLUSTER_NAME}" \
		"Name=group-name,Values=${EKS_CLUSTER_SG_PREFIX}${CLUSTER_NAME}-*" \
		--query 'SecurityGroups[].GroupId' --output text 2>/dev/null |
		tr -s ' \t' '\n' | grep -v '^$' | grep -v '^None$' || true)
	[ -n "$candidates" ] || return 0
	for sg in $candidates; do
		doc=$(aws ec2 describe-security-groups --region "$AWS_REGION" \
			--group-ids "$sg" --output json 2>/dev/null) || {
			log "sweep: $sg disappeared before revalidation; skipping"
			continue
		}
		mapfile -t fields < <(printf '%s' "$doc" | jq -r --arg owned_tag "kubernetes.io/cluster/${CLUSTER_NAME}" '
			.SecurityGroups // [] |
			if length != 1 then "count=\(length)" else .[0] |
				[ .VpcId // "", .GroupName // "",
				  (((.Tags // []) | map(select(.Key == "aws:eks:cluster-name")) | .[0].Value) // ""),
				  (((.Tags // []) | map(select(.Key == $owned_tag)) | .[0].Value) // ""),
				  "end" ] | .[]
			end' 2>/dev/null)
		case "${fields[0]-}" in
		count=*)
			log "sweep: $sg resolved to ${fields[0]} groups, not one; leaving it"
			continue
			;;
		esac
		if ! record_complete 4 "${fields[@]}"; then
			log "sweep: cannot parse the description of $sg; leaving it"
			continue
		fi
		local sg_vpc name cluster_tag owned_tag
		sg_vpc=${fields[0]} name=${fields[1]} cluster_tag=${fields[2]} owned_tag=${fields[3]}
		if [ "$sg_vpc" != "$vpc" ]; then
			log "sweep: $sg is in $sg_vpc, not the run VPC $vpc; leaving it"
			continue
		fi
		if [ "$name" = "default" ]; then
			log "sweep: $sg is the VPC default group; leaving it"
			continue
		fi
		case "$name" in
		"${EKS_CLUSTER_SG_PREFIX}${CLUSTER_NAME}-"*) : ;;
		*)
			log "sweep: $sg is named '$name', not the EKS cluster group of ${CLUSTER_NAME}; leaving it"
			continue
			;;
		esac
		if [ "$cluster_tag" != "$CLUSTER_NAME" ] || [ "$owned_tag" != "owned" ]; then
			log "sweep: $sg is tagged aws:eks:cluster-name='${cluster_tag}' kubernetes.io/cluster/${CLUSTER_NAME}='${owned_tag}'; not exactly this cluster's; leaving it"
			continue
		fi
		local references
		references=$(aws ec2 describe-network-interfaces --region "$AWS_REGION" \
			--filters "Name=group-id,Values=${sg}" \
			--query 'NetworkInterfaces[].NetworkInterfaceId|join(",",@)' --output text 2>/dev/null || true)
		if [ -n "$references" ] && [ "$references" != "None" ]; then
			log "sweep: $sg is still referenced by network interface(s) $references; leaving it"
			continue
		fi
		log "sweep: deleting orphaned EKS cluster security group $sg ($name)"
		aws ec2 delete-security-group --region "$AWS_REGION" --group-id "$sg" ||
			log "sweep: could not delete $sg yet (a stack-owned rule may still reference it); the stack retry decides"
	done
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
	drain_fixture_cleanup
	if [ -n "${_OPEN_INCIDENT:-}" ]; then
		close_incident "$_OPEN_INCIDENT" "phase ended without closing its incident"
		_OPEN_INCIDENT=""
	fi
	if [ -n "$_PF_PID" ]; then
		kill "$_PF_PID" 2>/dev/null || true
	fi
	# test-observability's bounded port-forward (exporter or VMSingle): the
	# same close helper the phase uses, so the child is waited on and reaped,
	# not left as a zombie behind a kill the trap never checked.
	obs_portforward_close
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
	deploy-observability) cmd_deploy_observability "$@" ;;
	test-dryrun) cmd_test_dryrun "$@" ;;
	test-threshold) cmd_test_threshold "$@" ;;
	test-dcgm) cmd_test_dcgm "$@" ;;
	test-verify-recur) cmd_test_verify_recur "$@" ;;
	test-drain) cmd_test_drain "$@" ;;
	test-destructive) cmd_test_destructive "$@" ;;
	test-observability) cmd_test_observability "$@" ;;
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
