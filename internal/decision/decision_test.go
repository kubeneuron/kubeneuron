package decision

import (
	"reflect"
	"testing"
	"time"

	"github.com/kubeneuron/kubeneuron/internal/config"
	"github.com/kubeneuron/kubeneuron/pkg/types"
)

func testSnapshot(now time.Time) Snapshot {
	profile := &config.AcceleratorRuntimeProfile{
		Name:              "nvidia-a100",
		NodeSelector:      map[string]string{"pool": "a100"},
		Vendor:            types.AcceleratorVendorNVIDIA,
		ProfileDigest:     "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		DriverVersion:     "550.54.15",
		RuntimeVersion:    "dcgm-3.3.5",
		ProfileUID:        "profile-uid",
		ProfileGeneration: 1,
		MaxReportAge:      config.Duration(5 * time.Minute),
		AllowedActions: []config.AcceleratorActionPolicy{{
			Action:                               types.AcceleratorActionResetDevice,
			Scopes:                               []types.AcceleratorTargetScope{types.AcceleratorScopePhysicalDevice},
			RequireVerifiedUnpartitionedTopology: true,
		}},
	}
	return Snapshot{
		Version:      EvaluatorVersion,
		EvaluatedAt:  now,
		ConfigDigest: "sha256:effective",
		Node: types.Node{
			Name: "gpu-a", UID: "uid-a", Labels: map[string]string{"pool": "a100"}, AgentLastSeen: now.Add(-time.Minute),
		},
		Report: &types.AgentAcceleratorReport{
			Node: "gpu-a", NodeUID: "uid-a", Vendor: types.AcceleratorVendorNVIDIA,
			ObservedAt: now.Add(-time.Minute), ProfileDigest: profile.ProfileDigest,
			ProfileUID: profile.ProfileUID, ProfileGeneration: profile.ProfileGeneration,
			DriverVersion: profile.DriverVersion, RuntimeVersion: profile.RuntimeVersion,
			TopologySafety: types.AcceleratorTopologyVerifiedUnpartitioned,
			Readiness:      types.AcceleratorReadinessReady,
			Devices:        []types.AgentAcceleratorDevice{{ID: "GPU-a", Kind: types.AcceleratorDevicePhysical, Family: types.AcceleratorFamilyGPU}},
			Capabilities: []types.AgentAcceleratorCapability{{
				Action: types.AcceleratorActionResetDevice,
				Scopes: []types.AcceleratorTargetScope{types.AcceleratorScopePhysicalDevice},
			}},
		},
		Profile: profile,
		Request: Request{
			Class: ActionRemediate, AcceleratorAction: types.AcceleratorActionResetDevice,
			Scope: types.AcceleratorScopePhysicalDevice, TargetDeviceID: "GPU-a",
		},
	}
}

func TestEvaluateEligibleAndDeterministic(t *testing.T) {
	now := time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)
	snapshot := testSnapshot(now)
	first := Evaluate(snapshot)
	second := Evaluate(snapshot)
	if first.State != StateEligible || !first.Permitted() {
		t.Fatalf("result = %#v, want eligible", first)
	}
	if len(first.ReasonCodes) != 0 || len(first.AllowedActions) != 1 {
		t.Fatalf("result = %#v, want no reasons and one allowed action", first)
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("same snapshot produced different results:\nfirst=%#v\nsecond=%#v", first, second)
	}
}

func TestEvaluateFailsClosedForStopAndStaleEvidence(t *testing.T) {
	now := time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)
	snapshot := testSnapshot(now)
	snapshot.GlobalPaused = true
	got := Evaluate(snapshot)
	if got.State != StateBlocked || len(got.ReasonCodes) != 1 || got.ReasonCodes[0] != ReasonEmergencyStopActive {
		t.Fatalf("paused result = %#v", got)
	}

	snapshot = testSnapshot(now)
	snapshot.Report.ObservedAt = now.Add(-6 * time.Minute)
	got = Evaluate(snapshot)
	if got.State != StateUnknown || len(got.ReasonCodes) != 1 || got.ReasonCodes[0] != ReasonEvidenceStale {
		t.Fatalf("stale result = %#v", got)
	}
}

func TestEvaluateObservedOnlyWithoutProfile(t *testing.T) {
	now := time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)
	snapshot := testSnapshot(now)
	snapshot.Profile = nil
	snapshot.Request = Request{Class: ActionObserve}
	got := Evaluate(snapshot)
	if got.State != StateObservedOnly || len(got.ReasonCodes) != 1 || got.ReasonCodes[0] != ReasonProfileMismatch {
		t.Fatalf("observed-only result = %#v", got)
	}
}

func TestEvaluateRequiresFreshTimestampedEvidenceSources(t *testing.T) {
	now := time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)
	snapshot := testSnapshot(now)
	snapshot.RequiredEvidenceSources = []string{"controller", "dcgm"}
	// A source name is descriptive metadata, not proof.  In particular, a
	// plan must not become autonomous merely because DCGM was configured at
	// some point in the past.
	snapshot.EvidenceSources = []string{"controller", "dcgm"}
	snapshot.EvidenceRefs = []EvidenceRef{{
		Source: "controller", ID: "runtime-config/1", ObservedAt: now.Add(-time.Minute), Digest: "sha256:controller",
	}}
	blocked := Evaluate(snapshot)
	if blocked.State != StateBlocked || !reflect.DeepEqual(blocked.ReasonCodes, []ReasonCode{ReasonEvidenceSourceMissing}) {
		t.Fatalf("bare dcgm source = %#v, want evidence-source block", blocked)
	}

	snapshot.EvidenceRefs = append(snapshot.EvidenceRefs, EvidenceRef{
		Source: "dcgm", ID: "report/gpu-a", ObservedAt: now.Add(-6 * time.Minute), Digest: "sha256:stale-dcgm",
	})
	blocked = Evaluate(snapshot)
	if blocked.State != StateBlocked || !reflect.DeepEqual(blocked.ReasonCodes, []ReasonCode{ReasonEvidenceSourceMissing}) {
		t.Fatalf("stale dcgm source = %#v, want evidence-source block", blocked)
	}

	snapshot.EvidenceRefs[1].ObservedAt = now.Add(-time.Minute)
	eligible := Evaluate(snapshot)
	if eligible.State != StateEligible {
		t.Fatalf("fresh required sources = %#v, want eligible", eligible)
	}
}

func TestEvaluateRejectsHistoricalApprovalAgainstChangedConfiguration(t *testing.T) {
	now := time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)
	snapshot := testSnapshot(now)
	snapshot.RequiredConfigDigest = "sha256:approved-config"
	got := Evaluate(snapshot)
	if got.State != StateBlocked || !reflect.DeepEqual(got.ReasonCodes, []ReasonCode{ReasonConfigurationChanged}) {
		t.Fatalf("changed configuration result = %#v", got)
	}
	if !reflect.DeepEqual(got.RequiredActions, []string{"review_configuration_revision"}) {
		t.Fatalf("changed configuration prerequisites = %#v", got.RequiredActions)
	}

	snapshot.RequiredConfigDigest = snapshot.ConfigDigest
	if got = Evaluate(snapshot); got.State != StateEligible {
		t.Fatalf("matching configuration result = %#v, want eligible", got)
	}
}
