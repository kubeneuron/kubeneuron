# Checkpoint coordination — design

Status: **design accepted, not implemented**, 2026-08-05.
Scope: §3.2 of the [definition plan](definition-plan.md) — "design first".

A training job that supports checkpointing would rather be **told** than
evicted. Today KubeNeuron evicts: `platform.drain` calls the Eviction API, the
pod gets SIGTERM and its grace period, and everything since the last checkpoint
is gone. For a job that checkpoints every 30 minutes on 64 GPUs, a reboot at
minute 29 costs more than the fault did.

This is the largest product differentiator available, and it is also the easiest
place to build something unsafe: a protocol where a workload can say "not yet"
is a workload that can veto fleet remediation forever. The design below gives
the job a warning and a deadline, and gives the workload no ability to extend
it.

## Recommendation

**Annotation-declared, Kubernetes-API-carried, deadline-bounded coordination,
implemented as a property of the disruption steps — not as a new playbook
action, and with no network path from the controller to the workload.**

- The workload **opts in** with a Pod annotation.
- KubeNeuron **notifies** by patching annotations onto that same Pod. The
  workload reads them through the downward API — no RBAC, no listener, no new
  credential on either side.
- The workload **acknowledges** by terminating, or by patching one annotation
  back if it already has RBAC on its own Pod.
- The wait is bounded by **operator policy**, stamped as an **absolute
  deadline** on the Pod, and survives a controller restart.
- On expiry the ladder proceeds exactly as it does today. The workload's only
  power is to make its own disruption *cleaner*, never *later*.

Everything else in this document follows from those five sentences.

## Why not the alternatives

**Rely on `terminationGracePeriodSeconds` + SIGTERM (do nothing new).** The
strongest alternative, and it loses for three concrete reasons. (1) *No lead
time*: SIGTERM arrives when the eviction starts, so a collective checkpoint
across a gang of pods cannot be coordinated — the peers see a rank disappear and
the job crashes before it writes anything. Distributed checkpointing must begin
*before* the first pod is disrupted. (2) *No context*: a grace period is a
static number in the pod spec; the job cannot tell "the node is being rebooted
in 10 minutes" from "the scheduler is rebalancing", and cannot know how long it
actually has. (3) *Signal propagation*: for `torchrun`/`mpirun` launchers PID 1
is not the trainer, and SIGTERM is routinely not propagated. An annotation the
trainer polls is delivered to the process that can act on it.

**Controller calls an endpoint the workload declares
(`kubeneuron.io/checkpoint-endpoint: http://…`).** Rejected on security. The
controller holds cluster credentials and, on EKS, an IRSA role that can
terminate instances; making it fetch a URL chosen by an arbitrary tenant pod is
a server-side request forgery primitive pointed at the most privileged component
in the system (`169.254.169.254`, in-cluster services, the Kubernetes API).
Constraining it — resolve to the pod's own IP, port from `containerPorts`, no
redirects, no DNS re-resolution — is exactly the checklist that is always
implemented incompletely. The Kubernetes API is already a mutually authenticated
channel both parties have; use it.

**Agent execs into the container to signal the process.** Rejected: `pods/exec`
into arbitrary namespaces is the largest privilege escalation available in a
cluster, and it would put it in a DaemonSet that already runs privileged with
hostPID. No.

**A CRD (`GPUWorkloadProfile`) selecting workloads by label.** Better trust
hygiene — only a cluster admin can create one — but it requires an admin to
describe every tenant's jobs, in a second place, kept in sync with them. The
annotation puts the declaration where the job already is, and the trust concern
is answered by the policy's namespace selector (below). A CRD remains the right
*override* mechanism later, for fleets where tenants must not self-declare.

**A mutating webhook that injects a checkpoint sidecar.** Heavy: a new
admission-path failure mode for every pod in the cluster, to deliver a string
the downward API already delivers.

## The protocol

### 1. The workload advertises

```yaml
metadata:
  annotations:
    kubeneuron.io/checkpoint: "true"          # opt-in; anything else is "no"
    kubeneuron.io/checkpoint-max-wait: "8m"   # a REQUEST, clamped by policy
    kubeneuron.io/checkpoint-group: "job/llama-pretrain"   # optional, gang scope
```

Discovery is a plain list of the pods on the node — the controller already does
this in `platform.NodeWorkloads`. `checkpoint-max-wait` is clamped by
`min(requested, policy.maxWait)`: a workload can always ask for **less** time and
never for more. A missing or unparseable value takes the policy default.

### 2. KubeNeuron requests

The controller patches the same Pod:

```yaml
kubeneuron.io/checkpoint-requested-at: "2026-08-05T11:02:13Z"
kubeneuron.io/checkpoint-deadline-at:  "2026-08-05T11:10:13Z"   # absolute
kubeneuron.io/checkpoint-incident:     "inc-8f2a"
kubeneuron.io/checkpoint-reason:       "fell-off-bus"           # problem class
kubeneuron.io/checkpoint-next-action:  "platform.drain"
```

The deadline is **absolute and durable on the object**, not a timer in
controller memory. A controller restart mid-wait re-derives the same deadline
from the Pod, so a crash cannot silently restart an 8-minute grant — the same
shape as the "durable bit is truth, the gate is a projection" invariant in
[design.md §2.4d](design.md).

The workload reads these with no RBAC at all through a downward-API volume:

```yaml
volumes:
- name: podinfo
  downwardAPI:
    items: [{ path: "annotations", fieldRef: { fieldPath: metadata.annotations } }]
```

The kubelet refreshes that file when annotations change. Sidecars and framework
operators that already watch their own pods can watch instead; both are
supported because both read the same field.

### 3. The workload acknowledges

Two paths, both accepted:

- **Terminate.** The container exits after writing its checkpoint. The pod
  reaching `Succeeded`/`Failed`, or disappearing, *is* the acknowledgement. This
  path needs no RBAC and no library — it is what a batch trainer should do
  anyway, since it is about to be evicted.
- **Patch one annotation** — `kubeneuron.io/checkpoint-state: complete` — for a
  workload that has (or is given) patch rights on its own Pod, e.g. a training
  operator that must keep the pod alive.

Anything else, including `checkpoint-state: in-progress`, is treated as *not
acknowledged*. There is deliberately no "extend" verb.

### 4. KubeNeuron proceeds

When every opted-in pod has acknowledged, or the deadline passes, the step
continues into the disruption unchanged. Both outcomes are audited on the
incident and counted (below). **Expiry is not a step failure** — escalating a
remediation because a job was slow to checkpoint would turn a courtesy into an
escalation trigger.

## Where it hooks into the ladder

**A property of the disruption, not a new action.** `platform.drain` and
`platform.evict_gpu_workload` gain a coordination pre-phase, configured once per
installation:

```yaml
spec:
  safety:
    checkpointCoordination:
      enabled: true
      defaultWait: 5m
      maxWait: 15m              # hard ceiling; annotations may only shorten
      namespaceSelector: {...}  # which tenants may self-declare
      skipClasses: ["fell-off-bus", "gpu-lost"]
```

A new `CheckpointWorkloads` CRD action was considered and rejected: protection
that depends on every playbook author remembering to add a step is protection
that is missing exactly in the hand-written playbook that matters. Making it a
property means every existing playbook — and every future one — gets it.

Two refinements that fall out of the ladder's existing shape:

- **`skipClasses` is not an optimization, it is correctness.** When the device
  has already fallen off the bus, the job is already dead; waiting eight minutes
  to be polite to a process that cannot make progress delays recovery for
  nothing. Default: skip the device-unavailable classes, coordinate for the rest.
- **The approval park is free lead time.** An incident parked in
  `AWAITING_APPROVAL` for six minutes is six minutes the job could have been
  checkpointing. Phase 2 sends an *advisory* notice at park time (same
  annotations, `checkpoint-advisory: "true"`, **no deadline, no wait**), and the
  binding request with the deadline still goes out when the disruption step
  starts. Advisory notices never block anything, so they cannot be abused.

The wait must also fit the step's budget: the effective deadline is
`min(policy.maxWait, workload request, remaining step timeout)`. A drain whose
step timeout is shorter than `maxWait` must not fail by timeout because of a
courtesy wait — the coordinator is the caller's guest, not its owner.

## Failure modes

| Mode | Behavior |
|---|---|
| The job lies — declares support, never checkpoints | Costs at most one bounded wait per disruption; the deadline is operator policy and the annotation can only shorten it. |
| The job never answers (crashed, wedged, no sidecar) | Deadline expires, `outcome="expired"` counted, audit records it, disruption proceeds. |
| The job answers *after* the eviction started | The patch lands on a pod that is terminating or gone; a 404 is ignored. Acknowledgement is advisory input to a decision that has already been made. |
| Controller restarts mid-wait | The absolute deadline on the Pod is the truth; the new leader resumes the same window instead of granting a fresh one. |
| The pod restarts during the window | It is a new pod object with no request annotation; it is re-requested if the step is still coordinating, and never inherits the old deadline. |
| The workload checkpoints but does not exit | Eviction still happens at the deadline. Checkpointing buys a clean restart, never a reprieve. |
| Annotation patch fails (RBAC, conflict, apiserver blip) | Log, count `outcome="unreachable"`, proceed after the same bounded wait. Coordination must never be able to block remediation by failing. |
| Every pod on the node opts in with the maximum wait | One wait per disruption step, not per pod: the requests go out together and the deadline is shared. |
| Dry-run incident | No patch is issued; the audit records what would have been requested. Dry-run stays free of side effects. |

## The security boundary

The rule: **a workload may influence how it dies, never whether.**

- **The clock belongs to the operator.** `maxWait` is installation policy;
  workload annotations clamp downward only. A hostile pod cannot delay
  remediation past the ceiling, and cannot delay it repeatedly — the wait is
  once per disruption step, and every existing gate (concurrency caps,
  maintenance windows, per-node pauses, approvals) is unchanged and still
  applies.
- **Self-declaration is scoped.** `namespaceSelector` decides which namespaces
  may opt in at all, so an untrusted tenant fleet can be excluded without
  disabling the feature for the trusted one.
- **No new inbound trust surface.** The controller opens no connection to any
  workload; the workload opens none to the controller. This preserves the
  property the agent channel already has ("no new trust surface for actions" —
  nodes run no listener).
- **One real privilege cost, stated plainly.** The controller today has
  `pods: get,list,watch` and `pods/eviction: create`
  (`internal/operator/resources.go`). This design adds **`pods: patch`
  cluster-wide**, because RBAC cannot restrict a patch to one annotation prefix
  or to label-selected namespaces. The controller must therefore enforce in code
  what RBAC cannot: it writes only `kubeneuron.io/checkpoint-*` keys, only on
  pods on the incident's target node, only in selected namespaces. That
  enforcement needs a unit test pinning it, in the same spirit as the
  blast-radius confinement test.
- **The audit is the record.** Every request and every outcome is an audit row
  on the incident, under the `system` actor, naming the pods.

## Observability — the metric that proves it worked

```
kubeneuron_checkpoint_requests_total{outcome}      # acknowledged|expired|skipped|unreachable
kubeneuron_checkpoint_wait_seconds                 # histogram: how long jobs actually took
```

`acknowledged / (acknowledged + expired)` is the number that proves the feature
works: the share of disruptions where a job was told and said "done" before
anything was killed. `kubeneuron_checkpoint_wait_seconds` is what calibrates
`maxWait` — a p90 far under the ceiling means the ceiling is too generous, a
cliff at the ceiling means jobs are being cut off.

It also feeds the §3.1 protection metric:
`kubeneuron_destructive_steps_deferred_total{reason="checkpoint-wait"}` — the
count of times the system chose to wait rather than disrupt.

**What is deliberately not claimed:** KubeNeuron cannot measure GPU-hours of
training preserved. It does not know what the job would have lost, and inventing
that number would be exactly the kind of claim this project spent two rounds
purging. The honest artifact is "N disruptions, M coordinated, p50 wait 42s".

## Implementation sketch

| File | Change |
|---|---|
| `internal/checkpoint/coordinator.go` *(new)* | Pure logic: parse/validate the workload annotations, clamp the deadline against policy and the step budget, decide acknowledged/expired from observed pod state. No Kubernetes client, so it is table-testable. |
| `internal/platform/platform.go` | New optional interface `WorkloadCheckpointer { RequestCheckpoint(ctx, []Workload, deadline) error; CheckpointStates(ctx, []Workload) (map[key]State, error) }`, in the style of the existing `CordonJanitor` / `InstanceRecycler` optionals. `Workload` gains `UID` and `Annotations`. |
| `internal/platform/kubernetes/checkpoint.go` *(new)* | The annotation patch and state read; enforces the key prefix, the node scope and the namespace selector. |
| `internal/platform/baremetal/baremetal.go` | Does not implement the optional interface; coordination is skipped, counted as `skipped`. |
| `internal/controller/execution.go` | In `executePlatformStep`, before `drain` / `evict_gpu_workload`: coordinate, audit, then disrupt. The wait runs inside the existing step goroutine and its timeout. |
| `api/v1alpha1/types.go` + `config/crd/bases` | `spec.safety.checkpointCoordination` with CEL bounds (`maxWait` ≤ 30m, `defaultWait` ≤ `maxWait`). |
| `internal/operator/config_snapshot.go`, `internal/config` | Compile the block into the runtime snapshot so it is digest-covered and rolls the controller on change. |
| `internal/operator/resources.go` | Add `patch` to the controller's `pods` rule; mirror in `deploy/helm/.../rbac.yaml`. |
| `internal/metrics/metrics.go` | The two series above. |
| `docs/playbook-authoring.md`, `docs/operations.md` | The annotation contract, the downward-API snippet, the policy block. |
| `test/e2e`, `hack/kind-integration.sh` | A fake platform that acknowledges, one that never answers, one that answers late; a kind phase with a real pod using the downward API. |

## Phases

**Phase 1 — the protocol, single-pod scope** (~1 week). Opt-in annotation,
request/acknowledge, bounded wait on `drain` and `evict_gpu_workload`, metrics,
audit, dry-run visibility, RBAC. Ships useful on its own: a single-pod trainer
gets a clean checkpoint before a reboot.

**Phase 2 — lead time and gangs** (~1 week). Advisory notice at approval park;
`checkpoint-group` so a gang is requested and waited on together (all members
acknowledge, or the deadline covers all of them); documented adapters for the
common training operators, shipped as examples rather than code.

**Phase 3 — policy shape** (only after phases 1–2 have run on a real fleet).
Per-class and per-namespace waits, a `GPUWorkloadProfile` override for fleets
where tenants must not self-declare, and calibration of the default ceiling from
`kubeneuron_checkpoint_wait_seconds`. Building this before the data exists would
be inventing a configuration language for behavior nobody has observed.

## Open questions for review

1. **Downward-API refresh latency.** The kubelet updates the annotations file on
   its sync loop, not instantly. If that is a minute, the effective grant is
   `maxWait − 1 minute` for the zero-RBAC path. Measure it in the kind phase and
   document the number rather than assuming it is small.
2. **Is "the pod terminated" a safe acknowledgement?** It conflates "I finished
   checkpointing" with "I died". Both lead to the same next action, so the
   decision is unaffected — but the metric would count a crashed job as a
   successful coordination. Phase 1 should count pod-exit separately from an
   explicit `checkpoint-state: complete`.
3. **Should coordination run before `platform.cordon` instead of `drain`?**
   Cordoning is when the node's fate is decided; draining is when work dies.
   Starting at cordon buys lead time for free, at the cost of notifying jobs on
   incidents that never reach a disruption. Phase 2's advisory notice is the
   proposed compromise; a reviewer may reasonably argue for making it the
   default.
