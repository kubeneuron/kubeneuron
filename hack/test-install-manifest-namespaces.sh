#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# The install manifest keeps the namespace each source object names: the
# operator's root-events Role and RoleBinding stay in default, where the
# KubeNeuron object's events and the node events are, and everything else that
# is namespaced lands in kube-neuron.
#
#   hack/test-install-manifest-namespaces.sh            # renders config/default
#   hack/test-install-manifest-namespaces.sh FILE       # checks a built manifest
set -euo pipefail
cd "$(dirname "$0")/.."
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

if [[ $# -gt 0 ]]; then
	cp "$1" "$tmp/manifest.yaml"
else
	kubectl kustomize config/default >"$tmp/manifest.yaml"
fi
yq -o=json -I=0 'select(.metadata.namespace != null) | {"kind": .kind, "name": .metadata.name, "namespace": .metadata.namespace}' \
	"$tmp/manifest.yaml" | jq -sS 'sort_by(.kind, .name)' >"$tmp/actual"

# Expected: the namespace a source file names, else kube-neuron.
for f in config/rbac/*.yaml config/default/operator_deployment.yaml; do
	case $f in */kustomization.yaml) continue ;; esac
	yq -o=json -I=0 'select(.kind != null) | {"kind": .kind, "name": .metadata.name, "namespace": (.metadata.namespace // "kube-neuron")}' "$f"
done | jq -s '[.[] | select(.kind | test("^Cluster") | not)]' | jq -S 'sort_by(.kind, .name)' >"$tmp/expected"
diff -u "$tmp/expected" "$tmp/actual"

for kind in Role RoleBinding; do
	got=$(jq -r --arg k "$kind" '.[] | select(.kind == $k and .name == "kubeneuron-operator-root-events") | .namespace' "$tmp/actual")
	[[ $got == default ]] || { echo "$kind kubeneuron-operator-root-events is in '${got:-<missing>}', want default" >&2; exit 1; }
done
echo "install manifest namespaces: OK"

# Exercise the actual uninstall output without connecting to a cluster.
mkdir "$tmp/bin"
cat >"$tmp/bin/kubectl" <<'MOCK'
#!/bin/sh
case " $* " in
*" get deployments --all-namespaces "*) echo "${OPERATOR_NS-kube-neuron}" ;;
esac
MOCK
chmod +x "$tmp/bin/kubectl"
PATH="$tmp/bin:$PATH" bash deploy/install.sh --uninstall --skip-open-incident-check >"$tmp/uninstall.log"
grep -Fq 'Agent data was left in /var/lib/kube-neuron on every node where the agent ran, control-plane nodes included' "$tmp/uninstall.log"
grep -Fq 'and no other KubeNeuron installation in the cluster uses it' "$tmp/uninstall.log"
grep -Fq 'Do not delete the full install manifest if you want to keep the data' "$tmp/uninstall.log"
if grep -Fq 'kubectl delete -f' "$tmp/uninstall.log"; then
	echo "uninstall suggests deleting a manifest" >&2
	exit 1
fi

yq -o=json -I=0 '.' "$tmp/manifest.yaml" |
	jq -r 'select(.kind != "Namespace" and .kind != "CustomResourceDefinition") |
		[(.metadata.namespace // "-"), ((.kind | ascii_downcase) + "/" + .metadata.name)] | @tsv' |
	LC_ALL=C sort >"$tmp/operator-objects"
awk '/^kubectl / {
	ns = "-"; i = 2
	if ($i == "-n") { ns = $(i+1); i += 2 }
	if ($i != "delete" || $NF != "--ignore-not-found") exit 1
	for (++i; i < NF; i++) print ns "\t" $i
}' "$tmp/uninstall.log" | LC_ALL=C sort >"$tmp/hinted-objects"
diff -u "$tmp/operator-objects" "$tmp/hinted-objects"

PATH="$tmp/bin:$PATH" bash deploy/install.sh --uninstall --skip-open-incident-check --namespace training >"$tmp/custom-namespace.log"
grep -Fq "The volume claims retained by this uninstall are in namespace 'training', which the install manifest does not delete." "$tmp/custom-namespace.log"

OPERATOR_NS=ops PATH="$tmp/bin:$PATH" bash deploy/install.sh --uninstall >"$tmp/ops"
grep -Fq 'kubectl -n ops delete deployment/kubeneuron-operator' "$tmp/ops"
OPERATOR_NS='' PATH="$tmp/bin:$PATH" bash deploy/install.sh --uninstall --delete-data --yes >"$tmp/absent"
if grep -Eq 'left in place: the operator|delete deployment/kubeneuron-operator' "$tmp/absent"; then exit 1; fi

# Run the preflight through the installer with a synthetic Service and Secret.
cat >"$tmp/bin/kubectl" <<'MOCK'
#!/usr/bin/env bash
case " $* " in
*" get daemonsets -l "*)
	echo '{"items":[{"metadata":{"uid":"agent-ds","ownerReferences":[{"controller":true,"uid":"uid-1"}]}}]}' ;;
*" get pods -l "*"component in "*" -o name "*)
	if [[ ! -e $CLEANUP_DIR/terminating ]]; then
		touch "$CLEANUP_DIR/terminating"
		echo pod/terminating-agent
	else
		touch "$CLEANUP_DIR/agents-gone"
	fi ;;
*" get pods -l "*"component in "*" -o json "*)
	jq -n '{items:(["ready","busy","timeout","foreign"] | map(. as $node | {metadata:{ownerReferences:[
		{controller:true,kind:"DaemonSet",uid:(if $node == "foreign" then "another-ds" else "agent-ds" end)}]},
		spec:{nodeName:$node}}))}' ;;
*" get pods -A --field-selector spec.nodeName=busy "*) echo '{"items":[{"spec":{"volumes":[{"hostPath":{"path":"/var/lib/kube-neuron"}}]}}]}' ;;
*" get pods -A --field-selector "*) echo '{"items":[]}' ;;
*" create -f - -o name "*)
	[[ -e $CLEANUP_DIR/agents-gone ]] || exit 1
	manifest=$(cat)
	node=$(jq -er '.spec.nodeName' <<<"$manifest") || exit 1
	printf '%s\n' "$manifest" >"$CLEANUP_DIR/$node.json"
	echo "pod/kubeneuron-agent-cleanup-$node" ;;
*" get pod/kubeneuron-agent-cleanup-ready pod/kubeneuron-agent-cleanup-timeout -o json "*)
	[[ -e $CLEANUP_DIR/ready.json && -e $CLEANUP_DIR/timeout.json ]] || exit 1
	echo '{"items":[{"spec":{"nodeName":"ready"},"status":{"phase":"Succeeded"}},{"spec":{"nodeName":"timeout"},"status":{"phase":"Failed"}}]}' ;;
*" patch node "*)
	printf '%s\n' "$*" >>"$UNINSTALL_MUTATIONS"
	[[ $2 != node || $3 != changed ]] || exit 1 ;;
*" get node changed -o json "*)
	jq '.items[] | select(.metadata.name == "changed") | .metadata.resourceVersion = "2" |
		.metadata.annotations["kubeneuron.io/cordon-restore"] = "do-not-disrupt"' "$NODE_LIST" ;;
*" get kubeneuron -o name "*) echo "${REMAINING_ROOTS:-}" ;;
*" get kubeneuron kubeneuron --ignore-not-found "*) [[ -n ${ROOT_GONE:-} ]] || echo uid-1 ;;
*" get kubeneuron kubeneuron -o jsonpath={.spec.namespace} "*) echo kube-neuron ;;
*" get deployments --all-namespaces "*)
	case ${FAIL_AT:-} in
	operator-absent) exit 0 ;;
	operator-forbidden) exit 1 ;;
	*) echo kube-neuron ;;
	esac ;;
*" port-forward "*) [[ ${FAIL_AT:-} != tunnel ]] || exit 1; echo 'Forwarding from 127.0.0.1:43210 -> 8080'; exec sleep 30 ;;
*" get kubeneuron kubeneuron -o json "*) if [[ ${FAIL_AT:-} == token ]]; then echo "{}"; exit; fi; echo '{"metadata":{"uid":"uid-1"},"spec":{"agent":{"image":"example/agent:test"},"notifications":{"operatorAPIToken":{"name":"operator-test"}}}}' ;;
*" get secret operator-test -o json "*) [[ ${FAIL_AT:-} != secret ]] || exit 1; echo '{"data":{"token":"dGVzdA=="}}' ;;
*" get nodes -o json "*) if [[ ${FAIL_AT:-} == node-list ]]; then exit 17; fi; if [[ -n ${NODE_LIST:-} ]]; then cat "$NODE_LIST"; else echo '{"items":[]}'; fi ;;
*" delete "*) echo delete >>"$UNINSTALL_MUTATIONS" ;;
esac
MOCK
cat >"$tmp/bin/curl" <<'MOCK'
#!/bin/sh
cat "$INCIDENT_LIST"
MOCK
chmod +x "$tmp/bin/curl"
export INCIDENT_LIST="$tmp/incidents" UNINSTALL_MUTATIONS="$tmp/mutations"
for stage in operator-absent operator-forbidden; do
	if FAIL_AT=$stage PATH="$tmp/bin:$PATH" bash deploy/install.sh --uninstall >"$tmp/error" 2>&1; then
		echo "uninstall continued after $stage" >&2
		exit 1
	fi
	case "$stage" in
	operator-absent) grep -Fq 'operator is not installed' "$tmp/error" ;;
	operator-forbidden) grep -Fq 'could not check for the operator Deployment' "$tmp/error" ;;
	esac
	[[ ! -e $UNINSTALL_MUTATIONS ]]
done
for payload in '{"invalid":"response"}' '[{"id":"inc-42","remove_first":{"outcome":"needs-operator"}}]' '[{"id":"inc-42","successor":{}}]'; do
	printf '%s\n' "$payload" >"$INCIDENT_LIST"
	if PATH="$tmp/bin:$PATH" bash deploy/install.sh --uninstall >"$tmp/preflight" 2>&1; then
		echo "uninstall continued after a failed incident check" >&2
		exit 1
	fi
	case "$payload" in
	'{'*) grep -Fq 'could not check for open incidents' "$tmp/preflight" ;;
	*) grep -Fq 'waiting for the operator: inc-42.' "$tmp/preflight" ;;
	esac
	[[ ! -e $UNINSTALL_MUTATIONS ]]
done
echo '[{"id":"inc-42","successor":{"outcome":"settled-by-operator"}}]' >"$INCIDENT_LIST"
PATH="$tmp/bin:$PATH" bash deploy/install.sh --uninstall >"$tmp/preflight"
[[ -s $UNINSTALL_MUTATIONS ]]

# After the root was removed, cleanup can resume without an available API.
echo invalid >"$INCIDENT_LIST"
ROOT_GONE=1 PATH="$tmp/bin:$PATH" bash deploy/install.sh --uninstall >"$tmp/retry"
grep -Fq 'remove only the operator' "$tmp/retry"

ROOT_GONE=1 REMAINING_ROOTS=kubeneuron/other PATH="$tmp/bin:$PATH" bash deploy/install.sh --uninstall --delete-data --yes >"$tmp/shared-data"
grep -Fq 'Agent data in /var/lib/kube-neuron was retained because another KubeNeuron installation may use the same directory.' "$tmp/shared-data"

for stage in token secret tunnel; do
	if FAIL_AT=$stage PATH="$tmp/bin:$PATH" bash deploy/install.sh --uninstall >"$tmp/error" 2>&1; then exit 1; fi
	case "$stage" in
	token) grep -Fq 'this installation has no operator API token' "$tmp/error" ;;
	secret) grep -Fq "API token Secret 'operator-test' in namespace 'kube-neuron' could not be read" "$tmp/error" ;;
	tunnel) grep -Fq "no connection to Service 'kubeneuron-controller' in namespace 'kube-neuron' could be opened" "$tmp/error" ;;
	esac
done

export NODE_LIST="$tmp/nodes"
jq -n '{items: (["ours", "prior", "unknown", "changed"] | map(. as $name |
	{metadata: {name: $name, resourceVersion: "1", annotations: {
		"karpenter.sh/do-not-disrupt":"true", "kubeneuron.io/cordon-reason":"incident"
	} + (if $name == "unknown" then {} else {"kubeneuron.io/cordon-restore":
		(if $name == "prior" then "do-not-disrupt" else "" end)} end)}}))}' >"$NODE_LIST"
: >"$UNINSTALL_MUTATIONS"
ROOT_GONE=1 PATH="$tmp/bin:$PATH" bash deploy/install.sh --uninstall >"$tmp/pins"
[[ $(grep -c '^patch node ' "$UNINSTALL_MUTATIONS") == 2 ]]
grep -Fq 'patch node ours --type=merge -p {"metadata":{"resourceVersion":"1","annotations":{"karpenter.sh/do-not-disrupt":null}}}' "$UNINSTALL_MUTATIONS"
grep -Fq 'patch node changed ' "$UNINSTALL_MUTATIONS"
grep -Fq 'kubectl annotate node unknown karpenter.sh/do-not-disrupt-' "$tmp/pins"

export CLEANUP_DIR="$tmp/cleanup-pods"
mkdir "$CLEANUP_DIR"
jq -n '{items:(["ready","busy","timeout","foreign","cpu"] | map({metadata:{name:.}}))}' >"$NODE_LIST"
PATH="$tmp/bin:$PATH" bash deploy/install.sh --uninstall --delete-data --yes --skip-open-incident-check >"$tmp/cleanup-output"
grep -Fq 'Emptied /var/lib/kube-neuron on 1 node.' "$tmp/cleanup-output"
grep -Fq 'was not confirmed on: busy, timeout.' "$tmp/cleanup-output"
[[ ! -e $CLEANUP_DIR/busy.json && ! -e $CLEANUP_DIR/foreign.json && ! -e $CLEANUP_DIR/cpu.json ]]
for node in ready timeout; do
	jq -e '.spec | .automountServiceAccountToken == false and
		.volumes == [{name:"state",hostPath:{path:"/var/lib/kube-neuron",type:"Directory"}}] and
		(.containers | length == 1) and
		(.containers[0] | .image == "example/agent:test" and .args == ["--clean-agent-data"] and
			.securityContext.capabilities.drop == ["ALL"])' "$CLEANUP_DIR/$node.json" >/dev/null
done

ROOT_GONE=1 PATH="$tmp/bin:$PATH" bash deploy/install.sh --uninstall --delete-data --yes >"$tmp/retry"
grep -Fq 'The installation was already removed' "$tmp/retry"
grep -Fq 'run: sudo rm -rf -- /var/lib/kube-neuron' "$tmp/retry"
if grep -Fq 'Emptied /var/lib/kube-neuron' "$tmp/retry"; then exit 1; fi

status=0
FAIL_AT=node-list PATH="$tmp/bin:$PATH" bash deploy/install.sh --uninstall --delete-data --yes --skip-open-incident-check >"$tmp/interrupted.out" 2>"$tmp/interrupted.err" || status=$?
[[ $status == 17 ]]
grep -Fq 'Agent data in /var/lib/kube-neuron will be emptied on these nodes: busy, ready, timeout. Keep this list:' "$tmp/interrupted.out"
grep -Fq 'while reading the node list' "$tmp/interrupted.err"
grep -Fq 'not confirmed on: busy, ready, timeout.' "$tmp/interrupted.err"
grep -Fq 'If the installation still exists, repeat the uninstall. If it was already removed, after verifying no installation uses the directory, run on each of these nodes: sudo rm -rf -- /var/lib/kube-neuron' "$tmp/interrupted.err"
