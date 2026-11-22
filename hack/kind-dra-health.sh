#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Checks the DRA device taint read against a real Kubernetes 1.36 API server
# in kind: a ResourceSlice with a device taint, created by hand for a node
# (no DRA driver runs), keeps the taint in resource.k8s.io/v1 with timeAdded
# set, and the platform read and the profile table see it as a fault. The
# controller's incident path is covered by the unit tests, not here.
# Needs docker, kind and kubectl.
set -euo pipefail
REPO_ROOT=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
KIND_BIN=${KIND_BIN:-kind}
KUBECTL_BIN=${KUBECTL_BIN:-kubectl}
# Kubernetes 1.36.4, where DRADeviceTaints is beta and on by default.
NODE_IMAGE='kindest/node:v1.36.4@sha256:099e049362a1526b2db71494e1947aae99bd16290d7c895f2b7ea312e3cbfaed'
CLUSTER=${CLUSTER:-kn-dra-health-$$}
work=$(mktemp -d)
cleanup() {
	[[ ${KEEP_CLUSTER:-0} == 1 ]] || "$KIND_BIN" delete cluster --name "$CLUSTER" >/dev/null 2>&1 || true
	rm -rf "$work"
}
trap cleanup EXIT

"$KIND_BIN" create cluster --name "$CLUSTER" --image "$NODE_IMAGE" --kubeconfig "$work/kubeconfig" --wait 120s >/dev/null 2>&1
export KUBECONFIG="$work/kubeconfig"
node=$("$KUBECTL_BIN" get nodes -o jsonpath='{.items[0].metadata.name}')
"$KUBECTL_BIN" apply -f - >/dev/null <<YAML
apiVersion: resource.k8s.io/v1
kind: ResourceSlice
metadata:
  name: kn-dra-health-gpu
spec:
  driver: gpu.nvidia.com
  nodeName: $node
  pool: {name: $node, generation: 1, resourceSliceCount: 1}
  devices:
  - name: gpu-0
    attributes:
      uuid: {string: GPU-0a1b2c3d-4e5f-6a7b-8c9d-0e1f2a3b4c5d}
    taints:
    - {key: gpu.nvidia.com/xid, value: "79", effect: NoSchedule}
  - name: gpu-1
YAML
echo "server: $("$KUBECTL_BIN" version -o json | sed -n 's/.*"gitVersion": "\(v[^"]*\)".*/\1/p' | tail -1)"
cd "$REPO_ROOT"
KUBENEURON_DRA_LIVE_KUBECONFIG="$work/kubeconfig" KUBENEURON_DRA_LIVE_NODE="$node" \
	go test -count=1 -run TestDRAHealthLive -v ./internal/platform/kubernetes | grep -E '^(=== RUN|--- |PASS|FAIL|ok)|device '
echo "kind-dra-health: PASS"
