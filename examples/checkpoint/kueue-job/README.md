# kueue-job — the same Job, admitted through Kueue

> This example demonstrates the annotation protocol; it is not evidence that
> any framework integrates with KubeNeuron. KubeNeuron never writes to a
> Kueue object; with `impactPreview.workloadContext.kueue: true` it only
> reads the Workload that admitted the Job to say so in the preview.

`job.yaml` is [`plain-job`](../plain-job/) with the `kueue.x-k8s.io/queue-name`
label and `suspend: true`, plus a minimal ResourceFlavor, ClusterQueue and
LocalQueue. Kueue admits the Workload and unsuspends the Job; the Pods then
run the same trainer and sidecar and answer the same advisory and request.

```sh
# Kueue must already be installed.
kubectl apply -k examples/checkpoint/kueue-job
kubectl -n ml-training get workloads
```

In the impact preview of an incident on the Pods' node, each item carries
`context.kueue` with `workload`, `local_queue` (`team-a`), `cluster_queue`
(`cpu-queue`), `priority` and `admitted_at`:

```sh
kubeneuronctl incidents impact <incident-id> --json | jq '.workloads.items[].context.kueue'
```

What a run proves: the preview can name the queue that owns the Pods it is
about to disrupt. What it does not prove: anything about Kueue preemption,
requeueing or scheduling behaviour under remediation — KubeNeuron does not
touch them.
