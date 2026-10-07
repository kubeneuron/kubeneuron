# Security review — v0.6.0 checkpoint coordination (Phase 1)

Status: review of the checkpoint coordination Phase 1 implementation on
`main`, **ahead of the v0.6.0 release, which has not happened**. It is a
review of the code in this tree, not a third-party audit, and it is not a
hardware qualification: the feature has been exercised by unit tests, by the
full CPU-only kind integration suite (85 CEL checks and the smoke suite), and
by the completed dual-store (SQLite and PostgreSQL) upgrade/rollback
rehearsal, and by nothing on GPU hardware. The candidate tree did pass the
full AWS hardware harness (`hack/hw-e2e.sh`) on a temporary EKS
`g4dn.xlarge` cluster on 2026-10-07, but that harness exercises the shared
agent/controller runtime against injected faults and carries no opted-in
checkpointing Pod. No real Pod + Downward API checkpoint scenario has been
run, and kubelet refresh latency is unmeasured.
Report anything that contradicts it through
[SECURITY.md](https://github.com/kubeneuron/kubeneuron/blob/main/SECURITY.md).

## Scope

v0.6.0 adds one feature: an opt-in, deadline-bounded checkpoint coordination
pre-phase on the two disruption steps, `platform.drain` and
`platform.evict_gpu_workload`, configured by
`spec.safety.checkpointCoordination`. The design and protocol are in
[checkpoint coordination](checkpoint-coordination-design.md). The review
covers the trust boundary the feature introduces between the controller and
tenant workloads, the one RBAC change it costs, and the ways a workload or a
failure could try to delay or block remediation.

## Default off, explicit allowlist

- The block is optional and defaults to `enabled: false`. An absent block
  and an explicit `false` compile to nothing: the runtime snapshot digest of
  an installation that never asked for coordination is unchanged, and a
  disabled policy returns from the pre-phase before touching the platform,
  the store, or a metric.
- An enabled policy must name a non-empty `namespaces` allowlist. The CRD
  rejects `enabled: true` without one; the operator's snapshot compiler and
  the controller's runtime-config install refuse it again. A refused policy
  leaves the previous configuration in force rather than degrading to "all
  namespaces". There is no label selector and no wildcard.
- Self-declaration (the `kubeneuron.io/checkpoint: "true"` annotation) is
  honored only inside the allowlist. An opted-in pod in any other namespace
  is ineligible and is never patched or waited for.

## The one RBAC change, and its in-code confinement

The controller needs `patch` on core `pods` to stamp request annotations.
RBAC cannot narrow a patch to an annotation prefix or to a list of
namespaces, so the verb is cluster-wide once held. The mitigations are:

- **Conditional grant.** The operator emits `patch` on the managed
  controller ClusterRole only when the policy is enabled. An omitted or
  disabled policy keeps exactly `get, list, watch`; the rule changes verbs
  and never gains a sibling. A unit test pins the exact verb set for all
  three cases.
- **Static operator grant.** The operator's own ClusterRole
  (`config/rbac/operator_role.yaml`, mirrored by the Helm chart) holds
  `pods: patch` unconditionally, because RBAC escalation prevention means the
  operator cannot delegate a verb it does not hold. The operator never
  patches a Pod itself. A unit test requires the static role to cover the
  enabled controller role and confirms it grants neither `bind` nor
  `escalate`; another checks the Helm role mirrors the static one.
- **Live identity checks before any write.** The adapter reads the pod live
  and refuses to write unless its namespace is on the allowlist, it is on
  the incident's node, and its UID is the one that was listed. A pod that
  fails any of these is not the workload the decision was made about; nothing
  is patched and the workload is counted as exited or unreachable.
- **Guarded JSON Patch.** The write is a JSON Patch whose `test` operations
  re-assert the UID, the node name, and the current value (or absence) of
  every key it is about to change. An object that moved or changed between
  the read and the write is rejected by the API server rather than
  overwritten. A conflict is retried once against a fresh read; a second
  conflict counts the workload unreachable and waits for nothing.
- **Closed key set.** The adapter may write exactly five keys:
  `checkpoint-requested-at`, `checkpoint-deadline-at`,
  `checkpoint-incident`, `checkpoint-reason`, `checkpoint-next-action`, all
  under `kubeneuron.io/`. The workload-owned opt-in, max-wait and state keys
  are absent from that set by construction, and any other key is refused
  before the patch is built. The controller never touches labels, spec, or
  status.

## No new trust surface

- No webhook, callback, or URL fetch: the controller opens no connection to
  any workload, and the SSRF shape of a workload-declared endpoint was
  rejected at design time.
- No `pods/exec`, no signal delivery, no injected sidecar, no mutating
  admission.
- No new credential for any workload. The zero-RBAC path is the downward
  API, which the workload already has. A workload that wants to acknowledge
  explicitly must already hold patch rights on its own Pod; KubeNeuron grants
  none and never writes or deletes `checkpoint-state`.
- An explicit acknowledgement is bound to the request it answers. The only
  value of `checkpoint-state` that counts is exactly `complete:<incident>`
  for the incident ID stamped on the Pod as `checkpoint-incident`. A plain
  `complete`, an acknowledgement bound to an earlier incident that a
  long-lived Pod still carries, a different case or surrounding whitespace
  is not an acknowledgement, so a stale value cannot short-circuit the wait
  a later incident granted. The binding is durable: the same incident
  resuming its stamp after a controller restart recognizes its own bound
  answer. When another incident's live request is honored, the answer is
  judged against that incident's ID, the one the workload read; the value
  is compared and never logged. The ownership boundary is unchanged: the
  controller never removes a workload-owned key to reset a stale answer.
- Neither the agent nor the operator gains any privilege. The agent DaemonSet
  is untouched.

## A workload cannot delay remediation beyond policy

- The wait granted to a workload is its request when present, parseable and
  positive, the policy `defaultWait` otherwise, and never more than the
  policy `maxWait`. The CRD caps `maxWait` at 30 minutes and requires
  `defaultWait ≤ maxWait`. A malformed, zero, or negative request is simply
  not a request.
- Each workload is stamped with its own deadline, its own grant from one
  common request time, so a `checkpoint-max-wait` request shortens only that
  workload's window and a neighbour's longer grant never lengthens it. The
  step waits once, until the latest of those deadlines, and every deadline
  is bounded by the step's own budget: the step's intended timeout minus a
  fixed reserve kept for the disruption itself. When nothing is left after
  the reserve, no request goes out. The reserve is enforced by a dedicated
  coordination context whose deadline is the budget: the listing, every
  request, every observation, every sleep and every audit row of the phase
  run under it, and nothing in the phase borrows time from the step's own
  context, so a platform call or a store append that hangs ends with the
  budget rather than consuming the reserve, and the disruption then starts
  under the step's own context with the reserve intact. An audit row whose
  turn comes after the budget is spent fails fast and is logged; the
  metrics are still counted. Unit tests pin this on the contexts themselves,
  with a platform that blocks on each call, and on the trail of a spent
  budget. Coordination therefore cannot make a disruption step time out.
- The request time every stamped deadline is measured from is fixed when
  the coordination window opens, before the listing, so a slow listing or a
  slow sequence of patches cannot let a later stamp promise a workload a
  fresh window past the end of the original one; once the window has closed
  no further request is patched and no further read is made. Unit tests pin
  a two-minute listing, a sequence of one-minute patches, and a patch that
  hangs until the budget is spent.
- The pre-phase asks only the workloads the disruption will touch: for a
  drain, the pods the drain itself will evict under the step's `force`
  setting, taken from the platform's own drain rule rather than a copy of
  it. A finished, mirror, DaemonSet or (without `force`) unmanaged Pod that
  opts in cannot hold a drain's wait for an eviction that would never come.
- A sub-second `checkpoint-max-wait` is a valid, shorter grant and is stamped
  at that precision (RFC 3339 with the fractional second), so the durable
  deadline is exactly the window granted rather than a whole-second rounding
  of it.
- The deadline is stamped on the Pod as an absolute time, and a stamp already
  on the Pod for the same incident resolves to the *earlier* of the stamp and
  the fresh proposal. A workload with patch rights that edits its own
  deadline later can only have it cut back down; an expired stamp is found
  expired and disrupted immediately after a controller restart.
- Every deadline read back from an object, whether from the same incident or
  a foreign one, is capped to the deadline this step proposed for that
  workload.
- The `checkpoint-requested-at` read back from an object feeds only the wait
  histogram, never a deadline. It is used when it is no later than this
  step's own request time and floored at the 30m ceiling before it;
  otherwise the step's own request time is used. A tampered stamp can
  therefore neither lengthen a wait nor produce a negative or out-of-range
  observation.
- The wait is once per disruption step; every other gate (concurrency caps,
  maintenance windows, node pauses, approvals, the destructive blast-radius
  confinement) applies unchanged before and after it.

## Foreign requests and durable deadlines

A pod may carry a live request stamped by a different incident. The adapter
never overwrites it: two incidents re-stamping one job would let a wait be
granted repeatedly. The step honors the existing deadline, capped to its own
proposal, and grants no new window. If the listing did not show the request
the live object carries, there is no deadline of record to honor and the
workload is counted unreachable with no wait. An expired stamp from an
earlier incident is residue, not a hold, and is overwritten. The refusal
error and the controller's log line name the workload by namespace/name
only: the foreign incident ID, deadline, reason and next action are
annotation values a tenant with patch rights could have written, and none of
them is copied into an error, a log field, or an audit row. Two unit tests
pin this: one that a deliberately malicious incident annotation does not
appear in the adapter's error text, and one that the controller's log line
and audit rows for an honored foreign request carry neither the foreign
incident, its deadline, its reason nor its next action.

## Failure never blocks remediation

- Every error on the pre-phase path is counted in the closed metrics and
  then proceeds; the audit row describing it is attempted under the bounded
  or cancelled coordination context and may be dropped and logged once the
  budget is spent (including the no-budget `skipped` row). The pre-phase
  returns no error to the step and cannot fail it. Expiry is not a
  step failure and is not an escalation trigger.
- A listing failure is one `unreachable` count for the step; a patch or read
  failure counts the workload unreachable. Before the deadline a failed read
  is looked at again on the next pass; at the deadline it is the workload's
  final answer. The bounded wait is never lengthened by a failure.
- Cancellation of the step context ends the wait immediately; anything
  still open is counted unreachable. Exhaustion of the coordination budget
  also ends the wait, and anything still open before its own stamped
  deadline is likewise counted unreachable: it was not observed for the
  step's reason, not the workload's. `expired` is kept for a workload whose
  own deadline passed unanswered. Either way the step goes on to its
  disruption.
- The platform without the optional capability (bare metal) is skipped, not
  blocked.

## Snapshot and race limitations

- Coordination acts on the pods listed at the start of the step. A pod that
  appears after discovery is not asked and not waited for, and the
  disruption treats it exactly as it did without the feature. This is a
  best-effort courtesy, not a guarantee, and it is deliberately not widened
  into a watch.
- A replacement pod under the same name (new UID) is reported to the
  classifier with its UID and **no** annotations, so nothing it carries can
  read as an acknowledgement of the original; the original is counted exited.
- The opt-in and max-wait annotations are read from the listing snapshot,
  the request and state from the live object. A workload that changes its
  opt-in after discovery has no effect on the running step.
- Two controller replicas cannot both run a step: leader election and the
  step's durable lease are unchanged. The durable deadline exists so that a
  successor resumes rather than restarts a window.

## Bounded metrics and audit

- `kubeneuron_checkpoint_requests_total` carries one label, `outcome`, from
  a closed set of five values (`acknowledged`, `exited`, `expired`,
  `unreachable`, `skipped`). `kubeneuron_checkpoint_wait_seconds` carries no
  labels. `kubeneuron_destructive_steps_deferred_total` gains one fixed
  reason, `checkpoint_wait`. No node, pod, namespace, or incident label is
  emitted, so a tenant cannot grow the series set by naming things.
- Audit rows name at most five workloads by namespace/name and count the
  rest; annotation values written by workloads are never copied into an
  audit row, a log line, or a metric label.
- Dry-run issues no patch, no wait, and no metric; its output states how
  many workloads would have been asked.

## Explicitly out of scope (Phase 2 and later)

- Advisory notices at approval park, gang scope (`checkpoint-group`), and
  training-operator adapters are **not** implemented. Any annotation with
  those names is ignored.
- Per-class or per-namespace waits and a `GPUWorkloadProfile` override are
  not implemented; the allowlist is the only scoping control.
- No claim is made that the feature has run on GPU hardware, and no
  measurement of downward-API refresh latency has been taken in this tree.

## Limitations

- **Cluster-wide `pods: patch` is real.** Once the policy is enabled the
  controller service account can patch any Pod's metadata as far as RBAC is
  concerned. The confinement above is in code, pinned by tests, and stated
  here so that a reviewer weighs it as code rather than as RBAC. An
  installation that does not accept that cost keeps the policy disabled and
  keeps the read-only verbs.
- Coordination cannot verify that a workload actually wrote a checkpoint. An
  explicit bound `complete:<incident>` and a pod exit are both taken at
  face value; a crashed job counts as `exited`, which is why that outcome is
  separate from `acknowledged`.
- A non-forced drain of a node carrying an unmanaged Pod is refused by the
  drain's own pre-flight before it evicts anything. The pre-phase predicts
  per-Pod eligibility, not that refusal, so the Pods the drain would have
  evicted are still asked and waited for once before the step fails.
- This review covers the v0.6.0 addition only. Earlier surfaces are
  described in the [design document](design.md), the
  [v0.5.0 review](security-review-v0.5.0.md), and the security policy.
