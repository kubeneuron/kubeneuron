package config

import "testing"

// Exact equality made every DCGM client bump a fleet-wide outage: the agent
// image ships a pinned client, so raising it degraded every installation whose
// profile still named the previous patch, with no change on the nodes at all.
func TestRuntimeVersionIgnoresPatchButNotMinor(t *testing.T) {
	for _, tc := range []struct {
		name             string
		attested, pinned string
		want             bool
	}{
		{"identical", "dcgm-4.6.1", "dcgm-4.6.1", true},
		{"newer patch", "dcgm-4.6.2", "dcgm-4.6.1", true},
		{"older patch", "dcgm-4.6.0", "dcgm-4.6.1", true},
		{"pinned without patch", "dcgm-4.6.1", "dcgm-4.6", true},
		{"pinned to a major only", "dcgm-4.6.1", "dcgm-4", true},
		// A newer minor was never reviewed against this profile, and this
		// profile is what admits a reset of real hardware.
		{"newer minor", "dcgm-4.7.0", "dcgm-4.6.1", false},
		{"older minor", "dcgm-4.5.2", "dcgm-4.6.1", false},
		{"different major", "dcgm-3.3.0", "dcgm-4.1", false},
		// A different runtime that happens to share a number must never satisfy
		// a DCGM profile.
		{"different vendor prefix", "rocm-4.6.1", "dcgm-4.6.1", false},
		{"no prefix at all", "4.6.1", "dcgm-4.6.1", false},
		{"empty attested", "", "dcgm-4.6.1", false},
		{"empty pinned", "dcgm-4.6.1", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := RuntimeVersionSatisfies(tc.attested, tc.pinned); got != tc.want {
				t.Fatalf("RuntimeVersionSatisfies(%q, %q) = %v, want %v",
					tc.attested, tc.pinned, got, tc.want)
			}
		})
	}
}
