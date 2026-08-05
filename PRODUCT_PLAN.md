# KubeNeuron Product Completion Plan

Status: **canonical product *plan*** (direction and sequencing). This document
supersedes the release ordering in `ROADMAP.md`.

> **⚠️ For current implementation status, `README.md`, `CHANGELOG.md`, and
> `PRODUCTION_READINESS_PLAN.md` are authoritative — not this file.** Some
> "current checkpoint" claims below are superseded as of v0.2.0: `executionMode:
> Enabled` is no longer unavailable (it is supported, off-by-default, confined
> by `spec.safety.destructiveExecution`); there are seven CRDs, not six; and
> cloud node remediation is validated on live EKS. The plan's *direction* stands.

## Product outcome

KubeNeuron is a Kubernetes-native control plane for detecting, diagnosing,
and safely remediating accelerator failures. It must turn telemetry and
node-local events into an auditable workflow that either restores a verified
healthy node/device or deliberately leaves it quarantined for a human.

The first production profile is NVIDIA Kubernetes clusters running the NVIDIA
GPU Operator. The core is accelerator-neutral: later profiles add AMD, Intel,
and TPU support without changing workflow, safety, audit, or Kubernetes
workload-control semantics.

`executionMode: Enabled` is now a supported, off-by-default mode confined by
`spec.safety.destructiveExecution` (a non-empty node selector plus an exact
acknowledgement), validated for cloud node remediation on live EKS. The one
door still deliberately closed is per-device *hardware* GPU reset, which a
virtualized instance cannot perform and which remains hardware-gated. Dry-run
stays the default for every other mode and node.

## Current checkpoint

The Kubernetes control plane is implemented and exercised: seven CRDs,
configuration compilation, controller/agent mTLS plus Pod-bound identity,
Alertmanager and agent ingestion, incident workflow, approvals, audit,
a durable controller action queue with conditional lease-token completion,
node cordon/drain integration, and an operator/API/CLI surface.

The agent has a `nvidia-smi`-backed driver for inventory, PCI attribution,
idle checks, reset, and bounded driver probing. It also has typed diagnostic,
bundle, guarded reboot, and allow-listed-script action contracts. A fsynced
action journal is now wired to the versioned lease protocol, alongside a
vendor-neutral accelerator contract and fail-closed NVIDIA adapter. This is
not yet a production agent runtime or hardware qualification: the current
distroless image does not provision the required NVIDIA or DCGM host tooling,
remediation scripts, or a qualified execution environment.

Recent durability foundations, still insufficient for Enabled mode:

- The SQLite event sink atomically archives each fresh agent event and creates
  a lease-based workflow outbox item; controller restart resumes unacknowledged
  items rather than losing them after an HTTP success response. The controller
  commits an outbox item's incident/audit mutation and acknowledgement in one
  transaction, so a crash cannot count the same item twice.
- Local action-journal fsyncs intent, the opaque lease token and expiry,
  running, known/unknown outcome, and reported state. It holds a node-local
  exclusive lock; recovery reuses only an unexpired persisted lease and
  converts an interrupted running action to `outcome-unknown`, blocking an
  automatic destructive retry.
- NVIDIA inventory, XID normalization, liveness health, and capabilities sit
  behind the accelerator contract. The `nvidia-smi` runtime reads current MIG
  mode on every preflight; physical reset is declared only when every GPU is
  explicitly unpartitioned. Unknown, failed, and MIG topology probes fail
  closed. It also reports a current, uniform driver version; a missing, mixed,
  or partial version response is blocked rather than replaced by a configured
  value. A configured GPU Operator/DCGM runtime version is display metadata
  only. A report can become `ready` only when bounded local `dcgmi --version`
  and `dcgmi discovery -l` observations prove a matching DCGM runtime and the
  same GPU count as `nvidia-smi`; the version must exactly match the
  server-selected profile. Missing, malformed, or mismatched probe output
  remains `degraded`. The current operator image does not provision `dcgmi`,
  so this is still a production-runtime blocker until its image and host
  tooling contract is completed.
- A versioned, agent-authenticated accelerator-report protocol now persists
  the newest observation per `(node, vendor)`. It carries stable inventory,
  driver/runtime versions, topology safety, semantic capabilities, readiness
  reasons, and an optional runtime-profile digest. Out-of-order and
  same-timestamp-conflicting reports are rejected; the endpoint is
  observation-only and never changes execution mode or authorizes an action.

The following gaps block production remediation:

1. The durable action protocol still lacks server-recorded attempts, executor
   boot identity, cancellation, and a production multi-controller recovery
   model. The current local journal/lease recovery is intentionally
   single-node-agent and fails closed when a second local process appears.
2. Verification currently proves an agent heartbeat and a quiet window, not
   DCGM/NVML health, expected device inventory, or successful diagnostics.
3. SQLite and a single Recreate controller are suitable for DryRun, not an
   enabled fleet with failover.
4. The operator API uses one bearer token and accepts an actor supplied in
   JSON; a production audit must derive the actor from an authenticated
   principal.
5. CPU-only kind tests and one-node platform smoke checks do not qualify GPU
   remediation, cross-node identity, action replay, or upgrades on hardware.
6. A `v1alpha1` `AcceleratorRuntimeProfile` now compiles into the immutable
   controller snapshot and gates physical NVIDIA reset on selector, digest,
   reviewed driver version, report age, readiness, declared capability, exact physical GPU, and
   verified-unpartitioned topology. The managed agent gets only its selected
   vendor/digest through an authenticated controller route and will not fall
   back to a local digest or publish a managed report when no profile selects
   it. The report also acknowledges the exact Kubernetes profile UID and
   generation, so a profile edit or recreation invalidates old evidence. This
   is intentionally incomplete: there is no runtime-asset/version attestation,
   and no other accelerator action is eligible through this initial gate.

## V1.0 boundary

V1.0 supports Kubernetes GPU clusters from 10 to 500 nodes on a declared
Kubernetes, NVIDIA driver, GPU Operator, DCGM, and agent-image compatibility
matrix. PostgreSQL is the production workflow authority; SQLite remains a
single-controller DryRun/development option.

V1.0 detection covers XID, ECC, row-remap, NVLink/PCIe, thermal/power,
driver-hang, missing-GPU/exporter, and stale-agent conditions. It provides
observe, workload eviction, cordon/drain, GPU reset, guarded reboot,
diagnostics, evidence collection, verification, quarantine, and an audited
hardware-escalation handoff.

SSH/BMC, Slurm, ClickHouse, automated ticket/RMA systems, and arbitrary
customer scripts are not V1.0 requirements. They may be introduced later as
separately reviewed adapters; none may bypass the typed-action safety model.

MIG, AMD partitions, Intel partitions, and TPU slices are never treated as
independent physical devices by default. A physical-device action is admitted
only when every affected partition is drained and the accelerator profile
declares that action safe. Unsupported partition topologies fail closed.

## Target architecture

```text
Kubernetes CRDs and policy
          |
          v
operator -> compiled runtime profile -> controller
                                         |
         durable incident/action state <-+-> PostgreSQL
                                         |
                         Kubernetes drain / audit / approvals
                                         |
               authenticated node agent + durable action journal
                                         |
                  accelerator adapter and verified host tooling

NVIDIA: NVML/DCGM/GPU Operator
AMD:    ROCm and AMD device-plugin profile
Intel:  Level Zero and Intel device-plugin profile
TPU:    provider-specific accelerator/node lifecycle profile
```

The controller owns generic incident state, correlation, workflow, safety,
approval, audit, queue leasing, and workload control. Vendor adapters own
inventory, failure normalization, health probes, diagnostics, capability
declarations, and the implementation of semantic actions.

Policies use semantic actions such as `drain`, `quarantine`, `reset-device`,
`restart-runtime`, `reboot-node`, `collect-diagnostics`, and `verify`.
An adapter declares which actions it supports. A playbook that requests an
unsupported action is rejected at compile time, rather than becoming a
best-effort command on a node.

The current `GPU*` v1alpha1 API remains compatible. A future v1beta1 adds an
accelerator-oriented API and migration/conversion path instead of breaking
existing NVIDIA users.

## Non-negotiable safety rules

1. Unit tests must never invoke host-side destructive commands. Every
   executor test injects a fake runner; CI runs rootless and without systemd.
   Reset/reboot tests require an explicit destructive-lab gate and a disposable
   hardware runner with an out-of-band watchdog.
2. Every risky action is typed, allow-listed, idempotent, capability-checked,
   audited, bounded by timeout/concurrency/cooldown, and eligible for
   approval.
3. The agent fsyncs intent and outcome records locally before a side effect
   and before reporting it. Ambiguous reset/reboot outcomes become `UNKNOWN`,
   never automatic retries.
4. A node remains cordoned/quarantined on failed or unverifiable remediation.
   The controller only uncordons a node it cordoned itself and only after
   verification succeeds.
5. Enabling automation is explicit, generation-bound, and revocable. A
   changed runtime profile, image, script digest, or node capability requires
   fresh preflight and approval.

## Delivery phases

### Phase 0 — Safety freeze and truthful release surface

- Commit the `boot_id`-required reboot guard and its regression test.
- Build a shared fake command runner for all executor tests; forbid direct
  host command execution from default test paths.
- Create a dedicated destructive hardware-test target requiring explicit
  environment gates, a known lab-node allowlist, and a watchdog.
- Make `PRODUCT_PLAN.md` the source of product status; reconcile README,
  design, docs, and Roadmap claims. Remove or quarantine retired plaintext
  Compose/systemd/static Kubernetes paths from supported installation docs.
- Complete open-source release hygiene: pinned CI tools, generated-file
  verification, SECURITY process, signed/SBOM image release workflow.

Exit: a normal developer command cannot reboot, reset, reinstall a driver, or
execute a host script; public documentation makes no unsupported claim.

### Phase 1 — Versioned accelerator runtime profiles

- Completed foundation: the controller accepts only authenticated-node,
  strict, versioned accelerator observations and durably retains the latest
  report per vendor. NVIDIA preflight is observation-only; fake drivers never
  identify as NVIDIA hardware, and unknown/MIG topology remains ineligible
  for physical reset.
- Completed foundation: cluster-scoped `AcceleratorRuntimeProfile` resources
  compile deterministically into the controller's immutable runtime snapshot.
  Same-vendor selector overlap is rejected, and a physical reset must satisfy
  the matching server profile (including its reviewed NVIDIA driver version)
  plus a fresh matching agent report. The profile has no execution-mode field;
  `Enabled` remains prohibited.
- Completed foundation: the managed DaemonSet requests its selected NVIDIA
  profile digest, UID, and generation from the authenticated controller and
  reports only with that binding. No selected profile, ambiguous selection, or
  controller lookup failure holds reporting; no action policy is exposed to
  the node. The controller gate rejects a report from any other profile
  generation.
- Completed foundation: every managed accelerator report is stamped with the
  authenticated Kubernetes Node UID and a reset gate compares it to the
  current Node object. Deleting and recreating a node with the same name
  invalidates all of the previous object's runtime evidence.
- Completed foundation: the profile pins an exact runtime/DCGM version, and
  bounded local `dcgmi --version` plus `dcgmi discovery -l` results must
  exactly match it and the independent `nvidia-smi` GPU count before a report
  can become ready. The current operator image does not yet deliver that
  reviewed executable or prove GPU Operator assets. Extend the profile with
  device-plugin resource names, supported versions, image/script digests,
  diagnostics, and an explicit partition policy. Graduate the API to
  accelerator-oriented v1beta1 with a migration path rather than breaking
  existing NVIDIA users.
- Persist a complete runtime attestation: device-plugin and DCGM versions,
  GPU Operator/runtime assets,
  physical-device/partition topology, and capability results. The current
  NVIDIA report already derives MIG mode and a uniform driver version from
  `nvidia-smi`; it does not yet attest the remaining runtime components.
- Publish generation-bound node and profile status. Nodes that fail preflight
  are visible but cannot receive action work.
- Implement the NVIDIA profile first and define explicit profile contracts
  for AMD, Intel, and TPU without claiming them supported yet.

Exit: the operator shows exactly why every node is Eligible, ObservedOnly, or
Blocked, and only Eligible nodes can be considered for Enabled mode.

### Phase 2 — Durable action protocol and real node executor

- Completed foundation: the agent fsyncs received, claimed, running,
  outcome-known, outcome-unknown, and reported transitions. It replays a
  known or unknown result only with a persisted unexpired lease; an expired
  lease requires a normal controller re-claim. It never renews an active lease
  by polling and never reruns an ambiguous action.
- Extend the server-side action protocol with attempts, executor boot ID,
  cancellation, renewal, and production failover semantics. Queue replay must
  attach to the same action rather than dispatch a second side effect.
- Create a hardened agent runtime that deliberately exposes the required host
  NVIDIA/DCGM tools. Host reboot and driver operations use a reviewed,
  explicit host-execution mechanism, not accidental container behavior.
- Pin and attest operator-provisioned scripts/assets by digest. Bundles have
  capacity limits, redaction rules, retention, and an optional secure upload
  path.

Exit: controller/agent crashes, network loss, duplicate polls, and lost result
posts cannot perform a reset or reboot twice.

### Phase 3 — Complete detection and verification

- Add NVIDIA NVML or DCGM event monitoring beside kmsg; add periodic inventory
  drift, driver, DCGM, and exporter probes. The initial NVIDIA adapter maps
  existing XID events and driver liveness only; it is not a DCGM replacement.
- Completed foundation: the SQLite transactional outbox commits each item's
  incident/audit mutation and `done` acknowledgement together. Promote this
  behavior to the production storage backend and test controller
  restart/failover processing and replay.
- Test each normalized failure class against pinned DCGM/GPU Operator labels
  and add source-heartbeat alerts so monitoring blind spots are visible.
- Implement post-action verification: expected GPU/partition inventory,
  driver responsiveness, DCGM health/diagnostics, idle state where required,
  metrics/event quiet window, and configured recovery thresholds.

Exit: every supported NVIDIA failure class has an evidence-backed detection
path and no incident resolves without health evidence, not merely a fresh
agent heartbeat.

### Phase 4 — Correct Kubernetes remediation semantics

- Preserve initial scheduling state and only undo KubeNeuron-owned cordons.
- Define PDB-aware drain behavior, workload ownership rules, timeout/failure
  handling, and GPU/MIG-targeted eviction semantics.
- Require approvals for destructive steps according to a vendor/profile
  decision matrix; facility, thermal, and power failures must not blindly
  trigger reset.
- Implement maintenance, pause, flap, cooldown, and concurrency behavior
  across controller restart and failover, not only in memory.

Exit: induced NVIDIA failures complete the documented ladder or safely stop
in `NEEDS_HUMAN` with the node protected from new workloads.

### Phase 5 — Production control plane and access security

- Implement PostgreSQL storage, migrations, backup/restore/PITR procedures,
  retention, leader election, and controller failover.
- Replace the operator bearer-token actor model with OIDC and/or Kubernetes
  RBAC roles. Persist the authenticated principal, role, request identity,
  and decision channel in audit entries.
- Add TLS for human-facing access, NetworkPolicies, least-privilege service
  accounts, signed images/SBOMs, vulnerability policy, and a review of the
  privileged node-agent threat boundary.
- Add automated certificate issuance/renewal convenience, expiry monitoring,
  auditable rotation, and a documented emergency revocation procedure.

Exit: a controller failover leaves no duplicate action, restore is rehearsed,
and every human mutation has a verifiable identity.

### Phase 6 — Accelerator expansion

- **AMD:** inventory and monitoring first; then adapter-specific diagnostics
  and only hardware-qualified remediation.
- **Intel:** inventory/telemetry and partition-aware health first; actions
  remain observed-only until qualified.
- **TPU:** use a provider-specific adapter. Initial remediation is workload
  evacuation, node quarantine, and provider workflow handoff; no generic GPU
  reset abstraction is assumed.
- Add one compatibility matrix, fault catalog, preflight suite, test lab, and
  operational runbook per vendor profile.

Exit: a new adapter can be installed without modifying core workflow code and
cannot expose unqualified actions.

### Phase 7 — Qualification, pilot, and GA

- Run multi-node self-hosted hardware CI: cross-node impersonation, agent and
  controller crashes at journal boundaries, partitions, queue replay, drain,
  reset, reboot, verification, and N-1 to N upgrades.
- Cover the declared NVIDIA GPU/driver/DCGM/Kubernetes matrix, including MIG
  or an explicit fail-closed exclusion.
- Run load and chaos tests for the target fleet size; test PostgreSQL outage,
  restore, certificate rotation, and audit durability.
- Complete dashboards, alerts, runbooks, dry-run demo, support process, and
  canary/pilot adoption. Hardware E2E must pass for two minor releases before
  Enabled is generally available.

Exit: a documented pilot proves the full signal-to-remediation flow on real
hardware and all V1 release criteria pass.

## Enabled admission gate

The operator may accept `executionMode: Enabled` only when all conditions are
true for the installation generation:

1. PostgreSQL is healthy and backup/restore has been verified.
2. Controller/agent images and remediation assets are pinned, verified, and
   match the approved profile digest.
3. TLS identity, certificate lifetime, and rotation state are valid.
4. Required GPU Operator, device-plugin, DCGM, metrics, and alerting
   dependencies are ready.
5. Every selected node has a current successful capability preflight and a
   durable agent journal.
6. The requested playbooks have supported actions and verification policies.
7. An administrator explicitly confirms the generation/profile digest.
8. Hardware qualification for that profile is green and no global/per-node
   pause or maintenance window blocks automation.

## Verification matrix

Every release requires unit, integration, Kubernetes, and hardware evidence:

- Rootless unit tests with fake command, filesystem, clock, and network seams.
- Envtest and multi-node kind tests for CRD validation, reconciliation,
  identity, upgrades, and deletion races.
- Failure injection at every durable action-journal and queue transition.
- Dedicated NVIDIA hardware tests for inventory, DCGM, injected signals,
  reset/reboot, drain/PDB, restart, and recovery.
- Per-vendor hardware qualification before any new adapter exposes Enabled
  actions.
- PostgreSQL failover, migration, restore, certificate rotation, scale, and
  security tests before GA.

## Delivery order and sizing

The immediate implementation order is: safety freeze; canonical docs;
queue/journal; runtime profile and preflight; NVIDIA agent image; detection
and verification; PostgreSQL/OIDC; multi-node hardware CI; pilot; only then
Enabled.

Completing NVIDIA V1.0 is roughly 25–35 engineering weeks with reliable GPU
lab access. A focused team of two Go engineers and one platform/SRE engineer
can run workstreams in parallel; one developer should plan for six to nine
calendar months. Each additional accelerator vendor normally adds four to
eight engineering weeks plus the availability of its own hardware test lab.
