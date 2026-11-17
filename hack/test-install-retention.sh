#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Real-API installer lifecycle regression. Uses an already provisioned kind
# cluster/operator; called by kind-integration.sh with local candidate images.
set -euo pipefail
REPO_ROOT=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
: "${KUBECONFIG:?isolated test kubeconfig required}"
: "${CONTROLLER_IMAGE:?candidate controller image required}"
: "${AGENT_IMAGE:?candidate agent image required}"
KUBECTL_BIN=${KUBECTL_BIN:-kubectl}
name=${INSTALL_TEST_NAME:-integration-retention}
namespace=$name
work=$(mktemp -d)
chmod 700 "$work"
pf_pid=''
cleanup() {
	[[ -z $pf_pid ]] || { kill "$pf_pid" 2>/dev/null || true; wait "$pf_pid" 2>/dev/null || true; }
	"$KUBECTL_BIN" delete kubeneuron "$name" --ignore-not-found --timeout=120s >/dev/null 2>&1 || true
	"$KUBECTL_BIN" delete gpuplaybooks,gpuremediationpolicies -l "kubeneuron.io/installation=$name" --ignore-not-found >/dev/null 2>&1 || true
	"$KUBECTL_BIN" delete gpuplaybook "$name-legacy" "$name-other" "$name-mislabeled" --ignore-not-found >/dev/null 2>&1 || true
	"$KUBECTL_BIN" delete namespace "$namespace" --ignore-not-found --timeout=120s >/dev/null 2>&1 || true
	rm -rf "$work"
}
trap cleanup EXIT
# Installer deliberately uses the standard kubectl name (including curl|bash).
mkdir "$work/bin"
ln -s "$(command -v "$KUBECTL_BIN")" "$work/bin/kubectl"
export PATH="$work/bin:$PATH"
fail() { echo "installer retention FAIL: $*" >&2; exit 1; }
install() {
	# CRDs/operator are installed by the parent harness. Reapplying only its
	# namespace avoids replacing its candidate Deployment with a release tag.
	bash "$REPO_ROOT/deploy/install.sh" --name "$name" --namespace "$namespace" \
		--manifest "$REPO_ROOT/config/default/namespace.yaml" --admin '' \
		--controller-image "$CONTROLLER_IMAGE" --agent-image "$AGENT_IMAGE" >"$work/install.log" 2>&1 || {
		# Installer log contains generated credentials: do not print it.
		"$KUBECTL_BIN" get kubeneuron "$name" -o jsonpath='{.status.conditions}' >&2 || true
		fail "installation failed (credential-bearing log withheld)"
	}
}
uninstall() { bash "$REPO_ROOT/deploy/install.sh" --name "$name" --namespace "$namespace" --uninstall "$@"; }
connect_api() {
	if [[ -n $pf_pid ]]; then kill "$pf_pid" 2>/dev/null || true; wait "$pf_pid" 2>/dev/null || true; fi
	"$KUBECTL_BIN" -n "$namespace" port-forward "service/$name-controller" :8080 >"$work/forward.log" 2>&1 & pf_pid=$!
	local deadline=$((SECONDS+60))
	port=''
	while ((SECONDS<deadline)); do
		port=$(sed -n 's/.*127.0.0.1:\([0-9]*\) -> 8080.*/\1/p' "$work/forward.log" | head -1)
		[[ -n $port ]] && break
		sleep 1
	done
	[[ -n $port ]] || fail "port-forward did not start"
	printf 'Authorization: Bearer %s\n' "$("$KUBECTL_BIN" -n "$namespace" get secret "$name-operator-api-token" -o jsonpath='{.data.token}' | base64 --decode)" >"$work/header"
}
api() { curl --silent --show-error --fail --noproxy '*' --connect-timeout 5 --max-time 30 -H "@$work/header" "$@"; }
install
claim_uid=$("$KUBECTL_BIN" -n "$namespace" get pvc "$name-controller-state" -o jsonpath='{.metadata.uid}')
connect_api
"$KUBECTL_BIN" wait "kubeneuron/$name" --for='jsonpath={.status.conditions[?(@.type=="GPUVisibility")].reason}=NoGPUResource' --timeout=180s
[[ -z $("$KUBECTL_BIN" get kubeneuron "$name" -o 'jsonpath={.status.conditions[?(@.type=="ResetAvailable")].type}') ]] || fail "reset condition present with node actions disabled"
# Run the user-facing recipe against the installation this test just created.
# Extract its fenced block so changes to the published commands run in CI.
recipe="$work/first-run.sh"
awk '/^<!-- first-run-recipe -->$/ {found=1; next} found && /^```sh$/ {copy=1; next} copy && /^```$/ {exit} copy {print}' \
    "$REPO_ROOT/docs/quickstart.md" >"$recipe"
[[ -s $recipe ]] || fail "first-run recipe missing"
export PATH="$REPO_ROOT/bin:$PATH"
SERVER="http://127.0.0.1:$port" KUBENEURONCTL_TOKEN="$(sed 's/^Authorization: Bearer //' "$work/header")" \
    bash -eu -o pipefail "$recipe" >"$work/first-run.out"
grep -Eq '^dry-run: +true$' "$work/first-run.out" || fail "first-run incident was not dry-run"
grep -Fq "$name-observe (step " "$work/first-run.out" || fail "first-run incident lacks observe playbook"
incident=$(awk '$1 == "id:" {print $2}' "$work/first-run.out")
[[ -n $incident ]] || fail "first-run incident lacks ID"
# Legacy objects lack the new installation label. Foreign and mislabeled
# objects both carry the old global label; neither may be deleted.
for suffix in legacy other mislabeled; do
	ref=$name
	[[ $suffix == legacy ]] || ref="$name-other"
	"$KUBECTL_BIN" apply -f - >/dev/null <<YAML
apiVersion: kubeneuron.io/v1alpha1
kind: GPUPlaybook
metadata:
  name: $name-$suffix
  labels:
    kubeneuron.io/installed-by: install-sh
spec:
  kubeNeuronRef: $ref
  target: GPU
  steps: [{name: record, action: Observe}]
YAML
done
"$KUBECTL_BIN" label gpuplaybook "$name-mislabeled" "kubeneuron.io/installation=$name" >/dev/null
# Deletion must be confirmed BEFORE touching root, PVC or other objects.
if uninstall --delete-data </dev/null >"$work/refused.log" 2>&1; then fail "unconfirmed data deletion succeeded"; fi
"$KUBECTL_BIN" get kubeneuron "$name" >/dev/null
uninstall
[[ $("$KUBECTL_BIN" -n "$namespace" get pvc "$name-controller-state" -o jsonpath='{.metadata.uid}') == "$claim_uid" ]] || fail "PVC identity changed on uninstall"
[[ -z $("$KUBECTL_BIN" -n "$namespace" get pvc "$name-controller-state" -o jsonpath='{.metadata.ownerReferences[*].uid}') ]] || fail "retained PVC is still owned"
"$KUBECTL_BIN" get gpuplaybook "$name-other" "$name-mislabeled" >/dev/null
[[ -z $("$KUBECTL_BIN" get gpuplaybook "$name-legacy" --ignore-not-found -o name) ]] || fail "own legacy playbook survived"
install
[[ $("$KUBECTL_BIN" -n "$namespace" get pvc "$name-controller-state" -o jsonpath='{.metadata.uid}') == "$claim_uid" ]] || fail "reinstall replaced PVC"
connect_api
api "http://127.0.0.1:$port/api/v1/incidents/$incident" | jq -e --arg id "$incident" '.incident.id == $id and (.audit | length) > 0' >/dev/null || fail "reinstall lost incident or audit history"
uninstall --delete-data --yes
[[ -z $("$KUBECTL_BIN" -n "$namespace" get pvc "$name-controller-state" --ignore-not-found -o name) ]] || fail "explicit delete left PVC"
"$KUBECTL_BIN" get gpuplaybook "$name-other" "$name-mislabeled" >/dev/null
echo 'installer retention PASS: confirmation, state/PVC identity, reinstall, legacy scoping, foreign resources and explicit deletion'
