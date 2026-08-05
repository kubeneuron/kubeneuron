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
| **seam only (no implementation)** | The interface, type, registry entry, or config surface accepts the vendor, and nothing implements it. An operator enabling it gets nothing — see the *silent no-op* warnings below for the two places where "nothing" is worse than an error. |
| **not applicable** | The capability is meaningless for that vendor by construction (an NVIDIA-native encoding for a non-NVIDIA device). |

Two rules govern edits to this page:

1. A cell may be promoted to *hardware-validated* only against a run recorded
   in the repository — `CHANGELOG.md`, `PRODUCTION_READINESS_PLAN.md`, a
   captured fixture, or a measured finding written into the code it came from.
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
| Polled telemetry fault detection (DCGM / vendor SMI) | shipped, not hardware-validated — `internal/agent/gpuhealth/gpuhealth.go` | shipped, not hardware-validated — `internal/agent/amdhealth/amdhealth.go` (fixtures are synthetic: the amd-smi JSON schema is reconstructed, not captured) | seam only (no implementation) |
| Metric-alert fault detection (vmalert → Alertmanager) | shipped, not hardware-validated — `configs/vmalert/gpu-rules.yaml`, `internal/detect/alertmanager.go` | seam only (no implementation) | seam only (no implementation) |
| XID catalog (vendor-native fault encoding) | shipped & hardware-validated — `internal/detect/xid.go` | not applicable | not applicable |
| Neutral fault catalog (`(vendor, code)` → class) | shipped, not hardware-validated — `internal/detect/fault.go` | shipped, not hardware-validated — `internal/detect/fault.go` (8 AMD rows; coverage pinned by `fault_coverage_test.go`) | seam only (no implementation) |
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

| Capability | Status | Notes |
|---|---|---|
| Cordon / drain | shipped & hardware-validated | `internal/platform/kubernetes/kubernetes.go`. Both executed for real on the EKS node in the confined destructive phase (`Cordon → Drain → ReplaceNode`, `hack/hw-e2e.sh`). Vendor-blind: eviction is a pod operation. The PDB-blocked retry loop is fixture-tested only — no hardware run has had an eviction refused by a budget. |
| Uncordon | shipped, not hardware-validated | Same file. Every hardware run either ended in a dry-run ladder or terminated the node, so the real uncordon (and the cordon janitor's recovery path) has not executed on hardware. |
| Node reboot | shipped, not hardware-validated | `internal/agent/executor/executor.go`. See the note below — this cell surprises people. |
| Cloud node replace (terminate for the autoscaler) | shipped & hardware-validated (AWS only) | `internal/cloud/aws/`, `internal/platform/kubernetes/recycle.go`. A real EC2 instance was terminated in the `kubeneuron-e2e10` run. Every non-AWS provider is seam only — `internal/cloud/providers/providers.go` registers exactly one. |
| Cloud node recycle (stop/start in place) | shipped, not hardware-validated (AWS only) | Same files. The AWS provider refuses autoscaling-group members (`cloud.ErrRecycleNotViable`), and every hardware node so far has been an ASG member, so the successful path has never run. |

## Evidence, per shipped cell

**Kernel-log detection — hardware-validated.** `internal/agent/kmsg/watcher.go`
parses `NVRM: Xid (PCI:…): NN,` lines. Validated on EKS `g4dn.xlarge` / Tesla T4
(`kubeneuron-e2e10`): XID 79 drove the full ladder and XID 92 the sub-threshold
hold. *Scope limit:* the XIDs were **injected into `/dev/kmsg`**
(`hack/hw-e2e.sh inject_xid`), not produced by a failing GPU. The kernel-log
transport, cursor, dedup and classification are hardware-proven; the claim "we
catch a real GPU dying" rests on the XID text format, not on an observed
failure.

**Polled telemetry — not hardware-validated.** `internal/agent/gpuhealth` polls
`dcgmi dmon -e 230` and falls back to `nvidia-smi -q`. Both parsers are driven
entirely by hand-written fixtures (`internal/agent/gpuhealth/testdata/`) — the
`nvidia-smi -q` fixture describes an A100 nobody has run. `dcgmi` is absent from
the stock EKS NVIDIA AMI, so the DCGM path degraded to observed-only in every
hardware run. `hack/hw-e2e.sh test-dcgm` exists for exactly this and has never
executed.

**Metric-alert detection — not hardware-validated.** `configs/vmalert/gpu-rules.yaml`
is written against `DCGM_FI_*` series and `internal/detect/alertmanager.go` maps
rule names to problem classes; a unit test cross-checks rules against the map.
No hardware run has fired an alert through vmalert into the webhook.

**XID catalog — hardware-validated.** `internal/detect/xid.go` classified
kernel-log XIDs 79 and 92 read off a live node into `ClassFellOffBus` and
`ClassECCSBERate` during the EKS runs, including the latter's 3-in-24h
threshold hold.

**Neutral fault catalog — not hardware-validated.** `internal/detect/fault.go`
holds exactly two rows, both `nvidia`: `ecc-dbe` and `row-remap-failure`. They
are emitted only by the `nvidia-smi -q` fallback, which has never run on
hardware. `GPUSignalMapping.spec.faults[]` can add `{vendor: amd, code: …}` rows
declaratively today (`internal/detect/catalog.go`, covered by
`catalog_test.go`) — **classification is genuinely vendor-neutral; production is
not**, because no source emits a non-NVIDIA fault.

**Accelerator inventory — hardware-validated.** `nvidia-smi --query-gpu=index,uuid,name,pci.bus_id`,
`driver_version` and `mig.mode.current` parsed real Tesla T4 output (driver
580.159.03) — the captured output is checked in at
`internal/agent/nvml/testdata/t4-g4dn-580.159.03.txt`. `Init`, `ListGPUs` and
`Healthy` also ran against a live L4 host.

**Kubernetes GPU inventory — hardware-validated; targeted eviction — not.**
`internal/platform/kubernetes/kubernetes.go` hardcodes
`gpuResource = "nvidia.com/gpu"` for both the node's GPU count and
`podUsesGPU`. The capacity read was validated on EKS (the node advertised
`nvidia.com/gpu=1` and appeared with its GPU in the fleet inventory).
`evict_gpu_workload` has never executed on hardware — every hardware playbook
used `Cordon → Drain`. See the silent-no-op warning below.

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

## Two silent no-ops (read before claiming multi-vendor)

Most missing vendor support fails loudly — no driver, no detection source, no
events. Two places fail **quietly**, which is worse:

1. **Node GPU count.** `nodeFromK8s` reads `nvidia.com/gpu` capacity. An AMD
   node (`amd.com/gpu`) or Intel node (`gpu.intel.com/i915`) is inventoried with
   **zero GPUs** and looks like a healthy CPU node.
2. **Targeted GPU-pod eviction.** `platform.evict_gpu_workload` selects pods by
   the same `nvidia.com/gpu` limit. On a non-NVIDIA node — and, as
   [the MIG decision](mig-decision.md) records, on a MIG node running the device
   plugin's *mixed* strategy, where pods request `nvidia.com/mig-1g.5gb` — it
   evicts nothing and **reports success**. The workload-protection rung silently
   protects nothing.

Fixing both is one change: the GPU resource name must become a set, not a
constant. It is a prerequisite for §1.1/§1.3 of the
[definition plan](definition-plan.md), not a follow-up.

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
| `verifyRuntimeEvidence` requires an NVIDIA accelerator report for every GPU-class target | no non-NVIDIA incident can execute a gated step |
| arming requires the concrete `*nvml.SMI` driver | no non-NVIDIA node can be armed for destructive execution |
| the physical-reset gate is `CapabilityNVIDIAReset` / `allowNVIDIAReset` only | per-device reset is structurally NVIDIA-only, not merely unimplemented |
| kmsg parsing is NVRM/XID-only; the quiesce stack manipulates NVIDIA components | detection and reset-prerequisite rungs are NVIDIA-only |
| `gpuResource = "nvidia.com/gpu"` | the two silent no-ops above |

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
