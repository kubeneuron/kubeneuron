# Checkpoint coordination examples

> This example demonstrates the annotation protocol; it is not evidence that
> any framework integrates with KubeNeuron. Each one is a CPU-only toy that
> writes a "checkpoint" file, and each README says exactly what its run
> proves and what it does not.

KubeNeuron talks to a workload through annotations on its own Pod and
nothing else — no listener, no webhook, and no credential held by
KubeNeuron for the workload or by the workload for KubeNeuron. Reading the
annotations needs no credential at all (the Downward API); a workload that
acknowledges by *writing* its state key, as the Ray example does, needs
`patch` on Pods in its own namespace, which is a credential its tenant
reviews ([design](../../docs/checkpoint-coordination-design.md),
[v0.8 record](../../docs/workload-safe-recovery-design.md)). A workload
reads them through the downward API:

| Who writes it | Key | Meaning |
|---|---|---|
| the workload | `kubeneuron.io/checkpoint: "true"` | opts in (exact value) |
| the workload | `kubeneuron.io/checkpoint-max-wait` | asks for a shorter wait; never a longer one |
| the workload | `kubeneuron.io/checkpoint-group` | names its gang (a DNS label); peers in the same namespace get the same request |
| KubeNeuron | `kubeneuron.io/checkpoint-advisory-*` | *advisory*: the incident parked for approval and a disruption may follow — checkpoint if you can; no deadline (v0.8.0) |
| KubeNeuron | `kubeneuron.io/checkpoint-requested-at`, `-deadline-at`, `-incident`, `-reason`, `-next-action` | *request*: checkpoint now; the disruption follows no later than the deadline — earlier once every asked workload has answered or left, and regardless of the answer |
| the workload | `kubeneuron.io/checkpoint-state: complete:<incident>` | acknowledges when it stays alive (needs `patch` on its own Pod); a Pod that exits acknowledges by leaving |

The two Job examples share one sidecar pattern (`scripts/sidecar.sh`):
poll the downward-API annotations file; on an advisory, log it and ask the
trainer to checkpoint at its next convenient point; on a request, ask for a
checkpoint now and, once the trainer has written it, let the Pod exit — which
is the acknowledgement, and which makes the Job replace the index. The Ray
example's worker Pods never exit on their own, so the training loop
acknowledges with the state annotation instead, using the workload's own
Role. The trainer never learns how to extend the deadline because there is
no way to.

| Example | Shows | Needs |
|---|---|---|
| [`plain-job/`](plain-job/) | an Indexed Job whose two Pods form a `checkpoint-group`; the sidecar reacts to the advisory and the request | any cluster with KubeNeuron and `checkpointCoordination` enabled for the namespace |
| [`kueue-job/`](kueue-job/) | the same Job admitted through a Kueue LocalQueue, so the impact preview's `kueue` context names the queue | Kueue installed; `impactPreview.workloadContext.kueue: true` |
| [`kuberay-ray-train/`](kuberay-ray-train/) | a RayJob whose worker Pods opt in and form a group; the Ray Train loop forces a checkpoint after an advisory | KubeRay installed; `impactPreview.workloadContext.kuberay: true` |

`hack/kind-workload-safe.sh` (`make test-workload-safe-kind`) runs all three
against an existing cluster where KubeNeuron is installed and reachable (the
cluster `KEEP_CLUSTER=1 make test-integration-kind` leaves behind fits),
installing the Kueue and KubeRay versions pinned in
`deploy/kubernetes/dependencies/versions.lock.yaml`, and asserts the
advisory, the preview with its contexts, the request, every acknowledgement
and that no Kueue or Ray object was written. `--verify`
(`make verify-workload-safe-examples`) is its offline half: it renders the
manifests, runs the trainer and sidecar locally through an advisory and a
request, and proves its own assertions fail on deliberate defects. The
offline kind gate (`make test-integration-kind`) exercises the same protocol
on bare Pods without these images.
