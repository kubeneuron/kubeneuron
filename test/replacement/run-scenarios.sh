#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
set -euo pipefail
: "${CLUSTER_NAME:?}" "${KUBECONFIG:?}" "${AGENT_IMAGE:?}" "${API_URL:?}" "${API_HEADER:?}" "${EVIDENCE_DIR:?}"
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
remove_worker() {
  test "$("${k[@]}" get node "$1" -o jsonpath='{.spec.providerID}')" = "aws:///az/$2"
  test "$(docker inspect -f '{{index .Config.Labels "io.x-k8s.kind.cluster"}}' "$1")" = "$CLUSTER_NAME"
  docker rm -f "$1"
  "${k[@]}" delete node "$1"
}
resolved() {
  wait_until audit_matches '.incident.state=="RESOLVED"'
  jq -e --arg node "$1" --arg correlation "$2" -f assert-resolution.jq "$audit"
}
policy_loaded() {
  local digest
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
inject "$worker" 124c:3b:00
wait_until audit_matches '.incident.successor.provider_id=="aws:///az/i-new" and any(.audit[];.action=="successor-created")'
./join-worker.sh successor i-new
wait_until audit_matches 'any(.audit[];.action=="successor-replaced")'
remove_worker "$worker" i-pinned
resolved "$successor" successor
"${k[@]}" patch gpuremediationpolicy rehearsal-cloud-fell-off-bus --type merge -p '{"spec":{"replacement":{"strategy":"RemoveFirst"}}}'
audit="$EVIDENCE_DIR/B.json"
wait_until pool_ready mixed
wait_until policy_loaded "strategy: remove-first"
inject "$successor" 124d:3b:00
wait_until audit_matches 'any(.audit[];.action=="replace" and (.result//""|startswith("terminated the instance behind ")))'
remove_worker "$successor" i-new
./join-worker.sh final i-final
wait_until report_ready "$final"
resolved "$final" pool

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
"${k[@]}" patch "$policy" --type merge -p '{"spec":{"automatic":{"qualifiedProfiles":["kind-early-missing"]}}}'
./inject-xid.sh "$final" 124f:3b:00
wait_until incident_opened "$final" 124f:3b:00
audit="$EVIDENCE_DIR/early-xid.json"
wait_until missing_profile_hold
jq -e '[.audit[] | select(.action=="recovery-held" and
  (.result | contains("the controller has not yet loaded the published configuration"))) | .time] as $holds |
  any(.audit[]; .to_state=="AWAITING_APPROVAL" and .time > $holds[0]) and ($holds | length)>0' "$audit"
