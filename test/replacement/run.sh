#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
set -euo pipefail
cd "$(dirname "$0")/../.."
export CLUSTER_NAME=${KIND_CLUSTER_NAME:-replacement-test}
work=$(mktemp -d)
export KUBECONFIG=${KIND_KUBECONFIG:-$work/config}
export EVIDENCE_DIR=${KIND_ARTIFACTS_DIR:-$work/evidence}
image_tag=${REPLACEMENT_IMAGE_TAG:-$CLUSTER_NAME}
export AGENT_IMAGE="kubeneuron-agent:$image_tag" OPERATOR_IMAGE="kubeneuron-operator:$image_tag"
control="kubeneuron-controller:$image_tag"
base="kubeneuron-agent-base:$image_tag"
d=test/replacement
# shellcheck source=test/replacement/provider.sh
source "$d/provider.sh"
mkdir -p "$EVIDENCE_DIR"
clusters=$(kind get clusters)
if grep -Fxq "$CLUSTER_NAME" <<<"$clusters"; then echo "Cluster already exists: $CLUSTER_NAME" >&2; exit 1; fi
created=0
pf=
cleanup() {
  if [[ -n $pf ]]; then kill "$pf" 2>/dev/null || true; fi
  if [[ $created == 1 && ${KEEP_CLUSTER:-0} != 1 ]]; then kind delete cluster --name "$CLUSTER_NAME"; fi
  rm -f "$work/header"
}
trap cleanup EXIT
docker build -f build/Dockerfile --target agent -t "$base" .
docker build -f build/Dockerfile --target operator -t "$OPERATOR_IMAGE" .
docker build --build-arg REHEARSAL_FIRST_NODE="$CLUSTER_NAME-worker" -f "$d/Dockerfile" -t "$control" .
arch=$(docker image inspect "$base" --format '{{.Architecture}}')
CGO_ENABLED=0 GOOS=linux GOARCH="$arch" go build -trimpath -o "$work/nvidia-smi" "./$d/smi"
docker build --build-arg AGENT_IMAGE="$base" -f "$d/smi/Dockerfile" -t "$AGENT_IMAGE" "$work"
sed -e "s|POOL_LABEL|$POOL_LABEL|g" -e "s|INITIAL_ID|$INITIAL_ID|g" "$d/kind.yaml" >"$work/kind.yaml"
created=1
kind create cluster --name "$CLUSTER_NAME" --config "$work/kind.yaml" --kubeconfig "$KUBECONFIG" --image kindest/node:v1.33.12@sha256:3f5c8443c620245e4d355cfe09e96a91ead32ceaa569d3f1ca9edf0cb2fe2ff4
kind load docker-image --name "$CLUSTER_NAME" "$OPERATOR_IMAGE" "$control" "$AGENT_IMAGE"
"$d/install-operator.sh"
k=(kubectl --context "kind-$CLUSTER_NAME")
"${k[@]}" create namespace rehearsal
for secret in rehearsal-operator-api-token rehearsal-webhook-token; do
  openssl rand -hex 32 | tr -d '\n' | "${k[@]}" -n rehearsal create secret generic "$secret" --from-file=token=/dev/stdin
done
worker="$CLUSTER_NAME-worker"
"${k[@]}" cordon "$worker"
"${k[@]}" taint node "$CLUSTER_NAME-control-plane" node-role.kubernetes.io/control-plane:NoSchedule-
sed "s|POOL_LABEL|$POOL_LABEL|g" "$d/installation.json" | jq --argjson cloud "$CLOUD_CONFIG" --arg agent "$AGENT_IMAGE" --arg controller "$control" '
  .spec.cloud=$cloud | .spec.agent.image=$agent | .spec.controller.image=$controller' | "${k[@]}" apply -f -
"${k[@]}" kustomize config/cloud-policies | sed -e 's/kubeneuron-cloud-/rehearsal-cloud-/g' -e 's/kubeNeuronRef: kubeneuron$/kubeNeuronRef: rehearsal/' | "${k[@]}" apply -f -
"${k[@]}" -n rehearsal wait --for=create deploy/rehearsal-controller --timeout=2m
"${k[@]}" -n rehearsal rollout status deploy/rehearsal-controller --timeout=3m
"${k[@]}" uncordon "$worker"
"${k[@]}" patch node "$worker" --subresource=status --type=merge -p '{"status":{"capacity":{"nvidia.com/gpu":"1"},"allocatable":{"nvidia.com/gpu":"1"}}}'
"$d/check-profile.sh"
umask 077
export API_HEADER="$work/header"
{ printf 'Authorization: Bearer '; "${k[@]}" -n rehearsal get secret rehearsal-operator-api-token -o jsonpath='{.data.token}' | openssl base64 -d -A; printf '\n'; } >"$API_HEADER"
"${k[@]}" -n rehearsal port-forward svc/rehearsal-controller :8080 >"$work/forward" 2>&1 &
pf=$!
for ((i=0;i<50;i++)); do
  port=$(sed -n 's/.*127\.0\.0\.1:\([0-9]*\) -> 8080.*/\1/p' "$work/forward" | head -1)
  [[ -z $port ]] || break
  sleep 1
done
export API_URL="http://127.0.0.1:${port:?port-forward failed}/api/v1"
"$d/run-scenarios.sh"
