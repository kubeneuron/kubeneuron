# Accelerator health from Kubernetes nodes (experimental)

KubeNeuron reads what the Kubernetes Node object of an accelerator node says
about its devices: conditions written by a vendor's or a cloud's node
monitoring component, labels written by a vendor exporter, and how many of the
node's accelerator devices are allocatable. It opens an incident when such a
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
- A condition or label must show the same fault on two consecutive reads.
  Allocatable below capacity must also last at least five minutes, so a
  device plugin restarted by an upgrade opens nothing.
- A signal that goes away before that starts over.
- A condition whose heartbeat is more than 10 minutes old, or whose status is
  neither `True` nor `False`, is unknown. So is a node that has none of the
  profile's conditions, and a device label with a value the profile does not
  know. Unknown is never healthy: it neither confirms a fault nor clears one,
  and `kubeneuron_accelerator_health_nodes` counts it apart from healthy nodes.
  A condition the profile reads but does not act on (see below) is no signal
  of the profile either.
- While an incident is open, the same signal adds nothing to it. The signal
  is reported again only when the incident was closed with the fault still
  present, or when the incident is verifying a recovery and the node still
  reports the fault: the verification then fails.
- The incident records the profile, its version, the rule, the condition type
  or label key, the condition's reason or the label's value, the device index
  when the signal names one, and when the condition last changed. The
  condition's message is not read.

The incident's target is the node. A device index is recorded as evidence
only.

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
| `amd-instinct` | `amd.com/gpu` | label `metricsexporter.amd.com.gpu.<index>.state`, written by the Device Metrics Exporter | `unhealthy` (`healthy` is healthy; any other value is unknown) | the index in the label key | `accelerator-fault` |
| `amd-instinct` | `amd.com/gpu` | conditions `AMDGPUProblem` and `AMDGPUIsDown` of the vendor's Node Problem Detector integration | `True` | not named | `accelerator-fault` |
| `aws-neuron` | `aws.amazon.com/neuron` (or the older `aws.amazon.com/neurondevice`) | condition `NeuronHealth` of the Neuron node problem detector | `True` | not named | `accelerator-fault` |
| `aws-neuron` | same | condition `AcceleratedHardwareReady` of the EKS node monitoring agent | `False` | not named | `accelerator-fault` |
| `nvidia` | `nvidia.com/gpu` | condition `AcceleratedHardwareReady` of the EKS node monitoring agent | `False` | not named | by reason: `NvidiaDoubleBitError` → `ecc-dbe`, `NvidiaNVLinkError` → `nvlink`, `NvidiaXID<code>Error` → the class of that XID, any other reason → `accelerator-fault` |
| `nvidia` | same | conditions `GPUMissing` and `NVLinkDown` of the AKS Node Problem Detector | `True` | not named | `gpu-lost`, `nvlink` |
| `google-tpu` | `google.com/tpu` | GKE writes no TPU condition; only the allocatable count below | | | |
| every profile | its resource | allocatable below capacity, for five minutes | | not named | `accelerator-unallocatable` |

### Read, not acted on

These AKS Node Problem Detector conditions are read and never open an
incident. A node on which one is `True` is counted as `ignored` in
`kubeneuron_accelerator_health_nodes` (in addition to its other state), so
the signal is not silent.

| Condition | Why it is not acted on |
|---|---|
| `XIDError` | It does not say which XID. An application XID is not a fault of the node. |
| `GPUECCError` | The check counts corrected and uncorrected errors alike. A corrected error is no reason to take the node. |
| `GPUClockThrottling` | Throttling slows the device. It is not a device fault. |
| `UnhealthyNvidiaDevicePlugin` | A stopped device plugin is software, not a device. The allocatable count shows what it costs. |
| `UnhealthyNvidiaDCGMServices` | A stopped DCGM service is monitoring software, not a device. |

Not read at all: conditions whose names a public document does not confirm,
Kubernetes events, DRA ResourceSlice device taints and cloud monitoring
APIs.

## When KubeNeuron only observes

- **Another repairer.** A node with the taint `amd-gpu-unhealthy` (set by the
  AMD GPU Operator's remediation) or `karpenter.sh/disrupted` is being repaired
  or replaced by that component. KubeNeuron holds every step that would cordon,
  drain, evict, recycle or replace the node, and records why. Other repairers
  (the Neuron detector's recovery mode, a cloud's own node repair outside the
  provider check) are not visible on the Node; the incident records
  `other_repairer: unknown`. Automatic mode acts only on the runtime profiles
  listed in `qualifiedProfiles`: listing a profile there means the operator has
  confirmed that no other repairer acts on those nodes.
- **Multi-host TPU slices.** A node whose `cloud.google.com/gke-tpu-topology`
  holds more chips than the node is one host of a slice. Its incident names the
  slice's node pool, and KubeNeuron never recycles or replaces one host alone:
  that is not a recovery of the slice.

## What only real hardware can show

That each component writes these signals on these nodes, with these names and
reasons, and how long after a fault it writes them. None of the profiles has
run on hardware.
