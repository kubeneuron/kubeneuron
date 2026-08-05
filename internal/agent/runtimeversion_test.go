package agent

import "testing"

// The agent's own readiness verdict must not disagree with the controller's
// gate: a report the agent calls ready and the gate rejects (or the reverse) is
// a confusing failure with no single place to look.
func TestAgentRuntimeVersionMatchesTheControllerRule(t *testing.T) {
	for _, tc := range []struct {
		attested, pinned string
		want             bool
	}{
		{"dcgm-4.6.1", "dcgm-4.6.1", true},
		{"dcgm-4.6.2", "dcgm-4.6.1", true},
		{"dcgm-4.6.1", "dcgm-4.6", true},
		{"dcgm-4.7.0", "dcgm-4.6.1", false},
		{"dcgm-3.3.0", "dcgm-4.1", false},
		{"rocm-4.6.1", "dcgm-4.6.1", false},
		{"", "dcgm-4.6.1", false},
		{"dcgm-4.6.1", "", false},
	} {
		if got := runtimeVersionMatchesProfile(tc.attested, tc.pinned); got != tc.want {
			t.Errorf("runtimeVersionMatchesProfile(%q, %q) = %v, want %v",
				tc.attested, tc.pinned, got, tc.want)
		}
	}
}
