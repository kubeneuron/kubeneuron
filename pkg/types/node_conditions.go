// SPDX-License-Identifier: Apache-2.0

package types

import "time"

// NodeConditionSnapshot records Kubernetes observations for incident context.
type NodeConditionSnapshot struct {
	NodeUID    string                     `json:"node_uid"`
	ObservedAt time.Time                  `json:"observed_at"`
	Conditions []NodeConditionObservation `json:"conditions"`
}

type NodeConditionObservation struct {
	Type               string     `json:"type"`
	Status             string     `json:"status"`
	LastTransitionTime *time.Time `json:"last_transition_time,omitempty"`
}
