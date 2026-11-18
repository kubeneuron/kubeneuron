// SPDX-License-Identifier: Apache-2.0

package types

import "time"

// NodeHealthSnapshot is what a Kubernetes Node object says about the node's
// accelerators at one read: the inputs of the health profiles. Condition
// messages are not part of it: they are free text the vendor writes, and
// they are not kept.
type NodeHealthSnapshot struct {
	Node       string                `json:"node"`
	UID        string                `json:"uid,omitempty"`
	ProviderID string                `json:"provider_id,omitempty"`
	Labels     map[string]string     `json:"labels,omitempty"`
	Taints     []NodeTaint           `json:"taints,omitempty"`
	Conditions []NodeHealthCondition `json:"conditions,omitempty"`
	Devices    []DeviceCount         `json:"devices,omitempty"`
	// ReadAt is when this controller read the Node.
	ReadAt time.Time `json:"read_at"`
	// CreatedAt is the Node's creation time.
	CreatedAt time.Time `json:"created_at,omitzero"`
	// Cloud is what the cloud's monitoring reports about the node's
	// instance, oldest first, when that read is turned on; CloudError is why
	// the read failed.
	Cloud      []CloudHealthPoint `json:"cloud,omitempty"`
	CloudError string             `json:"cloud_error,omitempty"`
}

// CloudHealthPoint is one sample of the instance's accelerator health from
// the cloud's monitoring.
type CloudHealthPoint struct {
	At       time.Time `json:"at"`
	Status   string    `json:"status"`
	Category string    `json:"category,omitempty"`
}

// NodeTaint is one taint of a Node.
type NodeTaint struct {
	Key    string `json:"key"`
	Value  string `json:"value,omitempty"`
	Effect string `json:"effect,omitempty"`
}

// NodeHealthCondition is one Node condition, without its message.
type NodeHealthCondition struct {
	Type               string    `json:"type"`
	Status             string    `json:"status"`
	Reason             string    `json:"reason,omitempty"`
	LastHeartbeatTime  time.Time `json:"last_heartbeat_time,omitzero"`
	LastTransitionTime time.Time `json:"last_transition_time,omitzero"`
}
