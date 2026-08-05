# KubeNeuron Production Readiness Plan

Status: **execution plan**, created 2026-07-25 from a full independent audit of
commit `afa7399`. This document turns the strategy in
[PRODUCT_PLAN.md](PRODUCT_PLAN.md) into an ordered, checkable work plan and
adds the concrete defects found in the audit. When this plan and
`PRODUCT_PLAN.md` disagree on ordering, this plan wins; when they disagree on
scope or safety rules, `PRODUCT_PLAN.md` wins.

Sizing: **S** ≈ up to a day, **M** ≈ days, **L** ≈ a week+, for one developer
familiar with the codebase.

## Superseded checkpoint (2026-07-25)

> [Historical snapshot from the original audit; see `CHANGELOG.md` (v0.2.1)
> for current state — tagged releases and signed images exist, and confined
> destructive remediation is validated on live EKS.]

- `make build`, `go vet ./...`, and `go test -race ./...` are green on a fresh
  checkout (~30 packages).
- The DryRun control plane, mTLS/Pod identity, SQLite outbox/leases, the
  action journal, TLS rotation tooling, and the kind harness are real and
  well-tested.
- Production remediation is unreachable by construction: five independent
  gates (CEL, compiler, hardcoded DryRun in the compiled config, an executor
  whose destructive mode has no production wiring, and a DaemonSet with no
  NVIDIA tooling). The `Fake` NVML driver reports success for `ResetGPU`.
- There is no shippable artifact: CI publishes no images, no git tag exists,
  and every documented install path references images nothing builds.

## Post-audit safety closure (2026-07-31)

These corrections were added after exercising the NVIDIA GPU Operator path and
reviewing the recovery and certificate lifecycles. They are deliberately
fail-closed and are covered by CPU-only regression tests; no hardware action
is executed by the test suite.

- [x] TLS leaf renewal now stamps a digest of mounted TLS material on the
      controller Deployment and agent DaemonSet templates, so operator- and
      externally-issued Secret changes roll processes that cache certificates.
      CA replacement is explicitly blocked: it requires the documented manual
      expand/activate/retire rotation, never an unsafe in-place overwrite.
- [x] The cordon janitor keeps a node cordoned when its incident lookup fails;
      only a confirmed missing incident releases an orphaned cordon.
- [x] A node disappearance is established through an authoritative single-node
      lookup, not GPU-filtered inventory. A temporary device-plugin or capacity
      outage cannot resolve a live incident.
- [x] Accelerator-host quiesce snapshots the original persistence service and
      mode to an fsync-backed host file before mutation. Kubernetes records a
      host recovery marker even without recognized GPU Operator labels, and
      clears it only after agent-side restoration succeeds.
- [x] Per-process GPU-holder inspection is fail-closed: an unreadable live
      process or descriptor blocks a reset instead of being silently skipped.
- [x] Runtime configuration reload validates all candidate settings and applies
      the sole fallible node-pause-store update before replacing live settings;
      a store failure keeps the previous configuration intact.
- [x] Public publication now uses a separate curated checkout, never a private
      history mirror. Its release audit rejects credential-shaped material,
      kubeconfigs, private session/incident artefacts, logs, and unexpected
      symlinks; its buildable OSS source remains independently reviewed.

---

## Phase 1 — Ship an artifact and clean the tree (target: v0.1.0)

Estimated effort: 1–2 weeks. Nothing else matters until an artifact exists.

### 1.1 Release engineering
- [x] (M) Release workflow on tag: build all four binaries as multi-arch
      (amd64/arm64) distroless images, push to GHCR, generate SBOM, sign with
      cosign, publish checksums. Promote `deploy/compose/Dockerfile` to the
      production build; add the missing `docker:` rule to the `Makefile`
      (it is declared `.PHONY` but has no recipe).
- [x] (S) Attach a single-file CRD/operator install manifest to each release.
- [x] (S) Create `CHANGELOG.md`; adopt tagged versioning (`v0.1.0` first).
- [x] (S) Image references pinned by digest (2026-07-26, after the real
      v0.1.0 release): `config/default`, `config/samples`, and the Helm
      values all carry `v0.1.0@sha256:...` from the release's `images.txt`;
      helm/kustomize lockstep re-verified; the kind harness's tag-agnostic
      substitution now consumes an optional digest suffix.
- [x] (S) Add a CEL rule on `spec.controller.image` / `spec.agent.image`
      rejecting `:latest` and requiring a tag or digest (the `KubeNeuron` CRD
      has 11 CEL rules today; none covers images).

### 1.2 Repo hygiene
- [x] (S) Delete the 12 tracked `*.orig` / `*.rej` files (including
      `internal/operator/reconciler.go.rej` — verify the rejected hunks are
      either applied or obsolete before deleting), plus `run_llm`.
- [x] (S) Move internal session working notes out of the
      public tree (they contain infrastructure details).
- [x] (S) Remove empty scaffold dirs `deploy/kubernetes/operator/base/` and
      `deploy/kubernetes/instances/{dev,prod}/`.
- [x] (S) Fix `web/dist`: it is `.gitignore`d but force-tracked while
      `web/embed.go` requires it via `//go:embed`. Track it cleanly or build
      it in CI.
- [x] (S) Add `SECURITY.md` (private disclosure contact, supported versions),
      issue/PR templates, CODEOWNERS.

### 1.3 CI hardening
- [x] (S) Pin golangci-lint to an exact version and add `.golangci.yml`
      (today: `version: latest`, no config, local `make lint` silently skips
      when the binary is absent).
- [x] (S) Add `govulncheck` and a Dependabot (or Renovate) config.
- [x] (S) Add `mkdocs build --strict` to CI (`make docs` exists but never
      runs in CI; `docs/quickstart.md` already ships a literal `...`
      placeholder).
- [x] (S) Align Go versions across CI jobs (`1.25` vs `1.25.9`) and pin
      `controller-gen` as a tool dependency instead of `go run @version` at
      CI time.
- [x] (S) Kind integration job verified on GitHub-hosted runners
      (2026-07-25): first remote run surfaced an optimistic-concurrency
      conflict tripping the harness log gate — fixed by conflict-silent
      requeue in the operator — and the job has been green since.

### 1.4 Kill the legacy deploy paths
- [x] (M) Delete or hard-break `deploy/kubernetes/base/` + `overlays/` and
      `deploy/systemd/` (structurally incompatible with the mandatory mTLS
      agent listener; the systemd units as shipped cannot start —
      `cmd/kubeneuron-controller/main.go:282-284` rejects
      `--platform=baremetal` without TLS flags the unit never passes). Keep
      `deploy/compose/` only as an explicitly-labeled dev sandbox, or remove
      it too. `config/default` + `config/samples` +
      `deploy/kubernetes/dependencies/` become the only documented paths.

**Exit criteria:** fresh clone → green GitHub CI including kind job → signed,
pullable v0.1.0 images that match the install docs; no conflict artifacts or
dead install paths in the tree.

---

## Phase 2 — Correctness fixes in the safety and durability layer (v0.1.x)

Estimated effort: ~2 weeks. All items are known, bounded defects found in the
audit; land them before stacking new features on top.

### 2.1 Safety gate (highest priority defect)
- [x] (M) Fix `safety.Gate` slot accounting: `Allow` admits a second action
      on the same target without counting it, and `Done` unconditionally
      deletes the reservation (`internal/safety/limits.go:87,102-112`).
      `advanceVerifying` additionally releases a slot it never reserved
      (`internal/controller/reconcile.go:219`). Refcount or key `active` by
      (target, action); separate cooldown recording from slot release. Add
      multi-action-per-target and concurrent-incident tests — the current
      safety tests exercise each limit only in isolation.

### 2.2 Data races and loss
- [x] (S) `notify.Async` enqueues a live `*types.Incident` that the reconcile
      loop keeps mutating (`internal/notify/async.go:66-77` vs
      `reconcile.go:314,533-535`). Deep-copy on enqueue; add nil-incident
      guards (`slack.go:52`); add a dropped-notification metric (the 256-item
      queue currently drops with only a log line — approvals are among the
      droppable items).
- [x] (S) Fix flap-detector semantics: `RecordReopen` fires for every *new*
      incident, not actual reopens, double-counts on failed transitions, and
      never GCs its keys (`reconcile.go:88`, `internal/safety/flap.go`).
- [x] (S) Drain must retry PDB rejections: a `429 TooManyRequests` from the
      Eviction API currently fails the step and **escalates to a more
      destructive playbook rung** (`internal/platform/kubernetes/kubernetes.go:169-171`).
      Special-case `apierrors.IsTooManyRequests` with bounded retry.

### 2.3 Unbounded growth (guaranteed medium-term outages)
- [x] (M) Action-journal compaction: `StateReported` entries are kept forever;
      at 10k entries / 64 MiB the agent refuses all further actions with no
      operator recourse (`internal/agent/actionjournal/actionjournal.go:182,379`).
      Prune reported entries older than a retention window using the existing
      `rewriteRecordsLocked` primitive; add an exhaustion-behavior test.
- [x] (M) Store retention: nothing in `internal/store/sqlite` ever deletes.
      Add bounded retention for `events`, `event_outbox`, `actions`, and
      resolved `incidents` (audit stays append-only per design), plus periodic
      `wal_checkpoint`.
- [x] (S) Evict expired entries from the executor idempotency cache
      (`internal/agent/executor/executor.go:129`).

### 2.4 Durability hardening
- [x] (S) Set `_pragma=synchronous(FULL)` explicitly in the SQLite DSN
      (`internal/store/sqlite/sqlite.go:57-68`) and assert it in a test.
- [x] (S) `syncDir` after rename in `spool.rewriteLocked`
      (`internal/agent/spool/spool.go:201-204`) — the atomic replace is not
      crash-durable today. Also create the spool directory in `spool.Open`.
- [x] (S) Add a schema-version ceiling check so an older binary refuses a
      newer database instead of proceeding silently.
- [x] (M) Persist gate cooldowns and flap history (or explicitly document
      restart amnesia as accepted for DryRun and revisit in Phase 5).

### 2.5 Consistency cleanup
- [x] (S) ~~Drop or fix the `minAvailable: 1` PodDisruptionBudget~~ —
      inspected: `resources.go` documents this as a deliberate trade (evicting
      the controller mid-remediation strands in-flight incidents and the RWO
      SQLite claim pins it to one node); administrators relocate the
      installation before draining that node. Kept as documented.
- [x] (S) Wire the detection-catalog thresholds (XID 13/31/43 occurrence
      counting is populated but never read — `internal/detect/xid.go:23-26`)
      or delete them from the catalog so docs stop implying enforcement.
- [x] (S) Prune the `httpapi` doc comment advertising six unregistered routes
      (Slack webhook, SSE, summary, config, login —
      `internal/httpapi/httpapi.go:140-157`).
- [x] (S) Fix the unresolvable-XID fallback that reports `GPUIndex: 0`
      (`internal/agent/agent.go:434-447`) — emit an explicit unknown marker.
- [x] (S) Coverage raised on live code (2026-07-26): `internal/agent/kmsg`
      23%→92% (`Watch()` streaming, tail-seek, cancellation, missing-device),
      `internal/approval` 25%→100% (`Decide()` happy/unknown/wrong-state),
      `internal/playbook` 55%→77% (engine select/next-step/escalation),
      `internal/notify` 44%→75% (retry/backoff/dead-letter).

**Exit criteria:** the gate holds under concurrent incidents on one node; no
unbounded-growth path remains; `go test -race` covers the new scenarios.

---

## Phase 3 — Operability and access security (target: v0.2.0)

Estimated effort: 3–4 weeks. Bar: an SRE reaches a monitored, backed-up,
TLS-protected DryRun install from docs alone.

### 3.1 Self-monitoring
- [x] (M) Scrape KubeNeuron itself: VMPodScrape/ServiceMonitor for the
      controller and operator (today only dcgm-exporter is scraped —
      `deploy/kubernetes/dependencies/observability/vmagent.yaml`), and add a
      `/metrics` endpoint to the agent (it has none).
- [x] (M) Reconcile the forked alert rules: `configs/vmalert/gpu-rules.yaml`
      (15 alerts) vs `deploy/kubernetes/dependencies/observability/rules.yaml`
      (11) — one source of truth, and every alert `docs/operations.md`
      instructs operators to use must actually be able to fire
      (`kubeneuron_incidents{state="NEEDS_HUMAN"}`, `KubeNeuronAgentDown`).
- [x] (S) Certificate-expiry metric + alert rule. The 100-day agent-leaf
      ceiling is currently an unmonitored calendar obligation — the most
      likely future self-inflicted outage.
- [x] (S) Operator polish (2026-07-26): EventRecorder wired
      (ConfigurationInvalid/ReconcileFailed warnings, SnapshotPublished on
      each new digest), readyz verifies informer-cache sync with a bounded
      wait, production zap logging by default (`--zap-devel` opt-in).

### 3.2 Access security
- [x] (M) TLS on the public listener (operator API, webhook, panel, metrics
      all cross the network in cleartext today —
      `cmd/kubeneuron-controller/main.go:341`), or ship a documented
      Ingress/Gateway TLS pattern plus NetworkPolicies.
- [x] (M) CEL validation (or a validating webhook) for
      `GPURemediationPolicy`, `GPUPlaybook`, `GPUNodeConfig` — currently zero
      rules on the three CRDs that decide what runs where, while one
      malformed policy CR invalidates the whole installation's config
      snapshot by design (`internal/operator/reconciler.go:389-395`).
- [x] (S) Token hygiene (2026-07-26): `auth_failures_total{api}` metric +
      source-address logging, per-source failure throttle (20/min → 429,
      RemoteAddr-keyed), operator/webhook tokens re-read with a 10s cache
      so in-place Secret rotation needs no restart; procedure documented.

### 3.3 Backup/restore
- [x] (M) Fix or remove `deploy/kubernetes/backup/backup-cronjob.yaml`: it
      execs `sqlite3` and `tar` inside a distroless image that contains
      neither, is referenced by no kustomization, and stages the snapshot on
      the live data PVC. Prefer a sidecar/ephemeral-container approach or CSI
      VolumeSnapshot; pin its image.
- [x] (M) Test restore end-to-end and document RPO/RTO. Zero restore coverage
      exists today.

### 3.4 Test matrix expansion
- [x] (L) Multi-node kind harness (the current cluster is single-node — for a
      per-node remediation product nothing exercises cross-node identity,
      DaemonSet scheduling, cordon/drain, or per-node concurrency).
- [x] (M) Upgrade test (2026-07-26, `hack/kind-upgrade.sh`, first run
      PASSED locally against the real v0.1.0 release): installs the
      released baseline on kind (GHCR pull, or built from the git tag when
      the token lacks read:packages), seeds an incident through the v0.1.0
      operator API, upgrades in the documented order (CRDs → RBAC/operator
      → controller/agent images), and asserts: root Ready again, the
      upgraded CRD serves new schema (hostTooling), and the seeded
      incident + audit survived the in-cluster SQLite migration. Version
      skew note: HEAD operator args (e.g. --api-authn-kubernetes) assume
      HEAD binaries — the runbook's step 2 must follow step 1 immediately,
      which the script enforces and proves convergent.
- [x] (M) Controller-restart-mid-playbook test at the Kubernetes level
      (2026-07-26, kind harness phase): an approval-gated dry-run ladder is
      driven to AWAITING_APPROVAL, the controller Pod is deleted, and the
      harness asserts the incident and audit survived the restart with no
      re-executed step (cordon recorded exactly once), then the
      post-restart approval resumes the ladder to VERIFYING with the
      approver identity in the audit. Complements
      test/e2e/failover_test.go (store-level replay, both backends).

### 3.5 Docs and distribution
- [x] (M) Operator docs: KubeNeuron upgrade/rollback runbook, sizing guide,
      per-alert runbooks, generated CRD API reference, REST API and
      `kubeneuronctl` references, metrics reference, uninstall page.
- [x] (M) Helm chart (the product's own prerequisites are Helm-installed;
      kustomize-only is a distribution mismatch and leaves no
      upgrade/rollback story).

**Exit criteria:** ROADMAP Milestone 4 bar met end-to-end: docs-only install
produces a monitored, alerting, backed-up, TLS-protected DryRun deployment.

---

## Phase 3.5 — Observability completion (dashboard, alert policy, runbooks)

No hardware needed; ship with the next minor.

- [x] (M) **Dashboard v2**: extend `deploy/grafana/kubeneuron-dashboard.json`
      with TLS-expiry countdown, notification drops, agent spool depth,
      controller/agent availability (`up{job=...}`), and event
      posted-vs-spooled rates, so every shipped alert has a matching panel.
- [x] (M) **Alert routing policy**: shipped Alertmanager example with
      severity-based routing (critical pages, warning notifies), inhibition
      (controller-down mutes downstream KubeNeuron alerts; exporter-down
      mutes per-GPU alerts on that node), and `runbook_url` annotations on
      every `kubeneuron-self` rule.
- [x] (M) **Per-alert runbooks**: `docs/runbooks.md` — one entry per shipped
      alert: meaning, first checks, remediation, escalation.
- [x] (S) **Latency/backlog metrics**: reconcile-pass duration histogram and
      pending-action queue depth gauge, with panels.

**Exit criteria:** every shipped alert has a runbook link and a dashboard
panel; an on-call engineer can triage from the alert alone.

---

## Phase 4 — Real NVIDIA runtime (target: v0.3.0)

Estimated effort: 6–10 weeks; requires GPU lab access for the final
validation, but most of the code lands and is fake-driver-tested before the
lab: items marked **[no-hw]** are executable immediately; items marked
**[hw]** need the lab. Parallelizable with Phase 5. This is PRODUCT_PLAN
Phases 1–3 made concrete.

- [x] (L) [no-hw built 2026-07-26, hw to verify] **NVIDIA agent image**:
      `spec.agent.hostTooling` mounts the node's `nvidia-smi`/`dcgmi`/driver
      libraries read-only into the distroless agent (PATH + LD_LIBRARY_PATH,
      defaults match the AL2023 EKS NVIDIA AMI), optional scriptsDir feeds
      `--scripts-dir`, and declaring host tooling arms
      `--require-real-driver`. CEL-guarded paths; admission matrix 60
      checks. **[hw] VERIFIED 2026-07-26** on EKS g4dn/T4 (2nd AWS run):
      the live run exposed a real defect — the scratch image lacks the ELF
      interpreter, and exec of a dynamic nvidia-smi resolves PT_INTERP
      before LD_LIBRARY_PATH — fixed by additionally mounting libDirs[0]
      at /lib64. After the fix the distroless agent ran the host
      nvidia-smi, registered real T4 inventory (UUID/model/boot_id), and a
      kernel-injected XID 79 walked the full dry-run ladder with the
      verified actor identity in the audit.
- [x] (S) [no-hw] Make the `Fake` driver fallback fail loudly in managed mode:
      `-require-real-driver` refuses startup without nvidia-smi. The operator
      will set it together with the NVIDIA agent image (setting it today
      would crash-loop every CPU-only install).
- [x] (M) [no-hw] **Wire destructive-action enablement end-to-end**:
      `-enable-destructive-actions` → `agent.Config` → `executor.NewWithOptions`,
      refused with a Fake driver at both the CLI and the constructor, loud
      startup warning. The operator never sets it until the Phase 6 gate —
      inert by default, but no longer unreachable.
- [x] (L) [no-hw] Server-side action protocol (2026-07-26): every claim —
      including lease-expiry reclaims — increments a persisted `attempts`
      counter; the agent binds claims and results to its node boot via
      `X-KubeNeuron-Executor-Boot-Id` (post-reboot results are rejected with
      `ErrExecutorBootMismatch`); `escalate()`/`quarantine()` tombstone the
      incident's still-pending queue entries so a superseded ladder rung can
      never be delivered late (leased work deliberately finishes or expires).
      Queue replay already attached to the same action ID (idempotent
      enqueue); conformance tests cover both SQLite and PostgreSQL. Lease
      *renewal* deferred: leases already cover the declared action timeout,
      so renewal adds nothing until an action's runtime is open-ended.
- [x] (M) [no-hw] Real verification before `RESOLVED`: DCGM health/diag, expected
      inventory, driver probe — not merely heartbeat + quiet window; uncordon
      only after verification succeeds.
- [x] (M) [hw] Parser-fidelity FIRST PASS done on AWS (EKS + g4dn/Tesla T4,
      driver 580.159.03, 2026-07-25): DetectSMI, inventory (real UUID/model),
      driver-version attestation, and the liveness health probe all parsed
      real output correctly; raw fixtures captured in
      `internal/agent/nvml/testdata/t4-g4dn-580.159.03.txt`; probe tool at
      `test/hwprobe`. **Finding:** MIG-incapable GPUs (T4) report `[N/A]`
      for `mig.mode.current`; the parser fail-closes to `unknown`, so reset
      stays ineligible on non-MIG hardware — decide whether `[N/A]` should
      map to an explicit "MIG-unsupported ⇒ unpartitioned" semantic (needs
      an NVIDIA-doc-backed decision, not a hasty mapping). dcgmi absent on
      the stock EKS NVIDIA AMI — readiness stayed observed-only, as
      designed. Remaining for full closure: MIG-capable GPU (A100/H100)
      fixtures and `dcgmi` outputs.
- [x] (L) [hw] Hardware CI target (first manual run PASSED 2026-07-25 on
      ephemeral EKS: multi-node mTLS registration, kernel-injected XID 79 →
      cordon→drain→approval→reboot→uncordon dry-run ladder with full audit;
      infra findings fixed: fsGroup for CSI volumes, EBS CSI addon
      prerequisite): self-hosted NVIDIA runner, destructive-lab
      environment gate, lab-node allowlist, out-of-band watchdog (per
      PRODUCT_PLAN safety rule 1). Injected and real XID paths, reset/reboot,
      drain/PDB, crash-recovery at journal boundaries.
      **BACKLOG decision (2026-07-26):** GPU E2E is deliberately NOT
      per-commit CI. Agreed tiering when this is picked up: (a) every
      push/PR stays CPU-only (current CI); (b) GPU E2E as
      `workflow_dispatch` + weekly cron on ephemeral EKS (the proven
      g4dn recipe, full teardown) and mandatory before each release tag;
      (c) a permanent self-hosted GPU runner only if a physical lab
      machine appears — nightly destructive ladder there. Not needed now.
      **SCAFFOLDED 2026-08-01:** the tiering above is now encoded —
      `.github/workflows/hw-e2e.yaml` (workflow_dispatch with a typed
      confirmation + `gpu-lab` environment approval, plus a weekly cron),
      `.github/workflows/hw-e2e-reaper.yaml` (out-of-band max-lifetime
      watchdog), and `hack/hw-e2e.sh` (up → deploy → dry-run + destructive
      ReplaceNode assertions → always-teardown with a leak sweep). Per-commit
      CI is untouched. actionlint/bash -n clean.
      **FIRST GREEN RUN 2026-08-05** on ephemeral EKS `kubeneuron-e2e10`
      (us-east-1, g4dn.xlarge/T4): the XID-79 dry-run ladder, a real
      confined destructive `ReplaceNode` instance termination, and the
      XID-92 threshold phase all passed, and teardown swept to zero
      leftovers. The run was driven locally via `hack/hw-e2e.sh`; the
      GitHub Actions `workflow_dispatch` path itself has not yet been
      exercised.
- [ ] (M) [hw] NVML/DCGM event stream as a second detection source beside kmsg.
      **CODE LANDED 2026-08-01:** `internal/agent/gpuhealth/` polls DCGM's
      last-XID (`dcgmi dmon -e 230`, level-triggered) with an `nvidia-smi -q`
      ECC/row-remap counter fallback (baselined, so history never replays),
      normalized into the same `types.AgentEvent`; a shared `handleDetection`
      path deduplicates a fault seen by both sources within a 2-minute window;
      the source runs only on the real driver. The kmsg tail-seek loss is
      separately fixed by a durable, crash-safe sequence cursor
      (`internal/agent/kmsg/cursor.go`) that fails safe to tail-seek. All
      unit-tested against synthetic `dcgmi`/`nvidia-smi` fixtures.
      **Still `[hw]`:** the real `dcgmi dmon` column layout, the
      driver-dependent `nvidia-smi -q` section labels, and `/dev/kmsg` ring
      semantics need validation on a live node before this is closed.

**Exit criteria:** documented, reproducible remediation of an induced failure
on real hardware in dry-run *and* lab-enabled mode, with full audit.

---

## Phase 5 — Production control plane (parallel with Phase 4)

Estimated effort: 4–6 weeks.

- [x] (L) PostgreSQL workflow store + migrations + backup/restore/PITR;
      leader election and controller failover (SQLite remains the
      DryRun/development option). Promote the transactional-outbox semantics
      to the new backend with restart/failover replay tests.
      **Store backend DONE** (internal/store/postgres over the shared
      sqlcore engine; conformance suite green against PostgreSQL 16 locally
      and as a CI service container). **Wiring + leader election DONE**:
      operator accepts `workflowStore: Postgres` (DSN Secret, no PVC, CEL
      53), controller runs an elected active/standby pair with
      readiness-follows-leadership, deposed leaders exit, retention is
      leader-gated. **Action-protocol hardening DONE** (2026-07-26, see
      Phase 4): attempts counter, executor-boot binding, pending-only
      cancellation — replay/failover can no longer double-execute or accept
      a post-reboot result. PITR/backup guidance for Postgres documented in
      docs/operations.md. **Failover-replay test DONE** (2026-07-26,
      test/e2e/failover_test.go): leader A dies while its action is leased
      to the agent, leader B replays over the shared store (SQLite always,
      PostgreSQL under the conformance DSN — CI runs both), and the replay
      attaches to the same action ID — attempts stays 1, one stored result,
      empty queue after. Lease-election mechanics are client-go's own,
      exercised live in the kind harness and the EKS run.
- [x] (M) Verifiable actor identity (2026-07-26): the operator API accepts
      any Kubernetes bearer token — TokenReview resolves the principal,
      SubjectAccessReview authorizes it against RBAC on the installation's
      `kubeneurons.kubeneuron.io` object (get = reads, update = mutations) —
      and audit rows record the verified username. The shared static token
      remains break-glass only; its self-asserted actor is persisted as
      `token:<name>`, permanently distinguishable from a verified identity.
      External OIDC federates through the Kubernetes API server's own OIDC
      support, so no separate issuer integration is needed.
- [x] (M) Notification reliability (2026-07-26): per-channel async queues
      with 4-attempt exponential backoff and dead-letter to the log +
      `notifications_dropped_total{reason="dead_letter"}`; generic JSON
      webhook channel (`spec.notifications.webhook`, url/token Secret) and
      PagerDuty Events v2 (`spec.notifications.pagerduty`, dedup by
      incident ID, critical paging on needs-human/approvals, auto-resolve).
      Opsgenie reachable through the generic webhook; Slack interactive
      approvals (signing-secret receiver) remain optional future work.
- [x] (M) cert-manager convenience path (2026-07-26,
      `deploy/cert-manager/`): two dedicated in-cluster CAs, auto-renewing
      90-day leaves (renewBefore 35d, ahead of the 30-day expiry alert),
      CA refs point at the leaf Secrets' `ca.crt` so no CA key reaches a
      workload; leaf renewal requires a rollout (documented); CA rotation
      deliberately stays the manual expand/contract procedure
      (`hack/tls-rotate.sh`, emergency path in
      `hack/tls-emergency-recover.sh`).

**Exit criteria:** a controller failover leaves no duplicate action; restore
is rehearsed; every human mutation has a verifiable identity.

---

## Phase 6 — Guarded enablement, pilot, GA

The Enabled admission gate and verification matrix in
[PRODUCT_PLAN.md](PRODUCT_PLAN.md) apply verbatim and are not restated here.
Sequence: multi-node hardware qualification green → chaos/failover/restore
rehearsed → pilot on a real fleet → hardware E2E green for two consecutive
minor releases. (The blanket `Enabled` rejection was replaced in v0.2.0 by
`spec.safety.destructiveExecution` confinement — selector plus
acknowledgement; the remaining sequence items still gate any broadening of
autonomy defaults.)

---

## Defect register (audit 2026-07-25)

Tracked so fixes can be checked off individually; severity: A = safety/data
loss, B = operational outage, C = correctness/hygiene.

| # | Sev | Defect | Location |
|---|-----|--------|----------|
| 1 | A | ~~Gate over-releases concurrency slots; `advanceVerifying` releases unowned slot~~ FIXED (Phase 2.1) | `internal/safety/limits.go:87,102-112`; `internal/controller/reconcile.go:219` |
| 2 | A | ~~`notify.Async` shares mutable `*Incident` across goroutines (race)~~ FIXED (Phase 2.2) | `internal/notify/async.go:66-77` |
| 3 | A | ~~Drain escalates on PDB 429 instead of retrying~~ FIXED (Phase 2.2) | `internal/platform/kubernetes/kubernetes.go:169-171` |
| 4 | A | ~~`Fake` NVML driver reports success for `ResetGPU`; silent fallback~~ FIXED (Phase 4: `-require-real-driver`; destructive mode refused with a Fake driver) | `internal/agent/nvml/nvml.go:70,73`; `cmd/kubeneuron-agent/main.go:98-104` |
| 5 | B | ~~Action journal wedges permanently at 10k/64MiB, no compaction~~ FIXED (Phase 2.3) | `internal/agent/actionjournal/actionjournal.go:182,379` |
| 6 | B | ~~SQLite store has zero retention; unbounded growth~~ FIXED (Phase 2.3) | `internal/store/sqlite/` |
| 7 | B | ~~`synchronous` pragma unset; spool rewrite not crash-durable~~ FIXED (Phase 2.4) | `internal/store/sqlite/sqlite.go:57-68`; `internal/agent/spool/spool.go:201` |
| 8 | B | ~~PDB `minAvailable:1` on 1-replica Recreate controller blocks drains~~ CLOSED as a documented deliberate trade (Phase 2.5) | `internal/operator/resources.go:514-523` |
| 9 | B | ~~Gate cooldowns / flap history in-memory only; reset on restart~~ FIXED (Phase 2.4) | `internal/safety/` |
| 10 | B | ~~Notify queue drops approvals silently, no metric~~ FIXED (Phase 2.2) | `internal/notify/async.go:72,82` |
| 11 | B | ~~Backup CronJob cannot run (no sqlite3/tar in distroless image)~~ FIXED (Phase 3.3) | `deploy/kubernetes/backup/backup-cronjob.yaml` |
| 12 | B | ~~No cert-expiry metric/alert; 100-day leaf is unmonitored~~ FIXED (Phase 3.1) | — |
| 13 | C | ~~Flap detector counts new incidents as reopens; keys never GC'd~~ FIXED (Phase 2.2) | `internal/controller/reconcile.go:88` |
| 14 | C | ~~Detection thresholds (XID 13/31/43) populated but never enforced~~ FIXED (Phase 2.5) | `internal/detect/xid.go:23-26` |
| 15 | C | ~~`httpapi` docs advertise six unregistered routes~~ FIXED (Phase 2.5) | `internal/httpapi/httpapi.go:140-157` |
| 16 | C | ~~Unresolvable XID reports `GPUIndex: 0` in evidence~~ FIXED (Phase 2.5) | `internal/agent/agent.go:434-447` |
| 17 | C | systemd deploy path cannot start as shipped | `deploy/systemd/*.service`; `cmd/kubeneuron-controller/main.go:282-284` |
| 18 | C | 12 tracked `.orig`/`.rej` files incl. core reconciler `.rej` | `internal/operator/reconciler.go.rej` et al. |
| 19 | C | `make docker` declared but has no rule; docs reference unbuilt images | `Makefile`; `config/default` |
| 20 | C | ~~kmsg watcher fd double-close pattern; goroutine leak~~ FIXED (sync.Once close + done channel); tail-seek XID loss remains until the NVML/DCGM event stream lands | `internal/agent/kmsg/watcher.go` |

## Effort summary

| Phase | Scope | Estimate |
|-------|-------|----------|
| 1 | Artifact + hygiene | 1–2 weeks |
| 2 | Safety/durability fixes | ~2 weeks |
| 3 | Operability + security | 3–4 weeks |
| 4 | Real NVIDIA runtime | 6–10 weeks (GPU lab required) |
| 5 | Production control plane | 4–6 weeks (parallel with 4) |
| 6 | Qualification, pilot, GA | fleet- and lab-dependent |

Total to NVIDIA v1.0: roughly 25–35 engineering weeks with reliable GPU lab
access — consistent with the PRODUCT_PLAN estimate. A public, credible v0.1.0
(Phases 1–2) is achievable in 3–4 weeks.
