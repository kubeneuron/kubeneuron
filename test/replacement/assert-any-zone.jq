# SPDX-License-Identifier: Apache-2.0
def preview_warning: "The replacement may join another zone of this pool. 1 pod on this node is tied to zone us-east-1a by persistent volumes or required scheduling rules and may not start in another zone.";
def resolution_warning: "The replacement joined zone us-east-1b; the removed node was in zone us-east-1a. 1 pod on the removed node was tied to zone us-east-1a and may not start in zone us-east-1b.";
.[0] as $detail
| $detail.incident.state == "RESOLVED"
and any(.[1:][]; .incident == $detail.incident.id and .version == "impact-preview/v1"
  and .unavailable == null and any(.warnings[]?; . == preview_warning))
and any($detail.audit[]; .action == "resolve" and .actor == "system"
  and .params.replacement_node == $node and (.result | contains(resolution_warning)))
