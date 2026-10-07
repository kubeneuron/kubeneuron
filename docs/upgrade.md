# Upgrading and rolling back

This runbook covers upgrading KubeNeuron itself. Third-party dependencies
(GPU Operator, VictoriaMetrics stack) have their own pinned procedure in
[`deploy/kubernetes/dependencies/`](https://github.com/kubeneuron/kubeneuron/tree/main/deploy/kubernetes/dependencies).

The API is `v1alpha1`: minor releases may change it without conversion
support. Read the release notes for every version you skip.

## Fleet membership changed in v0.2.2 — check it before you upgrade

Through v0.2.1 a node joined the GPU fleet only if it advertised
`nvidia.com/gpu`. v0.2.2 replaced that with a vendor-neutral matcher so AMD
and Intel nodes stop being invisible — but that widened the set of machines
KubeNeuron may cordon, drain, taint and reboot, and the v0.2.2 notes did not
say so. If you upgraded to v0.2.2 already, treat the list below as an audit
you still owe yourself.

The matcher is now anchored to recognised vendor domains (`nvidia.com`,
`amd.com`, `intel.com`, `gpu.intel.com`, `habana.ai`, `aliyun.com`) rather
than to the shape of the resource name, so a third-party counter such as
`example.com/gpu-licence` no longer drags a CPU node into the fleet.

Before upgrading, print the fleet as the new matcher will see it:

```
kubectl get nodes -o json | jq -r '
  .items[]
  | select([.status.capacity | keys[]
      | select(test("^((nvidia|amd|intel)\\.com|habana\\.ai|aliyun\\.com)/(gpu|mig-)|^gpu\\.intel\\.com/"))]
      | length > 0)
  | .metadata.name'
```

Compare it against `kubeneuronctl nodes` on the running version. Any node
that appears only in the new list is newly in scope. Two things bound what
that means in practice, and both are worth confirming rather than assuming:

- Confinement still applies. A node is only eligible for destructive steps
  if it matches `spec.safety.destructiveExecution.nodeSelector`, so a newly
  visible node is not automatically actionable.
- Non-destructive protection does apply immediately — a newly visible node
  can be cordoned and drained.

If a node should never be touched, label it `kubeneuron.io/pause=""` before
you upgrade, or narrow `spec.safety.destructiveExecution.nodeSelector`
first.

A GPU resource from a domain not on the list is now ignored for fleet
membership and logged once by the controller:

```
kubectl -n kube-neuron logs deploy/kubeneuron-controller | grep 'unrecognised vendor domain'
```

If that line names a real accelerator on your fleet, open an issue — the
node is invisible to KubeNeuron until the domain is recognised.

## Before any upgrade

1. Take a fresh workflow-store backup (see
   [operations](operations.md#sqlite-workflow-store-backup-and-restore)) and
   verify the snapshot opens. Schema migrations are **forward-only**: an
   older controller refuses a database touched by a newer one, so the only
   rollback path for the store is a restore.
2. Note the running versions: `kubectl -n kube-neuron get deployment
   kubeneuron-operator -o jsonpath='{.spec.template.spec.containers[0].image}'`
   and the `spec.controller.image` / `spec.agent.image` on your root object.
3. Check `kubectl get kubeneurons -o yaml` status conditions are all
   `Ready=True` — never start an upgrade from a degraded installation.

## v0.4.0 migration and autonomy notes

v0.4.0 adds SQLite migrations **0021–0023** and PostgreSQL migrations
**0012–0014** for durable decision snapshots, candidates, previews,
diagnostics, simulations, autonomy records, idempotency records, and
hash-chained operational audit heads with tenant/cluster scope. They are applied automatically when the
new controller starts and are forward-only. Take and verify the backup in the
previous section before rolling the controller image.

The release also installs the additive `GPUAutonomyPlan` CRD. Existing
installations do not gain automatic device actions from that CRD or from the
database migration: the stock v0.4 controller has no hardware autonomy
executor and records simulation-only rollout observations. Review every new
plan as a Draft, attach a new frozen simulation, and obtain distinct approvals
after the controller is upgraded; do not copy an approval or a digest from an
older policy revision.

If tenant/cluster labels are used, verify their values on the managed nodes
before creating scoped v0.4 resources. Candidate previews, diagnostics,
simulations, and autonomy selection now enforce those labels as a scope
boundary; a mismatched request is refused rather than falling back to an
unscoped node.

## v0.5.0 runtime contract lifecycle notes

The GPU Runtime Contract Lifecycle — read-only runtime contract coverage, the
candidate runtime contract impact inside policy impact previews, and
evidence-only runtime contract qualifications — is the v0.5.0 scope, released
on 2026-09-20 and the latest published release; its tag, published images,
and release assets are available from the
[GitHub Release](https://github.com/kubeneuron/kubeneuron/releases/tag/v0.5.0).
(The manifests and chart in this source tree carry the unpublished v0.6.0
candidate pins; see [the v0.6.0 notes](#v060-checkpoint-coordination-notes).)
This section describes the upgrade and rollback posture of that scope. The
CPU-only kind integration harness drives the v0.5 routes and commands against
a real controller and store with
synthetic accelerator evidence, alongside the existing reconciliation, RBAC,
mTLS, TLS rotation, backup/restore, restart, and cordon/uncordon scenarios.
No GPU hardware run has called the v0.5 routes, and v0.5.0 claims no
hardware qualification of its own surfaces.

- **No schema migration.** The migration heads stay at SQLite 0023 and
  PostgreSQL 0014. A qualification is a new kind
  (`runtime-contract-qualification`) inside the existing operational
  resource table, previews gain additive JSON fields, and coverage persists
  nothing. The upgrade is images only. Take and verify the backup from
  [Before any upgrade](#before-any-upgrade) anyway; that is standard
  practice, not a sign that the store changes.
- **No new CRD, RBAC rule, or Helm value.** The new read-only routes
  (coverage, qualification list and get) reuse the existing operator `get`
  authorization on the root `KubeNeuron` object and nothing else: no leader
  fencing, idempotency key, or per-operation quota applies to them. The two
  qualification mutations reuse the existing `update` authorization, leader
  fencing, idempotency, and per-source quotas. The agent protocol and
  capability token are unchanged, so agents need no coordinated rollout for
  this scope.
- **The prior binary does not understand the new surface.** A v0.4.0
  controller answers `404` on `/api/v1/nodes/{node}/runtime-contract`,
  `/api/v1/runtime-contracts/coverage`, and
  `/api/v1/runtime-contract-qualifications*`, and `kubeneuronctl
  runtime-contracts` / `runtime-qualifications` fail against it. It does not
  know the qualification kind, its `ReadyForApproval`/`Invalidated`
  lifecycle, or the `runtime_contract_impact` preview fields. During a
  PostgreSQL HA rolling update readiness follows leader election, so the
  Service sends every request to the elected leader: while that leader is
  still the old binary, the new routes answer `404` for everyone, and they
  appear only once an upgraded replica holds the lease. A not-yet-upgraded
  Pod addressed directly answers `404` until the rollout completes.
- **Images-only rollback is possible** because the schema did not change.
  After rolling back, qualification rows become invisible: the old
  controller does not serve them and cannot observe them, so no expiry is
  recorded while it runs. The rows stay in the store and are served again
  after a re-upgrade; the first observation after that records `Expired` if
  the window has passed. The old retention sweep does not recognise the
  `Invalidated` state, so such rows persist until the re-upgrade; rows
  already recorded as `Expired` are subject to the ordinary data-retention
  prune under both binaries once `expires_at` is older than the retention
  window, exactly as other terminal v0.4 summaries are. Export the
  active qualifications (`GET /api/v1/runtime-contract-qualifications`)
  before rolling back if you need their evidence on record.
- **Legacy candidate-preview semantics after rollback.** Preview payloads
  are decoded without rejecting unknown fields, so an old controller reads
  previews written with `runtime_contract_impact` and ignores the field.
  But previews the old controller **creates** follow its own v0.4.0 rule:
  it evaluates a candidate runtime profile against the report captured
  before deployment, so a candidate profile identical to the live one can
  read `Eligible`/`unchanged` there, and those previews carry no
  `runtime_contract_impact`. Under the v0.5 scope the same candidate lands
  in `newly_blocked` with `post_deploy_attestation: "FreshRequired"`,
  because a pre-deploy report is never candidate attestation. Do not compare
  previews across that boundary as if they used one rule; the presence or
  absence of `runtime_contract_impact_version` tells you which rule applied.

## v0.6.0 checkpoint coordination notes

> v0.6.0 is **prepared in source, not released**. The manifests, chart, and
> samples in this source tree carry the v0.6.0 candidate pins, but the
> `v0.6.0` tag, images, install manifest, and release assets do not yet
> exist; the latest published release is v0.5.0, which does not contain
> this feature. This section describes the upgrade and rollback posture of
> the checkpoint coordination scope so that it is written down before the
> release rather than after. Nothing here claims GPU hardware validation.

The v0.6.0 scope is one opt-in feature: `spec.safety.checkpointCoordination`,
which lets opted-in Pods be told and given a bounded deadline before a
`Drain` or `EvictGPUWorkload` step disrupts them
([design](checkpoint-coordination-design.md),
[operations](operations.md#checkpoint-coordination-opt-in-v060-scope-unreleased)).

- **Order: CRDs → operator → controller.** The CRD adds the optional
  `checkpointCoordination` block with its CEL bounds; the operator compiles
  it and, when enabled, grants the controller `patch` on Pods; the controller
  is what reads the compiled policy and coordinates. Applying the block
  before the CRD is upgraded is rejected as an unknown field; applying it
  before the operator is upgraded compiles to nothing; enabling it before the
  controller is upgraded grants a verb the old binary never uses. None of
  those orders is harmful, but only the documented one makes the feature
  take effect.
- **Default off, backward compatible.** An installation that does not set
  the block behaves exactly as before: the compiled snapshot digest is
  unchanged, the controller pre-phase returns on a single boolean, and the
  controller ClusterRole keeps exactly `get, list, watch` on Pods. Nothing
  arrives by upgrade alone. No schema migration is involved.
- **RBAC changes.** Static: the operator ClusterRole
  (`config/rbac/operator_role.yaml`, mirrored by the Helm chart) gains
  `patch` on core `pods`, unconditionally, because RBAC escalation prevention
  means the operator must hold a verb before it can delegate it. The
  operator never patches a Pod itself. Conditional: the managed
  `<name>-controller` ClusterRole gains `patch` on `pods` only while the
  policy is enabled, and loses it on the next reconcile after it is
  disabled. Review the static change with whoever owns cluster RBAC before
  the operator upgrade.
- **Rollback.** Set `checkpointCoordination.enabled: false` (or remove the
  block) **before or while** rolling back to a release that does not
  understand it. An older operator ignores the unknown field and compiles no
  policy, an older controller waits for nothing, and the older operator's
  next reconcile rewrites the controller role's rules to its own read-only
  shape, so a stale `enabled: true` is not dangerous. Disabling first is
  still the clean order: it stops coordination through a path both binaries
  understand, it keeps the stored object honest about what is in force, and
  it avoids a window in which the CR says enabled while nothing enforces it.
  Request annotations already stamped on
  Pods (`kubeneuron.io/checkpoint-*`) are durable history: harmless, read by
  nothing once the policy is off, and needing no cleanup. Images-only
  rollback is sufficient; the store is untouched by this scope.
- **Release rehearsal (run locally, pre-release).**
  `hack/kind-upgrade-rollback.sh` (`make test-upgrade-rollback-kind`, and the
  `upgrade-rehearsal` job of the release workflow) drives v0.5.0 → HEAD →
  v0.5.0 (images only) → HEAD on one kind cluster for both workflow stores.
  It was run as part of `make gates-full` on 2026-09-29/30 UTC against an
  earlier local, unpublished v0.6.0 candidate and passed on both SQLite and
  PostgreSQL. It seeded and read legacy API and data across the cycle,
  the v0.5.0 binary read the qualification rows HEAD wrote back
  byte-identically, and audit chains were preserved. For this scope it
  enabled a short `checkpointCoordination` policy on HEAD with the
  installation's own namespace as the allowlist and proved that the managed
  `<name>-controller` ClusterRole has no `patch` on Pods by default, holds
  exactly `get, list, patch, watch` only while the policy is enabled, and is
  back to exactly `get, list, watch` after disabling it and before the images
  are rolled back. The script now also asserts that neither toggle rolls or
  restarts a Pod: it snapshots every managed controller and agent Pod's UID
  and container restart counts before the policy is touched and requires
  them unchanged after enable and again after disable. That assertion was
  added after the 2026-09-29/30 run, which did not check it; the current
  candidate must rerun the rehearsal before it is tagged, and that rerun is
  the evidence for the claim. CPU-only, with synthetic accelerator evidence
  and no real checkpointing workload; it proves the upgrade, RBAC, and
  rollback wiring, not the protocol against a training job, and no real
  Pod + Downward API scenario has been run.

## Upgrade order

Always: **CRDs → operator → controller/agent images.**

1. **CRDs and operator** (from the release's install manifest):

   ```sh
   kubectl apply -f kubeneuron-install-vX.Y.Z.yaml
   kubectl -n kube-neuron rollout status deployment/kubeneuron-operator
   ```

   CRD schema additions are backward-compatible within `v1alpha1`; the
   operator tolerates older stored objects. CEL rules added by a release
   apply to *new* writes only — existing stored objects are not re-validated
   until modified.

2. **Controller and agent images**: edit the root object to the released,
   digest-pinned images:

   ```sh
   kubectl patch kubeneuron <name> --type=merge -p '{
     "spec": {
       "controller": {"image": "ghcr.io/kubeneuron/kubeneuron/controller:vX.Y.Z"},
       "agent":      {"image": "ghcr.io/kubeneuron/kubeneuron/agent:vX.Y.Z"}
     }
   }'
   ```

   On SQLite installs the controller Deployment uses `Recreate` with a
   single replica: expect a short public-API/webhook outage while the new
   Pod starts (Alertmanager retries deliveries). PostgreSQL HA installs run
   two replicas with rolling updates and leader-following readiness, so
   there is no such gap. The agent DaemonSet rolls node by node; each agent
   turns Ready only after a fresh durable registration acknowledgment from
   the new controller.

   Upgrading **to v0.2.2 or later** rolls the controller Deployment and the
   agent DaemonSet once each even with unchanged TLS material: the TLS
   digest became per-workload (a trust expansion now rolls only its
   consumers), so both `kubeneuron.io/tls-digest` annotations change format
   on the first reconcile. One-time, expected, and safe.

3. **Verify**: root `Ready=True`, controller `/metrics` serving, agent
   DaemonSet fully available, and a synthetic signal walks to an incident
   (see [quickstart](quickstart.md) step 5).

## Version skew

The agent posts with an exact versioned capability token: an upgraded agent
against an older controller (or vice versa) fails closed on registration
rather than corrupting inventory. Complete the controller rollout before or
together with the agent rollout inside one release; do not run mixed
versions longer than a rolling upgrade needs.

## Rolling back

- **Images only** (no schema migration in between): patch the root object
  back to the previous digests. The store is untouched.
- **After a schema migration**: the older controller will refuse the newer
  database (`database schema version N is newer than this binary`). Scale
  the controller to zero, restore the pre-upgrade snapshot onto the state
  PVC, then roll the images back. Incidents and audit rows written after the
  backup are lost — that is the RPO of your backup schedule.
- **Operator/CRDs**: re-apply the previous release's install manifest.
  Kubernetes does not remove already-stored fields; older operators ignore
  spec fields they do not know, and compilation stays fail-closed. The v0.4
  `GPUAutonomyPlan` CRD may remain installed during a rollback; old
  controllers do not consume it. Pause or roll back every active v0.4 plan
  and preserve its audit export before restoring an older store snapshot.
- **v0.5 runtime contract scope**: images only, no store restore needed;
  see [the v0.5.0 notes](#v050-runtime-contract-lifecycle-notes)
  for what the old binary can and cannot see afterwards.
- **v0.6 checkpoint coordination scope** (candidate in source, unreleased):
  disable
  `spec.safety.checkpointCoordination` first, then images only; see
  [the v0.6.0 notes](#v060-checkpoint-coordination-notes). Stamped
  `kubeneuron.io/checkpoint-*` annotations need no cleanup.

## Certificate material during upgrades

Upgrades do not rotate TLS Secrets. If an upgrade window coincides with a
planned rotation, finish the rotation first — `hack/tls-rotate.sh` phases
record exact root generations and will refuse to continue across an
unexpected generation change.
