# SPDX-License-Identifier: Apache-2.0
any(.pools[]; .pool == "gpu" and .status == "unavailable"
  and .reason_code == "multi-zone"
  and .reason == "EKS chooses the zone of a new node in this pool"
  and .action == "use a single-zone node pool, set RemoveFirst in the remediation policy that applies to these nodes, or set replacement.successorZone: AnyPoolZone if workloads on these nodes do not need the original zone")
and (.uid | type == "string" and length > 0)
and .node.metadata.uid == .uid
and .node.spec.providerID == "aws:///us-east-1a/i-pinned"
and .node.metadata.labels["topology.kubernetes.io/zone"] == "us-east-1a"
and (.log | contains("zone rehearsal: simulator ready"))
and (.log | test("zone rehearsal: (grow|delete) call") | not)
