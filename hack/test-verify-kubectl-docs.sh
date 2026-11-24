#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
set -euo pipefail
fixture=$(mktemp)
trap 'rm -f "$fixture"' EXIT
cat >"$fixture" <<'EOF'
kubectl -n wrong-namespace scale deploy/kubeneuron-operator --replicas=0
kubectl -n kube-neuron wait --for=delete pod \
  -l app.kubernetes.io/name=kubeneuron-operator
kubectl rollout status deployment/unknown
kubectl get pods --selector="app.kubernetes.io/component=absent"
EOF
if output=$(python3 hack/verify-kubectl-docs.py "$fixture" 2>&1); then
    exit 1
fi
for location in '1: namespace' '3: unknown pod selector' '4: unknown Deployment' '5: unknown pod selector'; do
    grep -F "$fixture:$location" <<<"$output" >/dev/null
done
