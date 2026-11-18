// SPDX-License-Identifier: Apache-2.0

package types

import "time"

// AcceleratorHealthPath serves what the controller last read from every
// accelerator health source, one row per node, source and signal.
const AcceleratorHealthPath = "/api/v1/accelerator-health"

// States of an accelerator health row besides healthy, fault, unknown and
// ignored, which are those of the health profiles.
const (
	// HealthRowNoSignal: the source lists the device and says nothing about
	// its health. That is not healthy.
	HealthRowNoSignal = "no-signal"
	// HealthRowNotReadThisTerm: the source has not been read since this
	// controller became leader. Until it is, no step that takes a node out
	// of service runs on any node.
	HealthRowNotReadThisTerm = "not-read-this-term"
	// HealthRowAvailable: the agent reads the source on the node. A fault it
	// reads opens an incident; no fault read is not health.
	HealthRowAvailable = "available"
)

// AcceleratorHealthView is the controller's last read of every accelerator
// health source. It is held in memory by the elected leader and starts empty
// in each leadership term.
type AcceleratorHealthView struct {
	// ProfileVersion is the version of the built-in profile table.
	ProfileVersion string                 `json:"profile_version"`
	Rows           []AcceleratorHealthRow `json:"rows"`
	// Truncated says rows were left out past the row limit.
	Truncated bool `json:"truncated,omitempty"`
}

// AcceleratorHealthRow is one signal of one source on one node.
type AcceleratorHealthRow struct {
	// Node is empty on a source row that is about every node.
	Node    string `json:"node,omitempty"`
	Source  string `json:"source"`
	Profile string `json:"profile,omitempty"`
	Rule    string `json:"rule,omitempty"`
	// Device is the device index or DRA device name, when the signal names
	// one.
	Device string `json:"device,omitempty"`
	State  string `json:"state"`
	// Class is the incident class a fault opens.
	Class ProblemClass `json:"class,omitempty"`
	// Reason is the condition's reason or the label's value, as the source
	// reports it, bounded.
	Reason string `json:"reason,omitempty"`
	// Why says why the row is not healthy, in KubeNeuron's words.
	Why    string    `json:"why,omitempty"`
	Since  time.Time `json:"since,omitzero"`
	ReadAt time.Time `json:"read_at,omitzero"`
}
