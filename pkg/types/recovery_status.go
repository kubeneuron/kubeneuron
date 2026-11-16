// SPDX-License-Identifier: Apache-2.0

package types

import "time"

// RecoveryAvailability is the last recorded eligibility assessment, not a
// current authorization or evidence that recovery succeeded.
type RecoveryAvailability struct {
	Status     string    `json:"status"`
	Reason     string    `json:"reason"`
	ObservedAt time.Time `json:"observed_at"`
}

// RecoveryAvailabilityFromAudit projects persisted assessments in audit order.
// No assessment is unknown; it must never be rendered as available.
func RecoveryAvailabilityFromAudit(trail []*AuditEntry) *RecoveryAvailability {
	for i := len(trail) - 1; i >= 0; i-- {
		e := trail[i]
		if e == nil {
			continue
		}
		status := ""
		switch e.Action {
		case "recovery-unavailable":
			status = "unavailable"
		case "recovery-available":
			status = "eligible"
		}
		if status != "" {
			return &RecoveryAvailability{Status: status, Reason: e.Result, ObservedAt: e.Time}
		}
	}
	return nil
}
