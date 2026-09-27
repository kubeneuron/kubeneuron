# Security review — v0.5.0 runtime contract lifecycle

Status: completed for the published v0.5.0 release (2026-09-20). This is a
review of the surfaces the release adds, against the code in this tree; it is
not a third-party audit and not a hardware qualification. Report anything
that contradicts it through
[SECURITY.md](https://github.com/kubeneuron/kubeneuron/blob/main/SECURITY.md).

## Scope

v0.5.0 adds three surfaces: read-only runtime contract coverage (per node and
fleet page), the candidate runtime contract impact inside policy impact
previews, and evidence-only runtime contract qualifications. Route and
command details are in the [REST](reference-api.md) and
[CLI](reference-cli.md) references.

## No new execution authority

- None of the three surfaces is read by admission, incident handling, action
  dispatch, verification before resolve, or `GPUAutonomyPlan`. Coverage and
  candidate impact persist nothing; a qualification persists its own record
  and audit chain and nothing else reads it.
- There is deliberately no approve, promote, apply, enable, or delete route
  for a qualification or a candidate. A `ReadyForApproval` qualification is a
  statement of observed evidence, not a grant.
- No new CRD, RBAC rule, Helm value, or store migration ships with the
  release. The new routes sit behind the existing operator authentication.

## Mutations: strict decode, idempotency, atomic audit

- Qualification create and observe strict-decode their JSON bodies; unknown
  fields are rejected. Observe accepts only `actor` and an optional
  `resource_version`; the controller captures all evidence itself, so a client
  cannot supply coverage results.
- Both mutations require an `Idempotency-Key`. Reusing a key with different
  input is refused with `409`; a replay returns the stored resource with
  `Idempotent-Replay: true`. Observe is fenced on `resource_version`.
- The key reservation, the resource write, and the audit event commit in one
  store transaction. Either the caller gets a qualification whose key, row,
  and hash-chained audit entry all exist, or nothing was written. A store
  that cannot commit these atomically answers `503` instead of writing
  partially.

## Quotas

The mutations share the existing per-source, per-operation direct-access
quota. Create is limited to 20 per minute and observe to 30 per minute per
source; excess answers `429` with `Retry-After`. Reads are not additionally
limited beyond the existing operator API behaviour. A deployment may impose a
stricter gateway policy in front of the API.

## Frozen cohort and scope

- A qualification freezes an explicit cohort of at most 32 nodes, each bound
  by name and UID, against the profile identity (name, UID, generation,
  digest) and compiled configuration digest selected at creation. Later
  observations re-read live evidence but never rebind the cohort or profile;
  a node whose UID changes, or whose selection drifts, fails the sample.
- The qualification's tenant and cluster are derived from controller-owned
  node labels, never copied from the request, so a request cannot claim a
  scope its nodes do not carry.
- Fleet coverage pages accept `tenant`/`cluster` label filters as a
  filtering contract only; they are not a substitute for Kubernetes RBAC on
  the API.

## Leader fencing

Create and observe are fenced to the elected leader. A non-leader replica
refuses the mutation rather than writing from a stale view. Reads are served
by any replica from the store.

## Live label lookup fails closed

Profile selection is judged against the labels the Kubernetes `Node` object
carries now, read through the watch-maintained cache. A platform lookup error
is answered as `503` (unavailable), not as an `Uncovered` node, so a transient
API-server failure can never be recorded by an observation as permanent
drift. A deleted `Node` object resolves to an empty label set and selects no
profile. On a platform without label lookup (bare metal, tests) the stored
labels are used.

## Fleet page bounds

The fleet coverage route accepts `limit` between 1 and 500 (default 100) and
an opaque cursor; a request outside that range is rejected. A page fails as a
whole when one node's coverage cannot be built, rather than silently omitting
the row, and a store without accelerator report retention answers `503`
rather than an assessed-looking node.

## Limitations

- **No V0.5 GPU hardware qualification exists.** The AWS hardware harness
  runs of 2026-09-13 and 2026-09-14 exercised the shared agent/controller
  runtime and did not call the v0.5 routes or commands. The kind integration
  harness drives coverage, qualifications, and the candidate preview against
  a real controller and store, but on CPU-only nodes with synthetic
  accelerator evidence posted over the real agent identity. It proves the
  wiring, not the hardware.
- A qualification observes evidence produced by the agent. It cannot detect
  an agent that reports falsely; the agent boundary (mTLS, Pod-bound token,
  controller-served arming) is the control for that, unchanged in this
  release.
- This review covers the v0.5.0 additions only. Earlier surfaces are
  described in the [design document](design.md) and the security policy.
