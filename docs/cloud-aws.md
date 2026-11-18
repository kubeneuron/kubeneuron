# Add-first replacement on EKS

**Experimental, not validated against a live EKS cluster.** The AWS provider
currently advertises terminate-first replacement. The add-first path described
here is under development and has been exercised with simulated EKS and Auto
Scaling responses.

## How the provider identifies the extra instance

The plan records the managed node group's ARN, its backing Auto Scaling group's
ARN, the availability zone, the original desired capacity and the EC2 instance
IDs. Planning requires one zone and room below the group's maximum size. In a
multi-zone group the new instance could land outside the zone required by a
training job's volume.

The scaling request sets desired capacity to the recorded value plus one and
uses a client token derived from the operation ID. Before accepting a successor,
the provider checks the EKS update, the group's membership and its scaling
activities. All original instances must still be present. Exactly one additional
instance must be in service, with a matching successful launch in the activity
history.

## When cleanup needs an operator

AWS bills the additional instance while it exists. If the operation is
abandoned, the group can retain the extra capacity until an operator intervenes.
A failed launch can also leave desired capacity elevated, allowing a later
launch when capacity becomes available.

When cleanup requests operator attention, inspect the saved group identities,
the EKS update and the Auto Scaling activity history. Compare current membership
and desired capacity with the saved plan. Check workloads on any candidate node
before deciding whether to remove its EC2 instance. Reducing desired capacity
alone lets Auto Scaling choose the instance to terminate.

For the deployed replacement path and its IAM permissions, see
[Operations](operations.md).
