#!/usr/bin/env bash
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
#   --uninstall          remove the installation created by this script
#
# Requirements: kubectl (>=1.29 server), openssl. For --version: gh.
# For the panel password hash: htpasswd, or a kubeneuronctl binary.

NAME=kubeneuron
NS=kube-neuron
VERSION=""
MANIFEST=""
ADMIN_USER="admin"
UNINSTALL=0
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
  --namespace NS         runtime namespace          (default: kube-neuron)
  --version vX.Y.Z|latest  released install manifest (public curl, gh fallback)
  --manifest FILE        pre-downloaded install manifest
  --admin USER           panel username to create   (default: admin; "" skips)
  --controller-image I / --agent-image I   image overrides (air-gapped/dev)
  --uninstall            remove what this script created
USAGE
}

while (($#)); do
	case $1 in
	--name) NAME=$2; shift 2 ;;
	--namespace) NS=$2; shift 2 ;;
	--version) VERSION=$2; shift 2 ;;
	--manifest) MANIFEST=$2; shift 2 ;;
	--admin) ADMIN_USER=$2; shift 2 ;;
	--controller-image) CONTROLLER_IMAGE=$2; shift 2 ;;
	--agent-image) AGENT_IMAGE=$2; shift 2 ;;
	--uninstall) UNINSTALL=1; shift ;;
	-h|--help) usage; exit 0 ;;
	*) echo "unknown flag: $1" >&2; exit 2 ;;
	esac
done

say() { printf '\033[1;34m==>\033[0m %s\n' "$*"; }
die() { printf '\033[1;31mERROR:\033[0m %s\n' "$*" >&2; exit 1; }

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
	tag=$(curl -sfL "https://api.github.com/repos/$REPO_SLUG/releases/latest" 2>/dev/null |
		sed -n 's/.*"tag_name": *"\([^"]*\)".*/\1/p' | head -1)
	if [[ -z $tag ]] && command -v gh >/dev/null; then
		tag=$(gh release view --repo "$REPO_SLUG" --json tagName -q .tagName 2>/dev/null || true)
	fi
	[[ -n $tag ]] || die "could not resolve the latest release (private repo? use --version vX.Y.Z with gh installed)"
	printf '%s' "$tag"
}

fetch_manifest() { # $1=version $2=dest
	local url="https://github.com/$REPO_SLUG/releases/download/$1/kubeneuron-install-$1.yaml"
	if curl -sfL "$url" -o "$2" 2>/dev/null && grep -q "kind: CustomResourceDefinition" "$2"; then
		return 0
	fi
	command -v gh >/dev/null || die "release download needs public releases or the gh CLI"
	gh release download "$1" --repo "$REPO_SLUG" -p "kubeneuron-install-$1.yaml" --dir "$(dirname "$2")" --clobber
	mv "$(dirname "$2")/kubeneuron-install-$1.yaml" "$2" 2>/dev/null || true
	[[ -s $2 ]] || die "could not download the $1 install manifest"
}

if ((UNINSTALL)); then
	say "removing KubeNeuron installation '$NAME' (namespace $NS)"
	kubectl delete kubeneuron "$NAME" --ignore-not-found --wait=true --timeout=120s
	kubectl delete gpuplaybooks,gpuremediationpolicies -l kubeneuron.io/installed-by=install-sh --ignore-not-found
	kubectl -n "$NS" delete secret \
		"$NAME-operator-api-token" "$NAME-webhook-token" "$NAME-panel-users" \
		"$NAME-controller-tls" "$NAME-controller-server-ca" \
		"$NAME-agent-tls" "$NAME-agent-client-ca" --ignore-not-found
	say "left in place: the operator, CRDs, namespace '$NS', and any PVC"
	say "remove the operator too with: kubectl delete -f <install manifest>"
	exit 0
fi

work=$(mktemp -d); trap 'rm -rf "$work"' EXIT

say "preflight"
server_minor=$(kubectl version -o json 2>/dev/null | sed -n 's/.*"minor": *"\([0-9]*\).*/\1/p' | head -1)
[[ -n $server_minor && $server_minor -ge 29 ]] || die "Kubernetes 1.29+ required (server minor: ${server_minor:-unknown})"
if ! kubectl get storageclass -o json | grep -q '"storageclass.kubernetes.io/is-default-class": *"true"'; then
	echo "  WARNING: no default StorageClass — the SQLite store PVC will stay Pending until one exists" >&2
fi

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
kubectl -n kube-neuron rollout status deployment/kubeneuron-operator --timeout=180s >/dev/null
for crd in kubeneurons gpuplaybooks gpuremediationpolicies; do
	kubectl wait --for=condition=Established "crd/$crd.kubeneuron.io" --timeout=60s >/dev/null
done

say "generating operational secrets"
api_token=$(openssl rand -hex 32)
kubectl -n "$NS" create secret generic "$NAME-operator-api-token" \
	--from-literal=token="$api_token" --dry-run=client -o yaml | kubectl apply -f - >/dev/null
kubectl -n "$NS" create secret generic "$NAME-webhook-token" \
	--from-literal=token="$(openssl rand -hex 32)" --dry-run=client -o yaml | kubectl apply -f - >/dev/null

admin_password=""
auth_block=""
if [[ -n $ADMIN_USER ]]; then
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

kubectl apply -f - >/dev/null <<EOF
apiVersion: kubeneuron.io/v1alpha1
kind: GPUPlaybook
metadata:
  name: $NAME-observe
  labels: {kubeneuron.io/installed-by: install-sh}
spec:
  kubeNeuronRef: $NAME
  target: GPU
  steps:
    - name: observe
      action: Observe
---
apiVersion: kubeneuron.io/v1alpha1
kind: GPURemediationPolicy
metadata:
  name: $NAME-default-observe
  labels: {kubeneuron.io/installed-by: install-sh}
spec:
  kubeNeuronRef: $NAME
  priority: 1000
  match:
    class: xid-app
  playbookRef: $NAME-observe
---
apiVersion: kubeneuron.io/v1alpha1
kind: KubeNeuron
metadata:
  name: $NAME
spec:
  namespace: $NS
  controller:
    image: $controller_image
  agent:
    image: $agent_image
    tolerations:
      - operator: Exists
$auth_block
  safety:
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
  observability:
    victoriaMetrics: {mode: External, endpoint: "http://vmsingle-unset.$NS.svc:8428"}
    alertmanager: {mode: External, endpoint: "http://alertmanager-unset.$NS.svc:9093"}
  tls:
    issuer: Operator
    serverSecretRef: {name: $NAME-controller-tls}
    serverCASecretRef: {name: $NAME-controller-server-ca}
    clientSecretRef: {name: $NAME-agent-tls}
    clientCASecretRef: {name: $NAME-agent-client-ca}
EOF

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
fi
cat <<EOF
  API token     $api_token
                (automation/CLI: kubeneuronctl --server http://127.0.0.1:8080 --token ...)
  Mode          DryRun — detection, workflows, approvals, audit; no node is touched.

  Next steps    bind real playbooks/policies (docs/playbook-authoring.md),
                point Alertmanager at the webhook, add spec.agent.hostTooling
                on GPU node pools, review docs/install.md for production TLS.
  TLS note      this script generated a self-signed 90-day PKI to get you
                running; for production follow docs/install.md §3 or
                deploy/cert-manager/.
EOF
