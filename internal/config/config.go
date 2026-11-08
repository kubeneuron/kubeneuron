// Package config loads and validates the controller configuration
// (configs/policies.yaml): safety limits, approval settings, and the
// signal-class → playbook policy bindings.
package config

import (
	"fmt"
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/kubeneuron/kubeneuron/internal/checkpoint"
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
	// CheckpointCoordination is the compiled spec.safety.checkpointCoordination.
	// A nil pointer is the whole of "off", for the same reason as the taint:
	// the operator emits this key only when an installation explicitly enables
	// coordination, so a configuration written before the feature existed can
	// never make a disruption start waiting after an upgrade.
	CheckpointCoordination *CheckpointCoordination `yaml:"checkpoint_coordination,omitempty"`
	// TopologyClassifier is the compiled spec.safety.topologyClassifier. The
	// classifier itself (internal/topology) always runs and can only narrow
	// the reset gate; this block carries only its opt-in inputs. A nil
	// pointer is the whole of the default: no Dynamic Resource Allocation
	// read is ever attempted by a controller whose configuration predates the
	// field, and the operator emits the key only when discovery is enabled.
	TopologyClassifier *TopologyClassifier `yaml:"topology_classifier,omitempty"`
	// ImpactPreview is the compiled spec.safety.impactPreview. Absent is off.
	ImpactPreview *ImpactPreview `yaml:"impact_preview,omitempty"`
}

// TopologyClassifier carries the opt-in inputs of the device topology
// classifier. See the CRD field of the same name.
type TopologyClassifier struct {
	DRA TopologyClassifierDRA `yaml:"dra"`
}

// TopologyClassifierDRA switches the read-only resource.k8s.io/v1 observation
// on. When Enabled the controller lists ResourceSlices for a node (get/list/
// watch only, v1 only) and inspects the claim references of the Pods
// scheduled on it (the core Pod list it already holds; ResourceClaim objects
// are never read), and refuses device-scoped actions when either read is
// unavailable; when false DRA is not consulted and the classification says
// so. Off by default.
type TopologyClassifierDRA struct {
	Enabled bool `yaml:"enabled"`
}

// CheckpointCoordination configures checkpoint-aware remediation. See the CRD
// field of the same name. Zero waits and an absent skip list are defaulted in
// Validate to the CRD defaults; every other invalid value is a rejected
// configuration, never a silently longer wait.
type CheckpointCoordination struct {
	Enabled bool `yaml:"enabled"`
	// DefaultWait is granted to an opted-in workload that requests nothing.
	DefaultWait Duration `yaml:"default_wait"`
	// MaxWait is the ceiling on any wait; never above CheckpointMaxWaitCeiling.
	MaxWait Duration `yaml:"max_wait"`
	// SkipClasses are problem classes for which no coordination is attempted.
	// The key is never omitted on output, so an explicit empty list survives
	// the operator-to-controller round trip: only an ABSENT key is filled with
	// checkpoint.DefaultSkipClasses, and `skip_classes: []` means "skip none".
	SkipClasses []types.ProblemClass `yaml:"skip_classes"`
	// Namespaces is the explicit allowlist; required when Enabled.
	Namespaces []string `yaml:"namespaces,omitempty"`
	// Advisory is the Phase 2 notice at approval park. Absent means off; the
	// operator emits the key only when the advisory is enabled.
	Advisory *CheckpointAdvisory `yaml:"advisory,omitempty"`
	// Groups is the bounded gang coordination. Absent means off.
	Groups *CheckpointGroups `yaml:"groups,omitempty"`
}

// CheckpointGroups configures group requests. See the CRD field
// spec.safety.checkpointCoordination.groups.
type CheckpointGroups struct {
	Enabled      bool `yaml:"enabled"`
	MaxGroupSize int  `yaml:"max_group_size"`
	CrossNode    bool `yaml:"cross_node"`
}

// ImpactPreview configures the impact preview of approval rounds. See the
// CRD field spec.safety.impactPreview. Absent means off; the operator emits
// the key only when enabled.
type ImpactPreview struct {
	Enabled            bool     `yaml:"enabled"`
	Budget             Duration `yaml:"budget"`
	MaxItems           int      `yaml:"max_items"`
	RequireForApproval bool     `yaml:"require_for_approval"`
	// WorkloadContext selects the read-only resolvers; each key is off
	// when absent.
	WorkloadContext ImpactWorkloadContext `yaml:"workload_context"`
}

// ImpactWorkloadContext selects the read-only context resolvers.
type ImpactWorkloadContext struct {
	BatchJobs bool `yaml:"batch_jobs"`
	Kueue     bool `yaml:"kueue"`
	KubeRay   bool `yaml:"kuberay"`
}

// Impact preview bounds, mirroring the CRD.
const (
	ImpactPreviewDefaultBudget = Duration(15 * time.Second)
	ImpactPreviewBudgetCeiling = Duration(60 * time.Second)
	ImpactPreviewDefaultItems  = 200
	ImpactPreviewMinItems      = 10
	ImpactPreviewMaxItems      = 1000
)

// validate applies the defaults and rejects what could not be enforced.
func (ip *ImpactPreview) validate() error {
	if !ip.Enabled {
		if ip.RequireForApproval {
			return fmt.Errorf("config: safety.impact_preview.require_for_approval requires safety.impact_preview.enabled")
		}
		return nil
	}
	if ip.Budget == 0 {
		ip.Budget = ImpactPreviewDefaultBudget
	}
	if ip.Budget < 0 {
		return fmt.Errorf("config: safety.impact_preview.budget must be positive, got %v", ip.Budget.Std())
	}
	if ip.Budget > ImpactPreviewBudgetCeiling {
		return fmt.Errorf("config: safety.impact_preview.budget %v exceeds the %v ceiling", ip.Budget.Std(), ImpactPreviewBudgetCeiling.Std())
	}
	if ip.MaxItems == 0 {
		ip.MaxItems = ImpactPreviewDefaultItems
	}
	if ip.MaxItems < ImpactPreviewMinItems || ip.MaxItems > ImpactPreviewMaxItems {
		return fmt.Errorf("config: safety.impact_preview.max_items must be between %d and %d, got %d", ImpactPreviewMinItems, ImpactPreviewMaxItems, ip.MaxItems)
	}
	return nil
}

// CheckpointAdvisory configures the advisory at approval park. See the CRD
// field spec.safety.checkpointCoordination.advisory.
type CheckpointAdvisory struct {
	Enabled bool `yaml:"enabled"`
	// Budget bounds one advisory round; zero is defaulted in validate to
	// CheckpointAdvisoryDefaultBudget and it is never above the ceiling.
	Budget Duration `yaml:"budget"`
}

// Checkpoint coordination bounds. The defaults mirror the CRD defaults and
// the ceiling is the installation-wide limit no configuration can raise.
const (
	CheckpointDefaultWait    = Duration(5 * time.Minute)
	CheckpointDefaultMaxWait = Duration(15 * time.Minute)
	CheckpointMaxWaitCeiling = Duration(30 * time.Minute)

	CheckpointAdvisoryDefaultBudget = Duration(10 * time.Second)
	CheckpointAdvisoryBudgetCeiling = Duration(60 * time.Second)
)

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

// Policy binds a problem class to a playbook. Within the same vendor scope the
// first match wins; a vendor-specific match takes precedence over an unscoped
// fallback for that vendor.
type Policy struct {
	Match    Match             `yaml:"match"`
	Playbook string            `yaml:"playbook"`
	Params   map[string]string `yaml:"params,omitempty"`
}

// Match selects signals a policy applies to.
type Match struct {
	Class types.ProblemClass `yaml:"class"`
	// Vendor scopes the policy to one accelerator vendor. Empty matches any,
	// which is what every policy written before this field did and still does.
	//
	// It exists because a problem class is not vendor-specific — "ecc-dbe"
	// happens on NVIDIA, AMD and Intel alike — while the ladder that answers it
	// very much is: a GPU reset is an NVIDIA contract, and quiescing the vendor
	// stack means different things per vendor. Without this, an operator adding
	// AMD nodes to a fleet could not give them their own ladder; the NVIDIA one
	// would be selected for their cards and refused at the capability gate,
	// after the cordon and the drain.
	Vendor types.AcceleratorVendor `yaml:"vendor,omitempty"`
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
	if cc := c.Safety.CheckpointCoordination; cc != nil {
		if err := cc.validate(); err != nil {
			return err
		}
	}
	if ip := c.Safety.ImpactPreview; ip != nil {
		if err := ip.validate(); err != nil {
			return err
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

// validate applies the CRD defaults to zero waits and to an absent skip list,
// and rejects everything else that could not be enforced as written. A
// disabled block is left alone: none of it is read, and the operator never
// emits one anyway.
func (cc *CheckpointCoordination) validate() error {
	if !cc.Enabled {
		if cc.Advisory != nil && cc.Advisory.Enabled {
			return fmt.Errorf("config: safety.checkpoint_coordination.advisory.enabled requires safety.checkpoint_coordination.enabled")
		}
		if cc.Groups != nil && cc.Groups.Enabled {
			return fmt.Errorf("config: safety.checkpoint_coordination.groups.enabled requires safety.checkpoint_coordination.enabled")
		}
		return nil
	}
	if cc.Groups != nil && cc.Groups.Enabled {
		if cc.Groups.MaxGroupSize == 0 {
			cc.Groups.MaxGroupSize = checkpoint.DefaultMaxGroupSize
		}
		if cc.Groups.MaxGroupSize < 1 || cc.Groups.MaxGroupSize > checkpoint.MaxGroupSizeCeiling {
			return fmt.Errorf("config: safety.checkpoint_coordination.groups.max_group_size must be between 1 and %d, got %d", checkpoint.MaxGroupSizeCeiling, cc.Groups.MaxGroupSize)
		}
	}
	if cc.Advisory != nil && cc.Advisory.Enabled {
		if cc.Advisory.Budget == 0 {
			cc.Advisory.Budget = CheckpointAdvisoryDefaultBudget
		}
		if cc.Advisory.Budget < 0 {
			return fmt.Errorf("config: safety.checkpoint_coordination.advisory.budget must be positive, got %v", cc.Advisory.Budget.Std())
		}
		if cc.Advisory.Budget > CheckpointAdvisoryBudgetCeiling {
			return fmt.Errorf("config: safety.checkpoint_coordination.advisory.budget %v exceeds the %v ceiling",
				cc.Advisory.Budget.Std(), CheckpointAdvisoryBudgetCeiling.Std())
		}
	}
	if cc.DefaultWait == 0 {
		cc.DefaultWait = CheckpointDefaultWait
	}
	if cc.MaxWait == 0 {
		cc.MaxWait = CheckpointDefaultMaxWait
	}
	if cc.SkipClasses == nil {
		// Absent, not empty: a decoded `skip_classes: []` is a non-nil empty
		// slice and is kept as the explicit "skip none" it says.
		cc.SkipClasses = checkpoint.DefaultSkipClasses()
	}
	if cc.DefaultWait < 0 {
		return fmt.Errorf("config: safety.checkpoint_coordination.default_wait must be positive, got %v", cc.DefaultWait.Std())
	}
	if cc.MaxWait < 0 {
		return fmt.Errorf("config: safety.checkpoint_coordination.max_wait must be positive, got %v", cc.MaxWait.Std())
	}
	if cc.MaxWait > CheckpointMaxWaitCeiling {
		return fmt.Errorf("config: safety.checkpoint_coordination.max_wait %v exceeds the %v ceiling",
			cc.MaxWait.Std(), CheckpointMaxWaitCeiling.Std())
	}
	if cc.DefaultWait > cc.MaxWait {
		return fmt.Errorf("config: safety.checkpoint_coordination.default_wait %v exceeds max_wait %v",
			cc.DefaultWait.Std(), cc.MaxWait.Std())
	}
	if len(cc.Namespaces) == 0 {
		return fmt.Errorf("config: safety.checkpoint_coordination.namespaces must name the allowed namespaces; an enabled policy without an allowlist is rejected")
	}
	for _, ns := range cc.Namespaces {
		if strings.TrimSpace(ns) == "" {
			return fmt.Errorf("config: safety.checkpoint_coordination.namespaces must not contain a blank entry")
		}
	}
	for _, class := range cc.SkipClasses {
		if strings.TrimSpace(string(class)) == "" {
			return fmt.Errorf("config: safety.checkpoint_coordination.skip_classes must not contain a blank entry")
		}
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
