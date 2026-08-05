// Package playbook defines the declarative remediation playbooks, the YAML
// loader that reads them, and the incident state machine that executes them
// step by step.
package playbook

import (
	"fmt"
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
	}
	return nil
}

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
