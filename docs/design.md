# KubeNeuron — Design Document

Status: **accepted target architecture**, actively maintained.

Last updated: 2026-08-05 (v0.2.1)

> This document is the architecture and its invariants (see §2.4): the seams,
> the concurrency/lifecycle rules, and the reasoning behind them. It is kept
> in sync with the code — the stale-status freeze notice that used to sit
> here is gone because the claims below were re-audited against v0.2.1. For
> the release-by-release capability surface, `README.md` and `CHANGELOG.md`
> remain the quickest references; `PRODUCTION_READINESS_PLAN.md` tracks
> status-by-item.

KubeNeuron monitors NVIDIA GPU clusters for hardware and driver failures and
drives a configurable, audited escalation ladder. Kubernetes and bare metal are
the first target environments; other schedulers and machine types remain future
extensions.

Unless a section explicitly says that a path is implemented, this document
describes the target design. The Kubernetes control-plane paths (workflow walk,
action queue, operator API, CLI, approvals, authenticated registration) are
implemented; `executionMode: Enabled` is a supported, off-by-default mode gated
by `spec.safety.destructiveExecution`. Dry-run remains the default. Consult the
authoritative documents above before relying on any "implemented" or "rejected"
annotation in the sections that follow.

## 1. Goals and non-goals

### Goals

- Detect the GPU failure catalog (XID errors, ECC faults, row-remap
  exhaustion, thermal/power events, NVLink/PCIe degradation, and driver
  hangs) quickly enough for operational response.
- Select remediation from typed, declarative policy and playbook data.
- Automate only actions that pass dry-run, concurrency, cooldown, approval,
  verification, and audit controls.
- Keep desired Kubernetes configuration separate from durable incident and
  workflow state.
- Preserve seams for Kubernetes and bare-metal actuation without coupling the
  core incident model to one scheduler.

### Non-goals

- KubeNeuron is not a general node-health system; use established node tooling
  for unrelated disk, network, and kernel problems.
- It is not a scheduler or workload retry service.
- Hardware validation is NVIDIA-only. AMD detection ships unvalidated and
  Intel is a seam; the per-vendor grading lives in
  `docs/reference-capabilities.md`, which CI machine-checks against the
  code in both directions.
- The KubeNeuron operator does not own the lifecycle of VictoriaMetrics,
  Alertmanager, PostgreSQL, or ClickHouse. External installations or their
  dedicated upstream operators own storage, backup, and upgrades.

## 2. Architecture

### 2.1 Components and maturity

KubeNeuron builds four custom Go binaries. The operator is the Kubernetes
desired-state control plane; the controller is the runtime incident/workflow
control plane.

| Component | Runs on | Responsibility | Current state |
|---|---|---|---|
| **`kubeneuron-operator`** | Kubernetes control plane | Watch KubeNeuron CRDs, validate and compile configuration, reconcile runtime ConfigMaps and controller/agent workloads. | First controller-runtime reconciler exists. |
| **`kubeneuron-controller`** | Runtime control plane | Ingest signals, correlate incidents, select playbooks, apply safety gates, execute and verify steps, persist audit data. | Implemented: full state walk, durable action queue, approvals, escalation, transactional audit, operator REST API. |
| **`kubeneuron-agent`** | Every GPU node | Watch kernel events, publish inventory and signals, spool during outages, execute local actions. | Registration/events use mTLS plus Pod-bound Kubernetes identity. Kernel watcher with reopen, fsynced spool with replay dedup, action-queue polling, a node-local exclusive action journal (intent/lease/outcome recovery), nvidia-smi driver with idle-check, diag/bundle/guarded-reboot actions. |
| **`kubeneuronctl`** | Operator workstation | Inspect state, decide approvals, trigger supported operations, and pause/resume automation. | All declared commands implemented against the operator REST API. |

Supporting integrations are not KubeNeuron binaries:

| Integration | Target responsibility | Current repository state |
|---|---|---|
| dcgm-exporter / node_exporter | GPU and host metrics | A pinned GPU Operator profile supplies dcgm-exporter; node_exporter remains outside the reference dependency profile. |
| VictoriaMetrics / vmagent / vmalert | Metric storage, scraping, and rule evaluation | A pinned upstream-operator profile exists; the KubeNeuron operator does not create these systems. |
| Alertmanager | Group and route alerts to the controller and humans | The pinned profile routes to the implemented webhook; approval interaction does not exist. |
| SQLite | Single-process development store | Implemented for controller incidents/audit foundations. |
| PostgreSQL | Durable workflow store for HA operation | Implemented and operator-accepted: DSN from a mounted Secret, leader-elected controller pair, same conformance suite as SQLite. |
| ClickHouse | Optional raw-event archive | Future work. |
| Grafana | Dashboards | Development container only; dashboards are future work. |

### 2.2 Kubernetes API and operator reconciliation

The `kubeneuron.io/v1alpha1` API contains seven cluster-scoped kinds. Each schema
has a status subresource, and the reconciler publishes generation-bound
`Ready` status on the root installation and on every selected child
configuration kind.

| Kind | Desired state |
|---|---|
| `KubeNeuron` | Root installation: target namespace, images, execution mode, safety settings, workflow store, TLS references, and observability/archive integration declarations. |
| `GPURemediationPolicy` | Priority-ordered mapping from a normalized problem class to a playbook. |
| `GPUPlaybook` | Typed steps selected from a closed action enum, with approval and failure/escalation metadata. |
| `GPUSignalMapping` | Declarative XID / alert / neutral-fault-code overrides compiled into signal-mappings.yaml and applied by the detection catalog (label matchers rejected fail-closed). |
| `GPUMaintenanceWindow` | Time-bounded automation pause for selected nodes; compiled into windows.yaml and enforced by the reconcile walk (matchLabels only; matchExpressions rejected fail-closed). |
| `GPUNodeConfig` | Per-node settings compiled into node-configs.yaml; `paused` is the complete per-node pause set (SSH/BMC refs rejected fail-closed). |

Every configuration object uses `spec.kubeNeuronRef` to select its root
`KubeNeuron` object. The intended reconciliation boundary is:

```text
KubeNeuron + referenced configuration CRs
                  |
                  v
        validate supported subset
                  |
                  v
 deterministic policies/playbooks/config digest
                  |
                  +----> runtime ConfigMaps
                  +----> controller ServiceAccount, Service, PVC, Deployment, PDB
                  +----> agent ServiceAccount and DaemonSet
                  +----> root status conditions and observed digest
```

The current reconciler performs that core compile/converge path. It rejects
several values the runtime cannot consume rather than silently ignoring them,
defaults an omitted execution mode to `DryRun`, and rolls managed workloads
when the compiled digest changes. It continues to watch all seven kinds, and all seven now compile into the
runtime: policies/playbooks into their YAML files, maintenance windows into
`windows.yaml`, signal mappings into `signal-mappings.yaml`, and node
configs into `node-configs.yaml` — every file digest-covered so any change
rolls the controller. Unsupported sub-fields (matchExpressions, label
matchers, SSH/BMC refs) still fail closed.
SQLite state is mounted from an owned, grow-only PersistentVolumeClaim.
Readiness requires a Bound, fully resized claim and current observed
Deployment/DaemonSet generations; the agent readiness endpoint specifically
requires a durable controller registration acknowledgment within the
configured stale window, and the Pod propagates a failure after its next
10-second readiness probe. A versioned capability preflight prevents a new
narrow-payload agent from posting to a legacy full-node registration endpoint;
coordinated rolling-upgrade behavior is not implemented. Reconciliation
failures overwrite stale Ready state. The reconciler wires the authenticated
agent ingress described below, issues and renews the installation's TLS
material when `spec.tls.issuer: Operator` (foreign material is watched and
warned about, never replaced; per-workload TLS revisions roll exactly the
consumers that mount changed material), and publishes generation-bound Ready
status on every selected child configuration kind. External dependency
readiness (metrics stack, Alertmanager) remains owned by their upstream
operators; store backup is a controller API (`GET /api/v1/backup`), not an
operator-scheduled policy; deletion behavior beyond owner-reference garbage
collection is not implemented.

The preview runtime accepts credential-free `External` VictoriaMetrics and
Alertmanager declarations only. `Managed` remains an API-reserved value but is
rejected until upstream-resource discovery and readiness exist. ClickHouse may
only be omitted or explicitly disabled. KubeNeuron never creates these
dependencies; the version-pinned reference profile under
[`deploy/kubernetes/dependencies`](../deploy/kubernetes/dependencies/) is an
independent cluster-administrator workflow.

After verifying the separately owned dependencies, the KubeNeuron preview
install order is shown below. The generated CRDs use Kubernetes quantity CEL
functions and conservatively require Kubernetes 1.29 or newer.

```sh
kubectl apply -k config/default
kubectl apply -k config/samples
```

[`config/default`](../config/default/) installs the generated CRDs, RBAC, and
operator deployment. [`config/samples`](../config/samples/) contains a
development root/configuration set. Samples and image references must be
reviewed before use; installation success is not a production-readiness
signal.

### 2.3 Runtime data flows

The target metrics path is:

```text
dcgm-exporter / node_exporter / kubeneuron-agent metrics
  -> vmagent -> VictoriaMetrics -> vmalert -> Alertmanager
  -> POST /api/v1/webhooks/alertmanager on kubeneuron-controller
```

The Alertmanager receiver is implemented and authenticated: operator-managed
installations require a bearer token (`spec.notifications.webhookToken`), the
route fails closed when the token is unconfigured, and payload severity/node
fields are validated. Scraping, rules, and routing are still assembled
through the pinned upstream-operator profile rather than reconciled by the
KubeNeuron operator.

The target discrete-event path is:

```text
kernel /dev/kmsg + NVML event set
  -> kubeneuron-agent parses and classifies
  -> POST /api/v1/events on kubeneuron-controller
  -> durable incident/event handling
```

The HTTP event and registration receivers, kernel watcher, and on-disk spool
foundations exist. Registration uses a versioned narrow agent-owned payload,
returns success only after a SQLite upsert, server-stamps the heartbeat, and
drives agent readiness from a recent acknowledgment. It preserves
controller-owned node metadata and fails closed against a legacy full-node
registration route. Spool replay is bounded, FIFO, and leaves a failed tail on
disk. Registration and event transport authentication plus Kubernetes node
authorization exist on the operator path, but real NVML, immutable Node UID
persistence, and the complete durable event-processing contract do not. In
particular, HTTP 202 for an event is not a crash-safe commit guarantee:
archival failures are currently logged rather than returned to the agent, and
the in-process signal path can still drop work.

#### Agent identity boundary

The controller separates public HTTP on port 8080 from agent ingress on port
8443. Health and the Alertmanager webhook are registered only on 8080;
registration capability/heartbeat and events are registered only on 8443.
The agent listener requires TLS 1.3 and a client certificate signed by the
configured agent-client CA bundle. There is no plaintext redirect or fallback.

Four installation-local Secret references are required on the root CR. Their
`namespace` fields must be omitted; the operator always resolves them in
`spec.namespace`:

1. controller serving `tls.crt`/`tls.key`, with `serverAuth` and the exact
   controller Service DNS SAN;
2. the agent-client CA bundle;
3. a shared fleet client `tls.crt`/`tls.key`, whose current, non-CA
   `clientAuth` leaf is valid for at most 100 days and has exactly one URI SAN:
   `spiffe://kubeneuron.io/installation/<root-UID>/agent`;
4. the controller-server CA bundle used by agents.

CA references default to the key `ca.crt` and may select another key. Key-pair
references may not select a single key. The operator does not read or own these
Secrets, and it has no Secret RBAC. This design requires no service mesh,
cert-manager, or external PKI service; an installer may use an existing PKI or
generate installation-local CAs. Issuance is outside the operator today.

First installation therefore creates `spec.namespace`, installs the
CRDs/operator, creates the root with the intended four Secret names, and reads
the root UID. The installer then issues the server and fleet leaves, creates
the two key-pair and two CA-bundle Secrets, and lets the pending managed
workloads start. The kind harness performs exactly this bootstrap with
ephemeral local CAs.

The shared fleet leaf proves installation membership only. Node authorization
comes from a second proof on every request: a one-hour projected,
Pod-bound ServiceAccount token with audience `kubeneuron-controller`. The
agent rereads the token file for every request. The controller performs
TokenReview and requires the exact ServiceAccount identity and current UID,
then verifies the bound live Running Pod's UID, labels, ServiceAccount,
DaemonSet controller owner, node binding, and live Node. The JSON node name
must equal this server-derived principal; a mismatch is rejected before any
backend mutation. Missing/invalid credentials return 401, valid credentials
for the wrong identity return 403, and Kubernetes API infrastructure failures
return 503.

Both proofs are required. Server authentication prevents the projected bearer
token from being sent to an impersonated controller. The fleet certificate
gates installation membership but is shared, while the Pod-bound token
identifies the current workload/node but is not accepted without that
independent client-certificate gate.

Certificates and CA pools are loaded at process start. Routine rotation uses
immutable, uniquely versioned external Secrets and reference-driven process
rollouts; same-name Secret data changes and hot reload are unsupported.
`hack/tls-rotate.sh` advances exactly one server or client direction through
`TrustExpanded`, `LeafActivated`, and explicitly approved `OldTrustRetired`
annotations. The current transaction marker also binds the declared Secret
names, CA keys, and UIDs; it rejects opposite-direction/same-ID collisions,
plan substitution, phase-relevant target replacement under the same name/with
a new UID, and reuse of the currently recorded terminal ID. IDs must be
globally unique because the marker is overwritten by the next transaction.
It validates Secret shape, immutability, and non-ownership, then waits for the
exact workload Secret reference, rollout, root generation/phase, and root
readiness. A controller rollout is complete only after every managed agent's
durable registration acknowledgment sequence advances from a baseline taken
after the replacement is ready, avoiding cross-node clock comparisons.
Recorded phases are resumable convergence checks.
Ordered rollback works before contraction, while `rollback-retirement`
restores overlap trust after a failed final contraction before leaf/trust
rollback. Rollback validates only the remaining material it needs, and the
helper never owns or deletes input Secrets.

Routine server ordering is agents trusting old+new server CAs, controller
activation of the new server leaf, then agents trusting only the new server
CA. Routine client ordering is the controller trusting old+new client CAs,
agents activating the new fleet leaf, then the controller trusting only the
new client CA. Only one direction may be active in the helper at a time. Ref
changes naturally map server-leaf/client-CA changes to the controller
Deployment and client-leaf/server-CA changes to the agent DaemonSet. The
controller is a single `Recreate` replica, so its TLS phases cause real ingress
downtime; this is not zero-downtime rotation.

The helper is an operator-adjacent offline procedure, not an operator-enforced
transaction or candidate validator. Direct root edits can bypass it. It does
not parse candidate certificate chains before rollout, issue or renew
certificates, schedule expiry, implement CRL/OCSP, pin leaves, or orchestrate
emergency revocation. Bad material fails through workload/root readiness and
can be rolled back while overlap trust remains. Leaf renewal under an unchanged
CA does not revoke a stolen old leaf; effective revocation requires removing
the old CA and terminating all processes that loaded it. The future
controller-to-agent action RPC is also outside this authenticated HTTP
boundary.

The target remediation path is:

```text
signal -> correlate incident -> policy/playbook selection
  -> safety admission -> optional approval -> execute typed action
  -> verify health and quiet window -> resolve or escalate
  -> persist every transition and decision
```

This path is implemented for dry-run operation and exercised end to end by
`test/e2e`: the reconcile loop advances incidents through observe thresholds,
gate admission, approval parking, execution, verification quiet windows,
escalation, and flap quarantine, persisting every transition with its audit
row in one store transaction. Actions dispatch through a durable work queue
that the agent polls over its authenticated channel. Real NVML and most
agent-side action implementations remain outstanding, so only dry-run
deployments complete this path with meaningful effects.

The approval path parks an incident in `AWAITING_APPROVAL`, records an
authenticated decision and channel, and expires it after a TTL without ever
auto-approving. REST and CLI decisions are implemented; Slack and Web UI
delivery are not wired.

### 2.4 Platform and actuation seams

Actuation is split into three concerns:

1. **Inventory** — nodes and GPUs known to the runtime. Kubernetes inventory
   is based on native objects; bare metal uses inventory data and eventual
   agent registration.
2. **Workload control** — cordon, drain/eviction, and uncordon through the
   `internal/platform` interface. A Kubernetes implementation exists; wider
   workflow integration is incomplete.
3. **Node actuation** — typed local actions through `internal/actuator`.
   Agent RPC is the intended primary path; SSH and BMC/Redfish are future
   fallbacks and must remain allow-listed rather than accepting arbitrary CRD
   commands.

Deployment mapping:

| Concern | Kubernetes operator path | Bare metal / local path |
|---|---|---|
| desired configuration | cluster-scoped CRDs | YAML configuration files |
| agent | operator-managed DaemonSet | target: systemd unit; currently unsupported |
| controller | operator-managed Deployment | target: systemd/Compose; currently unsupported |
| metrics/alerting | external/upstream-managed services | Compose or external services |
| node discovery | Kubernetes API plus agent registration target | inventory file plus registration target |

Only the operator-managed Kubernetes column has the current authenticated
identity wiring. Legacy static Kubernetes, Compose, and systemd assets still
describe the retired plaintext/bare-metal path; current fail-closed binaries
do not form a supported runtime from those assets.

### 2.4a The action registry (`internal/action`)

Every action KubeNeuron can take — playbook-visible or runtime-only — is one
declarative record in a single registry, keyed by its wire string
(`platform.drain`, `agent.gpu_reset`, ...). A record declares everything the
rest of the system needs to know about the action:

- **Executor kind** (platform / agent / verify / notify) — controller dispatch
  derives from it, not from string prefixes.
- **Safety-gate class** (`GateAction`) — which concurrency cap the step
  consumes, including the reboot class.
- **`ForcesApproval`** — the compiler injects a required approval on the step.
- **`Destructive`** — the controller's blast-radius confinement
  (`spec.safety.destructiveExecution.nodeSelector`) refuses the step on any
  node it cannot prove is inside the declared scope. Quiesce is in this set:
  standing down DCGM and the device plugin degrades a node like a drain does.
- **`CapabilityGate`** — the runtime-evidence gate (today: NVIDIA reset
  requires a fresh, profile-matched accelerator report).
- **`CloudPrimitive`** — the provider capability a cloud action needs; the
  operator rejects a playbook whose configured provider lacks it.

The operator compiler, controller dispatch, playbook validation, safety gate,
and confinement all read the registry; none keeps a private copy of these
facts. Adding an action is one registry record plus its executor — tests pin
the CRD enum ↔ registry bijection and the exact destructive set.

### 2.4b The cloud provider seam (`internal/cloud`)

Node-scope remediation on virtualized instances (where a PCI-level GPU reset
is impossible — measured on EC2 g4dn) is a cloud concern behind a
provider-neutral seam. Each provider registers by name and owns three things:

1. **Its providerID scheme** — `InstanceID(providerID)` parses the
   Kubernetes node providerID in the provider's own format; the platform
   layer never learns any scheme.
2. **Its primitives** — `Recycle` (stop/start: tears down and re-establishes
   GPU passthrough in place) and `Replace` (terminate for the autoscaler).
3. **Its capability declaration** — static, credential-free, queried by the
   operator at compile time so a `RecycleNode`/`ReplaceNode` playbook is
   rejected when the configured provider cannot perform it.

Capabilities are provider-scoped; **viability is instance-scoped**.
`CheckRecycle` renders a per-instance verdict (typed
`cloud.ErrRecycleNotViable`) — the AWS provider refuses stop/start for
autoscaling-group members, whose group would terminate a stopped instance
mid-recycle. The controller consults it when a `recycle_node` step becomes
current and escalates at admission instead of asking a human to approve a
step that will fail by timeout; `RecycleNode` re-checks before issuing the
stop. The package imports only the standard library; SDKs are linked only by
the binaries that wire a provider. Adding a cloud is a new package plus one
registry line.

### 2.4c The fault envelope (`AgentEvent.Fault`)

XID stays the NVIDIA-native fault encoding for sources that observe a genuine
XID (the kmsg NVRM line, DCGM's last-XID field). Every other fault — the
nvidia-smi ECC/row-remap counter fallback today, AMD/Intel sources tomorrow —
travels as a vendor-neutral `FaultSignal{vendor, source, code, attributes}`
beside it. The invariants:

- **Exactly one identity per event.** Classification is Fault-first, and the
  controller rejects an event carrying both a nonzero XID and a Fault at
  ingest rather than interpreting the ambiguity.
- **The envelope is durable.** The controller acknowledges an event before
  classifying it and always classifies the row read back from the event
  outbox, so the fault envelope (and PCI address) are persisted columns, not
  request-scoped values. System tests cross this exact seam.
- **One policy surface.** `internal/detect/fault.go` maps `(vendor, code)`
  into the same `ProblemClass` vocabulary as the XID catalog, cross-source
  dedup keys on the shared class, and `GPUSignalMapping` overrides cover both
  encodings (`xidCodes` and `faults`), so remapping a condition applies to
  every source that observes it.

**Vendor seam ledger.** The data model is vendor-neutral, but a real second
accelerator vendor hits these NVIDIA-specific joints, in the order it would
break: (1) **fixed in v0.2.3** — `verifyRuntimeEvidence` used to read an
NVIDIA accelerator report for every GPU-class target, so a device-scoped
incident on any other vendor could not RESOLVE; a vendor with no runtime
adapter now verifies on the agent heartbeat, at reduced depth, and says so;
(2) arming
requires the concrete `*nvml.SMI` driver (constructor guard and served-arming
adoption); (3) the physical-reset gate is
`CapabilityNVIDIAReset`/`allowNVIDIAReset` only; (4) the device-holder
preflight reads an NVIDIA report, so it silently no-ops elsewhere; (5) the
quiesce stack manipulates NVIDIA components.

Joint (1) was qualitatively different from the rest: the others withhold a
CAPABILITY a second vendor does not have yet, while (1) broke the lifecycle
for detections that vendor genuinely produces. Detection for AMD shipped in
v0.2.2, which made it live rather than hypothetical — and it was found by
assessment rather than by any test, because no test asserted that an incident
of a vendor with no adapter could reach a terminal state at all. That absence
is the more useful lesson: the seam was checked for what it withholds, never
for what it strands.

The seam itself is also positioned lower than the decisions that need it:
`internal/accelerator` is consumed by the AGENT, which translates its model
into wire structs, while the CONTROLLER — where every vendor-dependent
decision is actually made — never imports it and answers `nvidia` at four
call sites. A second vendor therefore needs a dispatch point invented, not
merely an adapter written. None of this should be generalized speculatively;
this ledger exists so the cost is a known quantity when a vendor lands.

### 2.4d Concurrency and lifecycle invariants (rounds 7–9)

These are the rules the controller's correctness rests on. Violating any of
them reintroduces a defect a past review round removed.

- **The durable bit is truth; the gate is a projection.**
  `Incident.RemediationSlotHeld` is set atomically with the first EXECUTING
  transition and cleared atomically with the halting one; the in-memory
  `safety.Gate` refcounts are rebuilt from the bits on leadership
  acquisition. The gate never persists; the bit never caches. A new cap must
  follow the same shape.
- **Approvals are rounds.** Each park mints `Incident.ApprovalEpoch` and its
  `park_epoch`-stamped request record in one transaction; decisions inherit
  the round's request identity and epoch; resume consults only the current
  round. Epoch 0 is the pre-upgrade population and is never consulted as a
  round — such parks are migrated into round 1 and re-decided. The round
  also travels to the click: notifications render it, and a decision that
  carries the round it displayed (`park_epoch` in the decision body,
  `--round` on the CLI, automatic in the panel) is refused if a re-park has
  superseded it. A round-less decision binds to the current round.
- **`StateChangedAt` is the write-fence.** Signal ingest owns
  `SignalSeen`/`UpdatedAt` and never touches `StateChangedAt`; every
  field-level rewrite that changes an incident's playbook position without a
  state change (the quiesce rewind) must bump `StateChangedAt`, and
  `transition()`/`parkForApproval()` conflict when the fresh row's value
  differs from their caller's snapshot. This is what makes the janitors safe
  on their own goroutine.
- **Configuration is pinned per pass.** One immutable `RuntimeConfig`
  snapshot behind an atomic pointer; the walk pins one snapshot per advance
  (carried on the call-tree context, inherited by the step goroutine) and
  each janitor pass pins its own; unpinned callers read live. Never pin
  twice; never install piecewise.
- **Arming is served, and undo is always allowed.** The controller answers
  each v2 registration with the node's arming, computed by the same
  selector-vs-labels match as blast-radius confinement; the agent adopts it
  live and its executor consults it per dispatch. `restore_accelerator_host`
  executes even unarmed — it can only replay a prior quiesce's snapshot —
  so shrinking the blast radius can never strand a quiesced node. The
  arming-in-flight hold (in scope, agent freshly unarmed) is bounded by a
  propagation grace anchored to the FIRST hold observation, kept in memory
  and cleared on every transition — never to `StateChangedAt`, which
  pre-ages behind pauses, windows, and cooldowns.
- **Loaded configuration is identifiable.** The controller publishes the
  operator-compiled snapshot digest it is actually running (`/readyz`
  suffix, `kubeneuron_runtime_config_info`, `GET /api/v1/runtime-config`);
  a digest lagging `KubeNeuron.status.configDigest` is a rollout that has
  not landed. Playbook binding is open-time; the ONE exception is the
  narrow late-bind: an incident with NO playbook may be bound in OBSERVING
  when a matching policy appears (write-fence bump + `bind-playbook` audit
  row) — bound incidents are never re-bound.
- **MIG instances are not remediation units.** The physical GPU is. A
  `MIG-` instance UUID is refused by the reset preflight fail-closed;
  remediation targets the parent device or escalates to a human.
  Per-instance reset is rejected permanently, not pending: NVIDIA exposes
  no such primitive, and recovering at instance granularity means
  destroying and recreating instances — a MIG-manager lifecycle operation
  that changes what the scheduler advertises, which a remediation step
  must never do. The decided parent semantics
  ([mig-decision.md](mig-decision.md)) are: remediate the parent only with
  the parent's instances enumerated, only when EVERY instance is idle, and
  only with the blast radius (instances, pods, namespaces) named in the
  approval — opt-in, and today still refused because none of it can be
  validated without A100/H100 MIG hardware. Note the asymmetry this
  leaves: node-scope rungs (drain, reboot, recycle, replace) have no MIG
  awareness at all and destroy every instance on the node today.

### 2.5 Scale posture

The target is roughly 10 to 500+ nodes without changing the incident model.

- SQLite is the single-replica default; **PostgreSQL is an implemented,
  conformance-tested HA store** (shared `sqlcore` engine, advisory-locked
  migrations, no-double-lease semantics) with Lease-based leader election and
  readiness-follows-leadership, so an active/standby pair keeps exactly one
  writer. Failover loses only in-memory projections that are rebuilt from
  durable state (gate occupancy, evidence pins).
- The design avoids requiring a message broker: Alertmanager retry, the
  durable agent spool, and the controller-side event outbox cover their
  respective delivery boundaries, with system tests crossing the seams.

## 3. Detection catalog

Static XID classification lives in `internal/detect/xid.go`; rationale is in
[xid-catalog.md](xid-catalog.md). Alert rules live in
`configs/vmalert/gpu-rules.yaml`. `GPUSignalMapping` overrides the built-in
classification declaratively for all three encodings — XID codes, alert
names, and vendor-neutral fault codes — and is compiled by the operator into
`signal-mappings.yaml` for the detection catalog.

The table records intended default policy, not a claim that every response is
currently executable:

| Signal | Class | Target default response |
|---|---|---|
| XID 48, 95 (DBE / uncontained ECC) | critical hardware | drain-and-reset; recurrence -> RMA |
| XID 64 (row remap failed) | critical hardware | RMA evaluation without reset retries |
| XID 79 (fell off the bus) | critical hardware | drain -> approved reboot |
| XID 74 (NVLink) | hardware/link | drain-and-reset; recurrence -> reboot/cabling review |
| XID 119/120 (GSP) | driver | reset -> reboot -> driver remediation |
| XID 94 (contained ECC) | hardware, contained | restart affected workload |
| XID 63 (remap recorded) | informational | reset when idle |
| XID 13/31/43 (application-level) | workload | observe; recurrence can mark the GPU suspect |
| ECC DBE counters | critical hardware | drain-and-reset backstop |
| Remap pending/failure/budget | hardware | reset when idle or hardware escalation |
| Thermal/power events | facility | observe or drain; do not blindly reset the GPU |
| NVLink CRC / PCIe replay storms | link | observe, drain/reset, or ticket by policy |
| Exporter down / GPU count low / driver timeout | liveness | node triage ladder |
| Agent heartbeat stale | meta | notify and suppress unverifiable automated actions |

## 4. Remediation design

### 4.1 Incident state machine

The transition validator is implemented in
`internal/playbook/statemachine.go`:

```text
signal -> OPEN -> EVALUATING -> EXECUTING -> VERIFYING -> RESOLVED
            |          |             ^            |
            |          v             | approved   | verification failed
            |   AWAITING_APPROVAL ----+            v
            |          |                       EVALUATING
            |          v expired/rejected
            |    EXPIRED / NEEDS_HUMAN
            +-> OBSERVING -- threshold --> EVALUATING
```

Any active state may fail closed to `NEEDS_HUMAN`. The controller's periodic
reconciler performs this walk (see `internal/controller/reconcile.go`), with
long-running steps in per-incident goroutines and orphaned-execution recovery
after controller restarts.

### 4.2 Target escalation ladder

YAML and CRD playbooks describe the following target rungs:

1. **observe** — record and notify at a threshold.
2. **workload-restart** — evict only an affected workload.
3. **gpu-reset** — check idle state and reset through the agent.
4. **drain-and-reset** — cordon, PDB-aware drain, reset, verify, uncordon.
5. **reboot** — approval, drain, guarded reboot, verify, uncordon.
6. **driver remediation** — approval, controlled repair, reboot, verify.
7. **hardware escalation** — approval, quarantine, diagnostics, ticket/RMA.

The playbook loader and typed CRD compiler implement useful validation, but
execution and verification for this ladder are incomplete. In particular,
agent RPC, diagnostics, bundles, reboot, SSH, and ticket integration include
placeholders.

### 4.3 Safety mechanisms and implementation boundary

| Mechanism | Target behavior | Current foundation |
|---|---|---|
| dry-run | All side effects become auditable no-ops until explicitly enabled. | File config defaults to dry-run; omitted CR mode compiles to dry-run. |
| typed actions | Configuration cannot inject arbitrary node commands. | CRD action enum and compiler validation exist. |
| concurrency/cooldown | Bound simultaneous targets/reboots and repeated actions. | In-memory gate exists; unfinished workflow does not exercise the complete lifecycle. |
| flap detection | Repeated reopen cycles stop automation. | Detector exists; reconcile wiring is incomplete. |
| approvals | Risky steps wait for authenticated, expiring human decisions. | Store/manager foundations exist; routes and channels are not wired. |
| pause | Global and per-node maintenance controls fail closed. | In-memory gate and CRD shapes exist; operational API/CLI path is not complete. |
| idempotency | Retries reuse an action ID and replay prior results. | In-process action-ID single-flight and short-lived result cache exist; no durable crash-safe claim/result transaction exists. |
| audit | Persist actor, transition, action parameters/result, and dry-run state. | SQLite primitives and incident-open audit exist; full step audit is pending. |
| verification | Require agent/driver/DCGM health and a quiet window before resolve. | Configuration shape exists; execution is pending. |

The CR API reserves `Postgres` and `executionMode: Enabled` for future runtime
implementations; current operator validation rejects both. `Paused` is
supported as a second safety gate over compiled dry-run, but requires an
operator API token so it can be resumed only through an authenticated path.
The operator-managed runtime remains PVC-backed SQLite until a PostgreSQL
backend is implemented and tested.

## 5. Web UI

The intended React/TypeScript control panel has four surfaces:

1. Fleet and GPU health.
2. Incidents, audit history, and approvals.
3. Manual operations and pause/resume.
4. Validated, versioned configuration.

The authorization design uses `viewer`, `operator`, and `admin` roles and
requires every mutation to carry an audited actor. SSE updates, metrics proxy,
configuration editing, and OIDC are target capabilities.

Current state: `web/dist/index.html` is only a placeholder and `web/embed.go`
provides an embedding primitive. There is no frontend package and the
controller does not serve the embedded files. See
[web/README.md](https://github.com/kubeneuron/kubeneuron/blob/main/web/README.md).

## 6. APIs

| Route | Purpose | State |
|---|---|---|
| `POST /api/v1/webhooks/alertmanager` on 8080 | slow-path alert ingestion | implemented; bearer token required for operator-managed installations (`spec.notifications.webhookToken`); a directly started development binary may opt out only with explicit `-allow-insecure-webhook` |
| `POST /api/v1/events` on 8443 | agent event ingestion; replay-deduplicated by capture event ID | implemented; mTLS plus Pod/node authorization |
| `GET /api/v1/agents/register/narrow-v1` on 8443 | exact narrow-registration capability preflight | implemented; mTLS plus Pod/node authorization |
| `POST /api/v1/agents/register/narrow-v1` on 8443 | narrow agent registration/heartbeat; durable acknowledgment | implemented; mTLS plus Pod/node authorization |
| `GET /api/v1/agents/actions/lease-v1` on 8443 | atomically claims the oldest available action for the authenticated node | implemented; mTLS plus Pod/node authorization, opaque lease token, and RFC3339Nano lease-expiry header |
| `POST /api/v1/agents/actions/lease-v1/{id}/result` on 8443 | node posts a result for its current action lease | implemented; node ownership and unexpired lease token enforced |
| `GET /livez`, `GET /readyz` on agent port 9402 | process liveness and recent durable registration acknowledgment | implemented; not GPU health |
| `GET /healthz` | controller liveness | implemented |
| `GET /api/v1/incidents[/{id}]`, `GET /api/v1/nodes[/{node}]` | fleet and incident state incl. audit trail | implemented; operator bearer token, fail-closed when unconfigured |
| `POST /api/v1/incidents[/{id}/approve\|reject\|resolve]` | manual trigger, decisions, manual resolution | implemented; operator bearer token, audited actor |
| `GET/POST/DELETE /api/v1/pause` | global automation control | implemented; operator bearer token |
| `GET /api/v1/targets` | vmagent HTTP service discovery | implemented; operator bearer token |
| `GET /metrics` | controller Prometheus telemetry | implemented |
| `POST /api/v1/login`, `GET /api/v1/auth/oidc/*`, `GET /api/v1/session` | operator identity: password users (`spec.auth.users`) and OIDC, server-side sessions; decisions audited under the verified identity | implemented |
| `GET /api/v1/runtime-config` | identity and shape of the configuration live in this controller (`/readyz` carries the same digest unauthenticated) | implemented; operator bearer token |
| summary/stream proxy APIs | Web UI data beyond the incident/node reads above | planned |
| configuration/version APIs | validated UI administration | planned |
| per-role authorization (beyond authenticated-operator) | granular access control | planned |

Controller→agent actions flow through the durable store-backed work queue
above: the agent polls over its authenticated channel and posts results, so
no per-node listener or serving certificate exists.
`api/proto/agent/v1/agent.proto` records a possible future push-style
`ExecuteAction`/`GetHealth`/`GetInventory` gRPC contract; it is not wired.

`kubeneuronctl` implements `status`, `nodes`, `incidents[ show]`, `approve`,
`reject`, `resolve`, `remediate`, `pause`, and `resume` against the operator
REST API with bearer-token authentication.

## 7. Repository layout

- `api/v1alpha1` — Kubernetes API source types and generated deepcopy code.
- `config/crd/bases` — generated CRD YAML.
- `config/default` — operator CRD/RBAC/deployment Kustomize entry point.
- `config/samples` — development custom-resource examples.
- `config/policies` — the baseline policy pack (`kubectl apply -k
  config/policies`): a GPURemediationPolicy for every problem class the
  detectors can emit, and the GPUPlaybook ladders they bind to. Coverage is
  gated by `internal/operator/policy_pack_test.go`.
- `cmd/kubeneuron-operator` — operator process.
- `internal/operator` — CRD compilation, resource construction, and
  reconciliation.
- `cmd/kubeneuron-controller`, `internal/controller`, `internal/httpapi` —
  runtime controller and ingestion APIs.
- `cmd/kubeneuron-agent`, `internal/agent` — node process, watcher, spool, and
  executor foundations.
- `cmd/kubeneuronctl` — CLI surface.
- `internal/platform`, `internal/actuator` — scheduler and node-action seams.
- `internal/playbook`, `internal/safety`, `internal/store` — workflow model,
  guardrails, and persistence.
- `configs` — current file-based local/bare-metal policies, playbooks, and
  alert rules.
- `deploy` — static development Kubernetes, Compose, and systemd assets.
- `test/simulator` — synthetic event producer; it is not a full fake action
  executor.

## 8. Persistence and optional analytics

SQLite (on a `ReadWriteOnce` claim) and PostgreSQL are both implemented and
operator-accepted controller stores. PostgreSQL is the HA choice: the DSN
comes from a mounted Secret, the controller Deployment is stateless, and the
store backend passes the same conformance suite as SQLite (see §2.5 for the
honest scope of what that parity does and does not prove). Migration heads
travel in lockstep (sqlite 0020 / postgres 0011 as of v0.2.3).

The operator provisions a `ReadWriteOnce` claim for SQLite, defaulting to
`5Gi`. Reconciliation preserves API-selected/bound fields, permits only
storage-request growth, records omitted/empty/named storage-class intent, and
rejects intent changes or shrink attempts. The API requires a stable
`workflowStore.sqlite` parent and makes workflow-store type immutable so nested
transition validation cannot be bypassed by removal/re-addition. Storage
growth rolls the single controller and readiness waits for reported capacity
and cleared resize state. The claim is controller-owned by the root
`KubeNeuron`, so deleting that root triggers claim garbage collection; the
PersistentVolume reclaim policy then controls data retention. Backup, restore,
resize-failure recovery, and an explicit retain/existing-claim policy remain
future work.

ClickHouse remains optional. It may become useful for multi-year raw XID
analytics, fleet-wide hardware/firmware comparisons, or high-volume raw event
retention. It must never become the incident lock or workflow authority.

## 9. Delivery phases

### Current foundation

- Four build targets and package skeletons.
- Six `v1alpha1` CRDs and a first operator reconciliation path.
- TLS 1.3 agent HTTP ingress with fleet certificates and live Pod-bound
  Kubernetes authorization for registration/events on the operator path.
- File and CRD configuration validation, playbook model, state transitions,
  in-memory safety primitives, SQLite store, event ingestion, watcher/spool,
  and development deployment assets.

### Phase 1 — executable dry-run runtime

- Wire real NVML, agent RPC with authentication, controller state advancement,
  verification, supported action implementations, REST/CLI operations, and an
  approval channel.
- Prove retry/idempotency and audit behavior in an end-to-end dry-run suite.
- Add PKI issuance, candidate preflight, expiry/emergency-rotation status, and
  stronger operator-enforced transaction behavior; authenticate the
  Alertmanager and action-RPC boundaries, and continue hardening dependency,
  deletion/update, and Kubernetes upgrade behavior.

### Phase 2 — controlled risky actions and UI

- Add guarded reboot/repair paths, complete flap and maintenance integration,
  scheduled diagnostics, dashboards, and the authenticated control panel only
  after Phase 1 safety behavior is testable.

### Phase 3 — scale and breadth

- Evaluate PostgreSQL/HA, ClickHouse archival, Slurm, ticketing/RMA systems,
  OIDC/SSO, and predictive draining based on demonstrated operational needs.

## 10. Testing strategy

Existing unit coverage includes selected XID parsing/mapping, playbook/state
validation, safety behavior, kernel parsing, operator snapshot compilation,
PVC mutation rules, generation-aware runtime readiness, ownership collisions,
and stale Ready-condition clearing. Registration tests cover strict HTTP
acknowledgment, preservation of controller-owned node fields, staleness, and
loss/recovery transitions. A checked-in, CPU-only kind target installs and
establishes all seven CRDs, exercises 53 CEL cases across `KubeNeuron`,
`GPUSignalMapping`, and `GPUMaintenanceWindow`, and reconciles `GPUPlaybook` and
`GPURemediationPolicy` fixtures. It also verifies minimal operator RBAC, durable
registration-readiness loss/recovery, all 11 managed ownership references,
root readiness, collision non-adoption/failure status, recovery, and an
acknowledged no-op reconciliation on a digest-pinned Kubernetes v1.33.12 node.
Authentication coverage includes distinct bearer-token checks for the public
operator API and Alertmanager webhook, public/plaintext separation, valid and
invalid certificate chains, another installation URI, malformed,
wrong-audience and wrong-ServiceAccount tokens, deleted-Pod token revocation,
spoofed node payloads, a real managed-agent rogue-certificate readiness
failure/recovery, and TLS input-Secret UID/data-hash/non-ownership checks. The
same harness creates immutable versioned candidates, proves an invalid server
key-pair phase reports `Ready=False/RuntimeUnavailable`, rejects same-ID and
substituted-plan continuations, and rolls back even after the failed/unused
candidates are quarantined. A separate incompatible final-CA probe proves a
failed trust contraction can restore overlap trust and unwind in safe order.
It also proves explicit dual-leaf emergency recovery from an unavailable root
using externally provisioned leaves signed by the existing CAs; it does not
provide automatic expiry detection, issuance, CA recovery, or revocation. The
run then completes routine server and client trust expansion, leaf
activation, and old-trust retirement. It verifies exact generation/reference
convergence, fresh post-controller agent acknowledgments, workload-scoped
rollouts, terminal idempotence, bound plan/Secret UIDs, absence of Secret data
in last-applied annotations, new-identity success, and rejection of both
retired trust directions.
The verified cluster has one Kubernetes v1.33.12 node: it proves rejection of
an arbitrary mismatched node name, not node-A credentials against a real node
B, and it does not directly exercise the documented Kubernetes 1.29 minimum.

Still required:

- envtest coverage for API admission/CEL and reconciler deletion/update races;
- integration coverage from event ingestion through durable state transitions
  and dry-run action verification;
- authenticated agent action RPC, crash-safe event delivery, and action
  idempotency/replay tests;
- additional kind coverage for the minimum supported Kubernetes release,
  upgrades, deletion races, multi-node impersonation, mixed multi-node
  rotation, emergency revocation, and rotation-resume races;
- induced-failure E2E tests before any claim of production remediation.
