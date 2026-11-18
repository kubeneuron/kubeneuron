# Replacing GPU nodes on GKE

**Experimental, not validated against a live GKE cluster; start in DryRun.**
The GCP provider has been tested only against a simulated GKE and Compute
Engine API. Start in KubeNeuron's `DryRun` mode and move a narrow node
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
- `RemoveFirst` is available only on a pool under the **cluster
  autoscaler**. The playbook has already cordoned and drained the node, since
  GKE does neither; KubeNeuron deletes the node's instance from the pool's
  instance group and the autoscaler decides whether a new one comes.
- A **fixed-size pool** is replaced only by adding first: under
  `RemoveFirst` the step is refused before anything is deleted, with the
  reason "a fixed-size GKE node pool is replaced only by adding first".
  Setting such a pool back to its size would take a second call after the
  delete, and a stop between the two would leave the pool a node short with
  nothing to show it. A **fixed-size pool across several zones** therefore
  has no replacement path in this release: recovery is unavailable, with the
  reason, before the node is cordoned.
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
2. Create a custom role with these permissions and grant it, on the
   project, to the controller's ServiceAccount (`<installation>-controller`
   in the installation namespace) as a Workload Identity Federation
   principal:

    ```yaml
    title: KubeNeuron GKE node replacement
    includedPermissions:
      - container.clusters.get
      - container.clusters.update        # grow a fixed-size pool (add-first)
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
    Add-first, and so any fixed-size pool, needs it.

3. Configure the KubeNeuron resource:

    ```yaml
    spec:
      cloud:
        provider: gcp
        gcp:
          project: ml-project
          location: us-central1      # the cluster's region or zone
          clusterName: training
    ```

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

## What needs a live GKE cluster

These are open until a run on a real cluster:

- that deleting a member of a node pool's instance group is accepted by GKE
  and lowers the pool by one, without GKE restoring it;
- when the instance group lists the new member of a grown pool, and the
  error codes of a member that cannot be created for lack of capacity;
- the `targetLink` forms of node pool and auto-repair operations;
- that auto-repair leaves Ready nodes with a GPU fault alone;
- the node label and VM field of host maintenance;
- that the three APIs accept the controller's Workload Identity Federation
  principal with the custom role alone.
