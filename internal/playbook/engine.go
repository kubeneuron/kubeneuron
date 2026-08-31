package playbook

import (
	"fmt"

	"github.com/kubeneuron/kubeneuron/pkg/types"
)

// Policy binds a problem class to a playbook. Policies come from
// configs/policies.yaml (see internal/config).
type Policy struct {
	Class types.ProblemClass
	// Vendor scopes the policy to one accelerator vendor; empty matches any.
	// See config.Match.Vendor for why a problem class alone is not enough.
	Vendor   types.AcceleratorVendor
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

// NewEngine builds an engine from loaded playbooks and ordered policies. Within
// one scope the first match wins; a vendor-specific policy always takes
// precedence over the unscoped fallback for that vendor.
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
	if pol, ok := e.PolicyFor(sig.Class, sig.Vendor()); ok {
		return e.books[pol.Playbook], true
	}
	return nil, false
}

// PolicyFor returns the policy matching the class and vendor (the same rule
// Select uses), so callers can read policy params such as observe thresholds.
// PolicyFor is the ONE place the selection rule lives. Select, the late bind
// that runs when a playbook was unbound, and the observation threshold lookup
// all ask it — they used to answer the question separately, and two of them
// answered it without the vendor at all.
//
// A vendor-scoped policy claims only its own vendor's signals, and a signal
// naming no vendor is not one of them: these ladders reset and reboot hardware,
// so acting on an unconfirmed guess is the wrong direction to fail in. An
// unscoped policy is a fallback. It is considered only after all exact vendor
// matches, regardless of file/CR priority: otherwise a generic policy written
// first silently shadows a vendor's safety ladder and can cordon and drain a
// node before the incompatible reset is refused.
func (e *Engine) PolicyFor(class types.ProblemClass, vendor types.AcceleratorVendor) (Policy, bool) {
	var fallback *Policy
	for _, pol := range e.policies {
		if pol.Class != class {
			continue
		}
		if pol.Vendor == vendor && vendor.Valid() {
			return pol, true
		}
		if pol.Vendor != "" {
			continue
		}
		if fallback == nil {
			candidate := pol
			fallback = &candidate
		}
	}
	if fallback != nil {
		return *fallback, true
	}
	return Policy{}, false
}

// SelectFor binds a playbook from a class and vendor directly, for the late
// bind: an incident that lost its playbook to an engine reload has no signal to
// reconstruct, and building a fake one dropped the vendor on the floor.
func (e *Engine) SelectFor(class types.ProblemClass, vendor types.AcceleratorVendor) (*Playbook, bool) {
	pol, ok := e.PolicyFor(class, vendor)
	if !ok {
		return nil, false
	}
	return e.books[pol.Playbook], true
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
