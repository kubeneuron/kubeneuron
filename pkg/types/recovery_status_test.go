// SPDX-License-Identifier: Apache-2.0
package types

import "testing"

func TestRecoveryAvailabilityTracksLatestAssessment(t *testing.T) {
	if RecoveryAvailabilityFromAudit(nil) != nil {
		t.Fatal("missing assessment is not eligible")
	}
	trail := []*AuditEntry{{Action: "recovery-unavailable", Result: "unknown repair"}, {Action: "recovery-available", Result: "same pin eligible"}, {Action: "approve", Result: "yes"}}
	if got := RecoveryAvailabilityFromAudit(trail); got.Status != "eligible" {
		t.Fatalf("got %+v", got)
	}
	trail = append(trail, &AuditEntry{Action: "recovery-unavailable", Result: "identity changed"})
	if got := RecoveryAvailabilityFromAudit(trail); got.Status != "unavailable" || got.Reason != "identity changed" {
		t.Fatalf("got %+v", got)
	}
}

func TestProviderEligibilityDoesNotClearPolicyUnavailability(t *testing.T) {
	trail := []*AuditEntry{
		{Action: "recovery-unavailable", Result: "add-first unsupported"},
		{Action: "provider-recovery-unavailable", Result: "repair unknown"},
		{Action: "provider-recovery-available", Result: "pin eligible"},
	}
	got := RecoveryAvailabilityFromAudit(trail)
	if got.Status != "unavailable" || got.Source != "policy" || got.Reason != "add-first unsupported" {
		t.Fatalf("got %+v", got)
	}
	trail = append(trail, &AuditEntry{Action: "recovery-available", Result: "strategy available"})
	if got = RecoveryAvailabilityFromAudit(trail); got.Status != "eligible" {
		t.Fatalf("got %+v", got)
	}
	trail = append(trail, &AuditEntry{Action: "provider-recovery-unavailable", Result: "identity changed"}, &AuditEntry{Action: "recovery-available", Result: "strategy available"})
	if got = RecoveryAvailabilityFromAudit(trail); got.Status != "unavailable" || got.Source != "provider" {
		t.Fatalf("got %+v", got)
	}
}
