package playbook

import (
	"fmt"

	"github.com/kubeneuron/kubeneuron/pkg/types"
)

// validTransitions encodes the incident state machine:
//
//	signal ──► OPEN ──► EVALUATING ──► EXECUTING ──► VERIFYING ──► RESOLVED
//	             │           │ ▲           ▲               │
//	             │           ▼ │ (rung)    │ (approved)    │ verify fail
//	             │     AWAITING_APPROVAL ──┘               ▼ (next rung)
//	             │           │                        EVALUATING
//	             │           ▼ (expired/rejected)
//	             │     EXPIRED / NEEDS_HUMAN
//	             │
//	             └──► OBSERVING ──(threshold reached)──► EVALUATING
//
// EVALUATING has a self-edge used only by escalate: when a rung is refused
// before its first disruptive step (an obstructed reset the ladder cannot
// clear), escalate advances to the next rung in place — it changes the bound
// playbook, resets StepIndex, and bumps Attempt, so it is real progress along
// the ladder, not a silent per-tick re-deny. Without it that refusal would
// re-fail every reconcile forever (EVALUATING -> EVALUATING was illegal).
//
// Any state may move to NEEDS_HUMAN (safety stop) and NEEDS_HUMAN may only be
// resolved manually.
var validTransitions = map[types.IncidentState][]types.IncidentState{
	types.StateOpen:             {types.StateObserving, types.StateEvaluating, types.StateNeedsHuman, types.StateResolved},
	types.StateObserving:        {types.StateEvaluating, types.StateNeedsHuman, types.StateResolved},
	types.StateEvaluating:       {types.StateEvaluating, types.StateExecuting, types.StateAwaitingApproval, types.StateObserving, types.StateNeedsHuman, types.StateResolved},
	types.StateAwaitingApproval: {types.StateExecuting, types.StateNeedsHuman, types.StateExpired, types.StateResolved},
	types.StateExecuting:        {types.StateVerifying, types.StateNeedsHuman, types.StateEvaluating},
	types.StateVerifying:        {types.StateResolved, types.StateEvaluating, types.StateNeedsHuman},
	types.StateNeedsHuman:       {types.StateResolved, types.StateEvaluating},
	// Terminal states have no outgoing transitions.
	types.StateResolved: {},
	types.StateExpired:  {},
}

// CanTransition reports whether moving from -> to is a legal state machine
// transition.
func CanTransition(from, to types.IncidentState) bool {
	for _, s := range validTransitions[from] {
		if s == to {
			return true
		}
	}
	return false
}

// Transition validates and applies a state change to an incident. It is the
// only place incident state should ever be mutated; callers persist the
// returned incident and append an audit entry in the same store transaction.
func Transition(inc *types.Incident, to types.IncidentState) error {
	if !CanTransition(inc.State, to) {
		return fmt.Errorf("illegal incident transition %s -> %s (incident %s)", inc.State, to, inc.ID)
	}
	inc.State = to
	return nil
}
