# MIG semantics — the decision

Status: **decided (product); hardware-gated (implementation)**, 2026-08-05.

The reset preflight refuses MIG instance UUIDs today. That is not the open
question — it has been settled since the physical GPU became the remediation
unit. The open question, and the one this page closes, is what *should* happen
when a fault lands on a GPU that is **partitioned into MIG instances**: does
remediating the parent evict every instance, refuse while any instance is busy,
or require a policy the operator writes?

This is a product decision, not missing code. It is written down before the code
because the wrong answer here is not a bug — it is a fleet-wide surprise the
first time an operator approves one reset and loses seven tenants' jobs.

## What the code does today

Four independent layers refuse, and one layer does not:

1. **The agent's topology probe is node-wide and fail-closed.**
   `nvml.SMI.PartitionTopology` returns `mig` if **any** GPU on the node reports
   `mig.mode.current: Enabled`. `[N/A]` (a MIG-incapable part such as a T4) is
   read as positive evidence of an unpartitioned device; a failed or partial
   query stays `unknown`.
2. **The NVIDIA adapter withholds the reset capability.** In
   `internal/accelerator/nvidia/nvidia.go`, any topology other than `none`
   yields `PreflightObservedOnly`: no `reset-device` capability is declared, and
   `Allows()` re-checks topology independently so a capability appended to the
   report later cannot make a partitioned node resettable.
3. **The controller gate requires verified-unpartitioned evidence.** The agent
   report carries `TopologySafety: partitioned` and `Readiness: degraded`;
   `AcceleratorRuntimeProfile.CheckReport` refuses a non-ready report outright,
   and `CheckAction` additionally refuses when the profile requires verified
   unpartitioned topology. `allowNVIDIAReset` is where an incident on a MIG node
   stops.
4. **The executor refuses the instance UUID.** `resolveResetIndex` fails closed
   on a `MIG-` prefixed UUID before it reads inventory, and
   `holders_test.go` pins that the reset never runs.

The layer that does **not** refuse is everything else. Cordon, drain,
`evict_gpu_workload`, quiesce, reboot, recycle and replace have **no MIG
awareness at all**. On a MIG node the ladder's *safest* rung — a single-device
reset — is the one that is disabled, while the rungs that destroy every instance
on every GPU of the node run exactly as they would on an unpartitioned node.
That inversion is the real defect in today's behavior, and it is independent of
which option below is chosen.

Two further facts, found while writing this page:

- **Targeted GPU-pod eviction can be a silent no-op on MIG.**
  `podUsesGPU` matches the `nvidia.com/gpu` limit. Under the device plugin's
  *mixed* strategy MIG pods request `nvidia.com/mig-1g.5gb`, so
  `platform.evict_gpu_workload` evicts nothing and reports success. Under the
  *single* strategy pods still request `nvidia.com/gpu` and it works. The step's
  behavior therefore depends on a device-plugin setting KubeNeuron never reads.
  (Drain is unaffected — it evicts by pod, not by resource.)
- **The holder preflight is blind on a MIG parent.** `DeviceHolders` resolves
  the device minor from `nvidia-smi -q`'s `Minor Number` and then scans
  `/proc/*/fd` for `/dev/nvidiaN`. MIG consumers hold `/dev/nvidia-caps/*`
  capability nodes in addition to the parent device node, so a parent-addressed
  holder scan cannot enumerate instance consumers. `parseMinorNumber` already
  treats `[N/A]` as an error, so the current outcome is a fail-closed refusal
  with an unhelpful message ("cannot determine which processes hold GPU 0")
  rather than "this GPU is partitioned".

## The options

**A. Keep refusing everything device-scoped on a partitioned GPU.**
Today's behavior, made explicit and permanent.
*For:* zero new code, zero new failure modes, no chance of taking down an
instance nobody accounted for. *Against:* it leaves the inversion above in
place. A MIG node's ladder skips straight from "observe" to "drain the node and
reboot it", which destroys strictly more work than the reset it refused. It also
means MIG fleets — which is to say most A100/H100 fleets that partition — get
the least automation and the most disruption.

**B. Parent remediation evicts every instance.**
Treat the physical GPU as the unit: enumerate the instances, evict every pod
holding any of them, then run the existing device-scoped ladder.
*For:* matches the hardware (there is no per-instance reset; a GPU reset takes
the whole device), and it is the only option that lets a MIG node use the
cheap rung. *Against:* on its own it is a blast-radius surprise. The incident's
evidence names one device; the approval a human sees says "reset GPU
`GPU-abc…`"; the effect is up to seven jobs in up to seven namespaces dying.
Evidence and effect must not disagree that badly.

**C. Refuse while any instance is busy.**
Same unit as B, but the preflight is all-instances-idle: remediate the parent
only when every instance on it is free; otherwise hold, then escalate.
*For:* the strongest workload protection, and a direct extension of the existing
`idle_check` / `wait_idle` contract. *Against:* on a well-packed MIG node the
instances are never all idle, so in practice this degenerates to option A —
except after a drain, which is exactly when B would also be safe. Alone, it is
option A with extra code.

**D. A dedicated MIG policy surface.**
An operator-authored policy decides: refuse, evict-all, or wait-for-idle, per
node class / per MIG profile, with its own approval text.
*For:* different fleets genuinely differ (a research cluster's 1g.5gb instances
are not a production inference tenant's). *Against:* it is a knob for a decision
nobody has data to make yet, and every knob is a way to be misconfigured into
the surprise in B. Policy is the right *eventual* shape, not the first one.

**E. Per-instance remediation.**
Reset the instance, not the GPU. **Rejected on technical grounds, permanently.**
NVIDIA exposes no per-instance reset: recovery at instance granularity means
destroying and recreating compute/GPU instances, which is a MIG-manager
lifecycle operation that reconfigures the *partitioning* — a different product
capability with a different owner (the GPU Operator's `mig-manager`), and one
that changes what the scheduler advertises. KubeNeuron must never do it as a
remediation step. The `MIG-` refusal in `resolveResetIndex` is the enforcement
of this line, and it stays.

## Decision

**Adopt B + C together, opt-in, with the blast radius named in the approval —
and fix the inversion now.**

Concretely, the decided semantics:

1. **The physical GPU remains the only remediation unit.** No per-instance
   reset, ever (option E stays rejected). The `MIG-` refusal stays.
2. **Parent remediation is permitted only when the agent can enumerate the
   parent's instances.** No enumeration, no device-scoped remediation — the
   current fail-closed behavior is the fallback, not the default answer.
3. **The preflight is all-instances-idle, not parent-idle.** Every instance on
   the parent must be free of compute processes and of device-node holders
   before a reset. A busy instance blocks the reset exactly as a busy GPU does
   today; `wait_idle` bounds the wait, and the ladder escalates on expiry. This
   is C.
4. **Eviction is explicit, enumerated, and approved.** When the ladder needs the
   instances emptied, the step evicts the pods holding them — it never relies on
   `nvidia.com/gpu` matching — and the approval request names the exact blast
   radius: *N* instances, the pods and namespaces holding them, and the profile
   (`1g.5gb` …). A human approving a MIG-parent reset must see that they are
   approving *N* evictions. This is B, with the surprise removed.
5. **Opt-in per installation.** MIG-parent remediation is off until an operator
   enables it, because the enumeration and idle semantics cannot be validated
   without MIG hardware (see below). Off means today's fail-closed refusal,
   which is a safe default rather than a placeholder.
6. **Fix the inversion regardless of (5).** Two changes are correctness fixes,
   not part of the gated feature: the GPU resource name must become a set that
   includes `nvidia.com/mig-*` (and, for §1.3 of the definition plan, other
   vendors' names), and a refusal on a partitioned device must say *why* —
   "GPU 0 is partitioned into MIG instances; per-instance reset is not a
   remediation unit" beats "cannot determine which processes hold GPU 0".

### Why this and not the others

Option A is the honest status quo, and it is the one to keep if the work is
never done — but adopting it as *the decision* institutionalizes a ladder that
prefers rebooting a whole node to resetting one device, on precisely the
hardware (A100/H100) where a node is most expensive to lose. B alone trades a
device-scoped surprise for a tenant-scoped one; the fix is not to refuse B but
to make the blast radius visible where the decision is made — which is the
approval, the same place this product already puts every other irreversible
choice. C alone is A with extra code, but as B's precondition it is what turns
"evict everything" into "evict what is genuinely in the way, after proving it is
not idle already". D is where this ends up once real fleets have opinions; it
should not be the first thing built, because a policy surface with no validated
mechanism behind it is a configuration language for behavior that does not
exist.

## What this implies for the idle/holder preflight

The current preflight cannot implement (3) as written:

- `EnsureIdleByUUID` addresses the parent with `nvidia-smi --query-compute-apps
  -i <uuid>`. Whether that lists processes running **inside** the parent's MIG
  instances is unverified. If it does not, a parent-addressed idle check on a
  fully occupied MIG GPU returns *idle* — the most dangerous possible false
  negative, and the reason (5) is opt-in.
- The holder scan must learn `/dev/nvidia-caps`. Enumerating instance holders
  means resolving each GI/CI to its capability nodes and scanning `/proc/*/fd`
  for those, in addition to the parent's `/dev/nvidiaN`.
- `parseMinorNumber`'s `[N/A]` case needs a MIG-aware branch rather than a bare
  error, so the refusal explains itself.
- The accelerator report needs the partition inventory it deliberately does not
  produce today: `AgentAcceleratorDevice` already models
  `Kind: partition` with `ParentID` and `PartitionProfile`, and
  `internal/accelerator/nvidia` deliberately never populates it ("this adapter
  never invents MIG partitions"). Populating it from `nvidia-smi -L` / `mig -lgi`
  is the enabling change, and it makes the controller's blast-radius message
  possible without new plumbing — the protocol was designed for this.

## What this implies for the workload-protection story

- Targeted eviction must select GPU pods by any accelerator resource, not by one
  constant. Until then, `EvictGPUWorkload` is a documented no-op under the mixed
  strategy and the honest advice is to reach the same effect through `Drain`.
- The protection metric proposed in §3.1 of the definition plan
  (`kubeneuron_destructive_steps_deferred_total{reason}`) gains a `mig-busy`
  reason: on a MIG fleet, "we declined to reset because instance 3 was training"
  is the protection story, and it is invisible otherwise.
- Approval text is part of the protection, not decoration. The blast-radius
  sentence is what makes B safe; if it cannot be rendered, the step must refuse.

## What cannot be validated without A100/H100 MIG hardware

Everything in this list is currently an assumption. None of it should be
promoted to a claim without a run recorded in `CHANGELOG.md`:

1. Whether `nvidia-smi --query-compute-apps -i <parent-uuid>` reports processes
   running inside MIG instances (decides whether the parent idle check is
   trustworthy at all).
2. Whether a MIG-enabled parent reports `Minor Number: [N/A]`, as
   `internal/agent/nvml/holders.go` assumes — and which device nodes its
   instance consumers actually hold.
3. Whether `nvidia-smi --gpu-reset` on a MIG parent succeeds with instances
   configured but idle, or requires the instances to be destroyed first (and
   whether MIG mode itself survives the reset, or returns as *pending*).
4. The exact output shapes of `nvidia-smi -L` / `mig -lgi -lci` across driver
   versions, which the partition inventory would parse.
5. Whether `mig.mode.current` can disagree with `mig.mode.pending` on a live
   node, which would make the node-wide topology probe stale rather than wrong.
6. Whether a per-GPU topology answer (this GPU is partitioned, that one is not)
   is available and worth having — today one MIG-enabled GPU makes the whole
   node ineligible, which is safe and coarse.

Estimated cost: one rented A100 or H100 day, in the same shape as the existing
`hack/hw-e2e.sh` recipe. Items 1–3 are the gate; 4–6 are the follow-on.

## What changed with this decision

- [design.md §2.4d](design.md) — the MIG invariant now records the decided
  parent semantics (all-instances-idle, enumerated blast radius, per-instance
  reset permanently rejected) instead of "semantics remain a gated decision".
- `internal/agent/executor/executor.go` — the `MIG-` refusal's comment now
  states *why* the parent is the unit and what an implementation would owe,
  rather than only that it refuses.
- Nothing else. The gated mechanism is not built; the fail-closed refusal is
  still the shipped behavior, and [the capability
  matrix](reference-capabilities.md) still reads *shipped, not
  hardware-validated* for per-device reset.
