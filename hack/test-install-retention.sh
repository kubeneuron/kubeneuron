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
		--manifest "${installer_manifest:-$REPO_ROOT/config/default/namespace.yaml}" "$@" --admin '' \
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
# Simulate the CRD left by a 0.x uninstall in this isolated kind cluster.
crd=kubeneurons.kubeneuron.io
"$KUBECTL_BIN" annotate crd "$crd" kubeneuron.io/release-line- >/dev/null
crd_revision=$("$KUBECTL_BIN" get crd "$crd" -o jsonpath='{.metadata.resourceVersion}')
deployment_generation() {
	"$KUBECTL_BIN" -n kube-neuron get deploy kubeneuron-operator -o jsonpath='{.metadata.generation}'
}
operator_before=$(deployment_generation)
if (install) >/dev/null 2>&1; then fail "installer accepted an unmarked CRD"; fi
grep -Fq 'Upgrading from 0.x releases is not supported. Uninstall the 0.x installation, then install v1.0.' "$work/install.log" || fail "missing 0.x diagnostic"
[[ $("$KUBECTL_BIN" get crd "$crd" -o jsonpath='{.metadata.resourceVersion}') == "$crd_revision" ]] || fail "installer modified the existing CRD"
[[ $(deployment_generation) == "$operator_before" ]] || fail "installer modified the operator"
"$KUBECTL_BIN" annotate crd "$crd" kubeneuron.io/release-line=v1 >/dev/null

# An interrupted first install can leave a user-edited pack object before the root exists.
"$KUBECTL_BIN" create -f - >/dev/null <<YAML
apiVersion: kubeneuron.io/v1alpha1
kind: GPURemediationPolicy
metadata:
  name: $name-cloud-xid-app
spec:
  kubeNeuronRef: $name
  priority: 891
  match: {class: xid-app}
  playbookRef: $name-observe
YAML
if (install) >/dev/null 2>&1; then
  fail "installer accepted an unlabelled recovery object"
fi
grep -Fq "Conflicting recovery object GPURemediationPolicy/$name-cloud-xid-app" "$work/install.log" || fail "missing ownership conflict diagnostic"
"$KUBECTL_BIN" label gpuremediationpolicy "$name-cloud-xid-app" kubeneuron.io/installed-by=install-sh "kubeneuron.io/installation=$name" kubeneuron.io/policy-pack=cloud >/dev/null
install
[[ $("$KUBECTL_BIN" get gpuremediationpolicy "$name-cloud-xid-app" -o jsonpath='{.spec.priority}') == 891 ]] || fail "resume overwrote the existing policy"
operator_args() {
 "$KUBECTL_BIN" -n kube-neuron get deployment kubeneuron-operator -o json |
  jq -r '.spec.template.spec.containers[] | select(.name == "operator") | .args[]'
}
cloud_args=$(operator_args)
if grep -Eq '^--allow-node-level-actions(=true)?$' <<<"$cloud_args"; then
 fail "fresh cloud install enabled node-level actions"
fi
claim_uid=$("$KUBECTL_BIN" -n "$namespace" get pvc "$name-controller-state" -o jsonpath='{.metadata.uid}')
token_versions() { "$KUBECTL_BIN" -n "$namespace" get secret "$name-operator-api-token" "$name-webhook-token" -o jsonpath='{.items[*].metadata.resourceVersion}'; }
tokens=$(token_versions)
# The installer writes the Managed stack; the operator makes the metrics
# store's claim, which outlives the installation.
[[ $("$KUBECTL_BIN" get kubeneuron "$name" -o jsonpath='{.spec.observability.store.mode}') == Managed ]] || fail "installer did not write a Managed store"
"$KUBECTL_BIN" -n "$namespace" wait --for=create "pvc/$name-metrics-store" --timeout=120s >/dev/null || fail "operator made no metrics store claim"
metrics_uid=$("$KUBECTL_BIN" -n "$namespace" get pvc "$name-metrics-store" -o jsonpath='{.metadata.uid}')
# Another installation's store in the same namespace: never touched.
"$KUBECTL_BIN" apply -f - >/dev/null <<YAML
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: $name-b-metrics-store
  namespace: $namespace
  labels:
    app.kubernetes.io/name: kube-neuron
    app.kubernetes.io/instance: $name-b
    app.kubernetes.io/component: metrics-store
    app.kubernetes.io/managed-by: kubeneuron-operator
spec:
  accessModes: [ReadWriteOnce]
  resources: {requests: {storage: 1Gi}}
YAML
other_metrics_uid=$("$KUBECTL_BIN" -n "$namespace" get pvc "$name-b-metrics-store" -o jsonpath='{.metadata.uid}')
# A re-run without --observability keeps the user's metrics path: here an
# External store, which must not turn back into Managed.
# The user's own edits elsewhere in the spec stay too: here Paused.
"$KUBECTL_BIN" patch kubeneuron "$name" --type merge \
	-p '{"spec":{"safety":{"executionMode":"Paused"},"observability":{"store":{"mode":"External","endpoint":"http://metrics.example.invalid:8428/opentelemetry"},"grafana":{"mode":"Disabled"}}}}' >/dev/null
install
[[ $("$KUBECTL_BIN" get kubeneuron "$name" -o jsonpath='{.spec.safety.executionMode}') == Paused ]] ||
	fail "a re-run reset the installation's execution mode"
grep -Fq 'API token     unchanged' "$work/install.log" || fail "a re-run did not report the kept API token"
"$KUBECTL_BIN" patch kubeneuron "$name" --type merge -p '{"spec":{"safety":{"executionMode":"DryRun"}}}' >/dev/null
[[ $("$KUBECTL_BIN" get kubeneuron "$name" -o jsonpath='{.spec.observability.store.mode}/{.spec.observability.store.endpoint}/{.spec.observability.grafana.mode}') == External/http://metrics.example.invalid:8428/opentelemetry/Disabled ]] ||
	fail "a re-run without --observability changed the installation's metrics path"
[[ -z $("$KUBECTL_BIN" get kubeneuron "$name" -o jsonpath='{.spec.observability.alertmanager}') ]] || fail "installer wrote an Alertmanager"
# An existing installation without the block (one from before the metrics
# path existed) gets none from a re-run: Managed is only the default of a
# new one.
"$KUBECTL_BIN" patch kubeneuron "$name" --type json -p '[{"op":"remove","path":"/spec/observability"}]' >/dev/null
install
[[ $("$KUBECTL_BIN" get kubeneuron "$name" -o jsonpath='{.spec.observability.collector.mode}/{.spec.observability.store.mode}/{.spec.observability.grafana.mode}') != *Managed* ]] ||
	fail "a re-run without --observability turned on the metrics path of an installation that had none"
[[ $(token_versions) == "$tokens" ]] || fail "a re-run replaced the API or webhook token of an existing installation"
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
# Exercise the same dry-run path against a real Node so its UID and Ready
# status can be checked against the API rather than the synthetic demo name.
node=$("$KUBECTL_BIN" get nodes -o jsonpath='{.items[0].metadata.name}')
node_uid=$("$KUBECTL_BIN" get node "$node" -o jsonpath='{.metadata.uid}')
ready=$("$KUBECTL_BIN" get node "$node" -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}')
(
 export KUBENEURONCTL_TOKEN
 KUBENEURONCTL_TOKEN=$(sed 's/^Authorization: Bearer //' "$work/header")
 ctl=(kubeneuronctl --server "http://127.0.0.1:$port")
 "${ctl[@]}" remediate "$node" --class xid-app >/dev/null
 deadline=$((SECONDS+90))
 until ((SECONDS >= deadline)); do
   id=$("${ctl[@]}" incidents | awk -v node="$node" '$3=="xid-app" && $4==node {print $1; exit}')
   if [[ -n $id ]] && api "http://127.0.0.1:$port/api/v1/incidents/$id" |
     jq -e --arg uid "$node_uid" --arg ready "$ready" '
       def matches_node: .node_uid == $uid and (.conditions | any(.type == "Ready" and .status == $ready));
       (.node_conditions | matches_node) and
       (.audit | any(.action == "node-context" and (.params.node_conditions | fromjson | matches_node)))' >/dev/null; then
     exit 0
   fi
   sleep 2
 done
 fail "incident did not record the observed node UID and readiness"
)
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
"$KUBECTL_BIN" label nodes --all kubeneuron.io/journal-dir=run --overwrite >/dev/null
uninstall >"$work/uninstall.log"
[[ -z $("$KUBECTL_BIN" get nodes -l kubeneuron.io/journal-dir -o name) ]] || fail "uninstall left the journal-dir node label"
[[ $("$KUBECTL_BIN" -n "$namespace" get pvc "$name-controller-state" -o jsonpath='{.metadata.uid}') == "$claim_uid" ]] || fail "PVC identity changed on uninstall"
[[ $("$KUBECTL_BIN" -n "$namespace" get pvc "$name-metrics-store" -o jsonpath='{.metadata.uid}') == "$metrics_uid" ]] || fail "uninstall without --delete-data removed the metrics claim"
grep -Fq "kubectl delete pvc $name-metrics-store -n $namespace" "$work/uninstall.log" || fail "uninstall did not say how to erase the retained metrics"
[[ -z $("$KUBECTL_BIN" -n "$namespace" get pvc "$name-controller-state" -o jsonpath='{.metadata.ownerReferences[*].uid}') ]] || fail "retained PVC is still owned"
"$KUBECTL_BIN" get gpuplaybook "$name-other" "$name-mislabeled" >/dev/null
[[ -z $("$KUBECTL_BIN" get gpuplaybook "$name-legacy" --ignore-not-found -o name) ]] || fail "own legacy playbook survived"
install
[[ $("$KUBECTL_BIN" -n "$namespace" get pvc "$name-controller-state" -o jsonpath='{.metadata.uid}') == "$claim_uid" ]] || fail "reinstall replaced PVC"
connect_api
api "http://127.0.0.1:$port/api/v1/incidents/$incident" | jq -e --arg id "$incident" '.incident.id == $id and (.audit | length) > 0' >/dev/null || fail "reinstall lost incident or audit history"
uninstall --delete-data --yes
[[ -z $("$KUBECTL_BIN" -n "$namespace" get pvc "$name-controller-state" --ignore-not-found -o name) ]] || fail "explicit delete left PVC"
[[ -z $("$KUBECTL_BIN" -n "$namespace" get pvc "$name-metrics-store" --ignore-not-found -o name) ]] || fail "explicit delete left the metrics claim"
[[ $("$KUBECTL_BIN" -n "$namespace" get pvc "$name-b-metrics-store" -o jsonpath='{.metadata.uid}') == "$other_metrics_uid" ]] || fail "explicit delete touched another installation's metrics claim"
"$KUBECTL_BIN" get gpuplaybook "$name-other" "$name-mislabeled" >/dev/null
# Reapply the candidate Deployment with an explicit false flag. A rerun must
# preserve the administrator's effective permission across that manifest apply.
installer_manifest="$work/operator.json"
"$KUBECTL_BIN" -n kube-neuron get deployment kubeneuron-operator -o json |
 jq '{apiVersion,kind,metadata:{name:.metadata.name,namespace:.metadata.namespace},spec}
     | (.spec.template.spec.containers[] | select(.name=="operator").args) += ["--allow-node-level-actions=false"]' >"$installer_manifest"
install --policy-pack bare-metal-alpha
assert_alpha_allowed() {
 local args
 args=$(operator_args)
 [[ $(grep '^--allow-node-level-actions' <<<"$args" | tail -1) == --allow-node-level-actions=true ]] || fail "bare-metal operator permission lost"
 "$KUBECTL_BIN" get kubeneuron "$name" -o json |
  jq -e '.spec.safety.nodeLevelActions == true and
         ([.status.conditions[]? | select(.type == "NodeLevelActionsAllowed" and .status == "False")] | length == 0)' >/dev/null || fail "bare-metal installation permission denied"
}
assert_alpha_allowed
install
assert_alpha_allowed
uninstall --delete-data --yes
"$KUBECTL_BIN" apply -f "$installer_manifest" >/dev/null
"$KUBECTL_BIN" -n kube-neuron rollout status deployment/kubeneuron-operator --timeout=180s >/dev/null
echo 'installer retention PASS: confirmation, state/PVC identity, reinstall, legacy scoping, foreign resources, metrics claim scoping and explicit deletion'
