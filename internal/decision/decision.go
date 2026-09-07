// Package decision contains KubeNeuron's side-effect-free decision and
// evidence evaluator.  It deliberately depends only on immutable inputs: it
// does not read a clock, Kubernetes, a database, or the controller's mutable
// safety gate.  Every caller captures those facts into a Snapshot first.
//
// Keeping this package pure is a safety boundary.  A preview, a simulation,
// and the controller's last-mile admission can then make the same decision
// about the same evidence instead of each carrying a slightly different
// interpretation of "fresh", "selected", or "safe".
package decision

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/kubeneuron/kubeneuron/internal/config"
	"github.com/kubeneuron/kubeneuron/pkg/types"
)

// EvaluatorVersion is stored with every DecisionSnapshot and result.  A
// version bump is an explicit compatibility event: callers can tell whether a
// historical decision was made under the same contract as a current one.
const EvaluatorVersion = "v1"

// State is the operator-facing disposition of a decision.
type State string

const (
	StateEligible     State = "Eligible"
	StateObservedOnly State = "ObservedOnly"
	StateBlocked      State = "Blocked"
	StateUnknown      State = "Unknown"
)

// ReasonCode is a stable machine-readable explanation.  Do not rename a
// value: APIs, CLI output, audit exports, and policy automation use these as a
// product contract.  New causes require a new code.
type ReasonCode string

const (
	ReasonEvidenceStale           ReasonCode = "EvidenceStale"
	ReasonProfileMismatch         ReasonCode = "ProfileMismatch"
	ReasonDriverUnsupported       ReasonCode = "DriverUnsupported"
	ReasonNoHealthyAgent          ReasonCode = "NoHealthyAgent"
	ReasonMaintenanceWindowClosed ReasonCode = "MaintenanceWindowClosed"
	ReasonEmergencyStopActive     ReasonCode = "EmergencyStopActive"
	ReasonIncidentConflict        ReasonCode = "IncidentConflict"
	ReasonSharedOwnershipConflict ReasonCode = "SharedOwnershipConflict"
	ReasonSelectorExcluded        ReasonCode = "SelectorExcluded"
	ReasonApprovalMissing         ReasonCode = "ApprovalMissing"
	ReasonAutonomyBudgetExhausted ReasonCode = "AutonomyBudgetExhausted"
	ReasonCapabilityMissing       ReasonCode = "CapabilityMissing"
	ReasonSnapshotIncomplete      ReasonCode = "SnapshotIncomplete"
	ReasonEvidenceSourceMissing   ReasonCode = "EvidenceSourceMissing"
	// ReasonConfigurationChanged says that the immutable configuration
	// revision an operation was approved/simulated against is no longer the
	// revision represented by this live snapshot.  It is intentionally a
	// distinct contract from ProfileMismatch: a valid new profile is still not
	// authority to execute an old autonomy plan.
	ReasonConfigurationChanged ReasonCode = "ConfigurationChanged"
)

// ActionClass tells the evaluator why a decision is being requested.  The
// class changes safety gates, but never makes an action permitted by itself.
type ActionClass string

const (
	ActionObserve    ActionClass = "observe"
	ActionDiagnostic ActionClass = "diagnostic"
	ActionSimulate   ActionClass = "simulate"
	ActionRemediate  ActionClass = "remediate"
	ActionAutonomous ActionClass = "autonomous"
)

// Request is the requested operation.  AcceleratorAction and Scope are
// intentionally explicit: an action name without its target scope is not an
// authority to touch a physical device.
type Request struct {
	Class             ActionClass                  `json:"class"`
	AcceleratorAction types.AcceleratorAction      `json:"accelerator_action,omitempty"`
	Scope             types.AcceleratorTargetScope `json:"scope,omitempty"`
	TargetDeviceID    string                       `json:"target_device_id,omitempty"`
	ApprovalRequired  bool                         `json:"approval_required,omitempty"`
	ApprovalGranted   bool                         `json:"approval_granted,omitempty"`
	// MaintenanceRequired is used by bounded extended diagnostics. It says a
	// window must be open; AllowDuringMaintenance is separate because ordinary
	// remediation remains paused while that same window is active.
	MaintenanceRequired          bool `json:"maintenance_required,omitempty"`
	AllowDuringMaintenance       bool `json:"allow_during_maintenance,omitempty"`
	ElevatedAuthorizationGranted bool `json:"elevated_authorization_granted,omitempty"`
	DisruptionBudgetApproved     bool `json:"disruption_budget_approved,omitempty"`
}

// EvidenceRef identifies evidence used by a decision without copying a raw,
// potentially sensitive diagnostic payload into every API response.
type EvidenceRef struct {
	Source     string    `json:"source"`
	ID         string    `json:"id"`
	ObservedAt time.Time `json:"observed_at"`
	Digest     string    `json:"digest"`
}

// Limits captures the bounds the caller must continue to enforce after an
// Eligible result.  Zero values mean that the corresponding limit was not
// present in the captured effective configuration, never "unlimited".
type Limits struct {
	Deadline           time.Time `json:"deadline,omitempty"`
	EvidenceExpiresAt  time.Time `json:"evidence_expires_at,omitempty"`
	MaxConcurrentNodes int       `json:"max_concurrent_nodes,omitempty"`
	MaxActionsPerHour  int       `json:"max_actions_per_hour,omitempty"`
	RetryLimit         int       `json:"retry_limit,omitempty"`
}

// Snapshot is the immutable, versioned input to Evaluate.  The builder that
// captures live state owns data access and time; this type owns only the
// semantics of evaluating it.
//
// A nil Profile is meaningful: it means this node can be observed but no
// reviewed runtime contract was selected.  A nil Report means evidence was
// unavailable, which is Unknown rather than a permissive default.
type Snapshot struct {
	Version      string    `json:"version"`
	EvaluatedAt  time.Time `json:"evaluated_at"`
	ConfigDigest string    `json:"config_digest"`
	// RequiredConfigDigest is set by a caller that has an immutable approval
	// or simulation binding (currently GPUAutonomyPlan).  Evaluate compares it
	// to the captured effective configuration rather than allowing an adapter
	// to overwrite a live digest with the historical one.
	RequiredConfigDigest string                            `json:"required_config_digest,omitempty"`
	Node                 types.Node                        `json:"node"`
	Report               *types.AgentAcceleratorReport     `json:"report,omitempty"`
	Profile              *config.AcceleratorRuntimeProfile `json:"profile,omitempty"`
	Request              Request                           `json:"request"`
	// EvidenceRefs records the individual immutable source facts selected by
	// the live adapter. Source names power explicit autonomy requirements.
	EvidenceRefs            []EvidenceRef `json:"evidence_refs,omitempty"`
	EvidenceSources         []string      `json:"evidence_sources,omitempty"`
	RequiredEvidenceSources []string      `json:"required_evidence_sources,omitempty"`

	// EvidenceMaxAge and AgentMaxAge are captured policy values.  A
	// non-positive EvidenceMaxAge falls back to the selected profile's
	// MaxReportAge; a non-positive AgentMaxAge defaults to ten minutes.
	EvidenceMaxAge time.Duration `json:"evidence_max_age"`
	AgentMaxAge    time.Duration `json:"agent_max_age"`

	GlobalPaused            bool `json:"global_paused,omitempty"`
	EmergencyStop           bool `json:"emergency_stop,omitempty"`
	MaintenanceActive       bool `json:"maintenance_active,omitempty"`
	ChangeFreeze            bool `json:"change_freeze,omitempty"`
	IncidentConflict        bool `json:"incident_conflict,omitempty"`
	OwnershipConflict       bool `json:"ownership_conflict,omitempty"`
	SelectorExcluded        bool `json:"selector_excluded,omitempty"`
	AutonomyBudgetExhausted bool `json:"autonomy_budget_exhausted,omitempty"`

	Limits Limits `json:"limits,omitempty"`
}

// Result is a complete evaluator answer.  It is safe to persist verbatim in a
// DecisionSnapshot and safe to render in the operator console.
type Result struct {
	EvaluatorVersion string                    `json:"evaluator_version"`
	State            State                     `json:"state"`
	ReasonCodes      []ReasonCode              `json:"reason_codes"`
	HumanSummary     string                    `json:"human_summary"`
	EvidenceRefs     []EvidenceRef             `json:"evidence_refs"`
	RequiredActions  []string                  `json:"required_actions,omitempty"`
	AllowedActions   []types.AcceleratorAction `json:"allowed_actions,omitempty"`
	Limits           Limits                    `json:"limits"`
	ConfigDigest     string                    `json:"config_digest"`
	EvaluatedAt      time.Time                 `json:"evaluated_at"`
	ExpiresAt        time.Time                 `json:"expires_at"`
}

// Permitted reports whether this result may be used to start the requested
// operation.  ObservedOnly is deliberately false: read access is not action
// authority.
func (r Result) Permitted() bool { return r.State == StateEligible }

// Evaluate makes one deterministic decision from a captured snapshot.
func Evaluate(s Snapshot) Result {
	if s.Version == "" {
		s.Version = EvaluatorVersion
	}
	result := Result{
		EvaluatorVersion: EvaluatorVersion,
		ConfigDigest:     s.ConfigDigest,
		EvaluatedAt:      s.EvaluatedAt.UTC(),
		Limits:           s.Limits,
	}
	if result.EvaluatedAt.IsZero() || strings.TrimSpace(s.Node.Name) == "" || strings.TrimSpace(s.Node.UID) == "" {
		return finalize(result, StateUnknown, []ReasonCode{ReasonSnapshotIncomplete}, s)
	}
	if result.ConfigDigest == "" {
		result.ConfigDigest = ConfigDigest(s.Profile)
	}

	result.EvidenceRefs = append([]EvidenceRef(nil), s.EvidenceRefs...)
	if s.Report != nil && !hasEvidenceSource(result.EvidenceRefs, "accelerator-report/"+string(s.Report.Vendor)) {
		result.EvidenceRefs = append(result.EvidenceRefs, reportEvidence(*s.Report))
	}
	result.AllowedActions = allowedActions(s)

	// A global stop is always first.  It blocks effects even if the evidence is
	// otherwise excellent, and it must not be hidden behind a lower-level
	// profile mismatch.
	if s.EmergencyStop || s.GlobalPaused {
		return finalize(result, StateBlocked, []ReasonCode{ReasonEmergencyStopActive}, s)
	}
	if s.ChangeFreeze {
		return finalize(result, StateBlocked, []ReasonCode{ReasonMaintenanceWindowClosed}, s)
	}
	if s.Request.MaintenanceRequired && !s.MaintenanceActive {
		return finalize(result, StateBlocked, []ReasonCode{ReasonMaintenanceWindowClosed}, s)
	}
	if s.MaintenanceActive && !s.Request.AllowDuringMaintenance {
		return finalize(result, StateBlocked, []ReasonCode{ReasonMaintenanceWindowClosed}, s)
	}
	if s.SelectorExcluded {
		return finalize(result, StateBlocked, []ReasonCode{ReasonSelectorExcluded}, s)
	}
	if s.OwnershipConflict {
		return finalize(result, StateBlocked, []ReasonCode{ReasonSharedOwnershipConflict}, s)
	}
	if s.IncidentConflict {
		return finalize(result, StateBlocked, []ReasonCode{ReasonIncidentConflict}, s)
	}
	if strings.TrimSpace(s.RequiredConfigDigest) != "" && result.ConfigDigest != s.RequiredConfigDigest {
		return finalize(result, StateBlocked, []ReasonCode{ReasonConfigurationChanged}, s)
	}
	if s.Request.Class == ActionAutonomous && s.AutonomyBudgetExhausted {
		return finalize(result, StateBlocked, []ReasonCode{ReasonAutonomyBudgetExhausted}, s)
	}
	if s.Request.ApprovalRequired && !s.Request.ApprovalGranted {
		return finalize(result, StateBlocked, []ReasonCode{ReasonApprovalMissing}, s)
	}
	if s.Request.MaintenanceRequired &&
		(!s.Request.ElevatedAuthorizationGranted || !s.Request.DisruptionBudgetApproved) {
		return finalize(result, StateBlocked, []ReasonCode{ReasonApprovalMissing}, s)
	}

	if !agentHealthy(s) {
		return finalize(result, StateUnknown, []ReasonCode{ReasonNoHealthyAgent}, s)
	}
	if s.Report == nil {
		return finalize(result, StateUnknown, []ReasonCode{ReasonEvidenceStale}, s)
	}
	if !reportFresh(s) {
		return finalize(result, StateUnknown, []ReasonCode{ReasonEvidenceStale}, s)
	}
	if len(missingEvidenceSources(s)) > 0 {
		return finalize(result, StateBlocked, []ReasonCode{ReasonEvidenceSourceMissing}, s)
	}

	// Observation remains useful without an action-capable profile.  It is
	// intentionally not Eligible: callers may display telemetry but cannot
	// turn that display into a remediation authorization.
	if s.Profile == nil {
		return finalize(result, StateObservedOnly, []ReasonCode{ReasonProfileMismatch}, s)
	}
	if err := s.Profile.CheckReport(s.EvaluatedAt, *s.Report); err != nil {
		return finalize(result, StateBlocked, reasonsForProfileError(err), s)
	}

	if s.Request.Class == "" || s.Request.Class == ActionObserve {
		if len(result.AllowedActions) == 0 {
			return finalize(result, StateObservedOnly, nil, s)
		}
		return finalize(result, StateEligible, nil, s)
	}

	// Diagnostic profiles may be allowed by an observation-capable report.  A
	// specific accelerator action still has to be both profile-authorized and
	// report-declared before the durable work queue sees it.
	if s.Request.AcceleratorAction == "" {
		if s.Request.Class == ActionDiagnostic {
			return finalize(result, StateEligible, nil, s)
		}
		return finalize(result, StateBlocked, []ReasonCode{ReasonCapabilityMissing}, s)
	}
	if err := s.Profile.CheckAction(s.EvaluatedAt, *s.Report, s.Request.AcceleratorAction, s.Request.Scope); err != nil {
		return finalize(result, StateBlocked, reasonsForProfileError(err), s)
	}
	if s.Request.TargetDeviceID != "" && !containsTargetDevice(*s.Report, s.Request.TargetDeviceID) {
		return finalize(result, StateBlocked, []ReasonCode{ReasonCapabilityMissing}, s)
	}
	return finalize(result, StateEligible, nil, s)
}

func finalize(result Result, state State, reasons []ReasonCode, s Snapshot) Result {
	result.State = state
	result.ReasonCodes = canonicalReasons(reasons)
	result.RequiredActions = requiredActions(result.ReasonCodes)
	result.HumanSummary = summary(state, result.ReasonCodes)
	result.ExpiresAt = result.EvaluatedAt
	if s.Report != nil {
		maxAge := evidenceMaxAge(s)
		if maxAge > 0 {
			expires := s.Report.ObservedAt.UTC().Add(maxAge)
			if expires.After(result.ExpiresAt) {
				result.ExpiresAt = expires
			}
			result.Limits.EvidenceExpiresAt = expires
		}
	}
	if !result.Limits.Deadline.IsZero() && (result.ExpiresAt.IsZero() || result.Limits.Deadline.Before(result.ExpiresAt)) {
		result.ExpiresAt = result.Limits.Deadline.UTC()
	}
	return result
}

func agentHealthy(s Snapshot) bool {
	if s.Node.AgentLastSeen.IsZero() || s.Node.AgentLastSeen.After(s.EvaluatedAt) {
		return false
	}
	maxAge := s.AgentMaxAge
	if maxAge <= 0 {
		maxAge = 10 * time.Minute
	}
	return s.EvaluatedAt.Sub(s.Node.AgentLastSeen) <= maxAge
}

func evidenceMaxAge(s Snapshot) time.Duration {
	if s.EvidenceMaxAge > 0 {
		return s.EvidenceMaxAge
	}
	if s.Profile != nil && s.Profile.MaxReportAge.Std() > 0 {
		return s.Profile.MaxReportAge.Std()
	}
	return 10 * time.Minute
}

func reportFresh(s Snapshot) bool {
	if s.Report == nil || s.Report.ObservedAt.IsZero() || s.Report.ObservedAt.After(s.EvaluatedAt) {
		return false
	}
	return s.EvaluatedAt.Sub(s.Report.ObservedAt) <= evidenceMaxAge(s)
}

func allowedActions(s Snapshot) []types.AcceleratorAction {
	if s.Profile == nil || s.Report == nil {
		return nil
	}
	set := make(map[types.AcceleratorAction]struct{})
	for _, policy := range s.Profile.AllowedActions {
		for _, capability := range s.Report.Capabilities {
			if capability.Action != policy.Action {
				continue
			}
			for _, scope := range policy.Scopes {
				for _, reported := range capability.Scopes {
					if scope == reported {
						set[policy.Action] = struct{}{}
					}
				}
			}
		}
	}
	out := make([]types.AcceleratorAction, 0, len(set))
	for action := range set {
		out = append(out, action)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

func containsTargetDevice(report types.AgentAcceleratorReport, id string) bool {
	for _, device := range report.Devices {
		if device.ID == id {
			return true
		}
	}
	return false
}

func reasonsForProfileError(err error) []ReasonCode {
	message := err.Error()
	switch {
	case strings.Contains(message, "older than max_report_age"), strings.Contains(message, "observation is in the future"):
		return []ReasonCode{ReasonEvidenceStale}
	case strings.Contains(message, "driver version"), strings.Contains(message, "runtime version"):
		return []ReasonCode{ReasonDriverUnsupported}
	case strings.Contains(message, "not allowed"), strings.Contains(message, "does not declare"), strings.Contains(message, "topology"), strings.Contains(message, "does not contain"):
		return []ReasonCode{ReasonCapabilityMissing}
	default:
		return []ReasonCode{ReasonProfileMismatch}
	}
}

var reasonOrder = map[ReasonCode]int{
	ReasonSnapshotIncomplete:      0,
	ReasonEmergencyStopActive:     1,
	ReasonMaintenanceWindowClosed: 2,
	ReasonSelectorExcluded:        3,
	ReasonSharedOwnershipConflict: 4,
	ReasonIncidentConflict:        5,
	ReasonApprovalMissing:         6,
	ReasonAutonomyBudgetExhausted: 7,
	ReasonNoHealthyAgent:          8,
	ReasonEvidenceStale:           9,
	ReasonProfileMismatch:         10,
	ReasonDriverUnsupported:       11,
	ReasonCapabilityMissing:       12,
	ReasonEvidenceSourceMissing:   13,
	ReasonConfigurationChanged:    14,
}

func canonicalReasons(in []ReasonCode) []ReasonCode {
	if len(in) == 0 {
		return []ReasonCode{}
	}
	set := make(map[ReasonCode]struct{}, len(in))
	for _, reason := range in {
		set[reason] = struct{}{}
	}
	out := make([]ReasonCode, 0, len(set))
	for reason := range set {
		out = append(out, reason)
	}
	sort.Slice(out, func(i, j int) bool {
		return reasonOrder[out[i]] < reasonOrder[out[j]]
	})
	return out
}

func requiredActions(reasons []ReasonCode) []string {
	var out []string
	for _, reason := range reasons {
		switch reason {
		case ReasonEvidenceStale, ReasonNoHealthyAgent:
			out = append(out, "refresh_evidence")
		case ReasonEvidenceSourceMissing:
			out = append(out, "restore_required_evidence_source")
		case ReasonProfileMismatch, ReasonDriverUnsupported, ReasonCapabilityMissing:
			out = append(out, "review_runtime_profile")
		case ReasonConfigurationChanged:
			out = append(out, "review_configuration_revision")
		case ReasonMaintenanceWindowClosed:
			out = append(out, "wait_for_maintenance_window")
		case ReasonEmergencyStopActive:
			out = append(out, "resume_automation_after_safety_review")
		case ReasonIncidentConflict, ReasonSharedOwnershipConflict:
			out = append(out, "resolve_conflicting_ownership")
		case ReasonSelectorExcluded:
			out = append(out, "adjust_declared_scope")
		case ReasonApprovalMissing:
			out = append(out, "collect_required_approval")
		case ReasonAutonomyBudgetExhausted:
			out = append(out, "wait_for_autonomy_budget")
		}
	}
	return out
}

func summary(state State, reasons []ReasonCode) string {
	if len(reasons) == 0 {
		switch state {
		case StateEligible:
			return "all captured safety, identity, and evidence requirements are satisfied"
		case StateObservedOnly:
			return "evidence is available for observation, but no action authority is configured"
		default:
			return string(state)
		}
	}
	return fmt.Sprintf("%s: %s", state, strings.Join(reasonStrings(reasons), ", "))
}

func reasonStrings(reasons []ReasonCode) []string {
	out := make([]string, 0, len(reasons))
	for _, r := range reasons {
		out = append(out, string(r))
	}
	return out
}

func reportEvidence(report types.AgentAcceleratorReport) EvidenceRef {
	blob, _ := json.Marshal(report)
	digest := sha256.Sum256(blob)
	return EvidenceRef{
		Source:     "accelerator-report/" + string(report.Vendor),
		ID:         report.Node + "/" + string(report.Vendor),
		ObservedAt: report.ObservedAt.UTC(),
		Digest:     "sha256:" + hex.EncodeToString(digest[:]),
	}
}

func hasEvidenceSource(refs []EvidenceRef, source string) bool {
	for _, ref := range refs {
		if ref.Source == source {
			return true
		}
	}
	return false
}

func missingEvidenceSources(snapshot Snapshot) []string {
	if len(snapshot.RequiredEvidenceSources) == 0 {
		return nil
	}
	missing := make([]string, 0)
	for _, source := range snapshot.RequiredEvidenceSources {
		source = strings.ToLower(strings.TrimSpace(source))
		if source != "" && !evidenceSourceFresh(snapshot, source) {
			missing = append(missing, source)
		}
	}
	sort.Strings(missing)
	return missing
}

// evidenceSourceFresh refuses to turn a bare source name into authority. A
// required source must carry a timestamped evidence ref (or be one of the two
// legacy facts with an explicit timestamp on Snapshot itself). This lets an
// autonomy plan distinguish “DCGM was configured” from “a fresh DCGM-backed
// capability fact was captured for this decision.”
func evidenceSourceFresh(snapshot Snapshot, source string) bool {
	source = strings.ToLower(strings.TrimSpace(source))
	if source == "agent" && agentHealthy(snapshot) {
		return true
	}
	if snapshot.Report != nil && source == "accelerator-report/"+string(snapshot.Report.Vendor) && reportFresh(snapshot) {
		return true
	}
	maxAge := evidenceMaxAge(snapshot)
	for _, ref := range snapshot.EvidenceRefs {
		if strings.ToLower(strings.TrimSpace(ref.Source)) != source || ref.ObservedAt.IsZero() || ref.ObservedAt.After(snapshot.EvaluatedAt) {
			continue
		}
		if snapshot.EvaluatedAt.Sub(ref.ObservedAt) <= maxAge {
			return true
		}
	}
	return false
}

// ConfigDigest returns the stable digest of a selected runtime profile.  Live
// callers normally pass the digest of their complete effective configuration;
// this fallback keeps standalone previews and tests explicit and deterministic.
func ConfigDigest(profile *config.AcceleratorRuntimeProfile) string {
	if profile == nil {
		return ""
	}
	blob, _ := json.Marshal(profile)
	digest := sha256.Sum256(blob)
	return "sha256:" + hex.EncodeToString(digest[:])
}
