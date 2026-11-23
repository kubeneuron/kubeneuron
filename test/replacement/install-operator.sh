#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
set -euo pipefail
: "${KUBECONFIG:?set the rehearsal kubeconfig}"
: "${CLUSTER_NAME:?set the rehearsal kind cluster name}"
: "${OPERATOR_IMAGE:?set the local operator image}"
k=(kubectl --context "kind-$CLUSTER_NAME")
repo=$(cd "$(dirname "$0")/../.." && pwd)
"${k[@]}" get node "${CLUSTER_NAME}-worker" -o json |
  jq -e --arg id "$INITIAL_ID" --arg label "$POOL_LABEL" '.spec.providerID == $id and .metadata.labels[$label] == "gpu"' >/dev/null
"${k[@]}" apply -k "$repo/config/crd"
"${k[@]}" apply -f "$repo/config/default/namespace.yaml"
"${k[@]}" apply -k "$repo/config/rbac"
patch=$(jq -cn --arg image "$OPERATOR_IMAGE" '{spec:{template:{spec:{nodeSelector:{"node-role.kubernetes.io/control-plane":""},tolerations:[{key:"node-role.kubernetes.io/control-plane",operator:"Exists",effect:"NoSchedule"}],containers:[{name:"operator",image:$image,imagePullPolicy:"Never"}]}}}}')
"${k[@]}" patch --local -f "$repo/config/default/operator_deployment.yaml" -p "$patch" -o yaml | "${k[@]}" apply -f -
"${k[@]}" -n kube-neuron rollout status deploy/kubeneuron-operator --timeout=2m
