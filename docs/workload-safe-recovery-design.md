# Workload-safe GPU recovery — design record (v0.8 development)

Status: **design accepted for v0.8 development; implemented on the v0.8.0
candidate and validated by the gates the CHANGELOG records (unit, kind,
the CPU-only examples fixture, and one hardware run on a single Tesla T4);
not released.** Recorded 2026-11-05. The source tree that carries this page
pins the v0.8.0 release candidate, and the latest published controller
(v0.7.0) has none of the surfaces described here. Every sentence below is
a decision about what to build and in which order; what shipped, and with
which evidence, is stated by each surface's reference page and by the
capability matrix with the gate, SHA and date that proved it. This page
records the decisions, not the evidence.

Scope: §3.2 of the [definition plan](definition-plan.md) ("protect harder"),
the "Phases" section of the
[checkpoint coordination design](checkpoint-coordination-design.md) (Phase 2:
advisory at approval park, gang scope, documented adapters), the
[MIG semantics decision](mig-decision.md) (blast radius named in the approval),
and §2.4d of the [design document](design.md) (MIG is not a remediation unit).

## The problem this release addresses

A human who approves a destructive step on a degraded GPU node today sees the
incident, the evidence and the step. They do not see **what the step will
destroy**: which workloads run on the node, which of them hold GPUs, which
namespaces, whether the GPU is partitioned or time-sliced so that one device's
fault is every tenant's fault, and whether any of those workloads could have
checkpointed first. Phase 1 checkpoint coordination gives an opted-in Pod a
bounded warning at the moment of disruption; the six minutes an incident spends
parked in `AWAITING_APPROVAL` are six minutes nobody told the job about.

v0.8 closes that gap in one direction only: **the human sees more, the job gets
more lead time, and no workload gains any power over the ladder.** The
boundaries of Phase 1 are the boundaries of this release: a workload influences
*how* it stops, never *whether*.

## Six decisions

### S1. Device topology classification — read-only, fail-closed

**Decision.** A pure controller function `topology.Classify` renders a node's
device layout into a closed enum (`whole-device`, `mig`, `time-sliced`,
`dra-managed`, `mixed`, `unknown`) with a verdict on device-scoped actions
(`eligible` or `refused`), stable reason codes, and an attribution mode
(`per-pod`, `device-wide`, `unknown`). It reads only what the controller already
holds — the node's capacity and GFD labels, the NVIDIA agent report — plus,
behind an explicit opt-in, a read-only list of `resource.k8s.io/v1`
ResourceSlices.

**Rules.** The classifier can only narrow. For every input on which the v0.7
reset gate (`allowNVIDIAReset`) refuses, the result stays refused; a `refused`
classification additionally holds a reset the gate would have admitted. Missing
or contradictory input is `unknown` and `unknown` is refused — there is no
"assume whole-device". Readiness is a separate fact from layout: a fresh,
verified-unpartitioned, inventoried report the agent degraded (on a cloud VM
the hypervisor withholds the PCI reset, so no report there is ever `ready`)
still establishes a `whole-device` layout; the verdict is then `refused` with
`ReportNotReady`, the one refusal that can stand beside a `whole-device`
layout. The first hardware run (2026-11-05, EC2 g4dn) found the classifier
collapsing such nodes to `unknown`, which would have left the layout
unreachable on any virtualized fleet; it was corrected on the candidate before
the gate was recorded. Node-scoped rungs (cordon, drain, reboot, recycle,
replace) are unchanged; the classification exists so a preview can say "this
node is MIG: a drain destroys N instances in M namespaces". No input comes from
the exporter. The only new metric is a counter with a closed label set.

**Alternatives rejected.** *Infer time-slicing from a replica count mismatch* —
the cause is not observable from the node object alone, so the mismatch is
reported as its own code and refused as `unknown` rather than guessed.
*Read ResourceClaim objects* — unnecessary; the Pod spec's `resourceClaims`
entries already prove a claim exists on the node, so no claim object is read
and the RBAC grant is `resourceslices` get/list/watch alone. *A gauge per node*
— rejected as a new series with a node label; the fleet already has a
readiness page per node.

### S2. Advisory at approval park

**Decision.** When an incident parks in `AWAITING_APPROVAL` and a `Drain` or
`EvictGPUWorkload` step remains downstream, the controller stamps a **closed
set of advisory annotation keys** on every eligible Pod — the same eligibility
Phase 1 uses — distinct from the Phase 1 request keys:
`checkpoint-advisory`, `-at`, `-incident`, `-round`, `-reason`, `-next-action`.
An advisory carries no deadline and starts no wait. It is stamped once per
`(pod UID, incident, approval epoch)`; a re-park with a new epoch stamps again;
the key on the object is the truth across controller restarts. If the round
ends without disruption, a best-effort `checkpoint-advisory-withdrawn-at` is
stamped; failure to withdraw is logged and counted, never blocking. Workload-
owned keys (`checkpoint-state`) are never removed.

**Boundary.** The same guarded JSON Patch (test on UID, node, current values),
the same namespace allowlist, the same conditional `pods: patch` RBAC. No new
RBAC edge. Dry-run performs no patch and reports how many it would have
stamped. The stamp runs in its own bounded context (default 10 s, policy
ceiling 60 s) *after* the park is committed, so a slow or unreachable apiserver
cannot delay the approval.

**Alternatives rejected.** *Reuse the Phase 1 request keys with a far deadline*
— a request promises a wait; an advisory must not, and conflating them would
let an advisory look like a veto window. *A single fleet-wide advisory event*
— the Pod annotation is the only channel that reaches the trainer without a
listener, and it is per-Pod by construction.

### S3. Bounded gang coordination

**Decision.** A Pod may declare `kubeneuron.io/checkpoint-group: <name>`
(DNS label); a group is every Pod in the same namespace with the same value.
Disruption stays **node-local**: a step on node N destroys only Pods on N.
Group members on other nodes receive the Phase 1 request keys with the same
incident and their own deadline, and nothing else — they are not evicted,
their nodes are not cordoned, and the step does not wait for them longer than
the same bounded window. Every member's deadline is `min(min(request-or-
default, maxWait), budget)` from one request time; the step waits until the
latest deadline *within the budget*. A member with a shorter `max-wait`
shortens only itself. Groups larger than `maxGroupSize` (default 64, ceiling
512) are not stamped beyond the node: peers are skipped, counted, and the
preview says so. `crossNode` is **off by default**: the group is then the
members on the same node (lead time for local ranks, zero new blast radius).
There is no group leader, no aggregate state, and no derivation of a group from
an owner reference in v0.8 — only the explicit annotation.

**Alternatives rejected.** *Derive groups from Job/RayCluster/PyTorchJob
owners* — convenient and wrong: an owner is not a checkpoint domain, and the
derivation would silently widen the stamp set when an operator changes its
controller. *Wait for the whole gang to acknowledge* — a gang that cannot
checkpoint would hold the ladder; the ceiling stays the ceiling.

### S4. Impact preview for the human approve

**Decision.** At every park and re-park the controller computes a **durable,
immutable** `impact-preview/v1` record bound to `(incident, epoch)`: the node
and its topology classification, the pending step and the disruptions
downstream of it, workload counts (total, GPU holders, namespaces, eligible for
checkpoint, ineligible by code), groups, up to `maxItems` items with their
owner chain and read-only context, device counts and attribution, the effective
checkpoint policy, limits, warnings, and — when it could not be computed — an
explicit `unavailable: {reason}`. A SHA-256 digest over the canonical JSON
identifies what the human saw.

**Rules.** The preview is advisory by default and optionally required for
approval: without `requireForApproval` it cannot block an approve; with it,
an approve is held while the preview is unavailable or carries no digest.
Either way an approve is **bound** to what was seen: the decision record carries the
existing `park_epoch` and, new, an optional `impact_digest`. With
`impactPreview.requireForApproval: true` (default `false`) an approve without a
digest, or with a digest that does not match the current round, is refused
with `409` — the same rule the API already applies to a stale round — and an
`unavailable` preview refuses approval until an idempotent, audited recompute
succeeds. The computation is bounded (default 15 s, ceiling 60 s), capped at
`maxItems` (10–1000, default 200) with `items_truncated` beyond it, and reads
nothing but Kubernetes objects. **No invented numbers:** the preview never
estimates lost training GPU-hours or saved progress; KubeNeuron does not know
them and will not pretend to.

**Alternatives rejected.** *Compute the preview on demand in the approve
handler* — the human would approve against a page the controller rendered
later than the one they read; durability per round is the whole point.
*Make the preview a hard gate* — a preview that fails because an apiserver
list timed out must not hold a reboot the policy already allowed; the
`requireForApproval` opt-in is the only place that trade is made, and it is the
operator's to make.

### S5. Read-only context resolvers and examples

**Decision.** An "adapter" in v0.8 answers one question for the preview —
*whose Pod is this* — and does nothing else. Three resolvers, each behind its
own `impactPreview.workloadContext.<x>` opt-in and each granted `get,list,
watch` alone on its group: `batchJobs` (owner reference to a `batch/v1` Job),
`kueue` (queue-name label and `Workload` admission), `kuberay` (cluster and
node-type labels, RayJob). An absent CRD group answers `not-observed`; the
preview shows it and nothing fails. Clients are dynamic and unstructured, so
the controller compiles no dependency on Kueue or KubeRay modules. The
examples under `examples/checkpoint/` — a plain Indexed Job, a KubeRay Ray
Train job, the same Job in a Kueue LocalQueue — demonstrate the annotation
protocol end to end on CPU-only kind: `hack/kind-workload-safe.sh` drives all
three through a real installation with the Kueue and KubeRay versions pinned
in `deploy/kubernetes/dependencies/versions.lock.yaml`, and its `--verify`
mode runs the toys locally and fails its own assertions on deliberate defects.
Each README states that it demonstrates the protocol and is not evidence that
any framework integrates with KubeNeuron.

**Alternatives rejected.** *Act on the framework object (suspend the Job,
preempt the Workload, scale the RayCluster)* — every one of those is a write
into a system KubeNeuron does not own and cannot reason about; it is a
non-goal of this release and a test pins the RBAC verbs. *Ship the adapters as
controller code* — an example a platform team can read and copy is worth more
than a code path nobody has run on a real fleet, and it makes no claim.

### S6. One eligibility function

**Decision.** A single `eligibility.Evaluate(workload, policy, topology)`
with closed codes (`NotOptedIn`, `NamespaceNotAllowed`, `DrainExcluded`,
`ClassSkipped`, `GroupInvalid`, `GroupTooLarge`, `BudgetExhausted`,
`PlatformUnsupported`) is used by the Phase 1 request, the advisory, the group
stamp and the preview, so the four surfaces cannot disagree about who is
eligible. Device-scoped eligibility comes **only** from S1 and the existing
`AcceleratorRuntimeProfile` gate; the profile gains no field that widens
permission in v0.8. The only profile change contemplated is a narrowing field,
`partitionPolicy: Refuse`, whose single admissible value is also its default
and which exists so a later decision under the MIG page needs no API change.

## Security boundary

The release adds **no remediation action, no device-scoped action on
partitioned or shared GPUs, no write to Kueue, Ray or DRA objects, and no
network path from the controller to any workload.** Concretely, and each with
a test or lint in the local gate:

1. No callbacks into workloads: the controller opens no connection to any Pod
   or Service, reads no URL from an annotation, calls no workload webhook.
2. No `pods/exec`, `pods/attach`, `pods/portforward`, `pods/ephemeralcontainers`
   in any generated or static role.
3. No veto and no extension: no annotation, API or resolver can make a window
   longer than `min(maxWait, budget)` or stop a step.
4. No claim about checkpoints: the system records `acknowledged`, `exited`,
   `expired`, `unreachable` and `skipped`, never "checkpoint saved".
5. No mutation of Kueue, Ray or batch objects; `resource.k8s.io` is read-only,
   `v1` only, opt-in only.
6. No device-scoped action on `mig`, `time-sliced`, `dra-managed`, `mixed` or
   `unknown` topology; the action registry gains no device-scoped action.
7. No new inbound trust surface: no ports, listeners or webhooks; the new
   routes are operator-token GET/POST on the existing listener.
8. Dry-run remains the default and every new policy block defaults to off; in
   dry-run no patch is performed.

RBAC additions are conditional and read-only: `resourceslices` get/list/watch
when the DRA opt-in is set, and get/list/watch on `batch/jobs`,
`kueue.x-k8s.io/{workloads,localqueues,clusterqueues}` and
`ray.io/{rayclusters,rayjobs}` when the corresponding context opt-in is set.
`pods: patch` continues to be granted only while `checkpointCoordination.
enabled` is true. The security review for v0.8 checks exactly this diff.

## Policy shape

All new blocks are optional and off when absent, with the same three
validation layers as v0.6 (CEL at admission, the operator's compiler, the
controller's runtime config); a rejected policy leaves the previous one in
force:

```yaml
spec:
  safety:
    checkpointCoordination:        # v0.6, unchanged
      advisory:   {enabled: false, budget: 10s}            # S2; requires checkpointCoordination.enabled
      groups:     {enabled: false, maxGroupSize: 64, crossNode: false}   # S3; same requirement
    impactPreview:                 # S4
      enabled: false
      budget: 15s
      maxItems: 200
      requireForApproval: false    # requires impactPreview.enabled
      workloadContext: {batchJobs: false, kueue: false, kuberay: false}  # S5
    topologyClassifier:            # S1; always on, no enabled switch
      dra: {enabled: false}        # the only knob: read ResourceSlices
```

## Data, audit and metrics

- Store: a new `impact_previews` table keyed by `(incident_id, epoch,
  computed_at)` with digest, bounded payload and an `unavailable_reason`;
  a nullable `impact_digest` column on the decision record. Migrations are
  forward-only and lockstep for SQLite and PostgreSQL; rollback follows the
  existing rule — restore the pre-upgrade snapshot, never run the old binary on
  the new schema — and the upgrade page will say so before v0.8 ships.
- Audit: new `action` values on the existing hash chain —
  `checkpoint-advisory`, `checkpoint-advisory-withdraw`,
  `checkpoint-group-request`, `impact-preview-computed`,
  `impact-preview-unavailable`, `impact-preview-recompute`,
  `topology-classified` (at approval park and at a reset-gate refusal only).
  Rows are written under `system`, name at most five Pods plus a count, and
  may be dropped under pressure; metrics may not.
- Metrics, all with closed label sets and no node, pod or namespace label:
  `kubeneuron_checkpoint_advisories_total{outcome}`,
  `kubeneuron_checkpoint_group_requests_total{outcome}`,
  `kubeneuron_impact_previews_total{result}`,
  `kubeneuron_impact_preview_seconds`,
  `kubeneuron_topology_classifications_total{topology,result}`,
  `kubeneuron_approvals_bound_total{binding}`, and a new
  `reason="topology_unqualified"` on the existing deferred-steps counter.

## What this release will not prove, and will say so

The hardware the project can rent is a single Tesla T4 on an EKS node. That
machine can exercise time-slicing and an opted-in checkpointing Pod; it cannot
partition into MIG instances and it cannot run a DRA driver. So MIG and DRA
exist in v0.8 only as **refusals a classifier renders from read-only
evidence**, exercised by unit and kind fixtures. The CHANGELOG entry for v0.8.0
will carry the sentence in the security boundary above verbatim and a block
naming what the hardware gate did not cover. No public document will call any
of this "validated", "pilot" or "supported with Kueue/Ray" before the
corresponding gate has passed on the code it describes.

## Order of work

Design record (this page) → S1 classifier → S2 advisory → S4 preview with S6
eligibility, store, API, CLI and UI → S5 resolvers in parallel with S3 groups
→ examples and the kind fixture → documentation, runbooks and the capability
matrix → release preparation, the local and kind gates on the candidate, the
hardware gate, tag and publication. The discovery gate that precedes feature
code is a product decision recorded privately; it is not a code artifact and
this page does not describe it.
