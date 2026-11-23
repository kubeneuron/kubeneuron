#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
set -euo pipefail
: "${CLUSTER_NAME:?}" "${KUBECONFIG:?}" "${AGENT_IMAGE:?}"
node=${1:?worker name}
provider_id=${2:?worker provider ID}
: "${POOL_LABEL:?}"
cp="$CLUSTER_NAME-control-plane"
k=(kubectl --context "kind-$CLUSTER_NAME")
image=$(docker inspect -f '{{.Config.Image}}' "$cp")
docker run -d --privileged --network kind --hostname "$node" --name "$node" \
  --cgroupns=private --tmpfs /tmp --tmpfs /run -v /var -v /lib/modules:/lib/modules:ro \
  --label "io.x-k8s.kind.cluster=$CLUSTER_NAME" --label io.x-k8s.kind.role=worker "$image"
docker exec "$node" sh -c 'printf "KUBELET_EXTRA_ARGS=--provider-id=%s --node-labels=%s=gpu,nvidia.com/gpu.present=true\n" "$1" "$2" >/etc/default/kubelet' sh "$provider_id" "$POOL_LABEL"
# Run kubeadm in a systemd service cgroup. A long-running docker exec
# in the root cgroup prevents kind from enabling kubelet controllers.
for ((attempt=0; attempt<60; attempt++)); do
  if docker exec "$node" systemctl is-active --quiet containerd; then break; fi
  sleep 1
done
docker exec "$node" systemctl is-active --quiet containerd
# The kind node has systemd's private control socket but no system bus
# for systemd-run. Start a oneshot unit via systemctl instead.
docker exec -i "$node" sh -c 'cat >/run/systemd/system/replacement-join.service' <<'UNIT'
[Service]
Type=oneshot
ExecStart=/bin/sh /run/replacement-join
ExecStopPost=/bin/rm -f /run/replacement-join
UNIT
# The node shares the already-running cluster's host kernel. Hosted runners
# can omit its config module, so kubeadm cannot inspect the kernel config.
docker exec "$cp" kubeadm token create --ttl 10m --print-join-command |
  docker exec -i "$node" sh -c 'umask 077; { read -r join; printf "%s --ignore-preflight-errors=SystemVerification\n" "$join"; } >/run/replacement-join'
docker exec "$node" systemctl daemon-reload
docker exec "$node" systemctl start --no-block replacement-join.service
# Allow systemctl to exit the root cgroup before kubelet configures it.

for ((attempt=0; attempt<120; attempt++)); do
  if "${k[@]}" get node "$node" >/dev/null 2>&1; then break; fi
  if docker exec "$node" systemctl is-failed --quiet replacement-join.service; then break; fi
  sleep 1
done
if ! "${k[@]}" get node "$node" >/dev/null; then
  docker exec "$node" journalctl -b --no-pager -u replacement-join.service -u kubelet -n 100 |
    sed -E 's/[a-z0-9]{6}\.[a-z0-9]{16}/[redacted]/g' >&2
  exit 1
fi
docker save "$AGENT_IMAGE" | docker exec -i "$node" ctr -n k8s.io images import - >/dev/null
"${k[@]}" wait --for=condition=Ready "node/$node" --timeout=2m
"${k[@]}" get node "$node" -o json | jq -e --arg id "$provider_id" --arg label "$POOL_LABEL" '.spec.providerID==$id and .metadata.labels[$label]=="gpu"' >/dev/null
"${k[@]}" patch node "$node" --subresource=status --type=merge -p '{"status":{"capacity":{"nvidia.com/gpu":"1"},"allocatable":{"nvidia.com/gpu":"1"}}}'
