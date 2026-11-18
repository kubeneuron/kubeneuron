#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0

set -euo pipefail

# KubeNeuron one-command installer.
#
#   ./deploy/install.sh                             # from a repo checkout
#   ./install.sh --version v0.1.0                   # from a release asset
#   curl -sfL https://github.com/kubeneuron/kubeneuron/releases/latest/download/install.sh \
#     | bash -s -- --version latest                 # once releases are public
#   gh release download v0.1.0 -p 'install-*.sh' -O - | bash -s -- --version v0.1.0
#                                                   # while the repo is private
#
# Installs CRDs and the operator, generates the operational secrets
# (operator API token, webhook token, optional panel admin password) and
# the four-Secret TLS material, applies a starter KubeNeuron with an
# observe-first playbook, and waits for Ready. Everything stays DryRun:
# nothing this script installs can touch a node destructively.
#
# Flags:
#   --name NAME          root object name           (default: kubeneuron)
#   --namespace NS       runtime namespace          (default: kube-neuron)
#   --version vX.Y.Z|latest
#                        released install manifest to download (public curl,
#                        gh fallback); omitted: use the local checkout
#   --manifest FILE      pre-downloaded install manifest (skips download)
#   --admin USER         panel username to create   (default: admin;
#                        empty string skips password sign-in)
#   --controller-image I / --agent-image I
#                        image overrides (air-gapped / development)
#   --observability managed|disabled
#                        managed: the operator runs a Collector, a metrics
#                        store, its alert rules and Grafana in the namespace;
#                        disabled: none of them. Omitted: managed for a new
#                        installation, an existing one keeps its own
#   --uninstall          remove the installation, retaining SQLite state
#   --delete-data        also remove SQLite state and the Managed metrics
#                        store's claim (confirmation required)
#   --yes                confirm --uninstall --delete-data non-interactively
#
# Requirements: kubectl (>=1.29 server), openssl. For --version: gh.
# For the panel password hash: htpasswd, or a kubeneuronctl binary.

NAME=kubeneuron
REQUESTED_PACK=""
NS=kube-neuron
VERSION=""
MANIFEST=""
ADMIN_USER="admin"
PROFILE=auto
UNINSTALL=0
DELETE_DATA=0
ASSUME_YES=0
OBSERVABILITY=""
CONTROLLER_IMAGE=""
AGENT_IMAGE=""

usage() {
	cat <<'USAGE'
KubeNeuron one-command installer.

  ./deploy/install.sh                    # from a repo checkout
  ./install.sh --version v0.1.0          # from a release asset
  curl -sfL <release-url>/install.sh | bash -s -- --version latest

Flags:
  --name NAME            root object name           (default: kubeneuron)
  --policy-pack PACK     cloud (new installation default) or bare-metal-alpha
                         --policy-pack bare-metal-alpha enables node-level actions on the operator and on this installation (Alpha, not validated on real hardware). These actions can reboot hosts and reset GPUs.
  --namespace NS         runtime namespace          (default: kube-neuron)
  --version vX.Y.Z|latest  released install manifest (public curl, gh fallback)
  --manifest FILE        pre-downloaded install manifest
  --profile PROFILE      auto (default), host-al2023, gpu-operator-ubuntu, or none
  --admin USER           panel username to create   (default: admin; "" skips)
  --controller-image I / --agent-image I   image overrides (air-gapped/dev)
  --observability MODE   managed: Collector, metrics store, alert rules and Grafana
                         in the namespace; disabled: none of them. Omitted: managed
                         for a new installation; an existing one keeps its own
  --uninstall            remove this installation; retain SQLite state and metrics by default
  --delete-data          with --uninstall: also delete the SQLite PVC with the audit
                         data and the Managed metrics store's PVC
  --yes                  confirm --delete-data without an interactive prompt
USAGE
}

while (($#)); do
	case $1 in
	--policy-pack) REQUESTED_PACK=${2:?missing policy pack}; shift 2 ;;
	--name) NAME=$2; shift 2 ;;
	--namespace) NS=$2; shift 2 ;;
	--version) VERSION=$2; shift 2 ;;
	--manifest) MANIFEST=$2; shift 2 ;;
	--profile) PROFILE=${2:?missing profile}; shift 2 ;;
	--admin) ADMIN_USER=$2; shift 2 ;;
	--controller-image) CONTROLLER_IMAGE=$2; shift 2 ;;
	--agent-image) AGENT_IMAGE=$2; shift 2 ;;
	--observability) OBSERVABILITY=${2:?missing observability mode}; shift 2 ;;
	--uninstall) UNINSTALL=1; shift ;;
	--delete-data) DELETE_DATA=1; shift ;;
	--yes) ASSUME_YES=1; shift ;;
	-h|--help) usage; exit 0 ;;
	*) echo "unknown flag: $1" >&2; exit 2 ;;
	esac
done

say() { printf '\033[1;34m==>\033[0m %s\n' "$*"; }
die() { printf '\033[1;31mERROR:\033[0m %s\n' "$*" >&2; exit 1; }

((DELETE_DATA == 0 || UNINSTALL == 1)) || die "--delete-data requires --uninstall"
((ASSUME_YES == 0 || DELETE_DATA == 1)) || die "--yes requires --uninstall --delete-data"
[[ $NAME =~ ^[a-z0-9]([-a-z0-9]*[a-z0-9])?$ && ${#NAME} -le 63 ]] || die "invalid installation name"
[[ $NS =~ ^[a-z0-9]([-a-z0-9]*[a-z0-9])?$ && ${#NS} -le 63 ]] || die "invalid namespace"

case $OBSERVABILITY in
''|managed|disabled) ;;
*) die "unknown --observability $OBSERVABILITY (managed or disabled; for your own Collector or store see docs/observability.md)" ;;
esac

case $PROFILE in
auto|host-al2023|gpu-operator-ubuntu|none) ;;
*) die "unknown --profile $PROFILE (see --help)" ;;
esac
case $REQUESTED_PACK in
""|cloud|bare-metal-alpha) ;;
*) die "--policy-pack must be cloud or bare-metal-alpha" ;;
esac

command -v kubectl >/dev/null || die "kubectl is required"
command -v openssl >/dev/null || die "openssl is required"
kubectl version >/dev/null 2>&1 || die "no reachable Kubernetes cluster (check KUBECONFIG)"

# Piped execution (curl | bash) has no BASH_SOURCE file: the checkout
# path is then unavailable and --version/--manifest is required.
repo_root=""
if [[ -n ${BASH_SOURCE[0]:-} && -f ${BASH_SOURCE[0]:-} ]]; then
	script_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
	candidate=$(cd -- "$script_dir/.." && pwd)
	[[ -d $candidate/config/crd && -f $candidate/go.mod ]] && repo_root=$candidate
fi

REPO_SLUG=${KUBENEURON_REPO:-kubeneuron/kubeneuron}
resolve_latest() {
	local tag
	# Bounded: an install that hangs on a network black hole looks identical to
	# one that is merely slow, and there is a working fallback right below.
	tag=$(curl -sfL --connect-timeout 10 --max-time 30 \
		"https://api.github.com/repos/$REPO_SLUG/releases/latest" 2>/dev/null |
		sed -n 's/.*"tag_name": *"\([^"]*\)".*/\1/p' | head -1)
	if [[ -z $tag ]] && command -v gh >/dev/null; then
		tag=$(gh release view --repo "$REPO_SLUG" --json tagName -q .tagName 2>/dev/null || true)
	fi
	[[ -n $tag ]] || die "could not resolve the latest release (private repo? use --version vX.Y.Z with gh installed)"
	printf '%s' "$tag"
}

fetch_manifest() { # $1=version $2=dest
	local url="https://github.com/$REPO_SLUG/releases/download/$1/kubeneuron-install-$1.yaml"
	if curl -sfL --connect-timeout 10 --max-time 120 "$url" -o "$2" 2>/dev/null &&
		grep -q "kind: CustomResourceDefinition" "$2"; then
		return 0
	fi
	command -v gh >/dev/null || die "release download needs public releases or the gh CLI"
	gh release download "$1" --repo "$REPO_SLUG" -p "kubeneuron-install-$1.yaml" --dir "$(dirname "$2")" --clobber
	mv "$(dirname "$2")/kubeneuron-install-$1.yaml" "$2" 2>/dev/null || true
	[[ -s $2 ]] || die "could not download the $1 install manifest"
}

if ((UNINSTALL)); then
	claim="$NAME-controller-state"
	# The Managed metrics store's claim outlives the installation (the
	# operator never deletes it). Only a claim labelled as this
	# installation's store is ever touched; an External store is not ours.
	metrics_claim="$NAME-metrics-store"
	metrics_uid=""
	metrics_labels=$(kubectl -n "$NS" get pvc "$metrics_claim" --ignore-not-found -o jsonpath='{.metadata.uid}{"/"}{.metadata.labels.app\.kubernetes\.io/instance}{"/"}{.metadata.labels.app\.kubernetes\.io/component}{"/"}{.metadata.labels.app\.kubernetes\.io/managed-by}')
	if [[ $metrics_labels == */"$NAME"/metrics-store/kubeneuron-operator ]]; then
		metrics_uid=${metrics_labels%%/*}
	fi
	root_uid=$(kubectl get kubeneuron "$NAME" --ignore-not-found -o jsonpath='{.metadata.uid}')
	if [[ -n $root_uid ]]; then
		actual_ns=$(kubectl get kubeneuron "$NAME" -o jsonpath='{.spec.namespace}')
		[[ $actual_ns == "$NS" ]] || die "installation is in namespace $actual_ns; pass --namespace $actual_ns"
		[[ -z $(kubectl get kubeneuron "$NAME" -o jsonpath='{.metadata.deletionTimestamp}') ]] || die "root is already being deleted; retention cannot be guaranteed"
	fi
	claim_uid=$(kubectl -n "$NS" get pvc "$claim" --ignore-not-found -o jsonpath='{.metadata.uid}')
	if [[ -n $claim_uid ]]; then
		[[ -z $(kubectl -n "$NS" get pvc "$claim" -o jsonpath='{.metadata.deletionTimestamp}') ]] || die "PVC is already being deleted; retention cannot be guaranteed"
		owners=$(kubectl -n "$NS" get pvc "$claim" -o jsonpath='{.metadata.ownerReferences[*].uid}')
		retained_name=$(kubectl -n "$NS" get pvc "$claim" -o go-template='{{index .metadata.annotations "kubeneuron.io/retain-state-for"}}')
		[[ ( -n $root_uid && $owners == "$root_uid" ) || ( -z $owners && $retained_name == "$NAME" ) ]] || die "PVC $NS/$claim is not owned by or retained for this installation"
	fi
	if ((DELETE_DATA)); then
		say "PERMANENT DATA REMOVAL: SQLite PVC $NS/$claim, including incidents and audit history. External PostgreSQL is not deleted."
		[[ -z $metrics_uid ]] || say "PERMANENT DATA REMOVAL: metrics store PVC $NS/$metrics_claim. An External metrics store is not deleted."
		if ((ASSUME_YES == 0)); then
			[[ -t 0 ]] || die "use --yes to confirm --delete-data in non-interactive execution"
			read -r -p "Type '$NAME' to confirm: " confirmation
			[[ $confirmation == "$NAME" ]] || die "data deletion cancelled"
		fi
	elif [[ -n $root_uid && -n $claim_uid ]]; then
		# The operator observes this UID-bound request, removes its owner, and
		# keeps the marker. A stale reconcile therefore cannot reattach the PVC.
		kubectl -n "$NS" annotate pvc "$claim" \
			"kubeneuron.io/retain-state-for=$NAME" "kubeneuron.io/retain-state-uid=$root_uid" --overwrite >/dev/null
		deadline=$((SECONDS + 120))
		while ((SECONDS < deadline)); do
			owners=$(kubectl -n "$NS" get pvc "$claim" -o jsonpath='{.metadata.ownerReferences[*].uid}')
			[[ -z $owners ]] && break
			sleep 2
		done
		[[ -z $owners ]] || die "operator did not acknowledge state retention; installation NOT removed. Upgrade the operator or inspect its logs."
	fi
	say "removing KubeNeuron installation '$NAME' (namespace $NS)"
	kubectl delete kubeneuron "$NAME" --ignore-not-found --wait=true --timeout=120s
	# Include legacy objects without the new installation label, but never
	# delete one unless its actual reference names this installation.
	for resource in gpuplaybooks gpuremediationpolicies; do
		objects=$(
			for selector in "kubeneuron.io/installed-by=install-sh,kubeneuron.io/installation=$NAME" "kubeneuron.io/installed-by=install-sh,!kubeneuron.io/installation"; do
				kubectl get "$resource" -l "$selector" \
					-o go-template='{{range .items}}{{.metadata.name}}{{"\t"}}{{.spec.kubeNeuronRef}}{{"\n"}}{{end}}'
			done
		)
		while IFS=$'\t' read -r object reference; do
			[[ -n $object && $reference == "$NAME" ]] || continue
			kubectl delete "$resource" "$object" --ignore-not-found
		done <<<"$objects"
	done
	kubectl -n "$NS" delete secret \
		"$NAME-operator-api-token" "$NAME-webhook-token" "$NAME-panel-users" \
		"$NAME-controller-tls" "$NAME-controller-server-ca" \
		"$NAME-agent-tls" "$NAME-agent-client-ca" --ignore-not-found
	if ((DELETE_DATA)); then
		kubectl -n "$NS" delete pvc "$claim" --ignore-not-found --wait=true --timeout=120s
		say "deleted SQLite PVC $NS/$claim; physical volume disposal follows its reclaim policy"
		if [[ -n $metrics_uid ]]; then
			# Only the claim checked above: one recreated since is left alone.
			if [[ $(kubectl -n "$NS" get pvc "$metrics_claim" --ignore-not-found -o jsonpath='{.metadata.uid}') == "$metrics_uid" ]]; then
				kubectl -n "$NS" delete pvc "$metrics_claim" --ignore-not-found --wait=true --timeout=120s
				say "deleted metrics store PVC $NS/$metrics_claim"
			fi
		fi
	else
		say "retained SQLite PVC $NS/$claim and its data (if present); reinstall with the same name, namespace and storage settings"
		[[ -z $metrics_uid ]] || say "retained metrics store PVC $NS/$metrics_claim: up to 30 days of metrics, including node names. Erase them with: kubectl delete pvc $metrics_claim -n $NS"
	fi
	say "left in place: the operator, CRDs, namespace '$NS', and any external database"
	say "remove the operator too with: kubectl delete -f <install manifest>"
	exit 0
fi

work=$(mktemp -d); trap 'rm -rf "$work"' EXIT

say "preflight"
server_minor=$(kubectl version -o json 2>/dev/null | sed -n 's/.*"minor": *"\([0-9]*\).*/\1/p' | head -1)
[[ -n $server_minor && $server_minor -ge 29 ]] || die "Kubernetes 1.29+ required (server minor: ${server_minor:-unknown})"
if ! kubectl get storageclass -o json | grep -q '"storageclass.kubernetes.io/is-default-class": *"true"'; then
	echo "  WARNING: no default StorageClass — the SQLite and metrics store PVCs will stay Pending until one exists" >&2
fi

# The Deployment is shared by installations. Preserve its permission across
# manifest upgrades, including upgrades requested by another installation.
operator_args() {
 kubectl -n kube-neuron get deployment kubeneuron-operator --ignore-not-found \
   -o 'go-template={{range .spec.template.spec.containers}}{{if eq .name "operator"}}{{range .args}}{{println .}}{{end}}{{end}}{{end}}'
}
operator_permission() {
 local arg value=false
 while IFS= read -r arg; do
   case "$arg" in
     --allow-node-level-actions|--allow-node-level-actions=true) value=true ;;
     --allow-node-level-actions=false) value=false ;;
   esac
 done
 echo "$value"
}
previous_args=$(operator_args)
previous_permission=$(operator_permission <<<"$previous_args")
say "installing CRDs and the operator"
if [[ -n $MANIFEST ]]; then
	kubectl apply -f "$MANIFEST" >/dev/null
elif [[ -n $VERSION ]]; then
	[[ $VERSION == latest ]] && VERSION=$(resolve_latest)
	say "using release $VERSION"
	fetch_manifest "$VERSION" "$work/install.yaml"
	kubectl apply -f "$work/install.yaml" >/dev/null
elif [[ -n $repo_root ]]; then
	kubectl apply -k "$repo_root/config/crd" >/dev/null
	kubectl apply -f "$repo_root/config/default/namespace.yaml" >/dev/null
	kubectl apply -k "$repo_root/config/rbac" >/dev/null
	kubectl apply -f "$repo_root/config/default/operator_deployment.yaml" >/dev/null
else
	die "not in a repo checkout — pass --version vX.Y.Z (or latest), or --manifest FILE"
fi
kubectl get namespace "$NS" >/dev/null 2>&1 || kubectl create namespace "$NS" >/dev/null
for crd in kubeneurons gpuplaybooks gpuremediationpolicies; do
	kubectl wait --for=condition=Established "crd/$crd.kubeneuron.io" --timeout=60s >/dev/null
done

# A re-run changes nothing an installation already has: its tokens (clients
# and the Alertmanager or vmalert holding them keep working), its panel
# users and its KubeNeuron, of which only the images and an explicit
# --observability are updated.
existing_cr=$(kubectl get kubeneuron "$NAME" --ignore-not-found -o name)
if [[ -z $existing_cr && $REQUESTED_PACK == bare-metal-alpha ]]; then
 say "--policy-pack bare-metal-alpha enables node-level actions on the operator and on this installation (Alpha, not validated on real hardware). These actions can reboot hosts and reset GPUs."
 previous_permission=true
fi
current_args=$(operator_args)
if [[ $previous_permission == true && $(operator_permission <<<"$current_args") != true ]]; then
 operator_index=$(kubectl -n kube-neuron get deployment kubeneuron-operator \
   -o 'go-template={{range $i,$c := .spec.template.spec.containers}}{{if eq $c.name "operator"}}{{$i}}{{end}}{{end}}')
 [[ $operator_index =~ ^[0-9]+$ ]] || die "operator container missing from deployment"
 path="/spec/template/spec/containers/$operator_index"
 kubectl -n kube-neuron patch deployment kubeneuron-operator --type=json \
   -p "[{\"op\":\"test\",\"path\":\"$path/name\",\"value\":\"operator\"},{\"op\":\"add\",\"path\":\"$path/args/-\",\"value\":\"--allow-node-level-actions=true\"}]" >/dev/null
fi
kubectl -n kube-neuron rollout status deployment/kubeneuron-operator --timeout=180s >/dev/null
say "generating operational secrets"
api_token=$(kubectl -n "$NS" get secret "$NAME-operator-api-token" --ignore-not-found -o jsonpath='{.data.token}' | base64 --decode)
api_token_kept=1
if [[ -z $api_token ]]; then
	api_token_kept=0
	api_token=$(openssl rand -hex 32)
	kubectl -n "$NS" create secret generic "$NAME-operator-api-token" \
		--from-literal=token="$api_token" --dry-run=client -o yaml | kubectl apply -f - >/dev/null
fi
if [[ -z $(kubectl -n "$NS" get secret "$NAME-webhook-token" --ignore-not-found -o name) ]]; then
	kubectl -n "$NS" create secret generic "$NAME-webhook-token" \
		--from-literal=token="$(openssl rand -hex 32)" --dry-run=client -o yaml | kubectl apply -f - >/dev/null
fi

admin_password=""
auth_block=""
panel_users_kept=0
if [[ -n $(kubectl -n "$NS" get secret "$NAME-panel-users" --ignore-not-found -o name) ]]; then
	panel_users_kept=1
	auth_block=$'  auth:\n    users:\n      name: '"$NAME-panel-users"
elif [[ -n $ADMIN_USER && -z $existing_cr ]]; then
	hash=""
	admin_password=$(openssl rand -base64 15)
	if command -v htpasswd >/dev/null; then
		hash=$(htpasswd -bnBC 12 "" "$admin_password" | tr -d ':\n')
	elif command -v kubeneuronctl >/dev/null; then
		hash=$(printf '%s' "$admin_password" | kubeneuronctl passwd)
	elif [[ -n $repo_root && -x $repo_root/bin/kubeneuronctl ]]; then
		hash=$(printf '%s' "$admin_password" | "$repo_root/bin/kubeneuronctl" passwd)
	fi
	if [[ -n $hash ]]; then
		kubectl -n "$NS" create secret generic "$NAME-panel-users" \
			--from-literal="$ADMIN_USER=$hash" --dry-run=client -o yaml | kubectl apply -f - >/dev/null
		auth_block=$'  auth:\n    users:\n      name: '"$NAME-panel-users"
	else
		admin_password=""
		echo "  NOTE: neither htpasswd nor kubeneuronctl found — panel password sign-in skipped" >&2
		echo "        (add it later: kubeneuronctl passwd + a $NAME-panel-users Secret + spec.auth.users)" >&2
	fi
fi

say "applying the starter configuration and root object"
image_repo=ghcr.io/kubeneuron/kubeneuron
# The release pipeline rewrites these two lines with the digests it published,
# so a downloaded installer deploys exactly the images the release names. Left
# as-is when this script runs from a source checkout, where there is no release
# to pin to. A digest here is what makes the install actually immutable: the
# operator manifest was already digest-pinned, but the controller and the agent
# — the two components that run the fleet — were resolved as MOVEABLE TAGS, so
# the supply-chain guarantee stopped exactly where it started to matter.
# RELEASE_STAMPED is set to the tag by the release pipeline at the same moment
# it writes the two digests below. Its presence is what tells this script it is
# a RELEASE artifact: without it, a stamping step that half-succeeded would be
# indistinguishable from a plain source checkout, and both digests would fall
# silently back to moveable tags — the precise failure this stamping exists to
# prevent, reappearing quietly.
RELEASE_STAMPED=${RELEASE_STAMPED:-}
CONTROLLER_DIGEST=${CONTROLLER_DIGEST:-}
AGENT_DIGEST=${AGENT_DIGEST:-}
if [[ -n $RELEASE_STAMPED ]]; then
	[[ $CONTROLLER_DIGEST == sha256:* && $AGENT_DIGEST == sha256:* ]] ||
		die "this installer was published for $RELEASE_STAMPED but carries no image digests; refusing to fall back to moveable tags"
fi
if [[ -z $VERSION && -n $repo_root && -f $repo_root/config/samples/kubeneuron_v1alpha1_kubeneuron.yaml ]]; then
	controller_image=$(sed -n 's/.*image: *\(.*controller.*\)/\1/p' "$repo_root/config/samples/kubeneuron_v1alpha1_kubeneuron.yaml" | head -1)
	agent_image=$(sed -n 's/.*image: *\(.*\/agent.*\)/\1/p' "$repo_root/config/samples/kubeneuron_v1alpha1_kubeneuron.yaml" | head -1)
elif [[ $CONTROLLER_DIGEST == sha256:* && $AGENT_DIGEST == sha256:* ]]; then
	controller_image=$image_repo/controller@$CONTROLLER_DIGEST
	agent_image=$image_repo/agent@$AGENT_DIGEST
else
	controller_image=$image_repo/controller:$VERSION
	agent_image=$image_repo/agent:$VERSION
fi
[[ -n $CONTROLLER_IMAGE ]] && controller_image=$CONTROLLER_IMAGE
[[ -n $AGENT_IMAGE ]] && agent_image=$AGENT_IMAGE
[[ -n $controller_image && -n $agent_image ]] || die "could not resolve component images"

# The agent has a single set of host paths for its entire DaemonSet. Only
# auto-select a layout when every GPU-labelled node matches that profile.
gpu_selector='nvidia.com/gpu.present=true'
if [[ $PROFILE == auto ]]; then
    PROFILE=none
    if gpu_nodes=$(kubectl get nodes -l "$gpu_selector" -o 'go-template={{range .items}}{{.status.nodeInfo.architecture}}|{{.status.nodeInfo.osImage}}|{{index .metadata.labels "nvidia.com/gpu.deploy.driver"}}{{"\n"}}{{end}}') && [[ -n $gpu_nodes ]]; then
        if ! grep -Evq '^amd64\|Amazon Linux 2023[^|]*\|(false|<no value>)$' <<<"$gpu_nodes"; then
            PROFILE=host-al2023
        elif ! grep -Evq '^amd64\|Ubuntu [^|]*\|true$' <<<"$gpu_nodes"; then
            PROFILE=gpu-operator-ubuntu
        fi
    fi
fi
agent_profile_yaml=''
if [[ $PROFILE != none ]]; then
    driver_bin=/usr/bin
    driver_libs=/usr/lib64
    if [[ $PROFILE == gpu-operator-ubuntu ]]; then
        driver_bin=/run/nvidia/driver/usr/bin
        driver_libs='/usr/lib/x86_64-linux-gnu, /run/nvidia/driver/usr/lib/x86_64-linux-gnu'
    fi
    agent_profile_yaml="    nodeSelector:
      nvidia.com/gpu.present: \"true\"
      kubernetes.io/arch: amd64
    hostTooling:
      binDir: $driver_bin
      libDirs: [$driver_libs]"
fi
say "NVIDIA host tooling profile: $PROFILE (see docs/quickstart.md)"
# Render the standalone installer copy of config/cloud-policies for this installation.
render_cloud_pack() {
  cat <<EOF
apiVersion: kubeneuron.io/v1alpha1
kind: GPUPlaybook
metadata:
  name: $NAME-observe
  labels: {kubeneuron.io/installed-by: install-sh, kubeneuron.io/installation: $NAME, kubeneuron.io/policy-pack: cloud, kubeneuron.io/policy-pack-version: "1"}
spec:
  kubeNeuronRef: $NAME
  target: GPU
  steps:
    - name: observe
      action: Observe
---
apiVersion: kubeneuron.io/v1alpha1
kind: GPUPlaybook
metadata:
  name: $NAME-cloud-replace
  labels: {kubeneuron.io/installed-by: install-sh, kubeneuron.io/installation: $NAME, kubeneuron.io/policy-pack: cloud, kubeneuron.io/policy-pack-version: "1"}
spec:
  kubeNeuronRef: $NAME
  target: Node
  cooldown: 6h
  steps:
    - name: cordon
      action: Cordon
    - name: drain
      action: Drain
      approval: Required
      timeout: 30m
    - name: replace
      action: ReplaceNode
      approval: Required
      timeout: 20m
---
apiVersion: kubeneuron.io/v1alpha1
kind: GPUPlaybook
metadata:
  name: $NAME-cloud-recycle
  labels: {kubeneuron.io/installed-by: install-sh, kubeneuron.io/installation: $NAME, kubeneuron.io/policy-pack: cloud, kubeneuron.io/policy-pack-version: "1"}
spec:
  kubeNeuronRef: $NAME
  target: Node
  cooldown: 6h
  steps:
    - name: cordon
      action: Cordon
    - name: drain
      action: Drain
      approval: Required
      timeout: 30m
    - name: recycle
      action: RecycleNode
      approval: Required
      timeout: 20m
  onFailure:
    playbookRef: $NAME-cloud-replace
EOF
  local class ladder
  for class in xid-app ecc-sbe-rate thermal power pcie agent-down exporter-down ecc-contained row-remap-ok gsp-error driver-hang row-remap-failure row-remap-budget ecc-dbe nvlink fell-off-bus gpu-lost; do
    case $class in
      ecc-dbe|nvlink|fell-off-bus|gpu-lost) ladder=$NAME-cloud-replace ;;
      *) ladder=$NAME-observe ;;
    esac
    cat <<EOF
---
apiVersion: kubeneuron.io/v1alpha1
kind: GPURemediationPolicy
metadata:
  name: $NAME-cloud-$class
  labels: {kubeneuron.io/installed-by: install-sh, kubeneuron.io/installation: $NAME, kubeneuron.io/policy-pack: cloud, kubeneuron.io/policy-pack-version: "1"}
spec:
  kubeNeuronRef: $NAME
  priority: 900
  mode: Approval
  match: {class: $class}
  playbookRef: $ladder
  replacement: {strategy: AddFirst}
  recycle: {enabled: false}
EOF
  done
}

# Compare only pack-owned fields. kubectl serializes spec maps in key order.
report_cloud_pack_changes() {
  local dir="$work/cloud-pack-review" file ref expected observed version
  mkdir -p "$dir"
  render_cloud_pack | awk -v dir="$dir" '
    BEGIN { n=1 }
    /^---$/ { close(path); n++; next }
    { path=dir "/" n ".yaml"; print >path }
  '
  for file in "$dir"/*.yaml; do
    ref=$(kubectl create --dry-run=client -f "$file" -o name)
    observed=$(kubectl get "$ref" --ignore-not-found -o jsonpath='{.spec}')
    if [[ -z $observed ]]; then
      say "Missing $ref (pack version 1); preserving deletion."
      continue
    fi
    expected=$(kubectl create --dry-run=client -f "$file" -o jsonpath='{.spec}')
    version=$(kubectl get "$ref" -o jsonpath='{.metadata.labels.kubeneuron\.io/policy-pack-version}')
    if [[ $expected != "$observed" || $version != 1 ]]; then
      say "Preserving changed $ref (installed label: ${version:-missing}; bundled version: 1)."
    fi
  done
}

# Recover from a partial create without taking ownership of existing resources.
create_pack() {
  local manifest=$1 pack=$2 ref installed_by installation label file
  local dir="$work/pack-parts" missing="$work/missing.yaml"
  mkdir -p "$dir"
  kubectl get gpuplaybooks,gpuremediationpolicies -o go-template='{{range .items}}{{if eq .spec.kubeNeuronRef "'"$NAME"'"}}{{.kind}}/{{.metadata.name}} {{with .metadata.labels}}{{index . "kubeneuron.io/installed-by"}} {{index . "kubeneuron.io/installation"}} {{index . "kubeneuron.io/policy-pack"}}{{else}}- - -{{end}}{{"\n"}}{{end}}{{end}}' > "$dir/ownership"
  while read -r ref installed_by installation label; do
    [[ $installed_by == install-sh && $installation == "$NAME" && $label == "$pack" ]] ||
      die "Conflicting recovery object $ref; check installation ownership and pack."
  done < "$dir/ownership"
  if [[ -s $dir/ownership ]]; then
    say "Resuming interrupted installation of $pack for $NAME; preserving existing recovery objects."
    awk -v dir="$dir" '/^---$/ {close(path); n++; next} {path=dir "/" (n+1) ".yaml"; print >path}' "$manifest"
    : > "$missing"
    for file in "$dir"/*.yaml; do
      ref=$(kubectl create --dry-run=client -f "$file" -o name)
      if [[ -n $(kubectl get "$ref" --ignore-not-found -o name) ]]; then
        # A name collision can also refer to a different root, outside the list above.
        local owner
        owner=$(kubectl get "$ref" -o jsonpath='{.spec.kubeNeuronRef}')
        [[ $owner == "$NAME" ]] || die "Conflicting recovery object $ref belongs to $owner."
        continue
      fi
      printf '%s\n' '---' >> "$missing"
      cat "$file" >> "$missing"
    done
    [[ -s $missing ]] || return 0
    manifest=$missing
  fi
  kubectl create --dry-run=server -f "$manifest" >/dev/null
  kubectl create -f "$manifest" >/dev/null
}

# The baseline policy pack: one binding for every problem class the shipped
# detectors can emit.
#
# This used to be a single observe-only binding for xid-app, and that was not
# merely thin coverage. A class with no policy bound to it opens an incident,
# observes, and quiet-resolves without remediating — and nothing about that
# reads as a gap: it does not error, it does not alert, and until the recovery
# report learned to count it separately it was reported as capacity KubeNeuron
# returned to service, unattended, because an incident nothing acted on never
# asks anybody's permission. A pilot installing this script got a product that
# detected faults and measured recoveries it had not performed.
#
# It is a COPY of config/policies/, and the duplication cannot be removed: this
# script is advertised as `curl … | bash`, which has no checkout to read the
# pack out of, and the release manifest is `kustomize build config/default` —
# CRDs and the operator, no policies. internal/operator/policy_pack_test.go
# pins the two together on the thing that must never differ: which class gets
# which ladder.
#
# NOTHING HERE IS ARMED. Every step that ends running work — drain, targeted
# eviction, device reset, reboot — carries approval: Required, so the ladders
# cordon, collect evidence, verify and close on their own and stop for a human
# before they cost anybody a job. That holds after executionMode is switched
# away from DryRun, which is the point: dry-run is a phase, the approval gates
# are the design. See config/policies/playbooks.yaml for the reasoning per
# ladder and config/policies/policies.yaml for the reasoning per class.
root=$(kubectl get kubeneuron "$NAME" --ignore-not-found -o name)
node_level_actions=false
pack_label=""
if [[ -n $root ]]; then
  selected=$(kubectl get "$root" -o jsonpath='{.metadata.labels.kubeneuron\.io/policy-pack}')
  if [[ $selected == cloud ]]; then
    pack_label="  labels: {kubeneuron.io/policy-pack: cloud}"
    report_cloud_pack_changes
  fi
  # Existing installations may have edited or intentionally removed any binding.
  # A normal installer rerun must preserve those choices, including an empty pack.
  [[ -z $REQUESTED_PACK ]] || say "Existing installation: preserving its policy objects; --policy-pack selects policies on first install."
  say "Preserving recovery policies for existing installation $NAME."
else
  # A root may be absent after an interrupted install or a manual uninstall.
  # Check spec.kubeNeuronRef even for objects without installer labels.
  if [[ ${REQUESTED_PACK:-cloud} == cloud ]]; then
    pack_label="  labels: {kubeneuron.io/policy-pack: cloud}"
    render_cloud_pack > "$work/cloud-pack.yaml"
    create_pack "$work/cloud-pack.yaml" cloud
    say "Cloud recovery supports observation and dry-run. Live replacement requires a configured provider with add-first support."
  else
    node_level_actions=true
    pack_label="  labels: {kubeneuron.io/policy-pack: bare-metal-alpha}"
    say "Node-level recovery: Alpha, not validated on real hardware."
    cat > "$work/bare-metal-pack.yaml" <<EOF
apiVersion: kubeneuron.io/v1alpha1
kind: GPUPlaybook
metadata:
  name: $NAME-observe
  labels: {kubeneuron.io/installed-by: install-sh, kubeneuron.io/installation: $NAME, kubeneuron.io/policy-pack: baseline}
spec:
  kubeNeuronRef: $NAME
  target: GPU
  cooldown: 30m
  steps:
    - name: record
      action: Observe
---
apiVersion: kubeneuron.io/v1alpha1
kind: GPUPlaybook
metadata:
  name: $NAME-workload-restart
  labels: {kubeneuron.io/installed-by: install-sh, kubeneuron.io/installation: $NAME, kubeneuron.io/policy-pack: baseline}
spec:
  kubeNeuronRef: $NAME
  target: GPU
  cooldown: 15m
  steps:
    - name: evict-workload
      action: EvictGPUWorkload
      approval: Required
      timeout: 10m
    - name: verify
      action: VerifyGPUHealth
      params:
        quiet_window: 10m
  onFailure:
    playbookRef: $NAME-drain-and-reset
---
apiVersion: kubeneuron.io/v1alpha1
kind: GPUPlaybook
metadata:
  name: $NAME-reset-when-idle
  labels: {kubeneuron.io/installed-by: install-sh, kubeneuron.io/installation: $NAME, kubeneuron.io/policy-pack: baseline}
spec:
  kubeNeuronRef: $NAME
  target: GPU
  cooldown: 6h
  steps:
    - name: wait-for-idle
      action: WaitIdle
      timeout: 12h
    - name: reset
      action: GPUReset
      approval: Required
      timeout: 5m
    - name: verify
      action: VerifyGPUHealth
      params:
        diag_level: "1"
        quiet_window: 10m
  onFailure:
    playbookRef: $NAME-drain-and-reset
---
apiVersion: kubeneuron.io/v1alpha1
kind: GPUPlaybook
metadata:
  name: $NAME-gpu-reset
  labels: {kubeneuron.io/installed-by: install-sh, kubeneuron.io/installation: $NAME, kubeneuron.io/policy-pack: baseline}
spec:
  kubeNeuronRef: $NAME
  target: GPU
  cooldown: 30m
  steps:
    - name: idle-check
      action: IdleCheck
      timeout: 2m
    - name: reset
      action: GPUReset
      approval: Required
      timeout: 5m
    - name: verify
      action: VerifyGPUHealth
      params:
        diag_level: "1"
        quiet_window: 10m
  onFailure:
    playbookRef: $NAME-drain-and-reset
---
apiVersion: kubeneuron.io/v1alpha1
kind: GPUPlaybook
metadata:
  name: $NAME-drain-and-reset
  labels: {kubeneuron.io/installed-by: install-sh, kubeneuron.io/installation: $NAME, kubeneuron.io/policy-pack: baseline}
spec:
  kubeNeuronRef: $NAME
  target: GPU
  effects: [nodeScheduling]
  cooldown: 1h
  steps:
    - name: cordon
      action: Cordon
    - name: drain
      action: Drain
      approval: Required
      timeout: 30m
    - name: reset
      action: GPUReset
      timeout: 5m
    - name: verify
      action: VerifyGPUHealth
      params:
        diag_level: "1"
        quiet_window: 10m
    - name: uncordon
      action: Uncordon
  onFailure:
    playbookRef: $NAME-reboot
---
apiVersion: kubeneuron.io/v1alpha1
kind: GPUPlaybook
metadata:
  name: $NAME-reboot
  labels: {kubeneuron.io/installed-by: install-sh, kubeneuron.io/installation: $NAME, kubeneuron.io/policy-pack: baseline}
spec:
  kubeNeuronRef: $NAME
  target: Node
  cooldown: 6h
  steps:
    - name: cordon
      action: Cordon
    - name: drain
      action: Drain
      approval: Required
      timeout: 30m
    - name: collect-bundle
      action: CollectBundle
      timeout: 15m
    - name: reboot
      action: Reboot
      approval: Required
      timeout: 20m
    - name: verify
      action: VerifyNodeHealth
      params:
        diag_level: "2"
        quiet_window: 15m
    - name: uncordon
      action: Uncordon
  onFailure:
    playbookRef: $NAME-rma
---
apiVersion: kubeneuron.io/v1alpha1
kind: GPUPlaybook
metadata:
  name: $NAME-rma
  labels: {kubeneuron.io/installed-by: install-sh, kubeneuron.io/installation: $NAME, kubeneuron.io/policy-pack: baseline}
spec:
  kubeNeuronRef: $NAME
  target: Node
  cooldown: 24h
  steps:
    - name: cordon
      action: Cordon
    - name: drain
      action: Drain
      approval: Required
      timeout: 60m
    - name: collect-bundle
      action: CollectBundle
      timeout: 30m
      params:
        diag_level: "3"
    - name: open-ticket
      action: OpenTicket
      approval: Required
      params:
        quarantine: "true"
---
apiVersion: kubeneuron.io/v1alpha1
kind: GPURemediationPolicy
metadata:
  name: $NAME-xid-app
  labels: {kubeneuron.io/installed-by: install-sh, kubeneuron.io/installation: $NAME, kubeneuron.io/policy-pack: baseline}
spec:
  kubeNeuronRef: $NAME
  priority: 900
  match: {class: xid-app}
  playbookRef: $NAME-observe
  parameters:
    threshold: "3"
    window: "1h"
---
apiVersion: kubeneuron.io/v1alpha1
kind: GPURemediationPolicy
metadata:
  name: $NAME-ecc-sbe-rate
  labels: {kubeneuron.io/installed-by: install-sh, kubeneuron.io/installation: $NAME, kubeneuron.io/policy-pack: baseline}
spec:
  kubeNeuronRef: $NAME
  priority: 900
  match: {class: ecc-sbe-rate}
  playbookRef: $NAME-observe
  parameters:
    threshold: "3"
    window: "24h"
---
apiVersion: kubeneuron.io/v1alpha1
kind: GPURemediationPolicy
metadata:
  name: $NAME-thermal
  labels: {kubeneuron.io/installed-by: install-sh, kubeneuron.io/installation: $NAME, kubeneuron.io/policy-pack: baseline}
spec:
  kubeNeuronRef: $NAME
  priority: 900
  match: {class: thermal}
  playbookRef: $NAME-observe
---
apiVersion: kubeneuron.io/v1alpha1
kind: GPURemediationPolicy
metadata:
  name: $NAME-power
  labels: {kubeneuron.io/installed-by: install-sh, kubeneuron.io/installation: $NAME, kubeneuron.io/policy-pack: baseline}
spec:
  kubeNeuronRef: $NAME
  priority: 900
  match: {class: power}
  playbookRef: $NAME-observe
---
apiVersion: kubeneuron.io/v1alpha1
kind: GPURemediationPolicy
metadata:
  name: $NAME-pcie
  labels: {kubeneuron.io/installed-by: install-sh, kubeneuron.io/installation: $NAME, kubeneuron.io/policy-pack: baseline}
spec:
  kubeNeuronRef: $NAME
  priority: 900
  match: {class: pcie}
  playbookRef: $NAME-observe
---
apiVersion: kubeneuron.io/v1alpha1
kind: GPURemediationPolicy
metadata:
  name: $NAME-agent-down
  labels: {kubeneuron.io/installed-by: install-sh, kubeneuron.io/installation: $NAME, kubeneuron.io/policy-pack: baseline}
spec:
  kubeNeuronRef: $NAME
  priority: 900
  match: {class: agent-down}
  playbookRef: $NAME-observe
---
apiVersion: kubeneuron.io/v1alpha1
kind: GPURemediationPolicy
metadata:
  name: $NAME-exporter-down
  labels: {kubeneuron.io/installed-by: install-sh, kubeneuron.io/installation: $NAME, kubeneuron.io/policy-pack: baseline}
spec:
  kubeNeuronRef: $NAME
  priority: 900
  match: {class: exporter-down}
  playbookRef: $NAME-observe
---
apiVersion: kubeneuron.io/v1alpha1
kind: GPURemediationPolicy
metadata:
  name: $NAME-ecc-contained
  labels: {kubeneuron.io/installed-by: install-sh, kubeneuron.io/installation: $NAME, kubeneuron.io/policy-pack: baseline}
spec:
  kubeNeuronRef: $NAME
  priority: 900
  match: {class: ecc-contained}
  playbookRef: $NAME-workload-restart
---
apiVersion: kubeneuron.io/v1alpha1
kind: GPURemediationPolicy
metadata:
  name: $NAME-row-remap-ok
  labels: {kubeneuron.io/installed-by: install-sh, kubeneuron.io/installation: $NAME, kubeneuron.io/policy-pack: baseline}
spec:
  kubeNeuronRef: $NAME
  priority: 900
  match: {class: row-remap-ok}
  playbookRef: $NAME-reset-when-idle
---
apiVersion: kubeneuron.io/v1alpha1
kind: GPURemediationPolicy
metadata:
  name: $NAME-gsp-error
  labels: {kubeneuron.io/installed-by: install-sh, kubeneuron.io/installation: $NAME, kubeneuron.io/policy-pack: baseline}
spec:
  kubeNeuronRef: $NAME
  priority: 900
  match: {class: gsp-error}
  playbookRef: $NAME-gpu-reset
---
apiVersion: kubeneuron.io/v1alpha1
kind: GPURemediationPolicy
metadata:
  name: $NAME-driver-hang
  labels: {kubeneuron.io/installed-by: install-sh, kubeneuron.io/installation: $NAME, kubeneuron.io/policy-pack: baseline}
spec:
  kubeNeuronRef: $NAME
  priority: 900
  match: {class: driver-hang}
  playbookRef: $NAME-gpu-reset
---
apiVersion: kubeneuron.io/v1alpha1
kind: GPURemediationPolicy
metadata:
  name: $NAME-ecc-dbe
  labels: {kubeneuron.io/installed-by: install-sh, kubeneuron.io/installation: $NAME, kubeneuron.io/policy-pack: baseline}
spec:
  kubeNeuronRef: $NAME
  priority: 900
  match: {class: ecc-dbe}
  playbookRef: $NAME-drain-and-reset
---
apiVersion: kubeneuron.io/v1alpha1
kind: GPURemediationPolicy
metadata:
  name: $NAME-nvlink
  labels: {kubeneuron.io/installed-by: install-sh, kubeneuron.io/installation: $NAME, kubeneuron.io/policy-pack: baseline}
spec:
  kubeNeuronRef: $NAME
  priority: 900
  match: {class: nvlink}
  playbookRef: $NAME-drain-and-reset
---
apiVersion: kubeneuron.io/v1alpha1
kind: GPURemediationPolicy
metadata:
  name: $NAME-fell-off-bus
  labels: {kubeneuron.io/installed-by: install-sh, kubeneuron.io/installation: $NAME, kubeneuron.io/policy-pack: baseline}
spec:
  kubeNeuronRef: $NAME
  priority: 900
  match: {class: fell-off-bus}
  playbookRef: $NAME-reboot
---
apiVersion: kubeneuron.io/v1alpha1
kind: GPURemediationPolicy
metadata:
  name: $NAME-gpu-lost
  labels: {kubeneuron.io/installed-by: install-sh, kubeneuron.io/installation: $NAME, kubeneuron.io/policy-pack: baseline}
spec:
  kubeNeuronRef: $NAME
  priority: 900
  match: {class: gpu-lost}
  playbookRef: $NAME-reboot
---
apiVersion: kubeneuron.io/v1alpha1
kind: GPURemediationPolicy
metadata:
  name: $NAME-row-remap-failure
  labels: {kubeneuron.io/installed-by: install-sh, kubeneuron.io/installation: $NAME, kubeneuron.io/policy-pack: baseline}
spec:
  kubeNeuronRef: $NAME
  priority: 900
  match: {class: row-remap-failure}
  playbookRef: $NAME-rma
---
apiVersion: kubeneuron.io/v1alpha1
kind: GPURemediationPolicy
metadata:
  name: $NAME-row-remap-budget
  labels: {kubeneuron.io/installed-by: install-sh, kubeneuron.io/installation: $NAME, kubeneuron.io/policy-pack: baseline}
spec:
  kubeNeuronRef: $NAME
  priority: 900
  match: {class: row-remap-budget}
  playbookRef: $NAME-rma
EOF
    create_pack "$work/bare-metal-pack.yaml" baseline
  fi
fi

# Managed: the operator runs the Collector, the metrics store with its alert
# rules, and Grafana. Detection from the agents and recovery never depend on
# them. Your own Collector or store: set spec.observability by hand
# (docs/observability.md). A re-run without --observability leaves an
# existing installation's block, or its lack of one, as it is: only a new
# installation defaults to Managed.
[[ -n $OBSERVABILITY || -n $existing_cr ]] || OBSERVABILITY=managed
observability_mode=""
case $OBSERVABILITY in
managed) observability_mode=Managed ;;
disabled)
	observability_mode=Disabled
	echo "  WARNING: --observability disabled: no metrics store, no dashboards, and no detection of the faults that reach KubeNeuron only as metric alerts (docs/observability.md)" >&2
	;;
esac
observability_yaml=""
[[ -z $observability_mode ]] || observability_yaml="  observability:
    collector: {mode: $observability_mode}
    store: {mode: $observability_mode}
    grafana: {mode: $observability_mode}"
if [[ $observability_mode == Managed ]]; then
	say "Installing third-party components, unmodified, pulled from their publishers: OpenTelemetry Collector, VictoriaMetrics, vmalert (Apache-2.0) and Grafana (AGPL-3.0). Use --observability disabled to skip them."
fi

if [[ -n $existing_cr ]]; then
	say "updating the images of the existing installation $NAME; the rest of its spec is kept"
	kubectl patch kubeneuron "$NAME" --type merge >/dev/null \
		-p "{\"spec\":{\"controller\":{\"image\":\"$controller_image\"},\"agent\":{\"image\":\"$agent_image\"}}}"
	if [[ -n $observability_mode ]]; then
		# The whole block: a mode change drops the endpoints of the old one.
		kubectl patch kubeneuron "$NAME" --type json >/dev/null \
			-p "[{\"op\":\"add\",\"path\":\"/spec/observability\",\"value\":{\"collector\":{\"mode\":\"$observability_mode\"},\"store\":{\"mode\":\"$observability_mode\"},\"grafana\":{\"mode\":\"$observability_mode\"}}}]"
	fi
else
kubectl apply -f - >/dev/null <<EOF
apiVersion: kubeneuron.io/v1alpha1
kind: KubeNeuron
metadata:
  name: $NAME
$pack_label
spec:
  namespace: $NS
  controller:
    image: $controller_image
  agent:
    image: $agent_image
$agent_profile_yaml
    tolerations:
      - operator: Exists
$auth_block
  safety:
    nodeLevelActions: $node_level_actions
    executionMode: DryRun
  notifications:
    operatorAPIToken:
      name: $NAME-operator-api-token
    webhookToken:
      name: $NAME-webhook-token
  workflowStore:
    type: SQLite
    sqlite:
      size: 5Gi
$observability_yaml
  tls:
    issuer: Operator
    serverSecretRef: {name: $NAME-controller-tls}
    serverCASecretRef: {name: $NAME-controller-server-ca}
    clientSecretRef: {name: $NAME-agent-tls}
    clientCASecretRef: {name: $NAME-agent-client-ca}
EOF
fi

# TLS material is issued by the operator, not here.
#
# It used to be generated with openssl at install time, which meant nothing ever
# renewed it: an installation stopped being able to authenticate roughly a year
# after it was created, silently. The operator now issues a long-lived authority
# and short-lived certificates and replaces those well before they expire.
#
# Supplying your own material still works — cert-manager, a corporate CA,
# anything. Create the four Secrets before this runs and the operator will leave
# them alone, reporting their expiry rather than replacing them.

say "waiting for the installation to become Ready"
deadline=$((SECONDS + 300))
ready=""
while ((SECONDS < deadline)); do
	ready=$(kubectl get kubeneuron "$NAME" -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}' 2>/dev/null || true)
	[[ $ready == True ]] && break
	sleep 5
done
if [[ $ready != True ]]; then
	kubectl get kubeneuron "$NAME" -o jsonpath='{.status.conditions}' || true; echo
	die "installation did not become Ready in 5 minutes (conditions above)"
fi

cat <<EOF

$(printf '\033[1;32m✓ KubeNeuron is Ready.\033[0m')

  Panel / API   kubectl -n $NS port-forward service/$NAME-controller 8080:8080
                → http://127.0.0.1:8080
EOF
if [[ -n $admin_password ]]; then
	cat <<EOF
  Sign in       $ADMIN_USER / $admin_password
                (change it: kubeneuronctl passwd → update the $NAME-panel-users Secret)
EOF
elif ((panel_users_kept)); then
	cat <<EOF
  Sign in       unchanged: the $NAME-panel-users Secret was kept
EOF
fi
if ((api_token_kept)); then
	cat <<EOF
  API token     unchanged: kept in the $NAME-operator-api-token Secret
                (kubectl -n $NS get secret $NAME-operator-api-token -o jsonpath='{.data.token}' | base64 -d)
EOF
else
	cat <<EOF
  API token     $api_token
EOF
fi
cat <<EOF
                (automation/CLI: kubeneuronctl --server http://127.0.0.1:8080 --token ...)
EOF
execution_mode=$(kubectl get kubeneuron "$NAME" -o jsonpath='{.spec.safety.executionMode}')
if [[ $execution_mode == DryRun ]]; then
	echo "  Mode          DryRun — detection, workflows, approvals, audit; no node is touched."
else
	echo "  Mode          $execution_mode (kept from the existing installation)"
fi
if [[ $(kubectl get kubeneuron "$NAME" -o jsonpath='{.spec.observability.grafana.mode}') == Managed ]]; then
	cat <<EOF
  Dashboards    kubectl -n $NS port-forward service/$NAME-grafana 3000:3000
                → http://127.0.0.1:3000 (user admin; password:
                kubectl -n $NS get secret $NAME-grafana-admin -o jsonpath='{.data.admin-password}' | base64 -d)
  Metrics       the store, Grafana and metric-alert detection start in the
                background; their state is in the conditions
                ObservabilityStoreReady and AlertDetectionConfigured:
                kubectl get kubeneuron $NAME -o jsonpath='{.status.conditions}'
EOF
fi
cat <<EOF

  Next steps    bind real playbooks/policies (docs/playbook-authoring.md)
                and follow docs/quickstart.md to check GPU visibility and
                open a dry-run incident.
  TLS           issued and renewed by the operator; to bring your own,
                follow docs/install.md §3 or deploy/cert-manager/.
EOF
