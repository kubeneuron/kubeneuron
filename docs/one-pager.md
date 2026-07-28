# KubeNeuron — GPU incident response that earns the right to act

**Autonomous GPU failure detection and safe, audited remediation for
NVIDIA Kubernetes clusters.**

![Fleet overview](assets/tour/02-overview.png)

## The problem

A single wedged GPU silently kills a multi-day training run. The failure
is in the kernel log at second zero — `NVRM: Xid 79` — but the first
human notices hours later, from a stalled loss curve or a pager storm of
secondary alerts. Then comes the manual ritual: find the node, cordon,
drain, decide about a reboot, remember to uncordon, and reconstruct what
happened for the postmortem — under pressure, at 3 a.m., with no record.

## What KubeNeuron does

It watches every node's kernel log, classifies NVIDIA XID faults in
seconds, opens an incident bound to the exact GPU, and walks a
declarative remediation playbook — cordon → drain → collect evidence →
reset/reboot → verify → uncordon — with a human approval gate in front of
every destructive step and an append-only audit trail as the system of
record.

**Detection in seconds, not scrape intervals.** The agent tails
`/dev/kmsg` directly — faults arrive before the next metrics scrape would
even run, with DCGM/NVML corroboration as runtime evidence.

## Why it is different

- **Fail-closed by construction.** Real destructive execution
  (`executionMode: Enabled`) is rejected by five independent layers until
  a hardware verification matrix passes. Concurrency limits, cooldowns,
  flap detection, maintenance windows, per-node pause, and a global big
  red button all fail toward *not acting*. The default production mode is
  dry-run with human-approved workflows.
- **Every actor is verifiable.** Sign in with a password from a
  Kubernetes Secret, **SSO via OIDC** (Okta, Keycloak, Dex, Entra), or a
  Kubernetes credential checked with TokenReview + RBAC. The audit trail
  records the verified identity of every approval, rejection, pause, and
  resume — a free-text name can never impersonate a person.
- **The queue cannot double-fire.** Actions are dispatched through a
  durable lease queue with attempt counters and node **boot-ID binding**:
  a controller failover replays into the *same* action, and a result
  posted after an unnoticed reboot is rejected. Proven by automated
  failover tests against both storage backends.
- **HA that routes to one writer.** PostgreSQL backend with leader
  election and readiness-follows-leadership; SQLite for single-node
  installs. Schema migrations are forward-only and upgrade-tested against
  the previous release.
- **Zero-trust node channel.** Agents authenticate with TLS 1.3 mTLS
  *plus* a projected Pod-bound token; the controller re-derives node
  identity server-side. The distroless agent runs the node's real
  `nvidia-smi` through opt-in read-only host mounts.
- **Operations included, not promised.** 20-panel Grafana dashboard,
  alert pack with per-alert runbooks, Slack/webhook/PagerDuty
  notifications with retry and dead-lettering, online backups, a
  documented upgrade path, Helm chart, and a CLI.

![Approval gate](assets/tour/04-drawer-approval.png)

## Proof, not claims

- Signed multi-arch releases with SBOMs (`v0.1.1` on GHCR).
- The full ladder — kernel-injected XID 79 → cordon → drain → approval →
  reboot → uncordon — validated end to end on a **real Tesla T4** on AWS
  EKS, approver identity in the audit.
- CI runs the store conformance suite against SQLite *and* PostgreSQL,
  a 63-case CRD admission matrix on a live API server, a
  controller-restart-mid-playbook scenario, and a release-to-HEAD
  upgrade test.

## Get started in minutes

```console
$ curl -sfL https://github.com/kubeneuron/kubeneuron/releases/latest/download/install.sh \
    | bash -s -- --version v0.1.1
✓ KubeNeuron is Ready.
  Sign in       admin / <generated password>
```

[Product tour with screenshots and a demo video](product-tour.md) ·
[Install guide](install.md) · [Operations](operations.md) ·
[Design document](design.md)
