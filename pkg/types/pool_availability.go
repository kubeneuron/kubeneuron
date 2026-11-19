// SPDX-License-Identifier: Apache-2.0

package types

import "time"

const PoolAvailabilityPath = "/api/v1/pools"

// PoolAvailability describes the leader's latest assessment of an accelerator pool.
type PoolAvailability struct {
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
}
