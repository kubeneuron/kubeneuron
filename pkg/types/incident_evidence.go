// SPDX-License-Identifier: Apache-2.0

package types

import "time"

// FaultLocation says where the cause of an incident most likely is.
type FaultLocation string

const (
	// LocationThisNode: a device fault the node's own accelerator reported,
	// with no other node showing the same class at the same time.
	LocationThisNode FaultLocation = "this-node"
	// LocationShared: the same class on several nodes within a short window,
	// which looks like one cause (a driver rollout, a fabric, monitoring)
	// rather than several broken machines.
	LocationShared FaultLocation = "shared"
	// LocationUnknown: nothing says which. Never read as "this node".
	LocationUnknown FaultLocation = "unknown"
)

// EvidenceState is the state of one evidence row. Unknown is a missing or
// unreadable signal and is never shown as healthy.
type EvidenceState string

const (
	EvidenceOK       EvidenceState = "ok"
	EvidenceDegraded EvidenceState = "degraded"
	EvidencePresent  EvidenceState = "present"
	EvidenceMissing  EvidenceState = "missing"
	EvidenceUnknown  EvidenceState = "unknown"
)

// Component pod states.
const (
	ComponentRunning  = "running"
	ComponentNotReady = "not-ready"
	ComponentMissing  = "missing"
)

// IncidentEvidence is what KubeNeuron read from the Kubernetes API about an
// incident's node, and where it places the fault. It is shown, recorded in the
// audit trail and never starts or authorises a step.
type IncidentEvidence struct {
	ObservedAt time.Time        `json:"observed_at"`
	Location   LocationEvidence `json:"fault_location"`
	Software   SoftwareEvidence `json:"vendor_software"`
	Network    NetworkEvidence  `json:"network_resource"`
}

// LocationEvidence is the fault location, an estimate by rule and not a
// diagnosis, with the rule and the evidence that led to it. For a shared
// location, Group is the incident that started the group, fixed once set,
// and Members the incidents of the group within the window. Only node names,
// the class and incident IDs are recorded.
type LocationEvidence struct {
	Location FaultLocation `json:"location"`
	Rule     string        `json:"rule"`
	Evidence []string      `json:"evidence,omitempty"`
	Group    string        `json:"group,omitempty"`
	Members  []string      `json:"members,omitempty"`
}

// SoftwareEvidence is the vendor software on the node: the accelerator
// resource it advertises against what the scheduler may allocate, and the
// vendor's component pods. Degraded means the device may be healthy while pods
// cannot use it, which is a software state, not a hardware fault.
type SoftwareEvidence struct {
	State      EvidenceState          `json:"state"`
	Reason     string                 `json:"reason,omitempty"`
	Resources  []ResourceObservation  `json:"resources,omitempty"`
	Components []ComponentObservation `json:"components,omitempty"`
}

// NetworkEvidence is whether the node advertises its high-performance network
// resource, and the state of that resource's device plugin pod. It is not a
// fabric diagnosis.
type NetworkEvidence struct {
	State     EvidenceState         `json:"state"`
	Reason    string                `json:"reason,omitempty"`
	Resources []ResourceObservation `json:"resources,omitempty"`
	Plugin    *ComponentObservation `json:"plugin,omitempty"`
}

// ResourceObservation is one extended resource of the node.
type ResourceObservation struct {
	Name        string `json:"name"`
	Capacity    int64  `json:"capacity"`
	Allocatable int64  `json:"allocatable"`
}

// ComponentObservation is the state of one vendor component's pod on the
// node. The pod's name is not recorded.
type ComponentObservation struct {
	Name  string `json:"name"`
	State string `json:"state"`
}
