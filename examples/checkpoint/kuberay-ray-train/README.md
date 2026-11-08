# kuberay-ray-train — a RayJob whose workers checkpoint when told

> This example demonstrates the annotation protocol; it is not evidence that
> any framework integrates with KubeNeuron. Ray does not read KubeNeuron's
> annotations; `train.py` shows how user code does, and KubeNeuron never
> writes to a RayCluster or RayJob.

The RayJob's two worker Pods opt in and name the group
`ray-train-checkpoint`. The Ray Train loop (a `DataParallelTrainer`, no
framework needed) polls the Pod's annotations through the downward API —
rank 0 reads them and broadcasts one decision per step, because
`ray.train.report` is a barrier every worker must reach the same number of
times:

- an **advisory** (the incident parked for approval; v0.8.0) makes
  the next iteration report a `ray.train.Checkpoint` and continue;
- a **request** (`checkpoint-deadline-at` is on the Pod) makes it report one,
  write `kubeneuron.io/checkpoint-state: complete:<incident>` on its own Pod
  and return.

A Ray worker Pod does not exit when a training function returns, so the
explicit acknowledgement is the only one it can give. That takes a Role in
the namespace granting the worker ServiceAccount `get` and `patch` on pods:
the workload's own credential, the only one in these examples, and still
nothing KubeNeuron holds; note that it covers every Pod in the namespace,
not only the worker's own, which is Ray's RBAC granularity and a tenant
decision. `FailureConfig(max_failures=3)` is what would let Ray restart the
training function from the reported checkpoint afterwards; the fixture
proves the checkpoint report and the acknowledgement, not that recovery —
the example's request path returns from the training function and the
run is not resumed by the test. Ray Train requires a storage path
every worker can read and write: the example mounts one
`ReadWriteOnce` PersistentVolumeClaim into the head and the workers, which
holds only while they share a node (the kind run steers them onto one);
point `RAY_STORAGE_PATH` at a `ReadWriteMany` volume or an object-store URI
in a real cluster.

```sh
# KubeRay must already be installed.
kubectl apply -f examples/checkpoint/kuberay-ray-train/rayjob.yaml
kubectl -n ml-training get rayjob ray-train-checkpoint -w
```

With `impactPreview.workloadContext.kuberay: true`, each worker item in the
preview carries `context.ray` with `cluster`, `node_type: worker`, `group:
trainers` and `ray_job: ray-train-checkpoint`.

What a run proves: a Ray Train worker can turn the advisory and the request
into checkpoints and acknowledge the request. What it does not prove:
anything about Ray's own fault tolerance, GPU training, or that this is the
right checkpoint cadence for your job. KubeNeuron measures
`acknowledged / (acknowledged + expired)`, never saved progress.
