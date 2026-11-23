#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Sourced after B opens, before the original controller records the delete.
: "${id:?}" "${audit:?}" "${successor:?}" "${successor_id:?}"
k=(kubectl --context "kind-${CLUSTER_NAME:?}")
selector=$("${k[@]}" -n rehearsal get deploy rehearsal-controller -o json |
  jq -er '.spec.selector.matchLabels | to_entries | map(.key + "=" + .value) | join(",")')
old=$("${k[@]}" -n rehearsal get pod -l "$selector" -o json | jq -er '.items | select(length==1) | first.metadata.name')
pod=$("${k[@]}" -n rehearsal get pod "$old" -o json)
old_uid=$(jq -er '.metadata.uid' <<<"$pod")
host=$(jq -er '.spec.nodeName' <<<"$pod")
claim=$(jq -er '.spec.volumes[]|select(.name=="state").persistentVolumeClaim.claimName' <<<"$pod")
pv=$("${k[@]}" -n rehearsal get pvc "$claim" -o jsonpath='{.spec.volumeName}')
dir=$("${k[@]}" get pv "$pv" -o json | jq -er '.spec.hostPath.path // .spec.local.path')
snapshot() { docker exec "$host" cat "$dir/delete-snapshot.json" >"$EVIDENCE_DIR/delete.json" 2>/dev/null && jq -e '.delete_calls==1 and .delete_effects==1' "$EVIDENCE_DIR/delete.json" >/dev/null; }
wait_until snapshot
"${k[@]}" -n rehearsal delete pod "$old" --grace-period=0 --force
"${k[@]}" -n rehearsal rollout status deploy/rehearsal-controller --timeout=3m
new_ready() {
  "${k[@]}" -n rehearsal get pod -l "$selector" -o json | jq -e --arg old "$old_uid" 'any(.items[];.metadata.uid!=$old and any(.status.conditions[]?;.type=="Ready" and .status=="True"))' >/dev/null
}
wait_until new_ready
"${k[@]}" -n rehearsal port-forward svc/rehearsal-controller :8080 >"$EVIDENCE_DIR/restart-forward.log" 2>&1 &
forward=$!
trap 'kill "$forward" 2>/dev/null || true' EXIT
new_port() { port=$(sed -n 's/.*127\.0\.0\.1:\([0-9]*\) -> 8080.*/\1/p' "$EVIDENCE_DIR/restart-forward.log" | head -1); [[ -n $port ]]; }
wait_until new_port
export API_URL="http://127.0.0.1:$port/api/v1"
restart_outcome() { api "incidents/$id" >"$audit" && jq -e --arg scenario "$RESTART_SCENARIO" -f assert-restart.jq "$audit" >/dev/null; }
if [[ $RESTART_SCENARIO == resolved ]]; then
  remove_worker "$successor" "$successor_id"
  if [[ ${REPLACEMENT_PROVIDER:-aws} == aks ]]; then
    final=aks-gpu-12345678-vmss000002
    final_id="${INITIAL_ID%/*}/2"
  else
    final_id=aws:///az/i-final
  fi
  ./join-worker.sh "$final" "$final_id"
  wait_until report_ready "$final"
fi
wait_until restart_outcome
snapshot
if [[ $RESTART_SCENARIO == resolved ]]; then
  docker exec "$host" sh -ec 'command -v sqlite3 >/dev/null || (apt-get update -qq && apt-get install -y --no-install-recommends sqlite3)' >/dev/null
  [[ $id =~ ^[a-zA-Z0-9_-]+$ ]]
  owners=$(docker exec "$host" sqlite3 -readonly "$dir/kubeneuron.db" "select count(*) from node_remediation_owners where incident_id='$id'")
  [[ $owners == 0 ]]
fi
