// Package config loads and validates the controller configuration
// (configs/policies.yaml): safety limits, approval settings, and the
// signal-class → playbook policy bindings.
package config

import (
	"fmt"
	"os"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/kubeneuron/kubeneuron/pkg/types"
)

// Config is the root controller configuration.
type Config struct {
	Safety              Safety                      `yaml:"safety"`
	Approvals           Approvals                   `yaml:"approvals"`
	Policies            []Policy                    `yaml:"policies"`
	AcceleratorProfiles []AcceleratorRuntimeProfile `yaml:"accelerator_profiles,omitempty"`
}

// Safety mirrors internal/safety.Limits plus flap settings.
type Safety struct {
	DryRun                    bool     `yaml:"dry_run"`
	MaxConcurrentRemediations int      `yaml:"max_concurrent_remediations"`
	MaxConcurrentReboots      int      `yaml:"max_concurrent_reboots"`
	Flap                      Flap     `yaml:"flap"`
	VerifyQuietWindow         Duration `yaml:"verify_quiet_window"`
	// QuiesceForbidResetWhenPresent lists process names whose presence on a
	// node means a per-device reset must not be attempted there. See the CRD
	// field of the same name: the motivating case is nv-fabricmanager, which
	// holds the GPUs and must not be stopped to make room for a remediation.
	QuiesceForbidResetWhenPresent []string `yaml:"quiesce_forbid_reset_when_present,omitempty"`
	// DestructiveExecutionNodeSelector is the compiled
	// spec.safety.destructiveExecution.nodeSelector. It is the declared blast
	// radius: on an Enabled install the controller must confine its own
	// destructive platform steps (cordon, drain, evict, recycle/replace) to
	// nodes whose labels match this selector, exactly as the agent DaemonSet is
	// already confined. Empty means no confinement is configured (every
	// non-Enabled install is globally dry-run, so nothing executes anyway).
	DestructiveExecutionNodeSelector map[string]string `yaml:"destructive_execution_node_selector,omitempty"`
	// TaintDegradedNodes is the compiled spec.safety.taintDegradedNodes. A nil
	// pointer is the whole of "off": the operator emits this key only when an
	// installation explicitly enables the taint, so a controller reading a
	// configuration written before the feature existed cannot start marking
	// nodes after an upgrade.
	TaintDegradedNodes *TaintDegradedNodes `yaml:"taint_degraded_nodes,omitempty"`
}

// TaintDegradedNodes configures the kubeneuron.io/degraded taint. See the CRD
// field of the same name.
type TaintDegradedNodes struct {
	Enabled bool `yaml:"enabled"`
	// Effect is PreferNoSchedule or NoSchedule. Empty defaults to
	// PreferNoSchedule in Validate; any other value is a rejected
	// configuration, never a silently stronger effect.
	Effect string `yaml:"effect,omitempty"`
}

// Taint effect values accepted in a compiled configuration. They are the
// Kubernetes spellings so the platform can pass them straight through.
const (
	TaintEffectPreferNoSchedule = "PreferNoSchedule"
	TaintEffectNoSchedule       = "NoSchedule"
)

// Flap configures flap detection.
type Flap struct {
	Count  int      `yaml:"count"`
	Window Duration `yaml:"window"`
}

// Approvals configures the human-approval workflow.
type Approvals struct {
	// Channels: any of "slack", "cli", "web".
	Channels []string `yaml:"channels"`
	// TTL: how long an approval request may stay pending before the
	// incident expires to EXPIRED and is re-notified.
	TTL Duration `yaml:"ttl"`
}

// Policy binds a problem class to a playbook (first match wins).
type Policy struct {
	Match    Match             `yaml:"match"`
	Playbook string            `yaml:"playbook"`
	Params   map[string]string `yaml:"params,omitempty"`
}

// Match selects signals a policy applies to.
type Match struct {
	Class types.ProblemClass `yaml:"class"`
}

// Load reads and validates the controller configuration file.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c Config
	if err := yaml.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if err := c.Validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &c, nil
}

// Validate checks configuration invariants and applies defaults.
func (c *Config) Validate() error {
	if c.Safety.MaxConcurrentRemediations <= 0 {
		c.Safety.MaxConcurrentRemediations = 2
	}
	if c.Safety.MaxConcurrentReboots <= 0 {
		c.Safety.MaxConcurrentReboots = 1
	}
	if c.Approvals.TTL == 0 {
		c.Approvals.TTL = Duration(12 * time.Hour)
	}
	if t := c.Safety.TaintDegradedNodes; t != nil {
		switch t.Effect {
		case "":
			t.Effect = TaintEffectPreferNoSchedule
		case TaintEffectPreferNoSchedule, TaintEffectNoSchedule:
		default:
			// Rejecting the whole configuration is the point: a typo'd effect
			// must not fall back to a working default, because the operator
			// asking for one effect and getting another is how a scheduling
			// preference becomes an outage.
			return fmt.Errorf("config: safety.taint_degraded_nodes.effect %q is not %s or %s",
				t.Effect, TaintEffectPreferNoSchedule, TaintEffectNoSchedule)
		}
	}
	if len(c.Policies) == 0 {
		return fmt.Errorf("config: at least one policy is required")
	}
	for i, p := range c.Policies {
		if p.Match.Class == "" {
			return fmt.Errorf("config: policy %d: match.class is required", i)
		}
		if p.Playbook == "" {
			return fmt.Errorf("config: policy %d (class %s): playbook is required", i, p.Match.Class)
		}
	}
	seenProfiles := make(map[string]struct{}, len(c.AcceleratorProfiles))
	for i, profile := range c.AcceleratorProfiles {
		if err := profile.Validate(); err != nil {
			return fmt.Errorf("config: accelerator profile %d: %w", i, err)
		}
		if _, duplicate := seenProfiles[profile.Name]; duplicate {
			return fmt.Errorf("config: duplicate accelerator runtime profile %q", profile.Name)
		}
		seenProfiles[profile.Name] = struct{}{}
	}
	return nil
}

// Duration wraps time.Duration for YAML round-tripping.
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
func (d Duration) MarshalYAML() (any, error) { return time.Duration(d).String(), nil }

// Std returns the standard-library duration.
func (d Duration) Std() time.Duration { return time.Duration(d) }
