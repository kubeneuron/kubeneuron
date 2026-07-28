# Changelog

All notable changes to this project are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html)
with a pre-1.0 caveat: minor versions may contain breaking changes while the
API is `v1alpha1`.

## [Unreleased]

Nothing yet.

## [v0.1.0] - 2026-07-26

First tagged release: the complete DryRun control plane with a shippable,
signed artifact set. Everything below the Phase 6 gate from
`PRODUCTION_READINESS_PLAN.md` that does not require GPU hardware is in.
`executionMode: Enabled` remains rejected by construction.

### Added (since the plan audit)
- PostgreSQL workflow store (shared `sqlcore` engine, conformance-tested
  against PostgreSQL 16) with Lease-based leader election: two controller
  replicas, readiness follows leadership, retention leader-gated; SQLite
  remains the single-replica/dev option. Failover replay is proven to
  attach to the same queued action — no duplicate side effect.
- Server-side action protocol: per-claim attempt counter, executor
  boot-ID binding (a result posted after an unnoticed reboot is rejected),
  and pending-only cancellation when an incident escalates or quarantines.
- Opt-in NVIDIA host tooling for the agent DaemonSet
  (`spec.agent.hostTooling`): read-only mounts of `nvidia-smi`/driver
  libraries/remediation scripts into the distroless image, arming
  `--require-real-driver`.
- Verifiable operator identity: the operator API accepts any Kubernetes
  bearer token (TokenReview + SubjectAccessReview against RBAC on the
  root `kubeneurons` object); the shared static token is break-glass and
  its self-asserted actor is recorded as `token:<name>`.
- Notification reliability: per-channel retry with backoff and
  dead-lettering, a generic JSON webhook channel, and PagerDuty Events v2
  (dedup by incident, critical paging on needs-human/approvals,
  auto-resolve).
- Token hygiene: `kubeneuron_auth_failures_total`, per-source throttling
  of failed attempts, and hot token rotation (files re-read, no restart).
- cert-manager convenience path (`deploy/cert-manager/`) for the
  four-Secret TLS model with auto-renewing 90-day leaves.
- Kind harness: controller-restart-mid-playbook phase (durable approval
  state, no re-executed step) and a 60-case CEL admission matrix.
- Operator emits Kubernetes Events for reconcile outcomes; readiness
  reflects informer-cache sync.

### Added
- Production readiness plan (`PRODUCTION_READINESS_PLAN.md`) with a phased
  path to v1.0 and a defect register.
- `SECURITY.md`, issue/PR templates, `CODEOWNERS`, Dependabot configuration.
- Release workflow: multi-arch distroless images for all four binaries
  published to GHCR with SBOM, cosign keyless signatures, and checksums;
  single-file CRD/operator install manifest attached to releases.
- `make docker` target building the four production images locally.

### Added (Phase 3, in progress)
- Gate cooldowns and flap history persist across controller restarts
  (`safety_state` snapshots restored on startup).
- The agent serves `/metrics`; vmagent scrapes the controller, agent, and
  operator; a `kubeneuron-self` alert group covers controller down,
  NEEDS_HUMAN, dropped signals/notifications, spool backlog, and TLS
  certificates expiring within 30 days
  (`kubeneuron_tls_certificate_not_after_seconds`).
- Admission-time CEL for the remaining config CRDs: unsupported policy match
  fields, Reboot-without-approval playbooks, malformed durations, and
  SSH/BMC references are rejected before the compiler sees them.
- Optional `spec.tls.publicServerSecretRef`: the controller's public
  listener (operator API, webhook, panel, metrics) serves TLS 1.3 when set,
  so bearer tokens stop crossing the network in cleartext; probes and the
  managed Deployment switch automatically (CEL matrix: 51 cases).

### Changed
- CI: pinned golangci-lint with a repo `.golangci.yml`, added `govulncheck`
  and strict docs builds, aligned Go toolchain versions across jobs.
- CRD validation rejects floating (`:latest` or untagged) controller/agent
  image references.
- Alert rules unified: the deployed VMRule mirrors the canonical vmalert
  file (a unit test pins them together); rules referencing never-exported
  agent series were removed.
- The kind integration harness runs a 3-node cluster by default
  (`WORKER_NODES`) and passes the full matrix on it; fixes surfaced by the
  run include a CEL cost-budget bound on `acceleratorruntimeprofiles`
  (previously uninstallable on Kubernetes 1.33), degraded-vs-stale
  readiness semantics, and all-pod agent log aggregation.
- Workflow-store backups moved to an authenticated `GET /api/v1/backup`
  snapshot endpoint with a rewritten curl-based CronJob; restore is proven
  in the e2e suite.
- Observability completion: dashboard panels for every shipped alert
  (availability, notification drops, spool backlog, TLS expiry, reconcile
  latency), Alertmanager routing/inhibition policy with an authenticated
  webhook (the shipped config previously omitted the mandatory bearer
  token), `runbook_url` on every rule, and per-alert runbooks
  (`docs/runbooks.md`).
- Real incidents (non-dry-run) now require runtime evidence — fresh agent
  heartbeat plus a ready accelerator report listing the target GPU — before
  resolving; missing evidence fails closed to NEEDS_HUMAN.
- A Helm chart (`deploy/helm/kubeneuron`) installs the CRDs and operator;
  CI pins it to the kustomize manifests. Upgrade runbook and REST
  API/CLI/metrics reference pages added to the docs site.

### Removed
- Tracked merge-conflict artifacts (`*.orig`, `*.rej`) and agent-session
  scratch documents.
- Legacy, non-functional deploy paths (`deploy/kubernetes/base` + overlays,
  `deploy/systemd`, `deploy/compose`) that predate the mandatory mTLS agent
  transport; the production image build moved to `build/Dockerfile`.

### Fixed
- `web/dist` (embedded control panel) is now tracked; a fresh clone builds
  the controller again.
- Safety gate concurrency slots are refcounted per target: one incident
  finishing no longer releases a slot a concurrent incident on the same node
  still holds, and recording a playbook cooldown no longer frees live slots.
- Flap detection counts resolve→reopen cycles instead of every new incident,
  no longer double-counts retried transitions, and prunes stale pairs.
- Incidents crossing the async notification queue are deep-copied (data
  race), nil incidents no longer panic the Slack notifier, and dropped
  notifications are counted in `kubeneuron_notifications_dropped_total`.
- Node drain retries PDB-blocked evictions (HTTP 429) until the drain
  timeout instead of escalating to a more destructive playbook rung.
- The agent action journal compacts acknowledged entries older than 24h
  instead of permanently refusing work at its 10k/64MiB limits.
- The SQLite store enforces retention (`-store-retention`, default 90 days,
  for events/outbox/actions; opt-in `-store-audit-retention` for terminal
  incidents with audit history), checkpoints the WAL, opens with an asserted
  `synchronous(FULL)`, and refuses databases from newer binaries.
- The spool syncs its directory after atomic replacement and creates its
  directory on open; the executor idempotency cache evicts expired entries.
- Detection-catalog occurrence thresholds (XID 13/31/43) are now enforced as
  the default observation policy instead of being documentation-only.
- An XID that cannot be attributed to a GPU reports index -1 instead of
  blaming GPU 0; the HTTP API doc comment no longer advertises unregistered
  routes.
