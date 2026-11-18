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

KubeNeuron reads only signals that a provider or its documented tooling puts
on the Node object. It calls no instance metadata endpoint.

| Cloud | Signal on the node | Set by | Kind | Deadline |
|---|---|---|---|---|
| GKE | taint `cloud.google.com/impending-node-termination` | GKE, ahead of host maintenance | `host-maintenance` | none given; assumed 30 seconds |
| GKE | label `cloud.google.com/scheduled-maintenance-time` | GKE, for scheduled maintenance | `scheduled-maintenance` | the label's time |
| AKS | condition `VMEventScheduled` `True` | the AKS node problem detector | `scheduled-maintenance` | none given; assumed 30 seconds |
| EKS | taint `aws-node-termination-handler/spot-itn` | AWS Node Termination Handler with `--taint-node` | `spot-reclaim` | none given; assumed 2 minutes |
| EKS | taint `aws-node-termination-handler/rebalance-recommendation` | AWS Node Termination Handler with `--taint-node` | `rebalance` | none |
| EKS | taint `aws-node-termination-handler/scheduled-maintenance` | AWS Node Termination Handler with `--taint-node` | `scheduled-maintenance` | none given; assumed 30 seconds |

EKS managed node groups set no signal of their own. On EKS, install the AWS
Node Termination Handler in its IMDS mode with `--taint-node`, or wait for the
queue-based reader. KubeNeuron does not install, copy or configure the
handler. GKE sets no documented node signal for spot reclaim; a GKE spot
node is not covered.

A signal without a time is assumed due a fixed time after KubeNeuron first
read it, the shortest notice documented for its source:

- AWS spot: 2 minutes. AWS sends the notice two minutes before the instance
  stops, and the handler taints the node when it receives it.
- AKS `VMEventScheduled`: 30 seconds. The condition does not say which
  Azure event is scheduled, and a preempted VM gets at least 30 seconds.
- GKE `impending-node-termination`: 30 seconds. GKE documents no time
  between the taint and the termination; 30 seconds is its default pod grace
  period.
- The handler's `scheduled-maintenance` taint: 30 seconds. The handler
  drains the node as soon as it sets the taint.

When the assumed time leaves no room for the 30-second margin, the jobs are
still asked, with the assumed deadline itself.

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
takes the instance, and the node group's own scaling replaces it.

| Mode | Checkpoint warning | Cordon |
|---|---|---|
| `DryRun` | recorded as "would warn" | recorded as "would cordon" |
| `Approval` | sent without approval | waits for a person |
| `Automatic` | sent without approval | runs within `spec.automatic` |

The warning is the one step that runs without approval in `Approval` mode. It
writes annotations on opted-in pods and stops nothing. The circuit breaker,
the concurrency limits and the cooldowns do not hold it, so a mass reclaim
warns every node; they apply to the cordon. A global pause holds both.

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
  incident is closed as ended, and only the cordon this incident placed is
  removed. KubeNeuron cannot tell a cancelled notice from maintenance that was
  completed in place (a reboot, a frozen VM, a VM restarted under the same
  node), so it does not say which.
- The node and the notice are both still there an hour after the deadline:
  the incident goes to a person (`NEEDS_HUMAN`), and the cordon is kept. The
  signal may be stuck.

## What is recorded

The incident records the kind, the deadline and the source of each notice.
It does not record the cloud's message text. When the workloads cannot
be read at the deadline, the outcome is recorded as unknown, not as missed.
The metric
`kubeneuron_cloud_interruptions_total` counts notices and warning outcomes by
`provider`, `kind` and `outcome`; it carries no node, pool or instance type.
