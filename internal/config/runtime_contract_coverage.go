package config

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/kubeneuron/kubeneuron/pkg/types"
)

// RuntimeContractCoverageVersion identifies the shape and semantics of a
// RuntimeContractCoverage result. Bump it when a field or enum changes
// meaning so persisted or logged results are never misread.
const RuntimeContractCoverageVersion = "runtime-contract-coverage/v1"

// DefaultRuntimeContractAgentMaxAge bounds how old an agent heartbeat can be
// and still count as a usable liveness signal when the caller supplies no
// explicit bound. It mirrors the decision package default.
const DefaultRuntimeContractAgentMaxAge = 10 * time.Minute

// RuntimeContractSelection describes how the configured accelerator runtime
// profiles relate to one node and vendor.
type RuntimeContractSelection string

const (
	// RuntimeContractSelectionExact means exactly one valid profile selects
	// the node and vendor.
	RuntimeContractSelectionExact RuntimeContractSelection = "Exact"
	// RuntimeContractSelectionUncovered means every configured profile is
	// valid but none selects the node and vendor.
	RuntimeContractSelectionUncovered RuntimeContractSelection = "Uncovered"
	// RuntimeContractSelectionAmbiguous means more than one valid profile
	// selects the node and vendor.
	RuntimeContractSelectionAmbiguous RuntimeContractSelection = "Ambiguous"
	// RuntimeContractSelectionInvalid means selection could not be evaluated:
	// a configured profile fails validation or the input itself is unusable.
	RuntimeContractSelectionInvalid RuntimeContractSelection = "Invalid"
)

// RuntimeContractAttestation describes whether the agent's accelerator
// report attests the selected profile.
type RuntimeContractAttestation string

const (
	// RuntimeContractAttestationFreshCompatible means the report satisfies
	// AcceleratorRuntimeProfile.CheckReport for the selected profile.
	RuntimeContractAttestationFreshCompatible RuntimeContractAttestation = "FreshCompatible"
	// RuntimeContractAttestationMissing means a profile was selected but no
	// report was supplied.
	RuntimeContractAttestationMissing RuntimeContractAttestation = "Missing"
	// RuntimeContractAttestationStale means the report is compatible but its
	// observation time is outside the profile's max_report_age.
	RuntimeContractAttestationStale RuntimeContractAttestation = "Stale"
	// RuntimeContractAttestationMismatch means the report exists but does not
	// attest the selected profile: wrong node, vendor, digest, revision,
	// versions, an invalid envelope, or a runtime that is not ready.
	RuntimeContractAttestationMismatch RuntimeContractAttestation = "Mismatch"
	// RuntimeContractAttestationNotApplicable means no single profile was
	// selected, so there is nothing for a report to attest.
	RuntimeContractAttestationNotApplicable RuntimeContractAttestation = "NotApplicable"
)

// RuntimeContractVerificationDepth describes how much of the runtime
// contract can currently be verified for the node. It is an observation
// about evidence; it never authorizes an effect.
type RuntimeContractVerificationDepth string

const (
	// RuntimeContractVerificationFull means an exact profile is attested by a
	// fresh, compatible, ready report and the agent heartbeat is usable. A
	// Full result never carries reasons.
	RuntimeContractVerificationFull RuntimeContractVerificationDepth = "Full"
	// RuntimeContractVerificationReduced means the agent is alive but no
	// profile-level attestation is available.
	RuntimeContractVerificationReduced RuntimeContractVerificationDepth = "Reduced"
	// RuntimeContractVerificationUnavailable means nothing about the runtime
	// contract can be verified right now.
	RuntimeContractVerificationUnavailable RuntimeContractVerificationDepth = "Unavailable"
)

// RuntimeContractReasonCode is a stable machine-readable explanation for a
// coverage result. Results list codes in the canonical order below.
type RuntimeContractReasonCode string

const (
	RuntimeContractReasonEvaluationTimeMissing         RuntimeContractReasonCode = "EvaluationTimeMissing"
	RuntimeContractReasonNodeIdentityMissing           RuntimeContractReasonCode = "NodeIdentityMissing"
	RuntimeContractReasonVendorInvalid                 RuntimeContractReasonCode = "VendorInvalid"
	RuntimeContractReasonProfileInvalid                RuntimeContractReasonCode = "ProfileInvalid"
	RuntimeContractReasonProfileOverlap                RuntimeContractReasonCode = "ProfileOverlap"
	RuntimeContractReasonProfileNotFound               RuntimeContractReasonCode = "ProfileNotFound"
	RuntimeContractReasonReportMissing                 RuntimeContractReasonCode = "ReportMissing"
	RuntimeContractReasonReportInvalid                 RuntimeContractReasonCode = "ReportInvalid"
	RuntimeContractReasonReportNodeMismatch            RuntimeContractReasonCode = "ReportNodeMismatch"
	RuntimeContractReasonReportVendorMismatch          RuntimeContractReasonCode = "ReportVendorMismatch"
	RuntimeContractReasonReportProfileDigestMismatch   RuntimeContractReasonCode = "ReportProfileDigestMismatch"
	RuntimeContractReasonReportProfileRevisionMismatch RuntimeContractReasonCode = "ReportProfileRevisionMismatch"
	RuntimeContractReasonReportDriverVersionMismatch   RuntimeContractReasonCode = "ReportDriverVersionMismatch"
	RuntimeContractReasonReportRuntimeVersionMismatch  RuntimeContractReasonCode = "ReportRuntimeVersionMismatch"
	RuntimeContractReasonReportObservedInFuture        RuntimeContractReasonCode = "ReportObservedInFuture"
	RuntimeContractReasonReportStale                   RuntimeContractReasonCode = "ReportStale"
	RuntimeContractReasonReportNotReady                RuntimeContractReasonCode = "ReportNotReady"
	RuntimeContractReasonReportRejected                RuntimeContractReasonCode = "ReportRejected"
	RuntimeContractReasonAgentNeverSeen                RuntimeContractReasonCode = "AgentNeverSeen"
	RuntimeContractReasonAgentHeartbeatInFuture        RuntimeContractReasonCode = "AgentHeartbeatInFuture"
	RuntimeContractReasonAgentHeartbeatStale           RuntimeContractReasonCode = "AgentHeartbeatStale"
)

// runtimeContractReasonOrder is the canonical emission order for reason
// codes. Every code must appear here exactly once.
var runtimeContractReasonOrder = []RuntimeContractReasonCode{
	RuntimeContractReasonEvaluationTimeMissing,
	RuntimeContractReasonNodeIdentityMissing,
	RuntimeContractReasonVendorInvalid,
	RuntimeContractReasonProfileInvalid,
	RuntimeContractReasonProfileOverlap,
	RuntimeContractReasonProfileNotFound,
	RuntimeContractReasonReportMissing,
	RuntimeContractReasonReportInvalid,
	RuntimeContractReasonReportNodeMismatch,
	RuntimeContractReasonReportVendorMismatch,
	RuntimeContractReasonReportProfileDigestMismatch,
	RuntimeContractReasonReportProfileRevisionMismatch,
	RuntimeContractReasonReportDriverVersionMismatch,
	RuntimeContractReasonReportRuntimeVersionMismatch,
	RuntimeContractReasonReportObservedInFuture,
	RuntimeContractReasonReportStale,
	RuntimeContractReasonReportNotReady,
	RuntimeContractReasonReportRejected,
	RuntimeContractReasonAgentNeverSeen,
	RuntimeContractReasonAgentHeartbeatInFuture,
	RuntimeContractReasonAgentHeartbeatStale,
}

// RuntimeContractCoverageInput is everything AssessRuntimeContractCoverage
// looks at. It is read-only: the assessment never mutates it.
type RuntimeContractCoverageInput struct {
	// Now is the evaluation instant. A zero value makes the input invalid.
	Now time.Time
	// ConfigDigest identifies the compiled configuration the profiles came
	// from. It is echoed into the result so a consumer can tie a coverage
	// result to the exact configuration it was computed against.
	ConfigDigest string
	// NodeName and NodeUID identify the node under assessment. NodeName is
	// required. NodeUID must equal the report's NodeUID exactly, mirroring
	// the reset admission rule: both may be empty for legacy or baremetal
	// input, but a blank UID never matches a nonblank one.
	NodeName string
	NodeUID  string
	// NodeLabels feed profile selection with matchLabels semantics.
	NodeLabels map[string]string
	// Vendor is the accelerator vendor whose contract is being assessed.
	Vendor types.AcceleratorVendor
	// AgentLastSeen is the most recent agent heartbeat. Zero means the agent
	// has never registered.
	AgentLastSeen time.Time
	// AgentMaxAge bounds a usable heartbeat. Nonpositive selects
	// DefaultRuntimeContractAgentMaxAge.
	AgentMaxAge time.Duration
	// Report is the latest accelerator report for the vendor, if any.
	Report *types.AgentAcceleratorReport
	// Profiles are the configured accelerator runtime profiles.
	Profiles []AcceleratorRuntimeProfile
}

// RuntimeContractCoverage is the pure result of assessing one node's runtime
// contract coverage. It classifies evidence and does not decide whether any
// effect is authorized.
//
// The JSON shape is part of the read-only operator API contract. NodeUID is
// always present, even when blank, so a consumer can see exactly which node
// identity the assessment matched a report against. Zero timestamps are
// omitted rather than rendered as the Go zero time.
type RuntimeContractCoverage struct {
	Version      string                  `json:"version"`
	ConfigDigest string                  `json:"config_digest"`
	EvaluatedAt  time.Time               `json:"evaluated_at"`
	NodeName     string                  `json:"node_name"`
	NodeUID      string                  `json:"node_uid"`
	Vendor       types.AcceleratorVendor `json:"vendor"`

	// Selected profile identity. Populated only when Selection is Exact.
	ProfileName       string `json:"profile_name,omitempty"`
	ProfileUID        string `json:"profile_uid,omitempty"`
	ProfileGeneration int64  `json:"profile_generation,omitempty"`
	ProfileDigest     string `json:"profile_digest,omitempty"`

	Selection         RuntimeContractSelection         `json:"selection"`
	Attestation       RuntimeContractAttestation       `json:"attestation"`
	VerificationDepth RuntimeContractVerificationDepth `json:"verification_depth"`

	// Reasons explain why coverage is less than Full, in canonical order.
	// A Full result has no reasons.
	Reasons []RuntimeContractReasonCode `json:"reasons,omitempty"`
	// Summary is a short human-readable rendering of the axes and reasons.
	Summary string `json:"summary"`

	// AgentLastSeen and ReportObservedAt are the timestamps the assessment
	// observed. ReportObservedAt is zero when no report was supplied.
	AgentLastSeen    time.Time `json:"agent_last_seen,omitzero"`
	ReportObservedAt time.Time `json:"report_observed_at,omitzero"`
}

// AssessRuntimeContractCoverage deterministically classifies how well the
// configured runtime contract covers one node and vendor. It is a pure
// function of its input.
//
// Verification depth is derived from the other two axes and the agent
// heartbeat:
//   - Exact selection with a FreshCompatible report and a usable agent
//     heartbeat is Full.
//   - Exact selection with a FreshCompatible report but an unusable agent
//     heartbeat is Unavailable: the evidence contradicts itself.
//   - Uncovered selection, or Exact selection without a fresh compatible
//     report, is Reduced when the agent heartbeat is usable and Unavailable
//     otherwise.
//   - Ambiguous or Invalid selection is always Unavailable.
func AssessRuntimeContractCoverage(in RuntimeContractCoverageInput) RuntimeContractCoverage {
	reasons := make(map[RuntimeContractReasonCode]struct{})
	result := RuntimeContractCoverage{
		Version:           RuntimeContractCoverageVersion,
		ConfigDigest:      in.ConfigDigest,
		EvaluatedAt:       in.Now,
		NodeName:          in.NodeName,
		NodeUID:           in.NodeUID,
		Vendor:            in.Vendor,
		Attestation:       RuntimeContractAttestationNotApplicable,
		VerificationDepth: RuntimeContractVerificationUnavailable,
		AgentLastSeen:     in.AgentLastSeen,
	}
	if in.Report != nil {
		result.ReportObservedAt = in.Report.ObservedAt
	}

	// Input validity.
	inputValid := true
	if in.Now.IsZero() {
		reasons[RuntimeContractReasonEvaluationTimeMissing] = struct{}{}
		inputValid = false
	}
	if strings.TrimSpace(in.NodeName) == "" {
		reasons[RuntimeContractReasonNodeIdentityMissing] = struct{}{}
		inputValid = false
	}
	if !in.Vendor.Valid() {
		reasons[RuntimeContractReasonVendorInvalid] = struct{}{}
		inputValid = false
	}

	// Agent heartbeat is assessed whenever time is usable, so the reason
	// codes describe the node even when selection fails.
	agentHealthy := false
	if !in.Now.IsZero() {
		agentHealthy = assessRuntimeContractHeartbeat(in, reasons)
	}

	// Selection.
	var selected *AcceleratorRuntimeProfile
	if !inputValid {
		result.Selection = RuntimeContractSelectionInvalid
	} else {
		profile, err := Config{AcceleratorProfiles: in.Profiles}.ResolveAcceleratorRuntimeProfile(in.NodeLabels, in.Vendor)
		switch {
		case err == nil:
			result.Selection = RuntimeContractSelectionExact
			copied := *profile
			selected = &copied
			result.ProfileName = profile.Name
			result.ProfileUID = profile.ProfileUID
			result.ProfileGeneration = profile.ProfileGeneration
			result.ProfileDigest = profile.ProfileDigest
		case errors.Is(err, ErrAmbiguousAcceleratorRuntimeProfile):
			result.Selection = RuntimeContractSelectionAmbiguous
			reasons[RuntimeContractReasonProfileOverlap] = struct{}{}
		case errors.Is(err, ErrNoAcceleratorRuntimeProfile):
			result.Selection = RuntimeContractSelectionUncovered
			reasons[RuntimeContractReasonProfileNotFound] = struct{}{}
		default:
			result.Selection = RuntimeContractSelectionInvalid
			reasons[RuntimeContractReasonProfileInvalid] = struct{}{}
		}
	}

	// Attestation.
	if selected != nil {
		result.Attestation = assessRuntimeContractAttestation(in, *selected, reasons)
	}

	// Verification depth. Full requires every finding to be clear: an exact
	// selection, a fresh compatible report, and a usable agent heartbeat. A
	// fresh report from an agent whose heartbeat is stale or in the future is
	// contradictory evidence, so it stays Unavailable rather than Reduced.
	switch {
	case result.Selection == RuntimeContractSelectionExact && result.Attestation == RuntimeContractAttestationFreshCompatible:
		if agentHealthy {
			result.VerificationDepth = RuntimeContractVerificationFull
		}
	case result.Selection == RuntimeContractSelectionExact || result.Selection == RuntimeContractSelectionUncovered:
		if agentHealthy {
			result.VerificationDepth = RuntimeContractVerificationReduced
		}
	}

	result.Reasons = orderRuntimeContractReasons(reasons)
	result.Summary = summarizeRuntimeContractCoverage(result)
	return result
}

func assessRuntimeContractHeartbeat(in RuntimeContractCoverageInput, reasons map[RuntimeContractReasonCode]struct{}) bool {
	if in.AgentLastSeen.IsZero() {
		reasons[RuntimeContractReasonAgentNeverSeen] = struct{}{}
		return false
	}
	if in.AgentLastSeen.After(in.Now) {
		reasons[RuntimeContractReasonAgentHeartbeatInFuture] = struct{}{}
		return false
	}
	maxAge := in.AgentMaxAge
	if maxAge <= 0 {
		maxAge = DefaultRuntimeContractAgentMaxAge
	}
	if in.Now.Sub(in.AgentLastSeen) > maxAge {
		reasons[RuntimeContractReasonAgentHeartbeatStale] = struct{}{}
		return false
	}
	return true
}

// assessRuntimeContractAttestation classifies the report against the
// selected profile. It records every observable discrepancy rather than only
// the first, then defers to CheckReport as the authoritative gate before
// reporting FreshCompatible.
func assessRuntimeContractAttestation(in RuntimeContractCoverageInput, profile AcceleratorRuntimeProfile, reasons map[RuntimeContractReasonCode]struct{}) RuntimeContractAttestation {
	if in.Report == nil {
		reasons[RuntimeContractReasonReportMissing] = struct{}{}
		return RuntimeContractAttestationMissing
	}
	report := *in.Report
	if err := report.Validate(); err != nil {
		reasons[RuntimeContractReasonReportInvalid] = struct{}{}
		return RuntimeContractAttestationMismatch
	}

	mismatch := false
	stale := false
	// Node identity uses the same exact-equality rule as reset admission
	// (checkNVIDIAResetEvidence): a report from a prior Node object that
	// reused the name, or one missing a UID the current node has, does not
	// attest this node.
	if report.Node != in.NodeName || report.NodeUID != in.NodeUID {
		reasons[RuntimeContractReasonReportNodeMismatch] = struct{}{}
		mismatch = true
	}
	if report.Vendor != profile.Vendor {
		reasons[RuntimeContractReasonReportVendorMismatch] = struct{}{}
		mismatch = true
	}
	if report.ProfileDigest != profile.ProfileDigest {
		reasons[RuntimeContractReasonReportProfileDigestMismatch] = struct{}{}
		mismatch = true
	}
	if report.ProfileUID != profile.ProfileUID || report.ProfileGeneration != profile.ProfileGeneration {
		reasons[RuntimeContractReasonReportProfileRevisionMismatch] = struct{}{}
		mismatch = true
	}
	if report.DriverVersion != profile.DriverVersion {
		reasons[RuntimeContractReasonReportDriverVersionMismatch] = struct{}{}
		mismatch = true
	}
	if !RuntimeVersionSatisfies(report.RuntimeVersion, profile.RuntimeVersion) {
		reasons[RuntimeContractReasonReportRuntimeVersionMismatch] = struct{}{}
		mismatch = true
	}
	if report.ObservedAt.After(in.Now) {
		reasons[RuntimeContractReasonReportObservedInFuture] = struct{}{}
		stale = true
	} else if in.Now.Sub(report.ObservedAt) > profile.MaxReportAge.Std() {
		reasons[RuntimeContractReasonReportStale] = struct{}{}
		stale = true
	}
	if report.Readiness != types.AcceleratorReadinessReady {
		reasons[RuntimeContractReasonReportNotReady] = struct{}{}
		mismatch = true
	}
	switch {
	case mismatch:
		return RuntimeContractAttestationMismatch
	case stale:
		return RuntimeContractAttestationStale
	}
	if err := profile.CheckReport(in.Now, report); err != nil {
		reasons[RuntimeContractReasonReportRejected] = struct{}{}
		return RuntimeContractAttestationMismatch
	}
	return RuntimeContractAttestationFreshCompatible
}

func orderRuntimeContractReasons(set map[RuntimeContractReasonCode]struct{}) []RuntimeContractReasonCode {
	if len(set) == 0 {
		return nil
	}
	ordered := make([]RuntimeContractReasonCode, 0, len(set))
	for _, code := range runtimeContractReasonOrder {
		if _, present := set[code]; present {
			ordered = append(ordered, code)
		}
	}
	return ordered
}

func summarizeRuntimeContractCoverage(r RuntimeContractCoverage) string {
	var b strings.Builder
	fmt.Fprintf(&b, "node %q vendor %q: selection=%s", r.NodeName, r.Vendor, r.Selection)
	if r.Selection == RuntimeContractSelectionExact {
		fmt.Fprintf(&b, " (profile %q uid %q generation %d)", r.ProfileName, r.ProfileUID, r.ProfileGeneration)
	}
	fmt.Fprintf(&b, ", attestation=%s, verification=%s", r.Attestation, r.VerificationDepth)
	if len(r.Reasons) == 0 {
		b.WriteString("; no findings")
		return b.String()
	}
	b.WriteString("; reasons: ")
	for i, code := range r.Reasons {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(string(code))
	}
	return b.String()
}
