# Replacing GPU nodes on GKE

**Experimental; start in DryRun.** Three times. Twice on pre-release builds, and once on 25 November 2026, on images built from commit 3fe83723 — the code released as v1.0.0-rc.2 (the commit after it changes only the changelog, the documentation and the cert-manager example manifest): GKE Standard add-first replacement completed on a live cluster (fixed-size single-zone node pool, CPU nodes; in the last two runs under an Automatic policy, without manual approvals; synthetic accelerator capacity; verification depth: node readiness and advertised capacity; no GPU was present and device health on the new node was not checked).
Three times. Twice on pre-release builds, and once on 25 November 2026, on images built from commit 3fe83723 — the code released as v1.0.0-rc.2 (the commit after it changes only the changelog, the documentation and the cert-manager example manifest): GKE Standard remove-first replacement on a fixed-size pool completed on a live cluster, with the pool's size restored (in the last two runs under an Automatic policy, without manual approvals; CPU nodes, synthetic accelerator capacity; verification depth: node readiness and advertised capacity; no GPU was present and device health was not checked).
On accelerator hardware, remove-first was run once, on 27 November 2026, on images built from the same commit: on a node with one Tesla T4 (n1-standard-4, Container-Optimized OS, NVIDIA driver and device plugin installed by GKE) it completed under an Automatic policy, without manual approvals. The test reported the fault as one XID 79 kernel-log line while the GPU was present. The agent saw no GPU on Container-Optimized OS, so the new node was verified by readiness and advertised capacity, and device health was not checked. Add-first was not run on accelerator hardware.
Remove-first on autoscaled pools, multi-zone pools and Autopilot
(observation-only) have been tested only against a simulated GKE and Compute
Engine API. Apart from that one remove-first run on a Tesla T4, nothing here is validated on accelerator hardware. Start in KubeNeuron's `DryRun` mode and move a narrow node
selector to live recovery only after you have watched it decide on your own
cluster.

!!! warning "Replacement deletes a billed instance"
    A `ReplaceNode` step deletes the virtual machine behind the node from its
    GKE node pool. The deletion cannot be undone, and a replacement may not
    appear: the zone can run out of capacity or your project out of quota.
    With add-first, KubeNeuron first grows the pool by one node, which Google
    Cloud bills while it exists. If the operation is abandoned part-way, that
    extra node can remain until a person removes it. You, the operator, are
    responsible for the instances this step deletes and for the capacity it
    leaves.

!!! note "A Compute Engine call GKE does not document for nodes"
    KubeNeuron deletes a node's VM with the Compute Engine
    `instanceGroupManagers.deleteInstances` call on the node pool's managed
    instance group. The call is documented by Compute Engine, but GKE does not
    document using it on the instance group of a node pool. Every change to a
    pool's size goes through the GKE API.

When a replacement fails for lack of capacity, check the incident for the provider's reported cause and a suggested action. If the error code leaves capacity and quota ambiguous, check the account quota for the machine type, then the provider's status for the zone.

## What KubeNeuron does on GKE

GKE documents no restart of a single node, so on GKE:

- `RecycleNode` is never available. A ladder goes straight to replacement.
- **Autopilot** clusters are observe-only: GKE manages their nodes, and
  KubeNeuron detects and reports but never cordons, drains or replaces.
- With the default `replacement.strategy: AddFirst`, on a **fixed-size node
  pool in one zone**, the new node comes up before the old one is cordoned;
  see [Add-first on GKE](#add-first-on-gke). On a pool under the cluster
  autoscaler, or one that spans several zones, add-first is unavailable and
  the incident is held before the cordon, with the reason: GKE sets a size in
  every zone of a pool at once.
  Multi-zone GKE Standard pools require `RemoveFirst`. Set it in the remediation
  policy that applies to these nodes. With an AddFirst policy, the pools API and the ADD-FIRST
  column of `kubeneuronctl pools` show the restriction before an incident.
  On GKE, `AnyPoolZone` behaves like `SameZone`: add-first works on a fixed-size
  single-zone pool, and a multi-zone pool requires remove-first.
- Under `RemoveFirst`, on a pool under the **cluster autoscaler**, the
  playbook has already cordoned and drained the node, since GKE does
  neither; KubeNeuron deletes the node's instance from the pool's instance
  group and the autoscaler decides whether a new one comes.
- Under `RemoveFirst`, on a **fixed-size pool**, KubeNeuron deletes the
  instance and then sets the pool back to its size through a recorded plan;
  see [Remove-first on a fixed-size pool](#remove-first-on-a-fixed-size-pool).
  A fixed-size pool across several zones is replaced this way only while
  every zone has the same size; otherwise recovery is unavailable before the
  node is cordoned, with the reason.
- KubeNeuron never lowers a pool's size through GKE: GKE would choose which
  node goes. Only a named instance is ever deleted.
- No replacement starts while the pool has a GKE operation running (an
  upgrade, a resize, an auto-repair).
- A node is recognized only if its VM carries the GKE labels of the
  configured cluster and is a member of the managed instance group that GKE
  lists for that node pool. Anything else is refused.

### Add-first on GKE

The phases are those of [Add-first replacement](playbook-authoring.md#add-first-replacement):

1. **Plan.** KubeNeuron reads the pool's instance group: its size, members,
   and the resize operations GKE already lists for the pool. Nothing changes
   in Google Cloud.
2. **Request.** If the pool is still as planned and has no operation
   running, KubeNeuron sets its size to one more through GKE and records the
   GKE operation's name in the incident. If the answer is lost, the retry
   finds the resize the plan had not seen and does not send another. That
   resize is taken for KubeNeuron's only if it started after the request
   went out (allowing a minute for the clocks) and grew the pool by exactly
   one; a resize someone else made in the meantime ends the step as
   add-first unavailable. Two such resizes cannot be told apart and need an
   operator.
3. **Created.** Once the instance group lists exactly one member that was
   not there at the plan, and it is running, that member is the successor.
   A member that fails to come up for lack of capacity or quota ends the step
   as "replacement capacity unavailable". More than one new member needs an
   operator.
4. **Replace.** After the successor's node is ready and the old node is
   drained, KubeNeuron deletes only the old instance, which brings the pool
   back to its size.

When the replacement is abandoned, KubeNeuron deletes only the successor (if
nothing but DaemonSet pods runs on it), by name, whether it runs or is still
being created: a zone out of GPUs keeps retrying the new member until it is
deleted. With no new member listed yet while the resize is still running, or
when the pool holds members it cannot account for, it changes nothing: that
needs an operator. A delete whose result is unknown also needs an operator,
and the successor is kept.

KubeNeuron writes nothing into Google Cloud to mark its operation: no label,
description or metadata. A delete carries a request ID derived from the
operation, so a resent request is the same request.

If the pool changes between the plan and the request, for instance while the
step waits a long time for approval, KubeNeuron does not grow it: the step
ends as add-first unavailable, and the replacement is planned again on the
next attempt.

### Remove-first on a fixed-size pool

Experimental. Three times. Twice on pre-release builds, and once on 25 November 2026, on images built from commit 3fe83723 — the code released as v1.0.0-rc.2 (the commit after it changes only the changelog, the documentation and the cert-manager example manifest): GKE Standard remove-first replacement on a fixed-size pool completed on a live cluster, with the pool's size restored (in the last two runs under an Automatic policy, without manual approvals; CPU nodes, synthetic accelerator capacity; verification depth: node readiness and advertised capacity; no GPU was present and device health was not checked). On a fixed-size
pool, deleting a node's instance leaves the pool one node short until its
size is set back, which takes a second call. KubeNeuron keeps a plan of the
replacement on the incident, so a controller that stops between the two
calls, or a call whose answer is lost, still ends with the size set back, or with the plan as needs-operator and the command to run:

1. **Planned.** KubeNeuron reads the pool: the node count of every zone,
   which must be the same in all of them. The plan, with that count, is
   recorded on the incident before anything is changed. No plan starts while
   the pool has a GKE operation running, and only one plan is open per pool:
   a second incident on the same pool waits in its replace step until the
   first one has set the pool back.
2. **Deleting.** KubeNeuron checks the node and the pool once more, records
   that the delete is about to be sent, and deletes the instance by name. A
   resent delete carries the same request ID, so Compute Engine runs it
   once.
3. **Resizing.** Once the instance is gone, KubeNeuron records that the
   resize is about to be sent, then sets the pool's node count back to the
   planned count through GKE (`setSize`), only while the node's zone is
   below it, every other zone is at it, and no other GKE operation runs on
   the pool. The count is absolute, so a resent resize is the same resize,
   and no zone is ever lowered. The name and location of the GKE operation
   are recorded on the incident. A recorded resize is never sent again: if
   its operation ended with an error, or finished with the pool still short,
   the plan ends as needs-operator.
   Setting the size back asks Google Cloud for a new instance; it does not guarantee one. If the zone has no capacity or quota for the machine type, the pool stays a node short until Google Cloud can create it. Add-first avoids deleting a node before its replacement exists.
4. **Done.** The plan ends when every zone is back at its count. If the
   recorded operation could not be read, or its name was never received,
   the plan ends on the zone sizes alone, and the audit trail says
   `confirmed by the pool's final state, not by the cloud operation`.

When the node's zonal group has up to 100 instances, the plan also records
them before the delete. If exactly one instance is new once the
resize is done, the resolution names it (`replacement_correlation:
resize-diff`); otherwise the new node is found as the only one that joined
the pool (`pool`). This uses the `listManagedInstances` read KubeNeuron
already makes, so no new permission is needed. It has been exercised with
simulated providers only.

The replace step ends once the resize is sent; the controller finishes the
plan on its own, also after a restart or a change of leader, from what the
plan recorded. A resize not yet sent when the controller stopped is a new
change: the controller sends it only if the policy would start the replace
step now. While remediation is paused, the policy is in DryRun, or the
published configuration is not loaded, nothing is sent. The incident's audit
trail, `kubeneuronctl status` and `kubeneuronctl pools` show the pool's node
count, read from GKE, and what the resize waits for, for example:

```text
pool gpu: 3 of 4 nodes — replacement of gke-training-gpu-a-0001 is waiting for remediation to be resumed (incident inc-1)
```

If the step is interrupted after the node is removed (for example by a
controller restart), the replace step is not started again and no approval
can be requested for it. Under a policy in Approval mode, the incident needs
an operator at once: set the size of the
pool back, then run `kubeneuronctl resolve <id> --cloud-operation-settled`.
`kubeneuronctl status` and `kubeneuronctl pools` show, for example:

```text
pool gpu: 3 of 4 nodes — replacement of gke-training-gpu-a-0001 needs the operator (incident inc-1)
```

If someone sets the size back in that time, the plan ends on the zone sizes.
After an hour the incident needs an operator. A plan that stopped before the delete changed nothing and is
made again. If the pool changed in a way KubeNeuron cannot set back safely
(its zones now differ in size, it was put under the autoscaler, its zones
changed), or the plan has not finished within an hour, also while nothing fails, the plan ends as
needs-operator. The audit trail explains what to check. If the pool no longer
exists, check the cluster. If the original machine cannot be identified,
check the machine and the size of the pool. Restore the planned size if the
pool is one node short, unless the pool is now autoscaled; then only check its
size. To set the size back, for example:

```sh
gcloud container clusters resize <cluster> --node-pool <pool> --num-nodes <count> --location <location>
```

The incident gives the total for the pool; for a multi-zone pool divide it by the number of zones.

After checking the outcome, record what you found:

```sh
kubeneuronctl resolve <id> --cloud-operation-settled --reason "<what you found>"
```

### GKE node auto-repair and host maintenance

On GKE Standard, node auto-repair acts on a node that is NotReady, reports no
status, or has run out of boot disk space, each for some minutes; GKE
documents no trigger for a GPU fault on a node that stays Ready
([node auto-repair](https://cloud.google.com/kubernetes-engine/docs/how-to/node-auto-repair)).
On a pool with auto-repair, KubeNeuron runs in the `not-ready-only` mode,
shown on the KubeNeuron resource's `CloudRecoveryAvailable` condition (reason
`NotReadyOnlyRepair`). Recovery runs only when the node is Ready before and
after every check and no auto-repair (`AUTO_REPAIR_NODES`) or other GKE
operation is running on its pool or cluster. A pool with auto-repair turned
off runs with no such restriction beyond the running-operation check.

While the node's host is under or due for maintenance (the
`cloud.google.com/active-node-maintenance` label, or upcoming maintenance on
the VM), the incident records it and no recovery step starts.

## Setup

1. Enable
   [Workload Identity Federation for GKE](https://cloud.google.com/kubernetes-engine/docs/how-to/workload-identity)
   on the cluster and the controller's node pool. The controller uses
   Application Default Credentials only, which on GKE are its federated
   token from the metadata server. Do not give it a service account key.
   Without credentials the controller still runs, but refuses every
   replacement and names the cause.
2. Enable, in the cluster's project, the Google APIs the controller calls:

    ```sh
    gcloud services enable container.googleapis.com compute.googleapis.com \
      cloudresourcemanager.googleapis.com --project=<project>
    ```

    A new project has the Cloud Resource Manager API off. KubeNeuron needs it
    to check its permissions before it changes anything; while it is off,
    every replacement holds and names the API to enable.

3. Create a custom role with these permissions and grant it, on the
   project, to the controller's ServiceAccount (`<installation>-controller`
   in the installation namespace) as a Workload Identity Federation
   principal:

    ```yaml
    title: KubeNeuron GKE node replacement
    includedPermissions:
      - container.clusters.get
      - container.clusters.update        # set a fixed-size pool's size (add-first, remove-first)
      - container.operations.list
      - container.operations.get
      - compute.instances.get
      - compute.instanceGroupManagers.get
      - compute.instanceGroupManagers.update
    ```

    ```sh
    gcloud iam roles create kubeneuronGkeReplacement --project=<project> --file=role.yaml
    gcloud projects add-iam-policy-binding <project> \
      --role=projects/<project>/roles/kubeneuronGkeReplacement \
      --member="principal://iam.googleapis.com/projects/<project-number>/locations/global/workloadIdentityPools/<project>.svc.id.goog/subject/ns/<namespace>/sa/<installation>-controller"
    ```

    If KubeNeuron only replaces nodes of pools under the cluster autoscaler,
    leave out `container.clusters.update`: those pools' sizes are never set.
    Any fixed-size pool needs it, with either strategy.

4. Configure the KubeNeuron resource:

    ```yaml
    spec:
      cloud:
        provider: gcp
        gcp:
          project: ml-project
          location: us-central1      # the cluster's region or zone
          clusterName: training
    ```

5. Optionally, to read TPU health from Google Cloud Monitoring
   ([what is read](accelerator-health.md#tpu-health-from-google-cloud-monitoring-opt-in);
   experimental, the metric is a Google Cloud Beta feature), set
   `tpuInfraHealth: {enabled: true}` under `gcp`, and grant the controller's
   principal, in each project of the TPU nodes, a custom role with the one
   permission `monitoring.timeSeries.list`. `roles/monitoring.viewer` also
   works, but it is wider: it gives read access to every metric of the
   project. Without the permission the TPU nodes stay unknown and nothing
   opens. Cloud Monitoring API read calls are billed to the project owner at
   Google Cloud rates.

## When a pool needs an operator

KubeNeuron stops and reports "needs an operator" rather than guess. The
incident (`kubeneuronctl incident show`) names the operation, the successor
instance, the GKE operation that grew the pool and why it stopped. Read the
pool:

```sh
gcloud container node-pools describe <pool> --cluster <cluster> --location <location> \
  --format="value(instanceGroupUrls)"
gcloud compute instance-groups managed list-instances <group> --zone <zone>
gcloud container operations list --location <location> \
  --filter="targetLink~nodePools/<pool> AND status!=DONE"
```

Wait for any running operation to finish. Delete the successor or the old
instance as needed (`gcloud compute instance-groups managed delete-instances
<group> --zone <zone> --instances <name>`), or set the pool to the size you
want (`gcloud container clusters resize <cluster> --node-pool <pool>
--num-nodes <n> --location <location>`; lowering it lets GKE choose which node
goes). Then resolve the incident, and run the KubeNeuron step again if the
node still needs replacing.

## Limits of the checks

Before any change, KubeNeuron asks Cloud Resource Manager
(`projects.testIamPermissions`) which of the permissions above the controller
holds on the project. Grants on narrower resources, and deny policies, are
not seen there: a call can still be refused after the pool has been grown.

`kubeneuronctl pools` runs the same check, so a GKE pool shows as available
only once these permissions are confirmed. A pool whose check failed shows
the disabled API or the missing permission, and what to do; the Google
error itself goes to the controller log only, as it names the project.

## Deleting the cluster

Deleting a GKE cluster does not delete the persistent disks behind
KubeNeuron's volume claims (the SQLite state and the metrics store).
Uninstall with `--uninstall --delete-data` first, or list the leftover disks
afterwards (`gcloud compute disks list --filter="name~^pvc-"`) and delete
them.

## What needs a live GKE cluster

One live run completed add-first and remove-first replacement on a fixed-size
single-zone Standard node pool (see the top of this page). That run did not
examine the points below one by one, so they stay open, as do remove-first
on autoscaled pools and multi-zone pools:

- that deleting a member of a node pool's instance group is accepted by GKE
  and lowers the pool by one, without GKE restoring it;
- when the instance group lists the new member of a grown pool, and the
  error codes of a member that cannot be created for lack of capacity;
- the `targetLink` forms of node pool and auto-repair operations;
- that auto-repair leaves Ready nodes with a GPU fault alone;
- the node label and VM field of host maintenance;
- that the three APIs accept the controller's Workload Identity Federation
  principal with the custom role alone.
