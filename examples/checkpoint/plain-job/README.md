# plain-job — an Indexed Job that checkpoints when told

> This example demonstrates the annotation protocol; it is not evidence that
> any framework integrates with KubeNeuron. The trainer is a shell loop that
> writes a step counter; "checkpoint" here means copying that counter.

Two Pods of an Indexed Job opt in with `kubeneuron.io/checkpoint: "true"`,
ask for at most `3m` of lead time, and name the group `plain-job-run`. Each
Pod runs:

- **trainer** (`../scripts/trainer.sh`): one step per second, a checkpoint every 30
  steps, and two signals — `SIGUSR1` checkpoints at the next step and keeps
  going, `SIGUSR2` checkpoints now and exits 75 (`EX_TEMPFAIL`), so the Job
  replaces the index instead of counting it complete.
- **checkpoint-sidecar** (`../scripts/sidecar.sh`): polls the downward-API annotations
  file. An **advisory** (the incident parked for approval; v0.8 development)
  becomes `SIGUSR1`. A **request** (`checkpoint-deadline-at` is on the Pod)
  becomes `SIGUSR2`; once the trainer has written its checkpoint and left,
  the sidecar exits too, and the Pod leaving is the acknowledgement.

```sh
kubectl apply -k examples/checkpoint/plain-job
kubectl -n ml-training get pods -w
```

With `spec.safety.checkpointCoordination` enabled for `ml-training`
(`advisory` and `groups` on to see the full path), open an incident on the
node running one of the Pods, approve it, and watch:

```sh
kubectl -n ml-training logs -l job-name=plain-job-run -c checkpoint-sidecar -f
kubeneuronctl incidents impact <incident-id>      # eligible_for_checkpoint: 2, groups: plain-job-run
kubeneuronctl incidents show <incident-id>        # checkpoint: complete after …: ml-training/plain-job-run-0-…=exited
```

What a run proves: the Pod read the advisory and the request off its own
object, the trainer wrote its checkpoint before the drain, and KubeNeuron
recorded `exited`. What it does not prove: anything about a real training
framework, GPU hardware, or the quality of the checkpoint.

## Resuming: the checkpoint has to outlive the Pod

`job.yaml` keeps the checkpoint on an `emptyDir`, which is deleted with the
Pod: the replacement the Job creates for the index starts from step 0, and
the protocol does not care. A real job keeps it on a volume that survives.
The [`../plain-job-pvc/`](../plain-job-pvc/) overlay is that shape with one
index and one `PersistentVolumeClaim` from the cluster's default
StorageClass:

```sh
kubectl apply -k examples/checkpoint/plain-job-pvc
kubectl -n ml-training logs -l job-name=plain-job-run -c trainer -f   # "trainer: resuming from step N" on the replacement
```

`hack/kind-workload-safe.sh` runs this overlay through a request and asserts
that the replacement Pod logs `resuming from step N` with the step the
first Pod checkpointed. That proves the toy resumes from what it wrote on
a volume; it says nothing about a real trainer's checkpoint format or about
a job with many indexes, which needs one claim per index or a
`ReadWriteMany` volume.
