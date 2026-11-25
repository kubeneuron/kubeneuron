#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
set -euo pipefail
: "${CLUSTER_NAME:?}" "${KUBECONFIG:?}" "${AGENT_IMAGE:?}" "${API_URL:?}" "${API_HEADER:?}" "${WEBHOOK_HEADER:?}" "${EVIDENCE_DIR:?}"
cd "$(dirname "$0")"
mkdir -p "$EVIDENCE_DIR"
k=(kubectl --context "kind-$CLUSTER_NAME")
api() { curl -fsS --max-time 5 -H "@$API_HEADER" "$API_URL/$1"; }
wait_until() {
  local deadline=$((SECONDS + 240))
  until "$@"; do
    if (( SECONDS >= deadline )); then echo "Timed out waiting for $*" >&2; return 1; fi
    sleep 2
  done
}
pool_ready() {
  api pools | jq -e --arg order "$1" 'any(.pools[]; .pool=="gpu" and .accelerator_nodes==1 and .replacement_policy==$order and .replacement_status=="available")' >/dev/null
}
report_ready() {
  api "nodes/$1/accelerators" >"$EVIDENCE_DIR/$1-report.json" &&
    jq -e 'any(.[]; .vendor=="nvidia" and .readiness=="degraded" and (.devices|length)==1 and (.devices[0].driver_healthy_at|strings|length)>0)' "$EVIDENCE_DIR/$1-report.json" >/dev/null
}
incident_opened() {
  id=$(api incidents | jq -er --arg node "$1" --arg pci "$2" '[.[]|select(.target.node==$node and .class=="fell-off-bus" and .target.pci_addr==$pci)] | select(length==1) | first.id')
}
audit_matches() {
  api "incidents/$id" >"$audit" || return 1
  if jq -e '.incident.state=="NEEDS_HUMAN" or .incident.state=="AWAITING_APPROVAL"' "$audit" >/dev/null; then
    echo "Unexpected hold for $id; see $audit" >&2; exit 1
  fi
  jq -e "$1" "$audit" >/dev/null
}
inject() {
  wait_until report_ready "$1"
  docker exec "$1" sh -ec 'export DEBIAN_FRONTEND=noninteractive; apt-get update -qq; apt-get install -y --no-install-recommends systemd-journal-remote'
  ./inject-xid.sh "$1" "$2"
  wait_until incident_opened "$1" "$2"
}
remove_machine() {
  test "$("${k[@]}" get node "$1" -o jsonpath='{.spec.providerID}')" = "$2"
  test "$(docker inspect -f '{{index .Config.Labels "io.x-k8s.kind.cluster"}}' "$1")" = "$CLUSTER_NAME"
  docker rm -f "$1"
}
remove_worker() {
  remove_machine "$1" "$2"
  "${k[@]}" delete node "$1"
}
resolved() {
  wait_until audit_matches '.incident.state=="RESOLVED"'
  jq -e --arg node "$1" --arg correlation "$2" -f assert-resolution.jq "$audit"
}
# The machine behind node $1 is removed and its Node is not deleted yet: until
# the cloud deletes it, which can take minutes, vmalert reports the agent down.
# The alert, sent as vmalert does, is recorded on the incident that removed
# the machine.
removed_machine_alert() {
  jq -nc --arg node "$1" '[{labels: {alertname: "KubeNeuronAgentDown", severity: "warning", job: "kubeneuron-agent", node: $node},
    annotations: {summary: ("kubeneuron-agent down on " + $node)}, startsAt: (now | todate)}]' |
    curl -fsS --max-time 5 -H "@$WEBHOOK_HEADER" -H 'Content-Type: application/json' -d @- "$API_URL/webhooks/vmalert/api/v2/alerts" >/dev/null
  wait_until audit_matches 'any(.audit[]; .action=="replaced-node-signal" and .params.signal_class=="agent-down" and .params.alertname=="KubeNeuronAgentDown")'
}
# The EKS node monitoring agent writes the injected fault on node $1 as a
# condition. The Node keeps it after the machine is gone, fresh for ten
# minutes after its last heartbeat, until the cloud deletes the Node.
fault_condition() {
  local now
  now=$(date -u +%Y-%m-%dT%H:%M:%SZ)
  "${k[@]}" patch node "$1" --subresource=status --type=strategic -p "$(jq -nc --arg now "$now" '{status: {conditions: [{type: "AcceleratedHardwareReady",
    status: "False", reason: "NvidiaXID79Error", lastHeartbeatTime: $now, lastTransitionTime: $now}]}}')" >/dev/null
}
# Node health reads the condition again while the incident verifies the
# replacement. The replacing incident records it, and it is no recurrence.
removed_machine_fault() {
  wait_until audit_matches 'any(.audit[]; .action=="replaced-node-signal" and .from_state=="VERIFYING" and .params.signal_source=="node-health" and .params.signal_class=="fell-off-bus")'
}
policy_loaded() {
  digest=$("${k[@]}" -n rehearsal get cm rehearsal-runtime -o json |
    jq -er --arg marker "$1" 'select(.data["policies.yaml"]|contains($marker)) | .data["config-digest"]') || return 1
  api runtime-config | jq -e --arg digest "$digest" '.source_digest==$digest' >/dev/null
}
worker="$CLUSTER_NAME-worker"
successor="$CLUSTER_NAME-successor"
final="$CLUSTER_NAME-final"
audit="$EVIDENCE_DIR/A.json"
wait_until pool_ready AddFirst
wait_until policy_loaded kind-nvidia
initial_uid=$("${k[@]}" get node "$worker" -o jsonpath='{.metadata.uid}')
if [[ ${ZONE_SCENARIO:-} == AnyPoolZone ]]; then
  # shellcheck source=test/replacement/any-pool-zone.sh
  source ./any-pool-zone.sh
  exit 0
fi
if [[ ${ZONE_SCENARIO:-} == SameZone ]]; then
  inject "$worker" 124c:3b:00
  # Called indirectly by wait_until.
  # shellcheck disable=SC2329
  refusal_recorded() {
    api "incidents/$id" >"$audit" &&
      jq -e 'any(.audit[]; (.result // "") | contains("EKS chooses the zone of a new node in this pool"))' "$audit" >/dev/null
  }
  wait_until refusal_recorded
  api pools >"$EVIDENCE_DIR/same-zone-pools.json"
  "${k[@]}" get node "$worker" -o json >"$EVIDENCE_DIR/same-zone-node.json"
  "${k[@]}" -n rehearsal logs deploy/rehearsal-controller >"$EVIDENCE_DIR/same-zone-controller.log"
  jq -ne --arg uid "$initial_uid" --slurpfile pool "$EVIDENCE_DIR/same-zone-pools.json" \
    --slurpfile original "$EVIDENCE_DIR/same-zone-node.json" --rawfile log "$EVIDENCE_DIR/same-zone-controller.log" \
    '{pools:$pool[0].pools,node:$original[0],log:$log,uid:$uid}' |
    jq -e -f assert-same-zone.jq
  exit 0
fi
inject "$worker" 124c:3b:00
fault_condition "$worker"
wait_until audit_matches '(.incident.successor.provider_id | strings | length)>0 and any(.audit[];.action=="successor-created")'
successor_id=$(jq -er '.incident.successor.provider_id' "$audit")
case ${REPLACEMENT_PROVIDER:-aws} in
  aws) test "$successor_id" = aws:///az/i-new ;;
  aks) test "${successor_id##*/}" = 1; successor=aks-gpu-12345678-vmss000001 ;;
  gke) successor=${successor_id##*/} ;;
esac
./join-worker.sh "$successor" "$successor_id"
wait_until audit_matches 'any(.audit[];.action=="successor-replaced")'
removed_machine_alert "$worker"
remove_machine "$worker" "$INITIAL_ID"
removed_machine_fault
"${k[@]}" delete node "$worker"
resolved "$successor" successor
"${k[@]}" patch gpuremediationpolicy rehearsal-cloud-fell-off-bus --type merge -p '{"spec":{"replacement":{"strategy":"RemoveFirst"}}}'
audit="$EVIDENCE_DIR/B.json"
wait_until pool_ready mixed
wait_until policy_loaded "strategy: remove-first"
inject "$successor" 124d:3b:00
if [[ -n ${RESTART_SCENARIO:-} ]]; then
  # shellcheck source=test/replacement/restart-controller.sh
  source ./restart-controller.sh
  exit 0
fi
fault_condition "$successor"
if [[ ${REPLACEMENT_PROVIDER:-aws} == gke ]]; then
  wait_until audit_matches '.incident.remove_first.phase=="done" and .incident.remove_first.state.size=="1" and (.incident.remove_first.state.resize_diff|strings|length)>0 and any(.audit[];.action=="remove-first-planned")'
  final_id=$(jq -er '.incident.remove_first.state.resize_diff' "$audit")
  final=${final_id##*/}
  test "$final" = "$worker"
  correlation=resize-diff
else
  wait_until audit_matches 'any(.audit[];.action=="replace" and (.result//""|startswith("terminated the instance behind ")))'
  if [[ ${REPLACEMENT_PROVIDER:-aws} == aks ]]; then
    final_id=$(jq -er --arg old "$successor" '.audit[] | select(.action=="replacement-instance" and .params.node==$old) | .params.provider_id' "$audit")
    test "$final_id" = "${INITIAL_ID%/*}/2"
    final=aks-gpu-12345678-vmss000002
    correlation=resize-diff
  else
    final_id=aws:///az/i-final
    correlation=pool
  fi
fi
wait_until audit_matches '.incident.remove_first.phase=="done"'
removed_machine_alert "$successor"
remove_machine "$successor" "$successor_id"
removed_machine_fault
"${k[@]}" delete node "$successor"
./join-worker.sh "$final" "$final_id"
wait_until report_ready "$final"
resolved "$final" "$correlation"
# Each replacement is one incident: neither alert opened one of its own.
api incidents | jq -e 'all(.[]; .class != "agent-down")' >/dev/null
if [[ ${REPLACEMENT_PROVIDER:-aws} == gke ]]; then
  final_uid=$("${k[@]}" get node "$final" -o jsonpath='{.metadata.uid}')
  test "$final_uid" != "$initial_uid"
  jq -e --arg uid "$final_uid" 'any(.audit[];.action=="resolve" and .params.replacement_node_uid==$uid)' "$audit"
  exit 0
fi

if [[ ${REPLACEMENT_PROVIDER:-aws} == aks ]]; then
  audit="$EVIDENCE_DIR/arm-hold.json"
  inject "$final" 124e:3b:00
  wait_until audit_matches 'any(.audit[]; .action=="provider-recovery-unavailable" and (.result|contains("AuthorizationFailed")))'
  jq -e '[.audit[] | select(.action=="provider-recovery-unavailable") | .result] |
    all(.[]; (contains("management.azure.com") or contains("/subscriptions/")) | not)' "$audit"
  jq -e 'all(.audit[]; .action!="policy-authorised" and .action!="approval-approved")' "$audit"
  exit 0
fi

# A policy that names a missing profile must park a real incident before acting.
policy=gpuremediationpolicy/rehearsal-cloud-fell-off-bus
"${k[@]}" patch "$policy" --type merge -p '{"spec":{"automatic":{"qualifiedProfiles":["kind-missing"]}}}'
"${k[@]}" wait --for=condition=QualifiedProfilesResolved=False "$policy" --timeout=60s
# Policy conditions are published before kubelet projects the new ConfigMap.
# Wait until the controller identifies the exact compilation under test.
wait_until policy_loaded kind-missing
audit="$EVIDENCE_DIR/missing-profile.json"
inject "$final" 124e:3b:00
missing_profile_hold() {
  api "incidents/$id" >"$audit" &&
    jq -e '.incident.state=="AWAITING_APPROVAL" and any(.audit[]; (.result//""|contains("node profile kind-nvidia is not listed in the policy"))) and all(.audit[]; .action!="policy-authorised" and .action!="approval-approved")' "$audit" >/dev/null
}
wait_until missing_profile_hold

# Close the parked fixture incident before exercising a second policy revision.
curl -fsS -H "@$API_HEADER" -H 'Content-Type: application/json' \
  -H "Idempotency-Key: resolve-$id" \
  -d '{"actor":"rehearsal","reason":"Completed the missing-profile assertion"}' \
  "$API_URL/incidents/$id/resolve" >/dev/null
"${k[@]}" patch "$policy" --type merge -p '{"spec":{"automatic":{"qualifiedProfiles":["kind-nvidia","kind-warmup"]}}}'
wait_until policy_loaded kind-warmup
before=$digest
"${k[@]}" patch "$policy" --type merge -p '{"spec":{"automatic":{"qualifiedProfiles":["kind-early-missing"]}}}'
./inject-xid.sh "$final" 124f:3b:00
wait_until incident_opened "$final" 124f:3b:00
audit="$EVIDENCE_DIR/early-xid.json"
wait_until missing_profile_hold
# Held until the reload, then parked for approval: see assert-early-hold.jq.
wait_until policy_loaded kind-early-missing
jq -e --arg before "$before" --arg after "$digest" --arg revision profile -f assert-early-hold.jq "$audit"

# The same early fault after the policy leaves Automatic for Approval with a
# qualified profile: drain waits for a person because the mode says so.
curl -fsS -H "@$API_HEADER" -H 'Content-Type: application/json' \
  -H "Idempotency-Key: resolve-$id" \
  -d '{"actor":"rehearsal","reason":"Completed the early fault assertion"}' \
  "$API_URL/incidents/$id/resolve" >/dev/null
"${k[@]}" patch "$policy" --type merge -p '{"spec":{"automatic":{"qualifiedProfiles":["kind-nvidia","kind-approval"]}}}'
wait_until policy_loaded kind-approval
before=$digest
"${k[@]}" patch "$policy" --type merge -p '{"spec":{"mode":"Approval","automatic":null}}'
./inject-xid.sh "$final" 1250:3b:00
wait_until incident_opened "$final" 1250:3b:00
audit="$EVIDENCE_DIR/early-approval.json"
parked() { api "incidents/$id" >"$audit" && jq -e '.incident.state=="AWAITING_APPROVAL"' "$audit" >/dev/null; }
wait_until parked
published_since() {
  digest=$("${k[@]}" -n rehearsal get cm rehearsal-runtime -o json | jq -er '.data["config-digest"]') &&
    [[ $digest != "$1" ]] && api runtime-config | jq -e --arg digest "$digest" '.source_digest==$digest' >/dev/null
}
wait_until published_since "$before"
# The loaded revision no longer carries the automatic settings.
"${k[@]}" -n rehearsal get cm rehearsal-runtime -o json | jq -e '.data["policies.yaml"] | contains("kind-approval") | not' >/dev/null
jq -e --arg before "$before" --arg after "$digest" --arg revision approval -f assert-early-hold.jq "$audit"
