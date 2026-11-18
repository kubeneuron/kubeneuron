# Announced cloud interruptions (experimental)

A cloud can announce that it will take a node: a spot instance is reclaimed, or
the host goes into maintenance. KubeNeuron reads these announcements from the
node, opens a `cloud-interruption` incident, asks the node's opted-in jobs to
checkpoint before the deadline the cloud announced, and cordons the node.

This is experimental: it is not validated against a live EKS, AKS or GKE
cluster. Start in DryRun.

KubeNeuron asks the job to checkpoint before the deadline the cloud announced.
It does not save the job's state itself and cannot guarantee that a checkpoint
finishes in time. A spot notice on AWS comes about two minutes before the
instance stops, and other clouds can give less. The signal reaches the node
with its own delay, and KubeNeuron reads nodes every 10 seconds by default
(the controller's reconcile interval), so up to 10 seconds more of the notice
are gone before the job is asked. A job that cannot write a
checkpoint in well under a minute will usually not finish one.

## What KubeNeuron reads

KubeNeuron reads signals that a provider or its documented tooling puts on
the Node object and, on AWS when configured, an
[event queue](#the-aws-interruption-queue). It calls no instance metadata
endpoint.

| Cloud | Signal on the node | Set by | Kind | Deadline |
|---|---|---|---|---|
| GKE | taint `cloud.google.com/impending-node-termination` | GKE, ahead of host maintenance | `host-maintenance` | none given; assumed 30 seconds |
| GKE | label `cloud.google.com/scheduled-maintenance-time` | GKE, for scheduled maintenance | `scheduled-maintenance` | the label's time |
| AKS | condition `VMEventScheduled` `True` | the AKS node problem detector | `scheduled-maintenance` | none given; assumed 30 seconds |
| EKS | taint `aws-node-termination-handler/spot-itn` | AWS Node Termination Handler with `--taint-node` | `spot-reclaim` | none given; assumed 2 minutes |
| EKS | taint `aws-node-termination-handler/rebalance-recommendation` | AWS Node Termination Handler with `--taint-node` | `rebalance` | none |
| EKS | taint `aws-node-termination-handler/scheduled-maintenance` | AWS Node Termination Handler with `--taint-node` | `scheduled-maintenance` | none given; assumed 30 seconds |

EKS managed node groups set no signal of their own. On EKS, configure the
[AWS interruption queue](#the-aws-interruption-queue), or install the AWS
Node Termination Handler in its IMDS mode with `--taint-node`. KubeNeuron does not install, copy or configure the
handler. GKE sets no documented node signal for spot reclaim; a GKE spot
node is not covered.

A signal without a time is assumed due a fixed time after KubeNeuron first
read it, the shortest notice documented for its source, or a stated
assumption where the source documents none:

- AWS spot: 2 minutes. AWS sends the notice two minutes before the instance
  stops, and the handler taints the node when it receives it. The real
  deadline is earlier than assumed by the time the notice took to reach the
  node.
- AKS `VMEventScheduled`: 30 seconds. The condition does not say which
  Azure event is scheduled, and a preempted VM gets at least 30 seconds.
- GKE `impending-node-termination`: 30 seconds. GKE documents no time
  between the taint and the termination; 30 seconds is its default pod grace
  period.
- The handler's `scheduled-maintenance` taint: 30 seconds. The handler
  drains the node as soon as it sets the taint.

When the assumed time leaves no room for the 30-second margin, the jobs are
still asked, with the assumed deadline itself.

## The AWS interruption queue

This is experimental: it is not validated against live AWS events. The event
formats are parsed from the examples in the AWS documentation, read on
2026-11-18.

Amazon EventBridge can deliver the account's EC2 interruption events to an
Amazon SQS queue. KubeNeuron reads that queue when
`spec.cloud.aws.interruptionQueueURL` is set; it is off by default.

```yaml
spec:
  cloud:
    provider: aws
    aws:
      region: us-east-1
      clusterName: training
      interruptionQueueURL: https://sqs.us-east-1.amazonaws.com/123456789012/kubeneuron-interruptions
```

Set up, in the controller's region:

- A standard SQS queue, **dedicated to this KubeNeuron installation**.
  KubeNeuron deletes the messages it reads, including those about instances
  that are not nodes of this cluster. Do not point it at a queue the AWS Node
  Termination Handler, Karpenter or another installation reads: they would
  lose those messages, and KubeNeuron would lose theirs.
- A queue policy that allows `sqs:SendMessage` to the principal
  `events.amazonaws.com`.
- EventBridge rules that send these events to the queue (the same rules the
  AWS Node Termination Handler documents for its queue mode):

  | Event source | Detail type | Kind | Deadline |
  |---|---|---|---|
  | `aws.ec2` | `EC2 Spot Instance Interruption Warning` | `spot-reclaim` | the event's `time` plus 2 minutes (assumed) |
  | `aws.ec2` | `EC2 Instance Rebalance Recommendation` | `rebalance` | none |
  | `aws.health` | `AWS Health Event`, service `EC2`, category `scheduledChange` | `scheduled-maintenance` | the event's `startTime` |
  | `aws.ec2` | `EC2 Instance State-change Notification` | see below | |

- For the controller's role, `sqs:ReceiveMessage` and `sqs:DeleteMessage` on
  this queue only.

The spot event documents no deadline: AWS sends it two minutes before the
instance is interrupted. KubeNeuron assumes the event's `time` plus two
minutes; the real deadline may be earlier.

An instance state change opens no incident. `running` says the instance is
back: it ends the instance's maintenance notices that AWS sent before it.
The queue does not keep order, so an older `running` ends nothing newer, and
it never ends a spot reclaim or a rebalance hint: those end when the node is
removed. The other states are left to the node's removal. A state change
that ends no notice is deleted without a record.

An AWS Health update of an event already read is the same notice. When it
moves `startTime`, the new deadline replaces the recorded one, and the
incident's steps and its hand-over to a person follow the new time; a
warning already sent is not sent again. `statusCode: closed` ends that
event's notice only.

How a message is handled:

- A notice is acknowledged, and its message deleted, only after the notice is
  written to the incident. A message whose notice could not be written comes
  back after five minutes (the visibility timeout used) and is recognised by
  its event ID, or for AWS Health by the event ARN and instance.
- A notice for an instance that is not a node of this cluster is deleted and
  counted as `outcome="foreign"`; it opens nothing. This includes an
  instance that has not joined the cluster as a node yet, such as one that
  receives a rebalance recommendation in its first minute.
- A message that is not one of the events above, or cannot be read, is
  deleted and counted as `kind="unknown"`, `outcome="unrecognised"`.

The controller that leads long-polls the queue (20 seconds per request) and
records a notice as soon as it arrives. The checkpoint warning then starts on
the next reconcile, within 10 seconds by default. With EventBridge's own
delivery time, expect the warning some seconds to tens of seconds after the
event; AWS states no delivery time.

The only change KubeNeuron makes in AWS for this is deleting the messages it
has read from this queue. The long poll runs without pause while the
controller leads; the SQS requests are billed to the account owner at AWS
rates.

## What happens

The cloud pack installs the `kubeneuron-cloud-interruption` playbook and a
policy for the `cloud-interruption` class. The playbook has two steps and no
others.

1. **Checkpoint warning** (`CheckpointWarning`). Every opted-in job on the node
   that checkpoint coordination would ask before a drain is asked to
   checkpoint, with a deadline 30 seconds before the cloud's. The step only
   stamps the request; it does not wait. KubeNeuron then records, by the
   deadline, how many of the asked jobs reported a checkpoint or finished.
   The warning needs [checkpoint coordination](operations.md#checkpoint-coordination-opt-in-v060)
   to be enabled; otherwise the step records that no job was asked.
2. **Cordon**. The node takes no new pods.

KubeNeuron never drains or replaces the node for an interruption: the cloud
takes the instance, and the node group's own scaling replaces it. A
`cloud-interruption` policy whose playbook, or a playbook it escalates to,
has any step other than these two is rejected when the policies load, in
every mode.
On an accelerator node that another repairer is already taking, such as a
node with the `karpenter.sh/disrupted` taint, the cordon is held and recorded
(see [Accelerator health](accelerator-health.md#when-kubeneuron-only-observes));
the checkpoint warning is still sent.

| Mode | Checkpoint warning | Cordon |
|---|---|---|
| `DryRun` | recorded as "would warn" | recorded as "would cordon" |
| `Approval` | sent without approval | waits for a person |
| `Automatic` | sent without approval | runs within `spec.automatic` |

The warning is the one step that runs without approval in `Approval` mode. It
writes annotations on opted-in pods and stops nothing. The circuit breaker,
the concurrency limits and the cooldowns do not hold it, so a mass reclaim
warns every node; they apply to the cordon. A global pause holds both.

An announced interruption is not a fault. It does not count toward the
circuit breaker of a repair policy, so a mass spot reclaim does not stop the
automatic repair of broken nodes. The breaker of the interruption's own cordon
counts only interruptions.

### When it happens

The incident and its notification are created as soon as the notice is read.
The steps start when the deadline is within the policy's lead:

```yaml
spec:
  interruption:
    lead: 10m   # default; from 1m to 60m
```

A spot notice has no time and starts at once. A maintenance window announced
for next week waits until ten minutes before it.

A `rebalance` hint is only recorded: no job is asked and the node is not
cordoned, unless a notice that takes the node follows.

### Other incidents on the node

When the node already has an open incident, the notice is added to that
incident's audit trail. The checkpoint warning is still sent, but the
interruption does not cordon the node and does not touch a replacement that
incident is running.

### Closing

- The node is deleted: the incident is resolved as replaced.
- The notice is no longer present and the node is still there: the
  incident is closed as ended. A notice from the AWS queue is delivered once,
  so its absence does not close the incident. It is closed as ended once
  each of its queued notices is over: an AWS Health event `closed`, a
  maintenance notice followed by `running`, or a rebalance hint that no
  notice taking the node followed within an hour. In every case only the
  cordon this incident placed is removed. KubeNeuron cannot tell a cancelled notice from maintenance that was
  completed in place (a reboot, a frozen VM, a VM restarted under the same
  node), so it does not say which.
- The node and the notice are both still there an hour after the deadline:
  the incident goes to a person (`NEEDS_HUMAN`), and the cordon is kept. The
  signal may be stuck.

## What is recorded

The incident records the kind, the deadline and the source of each notice.
It does not record the cloud's message text. From the AWS queue, KubeNeuron
reads the event's source, detail type, ID and time, the instance ID and state,
and for AWS Health the event ARN, service, category, `startTime`,
`statusCode` and the affected instance IDs. The message body, the Health
`eventDescription` and the event's account ID are not stored or logged. The node is not counted
as degraded: the incident is left out of `kubeneuron_degraded_gpus`,
`kubeneuron_degraded_gpu_seconds_total` and
`kubeneuron_incidents_recovered_total`, and `kubeneuronctl report` counts
announced interruptions apart, with no GPU-hours. When the workloads cannot
be read at the deadline, the outcome is recorded as unknown, not as missed.
The metric
`kubeneuron_cloud_interruptions_total` counts notices and warning outcomes by
`provider`, `kind` and `outcome`; it carries no node, pool or instance type.
