// SPDX-License-Identifier: Apache-2.0

package types

import "time"

// RecoveryAvailability is the last recorded eligibility assessment, not a
// current authorization or evidence that recovery succeeded.
type RecoveryAvailability struct {
	Source     string    `json:"source"`
	Status     string    `json:"status"`
	Reason     string    `json:"reason"`
	ObservedAt time.Time `json:"observed_at"`
}

// RecoveryAvailabilityFromAudit projects persisted assessments in audit order.
// No assessment is unknown; it must never be rendered as available.
func RecoveryAvailabilityFromAudit(trail []*AuditEntry) *RecoveryAvailability {
	seen := map[string]bool{}
	var eligible *RecoveryAvailability
	for i := len(trail) - 1; i >= 0; i-- {
		e := trail[i]
		if e == nil {
			continue
		}
		// Once verification begins or the incident closes, an earlier wait
		// for another repair no longer describes its current step.
		if e.Action == "verify-owner-replacement" || e.Action == "node-repair-released" || e.ToState.Terminal() {
			seen["node-repair"] = true
		}
		source, status := "", ""
		switch e.Action {
		case "wait-for-node-repair":
			source, status = "node-repair", "unavailable"
		case "provider-recovery-unavailable":
			source, status = "provider", "unavailable"
		case "provider-recovery-available":
			source, status = "provider", "eligible"
		case "recovery-unavailable":
			source, status = "policy", "unavailable"
		case "recovery-available", "recovery-available-again":
			source, status = "policy", "eligible"
		}
		if source == "" || seen[source] {
			continue
		}
		seen[source] = true
		assessment := &RecoveryAvailability{Source: source, Status: status, Reason: e.Result, ObservedAt: e.Time}
		// A satisfied condition cannot clear another condition's denial.
		if status == "unavailable" {
			return assessment
		}
		if eligible == nil {
			eligible = assessment
		}
	}
	return eligible
}
