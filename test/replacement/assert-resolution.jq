# SPDX-License-Identifier: Apache-2.0
.incident.state == "RESOLVED"
and any(.audit[]; .action == "policy-authorised")
and all(.audit[]; .action != "approval-approved" and .to_state != "AWAITING_APPROVAL")
and any(.audit[]; .action == "replaced-node-devices" and .params.device_count == "1")
and any(.audit[]; .action == "resolve" and .actor == "system" and .dry_run == false
  and .params.replacement_node == $node and .params.replacement_correlation == $correlation
  and .params.infrastructure_evidence == "verified_replacement_node"
  and (.result | endswith("Accelerator inventory and driver probe observed; DCGM not available to the agent, device health not checked.")))
