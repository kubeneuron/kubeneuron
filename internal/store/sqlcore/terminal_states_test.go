package sqlcore

import (
	"strings"
	"testing"

	"github.com/kubeneuron/kubeneuron/pkg/types"
)

// The claim guard's SQL state set and types.IncidentState.Halted are two
// spellings of one fact: automation has ended for the incident. A state added
// to only one of them reopens the round-4 defect from the other side — either
// a halted incident's spared action becomes claimable again, or a live
// incident's actions are silently starved. Pin them together.
func TestTerminalIncidentStatesMatchesHalted(t *testing.T) {
	all := []types.IncidentState{
		types.StateOpen, types.StateObserving, types.StateEvaluating,
		types.StateAwaitingApproval, types.StateExecuting, types.StateVerifying,
		types.StateNeedsHuman, types.StateResolved, types.StateExpired,
	}
	inSQL := func(s types.IncidentState) bool {
		return strings.Contains(terminalIncidentStates, "'"+string(s)+"'")
	}
	for _, s := range all {
		if inSQL(s) != s.Halted() {
			t.Errorf("state %s: SQL claim-guard membership %v, Halted() %v — the two definitions drifted",
				s, inSQL(s), s.Halted())
		}
	}
	// The literal holds exactly the halted states — nothing extra hides in it.
	wantCount := 0
	for _, s := range all {
		if s.Halted() {
			wantCount++
		}
	}
	if got := strings.Count(terminalIncidentStates, "'") / 2; got != wantCount {
		t.Errorf("SQL set holds %d states, Halted() defines %d", got, wantCount)
	}
}
