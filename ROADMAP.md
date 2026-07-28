# KubeNeuron Roadmap — historical implementation checklist

> **Superseded (2026-07-25).** Execution moved to
> [PRODUCTION_READINESS_PLAN.md](PRODUCTION_READINESS_PLAN.md), which
> carries the authoritative per-item status; the checkboxes below were
> never maintained afterwards and do not reflect what exists. Nearly all
> of Milestones 0–4 shipped in v0.1.0 (see CHANGELOG.md); "real NVML via
> go-nvml" was deliberately resolved as the exec-based `nvidia-smi`
> driver instead.

The canonical current product scope, accelerator architecture, safety gates,
and delivery order are in [PRODUCT_PLAN.md](PRODUCT_PLAN.md). This file keeps
the detailed implementation checklist and completed-work history that led to
the current checkpoint.

Status: planning document, updated 2026-07-20. Derived from the current
authenticated-agent checkpoint and a full-code
audit. Ordering matters: each milestone assumes the previous one is done and
verified. Nothing below claims to exist until its checkbox is checked.

Sizing: **S** ≈ up to a day, **M** ≈ days, **L** ≈ a week+, for one developer
familiar with the codebase.

## Milestone 0 — Open-source hygiene and repo cleanup

Goal: a repository a stranger can clone, understand, build, and trust.

- [ ] (S) Remove dead scaffolding: empty `deploy/kubernetes/operator/base/`
      and `deploy/kubernetes/instances/{dev,prod}/` directories.
- [ ] (S) Remove or quarantine session artifacts from the public tree:
      `*.orig`, `*.rej`, and internal working notes (moved to a private
      location; they contained infra details).
- [ ] (S) Fix or remove the broken `make web` target (no `package.json`
      exists yet); Makefile must not advertise targets that cannot run.
- [ ] (S) Pin golangci-lint version in CI (currently `latest`) and align the
      unit-test job Go version (`1.25`) with the kind job (`1.25.9`).
- [ ] (S) Add `SECURITY.md` (private disclosure contact, supported versions),
      issue/PR templates, and CODEOWNERS.
- [ ] (M) Release engineering: tagged versions, `CHANGELOG.md`, goreleaser or
      equivalent, multi-arch images published to GHCR with SBOM/signing
      (cosign), CRD install manifest attached to releases.
- [ ] (S) Verify the checked-in GitHub Actions kind job actually passes on
      GitHub runners (it has never run remotely).
- [ ] (S) Track generated files (`config/crd/bases`, deepcopy) so
      `make verify-generate` is green in CI.

Exit criteria: fresh clone → `make build && make test` green in GitHub CI;
release v0.0.x images pullable from GHCR.

## Milestone 1 — Correctness fixes in existing code (before new features)

Goal: eliminate the latent bugs found in audit so new code lands on solid
ground.

- [x] (M) **Store transactions.** Add a transaction primitive to
      `internal/store.Store`; persist incident + audit atomically
      (`internal/controller/controller.go` `openIncident`), as
      `statemachine.go`'s contract already requires.
- [x] (S) **Schema versioning.** Add a `schema_version` table and run
      migrations once per version instead of unconditionally
      (`internal/store/sqlite/sqlite.go`).
- [x] (S) **Approval expiry basis.** Expire approvals from the
      `AWAITING_APPROVAL` transition timestamp, not `UpdatedAt` (which every
      duplicate signal bumps) (`internal/approval/approval.go`).
- [x] (S) **Signal-queue overflow.** Make the ingest channel drop policy
      explicit: block with backpressure or count+alert on drops; never lose a
      critical-class signal silently (`controller.go:87-93`).
- [x] (S) **kmsg watcher recovery.** Reopen `/dev/kmsg` with backoff on
      watcher death instead of nil-ing the channel
      (`internal/agent/agent.go:225-229`).
- [x] (M) **Spool durability/throughput.** fsync on append (or batched
      fsync), stop re-reading the whole file per append, raise the replay
      rate (2 events/30s cannot drain a 10k backlog), receiver-side dedup by
      event ID so `202` can be meaningful.
- [x] (S) **XID 92 class split.** Give high-SBE-rate its own class so policy
      can distinguish it from critical DBE (`internal/detect/xid.go`).
- [x] (S) **Alert-path node identity.** Normalize the Alertmanager `instance`
      fallback to node names (strip port / prefer a `node` label) so both
      ingestion paths converge on one incident target.
- [x] (S) Prune the cooldown map in `internal/safety/limits.go`; align
      runtime dry-run toggling (Gate.SetDryRun) with actuator wrapping or
      remove the toggle.
- [x] (S) Reconsider PDB `minAvailable: 1` on the 1-replica Recreate
      controller (blocks node drains); document or drop.
- [x] (M) Tests for currently untested packages that already have real code:
      `internal/actuator` (DryRun/Chain), `internal/platform/kubernetes`
      (fake clientset), `internal/config`, controller ingest/dedup/overflow.

Exit criteria: all above covered by unit tests; `go test -race ./...` green.

## Milestone 2 — Phase 1 runtime: end-to-end dry-run remediation

Goal: a signal walks the whole ladder — with every side effect a logged
no-op. This is the heart of the project.

- [x] (L) **Reconcile state walk.** Implement the incident advancement loop
      (`controller.go` TODO(phase-1)): OPEN→EVALUATING (policy/playbook via
      the existing `Engine`), OBSERVING thresholds (XID 13/31/43 counters),
      EXECUTING through the safety `Gate`, AWAITING_APPROVAL parking/expiry,
      VERIFYING with quiet windows, RESOLVED/NEEDS_HUMAN, flap-detector
      wiring, escalation on failure. All transitions through
      `statemachine.Transition` inside store transactions with audit rows.
- [x] (L) **Authenticated agent action RPC.** Implemented as a durable
      store-backed work queue polled by the agent over its existing
      mTLS+token channel (no per-node listener or serving certificate);
      executor idempotency cache and `agentrpc` `Execute`/`Healthy` wired.
      The gRPC push contract in `api/proto` remains a future low-latency
      option.
- [x] (M) **Real GPU driver.** Implemented via `nvidia-smi` subprocess
      instead of cgo `go-nvml`, preserving CGO-free static builds: real
      inventory, kmsg↔smi PCI normalization for correct multi-GPU
      attribution, guarded `gpu-reset`, and a deadline-bounded driver-hang
      probe. An NVML event stream (`WatchEvents`) as a second detection
      source next to kmsg remains open (would need cgo or dcgm).
- [x] (M) **Webhook auth.** Authenticate the Alertmanager receiver (bearer
      token or mTLS) — today anyone reaching :8080 can open incidents.
      Note on topology: the supported alert pipeline is the one
      VictoriaMetrics itself recommends — vmalert evaluates the MetricsQL
      rules (`configs/vmalert/gpu-rules.yaml` is vmalert-specific already)
      and sends firing alerts to Alertmanager, which handles grouping,
      dedup, silences, and human routing before hitting this webhook.
      Alertmanager is not replaced by vmalert; they are different stages.
      A direct vmalert→controller notifier mode (no Alertmanager) may be
      evaluated later as an optional minimal-install profile.
- [x] (M) **REST read APIs**: incidents list/get, nodes, audit; wire
      `kubeneuronctl status|nodes|incidents` to them.
- [x] (M) **Decision APIs**: approve/reject/resolve + pause/resume with
      audited actor; `kubeneuronctl` fully wired. Remaining: the operator
      still rejects `executionMode: Paused` (runtime pause exists via API).
- [x] (S) **Slack notifier** via incoming webhook behind `notify.Async`,
      so delivery never stalls ingest or the walk. Interactive
      Approve/Reject buttons (Slack app + signing secret) remain open.
- [x] (M) **E2E dry-run suite**: `test/e2e` drives agent events and the
      Alertmanager webhook over real HTTP through the full walk: approval
      park/approve, replay dedup, pause/resume, audit assertions. A
      kind-integrated variant remains future work.

Exit criteria: `docker compose` / kind demo shows a synthetic XID 79
traveling signal→incident→policy→approval→dry-run drain→verify→resolve with
a complete audit log. Release **v0.1.0** and publish a demo walkthrough.

## Milestone 3 — Real actions, guarded enablement

Goal: after the remaining safety and hardware gates, `executionMode: Enabled`
can stop being rejected and a real GPU node can be remediated. The current
implementation rejects `Enabled` fail-closed.

- [ ] (M) **Guarded enablement:** accept `Enabled` only after explicit
      preconditions, crash-safe action completion across restarts, and
      hardware-gated validation. Today CEL admission and the compiler reject
      it fail-closed; action vocabulary and DryRun contracts alone are not
      sufficient to authorize GPU remediation.
- [x] (M) Agent executor actions: `gpu_reset` via nvidia-smi with
      idle-check, `run_diag` (dcgmi levels 1-3), `collect_bundle`
      (nvidia-bug-report.sh), boot-ID-guarded `reboot`, and
      `driver_reload`/`driver_reinstall`/`run_script` via allow-listed
      operator-provisioned scripts.
- [ ] (M) Verification steps: DCGM health + driver probe + quiet window
      before RESOLVED; uncordon only after verification.
- [ ] (M) Certificate lifecycle: document + script issuance/rotation
      (the manual 4-Secret bootstrap is a real adoption barrier); optional
      cert-manager integration as a non-required convenience path.
- [x] (M) All three reserved CRDs consumed: `GPUMaintenanceWindow`
      (windows.yaml, walk holds matching nodes), `GPUSignalMapping`
      (detection-catalog overrides for XIDs and alerts), `GPUNodeConfig`
      (complete per-node pause set). Unsupported sub-fields stay
      fail-closed.
- [x] (M) **Child-CR status:** selected `GPUPlaybook` and
      `GPURemediationPolicy` publish generation-bound `Ready` conditions and
      effective per-child digests; policies also publish `resolvedPlaybook`.
      A whole-installation compilation failure clears stale success status.
- [ ] (L) **Hardware-gated E2E**: a real NVIDIA node in CI (self-hosted
      runner) exercising fake-injected and real XID paths, multi-node agent
      identity (node A cannot act as node B), rolling upgrade test.
- [ ] (M) SSH fallback actuator with host-key verification and fixed
      command allow-list (design already mandates this).

Exit criteria: documented, reproducible remediation of an induced failure on
real hardware with approvals and full audit. Release **v0.2.0**.

## Milestone 4 — Operability, UI, docs (the "ready to use" bar)

- [x] (L) Web control panel **v1**: zero-dependency embedded page served
      by the controller at `/` — pause controls, incidents with audited
      approve/reject/resolve, audit drill-down, node inventory. React/TS
      with viewer/operator/admin roles, OIDC, and SSE remains the v2
      target.
- [x] (M) Controller `/metrics` (incident states, signals/drops, steps,
      gate denials, escalations) + Grafana overview dashboard in
      `deploy/grafana/`.
- [x] (M) `/api/v1/targets` vmagent http_sd behind the operator token.
      Remaining: `Managed` observability mode with readiness checks.
- [x] (M) Backup/restore + retention documented in `docs/operations.md`;
      scheduled daily backups shipped as a CronJob example in
      `deploy/kubernetes/backup/` (exec-based `.backup` snapshot with dated
      retention).
- [x] (L) Docs site: mkdocs-material config + full nav; production
      install guide with the TLS/SPIFFE bootstrap, playbook authoring
      guide, troubleshooting, operations, quickstart. Remaining: CI
      publishing of the rendered site (Milestone 0).
- [ ] (S) Public demo assets: asciinema/GIF of the dry-run walk, sample
      Grafana screenshots.

Exit criteria: an SRE who has never seen the project reaches a working
dry-run install from docs alone; **v0.5.0 / public announcement**.

## Milestone 5 — Scale and breadth (post-1.0 candidates)

- [ ] PostgreSQL workflow store + active/standby controller (API already
      reserves it); only after demonstrated need.
- [ ] ClickHouse raw-event archive (optional, never the workflow authority).
- [ ] Slurm platform adapter behind `internal/platform`.
- [ ] Ticketing/RMA integrations (Jira/ServiceNow) from the `rma` playbook.
- [ ] Predictive draining from SBE/remap trends.
- [ ] Windows for coordinated rolling upgrades of agent/controller protocol
      versions (capability negotiation beyond the current fail-closed guard).

## Release criteria for v1.0.0

1. Every safety mechanism in design §4.3 implemented, tested, and exercised
   by E2E (dry-run, typed actions, concurrency/cooldown, flap, approvals,
   pause, idempotency, audit, verification).
2. Hardware E2E green in CI for two consecutive minor releases.
3. Zero known data-loss paths (spool, store, migrations) under crash tests.
4. Upgrade path tested N-1 → N for CRDs, controller, and agent.
5. Security review of both trust boundaries (agent↔controller, human↔API);
   SECURITY.md process exercised at least once.
6. Docs complete; at least one external production adopter reference.
