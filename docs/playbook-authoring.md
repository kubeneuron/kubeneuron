# Playbook authoring

Remediation behavior is data: **policies** bind a normalized problem class
to a **playbook**, and a playbook is an ordered list of typed steps with an
optional escalation target. On Kubernetes both arrive as CRDs
(`GPURemediationPolicy`, `GPUPlaybook`); the operator compiles them into
the controller's runtime files.

`DryRun` is the default and every step is an audited no-op in it. Real
execution requires `executionMode: Enabled`, which in turn requires
`spec.safety.destructiveExecution` naming the permitted nodes and carrying the
exact acknowledgement string — and each step still passes the safety, runtime
attestation, and approval gates at execution time.

## The mental model

```
signal (class, vendor) ──policy (exact vendor, then first generic fallback)──▶ playbook
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

`GPUReset`, `Reboot`, `QuiesceAcceleratorStack` and `RestoreAcceleratorStack`
have been exercised against real NVIDIA hardware; a successful GPU reset has
not, because no cloud VM tested so far permits one (see below).

| CRD action | Intended effect | Notes |
|---|---|---|
| `Observe` | records + notifies | observe-first playbooks hold in OBSERVING; policy `params.threshold`/`window` control escalation |
| `Cordon` / `Uncordon` | Kubernetes cordon | intended to annotate the node with the incident reason |
| `Drain` | Eviction API drain | intended PDB-aware behavior; set a generous `timeout`; `params.force` — see below |
| `EvictGPUWorkload` | evicts only GPU-consuming pods | intended XID 94 targeted restart |
| `GPUReset` | `nvidia-smi --gpu-reset` on the node | refuses while any process holds the device node, naming the holders; pair with `QuiesceAcceleratorStack` |
| `QuiesceAcceleratorStack` / `RestoreAcceleratorStack` | stops/restarts the GPU vendor's own monitoring on the node | required before `GPUReset` on a GPU Operator cluster (see below) |
| `RunDiag` | `dcgmi diag -r <params.diag_level>` | levels 1–3 when DCGM is available |
| `CollectBundle` | `nvidia-bug-report.sh` | the agent runs it host-side into a bundle |
| `Reboot` | asks the host's init to reboot, from PID 1's namespaces | **must** set `approval: Required`; idempotent on `boot_id` so a retry cannot bounce the node twice |
| `RecycleNode` | cloud stop/start of the instance | approval-forced; AWS via scoped IRSA; refuses ASG members |
| `ReplaceNode` | terminates the instance for the autoscaler to replace | approval-forced; AWS via scoped IRSA |
| `IdleCheck` / `WaitIdle` | GPU idleness probes | the action journal persists intent and outcome across agent restarts |
| `VerifyGPUHealth` / `VerifyNodeHealth` | health probes | agent heartbeat freshness is exercised; DCGM verification is roadmap |
| `OpenTicket` | notification with quarantine note | intended end of the ladder (RMA) |

`DriverReload`/`DriverReinstall`/`RunScript` are reserved for a future
host-provisioned scripts directory. They must use fixed names (or a strictly
validated `params.script`), never command content from the CRD.

### `Drain` and `params.force`

A pod with no controller has nothing that would recreate it elsewhere, so
evicting it destroys work outright. `kubectl drain` refuses such a node for that
reason and makes you type `--force`; `Drain` refuses for the same reason, before
it evicts anything, and names the pods.

`params.force: "true"` is how a playbook says the eviction should happen anyway.
It is off by default, there is no global switch, and **the step must also set
`approval: Required`** — the loader refuses the playbook otherwise. The pods it
destroys are usually somebody's `kubectl run` or the debug shell an engineer
left open on the very node that is failing, so the eviction should be a decision
written down in a playbook and confirmed by a human, not a default nobody chose.

Approval is required because every other gate that reasons about blast radius —
the action registry, the destructive-step confinement, the compiler's whole-VM
rule — sees an unchanged `Drain`. An ordinary drain moves work; this one ends
it. `RecycleNode` and `ReplaceNode` force approval because they destroy an
instance; this destroys a tenant's running job.

The pods a forced drain destroys are named in the step's log line and counted by
`kubeneuron_forced_unmanaged_evictions_total`, so "where did my job go" has an
answer that does not depend on a Kubernetes Event for a pod that no longer
exists.

The trade-off is real in both directions. Without `force`, one transient bare
pod makes that node undrainable — and because every rung of the shipped ladder
begins with `cordon, drain`, the refusal fires at each one in turn: four rungs,
four pages, and no repair, ending in `NEEDS_HUMAN`. Nothing more destructive
runs, which is the point, but nothing is fixed either. Decide per playbook which
is worse for the fault class it handles.

Two spellings that used to fail quietly now fail at load: a non-boolean value
(`force: yes` read as "no" and you found out at 3am), and `force` on any action
other than `Drain`, where it was validated and then silently ignored.

## Why a GPU reset needs a quiesce step

Draining the node is not enough to reset a GPU. NVIDIA's own components keep
open handles on the device nodes: on a stock GPU Operator install, measured on
live hardware, `nv-hostengine`, `dcgm-exporter` and the device plugin each hold
`/dev/nvidia0` without ever appearing as compute applications. With any of them
running, `nvidia-smi --gpu-reset` fails with exit 19 and the text "currently in
use by another process", which names nothing.

`QuiesceAcceleratorStack` does two things. It switches the GPU Operator's
components off through the vendor's own `nvidia.com/gpu.deploy.*` node labels,
touching only those that were running so a restore never enables something the
cluster had deliberately switched off. Then it asks the node itself to release
what no label can reach — chiefly `nvidia-persistenced`, an ordinary host
service that holds the device even on a fully drained node — and to confirm,
from its own process table, that nothing holds the GPU any more.

That confirmation is deliberately the node's job. Inferring it from pod labels
failed on a real cluster: the device plugin came from the machine image rather
than the GPU Operator, carried labels the controller did not recognise, and the
step reported a settled stack while the plugin still held `/dev/nvidia0`. If
anything still holds the device when the step's timeout expires, the step fails
with those processes named rather than clearing a reset that cannot work.

Two consequences worth knowing:

- **DCGM supplies the attestation the reset gate requires, and stopping it
  erases that attestation.** The quiesce step therefore validates the evidence
  first and pins it for the rest of the playbook. If the evidence is not already
  good enough to admit a reset, nothing is switched off at all.
- **Monitoring always comes back.** `RestoreAcceleratorStack` is the explicit
  step, but the controller also restores automatically once the incident stops
  running, so a playbook that fails at the reset cannot leave the cluster blind.

Components outside the GPU Operator are not covered, and this is not
hypothetical: on EKS the NVIDIA device plugin ships in the machine image as a
`kube-system` DaemonSet with no `nvidia.com/gpu.deploy.*` label, so nothing
KubeNeuron does stands it down. Measured on that cluster, the quiesce released
everything else — the DCGM engine, the exporter, the persistence daemon — and
then failed by name on the one it could not reach:

```
quiesce_accelerator_host: GPU 0 is still held by 3 process(es):
  nvidia-device-p(11621) holds /dev/nvidia-uvm, ... /dev/nvidia0, ... /dev/nvidiactl
```

Stand such a plugin down yourself before the reset, or the step will keep
failing this way. It fails loudly rather than proceeding into a reset that
cannot succeed.

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
  # Cordon/Drain and the quiesce steps act on the whole node, so a GPU-target
  # playbook must declare that effect; the compiler rejects it otherwise.
  effects: [nodeScheduling]
  steps:
    - { name: cordon,   action: Cordon }
    - { name: drain,    action: Drain,    timeout: "30m" }
    - { name: quiesce,  action: QuiesceAcceleratorStack, timeout: "5m" }
    - { name: reset,    action: GPUReset, timeout: "5m" }
    - { name: restore,  action: RestoreAcceleratorStack, timeout: "5m" }
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
