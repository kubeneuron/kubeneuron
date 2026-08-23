// Package playbook defines the declarative remediation playbooks, the YAML
// loader that reads them, and the incident state machine that executes them
// step by step.
package playbook

import (
	"fmt"
	"strconv"
	"time"

	"github.com/kubeneuron/kubeneuron/internal/action"
)

// Playbook is a declarative remediation procedure: an ordered list of steps
// executed against a target, with an optional escalation on failure.
// Playbooks live in configs/playbooks/*.yaml.
type Playbook struct {
	Name string `yaml:"name"`
	// Target is "gpu" or "node" — what kind of target the playbook applies to.
	Target string `yaml:"target"`
	// Cooldown: minimum time between two runs of this playbook on the same
	// target.
	Cooldown Duration `yaml:"cooldown"`
	Steps    []Step   `yaml:"steps"`
	// OnFailure names the playbook to escalate to when a step (or the final
	// verification) fails. Empty means stop and go to NEEDS_HUMAN.
	OnFailure OnFailure `yaml:"on_failure"`
}

// Step is one action within a playbook.
type Step struct {
	Name string `yaml:"name"`
	// Action is namespaced: "platform.cordon", "platform.drain",
	// "platform.uncordon", "agent.gpu_reset", "agent.reboot",
	// "agent.collect_bundle", "agent.run_diag", "verify.gpu_health",
	// "notify.ticket", ...
	Action string `yaml:"action"`
	// Approval: "none" (default) or "required". Steps marked required park
	// the incident in AWAITING_APPROVAL until a human decides.
	Approval string            `yaml:"approval,omitempty"`
	Timeout  Duration          `yaml:"timeout,omitempty"`
	Params   map[string]string `yaml:"params,omitempty"`
}

// NeedsApproval reports whether the step requires human approval.
func (s Step) NeedsApproval() bool { return s.Approval == "required" }

// OnFailure describes escalation when the playbook fails.
type OnFailure struct {
	EscalateTo string `yaml:"escalate_to,omitempty"`
}

// Validate checks structural invariants of a playbook.
func (p *Playbook) Validate() error {
	if p.Name == "" {
		return fmt.Errorf("playbook: name is required")
	}
	if p.Target != "gpu" && p.Target != "node" {
		return fmt.Errorf("playbook %q: target must be \"gpu\" or \"node\", got %q", p.Name, p.Target)
	}
	if len(p.Steps) == 0 {
		return fmt.Errorf("playbook %q: at least one step is required", p.Name)
	}
	// A negative cooldown is not a slow cooldown, it is none at all: every
	// "has enough time passed" comparison is already true, so the playbook
	// that just failed on this GPU re-runs on the next tick and keeps
	// re-running. The suppression a cooldown exists to provide is exactly what
	// stops a reset loop from cycling a card forever.
	if p.Cooldown < 0 {
		return fmt.Errorf("playbook %q: cooldown must not be negative, got %s", p.Name, p.Cooldown.Std())
	}
	seen := map[string]bool{}
	for i, s := range p.Steps {
		if s.Name == "" {
			return fmt.Errorf("playbook %q: step %d: name is required", p.Name, i)
		}
		if seen[s.Name] {
			return fmt.Errorf("playbook %q: duplicate step name %q", p.Name, s.Name)
		}
		seen[s.Name] = true
		if s.Action == "" {
			return fmt.Errorf("playbook %q: step %q: action is required", p.Name, s.Name)
		}
		if !action.Supported(s.Action) {
			return fmt.Errorf("playbook %q: step %q: unsupported action %q", p.Name, s.Name, s.Action)
		}
		if s.Approval != "" && s.Approval != "none" && s.Approval != "required" {
			return fmt.Errorf("playbook %q: step %q: approval must be \"none\" or \"required\"", p.Name, s.Name)
		}
		// Likewise a negative timeout: the step's deadline is already in the
		// past when it is computed, so the step is cancelled before it does
		// anything and the ladder escalates to the next, more destructive rung
		// on a playbook that never actually ran.
		if s.Timeout < 0 {
			return fmt.Errorf("playbook %q: step %q: timeout must not be negative, got %s",
				p.Name, s.Name, s.Timeout.Std())
		}
		// force is read as a bool at execution time and anything unparseable
		// reads as false. False is the safe direction, but silently: an author
		// who writes `force: yes` gets a drain that refuses the node, and finds
		// out when the ladder escalates past it at 3am rather than here.
		//
		// Scoped to the one action that reads it. Validating it everywhere was
		// worse than not validating at all: a boolean check on
		// `platform.cordon` or `agent.gpu_reset` implies the key does
		// something there, and it is silently dropped.
		if v, ok := s.Params["force"]; ok {
			if s.Action != forceAwareAction {
				return fmt.Errorf("playbook %q: step %q: params.force has no meaning for action %q "+
					"and would be silently ignored; it is read only by %q",
					p.Name, s.Name, s.Action, forceAwareAction)
			}
			forced, err := strconv.ParseBool(v)
			if err != nil {
				return fmt.Errorf("playbook %q: step %q: params.force must be a boolean, got %q",
					p.Name, s.Name, v)
			}
			// A forced drain is a different kind of act from a drain, and
			// every other gate that reasons about blast radius sees an
			// unchanged Drain: the action registry's Destructive flag, the
			// destructive-step confinement, the compiler's whole-VM rule. An
			// ordinary drain MOVES work; this one ENDS it, for pods nothing
			// will recreate. The registry already forces approval on
			// RecycleNode and ReplaceNode because they destroy an instance;
			// this destroys a tenant's running job with no equivalent, so it
			// gets the equivalent here.
			if forced && !s.NeedsApproval() {
				return fmt.Errorf("playbook %q: step %q: params.force destroys pods that nothing "+
					"will recreate, so the step must set approval: required",
					p.Name, s.Name)
			}
		}
	}
	return nil
}

// forceAwareAction is the only step that reads params.force.
const forceAwareAction = "platform.drain"

// Duration wraps time.Duration for YAML ("30m", "6h") round-tripping.
type Duration time.Duration

// UnmarshalYAML implements yaml.Unmarshaler.
func (d *Duration) UnmarshalYAML(unmarshal func(any) error) error {
	var s string
	if err := unmarshal(&s); err != nil {
		return err
	}
	if s == "" {
		*d = 0
		return nil
	}
	dur, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", s, err)
	}
	*d = Duration(dur)
	return nil
}

// MarshalYAML implements yaml.Marshaler.
func (d Duration) MarshalYAML() (any, error) {
	return time.Duration(d).String(), nil
}

// Std returns the standard-library duration.
func (d Duration) Std() time.Duration { return time.Duration(d) }
