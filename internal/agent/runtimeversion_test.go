package agent

import (
	"testing"

	"github.com/kubeneuron/kubeneuron/internal/config"
)

// The agent's own readiness verdict must not disagree with the controller's
// gate: a report the agent calls degraded and the gate would have accepted is a
// reset that never happens, with a reason line that contradicts itself.
//
// It used to disagree. The agent carried its own copy of the rule, whose
// comment claimed to mirror the controller's, and the copy truncated BOTH sides
// to major.minor while the real rule compares only as many components as the
// profile actually pinned. The table below is the old one plus the single case
// that separates them — a profile pinning a bare major — which is exactly the
// case the old table omitted, so four rounds of green runs proved agreement on
// every input except the one where they differed.
//
// There is now one function. This test exists to keep it that way: if a second
// copy ever reappears here, the bare-major row is what catches it.
func TestAgentRuntimeVersionMatchesTheControllerRule(t *testing.T) {
	for _, tc := range []struct {
		attested, pinned string
		want             bool
		why              string
	}{
		{"dcgm-4.6.1", "dcgm-4.6.1", true, "exact"},
		{"dcgm-4.6.2", "dcgm-4.6.1", true, "patch is ignored"},
		{"dcgm-4.6.1", "dcgm-4.6", true, "pinned to minor"},
		{"dcgm-4.6.1", "dcgm-4", true,
			"pinned to a bare major: the documented way to opt into a looser rule. " +
				"The agent's deleted copy answered false here, so every report on such a " +
				"fleet was stamped degraded, no report was ever eligible, and every reset " +
				"was denied — after the ladder had already cordoned and drained the node"},
		{"dcgm-4.7.0", "dcgm-4.6.1", false, "minor must match; newer is not automatically reviewed"},
		{"dcgm-3.3.0", "dcgm-4.1", false, "major differs"},
		{"rocm-4.6.1", "dcgm-4.6.1", false, "different runtime entirely"},
		{"", "dcgm-4.6.1", false, "nothing attested"},
		{"dcgm-4.6.1", "", false, "nothing pinned"},
	} {
		if got := config.RuntimeVersionSatisfies(tc.attested, tc.pinned); got != tc.want {
			t.Errorf("RuntimeVersionSatisfies(%q, %q) = %v, want %v — %s",
				tc.attested, tc.pinned, got, tc.want, tc.why)
		}
	}
}
