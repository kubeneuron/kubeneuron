# Checkpoint coordination — design and Phase 1 implementation record

Status: **Phase 1 implemented on `main`; v0.6.0 candidate prepared in
source, not released.** Design accepted 2026-08-05. The manifests, chart,
and samples in this source tree carry the v0.6.0 candidate pins, but the
`v0.6.0` tag, images, and release assets do not yet exist; the latest
published release is v0.5.0, which does not contain this feature. Nothing in
this document is a claim that v0.6.0 has been released or that the feature
has been exercised on GPU hardware.
Scope: §3.2 of the [definition plan](definition-plan.md).

A training job that supports checkpointing would rather be **told** than
evicted. Without coordination KubeNeuron evicts: `platform.drain` calls the
Eviction API, the pod gets SIGTERM and its grace period, and everything since
the last checkpoint is gone. For a job that checkpoints every 30 minutes on 64
GPUs, a reboot at minute 29 costs more than the fault did.

This is the largest product differentiator available, and it is also the easiest
place to build something unsafe: a protocol where a workload can say "not yet"
is a workload that can veto fleet remediation forever. The design gives the job
a warning and a deadline, and gives the workload no ability to extend it.

## Recommendation (as implemented)

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
- On expiry the ladder proceeds exactly as it does without the feature. The
  workload's only power is to make its own disruption *cleaner*, never *later*.

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
is answered by the policy's explicit namespace allowlist (below). A CRD remains
the right *override* mechanism later, for fleets where tenants must not
self-declare.

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
```

The opt-in comparison is exact and case-sensitive: `"True"` and `"yes"` are
typos, not declarations. `checkpoint-max-wait` is a Go duration and is
honored only downward: the wait granted is the request when it is present,
parseable and positive, the policy `defaultWait` otherwise, and in every case
no more than the policy `maxWait`. A workload can always ask for **less** time
and never for more.

Discovery is the controller's existing list of the pods on the node
(`platform.NodeWorkloads`), which now carries each pod's UID and a copy of
its annotations. `checkpoint-group` (gang scope) is **not** part of Phase 1;
see [Phases](#phases).

### 2. KubeNeuron requests

The controller patches the same Pod, and only these five keys:

```yaml
kubeneuron.io/checkpoint-requested-at: "2026-08-05T11:02:13Z"
kubeneuron.io/checkpoint-deadline-at:  "2026-08-05T11:10:13Z"   # absolute
kubeneuron.io/checkpoint-incident:     "inc-8f2a"
kubeneuron.io/checkpoint-reason:       "ecc-dbe"                # problem class
kubeneuron.io/checkpoint-next-action:  "platform.drain"
```

The deadline is **absolute and durable on the object**, not a timer in
controller memory. A controller restart mid-wait re-derives the same deadline
from the Pod: a stamp of the same incident already on the Pod resolves to the
*earlier* of the stamp and the fresh proposal, so a crash cannot silently
restart an 8-minute grant, and an already-expired stamp is found expired and
disrupted immediately. This is the same shape as the "durable bit is truth,
the gate is a projection" invariant in [design.md §2.4d](design.md).

The workload reads these with no RBAC at all through a downward-API volume:

```yaml
volumes:
- name: podinfo
  downwardAPI:
    items: [{ path: "annotations", fieldRef: { fieldPath: metadata.annotations } }]
```

The kubelet refreshes that file when annotations change, on its own sync
loop rather than instantly (see [Open questions](#open-questions)). Sidecars
and framework operators that already watch their own pods can watch instead;
both are supported because both read the same field.

### 3. The workload acknowledges

Two paths, both accepted:

- **Terminate.** The container exits after writing its checkpoint. The pod
  reaching `Succeeded`/`Failed`, being deleted, or disappearing *is* the
  acknowledgement, counted as `exited`. This path needs no RBAC and no
  library — it is what a batch trainer should do anyway, since it is about to
  be evicted.
- **Patch one annotation** — `kubeneuron.io/checkpoint-state:
  complete:<incident>` where `<incident>` is exactly the value of
  `kubeneuron.io/checkpoint-incident` the workload read off its own Pod —
  for a workload that already has patch rights on its own Pod (for example a
  training operator that must keep the pod alive). Counted as
  `acknowledged`. KubeNeuron grants no such rights and never writes, and
  never deletes, this key.

The acknowledgement is **bound to the request**. A long-lived Pod keeps
whatever it last wrote to `checkpoint-state`, so an unbound `complete` would
answer every later incident's request the moment it was stamped, and the
wait that incident granted would be skipped for a checkpoint nobody took.
The controller therefore compares the value exactly against
`complete:<incident>` for the incident of the request in force on the Pod:
a plain `complete`, `complete:` with another incident's ID, a different
case, or surrounding whitespace is *not acknowledged*. The same incident
recognizes its own bound answer across a controller restart, because the
incident ID on the Pod is the durable key. Anything else, including
`checkpoint-state: in-progress`, is likewise *not acknowledged*. There is
deliberately no "extend" verb. A late bound `complete:<incident>`, after
the deadline but before the disruption, still reads as acknowledged: the
outcome is the same, and the cleaner disruption is still worth recording.

### 4. KubeNeuron proceeds

When every requested pod has settled, or the latest deadline in force passes,
the step continues into the disruption unchanged. Every outcome is counted
(below); the audit row describing it is attempted under the bounded
coordination context and may be dropped and logged once the budget is spent.
**Expiry is not a step failure** —
escalating a remediation because a job was slow to checkpoint would turn a
courtesy into an escalation trigger. Nothing on this path can return an error
to the step.

## Where it hooks into the ladder

**A property of the disruption, not a new action.** `platform.drain` and
`platform.evict_gpu_workload` carry a coordination pre-phase (`Drain` and
`EvictGPUWorkload` in playbooks). The candidates are exactly the pods the
disruption will touch: for the eviction step the GPU-holding pods; for the
drain, the pods the drain itself will evict under the step's `force`
setting, which is the platform's own drain rule published on each listed
workload (`platform.Workload.DrainExclusion`) rather than a second copy of
it. A finished pod, a mirror or DaemonSet pod, or an unmanaged pod on a
non-forced drain is never asked and never waited for, opted in or not,
because the drain would not evict it. It is configured once per
installation:

```yaml
spec:
  safety:
    checkpointCoordination:
      enabled: true               # default false; an absent block is off
      defaultWait: 5m             # granted to a workload that requests nothing
      maxWait: 15m                # hard ceiling; annotations may only shorten; ≤ 30m
      namespaces: [ml-training]   # explicit allowlist; required when enabled
      skipClasses: [fell-off-bus, gpu-lost]   # the default when omitted
```

The CRD rejects an enabled block without a non-empty `namespaces` list,
a non-positive `defaultWait`, a `maxWait` outside `(0, 30m]`, and a
`defaultWait` above `maxWait`. The operator's snapshot compiler and the
controller's runtime-config install repeat the same checks, so the same
policy is refused at every layer that could be reached first, and a refused
policy leaves the previous configuration in force.

A new `CheckpointWorkloads` CRD action was considered and rejected: protection
that depends on every playbook author remembering to add a step is protection
that is missing exactly in the hand-written playbook that matters. Making it a
property means every existing playbook — and every future one — gets it.

Two refinements that fall out of the ladder's existing shape:

- **`skipClasses` is not an optimization, it is correctness.** When the device
  has already fallen off the bus, the job is already dead; waiting eight minutes
  to be polite to a process that cannot make progress delays recovery for
  nothing. The default skips `fell-off-bus` and `gpu-lost`. An explicit list
  replaces the default rather than adding to it, and an explicit empty list
  means coordinate for every class.
- **The step budget is the outer bound.** Coordination runs inside the
  disruption step's own goroutine and timeout. It may spend the step's
  intended budget minus a fixed reserve kept for the disruption itself (the
  reserve covers the drain's eviction-grace margin and a default termination
  grace period, doubled). When nothing is left after the reserve, no request
  goes out at all and the step proceeds untouched: a drain whose timeout is
  shorter than `maxWait` must not fail by timeout because of a courtesy wait.
  The reserve is kept by construction: everything the pre-phase does with
  the platform or the store once the budget is known — the listing, every
  request, every observation, every sleep between them and every audit row
  of the phase — runs under a **coordination context** whose deadline is the
  budget, so a listing that hangs, a patch that stalls or a slow audit
  append ends with the budget and never eats into the reserve; the
  disruption then runs under the step's own context, which still has the
  reserve. Nothing in the pre-phase borrows time from the step's own
  context: an audit row whose turn comes after the budget is spent is
  attempted under the spent context, fails fast and is logged rather than
  written, so the pre-phase's audit rows are best effort in exactly that
  case, while the metric counts are always recorded. There is no exception
  for the row that says there is no budget at all: with no coordination
  window to bound it, that row is attempted under an already-cancelled child
  of the step context, so it fails fast rather than borrowing time from the
  disruption, and may be missing from the trail while the `skipped` count is
  still recorded. Each sleep is the poll interval cut to what is
  left until the latest deadline in force and to what is left of the window.
  No request is patched, and no read is made, once the window has closed. A
  request still open when the budget runs out before its own stamped
  deadline is `unreachable`, not `expired`: the workload was not observed
  for the step's reason, not its own. Neither is a failure.

  The one request time every deadline is measured from is fixed when the
  coordination window opens, **before** the listing, so a slow listing or a
  slow patch spends the window rather than resetting it: a stamp issued late
  in the phase promises the workload only what is left. Each workload's own
  deadline is therefore
  `min(min(request-or-default, maxWait), budget)` from that one request
  time, never past the end of the window, and the step as a whole waits
  until the latest of those deadlines:
  `min(max over candidates of min(request-or-default, maxWait), budget)`. A
  workload's `checkpoint-max-wait` shortens only that workload's window; a
  neighbour granted the default never lengthens it.

## What Phase 1 does and does not promise

Coordination operates on an **eligible workload snapshot**: the pods listed on
the node when the step starts, filtered to what the disruption will evict
(GPU holders for the eviction step; the drain's own per-pod eligibility under
`force` for the drain), the allowlisted namespaces, and the opt-in
annotation. It is a bounded, best-effort courtesy, not a guarantee:

- A pod that appears on the node **after** discovery is not asked and is not
  waited for; the disruption that follows treats it exactly as before.
- A pod that is replaced during the window (same name, new UID) is counted as
  `exited` for the instance that was asked; the replacement inherits nothing
  and is not re-requested by the running step.
- One wait per disruption step, not per pod: requests go out together, each
  stamped with its own deadline, and the wait ends at the latest deadline in
  force. A step that made no request or whose requests all settled on the
  first look never sleeps.
- A workload that acknowledges is still disrupted. Checkpointing buys a clean
  restart, never a reprieve.
- A non-forced drain of a node carrying an unmanaged pod is refused by the
  drain before it evicts anything. The pre-phase still asks the pods that
  drain would have evicted, since per-pod eligibility is what it predicts;
  the refusal is the drain's own pre-flight and is not predicted here.

## Failure modes

| Mode | Behavior |
|---|---|
| The job lies — declares support, never checkpoints | Costs at most one bounded wait per disruption; the deadline is operator policy and the annotation can only shorten it. |
| The job never answers (crashed, wedged, no sidecar) | Deadline expires, `outcome="expired"` counted, the audit row is attempted under the coordination budget (dropped and logged if that is spent), disruption proceeds. |
| The job answers *after* the eviction started | The acknowledgement lands on a pod that is terminating or gone; it is advisory input to a decision that has already been made. |
| Controller restarts mid-wait | The absolute deadline on the Pod is the truth; the new leader resumes the same window, or finds it expired, instead of granting a fresh one. An acknowledgement the workload wrote against that request (`complete:<incident>`) is recognized by the resumed step, because it is bound to the incident ID on the Pod. |
| The Pod carries a `checkpoint-state` from an earlier incident | It is bound to that incident and is not an answer to this one; the Pod is asked and waited for as if it had said nothing. KubeNeuron never deletes the workload-owned key. |
| A workload writes a plain `complete` | Not bound to any request, so not an acknowledgement; the deadline decides. |
| The listing, a patch or a read hangs | It ends with the coordination budget; the reserve is untouched and the disruption starts under the step's own context. The hang counts `unreachable`, and candidates whose turn had not yet come are not asked and count `unreachable` too. The audit row that would record the outcome is attempted under the spent budget and may be dropped (logged); the metrics are not. |
| The Pod is one the drain will skip (finished, mirror, DaemonSet, unmanaged without `force`) | Not a candidate: no request, no wait, since the drain would not evict it. With `force`, an unmanaged Pod is a candidate because it will be destroyed. |
| The pod restarts during the window | It is a new object with a new UID; the original is counted `exited`, and the replacement never inherits the old deadline. |
| The workload checkpoints but does not exit | Disruption still happens at the deadline. |
| Annotation patch fails (RBAC, apiserver blip) | Logged, counted `outcome="unreachable"`, nothing waited for; the disruption proceeds in its own budget. Coordination must never be able to block remediation by failing. |
| Annotation patch conflicts (object changed between read and write) | Retried once against a fresh read; a second conflict counts `unreachable`. |
| The pod already carries a live request from **another** incident | It is left in place and never overwritten. Its existing deadline is honored, capped to this step's own proposal, so nothing on the object can extend this step's wait. |
| Listing the node's workloads fails | One `unreachable` count for the step; an audit row is attempted under the coordination budget; the disruption proceeds. |
| The step is cancelled or times out mid-wait | The wait ends; anything still open is counted `unreachable`. |
| The coordination budget runs out before a stamped deadline | The wait ends; anything still open is counted `unreachable` (it was not observed for the step's reason, not its own). `expired` is reserved for a workload whose own stamped deadline passed unanswered. |
| Every pod on the node opts in with the maximum wait | One wait per disruption step, ending at the latest per-workload deadline, bounded by `maxWait` and the step budget. |
| Bare-metal platform | The platform does not implement the optional interface; the step is counted `skipped` and proceeds. |
| Dry-run incident | No patch, no wait, no metric; the dry-run output records how many workloads a real run would have asked. |

## The security boundary

The rule: **a workload may influence how it dies, never whether.**

- **The clock belongs to the operator.** `maxWait` is installation policy,
  itself capped at 30m by the CRD; workload annotations clamp downward only.
  A hostile pod cannot delay remediation past the ceiling, and cannot delay
  it repeatedly — the wait is once per disruption step, and every existing
  gate (concurrency caps, maintenance windows, per-node pauses, approvals) is
  unchanged and still applies.
- **Self-declaration is scoped.** `namespaces` is an explicit allowlist of
  the namespaces that may opt in at all, so an untrusted tenant fleet can be
  excluded without disabling the feature for the trusted one. There is no
  label selector and no "all namespaces" spelling.
- **No new inbound trust surface.** The controller opens no connection to any
  workload; the workload opens none to the controller. No webhook, no
  callback, no exec, no injected sidecar, and no new credential for any
  workload.
- **One real privilege cost, stated plainly.** The controller's baseline
  grant is `pods: get,list,watch` and `pods/eviction: create`. An enabled
  policy adds **`pods: patch` cluster-wide** to the managed controller
  ClusterRole, because RBAC cannot restrict a patch to one annotation prefix
  or to a list of namespaces. The operator emits that verb only when the
  policy is enabled; an installation that never asked for coordination keeps
  the exact read-only verbs. The controller enforces in code what RBAC
  cannot: the live pod is read first and must be in an allowlisted namespace,
  on the incident's node, and the exact UID that was listed; the write is a
  JSON Patch whose `test` operations re-assert the UID, the node, and the
  current value of every key it changes; and the key set it may write is a
  closed list of the five request keys. The operator's static ClusterRole
  holds `pods: patch` unconditionally because it must be able to grant it.
- **The audit is the narrative; the metrics are the count.** Requests,
  refusals, and outcomes are written as audit rows on the incident under the
  `system` actor, naming at most five pods per row and counting the rest.
  The rows are attempted under the bounded (or already cancelled)
  coordination context, never under the step's reserve, so a row whose turn
  comes after the budget is spent — including the `skipped` row of a step
  with no budget at all — may be dropped and logged. The closed
  `checkpoint_requests_total` outcomes still count every case, and the
  disruption proceeds regardless.

Details, threat by threat, are in the
[v0.6.0 security review](security-review-v0.6.0.md).

## Observability — the metric that proves it worked

```
kubeneuron_checkpoint_requests_total{outcome}   # acknowledged|exited|expired|unreachable|skipped
kubeneuron_checkpoint_wait_seconds              # histogram, no labels
kubeneuron_destructive_steps_deferred_total{reason="checkpoint_wait"}
```

`acknowledged / (acknowledged + expired)` is the number that proves the feature
works: the share of coordinated disruptions where a job was told and said
"done" before anything was killed. `exited` is counted separately because it
also covers a job that simply crashed and a job that was gone before it could
be asked; `unreachable` covers a workload that could not be stamped or read
at all, or that was still open when the coordination budget ran out or the
step was cancelled before its own deadline, so it includes cases where no
request ever reached the object;
`skipped` is one count per disruption step where no request went out at all.

`kubeneuron_checkpoint_wait_seconds` observes, for each workload with a
request in force, the elapsed time from that request's **durable**
`checkpoint-requested-at` on the object until its outcome settled. A request
stamped fresh by this step measures from the moment the step issued it. A
same-incident request resumed after a controller restart keeps the original
request time, so the observation is the whole window the workload was given,
including the part that elapsed before the restart, not only the time this
step slept. An honored foreign request measures from its own request time.
The start is never later than this step's own request time and never more
than the 30m ceiling before it; because the step may then wait on past its
own request time, that start bound alone does not bound the elapsed time, so
the observation itself is cut to the 0..30m range (`checkpoint.ObservedWait`).
A tampered or stale stamp can therefore neither produce a negative
observation nor push one past the histogram's range, and an observation of
exactly 30m may be a cut-off value rather than a measured one. It includes
near-zero observations for requests that settled on the first look (an
immediate acknowledgement, a resumed window that had already expired), so it
is *not* only the time the step spent sleeping. It is what calibrates
`maxWait`: a p90 far under the ceiling means the ceiling is more generous
than jobs need, a cliff at the ceiling means jobs are being cut off.

`checkpoint_wait` on the deferral counter is the count of steps that really
paused before disrupting — counted once per step and only when a wait
actually happened.

**What is deliberately not claimed:** KubeNeuron cannot measure GPU-hours of
training preserved. It does not know what the job would have lost, and inventing
that number would be exactly the kind of claim this project spent two rounds
purging. The honest artifact is "N disruptions, M coordinated, p50 wait 42s".

## Implementation (Phase 1, on `main`)

| File | What it holds |
|---|---|
| `internal/checkpoint/checkpoint.go` | The pure policy core: annotation keys, `Policy` (validate, eligibility, the per-workload bounded wait and the step-wide bound), `Request` (render and parse), `ResolveDeadline`, `RequestStart`, `Observation` and `Classify`. No Kubernetes client, no clock beyond the caller's `now`, no goroutine; every decision is a table test. |
| `internal/platform/platform.go` | `Workload` gains `UID` and `Annotations`; the optional `WorkloadCheckpointer` interface (`RequestCheckpoint`, `ObserveCheckpoint`) and the `ErrCheckpoint*` sentinels. |
| `internal/platform/kubernetes/checkpoint.go` | The Kubernetes adapter: live read, in-code confinement (allowlist, node, UID), the closed request-key set, the guarded JSON Patch, and the pure observation. `NodeWorkloads` fills UID and annotations. |
| `internal/platform/baremetal` | Does not implement the optional interface; coordination is skipped, counted as `skipped`. |
| `internal/controller/checkpoint.go` | The pre-phase: budget and reserve, candidate filtering, one per-workload deadline each from a common request time, the bounded observation loop up to the latest deadline, foreign-request handling, audit rows, metrics. Called from `executePlatformStep` before `drain` / `evict_gpu_workload`; the dry-run projection from `executeStep`. |
| `api/v1alpha1/types.go`, `config/crd/bases`, `deploy/helm/kubeneuron/crds` | `spec.safety.checkpointCoordination` with its CEL bounds. |
| `internal/operator/config_snapshot.go`, `internal/config` | The block compiles into the runtime snapshot (only when enabled), so it is digest-covered; the controller hot-reloads the new snapshot in place from its mounted ConfigMap, without a restart or a rollout, and both layers re-validate it. Separately, the operator reconciles the managed ClusterRole when the block toggles (see `resources.go` below). |
| `internal/controller/runtimeconfig.go`, `cmd/kubeneuron-controller/reload.go` | The compiled policy reaches the controller's runtime config; an invalid enabled policy is refused whole. |
| `internal/operator/resources.go` | `patch` on the controller's `pods` rule only when the policy is enabled; `config/rbac/operator_role.yaml` and the Helm operator role hold it statically so the operator can delegate it. |
| `internal/metrics/metrics.go` | The two series and the `checkpoint_wait` deferral reason. |
| `deploy/grafana/kubeneuron-dashboard.json` | Two panels: requests by outcome, wait p50/p90. |
| `test/integration/cel-admission.sh` | Server-side CEL cases for the block's bounds. |

## Phases

**Phase 1 — the protocol, single-pod scope.** *Implemented on `main`;
v0.6.0 candidate prepared in source, release pending.* Opt-in annotation,
request/acknowledge, bounded
wait on `drain` and `evict_gpu_workload`, metrics, audit, dry-run visibility,
conditional RBAC. Useful on its own: a single-pod trainer gets a clean
checkpoint before a reboot. It has been exercised by unit tests and the
CPU-only integration harness's CEL cases; it has **not** been exercised on GPU
hardware, and no such claim is made.

**Phase 2 — lead time and gangs.** *Future scope, not implemented.* An
advisory notice at approval park (same annotations, `checkpoint-advisory:
"true"`, no deadline, no wait — an incident parked in `AWAITING_APPROVAL` for
six minutes is six minutes the job could have been checkpointing);
`checkpoint-group` so a gang is requested and waited on together; documented
adapters for the common training operators, shipped as examples rather than
code.

**Phase 3 — policy shape.** *Future scope, only after phases 1–2 have run on
a real fleet.* Per-class and per-namespace waits, a `GPUWorkloadProfile`
override for fleets where tenants must not self-declare, and calibration of
the default ceiling from `kubeneuron_checkpoint_wait_seconds`. Building this
before the data exists would be inventing a configuration language for
behavior nobody has observed.

## Open questions

1. **Downward-API refresh latency.** The kubelet updates the annotations file
   on its sync loop, not instantly. If that is a minute, the effective grant
   is `maxWait − 1 minute` for the zero-RBAC path. This has not been measured
   in this tree; measure it before relying on short waits, and document the
   number rather than assuming it is small.
2. **"The pod terminated" as an acknowledgement.** It conflates "I finished
   checkpointing" with "I died". Both lead to the same next action, so the
   decision is unaffected; Phase 1 counts pod exit (`exited`) separately from
   an explicit bound `checkpoint-state: complete:<incident>` (`acknowledged`)
   so the metric does not count a crash as a successful coordination.
3. **Should coordination run before `platform.cordon` instead of `drain`?**
   Cordoning is when the node's fate is decided; draining is when work dies.
   Starting at cordon buys lead time for free, at the cost of notifying jobs
   on incidents that never reach a disruption. Phase 2's advisory notice is
   the proposed compromise; a reviewer may reasonably argue for making it the
   default.
