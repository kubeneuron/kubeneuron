# Playbook authoring

Remediation behavior is data: **policies** bind a normalized problem class
to a **playbook**, and a playbook is an ordered list of typed steps with an
optional escalation target. On Kubernetes both arrive as CRDs
(`GPURemediationPolicy`, `GPUPlaybook`); the operator compiles them into
the controller's runtime files. These are action contracts, not a claim that
the Kubernetes-managed agent can perform the named host operation today:
`executionMode: Enabled` is rejected at admission and compilation. The
managed Kubernetes path remains `DryRun` until host tooling, crash-safe action
completion, and hardware-gated verification exist.

## The mental model

```
signal (class) ──policy (first match wins)──▶ playbook
playbook: step₁ → step₂ → … → verify quiet window → RESOLVED
   any step fails ──▶ on_failure.escalate_to (next rung) ──▶ … ──▶ NEEDS_HUMAN
```

- One open incident per (target, class); duplicate signals attach to it.
- A step marked `approval: Required` parks the incident in
  `AWAITING_APPROVAL` until a human decides (TTL-bounded, never
  auto-approved).
- After the last step the incident waits out the configured quiet window in
  `VERIFYING`; a recurring signal there fails verification and escalates.
- The playbook `cooldown` throttles repeat runs on the same target.

## Actions (closed set)

In the current Kubernetes preview every action is a dry-run contract. This
CPU-only environment does not validate NVIDIA, NVML, DCGM, GPU telemetry, or
host-side remediation.

| CRD action | Intended effect | Notes |
|---|---|---|
| `Observe` | records + notifies | observe-first playbooks hold in OBSERVING; policy `params.threshold`/`window` control escalation |
| `Cordon` / `Uncordon` | Kubernetes cordon | intended to annotate the node with the incident reason |
| `Drain` | Eviction API drain | intended PDB-aware behavior; set a generous `timeout` |
| `EvictGPUWorkload` | evicts only GPU-consuming pods | intended XID 94 targeted restart |
| `GPUReset` | `nvidia-smi --gpu-reset` on the node | host tooling and idle safety are prerequisites for future Enabled support |
| `RunDiag` | `dcgmi diag -r <params.diag_level>` | levels 1–3 when DCGM is available |
| `CollectBundle` | `nvidia-bug-report.sh` | future host-side bundle collection |
| `Reboot` | `systemctl reboot` | **must** set `approval: Required` when implementation is enabled |
| `IdleCheck` / `WaitIdle` | GPU idleness probes | typed contracts exist; cross-restart completion persistence is still required |
| `VerifyGPUHealth` / `VerifyNodeHealth` | health probes | agent heartbeat freshness is exercised; DCGM verification is roadmap |
| `OpenTicket` | notification with quarantine note | intended end of the ladder (RMA) |

`DriverReload`/`DriverReinstall`/`RunScript` are reserved for a future
host-provisioned scripts directory. They must use fixed names (or a strictly
validated `params.script`), never command content from the CRD.

## Compiler-enforced safety rules

The operator rejects (fail-closed), among others:

- `Reboot` without `approval: Required`;
- a GPU-targeted playbook containing node-scoped steps unless it declares
  `effects: [nodeScheduling]`;
- policies referencing unknown playbooks, escalation to missing playbooks,
  duplicate playbook names, invalid durations.

## A worked example

```yaml
apiVersion: kubeneuron.io/v1alpha1
kind: GPUPlaybook
metadata: { name: drain-and-reset }
spec:
  kubeNeuronRef: fleet
  target: GPU
  cooldown: "1h"
  steps:
    - { name: cordon,   action: Cordon }
    - { name: drain,    action: Drain,    timeout: "30m" }
    - { name: reset,    action: GPUReset, timeout: "5m" }
    - { name: verify,   action: VerifyGPUHealth, params: { quiet_window: "10m" } }
    - { name: uncordon, action: Uncordon }
  onFailure: { escalateTo: reboot }
---
apiVersion: kubeneuron.io/v1alpha1
kind: GPURemediationPolicy
metadata: { name: ecc-dbe }
spec:
  kubeNeuronRef: fleet
  priority: 10
  match: { class: ecc-dbe }
  playbookRef: drain-and-reset
```

## Authoring guidelines

1. **Every rung must leave the node safe on failure** — cordon before
   anything disruptive, and remember failure walks *up* the ladder, so the
   next rung inherits a cordoned, drained node.
2. **Put verification inside the playbook** and rely on the quiet window;
   a signal recurring during `VERIFYING` is your escalation trigger.
3. **Reserve approvals for irreversibility** (reboot, driver reinstall,
   RMA quarantine). Approval fatigue is a real failure mode.
4. **Tune cooldowns against your flap window** — the flap detector
   quarantines a (target, class) that reopens 3× in 24 h by default; a
   cooldown shorter than your typical recurrence just burns rungs.
5. **Test in dry-run**: `kubeneuronctl remediate <node> --class <class>`
   drives the exact ladder as audited no-ops.
