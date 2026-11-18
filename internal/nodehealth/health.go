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
	// Device is the device index when the signal names one.
	Device string
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
	// any profile has no fresh signal, otherwise healthy.
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
// fault, classified from its reason; at the other of True/False it is
// healthy; at any other status it is unknown.
type conditionRule struct {
	id          string
	condition   string
	faultStatus string
	classify    func(reason string) types.ProblemClass
}

// labelRule reads the labels whose key matches pattern; the first capture
// group, when there is one, is the device index. A value that is neither a
// fault nor healthy is unknown.
type labelRule struct {
	id      string
	pattern *regexp.Regexp
	fault   map[string]bool
	healthy map[string]bool
}

// Profile is one accelerator family's signals.
type Profile struct {
	Name string
	// Resources are the extended resources that select the profile: a node
	// advertising one of them is read with it.
	Resources  []string
	conditions []conditionRule
	labels     []labelRule
}

// Profiles is the built-in table. Signal names are taken from vendor
// documentation on 2026-11-18 and are not validated on real hardware. Only
// names a public document confirms are listed.
var Profiles = []Profile{
	{
		Name:      "amd-instinct",
		Resources: []string{"amd.com/gpu"},
		labels: []labelRule{{
			// AMD Device Metrics Exporter: a label per unhealthy GPU.
			id:      "device-metrics-exporter",
			pattern: regexp.MustCompile(`^metricsexporter\.amd\.com\.gpu\.(\d+)\.state$`),
			fault:   map[string]bool{"unhealthy": true},
			healthy: map[string]bool{"healthy": true},
		}},
		conditions: []conditionRule{
			// AMD's Node Problem Detector integration.
			{id: "npd-amdgpu-problem", condition: "AMDGPUProblem", faultStatus: "True", classify: fault},
			{id: "npd-amdgpu-down", condition: "AMDGPUIsDown", faultStatus: "True", classify: fault},
		},
	},
	{
		Name:      "aws-neuron",
		Resources: []string{"aws.amazon.com/neuron", "aws.amazon.com/neurondevice"},
		conditions: []conditionRule{
			// The Neuron node problem detector in monitor-only mode.
			{id: "neuron-npd", condition: "NeuronHealth", faultStatus: "True", classify: fault},
			// The EKS node monitoring agent.
			{id: "eks-node-monitoring", condition: "AcceleratedHardwareReady", faultStatus: "False", classify: fault},
		},
	},
	{
		Name:      "nvidia",
		Resources: []string{"nvidia.com/gpu"},
		conditions: []conditionRule{
			// The EKS node monitoring agent.
			{id: "eks-node-monitoring", condition: "AcceleratedHardwareReady", faultStatus: "False", classify: eksNVIDIAReason},
			// The AKS Node Problem Detector GPU checks.
			{id: "aks-npd-gpu-missing", condition: "GPUMissing", faultStatus: "True", classify: always(types.ClassGPULost)},
			{id: "aks-npd-nvlink-down", condition: "NVLinkDown", faultStatus: "True", classify: always(types.ClassNVLink)},
			{id: "aks-npd-gpu-ecc", condition: "GPUECCError", faultStatus: "True", classify: fault},
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

func fault(string) types.ProblemClass { return types.ClassAcceleratorFault }

func always(class types.ProblemClass) func(string) types.ProblemClass {
	return func(string) types.ProblemClass { return class }
}

var eksXIDReason = regexp.MustCompile(`^NvidiaXID(\d+)Error$`)

// eksNVIDIAReason maps the EKS node monitoring agent's NVIDIA reasons to the
// class the same fault has when the agent reads it.
func eksNVIDIAReason(reason string) types.ProblemClass {
	switch reason {
	case "NvidiaDoubleBitError":
		return types.ClassECCDBE
	case "NvidiaNVLinkError":
		return types.ClassNVLink
	}
	if m := eksXIDReason.FindStringSubmatch(reason); m != nil {
		if code, err := strconv.Atoi(m[1]); err == nil {
			if info, ok := detect.ClassifyXID(code); ok {
				return info.Class
			}
		}
	}
	return types.ClassAcceleratorFault
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
			if d.Resource == r && d.Capacity > 0 {
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
		read = true
		out = append(out, p.readCondition(rule, cond, snap.ReadAt))
	}
	for _, rule := range p.labels {
		for key, value := range snap.Labels {
			m := rule.pattern.FindStringSubmatch(key)
			if m == nil {
				continue
			}
			o := Observation{Profile: p.Name, Rule: rule.id, Key: "label:" + key, Reason: value, Reads: confirmReads}
			if len(m) > 1 {
				o.Device = m[1]
			}
			switch {
			case rule.fault[value]:
				o.State, o.Class = StateFault, types.ClassAcceleratorFault
			case rule.healthy[value]:
				o.State = StateHealthy
			default:
				o.State, o.Why = StateUnknown, fmt.Sprintf("the node reports the device state as %q", value)
			}
			out = append(out, o)
		}
	}
	if !read && len(p.conditions) > 0 {
		names := make([]string, 0, len(p.conditions))
		for _, rule := range p.conditions {
			names = append(names, rule.condition)
		}
		out = append(out, Observation{Profile: p.Name, Rule: "conditions", State: StateUnknown, Key: "conditions",
			Why: "the node reports none of the conditions " + strings.Join(uniq(names), ", ")})
	}
	for _, d := range devices {
		o := Observation{Profile: p.Name, Rule: "allocatable", Key: "allocatable:" + d.Resource, Hold: unallocatableHold, Reads: confirmReads,
			Reason: fmt.Sprintf("%d of %d allocatable", d.Allocatable, d.Capacity)}
		if d.Allocatable < d.Capacity {
			o.State, o.Class = StateFault, types.ClassAcceleratorUnallocatable
		} else {
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
		o.State, o.Class = StateFault, rule.classify(cond.Reason)
		o.Key += "=" + cond.Status
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
// a node it is acting on.
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
// such node alone is not a recovery.
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
	if perNode <= 0 || chips <= perNode {
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
