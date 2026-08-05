package config

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/kubeneuron/kubeneuron/pkg/types"
)

var (
	// ErrNoAcceleratorRuntimeProfile means that no explicitly configured
	// profile selects the node and vendor. Callers must hold accelerator
	// remediation in this case; falling back to a generic GPU policy is unsafe.
	ErrNoAcceleratorRuntimeProfile = errors.New("no accelerator runtime profile matches node and vendor")
	// ErrAmbiguousAcceleratorRuntimeProfile means that more than one configured
	// profile selects the node and vendor. Callers must hold remediation rather
	// than choosing one based on configuration order.
	ErrAmbiguousAcceleratorRuntimeProfile = errors.New("multiple accelerator runtime profiles match node and vendor")
)

var profileDigestPattern = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)

// AcceleratorRuntimeProfile is the server-owned desired contract for one
// NVIDIA runtime fleet. It is deliberately separate from an agent report:
// the agent may describe its local capabilities but cannot enlarge the set of
// actions configured here.
//
// This is the runtime representation for a Kubernetes-native
// AcceleratorRuntimeProfile resource. Current configuration sources compile
// it into the controller's digest-covered configuration, just as maintenance
// windows and node configs are compiled today.
//
// A profile has no execution-mode switch. It can only constrain a controller
// whose global safety gate has independently allowed execution. An empty
// AllowedActions list is an explicit observation-only profile.
type AcceleratorRuntimeProfile struct {
	// Name is a stable, human-readable profile identity. It is not used to
	// break ties between selectors: overlapping profiles are an error at use
	// time.
	Name string `yaml:"name"`
	// NodeSelector uses Kubernetes matchLabels semantics. It must be nonempty
	// so a profile cannot silently cover every node in an installation.
	NodeSelector map[string]string `yaml:"node_selector"`
	// Vendor is deliberately NVIDIA-only in this first gate. Other vendors
	// require an explicit contract and must not inherit NVIDIA permissions.
	Vendor types.AcceleratorVendor `yaml:"vendor"`
	// ProfileDigest pins the reviewed runtime/profile asset used by the agent.
	// It must equal the agent report's profile_digest before any action can be
	// considered.
	ProfileDigest string `yaml:"profile_digest"`
	// DriverVersion is the exact NVIDIA driver version approved by this
	// runtime profile. It must match live agent evidence before an action is
	// considered; a profile declaration is never treated as observed state.
	DriverVersion string `yaml:"driver_version"`
	// RuntimeVersion is the exact locally attested DCGM/runtime version,
	// normalized as dcgm-X.Y[.Z...], approved by this profile. It must match
	// current agent evidence before an action is considered; configured
	// metadata alone is not evidence.
	RuntimeVersion string `yaml:"runtime_version"`
	// ProfileUID and ProfileGeneration pin this runtime contract to the exact
	// Kubernetes object revision compiled by the operator. A report from an
	// earlier profile revision must not remain eligible just because its asset
	// digest happens to be unchanged.
	ProfileUID        string `yaml:"profile_uid"`
	ProfileGeneration int64  `yaml:"profile_generation"`
	// MaxReportAge bounds how long an observation can support a capability
	// decision. A missing or nonpositive value is invalid rather than infinite.
	MaxReportAge Duration `yaml:"max_report_age"`
	// AllowedActions is the semantic allow-list. It does not enable execution;
	// it is combined with an Eligible fresh report, the normal workflow gates,
	// and the global execution gate by the controller.
	AllowedActions []AcceleratorActionPolicy `yaml:"allowed_actions,omitempty"`
}

// AcceleratorActionPolicy permits one semantic action for selected target
// scopes. Physical reset has an explicit topology precondition so the
// controller cannot mistake a profile declaration for evidence about MIG or
// another partitioned runtime.
type AcceleratorActionPolicy struct {
	Action types.AcceleratorAction        `yaml:"action"`
	Scopes []types.AcceleratorTargetScope `yaml:"scopes"`
	// RequireVerifiedUnpartitionedTopology is required for reset-device on a
	// physical GPU. It tells the controller to require a fresh agent report
	// with topology_safety=verified-unpartitioned before dispatching a reset.
	RequireVerifiedUnpartitionedTopology bool `yaml:"require_verified_unpartitioned_topology,omitempty"`
}

// Validate checks that a profile can only narrow, never implicitly grant,
// NVIDIA remediation capability.
func (p AcceleratorRuntimeProfile) Validate() error {
	if strings.TrimSpace(p.Name) == "" {
		return fmt.Errorf("accelerator runtime profile: name is required")
	}
	if strings.TrimSpace(p.Name) != p.Name {
		return fmt.Errorf("accelerator runtime profile %q: name must not have leading or trailing whitespace", p.Name)
	}
	if p.Vendor != types.AcceleratorVendorNVIDIA {
		return fmt.Errorf("accelerator runtime profile %q: vendor must be %q until another vendor contract is implemented", p.Name, types.AcceleratorVendorNVIDIA)
	}
	if len(p.NodeSelector) == 0 {
		return fmt.Errorf("accelerator runtime profile %q: node_selector is required and cannot select every node", p.Name)
	}
	for key, value := range p.NodeSelector {
		if strings.TrimSpace(key) == "" || strings.TrimSpace(key) != key {
			return fmt.Errorf("accelerator runtime profile %q: node_selector contains an invalid label key", p.Name)
		}
		if strings.TrimSpace(value) == "" || strings.TrimSpace(value) != value {
			return fmt.Errorf("accelerator runtime profile %q: node_selector label %q has an invalid value", p.Name, key)
		}
	}
	if !profileDigestPattern.MatchString(p.ProfileDigest) {
		return fmt.Errorf("accelerator runtime profile %q: profile_digest must be a lowercase sha256 digest", p.Name)
	}
	if strings.TrimSpace(p.DriverVersion) == "" || strings.TrimSpace(p.DriverVersion) != p.DriverVersion {
		return fmt.Errorf("accelerator runtime profile %q: driver_version is required", p.Name)
	}
	if strings.TrimSpace(p.RuntimeVersion) == "" || strings.TrimSpace(p.RuntimeVersion) != p.RuntimeVersion {
		return fmt.Errorf("accelerator runtime profile %q: runtime_version is required", p.Name)
	}
	if strings.TrimSpace(p.ProfileUID) == "" || strings.TrimSpace(p.ProfileUID) != p.ProfileUID {
		return fmt.Errorf("accelerator runtime profile %q: profile_uid is required", p.Name)
	}
	if p.ProfileGeneration <= 0 {
		return fmt.Errorf("accelerator runtime profile %q: profile_generation must be positive", p.Name)
	}
	if p.MaxReportAge.Std() <= 0 {
		return fmt.Errorf("accelerator runtime profile %q: max_report_age must be positive", p.Name)
	}

	seen := make(map[types.AcceleratorAction]map[types.AcceleratorTargetScope]struct{})
	for _, policy := range p.AllowedActions {
		if !validAcceleratorAction(policy.Action) {
			return fmt.Errorf("accelerator runtime profile %q: invalid allowed action %q", p.Name, policy.Action)
		}
		if len(policy.Scopes) == 0 {
			return fmt.Errorf("accelerator runtime profile %q: action %q has no scopes", p.Name, policy.Action)
		}
		if seen[policy.Action] == nil {
			seen[policy.Action] = make(map[types.AcceleratorTargetScope]struct{})
		}
		for _, scope := range policy.Scopes {
			if !validAcceleratorScope(scope) {
				return fmt.Errorf("accelerator runtime profile %q: action %q has invalid scope %q", p.Name, policy.Action, scope)
			}
			if _, duplicate := seen[policy.Action][scope]; duplicate {
				return fmt.Errorf("accelerator runtime profile %q: duplicate action %q for scope %q", p.Name, policy.Action, scope)
			}
			seen[policy.Action][scope] = struct{}{}

			if policy.Action == types.AcceleratorActionResetDevice {
				if scope != types.AcceleratorScopePhysicalDevice {
					return fmt.Errorf("accelerator runtime profile %q: reset-device only supports physical-device scope", p.Name)
				}
				if !policy.RequireVerifiedUnpartitionedTopology {
					return fmt.Errorf("accelerator runtime profile %q: physical reset-device requires require_verified_unpartitioned_topology=true", p.Name)
				}
			} else if policy.RequireVerifiedUnpartitionedTopology {
				return fmt.Errorf("accelerator runtime profile %q: require_verified_unpartitioned_topology is only valid for reset-device", p.Name)
			}
		}
	}
	return nil
}

// MatchesLabels reports whether this profile's explicit Kubernetes
// matchLabels-style selector covers the node. Invalid/empty selectors do not
// match; callers should still validate profiles while loading them.
func (p AcceleratorRuntimeProfile) MatchesLabels(labels map[string]string) bool {
	if len(p.NodeSelector) == 0 {
		return false
	}
	for key, value := range p.NodeSelector {
		if labels[key] != value {
			return false
		}
	}
	return true
}

// Allows reports whether the server-owned profile permits an action at the
// requested scope. It does not consult an agent report and does not authorize
// any side effect by itself.
func (p AcceleratorRuntimeProfile) Allows(action types.AcceleratorAction, scope types.AcceleratorTargetScope) bool {
	for _, policy := range p.AllowedActions {
		if policy.Action != action {
			continue
		}
		for _, allowedScope := range policy.Scopes {
			if allowedScope == scope {
				return true
			}
		}
	}
	return false
}

// RequiresVerifiedUnpartitionedTopology reports the policy-level precondition
// for an allowed action. Validation guarantees that a physical NVIDIA reset
// returns true. The controller must additionally verify the current agent
// report; configuration never proves runtime topology.
func (p AcceleratorRuntimeProfile) RequiresVerifiedUnpartitionedTopology(action types.AcceleratorAction, scope types.AcceleratorTargetScope) bool {
	for _, policy := range p.AllowedActions {
		if policy.Action != action {
			continue
		}
		for _, allowedScope := range policy.Scopes {
			if allowedScope == scope {
				return policy.RequireVerifiedUnpartitionedTopology
			}
		}
	}
	return false
}

// CheckReport verifies the report conditions owned by a profile. It is safe
// to use as one input to an execution gate, but it deliberately knows nothing
// about global execution mode, maintenance windows, approval, or workload
// safety checks.
func (p AcceleratorRuntimeProfile) CheckReport(now time.Time, report types.AgentAcceleratorReport) error {
	if err := p.Validate(); err != nil {
		return err
	}
	if err := report.Validate(); err != nil {
		return fmt.Errorf("accelerator runtime profile %q: invalid agent report: %w", p.Name, err)
	}
	if now.IsZero() {
		return fmt.Errorf("accelerator runtime profile %q: current time is required", p.Name)
	}
	if report.Vendor != p.Vendor {
		return fmt.Errorf("accelerator runtime profile %q: report vendor %q does not match profile vendor %q", p.Name, report.Vendor, p.Vendor)
	}
	if report.ProfileDigest != p.ProfileDigest {
		return fmt.Errorf("accelerator runtime profile %q: report profile digest does not match", p.Name)
	}
	if report.ProfileUID != p.ProfileUID || report.ProfileGeneration != p.ProfileGeneration {
		return fmt.Errorf("accelerator runtime profile %q: report profile identity does not match", p.Name)
	}
	if report.DriverVersion != p.DriverVersion {
		return fmt.Errorf("accelerator runtime profile %q: report driver version does not match", p.Name)
	}
	if !runtimeVersionSatisfies(report.RuntimeVersion, p.RuntimeVersion) {
		return fmt.Errorf("accelerator runtime profile %q: attested runtime version %q does not satisfy the pinned %q",
			p.Name, report.RuntimeVersion, p.RuntimeVersion)
	}
	if report.ObservedAt.After(now) {
		return fmt.Errorf("accelerator runtime profile %q: report observation is in the future", p.Name)
	}
	if now.Sub(report.ObservedAt) > p.MaxReportAge.Std() {
		return fmt.Errorf("accelerator runtime profile %q: report is older than max_report_age", p.Name)
	}
	if report.Readiness != types.AcceleratorReadinessReady {
		return fmt.Errorf("accelerator runtime profile %q: report readiness is %q, not ready", p.Name, report.Readiness)
	}
	return nil
}

// CheckAction checks this profile and report for one semantic capability. The
// caller must first select the profile from node labels and still apply all
// normal controller gates before action dispatch.
func (p AcceleratorRuntimeProfile) CheckAction(now time.Time, report types.AgentAcceleratorReport, action types.AcceleratorAction, scope types.AcceleratorTargetScope) error {
	if err := p.CheckReport(now, report); err != nil {
		return err
	}
	if !p.Allows(action, scope) {
		return fmt.Errorf("accelerator runtime profile %q: action %q at scope %q is not allowed", p.Name, action, scope)
	}
	if !reportDeclaresCapability(report, action, scope) {
		return fmt.Errorf("accelerator runtime profile %q: agent report does not declare action %q at scope %q", p.Name, action, scope)
	}
	if p.RequiresVerifiedUnpartitionedTopology(action, scope) && report.TopologySafety != types.AcceleratorTopologyVerifiedUnpartitioned {
		return fmt.Errorf("accelerator runtime profile %q: action %q at scope %q requires verified-unpartitioned topology", p.Name, action, scope)
	}
	return nil
}

// ResolveAcceleratorRuntimeProfile returns the only profile that explicitly
// selects the node labels and vendor. No profile, or overlapping profiles,
// is a fail-closed result.
func (c Config) ResolveAcceleratorRuntimeProfile(labels map[string]string, vendor types.AcceleratorVendor) (*AcceleratorRuntimeProfile, error) {
	var matched *AcceleratorRuntimeProfile
	for i := range c.AcceleratorProfiles {
		profile := &c.AcceleratorProfiles[i]
		if err := profile.Validate(); err != nil {
			return nil, fmt.Errorf("configured accelerator runtime profile %q is invalid: %w", profile.Name, err)
		}
		if profile.Vendor != vendor || !profile.MatchesLabels(labels) {
			continue
		}
		if matched != nil {
			return nil, fmt.Errorf("%w: %q and %q", ErrAmbiguousAcceleratorRuntimeProfile, matched.Name, profile.Name)
		}
		matched = profile
	}
	if matched == nil {
		return nil, fmt.Errorf("%w: vendor %q", ErrNoAcceleratorRuntimeProfile, vendor)
	}
	return matched, nil
}

func validAcceleratorAction(action types.AcceleratorAction) bool {
	switch action {
	case types.AcceleratorActionEvacuateWorkloads,
		types.AcceleratorActionQuarantineNode,
		types.AcceleratorActionResetDevice,
		types.AcceleratorActionRestartRuntime,
		types.AcceleratorActionRebootNode,
		types.AcceleratorActionCollectDiagnostics,
		types.AcceleratorActionVerifyHealth,
		types.AcceleratorActionReplaceNode:
		return true
	default:
		return false
	}
}

func validAcceleratorScope(scope types.AcceleratorTargetScope) bool {
	switch scope {
	case types.AcceleratorScopeNode, types.AcceleratorScopePhysicalDevice, types.AcceleratorScopePartition:
		return true
	default:
		return false
	}
}

func reportDeclaresCapability(report types.AgentAcceleratorReport, action types.AcceleratorAction, scope types.AcceleratorTargetScope) bool {
	for _, capability := range report.Capabilities {
		if capability.Action != action {
			continue
		}
		for _, reportedScope := range capability.Scopes {
			if reportedScope == scope {
				return true
			}
		}
	}
	return false
}
