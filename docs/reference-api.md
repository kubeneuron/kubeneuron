# REST API reference

The controller serves two listeners. Everything here is the **public
listener** (default `:8080`; plain HTTP unless
`spec.tls.publicServerSecretRef` is set). The **agent listener** (`:8443`,
TLS 1.3 with mandatory client certificates plus a projected Pod-bound token)
carries only agent registration, events, action polling, and accelerator
reports; it has no human-facing routes and is not documented for direct use.

Authentication: every `/api/v1/*` route below requires an
`Authorization: Bearer` credential or a server-side session except the
Alertmanager webhook, which requires the separate webhook token. Two
bearer credentials are accepted:

- **A Kubernetes credential (recommended, managed installations):** any
  bearer token the API server can verify — `kubectl create token <sa>`, an
  OIDC user token. The controller resolves the caller with `TokenReview`
  and authorizes with `SubjectAccessReview`: read routes require RBAC `get`
  and mutating routes RBAC `update` on the root `kubeneurons.kubeneuron.io`
  object of this installation. Audit rows record the verified principal
  (e.g. `system:serviceaccount:ops:sre-bot`); any `actor` in the body is
  ignored.
- **The shared static operator token (break-glass):** the `actor` body
  field is then required and recorded as `token:<actor>` — visibly a
  self-asserted claim, not a verified identity.

Two interactive sign-ins issue server-side sessions instead of a bearer
header: **password users** declared in `spec.auth.users`
(`POST /api/v1/login`) and **OIDC**
(`GET /api/v1/auth/oidc/login` → provider →
`GET /api/v1/auth/oidc/callback`). `GET /api/v1/session` returns the
current session's identity; audit rows record the verified user.

Critical v0.4 operations additionally require Kubernetes TokenReview group
membership. A browser session and the shared static token intentionally carry
no such groups and cannot approve an autonomy plan or request Extended
diagnostics.

Without a configured operator token the operator API is disabled entirely
(fail closed).

## Health and metrics

| Route | Auth | Purpose |
|---|---|---|
| `GET /healthz` | none | liveness («ok») |
| `GET /metrics` | none | Prometheus metrics (see the [metrics reference](reference-metrics.md)) |
| `GET /`, `GET /ui/` | none (static) | embedded control panel; its API calls still need the operator token |

## Ingestion

| Route | Auth | Purpose |
|---|---|---|
| `POST /api/v1/webhooks/alertmanager` | webhook token | Alertmanager `webhook_config` receiver; firing alerts become signals |

## Incidents

| Route | Purpose |
|---|---|
| `GET /api/v1/incidents` | list; filters `?state=OPEN,EXECUTING&node=<node>&limit=<n>` |
| `GET /api/v1/incidents/{id}` | detail including the audit trail |
| `POST /api/v1/incidents` | manual remediation trigger; body `{"node","class","actor?","gpu_uuid?","gpu_index?"}` — `node` and `class` are required; `actor` only with the static token |
| `POST /api/v1/incidents/{id}/approve` | approve the pending step; body `{"actor","reason?","park_epoch?"}` |
| `POST /api/v1/incidents/{id}/reject` | reject the pending step; body `{"actor","reason?","park_epoch?"}` |
| `POST /api/v1/incidents/{id}/acknowledge` | record operator custody without changing remediation state; body `{"actor","reason?","resource_version?"}` and `Idempotency-Key` required |
| `POST /api/v1/incidents/{id}/resolve` | manually resolve; current v0.4 controllers require `{"actor","reason?","resource_version?"}` and `Idempotency-Key` |

Decisions return `204 No Content`. `park_epoch` pins the decision to the
approval round shown to the human; a decision against a superseded round
is refused. With a Kubernetes credential the audit
actor is the authenticated principal and the body `actor` is ignored; with
the static token the claim is recorded as `token:<actor>`.

## Fleet

| Route | Purpose |
|---|---|
| `GET /api/v1/nodes` | registered nodes with GPU inventory and heartbeat age |
| `GET /api/v1/nodes/{node}` | one node's detail |
| `GET /api/v1/nodes/{node}/accelerators` | latest per-vendor accelerator runtime reports |
| `GET /api/v1/targets?port=<p>` | Prometheus `http_sd` target groups for registered nodes |
| `GET /api/v1/report/recovery?window=<Go duration>` | recovery report aggregated from the incident store over a trailing window (default `168h`, max `8784h`): degraded and recovered GPU-hours, unattended share, MTTR by class, incidents still open. See [`kubeneuronctl report`](reference-cli.md#report--what-the-fleet-got-back) for what each number counts. A controller that cannot compute it answers `503`, never an empty report |

## Control

| Route | Purpose |
|---|---|
| `GET /api/v1/pause` | current global pause state |
| `POST /api/v1/pause` | pause all automated remediation (big red button) |
| `DELETE /api/v1/pause` | resume |

## Remediation intelligence (v0.4.0)

The v0.4 operational paths use one side-effect-free, versioned decision
evaluator. A response that contains a decision also contains its evaluator
version, configuration digest, explicit expiry, reason codes, limits, and
redacted evidence references. A snapshot is immutable once captured: a newer
agent report or policy produces another snapshot instead of changing an old
preview, simulation, incident, or audit result.

Every mutable route in this section requires an `Idempotency-Key` header
(1–256 bytes). Reusing a key with different input returns `409`; a successful
retry returns the same durable resource and sets `Idempotent-Replay: true`.
Mutations of an existing resource also take `resource_version` from the last
GET; `409` means reread rather than overwrite a newer lifecycle state.

The controller applies a per-source, per-operation quota as a last-resort
direct-access guard (`20/min` for upload/preview/plan creation, `30/min` for
diagnostics and simulation, `60/min` for small lifecycle operations). A
deployment may impose a stricter gateway policy. A quota response is `429`
with `Retry-After`.

Three surfaces below — runtime contract coverage, the candidate runtime
contract impact inside previews, and runtime contract qualifications — form
the **GPU Runtime Contract Lifecycle** scope of v0.5.0, released on
2026-09-20. They are unit-tested and driven by the
CPU-only kind integration harness against a real controller with synthetic
accelerator evidence; no GPU hardware run has called them. A v0.4.0
controller answers `404` on the new routes and
its previews carry no `runtime_contract_impact`. All three are statements
about evidence: none of them is read by admission, incidents, action dispatch,
verification before resolve, or `GPUAutonomyPlan`, and none adds a CRD, RBAC
rule, or store migration.

### Readiness and evidence

| Route | Purpose |
|---|---|
| `GET /api/v1/readiness` | stable node-name ordered fleet page with `items`, optional opaque `next_cursor`, and evaluator version |
| `GET /api/v1/nodes/{node}/readiness` | one live explainability response: snapshot, typed decision, reason codes, limits, evidence references and effective digest |
| `GET /api/v1/nodes/{node}/evidence` | redacted evidence-reference timeline for the current node decision |

Fleet readiness accepts `limit` (1–500), opaque `cursor`, `state`
(`Eligible`, `ObservedOnly`, `Blocked`, `Unknown`), `vendor`, `profile`,
`tenant`, `cluster`, and `stale=true`. Tenant and cluster filters map to the
node labels `kubeneuron.io/tenant` and `kubeneuron.io/cluster`; they are a
filtering contract, not a substitute for Kubernetes RBAC on the API itself.

### Runtime contract coverage (read-only, v0.5.0)

| Route | Purpose |
|---|---|
| `GET /api/v1/nodes/{node}/runtime-contract?vendor=<v>` | one live `runtime-contract-coverage/v1` assessment of how the configured `AcceleratorRuntimeProfile` set covers this node for one vendor |
| `GET /api/v1/runtime-contracts/coverage?vendor=<v>` | stable node-name ordered fleet page of the same assessment: `items`, `coverage_version`, optional opaque `next_cursor` |

`vendor` (`nvidia`, `amd`, `intel`, or `google`) is required on both routes:
coverage is defined per (node, vendor) pair and the fleet page never picks a
vendor silently. The fleet route also accepts `limit` (1–500, default 100),
the opaque `cursor` from a previous page, and the `tenant`/`cluster` label
filters used by fleet readiness. Both routes require only the ordinary
operator `get` authorization: no leader fencing, `Idempotency-Key`, or
per-source, per-operation quota applies to them, because that quota guards
mutations. A missing or unknown vendor, a bad `limit`, or an invalid
cursor is `400`; an unknown node is `404`; an inventory entry without a name
fails the whole fleet page with `422`; a controller whose store cannot retain
accelerator reports, or whose node or report read fails, answers `503` rather
than presenting a node as assessed. A fleet page also fails as a whole when
any one node's coverage cannot be built; a row is never silently omitted.

The result is a pure function of live evidence — evaluation time, node
identity (name and UID), node labels, vendor, agent heartbeat, the latest
retained (node, vendor) accelerator report, and the compiled profiles — and
the same input always yields the same result. Nothing is persisted and
nothing in admission changes.

| Field | Values | Meaning |
|---|---|---|
| `selection` | `Exact`, `Uncovered`, `Ambiguous`, `Invalid` | how the configured profiles select this node and vendor: exactly one, none, more than one, or a profile/input that cannot be evaluated |
| `attestation` | `FreshCompatible`, `Missing`, `Stale`, `Mismatch`, `NotApplicable` | whether the retained report attests the selected profile; `NotApplicable` whenever selection is not `Exact` |
| `verification_depth` | `Full`, `Reduced`, `Unavailable` | how much of the runtime contract can be verified right now; an observation about evidence, never an authorization |
| `profile_name`, `profile_uid`, `profile_generation`, `profile_digest` | | identity of the selected profile; present only when `selection` is `Exact` |
| `config_digest`, `evaluated_at`, `node_name`, `node_uid`, `vendor` | | the compiled configuration and the exact node identity the report was matched against; `node_uid` is always present, even when blank |
| `agent_last_seen`, `report_observed_at` | | the timestamps the assessment observed; omitted when zero |
| `reasons`, `summary` | | stable codes in canonical order and a human rendering; a `Full` result has no reasons |

Verification depth follows from the other two axes and the agent heartbeat.
The heartbeat bound is the same five-minute evidence bound that verification
before resolve uses; report freshness is the selected profile's
`max_report_age`.

- `Exact` selection, a `FreshCompatible` report, and a usable heartbeat is
  `Full`.
- `Exact` selection with a `FreshCompatible` report but a heartbeat that is
  stale or in the future is `Unavailable`: the evidence contradicts itself,
  so it is not downgraded to `Reduced`.
- `Exact` selection without a fresh compatible report, or `Uncovered`
  selection, is `Reduced` while the heartbeat is usable and `Unavailable`
  otherwise. A missing report is ordinary evidence (`Missing`), not an error.
- `Ambiguous` or `Invalid` selection is always `Unavailable`.

Reason codes, in the order results list them: `EvaluationTimeMissing`,
`NodeIdentityMissing`, `VendorInvalid`, `ProfileInvalid`, `ProfileOverlap`,
`ProfileNotFound`, `ReportMissing`, `ReportInvalid`, `ReportNodeMismatch`,
`ReportVendorMismatch`, `ReportProfileDigestMismatch`,
`ReportProfileRevisionMismatch`, `ReportDriverVersionMismatch`,
`ReportRuntimeVersionMismatch`, `ReportObservedInFuture`, `ReportStale`,
`ReportNotReady`, `ReportRejected`, `AgentNeverSeen`,
`AgentHeartbeatInFuture`, `AgentHeartbeatStale`. Attestation records every
observable discrepancy rather than the first one, then defers to the
profile's own `CheckReport` gate before reporting `FreshCompatible`. Node
identity is compared with the same exact-equality rule as reset admission: a
report from a previous Node object that reused the name does not attest the
current node.

### Candidate configurations and policy impact

| Route | Purpose |
|---|---|
| `POST /api/v1/candidates` | upload one strict `CandidateConfiguration`, `AcceleratorRuntimeProfile`, or `GPURemediationPolicy` document without applying it to Kubernetes |
| `GET /api/v1/candidates`, `GET /api/v1/candidates/{id}` | list/get normalized immutable candidate metadata and compiler result |
| `DELETE /api/v1/candidates/{id}` | logical revocation; preserves normalized bytes and audit history, but blocks new previews |
| `POST /api/v1/candidates/{id}/preview` | evaluate the candidate against one captured fleet inventory snapshot |
| `GET /api/v1/candidates/{id}/preview`, `GET /api/v1/previews`, `GET /api/v1/previews/{id}` | retrieve deterministic, reviewable deltas |

Upload uses `application/yaml`, `application/x-yaml`, `text/yaml`, or
`application/json`, is limited to 1 MiB, rejects unknown schema fields and
multiple YAML documents, and has no Kubernetes write path. It stores the
original and normalized digests, compiler version, actor, expiry and optional
tenant/cluster scope. `expires_at` on upload is RFC3339; omitted means a
24-hour candidate lifetime. The candidate compiler has no Kubernetes client or
write credential; deployments needing content scanning should put their
malware/content scanner in front of this endpoint.

Durable resource list routes (candidates, previews, health checks,
simulations, and autonomy plans) accept `limit`, opaque `cursor`, `state`,
`tenant`, `cluster`, `since`, `until`, and `include_expired`.
`since`/`until` are RFC3339 creation-time bounds. A preview records the
candidate digest, inventory snapshot ID, evaluator version, and per-node
before/after reason/limit deltas; stale or incomplete inventory fails with
`422` rather than returning a plausible partial change.

#### Candidate runtime contract impact (pre-deploy, static, non-authorizing; v0.5.0)

Every per-node delta in `newly_eligible`, `newly_blocked`,
`changed_observed_only`, and `unchanged` carries a `runtime_contract_impact`
object, and the preview carries `runtime_contract_impact_version`
(`candidate-runtime-contract-impact/v1`) and `runtime_contract_profile_change`.
Both are absent on previews persisted before this field existed, including
every preview a v0.4.0 controller created. The impact is a pure function of
the candidate profile set and the captured node name, labels, and vendor
identity. That vendor identity is taken from the captured report, and it is
the **only** thing the captured report contributes: the report's attestation
content (profile digest and revision, driver and runtime versions,
readiness), its observation time, and the agent heartbeat are not inputs and
never prove a candidate. The impact never states that a candidate is
deployed, attested, or qualified: `assessment` is always `PreDeployStatic`.
It distinguishes two different things by design — the **static pre-deploy
selection** the candidate profile set would make, which can be computed now,
and the **fresh post-deploy attestation** only a report produced after
deployment can supply, which cannot.

| Field | Values | Meaning |
|---|---|---|
| `profile_change` | `NoProfileChange`, `ProfileSetReplaced` | whether the candidate contains any accelerator runtime profile |
| `static_selection` | `NotEvaluated`, `Exact`, `Uncovered`, `Ambiguous`, `Invalid` | how the candidate profile set alone selects this node and vendor; `NotEvaluated` only for `NoProfileChange` |
| `candidate_profile_name`, `candidate_profile_uid`, `candidate_profile_generation`, `candidate_profile_digest` | | identity of the selected candidate profile; present only when `static_selection` is `Exact` |
| `post_deploy_attestation` | `FreshRequired`, `NotRequired`, `NotApplicable` | the attestation the node would need after deployment; a requirement, never a status |
| `captured_report_usable_as_candidate_attestation` | always `false` | a report captured before deployment cannot attest a candidate profile, even when UID, digest, and versions match the live profile |
| `after_decision_evidence` | `LiveProfileWithCapturedReport`, `CandidateProfileWithoutReport`, `NoProfileWithCapturedReport` | exactly which profile and report the hypothetical `after` decision was evaluated from |
| `reasons`, `summary` | | stable codes in canonical order (`NoCandidateProfiles`, `CandidateProfilesPresent`, `VendorUnknown`, `ProfileInvalid`, `ProfileOverlap`, `ProfileNotFound`, `CapturedReportPredatesCandidate`) and a human rendering |

The `after` decision follows the impact:

- `NoProfileChange` (a policy-only candidate) keeps the live profile and the
  captured report, so the policy-impact decision is the live decision under
  the candidate digest. `post_deploy_attestation` is `NotRequired`.
- `Exact` selection evaluates the candidate profile with the captured report
  withheld. The `after` decision therefore fails closed (`Unknown` with
  `EvidenceStale`) and can never be `Eligible` from pre-deploy evidence; a
  candidate profile identical to the live profile still lands in
  `newly_blocked` with `post_deploy_attestation: "FreshRequired"`. This is the
  one case where an `Unknown` `after` answer is a deliberate candidate result
  rather than a `422` incomplete inventory.
- `Uncovered`, `Ambiguous`, or `Invalid` selection keeps the captured report
  but no profile: the existing observation-only fallback, which reads
  `ObservedOnly` and lists no allowed actions. `post_deploy_attestation` is
  `NotApplicable`; the impact never claims `FreshCompatible` or `Full`.

None of this touches live admission, runtime contract coverage, or
qualifications, and there is no route that deploys, applies, promotes, or
approves a candidate.

### Diagnostics, simulation, and incidents

| Route | Purpose |
|---|---|
| `POST /api/v1/health-checks` | create a durable `Passive`, `Quick`, or `Extended` health check |
| `GET /api/v1/health-checks`, `GET /api/v1/health-checks/{id}`, `GET /api/v1/nodes/{node}/health-checks` | inspect bounded diagnostic runs and outcomes |
| `POST /api/v1/health-checks/{id}/cancel` | cancel an unleased diagnostic action with idempotency/version fencing |
| `POST /api/v1/simulations`, `GET /api/v1/simulations`, `GET /api/v1/simulations/{id}` | create and inspect a no-side-effect frozen remediation graph |
| `POST /api/v1/incidents/from-simulation` | create/attach the durable incident using the frozen simulation, rationale, decision snapshot and evidence links |

`Passive` captures existing evidence and never queues device work. `Quick` is
bounded to 10 minutes; `Extended` is bounded to 30 minutes and requires an
open maintenance window plus a Kubernetes TokenReview identity in both the
`diagnostics-extended` and `disruption-budget-approver` groups. The
server derives both grants from that identity; request JSON cannot assert
them, and a static token or browser session receives `403`. Queued runs use
the existing durable agent action queue, so loss,
timeout, cancellation, global pause, and unsupported work end in an explicit
auditable state rather than an implicit success. Generic health-check responses
contain a safe summary plus an evidence digest, never raw agent command output.

Simulation never calls an executor or writes a Kubernetes resource. Its
ordered steps show locks, dependencies, expected evidence, and every gate that
would block the requested effect. A simulation-to-incident request records the
operator rationale and can be retried safely with its idempotency key.

### GPUAutonomyPlan

| Route | Purpose |
|---|---|
| `POST /api/v1/autonomy/plans`, `GET /api/v1/autonomy/plans`, `GET /api/v1/autonomy/plans/{id}` | create/list/read a time-bounded autonomy envelope |
| `POST /api/v1/autonomy/plans/{id}/simulation` | attach the exact permitted simulation before approvals |
| `POST /api/v1/autonomy/plans/{id}/approve` | add one role-bound approval; required roles must be distinct subjects |
| `POST /api/v1/autonomy/plans/{id}/pause`, `/resume`, `/rollback` | first-class fenced lifecycle transitions with a reason |
| `GET /api/v1/autonomy/plans/{id}/rollout` | canary/bake/expansion observations and effect references |

A plan has a non-empty selector, one action class, immutable `policy_ref` and
`profile_ref` (`name@sha256:…` or `name#generation`), fresh required sources,
positive quotas, `no_active_incident=true`, two distinct approval roles, and an
expiry. REST defaults a missing expiry to 24 hours; the Kubernetes CRD requires
one explicitly. The attached simulation must be permitted **and** match the
plan action, target scope, tenant/cluster boundary and configuration digest.
The live canary evaluator also compares the current captured digest to that
binding; `ConfigurationChanged` blocks an effect instead of reusing a prior
approval after a policy/profile revision.

For `POST .../approve`, the JSON `role` selects one already-required plan
role; it never grants that role. The caller must use a Kubernetes bearer
credential whose TokenReview groups contain that exact value, and the stored
actor is the verified TokenReview subject rather than the JSON `actor`.
Shared tokens and browser sessions cannot approve. Device-scoped autonomy
freezes the target device ID from the permitted simulation, retains each
deterministic effect record through plan expiry, and considers a
hardware-qualified bake successful only after fresh post-effect evidence passes
the shared evaluator.

Before each canary or expansion effect the controller captures and evaluates a
new snapshot. Expansion is one bounded batch followed by its bake period, not
a shortcut from a green canary to all selected nodes. Pause, expiry, missing
evidence, global stop, an incident/ownership conflict, failed effect, or an
error-budget breach prevents a later effect in that reconciliation cycle.

The stock controller intentionally has no hardware autonomy executor. Its
rollout is visibly `simulation-only`; it records would-be effects but never
touches a device. A production hardware effect requires an explicitly wired,
hardware-qualified adapter that accepts the persisted deterministic effect ID
as its idempotency key. The REST/CRD contract does not itself certify any
driver/runtime combination.

### Runtime contract qualifications (evidence only, v0.5.0)

| Route | Purpose |
|---|---|
| `POST /api/v1/runtime-contract-qualifications` | freeze an explicit node cohort against its currently selected accelerator runtime profile and start observing it |
| `GET /api/v1/runtime-contract-qualifications`, `GET /api/v1/runtime-contract-qualifications/{id}` | list/read qualifications with their frozen bindings, bounded observation history, and read-time effective state |
| `POST /api/v1/runtime-contract-qualifications/{id}/observe` | re-read inventory and coverage for the frozen cohort and record one observation |

Create strict-decodes `{"actor","nodes":[…],"vendor","tenant?","cluster?",
"requirements":{"min_samples","min_duration"},"expires_at"}`. `min_duration`
is a positive Go duration string (`30m`, `12h`); `expires_at` is RFC3339 and
must leave at least five minutes after the earliest instant the qualification
could become ready. The cohort is at most 32 nodes, every member must be
`Exact` against the same profile and compiled configuration, and the tenant
and cluster are derived from the controller-owned node labels, never copied
from the request. Observe strict-decodes only `{"actor","resource_version?"}`;
the controller captures all evidence itself. Both mutations require an
`Idempotency-Key`, are fenced to the elected leader, and share the
per-source quotas above (`20/min` create, `30/min` observe).

Each observation re-runs the runtime contract coverage assessment above for
every frozen member. A sample counts only when **every** node is `Exact`
against the frozen profile, `FreshCompatible`, and `Full`; the initial
coverage recorded at creation is not a sample. The qualification becomes
`ReadyForApproval` once `successful_samples` reaches `min_samples` **and**
`min_duration` has elapsed since the first successful sample. The lifecycle
is `Observing` → `ReadyForApproval`, ending in `Invalidated`
(any binding drift, or a non-Full observation after readiness) or `Expired`.
`ReadyForApproval` is evidence a human may consider in a separate, later
workflow: there is deliberately no approve, promote, apply, enable, or delete
route, and nothing in admission, incidents, actions, or `GPUAutonomyPlan`
reads a qualification.

Expiry is decided by the wall clock but persisted only by an observation, so a
qualification can be stored as `ReadyForApproval` after its window closed.
Every read therefore adds `evaluated_at`, `effective_state`, `expired`,
`expiry_pending`, and `ready_for_approval`, computed at read time without a
write: past `expires_at` a stored `ReadyForApproval` reads
`effective_state: "Expired"` and `ready_for_approval: false`, and
`expiry_pending: true` says the next observation will record it. Read
`ready_for_approval`, never `state`, to decide whether evidence is current.
Responses present `requirements.min_duration` as a duration string.

### Audit explorer and backup

| Route | Purpose |
|---|---|
| `GET /api/v1/audit-events` | globally ordered, opaque-cursor-paged append-only v0.4 audit events; filters `kind`, `resource_id`, `tenant`, `cluster`, `actor`, `request_id`, `decision_id`, `action`, `since`, `until` |
| `GET /api/v1/backup` | streams a transactionally consistent SQLite snapshot (`VACUUM INTO`); see [operations](operations.md#sqlite-workflow-store-backup-and-restore) |

Operational audit events form a per-resource SHA-256 chain (`prev_hash`,
`hash`) and are stored separately from raw diagnostic payloads. The API never
renders raw diagnostic blobs in these generic responses.

## Not implemented

Slack interactive approvals, SSE streaming, a metrics query proxy, and
versioned config editing are design targets that do **not** exist;
the API deliberately does not advertise them.
