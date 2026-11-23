#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Sourced after B opens, before the original controller records the delete.
: "${id:?}" "${audit:?}" "${successor:?}" "${successor_id:?}" "${worker:?}"
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
# lost-grow: the first grow of the pool is applied and its answer lost; the
# delete is sent only by the restarted controller. lost-resize: the resize
# that sets the GKE pool back is applied and its answer lost.
case $RESTART_SCENARIO in
  lost-grow) accepted='.grow_calls==1 and .grow_effects==1 and .delete_calls==0' ;;
  lost-resize) accepted='.resize_calls==1' ;;
  *) accepted='.delete_calls==1 and .delete_effects==1' ;;
esac
snapshot() { docker exec "$host" cat "$dir/delete-snapshot.json" >"$EVIDENCE_DIR/delete.json" 2>/dev/null && jq -e "$accepted" "$EVIDENCE_DIR/delete.json" >/dev/null; }
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
if [[ $RESTART_SCENARIO != identity-changed ]]; then
  if [[ $RESTART_SCENARIO == lost-grow ]]; then
    # Only the new controller deletes the machine; its Node goes with it,
    # as on AKS, not before.
    accepted='.delete_calls==1 and .delete_effects==1'
    wait_until snapshot
  fi
  remove_worker "$successor" "$successor_id"
  if [[ ${REPLACEMENT_PROVIDER:-aws} == aks ]]; then
    final=aks-gpu-12345678-vmss000002
    final_id="${INITIAL_ID%/*}/2"
  elif [[ ${REPLACEMENT_PROVIDER:-aws} == gke ]]; then
    plan_restored() { api "incidents/$id" >"$audit" && jq -e '.incident.remove_first.outcome=="restored" and (.incident.remove_first.state.resize_diff|strings|length)>0' "$audit" >/dev/null; }
    wait_until plan_restored
    final_id=$(jq -er '.incident.remove_first.state.resize_diff' "$audit")
    final=${final_id##*/}
    test "$final" = "$worker"
  else
    final_id=aws:///az/i-final
  fi
  ./join-worker.sh "$final" "$final_id"
  wait_until report_ready "$final"
fi
wait_until restart_outcome
accepted='.delete_calls==1 and .delete_effects==1'
if [[ $RESTART_SCENARIO == lost-resize ]]; then
  # One resize, never sent again, and every zone back at its size.
  size=$(jq -er '.incident.remove_first.state.size' "$audit")
  accepted='.resize_calls==1 and all(.groups[]; .target=='"$size"')'
elif [[ ${REPLACEMENT_PROVIDER:-aws} == aks && $RESTART_SCENARIO != identity-changed ]]; then
  # One grow and one delete, and the pool back at its size without the
  # replacement tag, so the next replacement in it does not wait.
  before=$(jq -er '.incident.remove_first.state.count_before' "$audit")
  accepted="$accepted"' and .grow_calls==1 and .grow_effects==1 and .Count=='"$before"' and (.Tags["kubeneuron-replacing"]==null)'
fi
snapshot
if [[ $RESTART_SCENARIO != identity-changed ]]; then
  docker exec "$host" sh -ec 'command -v sqlite3 >/dev/null || (apt-get update -qq && apt-get install -y --no-install-recommends sqlite3)' >/dev/null
  [[ $id =~ ^[a-zA-Z0-9_-]+$ ]]
  owners=$(docker exec "$host" sqlite3 -readonly "$dir/kubeneuron.db" "select count(*) from node_remediation_owners where incident_id='$id'")
  [[ $owners == 0 ]]
fi
