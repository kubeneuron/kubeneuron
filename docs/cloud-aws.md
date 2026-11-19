# Add-first replacement on EKS

**Experimental, not validated against a live EKS cluster.** The AWS provider
currently advertises remove-first replacement. The add-first path described
here is under development and has been exercised with simulated EKS and Auto
Scaling responses.

## How the provider identifies the extra instance

The plan records the managed node group's ARN, its backing Auto Scaling group's
ARN, the availability zone, the original desired capacity and the EC2 instance
IDs. Planning requires one zone and room below the group's maximum size. In a
multi-zone group the new instance could land outside the zone required by a
training job's volume.

| Planning refusal | Operator action |
| --- | --- |
| node group spans several zones | Use a single-zone managed node group, or set RemoveFirst in the remediation policy that applies to these nodes. |
| node group is managed by Cluster Autoscaler | Use a single-zone group outside Cluster Autoscaler management, or set RemoveFirst in the remediation policy that applies to these nodes. |
| no room below maxSize | Provide room for one more node, or set RemoveFirst in the remediation policy that applies to these nodes. |


The scaling request sets desired capacity to the recorded value plus one and
uses a client token derived from the operation ID. Before accepting a successor,
the provider checks the EKS update, the group's membership and its scaling
activities. All original instances must still be present. Exactly one additional
instance must be in service, with a matching successful launch in the activity
history.

## Permissions and preflight

The read-only preflight checks the managed node group and its backing Auto Scaling group. Write permissions are first exercised when requesting growth and terminating an instance.

| Actions | IAM resource |
| --- | --- |
| `eks:DescribeNodegroup`, `eks:UpdateNodegroupConfig`, `eks:DescribeUpdate` for the node group update | Managed node group ARN |
| `autoscaling:DescribeAutoScalingInstances`, `autoscaling:DescribeAutoScalingGroups`, `autoscaling:DescribeScalingActivities` | `*` |
| `autoscaling:TerminateInstanceInAutoScalingGroup` | Backing Auto Scaling group ARN |

See the AWS authorization references for [EKS](https://docs.aws.amazon.com/service-authorization/latest/reference/list_eks.html) and [EC2 Auto Scaling](https://docs.aws.amazon.com/service-authorization/latest/reference/list_autoscaling.html).

After replacement, the last desired size requested through EKS is the original size plus one. Termination decrements the backing group's desired capacity to the original size. EKS can temporarily show the higher value. When updating the node group's scaling configuration, explicitly setting the intended desired size reconciles the two.

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

The [EKS UpdateNodegroupConfig API reference](https://docs.aws.amazon.com/eks/latest/APIReference/API_UpdateNodegroupConfig.html)
documents idempotency for a client request token and gives no retention period
for this operation. When the original answer is lost and the pinned group already
shows the requested size, the provider reuses the saved token and absolute size.
It requires the returned update to predate or coincide with the retry and to
report the requested desired size. A conflicting update requires inspection.
