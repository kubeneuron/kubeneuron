# SPDX-License-Identifier: Apache-2.0
def entries($action): [.audit[] | select(.action == $action)];
.incident.id as $id
| (if $scenario == "lost-grow" then entries("remove-first-grow-intent")
   elif $scenario == "lost-resize" then entries("remove-first-planned")
   else entries("remove-first-delete-intent") end) as $intent
| entries("remove-first-delete-confirmed") as $confirmed
| entries("recover-confirmed-delete") as $recovered
| entries("remove-first-needs-operator") as $held
| (if $scenario == "identity-changed" then $held
   elif $scenario == "lost-resize" then entries("remove-first-restored")
   else $confirmed end) as $end
| ($intent | length) == 1 and ($end | length) == 1
  and ($intent[0].params.operation | type == "string" and length > 0)
  and $end[0].params.operation == $intent[0].params.operation
  and $end[0].id > $intent[0].id
  and all(.audit[]; .action != "approval-approved")
  and (if $scenario == "identity-changed" then
    .incident.state == "NEEDS_HUMAN" and ($confirmed | length) == 0 and ($recovered | length) == 0
    and (.incident.id | type == "string" and length > 0)
    and ($end[0].result | contains("cannot confirm that it is the original one, so the removal was not repeated."))
    and ($end[0].result | contains("kubeneuronctl resolve " + $id + " --cloud-operation-settled --reason"))
  elif $scenario == "lost-resize" then
    .incident.state == "RESOLVED" and ($held | length) == 0
    and ($end[0].result | endswith(": confirmed by the pool's final state, not by the cloud operation"))
  else ($scenario == "resolved" or $scenario == "lost-grow") and .incident.state == "RESOLVED" and ($held | length) == 0
    and ($scenario != "lost-grow" or (entries("remove-first-delete-intent") | length) == 0)
    and ($recovered | length) == 1 and $recovered[0].id > $confirmed[0].id
    and $recovered[0].actor == "system" and $recovered[0].to_state == "VERIFYING"
  end)
