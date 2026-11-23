# SPDX-License-Identifier: Apache-2.0
def entries($action): [.audit[] | select(.action == $action)];
.incident.id as $id
| entries("remove-first-delete-intent") as $intent
| entries("remove-first-delete-confirmed") as $confirmed
| entries("remove-first-needs-operator") as $held
| (if $scenario == "identity-changed" then $held else $confirmed end) as $end
| ($intent | length) == 1 and ($end | length) == 1
  and ($intent[0].params.operation | type == "string" and length > 0)
  and $end[0].params.operation == $intent[0].params.operation
  and $end[0].id > $intent[0].id
  and all(.audit[]; .action != "approval-approved")
  and (if $scenario == "identity-changed" then
    .incident.state == "NEEDS_HUMAN" and ($confirmed | length) == 0
    and (.incident.id | type == "string" and length > 0)
    and ($end[0].result | contains("cannot confirm that it is the original one, so the removal was not repeated."))
    and ($end[0].result | contains("kubeneuronctl resolve " + $id + " --cloud-operation-settled --reason"))
  else $scenario == "resolved" and .incident.state == "RESOLVED" and ($held | length) == 0
  end)
