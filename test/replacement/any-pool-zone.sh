# SPDX-License-Identifier: Apache-2.0
# shellcheck shell=bash
# Sourced by run-scenarios.sh with its Kubernetes and incident helpers.
policy=gpuremediationpolicy/rehearsal-cloud-fell-off-bus
"${k[@]}" patch "$policy" --type=merge -p '{"spec":{"mode":"Approval","automatic":null,"replacement":{"strategy":"AddFirst","successorZone":"AnyPoolZone"}}}'
wait_until policy_loaded AnyPoolZone

# Use the sandbox image already loaded on this kind worker.
pause_image=$(docker exec "$worker" crictl images -o json | jq -er 'first(.images[].repoTags[]? | select(test("/pause:")))')
jq -n --arg image "$pause_image" '{
  apiVersion:"apps/v1",kind:"Deployment",metadata:{name:"zone-bound",namespace:"rehearsal"},
  spec:{replicas:1,selector:{matchLabels:{app:"zone-bound"}},
    template:{metadata:{labels:{app:"zone-bound"}},spec:{
      nodeSelector:{"topology.kubernetes.io/zone":"us-east-1a"},
      containers:[{name:"workload",image:$image,imagePullPolicy:"Never"}]}}}}' | "${k[@]}" apply -f -
"${k[@]}" -n rehearsal rollout status deployment/zone-bound --timeout=90s
test "$("${k[@]}" -n rehearsal get pods -l app=zone-bound -o jsonpath='{.items[0].spec.nodeName}')" = "$worker"
inject "$worker" 124c:3b:00
joined=false
removed=false
last_round=0
deadline=$((SECONDS + 360))
while ((SECONDS < deadline)); do
  api "incidents/$id" >"$audit"
  if [[ $joined == false ]] && jq -e 'any(.audit[]; .action=="successor-created")' "$audit" >/dev/null; then
    successor_id=$(jq -er '.incident.successor.provider_id' "$audit")
    ./join-worker.sh "$successor" "$successor_id"
    joined=true
  fi
  if [[ $removed == false ]] && jq -e 'any(.audit[]; .action=="successor-replaced")' "$audit" >/dev/null; then
    remove_worker "$worker" "$INITIAL_ID"
    removed=true
  fi
  case $(jq -r '.incident.state' "$audit") in
    AWAITING_APPROVAL)
      api "incidents/$id/impact-preview" >"$EVIDENCE_DIR/current-preview.json"
      round=$(jq -er '.round' "$EVIDENCE_DIR/current-preview.json")
      if ((round > last_round)); then
        cp "$EVIDENCE_DIR/current-preview.json" "$EVIDENCE_DIR/preview-$round.json"
        body=$(jq -ce '{actor:"zone-rehearsal",park_epoch:.round,impact_digest:.digest}' "$EVIDENCE_DIR/current-preview.json")
        curl -fsS -H "@$API_HEADER" -H 'Content-Type: application/json' -d "$body" "$API_URL/incidents/$id/approve"
        last_round=$round
      fi ;;
    RESOLVED) break ;;
    NEEDS_HUMAN) echo "Unexpected operator intervention: $audit" >&2; exit 1 ;;
  esac
  sleep 2
done
test "$joined/$removed" = true/true
jq -s -e --arg node "$successor" -f assert-any-zone.jq "$audit" "$EVIDENCE_DIR"/preview-*.json
