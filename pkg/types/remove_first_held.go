// SPDX-License-Identifier: Apache-2.0

package types

import "fmt"

// State keys of an open remove-first record whose size is not set back yet
// because the resize is held: what it waits for, and the pool's node count
// now and at the plan, as last read from the cloud.
const (
	RemoveFirstHeldFor     = "held_for"
	RemoveFirstHeldCurrent = "held_current"
	RemoveFirstHeldPlanned = "held_planned"
	// RemoveFirstHeldForOperator is held_for of a record handed to an
	// operator with its pool's size not set back.
	RemoveFirstHeldForOperator = "operator"
)

// RemoveFirstHeldNote is the note of an open record whose resize is held, or
// of one handed to an operator with its pool short, as the pools row shows
// it; "" otherwise.
func RemoveFirstHeldNote(inc *Incident) string {
	r := inc.RemoveFirst
	if r != nil && r.Outcome == RemoveFirstNeedsOperator && inc.State == StateNeedsHuman && r.State[RemoveFirstHeldFor] == RemoveFirstHeldForOperator {
		return fmt.Sprintf("%s of %s nodes — replacement of %s needs the operator (incident %s)",
			r.State[RemoveFirstHeldCurrent], r.State[RemoveFirstHeldPlanned], inc.Target.Node, inc.ID)
	}
	if !r.Open() || r.State[RemoveFirstHeldFor] == "" || r.State[RemoveFirstHeldFor] == RemoveFirstHeldForOperator {
		return ""
	}
	return fmt.Sprintf("%s of %s nodes — replacement of %s is waiting for %s (incident %s)",
		r.State[RemoveFirstHeldCurrent], r.State[RemoveFirstHeldPlanned], inc.Target.Node, r.State[RemoveFirstHeldFor], inc.ID)
}

// RemoveFirstHeld is the status line of the same note, with its pool.
func RemoveFirstHeld(inc *Incident) string {
	if note := RemoveFirstHeldNote(inc); note != "" {
		return "pool " + inc.RemoveFirst.Pool + ": " + note
	}
	return ""
}
