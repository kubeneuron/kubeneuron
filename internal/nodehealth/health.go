// SPDX-License-Identifier: Apache-2.0

// Package nodehealth reads what a Kubernetes Node says about its accelerators
// through built-in profiles, one per accelerator family. A profile names the
// Node conditions, labels and resources a vendor or cloud component writes,
// and what each value means. Nothing here talks to the node or to a cloud:
// the input is one read of the Node object.
//
// A signal is what the node reports, never a diagnosis. A signal that is
// missing, stale or of unknown status is unknown, never healthy.
package nodehealth

import (
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/kubeneuron/kubeneuron/internal/detect"
	"github.com/kubeneuron/kubeneuron/pkg/types"
)

// Version is the version of the built-in profile table. It changes whenever
// a rule is added, removed or remapped, and is recorded on every signal.
const Version = "v1"

// State is what one rule, or one node, says.
type State string

const (
	StateHealthy State = "healthy"
	StateFault   State = "fault"
	StateUnknown State = "unknown"
	// StateIgnored: the node reports a problem on a signal that is read but
	// not acted on, because it does not say enough to blame the node.
	StateIgnored State = "ignored"
)

const (
	// conditionFreshness is how old a condition's heartbeat may be before
	// the condition is unknown. Node Problem Detector renews a condition's
	// heartbeat every 5 minutes by default; twice that allows one missed
	// renewal.
	conditionFreshness = 10 * time.Minute
	// confirmReads is how many consecutive reads a condition or label must
	// show before it opens an incident.
	confirmReads = 2
	// unallocatableHold is how long allocatable must stay below capacity: a
	// device plugin restarted by an upgrade takes its devices away for a
	// moment, and that is no incident.
	unallocatableHold = 5 * time.Minute
)

// Observation is one thing a profile read from a Node.
type Observation struct {
	Profile string
	Rule    string
	State   State
	// Class is the incident class of a fault; empty otherwise.
	Class types.ProblemClass
	// Key identifies the signal: the same key on consecutive reads is the
	// same signal. A reason that changes without a new transition does not
	// change it.
	Key string
	// Reason is the condition's reason or the label's value, as the node
	// reports it.
	Reason string
	// Device is the device index when the signal names one, or the DRA
	// device name.
	Device string
	// DeviceUUID is the device's UUID in the agent's form, when the source
	// names one.
	DeviceUUID string
	// Since is when the signal last changed, when the Node says so.
	Since time.Time
	// Why says why a signal is unknown.
	Why string
	// Reads and Hold are how long the signal must be seen before it counts:
	// Reads consecutive reads, and at least Hold since it was first seen.
	Reads int
	Hold  time.Duration
}

// NodeHealth is what the profiles read from one Node.
type NodeHealth struct {
	Node string
	// Profiles are the names of the profiles that apply to the node.
	Profiles []string
	// State is fault when any rule reports a fault, otherwise unknown when
	// any profile has no fresh signal, otherwise healthy. An ignored signal
	// does not change it.
	State        State
	Observations []Observation
	// OtherRepairers are the components the Node shows are repairing it on
	// their own.
	OtherRepairers []string
	// Slice is the node pool of a TPU slice that spans several nodes; empty
	// otherwise.
	Slice string
}

// conditionRule reads one Node condition. A condition at faultStatus is a
// fault when its source documents the reason, classified from it; with a
// reason the source does not document it is unknown, since an undocumented
// reason may be software as well. At the other of True/False it is healthy;
// at any other status it is unknown.
//
// A rule with ignored set is read and never acted on: at faultStatus the
// node is reported as ignored, with that reason, and nothing else follows.
// It does not count as a signal of the profile.
type conditionRule struct {
	id          string
	condition   string
	faultStatus string
	reasons     func(reason string) verdict
	ignored     string
}

// verdict is what a condition at its fault status says, given its reason.
type verdict struct {
	class types.ProblemClass
	// ignored, when set, says why the reason is read and not acted on.
	ignored string
	// known is false for a reason the source does not document.
	known bool
}

// Profile is one accelerator family's signals.
type Profile struct {
	Name string
	// Resources are the extended resources that select the profile: a node
	// advertising one of them is read with it.
	Resources  []string
	conditions []conditionRule
}

// Profiles is the built-in table. Signal names are taken from vendor
// documentation on 2026-11-18 and are not validated on real hardware. Only
// names a public document confirms are listed, each with its source.
var Profiles = []Profile{
	{
		Name:      "amd-instinct",
		Resources: []string{"amd.com/gpu"},
		conditions: []conditionRule{
			// AMD GPU Operator, Node Problem Detector integration: the
			// condition AMDGPUProblem is False while healthy and True with
			// the reason AMDGPUIsDown when the amdgpuhealth check reports a
			// problem.
			// https://instinct.docs.amd.com/projects/gpu-operator/en/latest/npd/node-problem-detector.html
			// (2026-11-18)
			{id: "npd-amdgpu-problem", condition: "AMDGPUProblem", faultStatus: "True",
				reasons: documented(map[string]types.ProblemClass{"AMDGPUIsDown": types.ClassAcceleratorFault})},
		},
	},
	{
		Name:      "aws-neuron",
		Resources: []string{"aws.amazon.com/neuron", "aws.amazon.com/neurondevice"},
		conditions: []conditionRule{
			// The Neuron node problem detector in monitor-only mode: the
			// condition NeuronHealth is True on an unrecoverable error. The
			// only reason a public document shows is NeuronHasError_DMA_ERROR.
			// https://aws.amazon.com/blogs/machine-learning/node-problem-detection-and-recovery-for-aws-neuron-nodes-within-amazon-eks-clusters
			// and https://awsdocs-neuron.readthedocs-hosted.com/en/latest/deploy/infrastructure/problem-detector.html
			// (2026-11-18)
			{id: "neuron-npd", condition: "NeuronHealth", faultStatus: "True",
				reasons: documented(map[string]types.ProblemClass{"NeuronHasError_DMA_ERROR": types.ClassAcceleratorFault})},
			// The EKS node monitoring agent: AcceleratedHardwareReady is
			// False with one of the Neuron reasons its table lists, all with
			// the repair action Replace.
			// https://docs.aws.amazon.com/eks/latest/userguide/node-health-nma.html
			// (2026-11-18)
			{id: "eks-node-monitoring", condition: "AcceleratedHardwareReady", faultStatus: "False",
				reasons: documented(map[string]types.ProblemClass{
					"NeuronDMAError":               types.ClassAcceleratorFault,
					"NeuronHBMUncorrectableError":  types.ClassAcceleratorFault,
					"NeuronNCUncorrectableError":   types.ClassAcceleratorFault,
					"NeuronSRAMUncorrectableError": types.ClassAcceleratorFault,
				})},
		},
	},
	{
		Name:      "nvidia",
		Resources: []string{"nvidia.com/gpu"},
		conditions: []conditionRule{
			// The EKS node monitoring agent: the NVIDIA reasons of severity
			// Condition its table lists, under AcceleratedHardwareReady=False.
			// https://docs.aws.amazon.com/eks/latest/userguide/node-health-nma.html
			// (2026-11-18)
			{id: "eks-node-monitoring", condition: "AcceleratedHardwareReady", faultStatus: "False", reasons: eksNVIDIAReason},
			// The AKS Node Problem Detector GPU checks,
			// https://learn.microsoft.com/en-us/azure/aks/gpu-health-monitoring
			// and https://learn.microsoft.com/en-us/azure/aks/node-problem-detector
			// (2026-11-10). True is a problem, the Node Problem Detector's
			// convention. The documents list no reasons: each condition
			// names one fault by its type.
			{id: "aks-npd-gpu-missing", condition: "GPUMissing", faultStatus: "True", reasons: anyReason(types.ClassGPULost)},
			{id: "aks-npd-nvlink-down", condition: "NVLinkDown", faultStatus: "True", reasons: anyReason(types.ClassNVLink)},
			{id: "aks-npd-xid", condition: "XIDError", faultStatus: "True",
				ignored: "the condition does not say which XID; an application XID is not a fault of the node"},
			{id: "aks-npd-gpu-ecc", condition: "GPUECCError", faultStatus: "True",
				ignored: "the check counts corrected and uncorrected errors alike; a corrected error is no reason to take the node"},
			{id: "aks-npd-clock-throttling", condition: "GPUClockThrottling", faultStatus: "True",
				ignored: "throttling slows the device; it is not a device fault"},
			{id: "aks-npd-device-plugin", condition: "UnhealthyNvidiaDevicePlugin", faultStatus: "True",
				ignored: "a stopped device plugin is software, not a device; the allocatable count shows what it costs"},
			{id: "aks-npd-dcgm-services", condition: "UnhealthyNvidiaDCGMServices", faultStatus: "True",
				ignored: "a stopped DCGM service is monitoring software, not a device"},
		},
	},
	{
		// GKE writes no TPU condition: the device plugin's allocatable count
		// is the only Node signal.
		Name:      "google-tpu",
		Resources: []string{"google.com/tpu"},
	},
}

// Classes is every class a profile can give a fault.
func Classes() []types.ProblemClass {
	out := []types.ProblemClass{
		types.ClassAcceleratorFault, types.ClassAcceleratorUnallocatable,
		types.ClassECCDBE, types.ClassNVLink, types.ClassGPULost,
	}
	for _, x := range detect.XIDTable() {
		if !slices.Contains(out, x.Class) {
			out = append(out, x.Class)
		}
	}
	return out
}

// documented reads only the reasons in classes; any other is unknown.
func documented(classes map[string]types.ProblemClass) func(string) verdict {
	return func(reason string) verdict {
		class, ok := classes[reason]
		return verdict{class: class, known: ok}
	}
}

// anyReason is for a condition whose type alone names the fault and whose
// source lists no reasons.
func anyReason(class types.ProblemClass) func(string) verdict {
	return func(string) verdict { return verdict{class: class, known: true} }
}

var (
	eksXIDReason  = regexp.MustCompile(`^NvidiaXID(\d+)Error$`)
	eksDCGMReason = regexp.MustCompile(`^(DCGMDiagnosticFailure|DCGMError|DCGMHealthCode\d+)$`)
)

// eksXIDCodes are the XIDs the EKS node monitoring agent's table lists as
// NvidiaXID<code>Error.
var eksXIDCodes = map[int]bool{
	46: true, 48: true, 54: true, 62: true, 63: true, 64: true, 74: true, 79: true, 95: true, 109: true, 110: true,
	119: true, 120: true, 136: true, 140: true, 142: true, 143: true, 151: true, 155: true, 156: true, 158: true,
}

// eksNVIDIAReason maps the EKS node monitoring agent's documented NVIDIA
// reasons to the class the same fault has when the agent reads it. DCGM's
// own reasons are read and not acted on: the table gives them no repair.
func eksNVIDIAReason(reason string) verdict {
	switch reason {
	case "NvidiaDoubleBitError":
		return verdict{class: types.ClassECCDBE, known: true}
	case "NvidiaNVLinkError":
		return verdict{class: types.ClassNVLink, known: true}
	}
	if eksDCGMReason.MatchString(reason) {
		return verdict{known: true, ignored: "DCGM reports its own failure or a diagnostic result; the EKS documentation gives it no repair action"}
	}
	m := eksXIDReason.FindStringSubmatch(reason)
	if m == nil {
		return verdict{}
	}
	code, err := strconv.Atoi(m[1])
	if err != nil || !eksXIDCodes[code] {
		return verdict{}
	}
	if info, ok := detect.ClassifyXID(code); ok {
		return verdict{class: info.Class, known: true}
	}
	return verdict{class: types.ClassAcceleratorFault, known: true}
}

// Evaluate reads one Node with every profile that applies to it.
func Evaluate(snap types.NodeHealthSnapshot) NodeHealth {
	out := NodeHealth{Node: snap.Node, State: StateHealthy}
	for _, p := range Profiles {
		devices := p.devices(snap)
		if len(devices) == 0 {
			continue
		}
		out.Profiles = append(out.Profiles, p.Name)
		obs := p.evaluate(snap, devices)
		out.Observations = append(out.Observations, obs...)
	}
	for _, o := range out.Observations {
		switch {
		case o.State == StateFault:
			out.State = StateFault
		case o.State == StateUnknown && out.State != StateFault:
			out.State = StateUnknown
		}
	}
	if len(out.Profiles) == 0 {
		out.State = ""
	}
	out.OtherRepairers = otherRepairers(snap)
	out.Slice = multiHostSlice(snap)
	return out
}

func (p Profile) devices(snap types.NodeHealthSnapshot) []types.DeviceCount {
	var out []types.DeviceCount
	for _, d := range snap.Devices {
		for _, r := range p.Resources {
			if d.Resource == r {
				out = append(out, d)
			}
		}
	}
	return out
}

func (p Profile) evaluate(snap types.NodeHealthSnapshot, devices []types.DeviceCount) []Observation {
	var out []Observation
	read := false
	for _, rule := range p.conditions {
		cond, ok := findCondition(snap.Conditions, rule.condition)
		if !ok {
			continue
		}
		if rule.ignored != "" {
			if cond.Status == rule.faultStatus {
				out = append(out, Observation{Profile: p.Name, Rule: rule.id, State: StateIgnored, Key: "condition:" + cond.Type,
					Reason: cond.Reason, Since: cond.LastTransitionTime, Why: rule.ignored})
			}
			continue
		}
		read = true
		out = append(out, p.readCondition(rule, cond, snap.ReadAt))
	}
	if !read && len(p.conditions) > 0 {
		names := make([]string, 0, len(p.conditions))
		for _, rule := range p.conditions {
			if rule.ignored == "" {
				names = append(names, rule.condition)
			}
		}
		out = append(out, Observation{Profile: p.Name, Rule: "conditions", State: StateUnknown, Key: "conditions",
			Why: "the node reports none of the conditions " + strings.Join(uniq(names), ", ")})
	}
	for _, d := range devices {
		o := Observation{Profile: p.Name, Rule: "allocatable", Key: "allocatable:" + d.Resource, Hold: unallocatableHold, Reads: confirmReads,
			Reason: fmt.Sprintf("%d of %d allocatable", d.Allocatable, d.Capacity)}
		switch {
		case d.Capacity <= 0:
			// No count to compare: neither a fault nor healthy, so a
			// pending unallocatable fault is held, not cleared.
			o.State, o.Why = StateUnknown, "the node lists "+d.Resource+" at zero"
		case d.Allocatable < d.Capacity:
			o.State, o.Class = StateFault, types.ClassAcceleratorUnallocatable
		default:
			o.State = StateHealthy
		}
		out = append(out, o)
	}
	return out
}

func (p Profile) readCondition(rule conditionRule, cond types.NodeHealthCondition, readAt time.Time) Observation {
	o := Observation{Profile: p.Name, Rule: rule.id, Key: "condition:" + cond.Type, Reason: cond.Reason,
		Since: cond.LastTransitionTime, Reads: confirmReads}
	healthyStatus := "False"
	if rule.faultStatus == "False" {
		healthyStatus = "True"
	}
	switch {
	case cond.LastHeartbeatTime.IsZero():
		o.State, o.Why = StateUnknown, "the condition "+cond.Type+" has no heartbeat time"
	case readAt.Sub(cond.LastHeartbeatTime) > conditionFreshness:
		o.State = StateUnknown
		o.Why = fmt.Sprintf("the condition %s is stale: heartbeat %s old, more than %s",
			cond.Type, readAt.Sub(cond.LastHeartbeatTime).Truncate(time.Second), conditionFreshness)
	case cond.Status == rule.faultStatus:
		v := rule.reasons(cond.Reason)
		switch {
		case !v.known:
			o.State = StateUnknown
			o.Why = fmt.Sprintf("reason %q of the condition %s is not in profile table %s", cond.Reason, cond.Type, Version)
		case v.ignored != "":
			o.State, o.Why = StateIgnored, v.ignored
		default:
			o.State, o.Class = StateFault, v.class
			o.Key += "=" + cond.Status
		}
	case cond.Status == healthyStatus:
		o.State = StateHealthy
	default:
		o.State, o.Why = StateUnknown, fmt.Sprintf("the condition %s has status %q", cond.Type, cond.Status)
	}
	return o
}

func findCondition(conds []types.NodeHealthCondition, name string) (types.NodeHealthCondition, bool) {
	for _, c := range conds {
		if c.Type == name {
			return c, true
		}
	}
	return types.NodeHealthCondition{}, false
}

// repairerTaints are taints a component that repairs nodes on its own puts on
// a node it is acting on:
//   - amd-gpu-unhealthy, the AMD GPU Operator's default remediation taint,
//     https://instinct.docs.amd.com/projects/gpu-operator/en/latest/autoremediation/auto-remediation.html
//   - karpenter.sh/disrupted, Karpenter's taint on a node it disrupts,
//     https://karpenter.sh/docs/concepts/disruption/
//
// (2026-11-18)
var repairerTaints = map[string]string{
	"amd-gpu-unhealthy":      "amd-gpu-operator",
	"karpenter.sh/disrupted": "karpenter",
}

func otherRepairers(snap types.NodeHealthSnapshot) []string {
	var out []string
	for _, t := range snap.Taints {
		if name, ok := repairerTaints[t.Key]; ok {
			out = append(out, name)
		}
	}
	return uniq(out)
}

const (
	labelTPUTopology = "cloud.google.com/gke-tpu-topology"
	labelGKENodePool = "cloud.google.com/gke-nodepool"
)

// multiHostSlice is the node pool of the TPU slice the node is one host of,
// when the slice's topology holds more chips than the node: replacing one
// such node alone is not a recovery. A node with a topology and no TPU count
// (a failed chip can take it to zero) may be one host of many, and is
// taken to be.
func multiHostSlice(snap types.NodeHealthSnapshot) string {
	topology, ok := snap.Labels[labelTPUTopology]
	if !ok {
		return ""
	}
	chips := int64(1)
	for _, dim := range strings.Split(topology, "x") {
		n, err := strconv.ParseInt(dim, 10, 64)
		if err != nil || n <= 0 {
			return ""
		}
		chips *= n
	}
	var perNode int64
	for _, d := range snap.Devices {
		if d.Resource == "google.com/tpu" {
			perNode = d.Capacity
		}
	}
	if perNode > 0 && chips <= perNode {
		return ""
	}
	if pool := snap.Labels[labelGKENodePool]; pool != "" {
		return pool
	}
	return "unnamed"
}

func uniq(in []string) []string {
	var out []string
	seen := map[string]bool{}
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}
