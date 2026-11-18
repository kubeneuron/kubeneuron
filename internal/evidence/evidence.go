// SPDX-License-Identifier: Apache-2.0

// Package evidence builds the incident evidence block from what the Kubernetes
// API shows about a node: where the fault most likely is, the vendor software
// state and the node's network resource. Everything here is shown and recorded;
// nothing here starts, authorises or skips a step.
package evidence

import (
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/kubeneuron/kubeneuron/pkg/types"
)

// Window is how close in time incidents of one class on several nodes must
// open to be read as one shared cause.
const Window = 10 * time.Minute

// Component is a vendor component pod recognised by one of its labels.
type Component struct {
	Name string
	// Values are matched against the pod's app, name, k8s-app and
	// app.kubernetes.io/name labels.
	Values []string
	// Required components must run for the software state to be ok: without
	// them pods cannot be given the device.
	Required bool
}

// acceleratorResources are the extended resources a device plugin advertises
// for an accelerator, by prefix.
var acceleratorResources = []string{
	"nvidia.com/gpu",
	"nvidia.com/mig-",
	"amd.com/gpu",
	"aws.amazon.com/neuron",
	"google.com/tpu",
	"gpu.intel.com/",
}

// softwareComponents are the NVIDIA GPU Operator's component pods, the
// device plugin and driver installer GKE manages (k8s-app
// nvidia-gpu-device-plugin and nvidia-driver-installer in kube-system), and
// the standalone device plugins of the other vendors. Only the device plugin
// is required: the driver and the toolkit may come with the node image, and
// the others do not decide whether a pod can see the device.
var softwareComponents = []Component{
	{Name: "driver", Values: []string{"nvidia-driver-daemonset", "nvidia-driver-installer"}},
	{Name: "container-toolkit", Values: []string{"nvidia-container-toolkit-daemonset"}},
	{Name: "device-plugin", Required: true, Values: []string{
		"nvidia-device-plugin-daemonset", "nvidia-device-plugin", "nvidia-device-plugin-ds", "nvidia-gpu-device-plugin",
		"amdgpu-dp-ds", "amdgpu-device-plugin", "neuron-device-plugin-ds", "neuron-device-plugin",
		"intel-gpu-plugin",
	}},
	{Name: "feature-discovery", Values: []string{"gpu-feature-discovery"}},
	{Name: "dcgm", Values: []string{"nvidia-dcgm"}},
	{Name: "dcgm-exporter", Values: []string{"nvidia-dcgm-exporter"}},
	{Name: "validator", Values: []string{"nvidia-operator-validator"}},
}

// networkResources are high-performance network resources a node may
// advertise, by prefix, and networkPlugins the device plugins that advertise
// them.
var (
	networkResources = []string{"vpc.amazonaws.com/efa", "rdma/"}
	networkPlugins   = Component{Name: "network-device-plugin", Values: []string{
		"aws-efa-k8s-device-plugin", "aws-efa-k8s-device-plugin-daemonset", "rdma-shared-dp", "rdma-shared-dp-ds",
	}}
)

// Pod is what the evidence needs of one pod on the node: its labels and
// state, never its name.
type Pod struct {
	Labels map[string]string
	// Running is phase Running; Ready is the Ready condition True.
	Running, Ready bool
}

// Node is what the evidence needs of the node. A nil Node, or PodsErr set,
// is an unread signal.
type Node struct {
	Capacity, Allocatable map[string]int64
	Pods                  []Pod
	PodsErr               error
}

// Software summarises the vendor software state.
func Software(n *Node) types.SoftwareEvidence {
	if n == nil {
		return types.SoftwareEvidence{State: types.EvidenceUnknown, Reason: "the node could not be read"}
	}
	out := types.SoftwareEvidence{Resources: resources(n, acceleratorResources)}
	for _, r := range out.Resources {
		if r.Allocatable < r.Capacity {
			out.State = types.EvidenceDegraded
			out.Reason = fmt.Sprintf("%s: %d of %d devices allocatable; pods cannot use the rest", r.Name, r.Allocatable, r.Capacity)
		}
	}
	if n.PodsErr != nil {
		out.State, out.Reason = worse(out.State, out.Reason, types.EvidenceUnknown, "the pods on the node could not be read")
		return out
	}
	for _, c := range softwareComponents {
		obs, found := component(n.Pods, c)
		if !found {
			if c.Required {
				out.Components = append(out.Components, obs)
				out.State, out.Reason = worse(out.State, out.Reason, types.EvidenceUnknown, "no device plugin pod recognised on the node")
			}
			continue
		}
		out.Components = append(out.Components, obs)
		if obs.State != types.ComponentRunning {
			out.State, out.Reason = worse(out.State, out.Reason, types.EvidenceDegraded, fmt.Sprintf("the %s pod is %s", c.Name, obs.State))
		}
	}
	if len(out.Resources) == 0 {
		out.State, out.Reason = worse(out.State, out.Reason, types.EvidenceDegraded, "the node advertises no accelerator resource")
	}
	if out.State == "" {
		out.State = types.EvidenceOK
	}
	return out
}

// Network summarises the node's high-performance network resource: present
// when advertised and allocatable, missing when its plugin runs or its
// capacity is there but nothing is allocatable, unknown otherwise: a node
// without either may simply have none.
func Network(n *Node) types.NetworkEvidence {
	if n == nil {
		return types.NetworkEvidence{State: types.EvidenceUnknown, Reason: "the node could not be read"}
	}
	out := types.NetworkEvidence{Resources: resources(n, networkResources)}
	if n.PodsErr == nil {
		if obs, found := component(n.Pods, networkPlugins); found {
			out.Plugin = &obs
		}
	}
	allocatable := false
	for _, r := range out.Resources {
		if r.Allocatable > 0 {
			allocatable = true
		}
	}
	switch {
	case allocatable:
		out.State = types.EvidencePresent
	case len(out.Resources) > 0:
		out.State, out.Reason = types.EvidenceMissing, "the network resource has capacity but none is allocatable"
	case out.Plugin != nil:
		out.State, out.Reason = types.EvidenceMissing, "the network device plugin runs but the node advertises no network resource"
	case n.PodsErr != nil:
		out.State, out.Reason = types.EvidenceUnknown, "the pods on the node could not be read"
	default:
		out.State, out.Reason = types.EvidenceUnknown, "the node advertises no high-performance network resource and runs no plugin for one"
	}
	return out
}

// Peer is another open incident considered for grouping. Group is the group
// it already belongs to, if any.
type Peer struct {
	ID       string
	Node     string
	Class    types.ProblemClass
	OpenedAt time.Time
	Group    string
}

// deviceClasses are faults the node's own accelerator reports about itself.
var deviceClasses = map[types.ProblemClass]bool{
	types.ClassECCDBE: true, types.ClassECCSBERate: true, types.ClassECCContained: true,
	types.ClassRowRemapOK: true, types.ClassRowRemapFailure: true, types.ClassRowRemapBudget: true,
	types.ClassNVLink: true, types.ClassFellOffBus: true, types.ClassGSPError: true,
	types.ClassThermal: true, types.ClassPower: true, types.ClassPCIe: true,
	types.ClassDriverHang: true, types.ClassDiagFailure: true,
}

// unknownClasses never place a fault on the node by themselves.
var unknownClasses = map[types.ProblemClass]string{
	types.ClassXIDApp:       "an application XID is usually the workload, not the node",
	types.ClassAgentDown:    "the agent stopped reporting; that says nothing about the device",
	types.ClassExporterDown: "the metrics exporter stopped reporting; that says nothing about the device",
}

// Locate places the fault of an incident of class on node opened at, given
// the other open incidents and the node's software state. The rule, per
// class:
//
//   - the same class on another node within Window: shared. The group is the
//     one a member already belongs to, else the earliest incident; once set
//     it does not change, even when that incident closes;
//   - a device fault (ECC, row remap, NVLink, fell off the bus, GSP, thermal,
//     power, PCIe, driver hang, diagnostic failure): this node;
//   - a lost GPU: this node only while the vendor software is ok, since a
//     device plugin that stopped also loses the device for pods;
//   - an application XID, a silent agent or exporter, any other class:
//     unknown.
func Locate(id string, class types.ProblemClass, node string, at time.Time, peers []Peer, software types.SoftwareEvidence) types.LocationEvidence {
	group := []Peer{{ID: id, Node: node, Class: class, OpenedAt: at}}
	nodes := map[string]bool{node: true}
	for _, p := range peers {
		if p.ID == id || p.Class != class || p.Node == "" || p.Node == node {
			continue
		}
		if d := p.OpenedAt.Sub(at); d > Window || d < -Window {
			continue
		}
		group = append(group, p)
		nodes[p.Node] = true
	}
	if len(nodes) > 1 {
		sort.Slice(group, func(i, j int) bool {
			if !group[i].OpenedAt.Equal(group[j].OpenedAt) {
				return group[i].OpenedAt.Before(group[j].OpenedAt)
			}
			return group[i].ID < group[j].ID
		})
		id := group[0].ID
		for _, p := range group {
			if p.Group != "" {
				id = p.Group
				break
			}
		}
		members := make([]string, 0, len(group))
		names := make([]string, 0, len(nodes))
		for _, p := range group {
			members = append(members, p.ID)
		}
		for n := range nodes {
			names = append(names, n)
		}
		slices.Sort(names)
		return types.LocationEvidence{
			Location: types.LocationShared,
			Rule:     fmt.Sprintf("shared: %d nodes, same class, within %d min", len(nodes), int(Window.Minutes())),
			Evidence: []string{"nodes: " + strings.Join(names, ", ")},
			Group:    id,
			Members:  members,
		}
	}
	switch {
	case deviceClasses[class]:
		return types.LocationEvidence{Location: types.LocationThisNode, Rule: "evidence points to this node: its accelerator reported a device fault, and no other node the same class"}
	case class == types.ClassGPULost && software.State == types.EvidenceOK:
		return types.LocationEvidence{Location: types.LocationThisNode, Rule: "evidence points to this node: fewer devices than the inventory while the vendor software is ok, and no other node the same class"}
	case class == types.ClassGPULost:
		return types.LocationEvidence{Location: types.LocationUnknown, Rule: "fewer devices than the inventory while the vendor software is " + string(software.State) + "; pods may not see a device that is there",
			Evidence: nonEmpty(software.Reason)}
	}
	if why, ok := unknownClasses[class]; ok {
		return types.LocationEvidence{Location: types.LocationUnknown, Rule: why}
	}
	return types.LocationEvidence{Location: types.LocationUnknown, Rule: "no rule places this class on the node"}
}

// Build assembles the evidence block for an incident.
func Build(inc *types.Incident, n *Node, peers []Peer, now time.Time) types.IncidentEvidence {
	sw := Software(n)
	return types.IncidentEvidence{
		ObservedAt: now.UTC(),
		Location:   Locate(inc.ID, inc.Class, inc.Target.Node, inc.OpenedAt, peers, sw),
		Software:   sw,
		Network:    Network(n),
	}
}

func resources(n *Node, prefixes []string) []types.ResourceObservation {
	var out []types.ResourceObservation
	for name, capacity := range n.Capacity {
		if capacity <= 0 || !hasPrefix(name, prefixes) {
			continue
		}
		out = append(out, types.ResourceObservation{Name: name, Capacity: capacity, Allocatable: n.Allocatable[name]})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func hasPrefix(name string, prefixes []string) bool {
	for _, p := range prefixes {
		if name == p || strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

var componentLabels = []string{"app", "name", "k8s-app", "app.kubernetes.io/name"}

// component finds the component's pod on the node; a running and ready one
// wins over one that is not.
func component(pods []Pod, c Component) (types.ComponentObservation, bool) {
	out := types.ComponentObservation{Name: c.Name, State: types.ComponentMissing}
	found := false
	for _, pod := range pods {
		if !matches(pod.Labels, c.Values) {
			continue
		}
		state := types.ComponentNotReady
		if pod.Running && pod.Ready {
			state = types.ComponentRunning
		}
		if !found || state == types.ComponentRunning {
			out.State = state
		}
		found = true
	}
	return out, found
}

func matches(labels map[string]string, values []string) bool {
	for _, key := range componentLabels {
		if v, ok := labels[key]; ok && slices.Contains(values, v) {
			return true
		}
	}
	return false
}

// worse keeps the first degraded reason: degraded outranks unknown, which
// outranks ok.
func worse(state types.EvidenceState, reason string, next types.EvidenceState, why string) (types.EvidenceState, string) {
	rank := map[types.EvidenceState]int{"": 0, types.EvidenceOK: 0, types.EvidenceUnknown: 1, types.EvidenceDegraded: 2}
	if rank[next] > rank[state] {
		return next, why
	}
	return state, reason
}

func nonEmpty(s string) []string {
	if s == "" {
		return nil
	}
	return []string{s}
}

// AuditAction is the audit action of an evidence row.
const AuditAction = "evidence"

// Latest returns the last evidence block of an audit trail, with the fault
// location of a later regrouping row. A row that does not decode is skipped:
// the earlier block stands, and nothing is invented in its place. Nil when
// none was recorded.
func Latest(audit []*types.AuditEntry) *types.IncidentEvidence {
	var out *types.IncidentEvidence
	var location *types.LocationEvidence
	for i := len(audit) - 1; i >= 0 && out == nil; i-- {
		entry := audit[i]
		if entry == nil || entry.Action != AuditAction {
			continue
		}
		if raw, ok := entry.Params["fault_location"]; ok && location == nil {
			var loc types.LocationEvidence
			if json.Unmarshal([]byte(raw), &loc) == nil && loc.Location != "" {
				location = &loc
			}
		}
		if raw, ok := entry.Params["incident_evidence"]; ok {
			var ev types.IncidentEvidence
			if json.Unmarshal([]byte(raw), &ev) == nil && !ev.ObservedAt.IsZero() {
				out = &ev
			}
		}
	}
	if out != nil && location != nil {
		out.Location = *location
	}
	return out
}
