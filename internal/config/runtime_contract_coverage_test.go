package config

import (
	"reflect"
	"testing"
	"time"

	"github.com/kubeneuron/kubeneuron/pkg/types"
)

func coverageInput(now time.Time) RuntimeContractCoverageInput {
	report := readyNVIDIAReport(now.Add(-5 * time.Minute))
	report.NodeUID = "node-uid-1"
	return RuntimeContractCoverageInput{
		Now:           now,
		ConfigDigest:  "sha256:config",
		NodeName:      "gpu-node-1",
		NodeUID:       "node-uid-1",
		NodeLabels:    map[string]string{"accelerator": "nvidia", "pool": "a100"},
		Vendor:        types.AcceleratorVendorNVIDIA,
		AgentLastSeen: now.Add(-time.Minute),
		Report:        &report,
		Profiles:      []AcceleratorRuntimeProfile{validNVIDIAProfile()},
	}
}

func TestAssessRuntimeContractCoverage(t *testing.T) {
	now := time.Date(2026, time.September, 9, 12, 0, 0, 0, time.UTC)

	tests := []struct {
		name          string
		mutate        func(*RuntimeContractCoverageInput)
		selection     RuntimeContractSelection
		attestation   RuntimeContractAttestation
		depth         RuntimeContractVerificationDepth
		reasons       []RuntimeContractReasonCode
		wantProfile   bool
		wantReportObs bool
	}{
		{
			name:          "fresh exact",
			mutate:        func(*RuntimeContractCoverageInput) {},
			selection:     RuntimeContractSelectionExact,
			attestation:   RuntimeContractAttestationFreshCompatible,
			depth:         RuntimeContractVerificationFull,
			wantProfile:   true,
			wantReportObs: true,
		},
		{
			name: "fresh exact stale heartbeat",
			mutate: func(in *RuntimeContractCoverageInput) {
				in.AgentLastSeen = now.Add(-time.Hour)
			},
			selection:     RuntimeContractSelectionExact,
			attestation:   RuntimeContractAttestationFreshCompatible,
			depth:         RuntimeContractVerificationUnavailable,
			reasons:       []RuntimeContractReasonCode{RuntimeContractReasonAgentHeartbeatStale},
			wantProfile:   true,
			wantReportObs: true,
		},
		{
			name: "fresh exact future heartbeat",
			mutate: func(in *RuntimeContractCoverageInput) {
				in.AgentLastSeen = now.Add(time.Minute)
			},
			selection:     RuntimeContractSelectionExact,
			attestation:   RuntimeContractAttestationFreshCompatible,
			depth:         RuntimeContractVerificationUnavailable,
			reasons:       []RuntimeContractReasonCode{RuntimeContractReasonAgentHeartbeatInFuture},
			wantProfile:   true,
			wantReportObs: true,
		},
		{
			name: "fresh exact never seen",
			mutate: func(in *RuntimeContractCoverageInput) {
				in.AgentLastSeen = time.Time{}
			},
			selection:     RuntimeContractSelectionExact,
			attestation:   RuntimeContractAttestationFreshCompatible,
			depth:         RuntimeContractVerificationUnavailable,
			reasons:       []RuntimeContractReasonCode{RuntimeContractReasonAgentNeverSeen},
			wantProfile:   true,
			wantReportObs: true,
		},
		{
			name: "no profile healthy heartbeat",
			mutate: func(in *RuntimeContractCoverageInput) {
				in.Profiles = nil
				in.Report = nil
			},
			selection:   RuntimeContractSelectionUncovered,
			attestation: RuntimeContractAttestationNotApplicable,
			depth:       RuntimeContractVerificationReduced,
			reasons:     []RuntimeContractReasonCode{RuntimeContractReasonProfileNotFound},
		},
		{
			name: "no profile stale heartbeat",
			mutate: func(in *RuntimeContractCoverageInput) {
				in.Profiles = nil
				in.Report = nil
				in.AgentLastSeen = now.Add(-time.Hour)
			},
			selection:   RuntimeContractSelectionUncovered,
			attestation: RuntimeContractAttestationNotApplicable,
			depth:       RuntimeContractVerificationUnavailable,
			reasons: []RuntimeContractReasonCode{
				RuntimeContractReasonProfileNotFound,
				RuntimeContractReasonAgentHeartbeatStale,
			},
		},
		{
			name: "no profile never seen",
			mutate: func(in *RuntimeContractCoverageInput) {
				in.Profiles = nil
				in.Report = nil
				in.AgentLastSeen = time.Time{}
			},
			selection:   RuntimeContractSelectionUncovered,
			attestation: RuntimeContractAttestationNotApplicable,
			depth:       RuntimeContractVerificationUnavailable,
			reasons: []RuntimeContractReasonCode{
				RuntimeContractReasonProfileNotFound,
				RuntimeContractReasonAgentNeverSeen,
			},
		},
		{
			name: "overlap",
			mutate: func(in *RuntimeContractCoverageInput) {
				second := validNVIDIAProfile()
				second.Name = "nvidia-a100-second"
				second.NodeSelector = map[string]string{"pool": "a100"}
				in.Profiles = append(in.Profiles, second)
			},
			selection:     RuntimeContractSelectionAmbiguous,
			attestation:   RuntimeContractAttestationNotApplicable,
			depth:         RuntimeContractVerificationUnavailable,
			reasons:       []RuntimeContractReasonCode{RuntimeContractReasonProfileOverlap},
			wantReportObs: true,
		},
		{
			name: "invalid profile",
			mutate: func(in *RuntimeContractCoverageInput) {
				in.Profiles[0].ProfileDigest = "latest"
			},
			selection:     RuntimeContractSelectionInvalid,
			attestation:   RuntimeContractAttestationNotApplicable,
			depth:         RuntimeContractVerificationUnavailable,
			reasons:       []RuntimeContractReasonCode{RuntimeContractReasonProfileInvalid},
			wantReportObs: true,
		},
		{
			name: "invalid profile not selecting this node",
			mutate: func(in *RuntimeContractCoverageInput) {
				broken := validNVIDIAProfile()
				broken.Name = "broken"
				broken.NodeSelector = map[string]string{"pool": "h100"}
				broken.MaxReportAge = 0
				in.Profiles = append(in.Profiles, broken)
			},
			selection:     RuntimeContractSelectionInvalid,
			attestation:   RuntimeContractAttestationNotApplicable,
			depth:         RuntimeContractVerificationUnavailable,
			reasons:       []RuntimeContractReasonCode{RuntimeContractReasonProfileInvalid},
			wantReportObs: true,
		},
		{
			name:        "missing report",
			mutate:      func(in *RuntimeContractCoverageInput) { in.Report = nil },
			selection:   RuntimeContractSelectionExact,
			attestation: RuntimeContractAttestationMissing,
			depth:       RuntimeContractVerificationReduced,
			reasons:     []RuntimeContractReasonCode{RuntimeContractReasonReportMissing},
			wantProfile: true,
		},
		{
			name: "stale report",
			mutate: func(in *RuntimeContractCoverageInput) {
				in.Report.ObservedAt = now.Add(-11 * time.Minute)
			},
			selection:     RuntimeContractSelectionExact,
			attestation:   RuntimeContractAttestationStale,
			depth:         RuntimeContractVerificationReduced,
			reasons:       []RuntimeContractReasonCode{RuntimeContractReasonReportStale},
			wantProfile:   true,
			wantReportObs: true,
		},
		{
			name: "stale report and stale heartbeat",
			mutate: func(in *RuntimeContractCoverageInput) {
				in.Report.ObservedAt = now.Add(-11 * time.Minute)
				in.AgentLastSeen = now.Add(-11 * time.Minute)
			},
			selection:   RuntimeContractSelectionExact,
			attestation: RuntimeContractAttestationStale,
			depth:       RuntimeContractVerificationUnavailable,
			reasons: []RuntimeContractReasonCode{
				RuntimeContractReasonReportStale,
				RuntimeContractReasonAgentHeartbeatStale,
			},
			wantProfile:   true,
			wantReportObs: true,
		},
		{
			name: "profile digest mismatch",
			mutate: func(in *RuntimeContractCoverageInput) {
				in.Report.ProfileDigest = "sha256:ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"
			},
			selection:     RuntimeContractSelectionExact,
			attestation:   RuntimeContractAttestationMismatch,
			depth:         RuntimeContractVerificationReduced,
			reasons:       []RuntimeContractReasonCode{RuntimeContractReasonReportProfileDigestMismatch},
			wantProfile:   true,
			wantReportObs: true,
		},
		{
			name: "profile UID generation mismatch",
			mutate: func(in *RuntimeContractCoverageInput) {
				in.Report.ProfileUID = "profile-uid-old"
				in.Report.ProfileGeneration = 7
			},
			selection:     RuntimeContractSelectionExact,
			attestation:   RuntimeContractAttestationMismatch,
			depth:         RuntimeContractVerificationReduced,
			reasons:       []RuntimeContractReasonCode{RuntimeContractReasonReportProfileRevisionMismatch},
			wantProfile:   true,
			wantReportObs: true,
		},
		{
			name: "not-ready report",
			mutate: func(in *RuntimeContractCoverageInput) {
				in.Report.Readiness = types.AcceleratorReadinessNotReady
				in.Report.ReadinessReasons = []string{"dcgm unreachable"}
			},
			selection:     RuntimeContractSelectionExact,
			attestation:   RuntimeContractAttestationMismatch,
			depth:         RuntimeContractVerificationReduced,
			reasons:       []RuntimeContractReasonCode{RuntimeContractReasonReportNotReady},
			wantProfile:   true,
			wantReportObs: true,
		},
		{
			name: "report for another node",
			mutate: func(in *RuntimeContractCoverageInput) {
				in.Report.Node = "gpu-node-2"
			},
			selection:     RuntimeContractSelectionExact,
			attestation:   RuntimeContractAttestationMismatch,
			depth:         RuntimeContractVerificationReduced,
			reasons:       []RuntimeContractReasonCode{RuntimeContractReasonReportNodeMismatch},
			wantProfile:   true,
			wantReportObs: true,
		},
		{
			name: "report node UID mismatch",
			mutate: func(in *RuntimeContractCoverageInput) {
				in.Report.NodeUID = "node-uid-recreated"
			},
			selection:     RuntimeContractSelectionExact,
			attestation:   RuntimeContractAttestationMismatch,
			depth:         RuntimeContractVerificationReduced,
			reasons:       []RuntimeContractReasonCode{RuntimeContractReasonReportNodeMismatch},
			wantProfile:   true,
			wantReportObs: true,
		},
		{
			name: "report node UID blank while node has UID",
			mutate: func(in *RuntimeContractCoverageInput) {
				in.Report.NodeUID = ""
			},
			selection:     RuntimeContractSelectionExact,
			attestation:   RuntimeContractAttestationMismatch,
			depth:         RuntimeContractVerificationReduced,
			reasons:       []RuntimeContractReasonCode{RuntimeContractReasonReportNodeMismatch},
			wantProfile:   true,
			wantReportObs: true,
		},
		{
			name: "node UID blank while report has UID",
			mutate: func(in *RuntimeContractCoverageInput) {
				in.NodeUID = ""
			},
			selection:     RuntimeContractSelectionExact,
			attestation:   RuntimeContractAttestationMismatch,
			depth:         RuntimeContractVerificationReduced,
			reasons:       []RuntimeContractReasonCode{RuntimeContractReasonReportNodeMismatch},
			wantProfile:   true,
			wantReportObs: true,
		},
		{
			name: "both node UIDs blank remain equal",
			mutate: func(in *RuntimeContractCoverageInput) {
				in.NodeUID = ""
				in.Report.NodeUID = ""
			},
			selection:     RuntimeContractSelectionExact,
			attestation:   RuntimeContractAttestationFreshCompatible,
			depth:         RuntimeContractVerificationFull,
			wantProfile:   true,
			wantReportObs: true,
		},
		{
			name: "report vendor mismatch",
			mutate: func(in *RuntimeContractCoverageInput) {
				in.Report.Vendor = types.AcceleratorVendorAMD
			},
			selection:     RuntimeContractSelectionExact,
			attestation:   RuntimeContractAttestationMismatch,
			depth:         RuntimeContractVerificationReduced,
			reasons:       []RuntimeContractReasonCode{RuntimeContractReasonReportVendorMismatch},
			wantProfile:   true,
			wantReportObs: true,
		},
		{
			name: "runtime version mismatch",
			mutate: func(in *RuntimeContractCoverageInput) {
				in.Report.RuntimeVersion = "dcgm-3.9"
			},
			selection:     RuntimeContractSelectionExact,
			attestation:   RuntimeContractAttestationMismatch,
			depth:         RuntimeContractVerificationReduced,
			reasons:       []RuntimeContractReasonCode{RuntimeContractReasonReportRuntimeVersionMismatch},
			wantProfile:   true,
			wantReportObs: true,
		},
		{
			name: "report observed in the future",
			mutate: func(in *RuntimeContractCoverageInput) {
				in.Report.ObservedAt = now.Add(time.Minute)
			},
			selection:     RuntimeContractSelectionExact,
			attestation:   RuntimeContractAttestationStale,
			depth:         RuntimeContractVerificationReduced,
			reasons:       []RuntimeContractReasonCode{RuntimeContractReasonReportObservedInFuture},
			wantProfile:   true,
			wantReportObs: true,
		},
		{
			name: "agent max age shorter than default marks heartbeat stale",
			mutate: func(in *RuntimeContractCoverageInput) {
				in.AgentMaxAge = 30 * time.Second
				in.AgentLastSeen = now.Add(-time.Minute)
			},
			selection:     RuntimeContractSelectionExact,
			attestation:   RuntimeContractAttestationFreshCompatible,
			depth:         RuntimeContractVerificationUnavailable,
			reasons:       []RuntimeContractReasonCode{RuntimeContractReasonAgentHeartbeatStale},
			wantProfile:   true,
			wantReportObs: true,
		},
		{
			name: "agent max age shorter than default still accepts a recent heartbeat",
			mutate: func(in *RuntimeContractCoverageInput) {
				in.AgentMaxAge = 30 * time.Second
				in.AgentLastSeen = now.Add(-10 * time.Second)
			},
			selection:     RuntimeContractSelectionExact,
			attestation:   RuntimeContractAttestationFreshCompatible,
			depth:         RuntimeContractVerificationFull,
			wantProfile:   true,
			wantReportObs: true,
		},
		{
			name: "invalid report envelope",
			mutate: func(in *RuntimeContractCoverageInput) {
				in.Report.Readiness = "unknown"
			},
			selection:     RuntimeContractSelectionExact,
			attestation:   RuntimeContractAttestationMismatch,
			depth:         RuntimeContractVerificationReduced,
			reasons:       []RuntimeContractReasonCode{RuntimeContractReasonReportInvalid},
			wantProfile:   true,
			wantReportObs: true,
		},
		{
			name:          "invalid vendor",
			mutate:        func(in *RuntimeContractCoverageInput) { in.Vendor = "quantum" },
			selection:     RuntimeContractSelectionInvalid,
			attestation:   RuntimeContractAttestationNotApplicable,
			depth:         RuntimeContractVerificationUnavailable,
			reasons:       []RuntimeContractReasonCode{RuntimeContractReasonVendorInvalid},
			wantReportObs: true,
		},
		{
			name: "unsupported vendor without a contract",
			mutate: func(in *RuntimeContractCoverageInput) {
				in.Vendor = types.AcceleratorVendorAMD
				in.Report = nil
			},
			selection:   RuntimeContractSelectionUncovered,
			attestation: RuntimeContractAttestationNotApplicable,
			depth:       RuntimeContractVerificationReduced,
			reasons:     []RuntimeContractReasonCode{RuntimeContractReasonProfileNotFound},
		},
		{
			name: "zero evaluation time",
			mutate: func(in *RuntimeContractCoverageInput) {
				in.Now = time.Time{}
			},
			selection:     RuntimeContractSelectionInvalid,
			attestation:   RuntimeContractAttestationNotApplicable,
			depth:         RuntimeContractVerificationUnavailable,
			reasons:       []RuntimeContractReasonCode{RuntimeContractReasonEvaluationTimeMissing},
			wantReportObs: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			in := coverageInput(now)
			tc.mutate(&in)
			got := AssessRuntimeContractCoverage(in)

			if got.Version != RuntimeContractCoverageVersion {
				t.Errorf("Version = %q, want %q", got.Version, RuntimeContractCoverageVersion)
			}
			if got.ConfigDigest != in.ConfigDigest {
				t.Errorf("ConfigDigest = %q, want %q", got.ConfigDigest, in.ConfigDigest)
			}
			if !got.EvaluatedAt.Equal(in.Now) {
				t.Errorf("EvaluatedAt = %s, want %s", got.EvaluatedAt, in.Now)
			}
			if got.NodeName != in.NodeName || got.NodeUID != in.NodeUID {
				t.Errorf("node identity = %q/%q, want %q/%q", got.NodeName, got.NodeUID, in.NodeName, in.NodeUID)
			}
			if got.Vendor != in.Vendor {
				t.Errorf("Vendor = %q, want %q", got.Vendor, in.Vendor)
			}
			if got.Selection != tc.selection {
				t.Errorf("Selection = %q, want %q", got.Selection, tc.selection)
			}
			if got.Attestation != tc.attestation {
				t.Errorf("Attestation = %q, want %q", got.Attestation, tc.attestation)
			}
			if got.VerificationDepth != tc.depth {
				t.Errorf("VerificationDepth = %q, want %q", got.VerificationDepth, tc.depth)
			}
			if !reflect.DeepEqual(got.Reasons, tc.reasons) {
				t.Errorf("Reasons = %v, want %v", got.Reasons, tc.reasons)
			}
			if tc.wantProfile {
				want := validNVIDIAProfile()
				if got.ProfileName != want.Name || got.ProfileUID != want.ProfileUID ||
					got.ProfileGeneration != want.ProfileGeneration || got.ProfileDigest != want.ProfileDigest {
					t.Errorf("selected profile = %q/%q/%d/%q, want %q/%q/%d/%q",
						got.ProfileName, got.ProfileUID, got.ProfileGeneration, got.ProfileDigest,
						want.Name, want.ProfileUID, want.ProfileGeneration, want.ProfileDigest)
				}
			} else if got.ProfileName != "" || got.ProfileUID != "" || got.ProfileGeneration != 0 || got.ProfileDigest != "" {
				t.Errorf("selected profile identity must be empty when selection is %q, got %q", got.Selection, got.ProfileName)
			}
			if !got.AgentLastSeen.Equal(in.AgentLastSeen) {
				t.Errorf("AgentLastSeen = %s, want %s", got.AgentLastSeen, in.AgentLastSeen)
			}
			if tc.wantReportObs {
				if in.Report == nil || !got.ReportObservedAt.Equal(in.Report.ObservedAt) {
					t.Errorf("ReportObservedAt = %s, want report observation", got.ReportObservedAt)
				}
			} else if !got.ReportObservedAt.IsZero() {
				t.Errorf("ReportObservedAt = %s, want zero without a report", got.ReportObservedAt)
			}
			if got.Summary == "" {
				t.Error("Summary is empty")
			}
			if got.VerificationDepth == RuntimeContractVerificationFull && len(got.Reasons) != 0 {
				t.Errorf("Full coverage must carry no reasons, got %v", got.Reasons)
			}
		})
	}
}

func TestAssessRuntimeContractCoverageFullRequiresUsableHeartbeat(t *testing.T) {
	now := time.Date(2026, time.September, 9, 12, 0, 0, 0, time.UTC)

	heartbeats := map[string]time.Time{
		"healthy":    now.Add(-time.Minute),
		"stale":      now.Add(-time.Hour),
		"future":     now.Add(time.Minute),
		"never seen": {},
	}
	for name, lastSeen := range heartbeats {
		t.Run(name, func(t *testing.T) {
			in := coverageInput(now)
			in.AgentLastSeen = lastSeen
			got := AssessRuntimeContractCoverage(in)

			if got.Attestation != RuntimeContractAttestationFreshCompatible {
				t.Fatalf("Attestation = %q, want FreshCompatible; the heartbeat must not affect attestation", got.Attestation)
			}
			if got.VerificationDepth == RuntimeContractVerificationFull && len(got.Reasons) != 0 {
				t.Fatalf("Full coverage must carry no reasons, got %v", got.Reasons)
			}
			if got.VerificationDepth == RuntimeContractVerificationReduced {
				t.Fatalf("VerificationDepth = Reduced; a fresh exact report is never Reduced, got reasons %v", got.Reasons)
			}
			wantFull := name == "healthy"
			if isFull := got.VerificationDepth == RuntimeContractVerificationFull; isFull != wantFull {
				t.Fatalf("VerificationDepth = %q with reasons %v, want Full=%t", got.VerificationDepth, got.Reasons, wantFull)
			}
		})
	}
}

func TestAssessRuntimeContractCoverageReasonOrderIsStable(t *testing.T) {
	now := time.Date(2026, time.September, 9, 12, 0, 0, 0, time.UTC)
	in := coverageInput(now)
	in.Report.ProfileDigest = "sha256:ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"
	in.Report.ProfileUID = "profile-uid-old"
	in.Report.DriverVersion = "560.0"
	in.Report.ObservedAt = now.Add(-time.Hour)
	in.Report.Readiness = types.AcceleratorReadinessDegraded
	in.Report.ReadinessReasons = []string{"xid storm"}
	in.AgentLastSeen = now.Add(-time.Hour)

	want := []RuntimeContractReasonCode{
		RuntimeContractReasonReportProfileDigestMismatch,
		RuntimeContractReasonReportProfileRevisionMismatch,
		RuntimeContractReasonReportDriverVersionMismatch,
		RuntimeContractReasonReportStale,
		RuntimeContractReasonReportNotReady,
		RuntimeContractReasonAgentHeartbeatStale,
	}
	first := AssessRuntimeContractCoverage(in)
	if !reflect.DeepEqual(first.Reasons, want) {
		t.Fatalf("Reasons = %v, want %v", first.Reasons, want)
	}
	if first.Attestation != RuntimeContractAttestationMismatch {
		t.Fatalf("Attestation = %q, want Mismatch to take precedence over Stale", first.Attestation)
	}
	if first.VerificationDepth != RuntimeContractVerificationUnavailable {
		t.Fatalf("VerificationDepth = %q, want Unavailable", first.VerificationDepth)
	}
	for i := 0; i < 5; i++ {
		if again := AssessRuntimeContractCoverage(in); !reflect.DeepEqual(again, first) {
			t.Fatalf("assessment %d differs from first:\n got %+v\nwant %+v", i, again, first)
		}
	}
}

func TestRuntimeContractReasonOrderCoversEveryCode(t *testing.T) {
	seen := make(map[RuntimeContractReasonCode]int)
	for _, code := range runtimeContractReasonOrder {
		seen[code]++
		if seen[code] > 1 {
			t.Errorf("reason %q listed more than once", code)
		}
	}
	// Every code produced by the assessment must be orderable; a missing
	// entry would silently drop the code from results.
	set := make(map[RuntimeContractReasonCode]struct{})
	for _, code := range []RuntimeContractReasonCode{
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
	} {
		set[code] = struct{}{}
	}
	if got := orderRuntimeContractReasons(set); len(got) != len(set) {
		t.Fatalf("orderRuntimeContractReasons dropped codes: got %d, want %d", len(got), len(set))
	}
}

func TestAssessRuntimeContractCoverageDoesNotMutateInput(t *testing.T) {
	now := time.Date(2026, time.September, 9, 12, 0, 0, 0, time.UTC)
	in := coverageInput(now)
	in.Report.ProfileGeneration = 2
	second := validNVIDIAProfile()
	second.Name = "nvidia-a100-overlap"
	second.NodeSelector = map[string]string{"pool": "a100"}
	in.Profiles = append(in.Profiles, second)

	snapshot := coverageInput(now)
	snapshot.Report.ProfileGeneration = 2
	snapshot.Profiles = append(snapshot.Profiles, second)

	_ = AssessRuntimeContractCoverage(in)
	if !reflect.DeepEqual(in, snapshot) {
		t.Fatalf("input mutated:\n got %+v\nwant %+v", in, snapshot)
	}

	// The exact-selection path copies profile identity out; the source slice
	// and labels must still be untouched.
	in = coverageInput(now)
	snapshot = coverageInput(now)
	result := AssessRuntimeContractCoverage(in)
	if result.VerificationDepth != RuntimeContractVerificationFull {
		t.Fatalf("VerificationDepth = %q, want Full", result.VerificationDepth)
	}
	if !reflect.DeepEqual(in, snapshot) {
		t.Fatalf("input mutated on the exact path:\n got %+v\nwant %+v", in, snapshot)
	}
}
