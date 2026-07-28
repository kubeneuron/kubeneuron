# KubeNeuron

**GPU failure detection and remediation for NVIDIA clusters, with a
Kubernetes-native configuration model.**

KubeNeuron models GPU and driver signals (XID errors, ECC faults, row-remap
exhaustion, NVLink/PCIe degradation, thermal events) as an audited,
policy-driven escalation ladder: observe → workload restart → GPU reset →
drain-and-reset → reboot → driver remediation → hardware escalation.

!!! warning "Status: Kubernetes dry-run preview; not production-ready"
    The end-to-end **dry-run** loop is implemented and tested. The
    operator rejects `Enabled` mode (real side effects) at admission and
    compilation because its managed agent runtime has not passed host-tooling
    and hardware-gated verification. Keep production installs in `DryRun`.
    See the
    [roadmap](https://github.com/kubeneuron/kubeneuron/blob/main/ROADMAP.md).

## Why KubeNeuron

- **Typed, closed remediation actions** — configuration can select actions
  and scripts, never inject commands.
- **Fail-closed safety at every layer** — dry-run by default, concurrency
  caps, cooldowns, flap quarantine, human approvals with TTL, maintenance
  windows, per-node pauses, and a transactional audit trail for every
  transition.
- **Two detection paths** — kernel-log XIDs seconds after they happen
  (agent fast path) and metric-based alerting through
  vmalert → Alertmanager (slow path), converging on one incident per
  (target, class).
- **No new trust surface for actions** — agents poll a durable work queue
  over their existing mTLS + Pod-token channel; nodes run no listener and
  hold no server certificate.

## Where to start

| I want to… | Read |
|---|---|
| See the loop work in 15 minutes, no GPU | [Quickstart](quickstart.md) |
| Install on a cluster | [Production install](install.md) |
| Run it day-2: tokens, pauses, backups, metrics | [Operations](operations.md) |
| Write or tune remediation behavior | [Playbook authoring](playbook-authoring.md) |
| Understand why each XID maps where it does | [XID catalog](xid-catalog.md) |
| Understand the architecture and its honest limits | [Design](design.md) |
