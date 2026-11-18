# Replacing GPU nodes on AKS

**Experimental, not validated against a live AKS cluster.** The Azure
provider has been tested only against a simulated AKS API. Start in
KubeNeuron's `DryRun` mode and move a narrow node selector to live recovery
only after you have watched it decide on your own cluster.

!!! warning "Replacement deletes a billed instance"
    A `ReplaceNode` step deletes the virtual machine behind the node from its
    AKS node pool. The deletion cannot be undone, and a replacement may not
    appear: the region can run out of capacity or your subscription out of
    quota. On a fixed-size pool KubeNeuron first grows the pool by one node,
    which Azure bills while it exists. If the operation is abandoned
    part-way, that extra node can remain until a person removes it. You, the
    operator, are responsible for the instances this step deletes and for the
    capacity it leaves.

## What KubeNeuron does on AKS

AKS has no supported way to restart a single node: rebooting, deallocating or
reimaging a scale set instance directly leaves the cluster unsupported. So on
AKS:

- `RecycleNode` is never available. A ladder goes straight to replacement.
- `ReplaceNode` uses only the AKS agent pool API. The playbook has already
  cordoned and drained the node, since AKS does neither.
    - On a **fixed-size pool**, KubeNeuron grows the pool by one, waits until
      AKS lists the new machine and the pool has settled, and only then
      deletes the node's machine. If there is no capacity or quota for the
      new machine, the step fails as "replacement capacity unavailable" and
      nothing is deleted. Capacity is added before the old instance is
      deleted, but after the drain: workloads are already evicted while the
      new node comes up. Playbooks that require add-first replacement still
      hold.
    - On a pool under the **cluster autoscaler**, the pool's size is left
      alone. The machine is deleted and the autoscaler decides whether a new
      one comes.
- One replacement runs per pool at a time. KubeNeuron marks a pool it has
  grown with the `kubeneuron-replacing` tag. A retry continues the same
  replacement and never grows the pool twice. A pool left a node larger, or
  a tag left behind, is reported as needing an operator and is never shrunk
  automatically: shrinking would delete a node AKS chooses.
- Each write to the pool carries its version (`If-Match`), so it never
  overwrites a change made after KubeNeuron read the pool.
- A node is recognized only if it is a machine of an agent pool of the
  configured cluster: its scale set instance must be in the cluster's node
  resource group and listed by AKS in that pool. Anything else is refused.

### AKS node auto-repair

AKS node auto-repair cannot be turned off. It acts on nodes that are
NotReady. A GPU fault on a node that stays Ready is not something it repairs.
KubeNeuron therefore runs in the `not-ready-only` mode, shown on the
KubeNeuron resource's `CloudRecoveryAvailable` condition (reason
`NotReadyOnlyRepair`). Recovery runs only when the node is Ready before and
after every check, no `aks-auto-repair` event on the node in the last hour
has been seen, the node's events could be read in full and the pool has no
operation running. Otherwise recovery holds. KubeNeuron does not use AKS
system labels, taints or annotations to keep auto-repair away from a node.

This rests on the AKS documentation and still has to be shown on a live
cluster.

## Setup

1. Create a user-assigned managed identity and federate it with the
   controller's ServiceAccount (`<installation>-controller` in the
   installation namespace) through
   [Azure Workload Identity](https://learn.microsoft.com/azure/aks/workload-identity-overview).
   The cluster needs the OIDC issuer and workload identity enabled.
2. Create a custom role with exactly these actions and assign it to the
   identity, scoped to the AKS cluster resource:

    ```json
    {
      "Name": "KubeNeuron AKS node replacement",
      "Actions": [
        "Microsoft.ContainerService/managedClusters/read",
        "Microsoft.ContainerService/managedClusters/agentPools/read",
        "Microsoft.ContainerService/managedClusters/agentPools/write",
        "Microsoft.ContainerService/managedClusters/agentPools/deleteMachines/action",
        "Microsoft.ContainerService/managedClusters/agentPools/machines/read"
      ],
      "AssignableScopes": ["/subscriptions/<subscription-id>/resourceGroups/<cluster-resource-group>"]
    }
    ```

    `agentPools/write` changes a pool's size and tags. Nothing is granted on
    the scale sets or virtual machines themselves.

3. Configure the KubeNeuron resource:

    ```yaml
    spec:
      cloud:
        provider: azure
        azure:
          subscriptionID: 11111111-2222-3333-4444-555555555555
          resourceGroup: ml            # the cluster resource's group, not MC_...
          clusterName: training
          clientID: 66666666-7777-8888-9999-000000000000   # the managed identity
          # tenantID: only when the identity is in another tenant
    ```

    The operator puts the client ID on the controller ServiceAccount
    (`azure.workload.identity/client-id`) and labels the controller pod for
    the workload identity webhook. No secret is read: the controller
    authenticates only with the projected workload identity token.

4. The operator grants the controller `list` on events in the `default`
   namespace, where Kubernetes records node events, and only for the Azure
   provider. The controller reads the source, reason and time of the node's
   events, never their message.

## Limits of the checks

Before any change, KubeNeuron reads the controller's effective permissions on
the node's pool and requires every action above, honouring wildcards and
`notActions`. That API does not show deny assignments, so a deny assignment
can still refuse a call after the pool has been grown.

## What needs a live AKS cluster

These are open until a run on a real cluster:

- whether deleting a machine lowers the pool's node count (the AKS documentation
  does not say; KubeNeuron fails as "needs an operator" if it does not);
- that a machine AKS lists after the pool grows is a node ready for work
  (the stable Machines API reports no state for a machine);
- the `aks-auto-repair` event source and reasons, and that auto-repair leaves
  Ready nodes alone;
- the ARM error codes AKS returns for quota and capacity failures on a scale;
- that the permissions read works with the custom role alone;
- the AKS VMSS providerID format and the agent pool name derived from the
  scale set name.
