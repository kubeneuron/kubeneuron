package playbook

import (
	"fmt"

	"github.com/kubeneuron/kubeneuron/pkg/types"
)

// Policy binds a problem class to a playbook. Policies come from
// configs/policies.yaml (see internal/config).
type Policy struct {
	Class    types.ProblemClass
	Playbook string
	// Params can override playbook defaults, e.g. observe thresholds.
	Params map[string]string
}

// Engine selects playbooks for signals and yields the next step for an
// incident. It is pure decision logic: execution, safety gating, and
// persistence live in the controller.
type Engine struct {
	books    map[string]*Playbook
	policies []Policy
}

// NewEngine builds an engine from loaded playbooks and ordered policies
// (first match wins).
func NewEngine(books map[string]*Playbook, policies []Policy) (*Engine, error) {
	for _, pol := range policies {
		if _, ok := books[pol.Playbook]; !ok {
			return nil, fmt.Errorf("policy for class %q references unknown playbook %q", pol.Class, pol.Playbook)
		}
	}
	return &Engine{books: books, policies: policies}, nil
}

// Select returns the playbook bound to the signal's problem class, or
// ok=false when no policy matches (the signal is then observe-only).
func (e *Engine) Select(sig types.Signal) (*Playbook, bool) {
	for _, pol := range e.policies {
		if pol.Class == sig.Class {
			return e.books[pol.Playbook], true
		}
	}
	return nil, false
}

// PolicyFor returns the first policy matching the class (the same rule
// Select uses), so callers can read policy params such as observe
// thresholds.
func (e *Engine) PolicyFor(class types.ProblemClass) (Policy, bool) {
	for _, pol := range e.policies {
		if pol.Class == class {
			return pol, true
		}
	}
	return Policy{}, false
}

// NextStep returns the step an incident should execute next, or done=true
// when the playbook is exhausted (the incident then moves to VERIFYING /
// RESOLVED).
func (e *Engine) NextStep(inc *types.Incident) (step *Step, done bool, err error) {
	book, ok := e.books[inc.Playbook]
	if !ok {
		return nil, false, fmt.Errorf("incident %s references unknown playbook %q", inc.ID, inc.Playbook)
	}
	if inc.StepIndex >= len(book.Steps) {
		return nil, true, nil
	}
	return &book.Steps[inc.StepIndex], false, nil
}

// Escalation returns the playbook to escalate to when the given playbook
// fails, or ok=false when there is none (incident goes to NEEDS_HUMAN).
func (e *Engine) Escalation(name string) (*Playbook, bool) {
	book, ok := e.books[name]
	if !ok || book.OnFailure.EscalateTo == "" {
		return nil, false
	}
	next, ok := e.books[book.OnFailure.EscalateTo]
	return next, ok
}

// Playbook returns a playbook by name.
func (e *Engine) Playbook(name string) (*Playbook, bool) {
	p, ok := e.books[name]
	return p, ok
}
