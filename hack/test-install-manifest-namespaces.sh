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
printf '#!/bin/sh\nexit 0\n' >"$tmp/bin/kubectl"
chmod +x "$tmp/bin/kubectl"
PATH="$tmp/bin:$PATH" bash deploy/install.sh --uninstall --skip-open-incident-check >"$tmp/uninstall.log"
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

# Run the preflight through the installer with a synthetic Service and Secret.
cat >"$tmp/bin/kubectl" <<'MOCK'
#!/usr/bin/env bash
case " $* " in
*" port-forward "*) echo 'Forwarding from 127.0.0.1:43210 -> 8080'; exec sleep 30 ;;
*" get kubeneuron kubeneuron -o json "*) echo '{"spec":{"notifications":{"operatorAPIToken":{"name":"operator-test"}}}}' ;;
*" get secret operator-test -o json "*) echo '{"data":{"token":"dGVzdA=="}}' ;;
*" get nodes -o json "*) echo '{"items":[]}' ;;
*" delete "*) echo delete >>"$UNINSTALL_MUTATIONS" ;;
esac
MOCK
cat >"$tmp/bin/curl" <<'MOCK'
#!/bin/sh
cat "$INCIDENT_LIST"
MOCK
chmod +x "$tmp/bin/curl"
export INCIDENT_LIST="$tmp/incidents" UNINSTALL_MUTATIONS="$tmp/mutations"
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
