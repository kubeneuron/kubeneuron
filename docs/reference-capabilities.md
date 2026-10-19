# Capability matrix — what ships, per vendor

This page answers one question honestly: **is KubeNeuron really
vendor-neutral?** The product sentence says "a vendor-neutral GPU fleet
reliability control plane". That is a statement about construction, not about
shipped support, and the difference is exactly what this table records.

Every cell below was derived by reading the code, not the roadmap. A cell that
says *shipped* names the file that backs it. Where the code is weaker than a
reader would assume, the note says so.

## How to read a cell

| Status | Means |
|---|---|
| **shipped & hardware-validated** | The code path exists, is tested, **and has executed against real accelerator hardware of that vendor** with recorded evidence (a hardware run in `CHANGELOG.md` / `PRODUCTION_READINESS_PLAN.md`, or a captured fixture). |
| **shipped, not hardware-validated** | The code path exists and is exercised by unit tests against fixtures. It has never run against that vendor's hardware. Fixtures are written by humans; they prove the parser, not the world. |
| **shipped, hardware evidence limited** | The code path exists, is tested, **and a stated part of it has executed against real hardware** in a run recorded in `CHANGELOG.md`, but the recorded assertions cover only a subset of what the row claims. The cell's note names the run's source commit, the GPU, exactly what was observed, and what was not. This is not *hardware-validated*: everything the note does not list keeps the standing of the row above. |
| **seam only (no implementation)** | The interface, type, registry entry, or config surface accepts the vendor, and nothing implements it. An operator enabling it gets nothing — see the *silent no-op* warnings below for the two places where "nothing" is worse than an error. |
| **not applicable** | The capability is meaningless for that vendor by construction (an NVIDIA-native encoding for a non-NVIDIA device). |

Two rules govern edits to this page:

1. A cell may be promoted to *hardware-validated* only against a run recorded
   in the repository — `CHANGELOG.md`, `PRODUCTION_READINESS_PLAN.md`, a
   captured fixture, or a measured finding written into the code it came from.
   The same holds for *hardware evidence limited*: its note must point at the
   recorded run and list the assertions that run actually made.
   "It should work" is not evidence.
2. `hack/verify-docs.sh` machine-checks Table 1 against the filesystem in both
   directions (see [Anti-rot](#anti-rot)). Editing the table without editing
   the code — or vice versa — fails CI.

## Table 1 — vendor-specific capabilities

These are the capabilities whose implementation is tied to a vendor's driver,
tooling, kernel-log format, or Kubernetes resource names.

<!-- vendor-matrix:start -->

| Capability | NVIDIA | AMD | Intel |
|---|---|---|---|
| Kernel-log fault detection (`/dev/kmsg`) | shipped & hardware-validated — `internal/agent/kmsg/watcher.go` | shipped, not hardware-validated — `internal/agent/kmsg/amdgpu.go` | seam only (no implementation) |
| Polled telemetry fault detection (DCGM / vendor SMI) | shipped & hardware-validated — `internal/agent/gpuhealth/gpuhealth.go` | shipped, not hardware-validated — `internal/agent/amdhealth/amdhealth.go` (fixtures are synthetic: the amd-smi JSON schema is reconstructed, not captured) | seam only (no implementation) |
| Metric-alert fault detection (vmalert → Alertmanager) | shipped, not hardware-validated — `configs/vmalert/gpu-rules.yaml`, `internal/detect/alertmanager.go` | seam only (no implementation) | seam only (no implementation) |
| XID catalog (vendor-native fault encoding) | shipped & hardware-validated — `internal/detect/xid.go` | not applicable | not applicable |
| Neutral fault catalog (`(vendor, code)` → class) | shipped, not hardware-validated — `internal/detect/fault.go` (9 NVIDIA rows; coverage pinned by `fault_coverage_test.go`) | shipped, not hardware-validated — `internal/detect/fault.go` (9 AMD rows; coverage pinned by `fault_coverage_test.go`) | seam only (no implementation) |
| Accelerator inventory and runtime attestation | shipped & hardware-validated — `internal/agent/nvml/smi.go`, `internal/accelerator/nvidia/nvidia.go` | seam only (no implementation) | seam only (no implementation) |
| Idle / device-holder preflight | shipped, not hardware-validated — `internal/agent/executor/executor.go`, `internal/agent/nvml/holders.go` | seam only (no implementation) | seam only (no implementation) |
| Per-device reset | shipped, not hardware-validated — `internal/agent/nvml/smi.go`, `internal/agent/nvml/resetcap.go` | seam only (no implementation) | seam only (no implementation) |
| Quiesce / restore of the vendor monitoring stack | shipped & hardware-validated — `internal/platform/kubernetes/acceleratorstack.go`, `internal/agent/executor/acceleratorhost.go` | seam only (no implementation) | seam only (no implementation) |
| Vendor diagnostics (diag run, bug-report bundle) | shipped & hardware-validated — `internal/agent/executor/executor.go` | seam only (no implementation) | seam only (no implementation) |
| Driver reload / reinstall | seam only (no implementation) — `internal/agent/executor/executor.go` runs an operator-provisioned script; KubeNeuron ships none | seam only (no implementation) | seam only (no implementation) |

<!-- vendor-matrix:end -->

## Table 2 — vendor-independent capabilities

Two capabilities moved here in the round that added AMD detection: Kubernetes
node GPU inventory and `evict_gpu_workload` used to hardcode `nvidia.com/gpu`,
which made an AMD or Intel node inventory as zero GPUs and made the eviction
step report success without evicting anything. They now match any vendor's GPU
resource name (`isAcceleratorResource` in
`internal/platform/kubernetes/kubernetes.go`, including NVIDIA MIG
partitions), so they need no vendor package and belong on this side of the
line — validated on NVIDIA hardware, exercised by unit tests for the rest.

These paths never touch a vendor's driver. They are listed because half the
escalation ladder lives here, and because "shipped for AMD" in this table is a
weaker claim than it looks: **an action nothing can trigger is unreachable.**
No AMD or Intel fault is detected today, so no AMD or Intel incident opens, so
none of these rungs is ever climbed on those nodes unless a human opens the
incident by hand (`POST /api/v1/incidents`) or authors alert rules for a
non-NVIDIA exporter.

The same sentence used to apply to NVIDIA for a different reason, and it is
worth recording because it made every cell below weaker than it reads: an
installation from `deploy/install.sh` bound exactly ONE problem class, so on a
default install none of these rungs could be reached by a detected fault
either. The baseline pack in `config/policies` (applied by `install.sh`, and
`kubectl apply -k config/policies` otherwise) binds every class the detectors
emit; `internal/operator/policy_pack_test.go` fails the build if a class stops
being covered. That changes what is REACHABLE, not what is validated — every
evidence statement below still describes the same runs.

| Capability | Status | Notes |
|---|---|---|
| Cordon / drain | shipped & hardware-validated | `internal/platform/kubernetes/kubernetes.go`. Both executed for real on the EKS node in the confined destructive phase (`Cordon → Drain → ReplaceNode`, `hack/hw-e2e.sh`). Vendor-blind: eviction is a pod operation. The PDB-blocked retry loop is fixture-tested only — no hardware run has had an eviction refused by a budget. |
| Uncordon | shipped, not hardware-validated | Same file. Every hardware run either ended in a dry-run ladder or terminated the node, so the real uncordon (and the cordon janitor's recovery path) has not executed on hardware. |
| Node reboot | shipped, not hardware-validated | `internal/agent/executor/executor.go`. See the note below — this cell surprises people. |
| Cloud node replace (terminate for the autoscaler) | shipped & hardware-validated (AWS only) | `internal/cloud/aws/`, `internal/platform/kubernetes/recycle.go`. A real EC2 instance was terminated in the `kubeneuron-e2e10` run. Every non-AWS provider is seam only — `internal/cloud/providers/providers.go` registers exactly one. |
| Cloud node recycle (stop/start in place) | shipped, not hardware-validated (AWS only) | Same files. The AWS provider refuses autoscaling-group members (`cloud.ErrRecycleNotViable`), and every hardware node so far has been an ASG member, so the successful path has never run. |

## Table 3 — GPU reliability observability (v0.7, NVIDIA / DCGM only)

v0.7 adds a read-only observability layer beside the detection and
remediation paths above: a metric catalog, a wider dcgm-exporter profile, a
scrape label contract, recording rules, five Grafana boards and shadow-only
alerts. Every row is NVIDIA-specific by construction (the inputs are DCGM
field ids through dcgm-exporter). Exactly one hardware run is recorded for
this layer: the final G3 run of 2026-10-18 on an ephemeral EKS g4dn.xlarge /
Tesla T4 node from source `d06fcfa` (`CHANGELOG.md`, v0.7.0 evidence
section), whose read-only `test-observability` phase makes a deliberately
short list of assertions. Four rows below are therefore **shipped, hardware
evidence limited**, and their notes name exactly what that phase observed;
the other three stay **shipped, not hardware-validated**, because the phase
does not look at Grafana, fires no shadow alert, and never computed a
fail-slow score for a real peer group. No row is *hardware-validated*. The
AMD and Intel columns of Table 1 are untouched by v0.7; the catalog maps NVIDIA/DCGM sources only, and mapping
any other vendor's exporter is a later phase. The layer adds no action, no rung,
no controller or agent code path, no CRD field, no store migration and no
controller RBAC; a cell here is a claim about what an operator can *see*,
never about what KubeNeuron *does*.

| Capability | Status | Notes |
|---|---|---|
| Metric catalog (`kubeneuron_gpu_*` names, units, DCGM field ids, support cells) | shipped, hardware evidence limited (Tesla T4) | `internal/gpucatalog`, rendered to `config/gpu-metrics/catalog.yaml` and `docs/reference-gpu-metrics.md`. The catalog's own support cells are unchanged: the only `validated` cell is still the read of field 230 on a Tesla T4 from the v0.6.0-era hardware run; every other cell is `declared`, `not-applicable` or `unknown`. Hardware evidence (final G3 run, 2026-10-18, source `d06fcfa`): the real dcgm-exporter on a Tesla T4 served `DCGM_FI_DEV_POWER_USAGE` (tier T0), `DCGM_FI_DEV_ECC_SBE_VOL_TOTAL` (tier T1) and `DCGM_FI_DEV_GPU_TEMP`, its raw `DCGM_FI_DEV_FB_USED` sample was read back for the MiB-to-bytes check, and three canonical names — `kubeneuron_gpu_temperature_celsius`, `kubeneuron_gpu_power_watts` and `kubeneuron_gpu_memory_used_bytes` — were recorded from them with the catalog's units. No other catalog field or canonical name has been observed on any GPU by this project. |
| dcgm-exporter reliability profile (tier T0 + T1 fields) | shipped, hardware evidence limited (Tesla T4) | `deploy/kubernetes/dependencies/gpu-operator/values.yaml`, pinned to the catalog in both directions by `internal/gpucatalog`. Hardware evidence (same run): the profile ConfigMap reached an EKS cluster through the pinned GPU Operator chart v26.3.3; the dcgm-exporter DaemonSet rolled out on a Tesla T4 node beside DCGM client (`dcgmi`) 4.3.1 and host engine 4.5.2 (the deployed exporter is the checked-in source pin `4.5.3-4.8.2-distroless`; no image digest was captured); and the exporter served `DCGM_FI_DEV_POWER_USAGE`, `DCGM_FI_DEV_ECC_SBE_VOL_TOTAL` and `DCGM_FI_DEV_FB_USED` — three of the fields v0.7 adds — plus `DCGM_FI_DEV_GPU_TEMP` from the v0.6.1 list. The remaining added fields are listed from NVIDIA documentation and have not been observed on any GPU by this project; the fields a Tesla T4 cannot serve (row remap, NVLink, MIG) were checked absent from the recorded series, not zero. Reversible by the documented rollback. |
| Scrape label contract (`gpu_uuid`, `gpu_model`, optional `node_pool`, `mig_profile`, admin-set `cluster`; no `driver`) | shipped, hardware evidence limited (Tesla T4 / EKS) | `deploy/kubernetes/dependencies/observability/vmagent.yaml`, relabel rules evaluated by `internal/depprofile`. Needs read-only `nodes` access that the VictoriaMetrics operator's generated ClusterRole grants. Hardware evidence (same run): on the three canonical series named in the rows above, `node`, `gpu_uuid` and `gpu_model` were present and equal to the real node's name and the exporter's own `UUID` and `modelName`, and `node_pool` equalled the EKS nodegroup label read from that node. Not observed: `node_pool` on GKE or Karpenter nodes (still derived from documented labels), the admin-set `cluster` label, and `mig_profile` beyond its expected absence on a GPU without MIG. |
| Canonical and derived recording rules | shipped, hardware evidence limited (Tesla T4, three canonical rules) | `configs/vmalert/gpu-recording-rules.yaml`, mirrored in the deployed VMRule; catalog ⇄ rules pinned in both directions by `internal/gpucatalog`. No formula synthesises a sample: a missing input is a missing series. `vmalert -dryRun` on the rule files passes (a local syntax check of the files). Hardware evidence (same run): the deployed VMRule, evaluated by a live vmalert against a live VMSingle, recorded `kubeneuron_gpu_temperature_celsius`, `kubeneuron_gpu_power_watts` and `kubeneuron_gpu_memory_used_bytes` from the real exporter's samples for one GPU; `kubeneuron_gpu_memory_used_bytes` equalled `DCGM_FI_DEV_FB_USED` × 2^20 at the same instant; and the row-remap, NVLink and fail-slow rules recorded no sample for a GPU with no such input (the missing-input rule held on hardware for those three). Every other canonical rule and every derived rule were not asserted on hardware. |
| Fail-slow peer score (`kubeneuron_gpu_fail_slow_score_ratio`) | shipped, not hardware-validated — experimental | The fallback ratio-to-peer-median score with a throttle gate and a three-device floor, not the robust median/MAD score of the brief; formula in the catalog. Has never been computed for a peer group larger than one by this project, so its discrimination is untested. The only hardware observation is negative: on the single Tesla T4 of the 2026-10-18 run the score was absent, as the three-device floor requires. |
| Grafana boards (Fleet Reliability Overview, Health & Errors, Fail-slow Radar, Remediation & Recovered Capacity, GPU Passport) | shipped, not hardware-validated | `deploy/grafana/*.json`, generated from `internal/gpucatalog/dashboards.go` and pinned by `internal/metrics/dashboard_test.go`. Provisioned into a pinned Grafana 12.4.13 by the kind fixture (`make test-gpu-observability-kind`), where every variable and panel query executes against synthetic series without a query error; never loaded against live GPU data. The hardware run of 2026-10-18 did not look at Grafana. |
| Shadow-only alerts (`ShadowGpu*`) | shipped, not hardware-validated — observation only | `configs/vmalert/gpu-shadow-rules.yaml`, mirrored in the deployed VMRule. Routed to humans ahead of the controller webhook, absent from the controller's alert-to-class map, pinned by `internal/detect/shadow_isolation_test.go` and `internal/depprofile/shadow_routing_test.go`; the kind fixture fires one rule on a synthetic input through vmalert and the shipped Alertmanager route tree and sees it reach the human receiver only, with zero requests at the controller webhook address. The hardware run of 2026-10-18 fired no shadow alert, so no rule has fired on a real cluster. Not a detection capability of the product: no shadow alert opens an incident. |

Promoting any row to *hardware-validated* follows rule 1 above: a recorded
run on this code, not the fact that the files exist. The one recorded run
asserts only what the notes above list — it does not look at Grafana, fires
no shadow alert, and does not prove every catalog field, rule or dashboard —
which is why the four promoted rows stop at *hardware evidence limited*. The
hardware this project has access to (Tesla T4) has no NVLink, no row
remapping and no MIG, so the rows that read those fields cannot be validated
on it even when the gates run; nothing here is evidence for A100/H100. That
run is not release authorization: no `v0.7.0` tag or release exists.

## Evidence, per shipped cell

**Kernel-log detection — hardware-validated.** `internal/agent/kmsg/watcher.go`
parses `NVRM: Xid (PCI:…): NN,` lines. Validated on EKS `g4dn.xlarge` / Tesla T4
(`kubeneuron-e2e10`): XID 79 drove the full ladder and XID 92 the sub-threshold
hold. *Scope limit:* the XIDs were **injected into `/dev/kmsg`**
(`hack/hw-e2e.sh inject_xid`), not produced by a failing GPU. The kernel-log
transport, cursor, dedup and classification are hardware-proven; the claim "we
catch a real GPU dying" rests on the XID text format, not on an observed
failure.

**Polled telemetry — hardware-validated (DCGM path).** `internal/agent/gpuhealth`
polls `dcgmi dmon -e 230` and falls back to `nvidia-smi -q`. The DCGM path has
executed on a real Tesla T4 (EKS `g4dn.xlarge`) with the agent on the real
driver and a version-compatible DCGM client/engine: `hack/hw-e2e.sh test-dcgm`
injects a DCGM field value and the `gpuhealth` source observed it, most
recently in the full hardware run of 2026-09-13. *Scope limit:* the value is
injected, not produced by a failing GPU, and the `nvidia-smi -q` fallback is
still driven only by a hand-written fixture
(`internal/agent/gpuhealth/testdata/`) describing an A100 nobody has run.

**Metric-alert detection — not hardware-validated.** `configs/vmalert/gpu-rules.yaml`
is written against `DCGM_FI_*` series and `internal/detect/alertmanager.go` maps
rule names to problem classes; a unit test cross-checks rules against the map.
No hardware run has fired an alert through vmalert into the webhook.

**XID catalog — hardware-validated.** `internal/detect/xid.go` classified
kernel-log XIDs 79 and 92 read off a live node into `ClassFellOffBus` and
`ClassECCSBERate` during the EKS runs, including the latter's 3-in-24h
threshold hold.

**Neutral fault catalog — not hardware-validated.** `internal/detect/fault.go`
holds 18 rows across two vendors: 9 `nvidia` and 9 `amd`. Both vendors' rows
are emitted by shipping sources — the NVIDIA rows by the `nvidia-smi -q`
fallback, the AMD rows by `internal/agent/amdhealth` and the `amdgpu` kmsg
families — and `fault_coverage_test.go` fails the build if any class the XID
catalog can reach stops being reachable through a neutral `(vendor, code)` row.
`GPUSignalMapping.spec.faults[]` can add further rows declaratively
(`internal/detect/catalog.go`, covered by `catalog_test.go`).

**Classification is vendor-neutral; verification DEPTH is not.** A GPU-scoped
incident clears `verifyRuntimeEvidence` (`internal/controller/reconcile.go`)
against an accelerator report — and a report exists only for vendors this
build has a runtime adapter for, which today means NVIDIA alone. An AMD
incident is therefore verified on the agent's durable heartbeat, the same
evidence a node-scoped incident uses, and the controller logs that the depth
was reduced. It resolves; it is simply attested less strongly, and that is
stated rather than hidden.

Until v0.2.3 it did not resolve at all: the NVIDIA report was read
unconditionally, so an AMD device-scoped incident held for the evidence
deadline and then parked in `NEEDS_HUMAN` after the cordon and drain had
already run — and an AMD fleet's recovery report read 0% recovered for a
system that had recovered it.

**An NVIDIA node that no `AcceleratorRuntimeProfile` selects is verified the
same way, and for the same reason.** The operator-managed agent asks the
controller which profile selects its node and, when none does, holds
observation: it posts no accelerator report at all (the default-deny that
keeps a DaemonSet from claiming capabilities during a rollout). A profile is a
contract that narrows accelerator actions; the standard install ships none,
and creating one is not a prerequisite for closing an incident. So on such a
node the report cannot exist by construction, and a real (non-dry-run)
device-scoped incident resolves on the durable heartbeat plus the quiet
window, with the resolve audit entry naming that reduced depth. Without a
profile no accelerator-runtime action (reset, quiesce) can have been admitted
for that incident either, so the ladder it verifies is the same platform
ladder a node-scoped incident runs. The full-depth check is unchanged
wherever the agent WAS told to report: a node a profile selects still fails
closed on a missing or stale report, a fresh report that says the runtime is
not ready is never outranked by the heartbeat, and overlapping profiles or
unresolvable node labels fail closed with the cause in the reason. Until
v0.4.0 the no-profile case was indistinguishable from a degraded agent, so
every device-scoped incident on an ordinary Enabled install with a real
driver parked in `NEEDS_HUMAN` after the evidence deadline; hardware run 12
was the first to attempt one and found it.

**What AMD still does not get: arming, per-device reset, or a device-holder
preflight with anything to read.** Detection, classification, cordon, drain
and resolution work; the destructive rungs do not. The honest summary is that
for AMD this is a detect-protect-and-close product, not yet a
repair-the-device one.

**Accelerator inventory — hardware-validated.** `nvidia-smi --query-gpu=index,uuid,name,pci.bus_id`,
`driver_version` and `mig.mode.current` parsed real Tesla T4 output (driver
580.159.03) — the captured output is checked in at
`internal/agent/nvml/testdata/t4-g4dn-580.159.03.txt`. `Init`, `ListGPUs` and
`Healthy` also ran against a live L4 host.

**Kubernetes GPU inventory — hardware-validated; targeted eviction — not.**
`internal/platform/kubernetes/kubernetes.go` now spans every recognised
vendor: `acceleratorCount` reads the device count from whichever vendor
resource the node advertises, and `podUsesGPU` matches any vendor's GPU or MIG
partition. The capacity read was validated on EKS (the node advertised
`nvidia.com/gpu=1` and appeared with its GPU in the fleet inventory); the
non-NVIDIA paths are covered by tests only. `evict_gpu_workload` has never
executed on hardware — every hardware playbook used `Cordon → Drain`.

**Idle / device-holder preflight — not hardware-validated (the shipped path).**
`EnsureIdle` (index-addressed) ran against a live L4 and passed, and the holder
enumeration named real holders on the EKS T4 — the failure text
`GPU 0 is still held by nvidia-device-plugin(11621)` came from that run. But the
production reset path prefers the **UUID-addressed** variants
(`EnsureIdleByUUID`, `DeviceHoldersByUUID`, `nvidia-smi … -i <uuid>`), added to
close the resolve→reset renumber window, and those have never run on hardware;
the source marks them `HARDWARE-DEPENDENT`. The cell states the weaker fact
because the weaker fact is the one that executes.

**Per-device reset — not hardware-validated, and no successful reset has ever
been observed.** `SMI.ResetGPUByUUID` shells out to `nvidia-smi --gpu-reset`. On
every cloud instance tested the hypervisor exposes no PCI reset
(`/sys/bus/pci/devices/<addr>/reset` absent), so `nvml.ResetCapability` returns
unsupported and the NVIDIA adapter withholds the capability, the controller gate
refuses, and the ladder routes to reboot/replace. **That refusal is
hardware-validated on g4dn; the reset itself is not.** Bare metal is required to
change this cell.

**Quiesce / restore — hardware-validated, on weaker evidence than the rest.**
[playbook-authoring.md](playbook-authoring.md) states both steps have been
exercised against real NVIDIA hardware, and the design could only have come from
a live run: the node-side release exists because inferring a settled stack from
pod labels *failed on a real cluster* — a device plugin shipped in the machine
image carried labels the controller did not recognise and held `/dev/nvidia0`
after the step reported success. No `CHANGELOG.md` entry records that run as a
hardware phase, and `hack/hw-e2e.sh` has no quiesce phase, so this is the one
*hardware-validated* cell not backed by a repeatable harness. It remains the
prerequisite for a reset that has itself never succeeded.

**Vendor diagnostics — hardware-validated.** `dcgmi diag -r 1` ran on a live L4
(returning 226 with persistence mode off, then 0 after it was enabled), and
`nvidia-bug-report.sh` produced a bundle as root and failed honestly as a
non-root user.

**Node reboot — not hardware-validated.** The idempotency guard was validated
after a real reboot on the L4 host (`already rebooted: boot ID … differs from
requested …`). The **shipped mechanism** — `nsenter -t 1 … systemctl reboot`
from the distroless agent container — has never rebooted a node: the first
hardware attempt failed with `systemctl reboot: exit status 127` *after* a human
had approved it, which is why `Executor.PreflightReboot` exists. Every EKS
ladder that reached a reboot rung ran it in dry-run.

## Two silent no-ops (fixed in v0.2.2 and v0.2.3)

Most missing vendor support fails loudly — no driver, no detection source, no
events. Two places used to fail **quietly**, which is worse. Both are fixed;
they are kept here because the shape of the failure is the one to look for
when adding the next vendor.

1. **Node GPU count.** `nodeFromK8s` read `nvidia.com/gpu` capacity, so an AMD
   node (`amd.com/gpu`) or Intel node (`gpu.intel.com/i915`) was inventoried
   with **zero GPUs** and looked like a healthy CPU node. It now counts
   whichever recognised vendor resource the node advertises, preferring whole
   devices over MIG partitions so the mixed strategy does not count the same
   silicon twice.
2. **Targeted GPU-pod eviction.** `platform.evict_gpu_workload` selected pods
   by the same `nvidia.com/gpu` limit. On a non-NVIDIA node — and, as
   [the MIG decision](mig-decision.md) records, on a MIG node running the
   device plugin's *mixed* strategy, where pods request
   `nvidia.com/mig-1g.5gb` — it evicted nothing and **reported success**. The
   matcher now spans every vendor and MIG partitions, and evicting zero pods
   is reported as the refusal it is rather than as a completed step.

The two matchers are deliberately **not** the same function, and that
asymmetry is the thing to preserve. Deciding which pods hold a device errs
toward matching: a false positive costs one extra eviction on a node already
being drained, while a false negative leaves a live training job on hardware
about to be reset. Deciding which nodes are in the fleet errs toward refusing:
everything in the fleet is a machine a playbook may cordon, drain and reboot,
so membership is anchored to recognised vendor domains and a GPU-shaped
resource from anywhere else is ignored and logged. See
[upgrading](upgrade.md#fleet-membership-changed-in-v022--check-it-before-you-upgrade)
for the audit that change deserves.

## What is actually vendor-neutral today

Neutral **by construction** and proven by tests: the fault envelope
(`types.FaultSignal{Vendor, Source, Code}`), the `(vendor, code) → ProblemClass`
table, `AcceleratorVendor` as a first-class type (`nvidia`, `amd`, `intel`,
`google` all validate), the accelerator report protocol, `GPUSignalMapping`
fault overrides, the action registry's `Vendor` field, the incident model, the
safety gates, and the approval protocol.

NVIDIA-specific joints a second vendor hits, in the order they break — the
ledger in [design.md §2.4c](design.md) with this page's cell for each:

| Joint | Matrix consequence |
|---|---|
| ~~`verifyRuntimeEvidence` reads an NVIDIA accelerator report for every GPU-class target~~ | **fixed in v0.2.3.** A vendor with no runtime adapter now verifies on the agent heartbeat, logged as reduced depth, instead of holding for a report nothing can produce |
| arming requires the concrete `*nvml.SMI` driver | no non-NVIDIA node can be armed for destructive execution |
| the physical-reset gate is `CapabilityNVIDIAReset` / `allowNVIDIAReset` only | per-device reset is structurally NVIDIA-only, not merely unimplemented |
| device-holder preflight (`holders.go`) has no non-NVIDIA report to read | it reads the incident's own vendor now, but only NVIDIA agents produce holder data, so the pre-cordon obstruction check is unprotected elsewhere — conservative in direction, absent in effect |
| the quiesce stack manipulates NVIDIA components | the reset-prerequisite rungs are NVIDIA-only |

## Anti-rot

`hack/verify-docs.sh` enforces this page mechanically:

- **Derived fact (both directions).** Table 1's AMD and Intel columns are
  extracted between the `vendor-matrix` markers. If a non-NVIDIA implementation
  appears in the tree (`internal/agent/<vendor>*` or
  `internal/accelerator/<vendor>*`) while the column still says *seam only*, the
  build fails — the matrix went stale behind the code. If a column claims
  *shipped* while no such package exists, the build fails — the matrix went
  optimistic ahead of the code.
- **Forbidden claims.** `hack/stale-claims.txt` carries patterns for the prose
  spellings of shipped multi-vendor support: a non-NVIDIA vendor named as
  supported / shipped / hardware-validated, a sentence claiming support for
  NVIDIA plus a second vendor, a claim that a non-NVIDIA detection source or
  driver ships, and the vendor tools (`amd-smi`, `rocm-smi`, `xpu-smi`) named as
  implemented. They fail the build in any published document — **including this
  one**, which is why the spellings above are described rather than quoted; the
  first draft of this paragraph failed CI against its own examples.

When a vendor genuinely lands, both halves move in the same commit: the cells,
and the removal of the now-true patterns from `hack/stale-claims.txt`. That is
the same discipline that caught five documents claiming a rejected execution
mode two releases after it shipped.
