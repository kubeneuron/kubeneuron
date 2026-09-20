package operations

import (
	"errors"
	"fmt"
	"strings"

	"github.com/kubeneuron/kubeneuron/internal/config"
	"github.com/kubeneuron/kubeneuron/internal/decision"
	"github.com/kubeneuron/kubeneuron/pkg/types"
)

// CandidateRuntimeContractImpactVersion identifies the shape and semantics of
// a CandidateRuntimeContractImpact. Bump it when a field or enum changes
// meaning so persisted previews are never misread.
const CandidateRuntimeContractImpactVersion = "candidate-runtime-contract-impact/v1"

// RuntimeContractImpactAssessment states what kind of statement the impact
// is. There is exactly one value on purpose: a preview only ever makes a
// static, pre-deployment statement about a candidate that is not applied.
type RuntimeContractImpactAssessment string

// RuntimeContractImpactPreDeployStatic means the candidate was compared
// against captured node identity and labels only. Nothing about it has been
// deployed, attested, or qualified.
const RuntimeContractImpactPreDeployStatic RuntimeContractImpactAssessment = "PreDeployStatic"

// RuntimeContractProfileChange says whether the candidate carries a runtime
// profile change at all.
type RuntimeContractProfileChange string

const (
	// RuntimeContractProfileChangeNone means the candidate contains no
	// accelerator runtime profiles. The live profile stays the decision input
	// and there is no new runtime attestation requirement.
	RuntimeContractProfileChangeNone RuntimeContractProfileChange = "NoProfileChange"
	// RuntimeContractProfileChangeReplaced means the candidate contains one
	// or more accelerator runtime profiles that would replace the live profile
	// set if deployed.
	RuntimeContractProfileChangeReplaced RuntimeContractProfileChange = "ProfileSetReplaced"
)

// RuntimeContractStaticSelection is the outcome of selecting a candidate
// profile with only the candidate profile set, the captured node labels, and
// the captured vendor. It never consults a report or heartbeat. The Exact,
// Uncovered, Ambiguous, and Invalid values match the live coverage
// RuntimeContractSelection strings.
type RuntimeContractStaticSelection string

const (
	// RuntimeContractStaticSelectionNotEvaluated means the candidate carries
	// no profiles, so there is no candidate profile set to select from.
	RuntimeContractStaticSelectionNotEvaluated RuntimeContractStaticSelection = "NotEvaluated"
	// RuntimeContractStaticSelectionExact means exactly one candidate profile
	// selects the node and vendor.
	RuntimeContractStaticSelectionExact RuntimeContractStaticSelection = RuntimeContractStaticSelection(config.RuntimeContractSelectionExact)
	// RuntimeContractStaticSelectionUncovered means no candidate profile
	// selects the node and vendor.
	RuntimeContractStaticSelectionUncovered RuntimeContractStaticSelection = RuntimeContractStaticSelection(config.RuntimeContractSelectionUncovered)
	// RuntimeContractStaticSelectionAmbiguous means more than one candidate
	// profile selects the node and vendor.
	RuntimeContractStaticSelectionAmbiguous RuntimeContractStaticSelection = RuntimeContractStaticSelection(config.RuntimeContractSelectionAmbiguous)
	// RuntimeContractStaticSelectionInvalid means selection could not be
	// evaluated: the captured vendor is unknown or a candidate profile is
	// invalid.
	RuntimeContractStaticSelectionInvalid RuntimeContractStaticSelection = RuntimeContractStaticSelection(config.RuntimeContractSelectionInvalid)
)

// RuntimeContractPostDeployAttestation states what runtime attestation the
// node would need after the candidate is deployed. It is a requirement, never
// a claim that the requirement is met.
type RuntimeContractPostDeployAttestation string

const (
	// RuntimeContractAttestationFreshRequired means a candidate profile
	// selects the node, so a fresh post-deploy agent report attesting that
	// candidate profile is required before any decision can be Eligible.
	RuntimeContractAttestationFreshRequired RuntimeContractPostDeployAttestation = "FreshRequired"
	// RuntimeContractAttestationNotRequired means the candidate introduces
	// no runtime profile, so it adds no new attestation requirement.
	RuntimeContractAttestationNotRequired RuntimeContractPostDeployAttestation = "NotRequired"
	// RuntimeContractAttestationNotApplicable means no single candidate
	// profile selects the node, so there is no candidate profile a report
	// could attest. It is blocking for actions, never permissive.
	RuntimeContractAttestationNotApplicable RuntimeContractPostDeployAttestation = "NotApplicable"
)

// RuntimeContractAfterEvidence names exactly which profile and report the
// hypothetical After decision was evaluated from.
type RuntimeContractAfterEvidence string

const (
	// RuntimeContractAfterEvidenceLiveProfileCapturedReport means the After
	// decision kept the live snapshot profile and the captured report.
	RuntimeContractAfterEvidenceLiveProfileCapturedReport RuntimeContractAfterEvidence = "LiveProfileWithCapturedReport"
	// RuntimeContractAfterEvidenceCandidateProfileNoReport means the After
	// decision used the selected candidate profile with the captured report
	// withheld, because a pre-deploy report cannot attest a candidate.
	RuntimeContractAfterEvidenceCandidateProfileNoReport RuntimeContractAfterEvidence = "CandidateProfileWithoutReport"
	// RuntimeContractAfterEvidenceNoProfileCapturedReport means the After
	// decision used no profile with the captured report: the reduced-depth
	// observation fallback that can never authorize an action.
	RuntimeContractAfterEvidenceNoProfileCapturedReport RuntimeContractAfterEvidence = "NoProfileWithCapturedReport"
)

// RuntimeContractImpactReason is a stable machine-readable explanation of an
// impact. Impacts list reasons in the canonical order below.
type RuntimeContractImpactReason string

const (
	RuntimeContractImpactReasonNoCandidateProfiles             RuntimeContractImpactReason = "NoCandidateProfiles"
	RuntimeContractImpactReasonCandidateProfilesPresent        RuntimeContractImpactReason = "CandidateProfilesPresent"
	RuntimeContractImpactReasonVendorUnknown                   RuntimeContractImpactReason = "VendorUnknown"
	RuntimeContractImpactReasonProfileInvalid                  RuntimeContractImpactReason = "ProfileInvalid"
	RuntimeContractImpactReasonProfileOverlap                  RuntimeContractImpactReason = "ProfileOverlap"
	RuntimeContractImpactReasonProfileNotFound                 RuntimeContractImpactReason = "ProfileNotFound"
	RuntimeContractImpactReasonCapturedReportPredatesCandidate RuntimeContractImpactReason = "CapturedReportPredatesCandidate"
)

var runtimeContractImpactReasonOrder = []RuntimeContractImpactReason{
	RuntimeContractImpactReasonNoCandidateProfiles,
	RuntimeContractImpactReasonCandidateProfilesPresent,
	RuntimeContractImpactReasonVendorUnknown,
	RuntimeContractImpactReasonProfileInvalid,
	RuntimeContractImpactReasonProfileOverlap,
	RuntimeContractImpactReasonProfileNotFound,
	RuntimeContractImpactReasonCapturedReportPredatesCandidate,
}

// CandidateRuntimeContractImpact is the per-node, pre-deploy, static runtime
// contract statement inside a policy impact preview. It is a pure function of
// the candidate profile set and the captured node name, labels, and vendor;
// it never reads the captured report contents or the agent heartbeat, and it
// never claims that a candidate is deployed, attested, or qualified.
//
// CapturedReportUsableAsCandidateAttestation is always false: a report
// captured before deployment cannot attest a profile that did not exist when
// the agent produced it, even when the profile UID, digest, and versions
// happen to be identical to the live profile.
type CandidateRuntimeContractImpact struct {
	Version    string                          `json:"version"`
	Assessment RuntimeContractImpactAssessment `json:"assessment"`
	Node       string                          `json:"node"`
	Vendor     types.AcceleratorVendor         `json:"vendor,omitempty"`

	ProfileChange   RuntimeContractProfileChange   `json:"profile_change"`
	StaticSelection RuntimeContractStaticSelection `json:"static_selection"`

	// Selected candidate profile identity. Populated only when
	// StaticSelection is Exact.
	CandidateProfileName       string `json:"candidate_profile_name,omitempty"`
	CandidateProfileUID        string `json:"candidate_profile_uid,omitempty"`
	CandidateProfileGeneration int64  `json:"candidate_profile_generation,omitempty"`
	CandidateProfileDigest     string `json:"candidate_profile_digest,omitempty"`

	PostDeployAttestation                      RuntimeContractPostDeployAttestation `json:"post_deploy_attestation"`
	CapturedReportUsableAsCandidateAttestation bool                                 `json:"captured_report_usable_as_candidate_attestation"`
	AfterDecisionEvidence                      RuntimeContractAfterEvidence         `json:"after_decision_evidence"`

	Reasons []RuntimeContractImpactReason `json:"reasons,omitempty"`
	Summary string                        `json:"summary"`
}

// candidateRuntimeContract is the private result of assessing a candidate
// for one captured node: the operator-visible impact plus the selected
// candidate profile the After snapshot should carry.
type candidateRuntimeContract struct {
	impact  CandidateRuntimeContractImpact
	profile *config.AcceleratorRuntimeProfile
}

// assessCandidateRuntimeContract deterministically classifies how a
// candidate's profile set relates to one captured node. Only the candidate
// profiles, node name, node labels, and the captured vendor participate; the
// report body and the heartbeat are deliberately not inputs.
func assessCandidateRuntimeContract(before decision.Snapshot, candidate *CandidateConfiguration) candidateRuntimeContract {
	reasons := make(map[RuntimeContractImpactReason]struct{})
	impact := CandidateRuntimeContractImpact{
		Version:    CandidateRuntimeContractImpactVersion,
		Assessment: RuntimeContractImpactPreDeployStatic,
		Node:       before.Node.Name,
	}
	if before.Report != nil && before.Report.Vendor.Valid() {
		impact.Vendor = before.Report.Vendor
	}

	if len(candidate.Profiles) == 0 {
		impact.ProfileChange = RuntimeContractProfileChangeNone
		impact.StaticSelection = RuntimeContractStaticSelectionNotEvaluated
		impact.PostDeployAttestation = RuntimeContractAttestationNotRequired
		impact.AfterDecisionEvidence = RuntimeContractAfterEvidenceLiveProfileCapturedReport
		reasons[RuntimeContractImpactReasonNoCandidateProfiles] = struct{}{}
		impact.Reasons = orderRuntimeContractImpactReasons(reasons)
		impact.Summary = summarizeCandidateRuntimeContractImpact(impact)
		return candidateRuntimeContract{impact: impact}
	}

	impact.ProfileChange = RuntimeContractProfileChangeReplaced
	reasons[RuntimeContractImpactReasonCandidateProfilesPresent] = struct{}{}
	// Every profile-carrying candidate withholds the captured report from the
	// candidate decision and defaults to the non-authorizing fallback; only an
	// exact selection upgrades that to an explicit fresh-attestation
	// requirement.
	impact.PostDeployAttestation = RuntimeContractAttestationNotApplicable
	impact.AfterDecisionEvidence = RuntimeContractAfterEvidenceNoProfileCapturedReport

	var selected *config.AcceleratorRuntimeProfile
	if impact.Vendor == "" {
		impact.StaticSelection = RuntimeContractStaticSelectionInvalid
		reasons[RuntimeContractImpactReasonVendorUnknown] = struct{}{}
	} else {
		profile, err := (config.Config{AcceleratorProfiles: candidate.Profiles}).ResolveAcceleratorRuntimeProfile(before.Node.Labels, impact.Vendor)
		switch {
		case err == nil:
			copied := *profile
			selected = &copied
			impact.StaticSelection = RuntimeContractStaticSelectionExact
			impact.CandidateProfileName = profile.Name
			impact.CandidateProfileUID = profile.ProfileUID
			impact.CandidateProfileGeneration = profile.ProfileGeneration
			impact.CandidateProfileDigest = profile.ProfileDigest
			impact.PostDeployAttestation = RuntimeContractAttestationFreshRequired
			impact.AfterDecisionEvidence = RuntimeContractAfterEvidenceCandidateProfileNoReport
			reasons[RuntimeContractImpactReasonCapturedReportPredatesCandidate] = struct{}{}
		case errors.Is(err, config.ErrAmbiguousAcceleratorRuntimeProfile):
			impact.StaticSelection = RuntimeContractStaticSelectionAmbiguous
			reasons[RuntimeContractImpactReasonProfileOverlap] = struct{}{}
		case errors.Is(err, config.ErrNoAcceleratorRuntimeProfile):
			impact.StaticSelection = RuntimeContractStaticSelectionUncovered
			reasons[RuntimeContractImpactReasonProfileNotFound] = struct{}{}
		default:
			impact.StaticSelection = RuntimeContractStaticSelectionInvalid
			reasons[RuntimeContractImpactReasonProfileInvalid] = struct{}{}
		}
	}
	impact.Reasons = orderRuntimeContractImpactReasons(reasons)
	impact.Summary = summarizeCandidateRuntimeContractImpact(impact)
	return candidateRuntimeContract{impact: impact, profile: selected}
}

func orderRuntimeContractImpactReasons(set map[RuntimeContractImpactReason]struct{}) []RuntimeContractImpactReason {
	if len(set) == 0 {
		return nil
	}
	ordered := make([]RuntimeContractImpactReason, 0, len(set))
	for _, code := range runtimeContractImpactReasonOrder {
		if _, present := set[code]; present {
			ordered = append(ordered, code)
		}
	}
	return ordered
}

func summarizeCandidateRuntimeContractImpact(impact CandidateRuntimeContractImpact) string {
	var b strings.Builder
	fmt.Fprintf(&b, "pre-deploy static assessment for node %q", impact.Node)
	if impact.Vendor != "" {
		fmt.Fprintf(&b, " vendor %q", impact.Vendor)
	}
	switch impact.ProfileChange {
	case RuntimeContractProfileChangeNone:
		b.WriteString(": candidate carries no runtime profile (NoProfileChange); the live runtime profile and captured report remain the policy-impact decision input; no new post-deploy runtime attestation is required")
	default:
		fmt.Fprintf(&b, ": candidate profile set would replace the live profiles; static selection=%s", impact.StaticSelection)
		if impact.StaticSelection == RuntimeContractStaticSelectionExact {
			fmt.Fprintf(&b, " (candidate profile %q uid %q generation %d)", impact.CandidateProfileName, impact.CandidateProfileUID, impact.CandidateProfileGeneration)
		}
		fmt.Fprintf(&b, "; post-deploy attestation=%s", impact.PostDeployAttestation)
		switch impact.PostDeployAttestation {
		case RuntimeContractAttestationFreshRequired:
			b.WriteString(": the captured pre-deploy report is not candidate attestation, so the hypothetical after decision fails closed until a fresh post-deploy report attests this candidate profile")
		default:
			b.WriteString(": no single candidate profile selects this node, so the after decision uses no profile and cannot authorize an action")
		}
	}
	if len(impact.Reasons) > 0 {
		b.WriteString("; reasons: ")
		for i, code := range impact.Reasons {
			if i > 0 {
				b.WriteString(", ")
			}
			b.WriteString(string(code))
		}
	}
	return b.String()
}
