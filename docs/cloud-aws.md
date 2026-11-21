# Add-first replacement on EKS

**Experimental.** Yes, once, on pre-release builds: EKS add-first replacement
completed on a live cluster (CPU nodes, synthetic accelerator capacity;
verification depth: node readiness and advertised capacity). The controller
was updated during the incident. Not repeated on v1.0.0-rc.1. It is not
validated on accelerator hardware. Single-zone managed node groups only.

## Declare who controls the group size

Before configuring add-first, confirm that no autoscaler or other automation
manages the group's desired size. If another component manages it, use
`RemoveFirst`. For a group whose size you control, set `CLUSTER` and
`NODEGROUP` to the EKS cluster and managed node group names, then tag the group:

```sh
GROUP_ARN=$(aws eks describe-nodegroup --cluster-name "$CLUSTER" --nodegroup-name "$NODEGROUP" --query 'nodegroup.nodegroupArn' --output text) &&
  aws eks tag-resource --resource-arn "$GROUP_ARN" --tags 'kubeneuron.io/fixed-size=true'
```

The controller reads this declaration from the EKS managed node group with
`eks:DescribeNodegroup` during planning and again before increasing capacity.
Only the exact value `true` qualifies. The identity applying the tag needs
`eks:TagResource`; the controller only reads the tag. Remove the declaration
before handing control of desired size to an autoscaler.

The tag is the operator's declaration. KubeNeuron cannot independently verify
that no other tool changes the group's size. It recognizes Cluster Autoscaler
when a running status report in `kube-system/cluster-autoscaler-status` names
the backing Auto Scaling group and its last health probe is at most five minutes
old. That group is treated as autoscaled regardless of its tag. If the ConfigMap
is absent, the declaration is still required. A status ConfigMap in another
namespace or with a different name is invisible to this check. A stale, incomplete or unreadable
status blocks add-first. Other tools that change the group's size are outside
this check.

Before accepting a successor, KubeNeuron compares observed capacity and group
membership with the saved plan. An unexpected desired size or a missing original
member at that check requires operator inspection before KubeNeuron can terminate
the old instance.

## How the provider identifies the extra instance

The plan records the managed node group's ARN, its backing Auto Scaling group's
ARN, the availability zone, the original desired capacity and the EC2 instance
IDs. Planning requires one zone and room below the group's maximum size. In a
multi-zone group the new instance could land outside the zone required by a
training job's volume or its scheduling constraints.

| Planning refusal | Operator action |
| --- | --- |
| EKS chooses the zone of a new node in this pool | Use a single-zone node pool, or set RemoveFirst in the remediation policy that applies to these nodes. |
| node group is managed by Cluster Autoscaler | Use a single-zone group outside Cluster Autoscaler management, or set RemoveFirst in the remediation policy that applies to these nodes. |
| EKS does not say whether an autoscaler manages this node group | Tag the managed node group with `kubeneuron.io/fixed-size=true` if nothing else changes its size, or select `RemoveFirst` in its remediation policy. |
| Cluster Autoscaler publishes a legacy text status report | Upgrade Cluster Autoscaler to a version that publishes YAML or use RemoveFirst. |
| Could not confirm Cluster Autoscaler status | Check the controller's access to the status ConfigMap and the freshness and completeness of the YAML report. The pool assessment shows `unknown` if the read fails. |
| no room below maxSize | Provide room for one more node, or set RemoveFirst in the remediation policy that applies to these nodes. |


The scaling request sets desired capacity to the recorded value plus one and
uses a client token derived from the operation ID. Before accepting a successor,
the provider checks the EKS update, the group's membership and its scaling
activities. All original instances must still be present. Exactly one additional
instance must be in service, with a matching successful launch in the activity
history. The Auto Scaling group starts the launch some seconds after EKS reports
the size update done, and lists the new instance as pending until it is in
service: KubeNeuron reads the group again in both cases. It hands the
replacement to an operator when no launch activity appears within five minutes
of the request, or when the activity does not match the request.

The controller then accepts the new node when it is Ready and schedulable, has at
least the old node's allocatable accelerator resources and its KubeNeuron agent
has registered since the request. When the old node's agent reported its
devices, the new node's agent must report at least as many. When it reported
none, as on a vendor the agent does not inventory, the health of the new node's
devices is not checked.

## Permissions and preflight

The read-only preflight checks the managed node group and its backing Auto Scaling group. Write permissions are first exercised when requesting growth and terminating an instance.

For AWS installations the operator creates a Role and RoleBinding in
`kube-system` that let the controller read the `cluster-autoscaler-status`
ConfigMap (`get` only). They are removed when the AWS provider is turned off.

| Actions | IAM resource |
| --- | --- |
| `eks:DescribeNodegroup`, `eks:UpdateNodegroupConfig` | Managed node group ARN |
| `eks:ListUpdates`, `eks:DescribeUpdate` for the node group's updates | Cluster ARN and managed node group ARN |
| `autoscaling:DescribeAutoScalingInstances`, `autoscaling:DescribeAutoScalingGroups`, `autoscaling:DescribeScalingActivities` | `*` |
| `ec2:DescribeInstanceStatus` | `*` |
| `autoscaling:TerminateInstanceInAutoScalingGroup` | Backing Auto Scaling group ARN |

See the AWS authorization references for [EKS](https://docs.aws.amazon.com/service-authorization/latest/reference/list_eks.html) and [EC2 Auto Scaling](https://docs.aws.amazon.com/service-authorization/latest/reference/list_autoscaling.html).

After replacement, the last desired size requested through EKS is the original size plus one. Termination decrements the backing group's desired capacity to the original size. EKS can temporarily show the higher value. When updating the node group's scaling configuration, explicitly setting the intended desired size reconciles the two.

## When EKS's own repair holds a replacement

KubeNeuron and EKS node auto repair do not act on the same node group. While
auto repair is enabled, KubeNeuron opens incidents for the group's nodes but
does not replace them. Turning auto repair off is your decision: EKS then
stops repairing nodes in that group, including for conditions KubeNeuron does
not detect. KubeNeuron does not change this setting.

Before replacing a node, and in the background for `kubeneuronctl pools`, the
controller reads the node group's repair setting. A group created without
`nodeRepairConfig` needs no step: the
[EKS API reference](https://docs.aws.amazon.com/eks/latest/APIReference/API_NodeRepairConfig.html)
states that "Node auto repair is disabled by default.", and KubeNeuron reads
the absent setting as off.

With auto repair off, a replacement also waits until the node group is
settled: its EKS update history shows no unfinished update and no update in
the last 30 minutes, its Auto Scaling group has no unfinished activity, and
the pinned instance is running without scheduled events. An update that
changed only the group's size (desired, minimum or maximum) does not open the
30-minute window, so a replacement's own scale-out does not hold the next
replacement in the pool. The group's `modifiedAt` is not used: EKS moves it
without an update.

`kubeneuronctl incidents show` prints the current reason a replacement is
held at the top, and the incident's audit records each new reason. Times are
in UTC, with the date when it is not today's. A pool's row in
`kubeneuronctl pools` shows a hold that needs an operator; a hold that clears
on its own does not change the pool's status. The controller log carries the
AWS error with the incident ID; the incident and the pool row name only the
IAM action.

| Reason code | Meaning | What to do |
| --- | --- | --- |
| `native-repair-on` | Node auto repair is enabled for the group. | Leave repairs of this group to EKS, or turn node auto repair off for the group. The message gives the command. |
| `pool-changed-recently` | An update other than a size change was created less than 30 minutes ago. | Nothing; the message gives the time the check passes. |
| `pool-update-running` | An EKS update of the group is unfinished. | Nothing; checked again every minute. |
| `pool-activity-running` | The group's Auto Scaling group has an unfinished activity. | Nothing; checked again every minute. |
| `pool-changed-during-check` | The repair setting, backing group, launch template, version or update history changed while the controller checked. | Nothing; checked again every minute. |
| `read-failed` | An AWS read was denied or failed; the message names the IAM action. | If denied, allow that action in the controller's IAM role. |
| `repair-unconfirmed` | The answers did not establish that EKS is not repairing the node, for example the instance is not a healthy member of the group. | Inspect the node group and the instance. |

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
