// SPDX-License-Identifier: Apache-2.0

package types

import "time"

const PoolAvailabilityPath = "/api/v1/pools"

// PoolAvailabilityView contains the most recent assessments of accelerator pools.
type PoolAvailabilityView struct {
	Pools []PoolAvailability `json:"pools"`
}

// PoolAvailability describes the leader's latest assessment of an accelerator pool.
type PoolAvailability struct {
	Zones             []string   `json:"zones,omitempty"`
	SuccessorZone     string     `json:"successor_zone,omitempty"`
	Pool              string     `json:"pool"`
	Cloud             string     `json:"cloud"`
	AcceleratorNodes  int        `json:"accelerator_nodes"`
	ReplacementPolicy string     `json:"replacement_policy"`
	CapabilityEnabled bool       `json:"capability_enabled"`
	Status            string     `json:"status,omitempty"`
	ReasonCode        string     `json:"reason_code,omitempty"`
	Reason            string     `json:"reason,omitempty"`
	Action            string     `json:"action,omitempty"`
	CheckedAt         *time.Time `json:"checked_at,omitempty"`
	// ReplacementStatus says whether a node of the pool could be replaced at
	// all, in any order: "available", "unavailable" (the operator must change
	// something first) or "unknown". Empty when no remediation policy
	// replaces these nodes. Status is the add-first assessment on top of it.
	ReplacementStatus     string `json:"replacement_status,omitempty"`
	ReplacementReasonCode string `json:"replacement_reason_code,omitempty"`
	ReplacementReason     string `json:"replacement_reason,omitempty"`
	ReplacementAction     string `json:"replacement_action,omitempty"`
}
