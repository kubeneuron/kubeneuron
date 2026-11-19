# Accelerator health from Kubernetes nodes (experimental)

KubeNeuron reads what the Kubernetes Node object of an accelerator node says
about its devices: conditions written by a vendor's or a cloud's node
monitoring component, and how many of the node's accelerator devices are
allocatable. It opens an incident when such a
signal reports a fault. It reads only what the components your cluster
already runs write to the Node. KubeNeuron ships no vendor tool, needs no agent
on the node for this, and adds no permission: the controller already reads
Nodes.

This is experimental. Each profile's signal names are taken from vendor
documentation on 2026-11-18 and are not validated on real hardware. A signal is
what the node reports, not a diagnosis by KubeNeuron.

Vendor and product names (AMD Instinct, AWS Neuron, Trainium, NVIDIA, Google
TPU, Karpenter) identify the components whose signals are read. KubeNeuron is
not affiliated with, endorsed by or certified by these vendors, and is
unrelated to the AWS Neuron SDK.

## How a signal becomes an incident

- A profile applies to a node that advertises its device resource.
- A condition must show the same fault on two consecutive reads.
  Allocatable below capacity must also last at least five minutes, so a
  device plugin restarted by an upgrade opens nothing.
- A signal that goes away before that starts over.
- A condition whose heartbeat is more than 10 minutes old, or whose status is
  neither `True` nor `False`, is unknown. So is a node that has none of the
  profile's conditions, and a condition at its fault status with a reason
  the profile's source does not document: an undocumented reason may be
  software as well, and it must not lead to a replacement. Unknown is never healthy: it neither confirms a fault nor clears one,
  and `kubeneuron_accelerator_health_nodes` counts it apart from healthy nodes.
  A condition the profile reads but does not act on (see below) is no signal
  of the profile either.
- While an incident is open, the same signal adds nothing to it. The signal
  is reported again only when the incident was closed with the fault still
  present, or when the incident is verifying a recovery and the node still
  reports the fault: the verification then fails.
- The incident records the profile, its version, the rule, the condition
  type, the condition's reason, and when the condition last changed. The
  condition's message is not read.

The incident's target is the node. When the agent has already opened the
same class for one of the node's devices, the node's signal is recorded on
that incident instead, and no second incident opens. When the node incident
opens first, later agent reports for identified GPUs on that node join the
open incident of the same class. Its audit records each reporting GPU UUID,
and its playbook continues from the current step.

## Classes

| Class | Opened for | Fault location |
|---|---|---|
| `accelerator-fault` | An uncorrectable error the node reports that no existing class names | This node, like the other device faults |
| `accelerator-unallocatable` | Fewer allocatable devices than capacity, for five minutes | Unknown: a stopped device plugin and a failed device look the same, so this class never leads to an automatic replacement |
| an existing class | A signal that names one exactly: a double-bit ECC error (`ecc-dbe`), an NVLink error (`nvlink`), a missing GPU (`gpu-lost`), an XID by its code | As for that class |

The shipped policy packs bind `accelerator-fault` to the RMA ladder (the cloud
pack: to replacement, with approval), and `accelerator-unallocatable` to
observe.

## Profiles

Signal names are taken from vendor documentation on 2026-11-18; not validated
on real hardware.

| Profile | Selected by | Signal | Fault when | Device | Class |
|---|---|---|---|---|---|
| `amd-instinct` | `amd.com/gpu` | condition `AMDGPUProblem` of the AMD GPU Operator's [Node Problem Detector integration](https://instinct.docs.amd.com/projects/gpu-operator/en/latest/npd/node-problem-detector.html) | `True` with reason `AMDGPUIsDown` | not named | `accelerator-fault` |
| `aws-neuron` | `aws.amazon.com/neuron` (or the older `aws.amazon.com/neurondevice`) | condition `NeuronHealth` of the [Neuron node problem detector](https://awsdocs-neuron.readthedocs-hosted.com/en/latest/deploy/infrastructure/problem-detector.html) | `True` with reason `NeuronHasError_DMA_ERROR`, the only reason a public document shows ([AWS blog](https://aws.amazon.com/blogs/machine-learning/node-problem-detection-and-recovery-for-aws-neuron-nodes-within-amazon-eks-clusters)) | not named | `accelerator-fault` |
| `aws-neuron` | same | condition `AcceleratedHardwareReady` of the [EKS node monitoring agent](https://docs.aws.amazon.com/eks/latest/userguide/node-health-nma.html) | `False` with reason `NeuronDMAError`, `NeuronHBMUncorrectableError`, `NeuronNCUncorrectableError` or `NeuronSRAMUncorrectableError` | not named | `accelerator-fault` |
| `nvidia` | `nvidia.com/gpu` | condition `AcceleratedHardwareReady` of the [EKS node monitoring agent](https://docs.aws.amazon.com/eks/latest/userguide/node-health-nma.html) | `False` | not named | by reason: `NvidiaDoubleBitError` → `ecc-dbe`, `NvidiaNVLinkError` → `nvlink`, `NvidiaXID<code>Error` for an XID the agent's table lists → the class of that XID |
| `nvidia` | same | conditions `GPUMissing` and `NVLinkDown` of the AKS Node Problem Detector | `True` | not named | `gpu-lost`, `nvlink` |
| `google-tpu` | `google.com/tpu` | GKE writes no TPU condition; only the allocatable count below, and, when turned on, [Google Cloud Monitoring](#tpu-health-from-google-cloud-monitoring-opt-in) | | | |
| every profile | its resource | allocatable below capacity, for five minutes | | not named | `accelerator-unallocatable` |

A condition at its fault status with any other reason is unknown, with the
reason named in the observation: for example a Neuron node problem detector
reason other than `NeuronHasError_DMA_ERROR`, or an XID the EKS table does
not list.

A node that lists its resource at zero (a failed device can take the count
down) is still read with the profile: its conditions and taints as above,
and its allocatable count as unknown, which neither confirms nor clears a
pending `accelerator-unallocatable`.

### Read, not acted on

These conditions and reasons are read and never open an incident. A node on which one is `True` is counted as `ignored` in
`kubeneuron_accelerator_health_nodes` (in addition to its other state), so
the signal is not silent.

| Condition | Why it is not acted on |
|---|---|
| AKS `XIDError` | It does not say which XID. An application XID is not a fault of the node. |
| AKS `GPUECCError` | The check counts corrected and uncorrected errors alike. A corrected error is no reason to take the node. |
| AKS `GPUClockThrottling` | Throttling slows the device. It is not a device fault. |
| AKS `UnhealthyNvidiaDevicePlugin` | A stopped device plugin is software, not a device. The allocatable count shows what it costs. |
| AKS `UnhealthyNvidiaDCGMServices` | A stopped DCGM service is monitoring software, not a device. |
| EKS `AcceleratedHardwareReady=False` with reason `DCGMDiagnosticFailure`, `DCGMError` or `DCGMHealthCode<code>` | DCGM reports its own failure or a diagnostic result. The EKS documentation gives it no repair action. |

Not read at all: conditions whose names a public document does not confirm,
Kubernetes events, and DRA ResourceSlice device taints and cloud monitoring
APIs other than the opt-in reads below.

## Sources off by default

These sources are not validated on live hardware, so none of them opens an
incident until it is switched on. In Automatic mode a misread signal could
mean a replaced node.

| Source | Switch | While it is off |
|---|---|---|
| [TPU health from Google Cloud Monitoring](#tpu-health-from-google-cloud-monitoring-opt-in) | `spec.cloud.gcp.tpuInfraHealth.enabled` | not read |
| [Device taints from Kubernetes DRA](#device-taints-from-kubernetes-dra-opt-in) | `spec.safety.acceleratorHealth.dra.enabled` | not read |
| [AWS Neuron device health from the driver's sysfs](#aws-neuron-device-health-from-the-drivers-sysfs-incidents-opt-in), read by the agent | `spec.safety.acceleratorHealth.awsNeuronSysfs.enabled` | a fault is shown in `kubeneuronctl health` as not acted on, for up to a day, and counted in `kubeneuron_accelerator_faults_not_acted_on_total`; no incident |

After a restart or a change of leader the controller reads the faults of
the last day back from its event archive; a fault that came while an
incident of its node was open is not listed, since that incident says what
was done. With an external event archive the rows are not restored after a
restart.

## TPU health from Google Cloud Monitoring (opt-in)

GKE writes no TPU condition to the Node. With
`spec.cloud.gcp.tpuInfraHealth.enabled: true`, the controller also reads the
Cloud Monitoring metric `compute.googleapis.com/instance/tpu/infra_health`
for the instances of the cluster's TPU nodes, as a second source of the
`google-tpu` profile. It is off by default.

The metric is a Google Cloud Beta (pre-GA) feature: Google may change or
withdraw it and gives it no SLA. This read is experimental and not validated
against a live GKE cluster; whether Google writes this metric for GKE nodes
is not confirmed. The values are those of Google's
[TPU health page](https://docs.cloud.google.com/tpu/docs/troubleshooting/tpu-health)
and [metric list](https://docs.cloud.google.com/monitoring/api/metrics_gcp_c)
(2026-11-18).

- **What is read.** Once a minute, one list per project of the TPU nodes'
  provider IDs, for the last 15 minutes; one read waits at most 20 seconds
  for an answer, then counts as `api-error`. A TPU node is one that lists
  `google.com/tpu`, even at zero (a failed chip can take the count down), or
  carries GKE's `cloud.google.com/gke-tpu-accelerator` label. A series is
  matched to a node by the instance's zone and name. Nothing is asked of
  Cloud Monitoring while the cluster has no TPU node. The metric names no
  chip: a fault is the node's. A TPU node without a TPU count is taken to be
  one host of a multi-host slice when it has a topology label, so it is not
  replaced alone.
- **Fault.** `UNHEALTHY` with category `BAD_TPU_CHIP` or `BAD_SC_CHIP` on two
  consecutive samples (two samples, not two reads of one) opens
  `accelerator-fault` on the node. The incident records the source
  (`cloud-monitoring`), the rule `gcp-infra-health`, the category and when
  the run of samples began.
- **Read, not acted on.** `DEGRADED` (reason `degraded`: performance, not a
  fault) and `UNHEALTHY` with `NETWORKING_ISSUE` (reason `networking-issue`:
  it does not place the fault on this node) open nothing and are counted as
  `ignored` in `kubeneuron_accelerator_health_nodes`, and by reason in
  `kubeneuron_accelerator_health_ignored_nodes`.
- **Unknown, never healthy.** No sample for the instance, a newest sample
  more than 10 minutes old (the metric is sampled every 60 seconds and
  visible up to 420 seconds later), a failed read (`permission-denied`,
  `api-error`), an answer longer than 20 pages, two statuses at one sample
  time, `UNKNOWN`, any other status, and `UNHEALTHY` with no category or one
  not listed here. Unknown neither confirms a pending fault nor clears it;
  only a fresh `HEALTHY` sample does. A sample older than the Node belongs to
  an earlier instance of the same name and is not read.
- **Reads** are counted in `kubeneuron_cloud_health_reads_total` by outcome.
  A failed read leaves the TPU nodes unknown and holds no step on other
  nodes. After a failed read the controller waits longer before the next one,
  up to five minutes.
- **Cost and permission.** Cloud Monitoring API read calls are billed to the
  project owner at Google Cloud rates. The controller needs
  `monitoring.timeSeries.list` in those projects; see
  [Replacing GPU nodes on GKE](cloud-gcp.md#setup).

## Device taints from Kubernetes DRA (opt-in)

With `spec.safety.acceleratorHealth.dra.enabled: true`, the controller also
lists the `resource.k8s.io/v1` ResourceSlices and reads the taints that
Dynamic Resource Allocation drivers set on the devices they publish, as one
more source (`dra`) of the profiles. It is off by default. Device taints
are a Kubernetes beta feature in 1.36 (feature gate `DRADeviceTaints`, on by
default there); Kubernetes and the drivers may still change them. This read
is experimental and has not run against a driver on hardware. Checked
against a Kubernetes 1.36.4 API server in kind (`hack/kind-dra-health.sh`):
a ResourceSlice written by hand, with no driver running, keeps a device
taint in `resource.k8s.io/v1` with `timeAdded` set, and the read takes it as
a fault. The incident path is covered by unit tests only. That script is
run by hand; CI does not run it.

- **What is read.** Once per controller pass, one list of the v1
  ResourceSlices. Only a slice whose `spec.nodeName` names an existing Node
  is read; a slice scoped by a node selector, all nodes or per-device
  selection is skipped. From a device: the driver, pool and device name, its
  `uuid` attribute and its taints (key, value, effect, `timeAdded`). Nothing
  is written, and `DeviceTaintRule` objects are not read.
- **Drivers and taints.** Only the drivers and keys below, from the drivers'
  own documentation (read 2026-11-18); a key is read only in a slice of its
  own driver. Effects `NoSchedule` and `NoExecute` act alike; `None`, or an
  effect not listed, does not act.

| Driver (the slice's driver name) | Profile | Taint | Effect | Read as |
|---|---|---|---|---|
| `gpu.nvidia.com` ([NVIDIA DRA driver](https://github.com/kubernetes-sigs/dra-driver-nvidia-gpu) v0.4.0+, health checks behind its `NVMLDeviceHealthCheck` gate) | `nvidia` | `gpu.nvidia.com/xid`, value the XID | `NoSchedule` / `NoExecute` | fault, classed by the XID table |
| | | `gpu.nvidia.com/xid` | `None` | ignored, `non-fatal-xid` |
| | | `gpu.nvidia.com/gpu-lost` | `NoSchedule` / `NoExecute` | `accelerator-fault` |
| | | `gpu.nvidia.com/unmonitored` | any | no signal (`unmonitored`) |
| `neuron.aws.com` ([Neuron DRA driver](https://awsdocs-neuron.readthedocs-hosted.com/en/latest/deploy/eks/neuron-device-taints-guide.html) 1.3.0+, Kubernetes 1.36+, health monitoring off by default) | `aws-neuron` | `neuron.aws.com/device-unhealthy`: `hbm-uncorrectable-error`, `sram-uncorrectable-error`, `device-unreachable`, `link-host-error`, `firmware-service-processor-error` | `NoSchedule` / `NoExecute` | `accelerator-fault` |
| | | `neuron.aws.com/device-unhealthy`: `link-intraserver-error`, `link-interserver-error`, `link-efa-error`, `link-switch-error`, `fabric-error` | any | ignored, `interconnect`: it does not place the fault on this device |
| | | `neuron.aws.com/device-unhealthy`, any other value (including `firmware-service-api-error`) | any | unknown |
| | | `neuron.aws.com/peer-tainted` | `None` | ignored, `peer-tainted` |
| `gpu.amd.com` | `amd-instinct` | none documented for health | | |
| any of these | | any other key | any | ignored, `undocumented-taint` |

  The Google TPU DRA driver publishes no device taints; TPU health is not
  read this way. A slice of any other driver is not read.
- **No taint is no signal.** A device without a taint, or one its driver
  marks `unmonitored`, says nothing: it is neither healthy nor unknown,
  holds no step and clears no other source's fault. With the gate off the
  API server drops the taints, and a driver may not publish health taints at
  all, so the absence of a taint is not health. Such devices are counted as
  `no-health-taint` and `unmonitored` in `kubeneuron_dra_health_devices`.
- **Fault.** A fault taint opens its class on the node after two reads, or
  on the first read when its `timeAdded` is at least two minutes old. The
  incident is the node's; its evidence names the source (`dra`), the
  driver, pool and device, the device's UUID when it is in the agent's form,
  and the XID or the documented value. The taint's text is kept only when
  it is a number or a value listed above.
- **Clearing and holding.** A pending fault clears only when the same
  device (driver, pool, device name) is published again without it. A
  device its Node no longer publishes, or a read that fails, is unknown and
  holds the fault. A Node that no longer exists, as at the end of its
  replacement, holds nothing: its pending faults are dropped.
- **Reads** are counted in `kubeneuron_dra_health_reads_total` by outcome.
  A cluster that serves no `resource.k8s.io/v1` ResourceSlices (before
  Kubernetes 1.34, or a beta version only) reads as `api-unavailable`;
  nothing else changes, and no step is held for it.
- **Trust.** Anyone who can write ResourceSlices can open an incident here
  and, through the policy, have the node replaced. Grant that write only to
  the DRA drivers.
- **Permission.** The controller's ClusterRole gains `get`, `list` and
  `watch` on `resource.k8s.io` `resourceslices` while this, or the topology
  classifier's DRA read, is on; nothing else in the group.

## AWS Neuron device health from the driver's sysfs (incidents opt-in)

On a node with the AWS Neuron kernel driver, the agent reads each device's
error counters and its list of failing components from the driver's sysfs
tree, `/sys/devices/virtual/neuron_device`, as one more source
(`aws-neuron-sysfs`) of the `aws-neuron` profile. AWS Neuron devices are the
accelerators (AWS Trainium and AWS Inferentia) that the
`aws.amazon.com/neuron` resource counts. The read needs no switch; an
incident from it does: with
`spec.safety.acceleratorHealth.awsNeuronSysfs.enabled: true` a fault opens an
incident, and it is off by default. Off, a fault is shown in
`kubeneuronctl health` as a fault that was not acted on, with the switch
that would act on it, and nothing is held, cordoned or replaced for it.

This read is experimental, not validated against a live AWS Trainium or
Inferentia node. The file paths and their meanings are taken from AWS
documentation, reviewed 2026-11-18: the
[Neuron sysfs user guide](https://awsdocs-neuron.readthedocs-hosted.com/en/v2.25.0/tools/neuron-sys-tools/neuron-sysfs-user-guide.html)
and the
[Neuron runtime and driver release notes](https://awsdocs-neuron.readthedocs-hosted.com/en/v2.32.0/release-notes/components/runtime.html).
The driver version that adds the component list (2.30.2) is AWS's statement,
not something KubeNeuron checked. Every test tree is synthetic.

- **What is read.** For each `neuron<N>` directory, at most 64 of them:
  the HBM and SRAM ECC counters under `stats/hardware` and
  `stats/hardware/health_status/unhealthy_components`. Only numbers and
  component names are read, each file at most 4 KiB. The agent runs no AWS
  tool, never writes to the tree (writing clears the counters, which another
  tool on the node may also read) and does not read serial numbers. A device
  is its index, `neuron<N>`.
- **Counters.** A rise of `mem_ecc_uncorrected` (HBM) or
  `sram_ecc_uncorrected` (SRAM) is an `accelerator-fault`. The last values are
  kept in the agent's state directory with the boot they were read in, so an
  error that happened while the agent was down is still a rise; they are kept
  only after the finding has been delivered. A counter that was already above
  zero when there was no earlier value to compare with is recorded, never a
  fault: nothing says when it rose. A rise of
  `mem_ecc_repairable_uncorrected` is an observation, not a diagnosis: the
  device can repair these. The corrected counters are read and not acted
  on.
- **Components.** A component the driver lists as failing is an
  `accelerator-fault` once two consecutive reads list it; one read is no
  fault. It is sent once, and again only after a read no longer lists it. A name not in the documented list (`hbm`,
  `sram`, `firmware_service_api`, `firmware_service_processor`,
  `link_intraserver`, `link_host`, `link_efa`, `link_interserver`,
  `link_switch`, `axi_fabric`) is recorded as `unknown-component`; text read
  from the node is never kept.
- **No file is no signal.** A driver without `health_status` says nothing
  about components; it is not healthy. A node without the `neuron_device`
  directory does not report this source at all. A device that cannot be read
  is unknown and holds its own state while the other devices are read; a
  device that was read before and is gone from the tree is an observation. A
  tree that cannot be read is shown in `kubeneuronctl health` as an
  unavailable agent source with its reason, never as a healthy node.
- **Vendor.** The agent sends the node's devices to the controller in an
  accelerator report of vendor `aws`, observation only: it declares no
  action KubeNeuron could take on a device.
- **Fault.** With the switch on, the incident is the node's. Its evidence
  names the source, the device (`neuron<N>`), the class and, for a
  component, its name from the list above.

## When KubeNeuron only observes

- **Another repairer.** A node with the taint `amd-gpu-unhealthy` (set by the
  AMD GPU Operator's [remediation](https://instinct.docs.amd.com/projects/gpu-operator/en/latest/autoremediation/auto-remediation.html))
  or `karpenter.sh/disrupted` (set by [Karpenter](https://karpenter.sh/docs/concepts/disruption/))
  is being repaired or replaced by that component. KubeNeuron holds every step that would cordon,
  drain, evict, recycle or replace the node, and records why. Other repairers
  (the Neuron detector's recovery mode, a cloud's own node repair outside the
  provider check) are not visible on the Node; the incident records
  `other_repairer: unknown`. Automatic mode acts only on the runtime profiles
  listed in `qualifiedProfiles`: listing a profile there means the operator has
  confirmed that no other repairer acts on those nodes.
- **Before the first read.** Until every health source (the Nodes, and
  Cloud Monitoring and the DRA device taints when they are turned on) has been read once since this
  controller became leader, KubeNeuron cannot tell whether another repairer
  is acting on a node, so it holds every such step on every node and records
  why. A failed read of the Nodes does not count. A new leader asks Cloud
  Monitoring again rather than use what an earlier term read.
- **Multi-host TPU slices.** A node whose `cloud.google.com/gke-tpu-topology`
  holds more chips than the node is one host of a slice. Its incident names the
  slice's node pool, and KubeNeuron never recycles or replaces one host alone:
  that is not a recovery of the slice.

## Seeing what was read

`kubeneuronctl health [--node <node>] [--json]` (`GET /api/v1/accelerator-health?node=<node>`)
shows what the elected leader last read from every source: one row per node,
source, profile, rule and device, sorted in that order, with the state, the
reason the source reported, KubeNeuron's explanation and when the source was
read. `--node` is an exact node name. The view is held in the leader's memory
and starts empty in every leadership term; a standby answers `503`. It reads
nothing new and changes nothing.

- **States.** `healthy`, `fault`, `unknown` and `ignored` are those of the
  profiles above. `no-signal` is a DRA device that carries no taint, or that
  its driver says it does not watch: the source says nothing about its health,
  and that is not healthy. For the signal sources an agent reads on the node
  (`source` `agent`, the rule naming the agent's source), `unknown` is one it
  cannot read, with its reason, and `available` one it reads: a fault it reads
  opens an incident, and none read is not health. Agents send a full report at registration heartbeats. Reports expire two minutes
  after receipt. A node
  asked for by name whose agent reports none (an older agent, or no agent) has
  one `agent` row `unknown` saying there is no data.
- **Held steps.** A source not yet read in this term has one row with the
  state `not-read-this-term`: it is the reason every step that takes a node
  out of service is held on every node (see [Before the first read](#when-kubeneuron-only-observes)).
  A source whose reads fail says so on that row; once read, a later failed read
  adds an `unknown` row and the other rows stay those of the read before. A
  node another repairer acts on has a row with the rule `other-repairer`.
- **Text.** The explanation is KubeNeuron's own. What a source reports is
  shown only as its reason, cut to 256 characters; a condition's message and a
  cloud's error text are never shown. An agent's source name or reason is
  shown only in the form of a code.
- **Size.** At most 5000 rows; past that the answer has `truncated: true` and
  the command says so.

## What only real hardware can show

That each component writes these signals on these nodes, with these names and
reasons, and how long after a fault it writes them. None of the profiles has
run on hardware. For Cloud Monitoring: whether Google writes
`infra_health` for the instances of GKE TPU nodes, with these labels, and how
long after a fault; and whether it writes one series per instance or one per
status with the value 0 or 1. The value is not read, so in the second case
every sample time carries two statuses and the node stays unknown: safe, but
the source then tells nothing. For DRA: that the drivers set these taints on
real faults, with these values, and how long after.
