# Capability matrix — what ships, per vendor

This page answers one question honestly: **is KubeNeuron really
vendor-neutral?** The product sentence says "a vendor-neutral GPU fleet
reliability control plane". That is a statement about construction, not about
shipped support, and the difference is exactly what this table records.

Every cell below was derived by reading the code, not the roadmap. A cell that
says *shipped* names the file that backs it. Where the code is weaker than a
reader would assume, the note says so.

## Agent signals with node-level actions disabled

| Source | What the agent can observe |
|---|---|
| NVIDIA DCGM hostengine | Field 230 reports the most recent numeric XID. Repeated identical XIDs between polls may be missed, so an `xid-app` threshold may never be reached. The shipped exporter profile omits field 230. Without a readable kernel log or a reachable DCGM hostengine, numeric XIDs are unavailable. A CPU-only test uses a DCGM fixture to verify that an XID reaches the controller and opens an incident. |
| NVIDIA SMI | When available, successive samples can show a rise in volatile uncorrectable ECC errors or a transition to row-remap failure. Neither reading supplies a numeric XID. |
| AMD kernel log | Without kernel log access, kernel ring timeouts (`driver-hang`) and kernel PCIe errors (`pcie`) are unavailable. The agent reads the kernel log from the node's journal where it can; see [Kernel log from the journal](#kernel-log-from-the-journal). |
| AMD SMI | ECC and page retirement remain observable when the SMI tooling and devices are accessible. |

## Kernel log from the journal

With node-level actions disabled, the agent reads kernel entries from one
read-only journal directory per node (experimental). The controller
chooses the directory from the node's OS image with the table in
`internal/controller/journal_os_images.go`; an override in
`spec.agent.kernelLog.journal.nodeOverrides` comes first. OS names
describe the node images the table matches, nothing more. Sources were
reviewed on 2026-11-19.

| Node image | Directory | Status | Source |
|---|---|---|---|
| Amazon Linux 2023 (EKS) | `/var/log/journal` | journal files readable by an unprivileged Pod, checked on a live node (AL2023.12.20261112, EKS v1.34.12); kernel record to incident through the agent not yet checked | [AL2023 journald](https://docs.aws.amazon.com/linux/al2023/ug/journald.html) |
| Bottlerocket (EKS) | `/var/log/journal` | from documentation, not checked | [Bottlerocket restricted filesystem](https://bottlerocket.dev/en/os/1.50.x/concepts/restricted-filesystem/) |
| Container-Optimized OS (GKE) | `/var/log/journal` | from documentation, not checked | [GKE node images](https://docs.cloud.google.com/kubernetes-engine/docs/concepts/node-images) |
| Ubuntu (GKE, AKS) | `/var/log/journal` | from documentation, not checked | [GKE node images](https://docs.cloud.google.com/kubernetes-engine/docs/concepts/node-images), [AKS kubelet logs](https://learn.microsoft.com/en-us/azure/aks/kubelet-logs) |
| Azure Linux (AKS) | `/var/log/journal` | from documentation, not checked | [AKS kubelet logs](https://learn.microsoft.com/en-us/azure/aks/kubelet-logs) |
| kind node image | `/run/log/journal` | checked on kind (Debian-based node image): the label, the mount and the move between agent DaemonSets (`hack/kind-journal.sh`) | [kind node image](https://kind.sigs.k8s.io/docs/design/node-image/) |

Another image keeps the agent without a journal mount, and its kernel log
source reads `os-image-not-recognized`. If the agent cannot read the
mounted journal, for example because of file permissions or a security
module, the source reads `journal-unreadable`; KubeNeuron adds no
privilege for it.

The agent skips what it cannot read in the journal and reads on:

- an entry it cannot decode or that is too large to read in one cycle;
- an archived journal file it cannot read or that was removed before it
  was read;
- the oldest journal files, beyond the newest 128;
- the entries between a saved read position it cannot use and the end of
  the journal, where it starts again.

Any kernel fault in what it skipped is lost and opens no incident. The
source reads `journal-unreadable` from the skip until ten minutes after
the last one, and `kubeneuron_agent_journal_unreadable_entries_total`
counts the skips. The agent does not store or log the content of what it
skipped.

## Live cloud evidence

On Amazon EKS, in a node pool with real NVIDIA Tesla T4 GPUs, KubeNeuron replaced a node twice under an Automatic policy, with no manual approvals: add-first, then remove-first on the node that had replaced the first. Each fault was injected as an NVIDIA XID 79 line in the node's kernel log; the GPU itself was healthy. KubeNeuron verified each new node by readiness, advertised capacity and the device inventory its agent reported, and the driver answered; device health was not checked. Separately from KubeNeuron, the run checked the GPU on each new node with nvidia-smi (Tesla T4, driver 580.178.04) and the CUDA vectorAdd sample, which passed. This was done twice, on 25 November 2026, on images built from commit a398c51666e9cac28686c65eeb6c7c9260c7a0e0 — the code released as v1.0.0-rc.2 (the commits after it change only the changelog, the documentation and the navigation of the documentation site). In each run, two more incidents opened. In the second run, where they were recorded in full, both were agent-down observation incidents for the nodes that KubeNeuron was removing, raised by the KubeNeuronAgentDown alert after the node's agent stopped reporting: each sent an opened and a resolved notification, started no action, and was closed when the node was gone.

The kind tests run on CPU-only Kubernetes nodes without GPUs or cloud API calls.
Each live-cloud entry applies only to the path and run shown.

| Path | Unit tests or simulated provider | kind (CPU only) | Live cloud |
|---|---|---|---|
| EKS and AKS add-first across zones of a fixed-size pool with `AnyPoolZone` | Simulated provider only | No | No |
| EKS add-first on a fixed-size single-zone managed node group | Simulated EKS and Auto Scaling APIs, sequence from a live run | No | Yes, once, on pre-release builds: EKS add-first replacement completed on a live cluster (CPU nodes, synthetic accelerator capacity; verification depth: node readiness and advertised capacity). The controller was updated during the incident. Not repeated on v1.0.0-rc.1. The live run used a single-zone group; add-first across zones (AnyPoolZone) was tested with simulated providers only. |
| EKS managed nodegroup replacement (remove-first) | AWS provider and contract tests | Controller approval, cordon and drain only | Yes, once, on a pre-release build: EKS remove-first replacement completed on a live cluster (CPU nodes, synthetic accelerator capacity; verification depth: node readiness and advertised capacity). Not repeated on v1.0.0-rc.1. On 2026-08-05, the v0.2.1 run (a 0.x release, no longer published) on EKS `g4dn.xlarge` / Tesla T4 reached EC2 termination after an injected fault. Replacement readiness and resumed training were not verified. |
| AKS add-first on a fixed-size single-zone node pool | Simulated AKS API and provider contract tests | No | Yes, twice. Once on a pre-release build, and once on 25 November 2026, on images built from commit a398c516 — the code released as v1.0.0-rc.2 (the commits after it change only the changelog, the documentation and the navigation of the documentation site): AKS add-first replacement completed on a live cluster (CPU nodes, synthetic accelerator capacity; in the second run under an Automatic policy, without manual approvals, on AKS 1.36, Ubuntu 24.04, Standard_D2s_v3 nodes in a fixed-size single-zone user node pool; verification depth: node readiness and advertised capacity; no GPU was present and device health on the new node was not checked). |
| AKS remove-first replacement on a fixed-size node pool | Simulated AKS API and provider contract tests | No | Yes, twice. Once on a pre-release build, and once on 25 November 2026, on images built from commit a398c516 — the code released as v1.0.0-rc.2 (the commits after it change only the changelog, the documentation and the navigation of the documentation site): AKS remove-first replacement on a fixed-size node pool completed on a live cluster (CPU nodes, synthetic accelerator capacity; in the second run under an Automatic policy, without manual approvals, and, as documented for a fixed-size pool, the pool grew by one node before the machine was deleted; verification depth: node readiness and advertised capacity; no GPU was present and device health was not checked). |
| AKS remove-first on autoscaled node pools; restart; refusal of configured multi-zone pools before growth | Simulated AKS API and provider contract tests | No | No |
| GKE Standard add-first on a fixed-size single-zone node pool | Simulated GKE and Compute Engine APIs | No | Yes, twice. Once on a pre-release build, and once on 25 November 2026, on images built from commit a398c516 — the code released as v1.0.0-rc.2 (the commits after it change only the changelog, the documentation and the navigation of the documentation site): GKE Standard add-first replacement completed on a live cluster (fixed-size single-zone node pool; in the second run under an Automatic policy, without manual approvals, on GKE 1.35, Container-Optimized OS, e2-standard-2 CPU nodes, synthetic `nvidia.com/gpu` capacity; verification depth: node readiness and advertised capacity; no GPU was present and device health on the new node was not checked). |
| GKE Standard remove-first on a fixed-size pool, with a recorded size restore | Simulated GKE and Compute Engine APIs | No | Yes, twice. Once on a pre-release build, and once on 25 November 2026, on images built from commit a398c516 — the code released as v1.0.0-rc.2 (the commits after it change only the changelog, the documentation and the navigation of the documentation site): GKE Standard remove-first replacement on a fixed-size pool completed on a live cluster, with the pool's size restored (in the second run under an Automatic policy, without manual approvals; CPU nodes, synthetic accelerator capacity; verification depth: node readiness and advertised capacity; no GPU was present and device health was not checked). |
| GKE Standard remove-first on autoscaled pools; fixed-size pools of several equal zones | Simulated GKE and Compute Engine APIs. Autopilot is observation-only. | No | No |
| Incident resolves after a cloud replacement (add-first and remove-first) | Simulated provider | No | Yes, once per order, on pre-release builds, on EKS, AKS and GKE Standard: the incident resolved by itself after the replacement in the runs listed above (CPU nodes, synthetic accelerator capacity; verification depth: node readiness and advertised capacity). Not repeated on v1.0.0-rc.1. Repeated for both orders on GKE Standard and AKS on images built from commit a398c516 (25 November 2026), and on the EKS Tesla T4 nodes described above. |
| DryRun and Approval modes | Controller policy tests | DryRun leaves pods unchanged. Approval exercises drain after approval. | No for the current modes |
| Automatic mode | Controller policy tests | No dedicated Automatic execution scenario | No |
| Managed monitoring (Collector, metrics store, Grafana and alert delivery to controller) | Operator and rule tests | Synthetic DCGM input through the Managed stack | No for the current stack |
| NVIDIA fault detection | Agent parser fixtures | Synthetic DCGM input exercises metric alerts | Yes for injected kernel XIDs: 2026-08-05, v0.2.1, EKS `g4dn.xlarge` / Tesla T4 (`kubeneuron-e2e10`). |
| AMD fault detection | Synthetic amdgpu kernel logs and AMD SMI / ROCm SMI polling fixtures | No | No |
| Node conditions in incident context | Controller and API tests | Installer suite checks conditions in incident detail and audit | No |
| AWS interruption queue (EventBridge to SQS: spot warning, rebalance, AWS Health scheduled change, instance state change) | Event parser on the AWS documentation examples (2026-11-18), simulated SQS queue, provider contract and controller tests | No | No |
| Accelerator health from Kubernetes nodes ([profiles](accelerator-health.md)) | Profile and controller tests with fake nodes | No | No. Experimental: signal names from vendor documentation, not validated on hardware |
| TPU health from Google Cloud Monitoring (`infra_health`, Google Cloud Beta; opt-in, [how it is read](accelerator-health.md#tpu-health-from-google-cloud-monitoring-opt-in)) | State table, fake Cloud Monitoring API, REST client against a local test server, controller tests with fake nodes | No | No. Experimental, not validated against a live GKE cluster; whether Google writes the metric for GKE nodes is not confirmed |
| Device health from Kubernetes DRA device taints (`resource.k8s.io/v1` ResourceSlices, Kubernetes beta in 1.36; opt-in, [how it is read](accelerator-health.md#device-taints-from-kubernetes-dra-opt-in)) | Profile table, fake dynamic client (served, not served, forbidden, no taints), controller tests with fake devices; read against a Kubernetes 1.36.4 API server in kind with a hand-written ResourceSlice (`hack/kind-dra-health.sh`) | No | No. Experimental: taint keys from the NVIDIA and AWS Neuron driver documentation, not run against a driver on hardware; Kubernetes and the drivers may change them |
| AWS Neuron device health from the driver's sysfs on the node (`aws-neuron-sysfs`, incidents opt-in, [how it is read](accelerator-health.md#aws-neuron-device-health-from-the-drivers-sysfs-incidents-opt-in)) | Package tests on synthetic sysfs trees (`internal/agent/neuronsysfs`): counters, component list, unreadable devices, baselines across restarts and boots; agent tests on synthetic trees (two reads for a component, counter rise, report of vendor `aws`) and through the controller's agent routes to the stored report and the event archive | No | No. Experimental, not validated against a live AWS Trainium or Inferentia node; file paths and meanings are taken from AWS documentation (reviewed 2026-11-18) |
| AWS Neuron device health from Amazon CloudWatch, read off the node | Not built | No | Not built. We found no per-device health signal that AWS publishes without an agent on the node, as of 2026-11-18: [EC2 status checks](https://docs.aws.amazon.com/AWSEC2/latest/APIReference/API_PerformanceStatusSummary.html) and [AWS Health](https://docs.aws.amazon.com/health/latest/ug/aws-health-concepts-and-terms.html) name an instance, not a device, and the [EC2 accelerator metrics](https://docs.aws.amazon.com/AWSEC2/latest/UserGuide/viewing_metrics_with_cloudwatch.html) describe NVIDIA GPUs only. The per-device ECC metric of [Container Insights](https://docs.aws.amazon.com/AmazonCloudWatch/latest/monitoring/Container-Insights-metrics-enhanced-EKS.html) and the `NeuronHealthCheck` namespace of the [Neuron node problem detector](https://awsdocs-neuron.readthedocs-hosted.com/en/latest/deploy/infrastructure/problem-detector.html) come from agents the user runs on the node; the detector's node condition is already read ([profiles](accelerator-health.md#profiles)) |
| Node conditions read and not acted on (AKS `XIDError`, `GPUECCError`, `GPUClockThrottling`, `UnhealthyNvidiaDevicePlugin`, `UnhealthyNvidiaDCGMServices`; EKS DCGM reasons) | Profile and controller tests with fake nodes | No | No. Counted as `ignored`, never an incident: none of them says enough to blame the node ([why, per condition](accelerator-health.md#read-not-acted-on)) |
| Training reports | Authentication, ownership, storage and recovery timing tests | Two CPU processes in one Job pod train a synthetic linear model. The second loads the first process's checkpoint and reduces the loss. Both report over HTTPS with a projected pod token, and the test verifies the stored checkpoint, resume and progress reports | No live cloud or GPU training run. |

The 2026-10-18 hardware harness used v0.7.0 release source on EKS
`g4dn.xlarge` / Tesla T4. Its observability phases predate the current Managed
stack, and the run did not exercise current EKS managed-nodegroup replacement.

## How to read a cell

| Status | Means |
|---|---|
| **shipped & hardware-validated** | The code path exists, is tested, **and has executed against real accelerator hardware of that vendor** with recorded evidence (a hardware run in `CHANGELOG.md`, or a captured fixture). |
| **shipped, not hardware-validated** | The code path exists and is exercised by unit tests against fixtures. It has never run against that vendor's hardware. Fixtures are written by humans; they prove the parser, not the world. |
| **shipped, hardware evidence limited** | The code path exists, is tested, **and a stated part of it has executed against real hardware** in a run recorded in `CHANGELOG.md`, but the recorded assertions cover only a subset of what the row claims. The cell's note names the run's source commit, the GPU, exactly what was observed, and what was not. This is not *hardware-validated*: everything the note does not list keeps the standing of the row above. |
| **seam only (no implementation)** | The interface, type, registry entry, or config surface accepts the vendor, and nothing implements it. An operator enabling it gets nothing — see the *silent no-op* warnings below for the two places where "nothing" is worse than an error. |
| **not applicable** | The capability is meaningless for that vendor by construction (an NVIDIA-native encoding for a non-NVIDIA device). |

Two rules govern edits to this page:

1. A cell may be promoted to *hardware-validated* only against a run recorded
   in the repository — `CHANGELOG.md`, a
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
| Polled telemetry fault detection (DCGM / vendor SMI) | shipped & hardware-validated — `internal/agent/gpuhealth/gpuhealth.go` | code present, not enabled in an operator-managed installation, not hardware-validated — `internal/agent/amdhealth/amdhealth.go` (the operator does not set the agent's `--amd-detection` flag; fixtures are synthetic: the amd-smi field names follow the amd-smi CLI source, not a capture). From amd-smi the agent reads the device list (PCI address, UUID), ECC error counts and hotspot temperature. XGMI link errors, retired pages and thermal throttling are not read from amd-smi in this release. Not validated on AMD hardware. | seam only (no implementation) |
| Metric-alert fault detection (vmalert → Alertmanager, or the managed vmalert → controller) | shipped, not hardware-validated — `configs/vmalert/gpu-rules.yaml`, `configs/vmalert/gpu-alert-rules.yaml`, `internal/detect/alertmanager.go` | seam only (no implementation) | seam only (no implementation) |
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

These actions use platform APIs. Experimental AMD detection reads ring
timeouts, uncorrectable ECC errors, page retirement and PCIe errors from
the amdgpu kernel log. The agent also reads health data from available
AMD SMI tools. Synthetic tests cover both paths. AMD hardware validation
is still outstanding. Intel requires a manually opened incident or a
configured supported alert source.

A fresh installation uses the cloud policies in `config/cloud-policies`.
Node-level policies in `config/policies` require the installer option
`--policy-pack bare-metal-alpha` (Alpha, not validated on real hardware).
The table below records the evidence for each action.

| Capability | Status | Notes |
|---|---|---|
| Cordon / drain | shipped & hardware-validated | `internal/platform/kubernetes/kubernetes.go`. Both executed for real on the EKS node in the confined destructive phase (`Cordon → Drain → ReplaceNode`, `hack/hw-e2e.sh`). Vendor-blind: eviction is a pod operation. The PDB-blocked retry loop is fixture-tested only — no hardware run has had an eviction refused by a budget. |
| Uncordon | shipped, not hardware-validated | Same file. Every hardware run either ended in a dry-run ladder or terminated the node, so the real uncordon (and the cordon janitor's recovery path) has not executed on hardware. |
| Node reboot | shipped, not hardware-validated | `internal/agent/executor/executor.go`. See the note below — this cell surprises people. |
| Cloud node replacement | See [live cloud evidence](#live-cloud-evidence) for provider-specific test coverage. | Historical and current paths are listed separately there. |
| Cloud node recycle (stop/start in place) | shipped, not hardware-validated (AWS only) | Same files. The AWS provider refuses autoscaling-group members (`cloud.ErrRecycleNotViable`), and every hardware node so far has been an ASG member, so the successful path has never run. |

## Table 3 — GPU reliability observability (NVIDIA / DCGM only)

The hardware runs in this table were recorded on 0.x builds (dates are given per row). 0.x releases are no longer published. The runs have not been repeated on v1.0.0-rc.1.

The GPU metric catalog describes a read-only observability layer beside the detection and
remediation paths above: a metric catalog, a wider dcgm-exporter profile, a
scrape label contract, recording rules, five Grafana boards and shadow-only
alerts. Every row is NVIDIA-specific by construction (the inputs are DCGM
field ids through dcgm-exporter). Exactly one hardware run is recorded for
this layer: the final G3 run of 2026-10-18 on an ephemeral EKS g4dn.xlarge /
Tesla T4 node from v0.7.0 release source, whose read-only `test-observability` phase makes a deliberately
short list of assertions. Four rows below are therefore **shipped, hardware
evidence limited**, and their notes name exactly what that phase observed;
the other three stay **shipped, not hardware-validated**, because the phase
does not look at Grafana, fires no shadow alert, and never computed a
fail-slow score for a real peer group. No row is *hardware-validated*. The
The catalog maps NVIDIA/DCGM sources only, and mapping
any other vendor's exporter is a later phase. The layer adds no action, no rung,
no controller or agent code path, no CRD field, no store migration and no
controller RBAC; a cell here is a claim about what an operator can *see*,
never about what KubeNeuron *does*.

| Capability | Status | Notes |
|---|---|---|
| Metric catalog (`kubeneuron_gpu_*` names, units, DCGM field ids, support cells) | shipped, hardware evidence limited (Tesla T4) | `internal/gpucatalog`, rendered to `config/gpu-metrics/catalog.yaml` and `docs/reference-gpu-metrics.md`. The catalog's own support cells are unchanged: the only `validated` cell is still the read of field 230 on a Tesla T4 from the v0.6.0-era hardware run; every other cell is `declared`, `not-applicable` or `unknown`. Hardware evidence (final G3 run, 2026-10-18, v0.7.0 release source): the real dcgm-exporter on a Tesla T4 served `DCGM_FI_DEV_POWER_USAGE` (tier T0), `DCGM_FI_DEV_ECC_SBE_VOL_TOTAL` (tier T1) and `DCGM_FI_DEV_GPU_TEMP`, its raw `DCGM_FI_DEV_FB_USED` sample was read back for the MiB-to-bytes check, and three canonical names — `kubeneuron_gpu_temperature_celsius`, `kubeneuron_gpu_power_watts` and `kubeneuron_gpu_memory_used_bytes` — were recorded from them with the catalog's units. No other catalog field or canonical name has been observed on any GPU by this project. |
| dcgm-exporter reliability profile (tier T0 + T1 fields) | shipped, hardware evidence limited (Tesla T4) | `deploy/kubernetes/dependencies/gpu-operator/values.yaml`, pinned to the catalog in both directions by `internal/gpucatalog`. Hardware evidence (same run): the profile ConfigMap reached an EKS cluster through the pinned GPU Operator chart v26.3.3; the dcgm-exporter DaemonSet rolled out on a Tesla T4 node beside DCGM client (`dcgmi`) 4.3.1 and host engine 4.5.2 (the deployed exporter is the checked-in source pin `4.5.3-4.8.2-distroless`; no image digest was captured); and the exporter served `DCGM_FI_DEV_POWER_USAGE`, `DCGM_FI_DEV_ECC_SBE_VOL_TOTAL` and `DCGM_FI_DEV_FB_USED` — three of the fields v0.7 adds — plus `DCGM_FI_DEV_GPU_TEMP` from the v0.6.1 list. The remaining added fields are listed from NVIDIA documentation and have not been observed on any GPU by this project; the fields a Tesla T4 cannot serve (row remap, NVLink, MIG) were checked absent from the recorded series, not zero. Reversible by the documented rollback. |
| Scrape label contract (`gpu_uuid`, `gpu_model`, optional `node_pool`, `mig_profile`, admin-set `cluster`; no `driver`) | shipped, hardware evidence limited (Tesla T4 / EKS) | `deploy/kubernetes/dependencies/observability/vmagent.yaml`, relabel rules evaluated by `internal/depprofile`. Needs read-only `nodes` access that the VictoriaMetrics operator's generated ClusterRole grants. Hardware evidence (same run): on the three canonical series named in the rows above, `node`, `gpu_uuid` and `gpu_model` were present and equal to the real node's name and the exporter's own `UUID` and `modelName`, and `node_pool` equalled the EKS nodegroup label read from that node. Not observed: `node_pool` on GKE or Karpenter nodes (still derived from documented labels), the admin-set `cluster` label, and `mig_profile` beyond its expected absence on a GPU without MIG. |
| Canonical and derived recording rules | shipped, hardware evidence limited (Tesla T4, three canonical rules) | `configs/vmalert/gpu-recording-rules.yaml`, mirrored in the deployed VMRule; catalog ⇄ rules pinned in both directions by `internal/gpucatalog`. No formula synthesises a sample: a missing input is a missing series. `promtool check rules` (Prometheus v3.16.0) and `vmalert -dryRun` (v1.147.0) pass on the rule files and on the dashboard queries, and one set of rule unit tests passes in both promtool and vmalert-tool (`make check-rules`, in CI; a check of the files, not a run against a live Prometheus). Hardware evidence (same run): the deployed VMRule, evaluated by a live vmalert against a live VMSingle, recorded `kubeneuron_gpu_temperature_celsius`, `kubeneuron_gpu_power_watts` and `kubeneuron_gpu_memory_used_bytes` from the real exporter's samples for one GPU; `kubeneuron_gpu_memory_used_bytes` equalled `DCGM_FI_DEV_FB_USED` × 2^20 at the same instant; and the row-remap, NVLink and fail-slow rules recorded no sample for a GPU with no such input (the missing-input rule held on hardware for those three). Every other canonical rule and every derived rule were not asserted on hardware. The hardware run predates the rewrite of the derived rules to plain PromQL; the rewritten expressions were checked by the rule unit tests only. |
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
on it even when the gates run; nothing here is evidence for A100/H100.

## Table 4 — Workload-safe recovery (hardware evidence limited to one Tesla T4)

The hardware runs in this table were recorded on 0.x builds (dates are given per row). 0.x releases are no longer published. The runs have not been repeated on v1.0.0-rc.1.

The CPU-only evidence behind every cell is the same: unit tests, the offline
kind gate (`make test-integration-kind`, which drives the advisory, the
preview, a group peer on a second worker and a real drain on bare Pods) and
the examples fixture (`make test-workload-safe-kind`, which drives the three
`examples/checkpoint/*` toys through a real installation with the Kueue and
KubeRay versions pinned in `deploy/kubernetes/dependencies/versions.lock.yaml`).
The rows marked *hardware evidence limited (Tesla T4)* additionally ran once
on real hardware: the G3 run of 2026-11-05 on an ephemeral EKS cluster with
one `g4dn.xlarge` (Tesla T4, driver 580.178.04, DCGM client 4.3.1, GPU
Operator v26.3.3), through `hack/hw-e2e.sh test-impact`, `test-checkpoint`
and `test-timeslice`, each cell quoting the phase that observed it. That
hardware is one GPU on one virtualized node: the agent degrades its report
there because the hypervisor withholds the PCI reset, so every
classification it produced carried `device_actions: refused` with
`ReportNotReady`, and the rows that name MIG, DRA, Kueue, KubeRay or a
multi-node gang cannot be promoted on the hardware this project can rent. The release adds no
remediation action, no device-scoped action on partitioned or shared GPUs,
no write to Kueue, Ray or DRA objects, and no network path from the
controller to any workload; the [design record](workload-safe-recovery-design.md)
carries the argument.

| Capability | Status | Notes |
| --- | --- | --- |
| Device topology classification (`whole-device`, `mig`, `time-sliced`, `dra-managed`, `mixed`, `unknown`; fail-closed) | shipped, hardware evidence limited (Tesla T4: `whole-device` and `time-sliced`) | `internal/topology`, read from node status, GPU Feature Discovery labels and, under the DRA opt-in alone, read-only `resourceslices`. Anything but `whole-device` renders `device_actions: refused`; the classifier only narrows, and the action registry gains no device-scoped action (pinned by the bijection test). Evidence: unit tables, and eight synthetic node forms the kind gate places on a node and removes (`hack/kind-integration.sh`). On hardware (G3 2026-11-05, `test-timeslice`): the untouched Tesla T4 read `whole-device` / `per-pod` with device actions `refused` (`ReportNotReady`, the agent's own PCI-reset finding), the GPU Operator device plugin switched to time-slicing `replicas: 2` read `time-sliced` / `refused` / `device-wide` with the `TimeSliced` reason from the unrenamed `nvidia.com/gpu: 2` advertisement and the GFD labels, and the revert read `whole-device` again; `test-impact` and `test-checkpoint` read the same `whole-device` document inside the preview. Never `eligible` on this hardware, and MIG and DRA exist here only as refusals rendered from fixtures, since the Tesla T4 cannot partition and the rented cluster runs no DRA driver. The first hardware read found the classifier collapsing a degraded-but-verified report to `unknown`; the correction (layout and readiness kept apart) is on the candidate. |
| Checkpoint advisory at approval park | shipped, hardware evidence limited (Tesla T4; one toy trainer) | `internal/checkpoint/advisory.go`, `internal/controller/advisory.go`. Closed `kubeneuron.io/checkpoint-advisory-*` key set, round-bound, withdrawn best effort when the round ends without its disruption, nothing patched in `DryRun`, bounded by its own budget (default 10s, ceiling 60s). Evidence: unit tests; the kind gate sees the advisory on opted-in Pods only, the withdrawal after a rejection, and the dry-run audit row; the examples fixture sees the toy trainers checkpoint on it. On hardware (G3 2026-11-05): `test-impact` saw the advisory stamped on the one opted-in Pod holding the Tesla T4 and withdrawn after the rejection with the opt-in untouched; `test-checkpoint` saw the sidecar read it off the Downward API and the trainer checkpoint 3–4 s after the stamp was observed on the object (the first measurement of that latency outside kind). One Pod, one node, a busybox toy: not a training framework. |
| Checkpoint groups (gang peers, `crossNode` opt-in, `maxGroupSize`) | shipped, not hardware-validated | `internal/checkpoint/group.go`, `internal/platform/kubernetes/group.go`, `internal/controller/group.go`. One paged, read-only Pod list per group, capped; peers get a deadline from the Phase 1 formula and no veto. Evidence: unit tests; the kind gate's second worker receives the peer request, acknowledges, and keeps its Pod and its node untouched. On hardware the toy carried a group of one member (`checkpoint-group-request … 1 member(s) … requesting 0 peer(s)` in the G3 2026-11-05 audit), which exercises the group path and proves nothing about peers; the hardware this project rents has one GPU node, so a cross-node gang on GPUs has never been observed. |
| Impact preview and digest-bound approve | shipped, hardware evidence limited (Tesla T4; one GPU holder) | `internal/impact`, `internal/controller/impact.go`, store migrations SQLite 0024 / PostgreSQL 0015, `GET …/impact-preview`, `kubeneuronctl incidents impact`, the panel drawer. Counts and contexts, never an estimate of saved or lost training. `requireForApproval` refuses an approve without the current round's digest (409). Evidence: unit tests on both stores; the kind gate's 409 without and with a stale digest, 204 with the current one, and the digest on the incident detail. On hardware (G3 2026-11-05): `test-impact` read a round-1 preview for the real node with the Tesla T4 as `whole-device` / `refused` (`DeviceActionsRefused` warning), one GPU holder eligible for checkpoint with its `Job` owner and group, a 409 without and with a stale digest, and the digest on the incident detail; `test-checkpoint` approved through `kubeneuronctl approve --round 1` (which at `38046bd` fetched the preview and bound its digest itself; the CLI now refuses to approve without `--impact-digest` when a preview is computed, and the phase asserts that refusal and the digest on the decision row), and the controller then requested, saw the Pod exit after 6 s and drained the node. |
| Incident job map | Controller RBAC | By default the controller may `get` individual Jobs, JobSets, RayClusters and RayJobs named in the controller owner chain of an affected pod. Missing or inaccessible owners leave the job status unknown. |
| Read-only workload context (`batchJobs`, `kueue`, `kuberay`) | shipped, not hardware-validated | `internal/platform/kubernetes/context.go`, dynamic clients, `get,list,watch` alone on `batch/jobs`, `kueue.x-k8s.io/{workloads,localqueues,clusterqueues}` and `ray.io/{rayclusters,rayjobs}`, granted only while the matching opt-in is set (pinned by `internal/operator` tests). Read-only context from Kueue and KubeRay, demonstrated in kind, not validated on a production fleet: the examples fixture installs Kueue v0.20.4 and the KubeRay operator chart 1.7.1 from pinned archives, sees `context.kueue` and `context.ray` on the toy Pods, and checks that no Kueue or Ray object gained a KubeNeuron field manager. That is a demonstration of the annotation protocol, not evidence that any framework integrates with KubeNeuron. On hardware only `batchJobs` ran (G3 2026-11-05, `test-impact`: the GPU holder's `owner_chain` named its `Job`); Kueue and KubeRay were not installed on the rented cluster. |
| One eligibility function (`drain-excluded`, `not-gpu-holder`, `group-invalid`, `group-too-large`, `budget-exhausted`, `platform-unsupported`, beside the Phase 1 codes) | shipped, hardware evidence limited (Tesla T4; the eligible branch only) | `internal/checkpoint/eligibility.go`, shared by the advisory, the groups, the preview and the request. Evidence: unit tables; the kind gate's bystander Pod stays `not-opted-in` in the preview and is never stamped. On hardware (G3 2026-11-05, `test-impact`) the preview counted one GPU holder among 30-odd workloads on the node and exactly one eligible for checkpoint, the opted-in toy; the ineligibility codes were not exercised there. |
| Checkpoint examples (`examples/checkpoint/{plain-job,kueue-job,kuberay-ray-train}`) | shipped, not hardware-validated — examples, not product code | CPU-only toys that read the advisory and the request off their own Pod through the Downward API and checkpoint a step counter; the Job toys acknowledge by leaving, the Ray toy by writing the state key with its own namespaced Role. Evidence: `hack/kind-workload-safe.sh --verify` runs the trainer and sidecar locally and fails its own assertions on 28 deliberate defects; the cluster run drives all three through advisory, preview, request and drain. Not a hardware result and not a statement about any training framework. |

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
With the Managed store, `configs/vmalert/gpu-alert-rules.yaml` (thermal, power,
remapped-row budget, PCIe replays, exporter down, agent down) goes from the
managed vmalert straight to the controller's `/api/v1/webhooks/vmalert`; the
rules were evaluated by vmalert v1.147.0 against VictoriaMetrics fed over OTLP
from a fake exporter, and a captured notifier body is replayed in a unit test.
`exporter-down` and `agent-down` are observe only. No hardware run has fired an
alert through vmalert into the controller.

**XID catalog — hardware-validated.** `internal/detect/xid.go` classified
kernel-log XIDs 79 and 92 read off a live node into `ClassFellOffBus` and
`ClassECCSBERate` during the EKS runs, including the latter's 3-in-24h
threshold hold.

**Neutral fault catalog — not hardware-validated.** `internal/detect/fault.go`
holds 18 rows across two vendors: 9 `nvidia` and 9 `amd`. The NVIDIA source uses the
`nvidia-smi -q` fallback. The managed agent detects AMD faults from matching
`amdgpu` kernel-log entries. Additional AMD rows map signals from
`internal/agent/amdhealth`, which an operator-managed installation does not enable. `fault_coverage_test.go` fails the build if any class the XID
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

**An accelerator adapter's report raises that depth.** When the agent runs an
accelerator adapter for such a vendor (`spec.agent.adapters`), the adapter
posts an observation-only report for the node and vendor. The accelerator
report protocol accepts any well-formed vendor id (lowercase letters, digits,
and hyphens, at most 32 characters), so this works for vendors beyond the four
built-in constants. When a current (at most five minutes old) report exists,
the incident is verified against it instead of the heartbeat alone: the report
must not be `not-ready`, and the target device must be in its inventory as a
physical GPU. An observation-only report is `degraded` or `observed` at best
by design, and that is accepted; the resolve audit entry says the device was present and the
runtime was not attested. With no current report the heartbeat path above
still applies. Only an authenticated agent can name an incident's vendor:
a `vendor` annotation on an Alertmanager alert, or on a manual trigger, is
ignored, and an agent event whose fault names a malformed vendor id is
rejected.

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
unresolvable node labels fail closed with the cause in the reason.

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

## Vendor matching for inventory and eviction

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
resource from anywhere else is ignored and logged.

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
| ~~`verifyRuntimeEvidence` reads an NVIDIA accelerator report for every GPU-class target~~ | A vendor with no runtime adapter now verifies on the agent heartbeat, logged as reduced depth, instead of holding for a report nothing can produce |
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
