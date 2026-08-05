# Making the definition true

> **A vendor-neutral GPU fleet reliability control plane that detects
> degradation, protects workloads, automates safe recovery, and measures
> recovered.**

That sentence is the product's contract. This page grades each clause
against what actually ships today, and lays out the work that closes the
gap — in the order that retires the most risk per unit of effort.

Grading scale: **shipped** (in a released tag, exercised by tests),
**partial** (real but narrower than the word implies), **stated**
(architecture supports it; no implementation).

| Clause | Today | Gap |
|---|---|---|
| vendor-neutral | **stated** | seams are vendor-agnostic; every shipping detection and action path is NVIDIA |
| GPU fleet reliability control plane | **shipped** | — |
| detects degradation | **partial** | kmsg XID + DCGM/nvidia-smi; DCGM path never validated on live hardware; no predictive signals |
| protects workloads | **partial** | evict/cordon/drain/idle-check ship; no workload-aware scheduling feedback, no checkpoint coordination |
| automates safe recovery | **shipped** | validated live on EKS, including a real destructive node replace |
| measures recovered | **shipped (v0.2.2)** | metrics land this release; dashboard and periodic report do not exist yet |

---

## 1. Vendor-neutral — the largest gap, and the one with a real seam

**What is already true.** The fault envelope is vendor-agnostic by
construction: `types.FaultSignal{Vendor, Source, Code}` carries a neutral
fault with no XID pretense, `internal/detect/fault.go` maps
`(vendor, code) → ProblemClass` through a table, `AcceleratorVendor` is a
first-class type, `AcceleratorRuntimeProfile` selects per-vendor runtime
contracts, and `GPUSignalMapping` already accepts `source: fault` with
`{vendor, code}` overrides. A unit test classifies a synthetic
`amd/page-retirement` fault today — the plumbing accepts a second vendor;
nothing produces one.

**What is missing**, in dependency order:

**1.1 An AMD detection source** (~1 week). A `internal/agent/amdhealth`
sibling of `gpuhealth`, polling `amd-smi metric` / `rocm-smi --showretiredpages`
and emitting `FaultSignal{Vendor: "amd", Source: "amd-smi", Code: …}`.
Codes worth the first pass: `page-retirement`, `ecc-uncorrectable`,
`xgmi-link-error`, `thermal-throttle`, `gpu-lost`. Each needs a
`faultTable` row mapping to an existing `ProblemClass` — no new classes,
which is the point of a neutral catalog.

**1.2 Kernel-log parity** (~3 days). `amdgpu` logs to the same `/dev/kmsg`
the XID watcher already tails; the parser is a second regex family plus a
vendor tag on the event. The cursor, dedup, and spool machinery are
shared and need no change.

**1.3 Vendor-scoped inventory and actions** (~1 week). `nvml.SMI` becomes
one implementation of an accelerator driver interface; `amd-smi` is the
second. Reset semantics differ (`amd-smi reset --gpu`), and the
reset-capability evidence gate must learn AMD's answer rather than
assuming NVIDIA's PCI path. Node reboot/replace are already vendor-blind.

**1.4 An honest capability matrix** (~1 day). A table in
`docs/reference-*` stating, per vendor, which detection sources and which
actions are supported and validated — and a stale-claims lint entry that
fails the build if the README's vendor sentence drifts from it.

**Gate:** none of this changes the incident model, the safety gates, or the
approval protocol. That is the test of whether the seam was real.

**Recommendation:** do 1.1 + 1.2 + 1.4 as one release theme (v0.3.0). Hold
1.3 until an AMD node exists to validate against — shipping unvalidated
destructive actions for a vendor nobody has tested is exactly the failure
this project spent twelve rounds designing against.

---

## 2. Detects degradation — broaden, then prove

**2.1 Validate the DCGM path on live hardware** (~half a day of harness
work, one paid run). The second detection source ships, but every hardware
run so far injected XIDs through `/dev/kmsg`; the DCGM poll's column
parsing has only ever seen fixtures. `hack/hw-e2e.sh test-dcgm` is written
for exactly this and has never executed. **Highest value per dollar in
this document** — it converts a shipped-but-unproven path into a proven
one.

**2.2 Predictive signals** (~1 week). Today's catalog is reactive: a fault
happened. Degradation has trends — remapped-row burn rate, SBE rate slope,
thermal headroom shrinking run over run. A `trend` signal source that
opens an *observe* incident before the hard fault would make "detects
degradation" mean what a reader assumes it means. Requires care: a noisy
predictive class that pages people is worse than no class at all, so the
first version must be observe-only with no ladder beyond notification.

**2.3 Vendor-neutral fault coverage audit** (~2 days). The XID table is
rich; the neutral `faultTable` has a handful of rows. Every class reachable
by XID should be reachable by a neutral code, or the catalog is neutral in
form only.

---

## 3. Protects workloads — the clause with the least written down

This is the clause a platform team buys, and it is the one the docs
currently undersell. What ships: `platform.evict_gpu_workload`, cordon,
drain with PodDisruptionBudget respect, `agent.idle_check` /
`agent.wait_idle` before anything destructive, maintenance windows,
per-node pauses, and concurrency caps that stop a fleet-wide storm from
draining half the cluster at once.

**3.1 Make the protection visible** (~2 days). None of the above appears
in a metric or in the panel as *protection*. Add
`kubeneuron_workloads_evicted_total{node,reason}` and
`kubeneuron_destructive_steps_deferred_total{reason}` (idle-check refusal,
maintenance window, concurrency cap, PDB block) — the count of times the
system chose *not* to disrupt is the protection story, and right now it is
invisible.

**3.2 Checkpoint coordination** (~1–2 weeks, design first). Before a
reboot or replace, a training job that supports checkpointing would rather
be told than evicted. A pre-drain hook — an annotation on the workload
naming a signal endpoint, with a bounded wait — turns eviction from a loss
into a pause. This is the single largest product differentiator available
and deserves its own design document before any code.

**3.3 Scheduler feedback** (~1 week). A node under an open incident should
stop attracting new GPU work even before cordon: a taint applied at
OBSERVING and removed at RESOLVED, opt-in per policy. Cheap, and it stops
the fleet from feeding jobs into a device that is already failing.

---

## 4. Automates safe recovery — finish the hardware story

Shipped and validated live: the full ladder, confined destructive
execution, cloud node replace with a real instance termination, approval
rounds bound to what the human saw.

**4.1 Bare-metal per-device reset** (blocked on hardware). Cloud guests
have no PCI reset; the agent refuses on measured evidence and routes to
node replace. Validating an actual reset needs a bare-metal box — one day
of rental, ~$50–150. Until then the claim stays scoped exactly as it is
today.

**4.2 Negative-path verification** (~half a day + the same paid run as
2.1). `test-verify-recur` — a recurrence during VERIFYING must escalate,
not resolve — is written and never run. Bundle it with the DCGM phase.

**4.3 MIG semantics** (decision first, ~1 day). The reset preflight refuses
MIG instance UUIDs today (physical GPU is the remediation unit). Whether a
MIG parent's remediation should evict every instance, or refuse while any
instance is busy, is an unmade product decision, not missing code.

---

## 5. Measures recovered — land the number, then make it legible

**Shipped in v0.2.2** (this release):
`kubeneuron_incident_duration_seconds{class,outcome}` (MTTR),
`kubeneuron_incidents_recovered_total{class,unattended}` (automation
yield), `kubeneuron_degraded_gpu_seconds_total{class,outcome}`
(GPU-seconds under incident, `resolved` = returned to service). Emitted
once per incident on the committed halting transition.

**5.1 A recovery dashboard row** (~1 day). Three panels in
`deploy/grafana/kubeneuron-dashboard.json`: GPU-hours degraded vs
recovered over the window, unattended-recovery percentage, MTTR p50/p90 by
class. Without a default panel the metrics exist and nobody looks at them.

**5.2 A periodic recovery report** (~2 days). `kubeneuronctl report
--since 30d` printing exactly what a capacity owner asks for: GPU-hours
degraded, share recovered, share recovered without a human, top classes by
cost, incidents still open. Text and JSON. This is the artifact that
renews the budget.

**5.3 An SLO the operator can set** (~3 days, after 5.1/5.2 have run for a
quarter). `spec.reliability.targets` — e.g. "95% of `fell-off-bus`
incidents recovered within 30 minutes" — with a burn-rate alert. Do not
build this before real data exists to calibrate it.

---

## Sequencing

| Release | Theme | Contents |
|---|---|---|
| **v0.2.2** | measure what we already do | §5 metrics (done), release-pipeline proof via rc tag, operational pack |
| **v0.2.3** | prove and expose | 2.1 + 4.2 (one paid hardware run), 5.1 dashboard, 3.1 protection metrics, 5.2 report |
| **v0.3.0** | vendor-neutral in fact | 1.1 + 1.2 + 1.4 (AMD detection, kernel parity, capability matrix), 2.3 catalog audit |
| **v0.3.x** | protect harder | 3.3 scheduler feedback, 3.2 checkpoint coordination (design first) |
| **hardware-gated** | — | 4.1 bare-metal reset, 1.3 AMD actions, 4.3 MIG decision + hardware |

The ordering is deliberate: **measure before extending** (you cannot tell
whether AMD support helped without a recovery number), **prove before
claiming** (the DCGM path is shipped and unproven — that is a claim
waiting to become a lie), and **decide before building** for the two items
that are product questions rather than engineering ones (checkpoint
coordination, MIG).

## The rule this document exists to enforce

Every clause of the definition must be gradeable against the code. When a
clause moves from *stated* to *shipped*, its evidence goes in
`CHANGELOG.md`, its claim goes in the README, and — if it can rot — its
old spelling goes in `hack/stale-claims.txt` so CI fails when the two
drift apart. That is the same discipline that caught five documents
claiming a rejected execution mode two releases after it shipped.
