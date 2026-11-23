#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
set -euo pipefail
: "${KUBECONFIG:?set the rehearsal kubeconfig}" "${CLUSTER_NAME:?set the rehearsal cluster name}"
k=(kubectl --context "kind-$CLUSTER_NAME")
policy=gpuremediationpolicy/rehearsal-cloud-fell-off-bus
"${k[@]}" get node "$CLUSTER_NAME-worker" -o json | jq -e --arg id "$INITIAL_ID" '.spec.providerID == $id' >/dev/null
patch=$(jq -cn --arg label "$POOL_LABEL" --arg now "$(date -u +%FT%TZ)" '{spec:{
  mode:"Automatic",playbookRef:"rehearsal-cloud-replace",replacement:{strategy:"AddFirst"},
  automatic:{qualifiedProfiles:["kind-nvidia"],poolLabel:$label,
    nodeSelector:{($label):"gpu"},maxConcurrentNodes:1,
    maxUnhealthyPercent:25,nodeCooldown:"6h",enabledBy:"kind rehearsal",enabledAt:$now,
    acknowledgement:"I understand that automatic mode drains and replaces nodes without asking"}}}')
"${k[@]}" patch "$policy" --type=merge -p "$patch"
"${k[@]}" wait --for=condition=QualifiedProfilesResolved=False "$policy" --timeout=60s
"${k[@]}" get "$policy" -o json | jq -e 'any(.status.conditions[]; .type=="QualifiedProfilesResolved" and .status=="False" and (.message|contains("kind-nvidia")))' >/dev/null
sed "s|POOL_LABEL|$POOL_LABEL|g" "$(dirname "$0")/profile.yaml" | "${k[@]}" apply -f -
"${k[@]}" wait --for=condition=QualifiedProfilesResolved=True "$policy" --timeout=60s
