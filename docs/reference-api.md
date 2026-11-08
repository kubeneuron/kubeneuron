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
| `POST /api/v1/incidents/{id}/approve` | approve the pending step; body `{"actor","reason?","park_epoch?","impact_digest?"}` |
| `POST /api/v1/incidents/{id}/reject` | reject the pending step; body `{"actor","reason?","park_epoch?","impact_digest?"}` |
| `GET /api/v1/incidents/{id}/impact-preview[?round=N]` | the newest `impact-preview/v1` document of one approval round (the current round by default) — what the pending step and the disruptions downstream of it would touch (v0.8.0, below) |
| `POST /api/v1/incidents/{id}/impact-preview/recompute` | recompute the current round's preview; body `{"actor"}` and `Idempotency-Key` required; answers the new document (v0.8.0) |
| `POST /api/v1/incidents/{id}/acknowledge` | record operator custody without changing remediation state; body `{"actor","reason?","resource_version?"}` and `Idempotency-Key` required |
| `POST /api/v1/incidents/{id}/resolve` | manually resolve; current v0.4 controllers require `{"actor","reason?","resource_version?"}` and `Idempotency-Key` |

Decisions return `204 No Content`. `park_epoch` pins the decision to the
approval round shown to the human; a decision against a superseded round
is refused. `impact_digest` (v0.8.0) pins an approve to the impact
preview the human read: an approve whose digest is not the round's current
preview is refused with `409`, and under
`spec.safety.impactPreview.requireForApproval` an approve without a digest,
or while the round's preview is unavailable, is refused the same way. A
reject is never held back by the preview. The detail response carries
`impact_preview_digest` (and `impact_preview_unavailable` with its reason)
for the current round when a preview exists. With a Kubernetes credential the audit
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
| `GET /api/v1/nodes/{node}/readiness` | one live explainability response: snapshot, typed decision, reason codes, limits, evidence references and effective digest; plus an additive `topology` object (the device topology classification below) when the controller classifies device topology |
| `GET /api/v1/nodes/{node}/topology` | one live, read-only `topology-classification/v1` document: the node's device layout and whether device-scoped actions are eligible on it (v0.8.0) |
| `GET /api/v1/nodes/{node}/evidence` | redacted evidence-reference timeline for the current node decision |

Fleet readiness accepts `limit` (1–500), opaque `cursor`, `state`
(`Eligible`, `ObservedOnly`, `Blocked`, `Unknown`), `vendor`, `profile`,
`tenant`, `cluster`, and `stale=true`. Tenant and cluster filters map to the
node labels `kubeneuron.io/tenant` and `kubeneuron.io/cluster`; they are a
filtering contract, not a substitute for Kubernetes RBAC on the API itself.

### Device topology (read-only, v0.8.0)

`GET /api/v1/nodes/{node}/topology` classifies one node's device layout from
the evidence the controller already holds and says whether a device-scoped
action (a GPU reset) is eligible on it. The same document is embedded
additively as `topology` in `GET /api/v1/nodes/{node}/readiness`; fleet
readiness pages do not carry it. The only case in which the single-node
readiness response omits the field is a controller with no classifier wired
(older wiring); when a classifier is wired and its build fails, the whole
readiness request fails with the error below rather than silently dropping
`topology`. It explains; it never changes the readiness `decision` beside
it. The route is a read behind the ordinary operator `get` authorization: no
`Idempotency-Key`, leader fencing or per-source quota, nothing persisted,
nothing in admission changed by asking. The classifier can only narrow: a
reset the existing evidence gate refuses stays refused, and a `refused`
classification holds a reset that gate would have admitted.

An unknown node is `404`. A `503` is "could not assess", never a
classification: the workflow store cannot retain accelerator reports (it
does not implement the report store, checked before any read, so a known and
an unknown node answer the same `503`), the node or report read fails, or
`GET /api/v1/nodes/{node}/topology` has no classifier wired. A report store
that simply holds no NVIDIA report for the node is not an error: that is
ordinary evidence, classified `unknown`/`refused` with `ReportMissing`.

The same classification is also captured into every decision snapshot built
for a `reset-device` request with `physical-device` scope, whichever adapter
asked for it: a `POST /api/v1/simulations` with that action and scope, and
the shared-evaluator snapshot the live reset admission records (an incident
or autonomy-executed reset) before the reset gate's own narrowing step. The
snapshot carries it as `topology`, and a `refused` classification turns the
decision `Blocked` with reason `TopologyUnqualified` and required action
`review_device_topology`. It is consulted last and only narrows: it cannot
unblock a decision that any other check blocked, and node-scoped resets,
reboots, observe-class and diagnostic requests carry no classification and
are evaluated exactly as before.

| Field | Values | Meaning |
|---|---|---|
| `version` | `topology-classification/v1` | document version |
| `topology` | `whole-device`, `mig`, `time-sliced`, `dra-managed`, `mixed`, `unknown` | the layout found; only `whole-device` can be eligible. `dra-managed` is DRA in use (a slice or a claim for the node) with no device-plugin facet beside it; DRA beside a whole-GPU, MIG or time-slicing advertisement is `mixed`, as is MIG beside time-slicing or beside a whole-GPU advertisement under any `mig.strategy` other than `single`. `unknown` is missing, implausible or contradictory input and is always refused. Readiness never decides the layout, only the verdict: a `whole-device` layout whose agent report is fresh, verified-unpartitioned and inventoried but not `ready` stays `whole-device` with `per-pod` attribution and is `refused` with exactly `ReportNotReady` — the agent established the layout and cannot stand behind a device action, which is what every virtualized GPU node looks like (the hypervisor withholds the PCI reset, so the agent degrades its report; measured on EC2 g4dn, see the operations guide) |
| `device_actions` | `eligible`, `refused` | the verdict on device-scoped actions; `eligible` is exactly `whole-device` with no reasons, and `ReportNotReady` is the one reason that can stand beside a `whole-device` layout |
| `attribution` | `per-pod`, `device-wide`, `unknown` | how a fault on a device maps to workloads; `device-wide` under time-slicing, where every pod on the GPU is hit |
| `reasons` | stable codes in canonical order | `ReplicasLabelInvalid`, `MigStrategyLabelInvalid`, `MigPartitioned`, `TimeSliced`, `DraManaged`, `DraObservationInvalid`, `MixedTopology`, `ReportMissing`, `ReportStale`, `ReportNotReady`, `ReportNoPhysicalDevices`, `WholeDeviceCountMismatch`, `TopologyUnknown`; empty when eligible. `WholeDeviceCountMismatch` is a node whose `nvidia.com/gpu` advertisement counts a different number of devices than the fresh, ready, verified-unpartitioned report counts physical GPUs, with no partition, sharing or DRA facet beside it to explain the difference (the shape a time-slicing device plugin leaves on a node without GPU Feature Discovery labels: physical x replicas under the whole-GPU name and nothing else). It is refused as `unknown`; the cause is not inferred, so it is never reported as `TimeSliced` |
| `evidence` | | the normalized inputs: `resource_source` says where the advertisement came from and is `node` (the node object's own `nvidia.com/gpu`, `nvidia.com/mig-*` and `nvidia.com/gpu.shared` capacity and allocatable, counted beside it), `not-applicable` (the platform explicitly says it has no device-plugin advertisement, so the agent report is the whole of the evidence) or empty (unobserved: no observer, an unreadable or absent node, or an observer answering without a known source; always refused); then the GPU Feature Discovery label values the classifier reads (trimmed and bounded, never raw), and the NVIDIA agent report's observation time, topology safety, readiness, physical device and partition counts and freshness |
| `dra` | `state` is `disabled`, `not-observed` or `observed` | the Dynamic Resource Allocation observation, below |
| `summary`, `observed_at`, `inputs_digest`, `node`, `node_uid` | | a human rendering, the evaluation time, and a SHA-256 over the canonical normalized inputs (not the time), so identical evidence shares a digest and any changed input changes it |

DRA is opt-in through `spec.safety.topologyClassifier.dra.enabled` and off by
default: `dra.state` is then `disabled`, DRA is not consulted, and the
classification says so. When enabled the controller lists
`resource.k8s.io/v1` ResourceSlices (that version only, list only, bounded to
five seconds; beta versions are never consulted) and inspects the
`resourceClaims` entries in the Pod spec of the Pods scheduled on the node
through the core Pod list it already holds. No ResourceClaim object is ever read and nothing in
the group is written, so the conditional RBAC grant is `resourceslices`
get/list/watch alone. `observed` carries `api_version`, bounded
`resource_slices` and `devices` counts for slices published for this node by
`spec.nodeName`, `drivers` (the sorted list of distinct validated driver
names across those slices, at most eight, not a count), `pods_inspected:
true`, and `claim_pods`, `direct_claims` and `template_claims`; any slice or
claim reference for the node classifies it `dra-managed` (or `mixed` beside
a device-plugin facet). `not-observed` carries a short stable `reason` and
is always refused, because the configuration says DRA may be in use and
nothing proved otherwise. The reasons the Kubernetes platform currently
emits include `api-unavailable` (the apiserver answered the v1 list with
not-found, so it serves no DRA or only a beta version), `forbidden`,
`timeout`, `list-failed`, `pods-forbidden`, `pods-list-failed`,
`locality-unsupported` (a slice scoped by nodeSelector, allNodes or
perDeviceNodeSelection, which this version does not evaluate),
`locality-invalid`, `driver-invalid`, `malformed-object`,
`malformed-claim-reference`, `bounds-exceeded`, and
`dynamic-client-unavailable` or `platform-cannot-observe` for a controller
that cannot read the group at all; the list is not a closed set and the
reason is a bounded token, so read it as explanation rather than matching
on it. The classifier also re-checks every observation it is handed: an
`observed` answer from a version other than `resource.k8s.io/v1`, without
`pods_inspected`, with counts out of bounds or not adding up, or with too
many or invalid driver names, and a `disabled` answer carrying data, are
rendered as `not-observed` with the failed check as the reason (for example
`api-version-invalid`, `pods-not-inspected`, `slice-counts-inconsistent`)
and add `DraObservationInvalid` to `reasons`; the raw observation is never
rendered or digested.

This surface shipped in v0.8.0 (2026-11-08). It is unit-tested and was
exercised on one Tesla T4 (`hack/hw-e2e.sh test-impact`, where every
classification was `whole-device` refused with `ReportNotReady`, because a
guest cannot reset the GPU); nothing is claimed for any other device.
Controllers before v0.8.0 have no such route and their readiness responses
carry no `topology` field.

### Impact preview (v0.8.0)

With `spec.safety.impactPreview.enabled`, every park and re-park for
approval computes, under the policy `budget` (default 15s, at most 60s), a
durable `impact-preview/v1` record for `(incident, round)`: the node and its
[device topology classification](#device-topology-read-only-v08-development),
the pending step and the coordinating disruptions downstream of it
(`platform.drain`, `platform.evict_gpu_workload`), the workloads on the node
— `total`, `gpu_holders`, `namespaces`, `eligible_for_checkpoint`, and
`ineligible` by stable code (`not-opted-in`, `namespace-not-allowed`,
`drain-excluded`, `not-gpu-holder`, `class-skipped`, `policy-disabled`,
`group-invalid`, `group-too-large`, `no-disruption` when no coordinating
step lies downstream) — up to `maxItems` itemised workloads with their
checkpoint verdict and effective wait (the wait the downstream step would
actually grant: the policy grant bounded by that step's coordination
budget), checkpoint groups (`members_on_node`, `members_total` with
`members_total_known`: a group the controller could not list keeps the
on-node count as a lower bound and is flagged, never shown as a total),
device counts and attribution, the effective checkpoint policy, `limits`,
`warnings` (`TimeSlicedAttributionDeviceWide`, `DeviceActionsRefused`,
`ItemsTruncated`, `WorkloadContextNotResolved`, `GroupTooLarge`,
`GroupTotalUnknown`), and either `unavailable:
null` or `unavailable: {reason}` when it could not be computed
(`WorkloadListingFailed`, `WorkloadListingTimeout`, `NoPendingStep`,
`PayloadTooLarge`). `digest` is `sha256:` over the canonical document with an
empty digest field; a recompute is a new record with a new digest, and the
newest by `computed_at` is the round's current one. The preview counts only
what it listed: it never estimates lost or saved training progress.

Each itemised workload may carry `owner_chain` (its controller references
as `Kind/name`) and a `context` object filled by the **read-only context
resolvers** selected under `spec.safety.impactPreview.workloadContext`:
`batch_job` (the owning `batch/v1` Job: `completions`, `parallelism`,
`completion_mode`, `indexed`, `suspend`, `active`), `kueue` (the
`kueue.x-k8s.io` Workload admitting it: `workload`, `local_queue`,
`cluster_queue`, `priority`, `admitted_at`, `active`), and `ray` (the Pod's
`ray.io` labels: `cluster`, `node_type`, `group`, `ray_job`, and whether the
RayCluster object was found). A resolver answers one question — whose Pod
this is — through get/list only; it never writes, and the controller role
gains get/list/watch on that group exactly while the resolver is selected.
A CRD group the cluster does not serve, a refused read or a timeout is
reported as not observed, and the preview carries
`WorkloadContextNotResolved` among its warnings rather than failing. The
resolvers attribute nothing by name alone: a Job whose UID is not the Pod's
owner UID (a Job re-created under the same name) is `owner-replaced`, not
described; the Kueue Workload is looked up only by the Pod's controller
owner UID, and a queue-labelled Pod with no controller owner names its
queue with `workload_lookup: no-owner` rather than the namespace's first
Workload. None of
this is a claim that Kueue, KubeRay or any framework integrates with
KubeNeuron.

`GET …/impact-preview` is a read behind the operator `get` authorization;
`404` is a round with no preview, `503` a controller or store that retains
none. `POST …/impact-preview/recompute` is an operational mutation (operator
`update`, leader fencing, a per-source quota, `Idempotency-Key`); a replay
answers the same record with `Idempotent-Replay: true`. Both are audited on
the incident (`impact-preview-computed`, `impact-preview-unavailable`,
`impact-preview-recompute`). The notification channels carry a three-line
summary and the digest beside the round. A released controller has no such
routes and no `impact_digest` field; it rejects the field with `400`.

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
