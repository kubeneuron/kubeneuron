package operations

import (
	"context"
	"encoding/json"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/kubeneuron/kubeneuron/internal/decision"
	"github.com/kubeneuron/kubeneuron/pkg/types"
)

const previewProfileHeader = `
apiVersion: kubeneuron.io/v1alpha1
kind: CandidateConfiguration
accelerator_profiles:
`

// previewProfileYAML renders one candidate profile that selects nodes with the
// given pool label. Everything else mirrors operationProfile unless overridden.
func previewProfileYAML(name, pool, digest, driver, uid string, generation int) string {
	return `  - name: ` + name + `
    node_selector: {pool: ` + pool + `}
    vendor: nvidia
    profile_digest: ` + digest + `
    driver_version: "` + driver + `"
    runtime_version: dcgm-3.3.5
    profile_uid: ` + uid + `
    profile_generation: ` + strconv.Itoa(generation) + `
    max_report_age: 5m
    allowed_actions:
      - action: reset-device
        scopes: [physical-device]
        require_verified_unpartitioned_topology: true
`
}

const (
	liveProfileDigest    = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	changedProfileDigest = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

func uploadPreviewCandidate(t *testing.T, mgr *Manager, key, content string) *CandidateConfiguration {
	t.Helper()
	candidate, _, err := mgr.CreateCandidate(context.Background(), "alice", CandidateUpload{Content: []byte(content), IdempotencyKey: key})
	if err != nil {
		t.Fatalf("upload candidate %s: %v", key, err)
	}
	return candidate
}

func singleDelta(t *testing.T, preview *PolicyImpactPreview) (string, NodeDecisionDelta) {
	t.Helper()
	buckets := map[string][]NodeDecisionDelta{
		"newly_eligible": preview.NewlyEligible, "newly_blocked": preview.NewlyBlocked,
		"changed_observed_only": preview.ChangedObservedOnly, "unchanged": preview.Unchanged,
	}
	found := ""
	var delta NodeDecisionDelta
	for name, items := range buckets {
		if len(items) == 0 {
			continue
		}
		if found != "" || len(items) != 1 {
			t.Fatalf("preview has more than one delta: %#v", preview)
		}
		found, delta = name, items[0]
	}
	if found == "" {
		t.Fatalf("preview has no delta: %#v", preview)
	}
	return found, delta
}

// requireFailClosedCandidateProfile asserts the shared contract for a candidate
// profile that statically selects the node: the After decision is never
// Eligible, is built from the candidate profile without the captured report,
// and the impact says fresh post-deploy attestation is required.
func requireFailClosedCandidateProfile(t *testing.T, bucket string, delta NodeDecisionDelta, wantName, wantUID, wantDigest string, wantGeneration int64) {
	t.Helper()
	if bucket != "newly_blocked" || !delta.Changed {
		t.Fatalf("candidate profile delta bucket = %s changed=%v, want newly_blocked/changed", bucket, delta.Changed)
	}
	if delta.Before.State != decision.StateEligible {
		t.Fatalf("live before state = %s, want Eligible from the captured report", delta.Before.State)
	}
	if delta.After.State == decision.StateEligible || delta.After.State == decision.StateObservedOnly || len(delta.After.AllowedActions) != 0 {
		t.Fatalf("after result = %#v, want a fail-closed non-Eligible answer with no allowed actions", delta.After)
	}
	if len(delta.After.ReasonCodes) != 1 || delta.After.ReasonCodes[0] != decision.ReasonEvidenceStale {
		t.Fatalf("after reason codes = %v, want exactly EvidenceStale: the candidate has no evidence yet", delta.After.ReasonCodes)
	}
	for _, ref := range delta.After.EvidenceRefs {
		if strings.HasPrefix(ref.Source, "accelerator-report/") {
			t.Fatalf("after evidence refs %v still cite the captured pre-deploy report", delta.After.EvidenceRefs)
		}
	}
	impact := delta.RuntimeContractImpact
	if impact == nil {
		t.Fatal("candidate profile delta has no runtime contract impact")
	}
	want := CandidateRuntimeContractImpact{
		Version: CandidateRuntimeContractImpactVersion, Assessment: RuntimeContractImpactPreDeployStatic,
		Node: delta.Node, Vendor: types.AcceleratorVendorNVIDIA,
		ProfileChange: RuntimeContractProfileChangeReplaced, StaticSelection: RuntimeContractStaticSelectionExact,
		CandidateProfileName: wantName, CandidateProfileUID: wantUID, CandidateProfileGeneration: wantGeneration, CandidateProfileDigest: wantDigest,
		PostDeployAttestation: RuntimeContractAttestationFreshRequired, CapturedReportUsableAsCandidateAttestation: false,
		AfterDecisionEvidence: RuntimeContractAfterEvidenceCandidateProfileNoReport,
		Reasons:               []RuntimeContractImpactReason{RuntimeContractImpactReasonCandidateProfilesPresent, RuntimeContractImpactReasonCapturedReportPredatesCandidate},
		Summary:               impact.Summary,
	}
	if !reflect.DeepEqual(*impact, want) {
		t.Fatalf("runtime contract impact = %#v\nwant %#v", *impact, want)
	}
	for _, fragment := range []string{"pre-deploy static assessment", "FreshRequired", "not candidate attestation", "fails closed", wantName} {
		if !strings.Contains(impact.Summary, fragment) {
			t.Fatalf("impact summary %q lacks %q", impact.Summary, fragment)
		}
	}
}

func TestPreviewIdenticalCandidateProfileStillRequiresFreshAttestation(t *testing.T) {
	mgr, st, _ := newOperationManager(t)
	defer func() { _ = st.Close() }()
	ctx := context.Background()
	// Same name, selector, digest, driver, UID, and generation as the live
	// profile: the captured report would satisfy CheckReport for it.
	candidate := uploadPreviewCandidate(t, mgr, "identical", previewProfileHeader+previewProfileYAML("nvidia-a100", "a100", liveProfileDigest, "550.54.15", "profile-uid", 1))
	if candidate.Profiles[0].CheckReport(mgr.now(), *operationSnapshot(mgr.now(), decision.Request{}).Report) != nil {
		t.Fatal("test setup: the captured report must satisfy the identical candidate profile so the preview cannot rely on a mismatch")
	}

	preview, _, err := mgr.CreatePreview(ctx, "alice", candidate.ID, "identical-preview")
	if err != nil {
		t.Fatal(err)
	}
	if preview.RuntimeContractImpactVersion != CandidateRuntimeContractImpactVersion || preview.RuntimeContractProfileChange != RuntimeContractProfileChangeReplaced {
		t.Fatalf("preview header = version %q change %q", preview.RuntimeContractImpactVersion, preview.RuntimeContractProfileChange)
	}
	bucket, delta := singleDelta(t, preview)
	requireFailClosedCandidateProfile(t, bucket, delta, "nvidia-a100", "profile-uid", liveProfileDigest, 1)
	if delta.After.ConfigDigest != candidate.NormalizedDigest {
		t.Fatalf("after config digest = %s, want candidate digest %s", delta.After.ConfigDigest, candidate.NormalizedDigest)
	}
}

func TestPreviewChangedCandidateProfileRequiresFreshAttestation(t *testing.T) {
	mgr, st, _ := newOperationManager(t)
	defer func() { _ = st.Close() }()
	candidate := uploadPreviewCandidate(t, mgr, "changed", previewProfileHeader+previewProfileYAML("nvidia-a100-v2", "a100", changedProfileDigest, "560.35.03", "profile-uid-v2", 2))

	preview, _, err := mgr.CreatePreview(context.Background(), "alice", candidate.ID, "changed-preview")
	if err != nil {
		t.Fatal(err)
	}
	bucket, delta := singleDelta(t, preview)
	requireFailClosedCandidateProfile(t, bucket, delta, "nvidia-a100-v2", "profile-uid-v2", changedProfileDigest, 2)
}

func TestPreviewUncoveredAndAmbiguousCandidateSelectionNeverAuthorize(t *testing.T) {
	cases := map[string]struct {
		content   string
		selection RuntimeContractStaticSelection
		reason    RuntimeContractImpactReason
		fragment  string
	}{
		"uncovered": {
			content:   previewProfileHeader + previewProfileYAML("nvidia-h100", "h100", changedProfileDigest, "560.35.03", "h100-uid", 1),
			selection: RuntimeContractStaticSelectionUncovered, reason: RuntimeContractImpactReasonProfileNotFound, fragment: "Uncovered",
		},
		"ambiguous": {
			content: previewProfileHeader +
				previewProfileYAML("nvidia-a100-one", "a100", liveProfileDigest, "550.54.15", "one-uid", 1) +
				previewProfileYAML("nvidia-a100-two", "a100", changedProfileDigest, "560.35.03", "two-uid", 1),
			selection: RuntimeContractStaticSelectionAmbiguous, reason: RuntimeContractImpactReasonProfileOverlap, fragment: "Ambiguous",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			mgr, st, _ := newOperationManager(t)
			defer func() { _ = st.Close() }()
			candidate := uploadPreviewCandidate(t, mgr, name, tc.content)
			preview, _, err := mgr.CreatePreview(context.Background(), "alice", candidate.ID, name+"-preview")
			if err != nil {
				t.Fatalf("preview with %s candidate selection must complete and explain itself, got %v", name, err)
			}
			bucket, delta := singleDelta(t, preview)
			// The live node was Eligible; without a single selected candidate
			// profile the After answer is the observation-only fallback.
			if bucket != "newly_blocked" || delta.After.State != decision.StateObservedOnly || len(delta.After.AllowedActions) != 0 {
				t.Fatalf("%s delta = bucket %s after %#v, want newly_blocked with ObservedOnly and no allowed actions", name, bucket, delta.After)
			}
			impact := delta.RuntimeContractImpact
			if impact == nil {
				t.Fatal("no runtime contract impact")
			}
			want := CandidateRuntimeContractImpact{
				Version: CandidateRuntimeContractImpactVersion, Assessment: RuntimeContractImpactPreDeployStatic,
				Node: "gpu-a", Vendor: types.AcceleratorVendorNVIDIA,
				ProfileChange: RuntimeContractProfileChangeReplaced, StaticSelection: tc.selection,
				PostDeployAttestation: RuntimeContractAttestationNotApplicable, CapturedReportUsableAsCandidateAttestation: false,
				AfterDecisionEvidence: RuntimeContractAfterEvidenceNoProfileCapturedReport,
				Reasons:               []RuntimeContractImpactReason{RuntimeContractImpactReasonCandidateProfilesPresent, tc.reason},
				Summary:               impact.Summary,
			}
			if !reflect.DeepEqual(*impact, want) {
				t.Fatalf("%s impact = %#v\nwant %#v", name, *impact, want)
			}
			if impact.CandidateProfileName != "" || impact.CandidateProfileUID != "" {
				t.Fatalf("%s impact names a candidate profile: %#v", name, *impact)
			}
			for _, fragment := range []string{tc.fragment, "NotApplicable", "cannot authorize an action", string(tc.reason)} {
				if !strings.Contains(impact.Summary, fragment) {
					t.Fatalf("%s summary %q lacks %q", name, impact.Summary, fragment)
				}
			}
			for _, banned := range []string{"FreshCompatible", "Full"} {
				if strings.Contains(impact.Summary, banned) {
					t.Fatalf("%s summary %q claims %q", name, impact.Summary, banned)
				}
			}
		})
	}
}

func TestPreviewUncoveredCandidateKeepsNoProfileFallbackUnchanged(t *testing.T) {
	mgr, st, now := newOperationManager(t)
	defer func() { _ = st.Close() }()
	// The live node already has no selected profile: it is observed only.
	mgr.buildSnapshot = func(_ context.Context, _ string, request decision.Request) (decision.Snapshot, error) {
		snapshot := operationSnapshot(now, request)
		snapshot.Profile = nil
		return snapshot, nil
	}
	candidate := uploadPreviewCandidate(t, mgr, "uncovered-fallback", previewProfileHeader+previewProfileYAML("nvidia-h100", "h100", changedProfileDigest, "560.35.03", "h100-uid", 1))
	preview, _, err := mgr.CreatePreview(context.Background(), "alice", candidate.ID, "uncovered-fallback-preview")
	if err != nil {
		t.Fatal(err)
	}
	bucket, delta := singleDelta(t, preview)
	if bucket != "unchanged" || delta.Changed || delta.Before.State != decision.StateObservedOnly || delta.After.State != decision.StateObservedOnly {
		t.Fatalf("no-profile fallback delta = bucket %s %#v, want unchanged ObservedOnly before and after", bucket, delta)
	}
	if len(delta.After.AllowedActions) != 0 {
		t.Fatalf("uncovered candidate granted actions %v", delta.After.AllowedActions)
	}
	if impact := delta.RuntimeContractImpact; impact == nil || impact.StaticSelection != RuntimeContractStaticSelectionUncovered || impact.PostDeployAttestation != RuntimeContractAttestationNotApplicable {
		t.Fatalf("impact = %#v, want Uncovered/NotApplicable", impact)
	}
}

func TestPreviewPolicyOnlyCandidatePreservesLiveProfileDecision(t *testing.T) {
	mgr, st, _ := newOperationManager(t)
	defer func() { _ = st.Close() }()
	candidate := uploadPreviewCandidate(t, mgr, "policy-only", `
apiVersion: kubeneuron.io/v1alpha1
kind: GPURemediationPolicy
metadata:
  name: ecc-dbe-reset
spec:
  match:
    class: ecc-dbe
  playbookRef: reset-device
`)
	if len(candidate.Profiles) != 0 || len(candidate.Policies) != 1 {
		t.Fatalf("policy-only candidate compiled to %#v", candidate)
	}
	preview, _, err := mgr.CreatePreview(context.Background(), "alice", candidate.ID, "policy-only-preview")
	if err != nil {
		t.Fatal(err)
	}
	if preview.RuntimeContractProfileChange != RuntimeContractProfileChangeNone {
		t.Fatalf("policy-only preview profile change = %q, want NoProfileChange", preview.RuntimeContractProfileChange)
	}
	bucket, delta := singleDelta(t, preview)
	if bucket != "unchanged" || delta.Changed || delta.Before.State != decision.StateEligible || delta.After.State != decision.StateEligible {
		t.Fatalf("policy-only delta = bucket %s %#v, want the live Eligible decision preserved", bucket, delta)
	}
	if delta.After.ConfigDigest != candidate.NormalizedDigest {
		t.Fatalf("after config digest = %s, want the candidate digest", delta.After.ConfigDigest)
	}
	impact := delta.RuntimeContractImpact
	if impact == nil {
		t.Fatal("policy-only delta has no runtime contract impact")
	}
	want := CandidateRuntimeContractImpact{
		Version: CandidateRuntimeContractImpactVersion, Assessment: RuntimeContractImpactPreDeployStatic,
		Node: "gpu-a", Vendor: types.AcceleratorVendorNVIDIA,
		ProfileChange: RuntimeContractProfileChangeNone, StaticSelection: RuntimeContractStaticSelectionNotEvaluated,
		PostDeployAttestation: RuntimeContractAttestationNotRequired, CapturedReportUsableAsCandidateAttestation: false,
		AfterDecisionEvidence: RuntimeContractAfterEvidenceLiveProfileCapturedReport,
		Reasons:               []RuntimeContractImpactReason{RuntimeContractImpactReasonNoCandidateProfiles},
		Summary:               impact.Summary,
	}
	if !reflect.DeepEqual(*impact, want) {
		t.Fatalf("policy-only impact = %#v\nwant %#v", *impact, want)
	}
	for _, fragment := range []string{"NoProfileChange", "no new post-deploy runtime attestation is required"} {
		if !strings.Contains(impact.Summary, fragment) {
			t.Fatalf("policy-only summary %q lacks %q", impact.Summary, fragment)
		}
	}
	// The persisted After snapshot still carries the live profile and report.
	inventory, err := st.GetOperationalResource(context.Background(), types.ResourceDecisionSnapshot, preview.InventorySnapshotID)
	if err != nil {
		t.Fatal(err)
	}
	var fleet fleetSnapshot
	if err := json.Unmarshal(inventory.Payload, &fleet); err != nil {
		t.Fatal(err)
	}
	if len(fleet.Decisions) != 1 || fleet.Decisions[0].After.Profile == nil || fleet.Decisions[0].After.Profile.Name != "nvidia-a100" || fleet.Decisions[0].After.Report == nil {
		t.Fatalf("policy-only after snapshot = %#v, want the live profile and captured report preserved", fleet.Decisions)
	}
}

func TestPreviewRuntimeContractImpactIsDeterministicAndReplayable(t *testing.T) {
	mgr, st, now := newOperationManager(t)
	defer func() { _ = st.Close() }()
	ctx := context.Background()
	candidate := uploadPreviewCandidate(t, mgr, "replay", previewProfileHeader+previewProfileYAML("nvidia-a100", "a100", liveProfileDigest, "550.54.15", "profile-uid", 1))

	created, _, err := mgr.CreatePreview(ctx, "alice", candidate.ID, "replay-preview")
	if err != nil {
		t.Fatal(err)
	}
	replayed, wasReplay, err := mgr.CreatePreview(ctx, "alice", candidate.ID, "replay-preview")
	if err != nil || !wasReplay {
		t.Fatalf("replay = %#v replay=%v err=%v", replayed, wasReplay, err)
	}
	stored, err := mgr.GetPreview(ctx, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	latest, err := mgr.LatestPreviewForCandidate(ctx, candidate.ID)
	if err != nil {
		t.Fatal(err)
	}
	createdJSON, _ := json.Marshal(created)
	for name, preview := range map[string]*PolicyImpactPreview{"replay": replayed, "get": stored, "latest": latest} {
		blob, _ := json.Marshal(preview)
		if string(blob) != string(createdJSON) {
			t.Fatalf("%s preview differs from the created preview:\n%s\n%s", name, blob, createdJSON)
		}
	}
	if !strings.Contains(string(createdJSON), `"runtime_contract_impact":{"version":"candidate-runtime-contract-impact/v1","assessment":"PreDeployStatic"`) ||
		!strings.Contains(string(createdJSON), `"captured_report_usable_as_candidate_attestation":false`) {
		t.Fatalf("persisted preview JSON lacks the impact contract: %s", createdJSON)
	}

	// The frozen inventory records the same impact and an After snapshot that
	// carries the candidate profile without any report.
	inventory, err := st.GetOperationalResource(ctx, types.ResourceDecisionSnapshot, created.InventorySnapshotID)
	if err != nil {
		t.Fatal(err)
	}
	var fleet fleetSnapshot
	if err := json.Unmarshal(inventory.Payload, &fleet); err != nil {
		t.Fatal(err)
	}
	if len(fleet.Decisions) != 1 || fleet.Decisions[0].RuntimeContractImpact == nil || !reflect.DeepEqual(*fleet.Decisions[0].RuntimeContractImpact, *created.NewlyBlocked[0].RuntimeContractImpact) {
		t.Fatalf("inventory impact = %#v, want the preview impact", fleet.Decisions)
	}
	if after := fleet.Decisions[0].After; after.Report != nil || after.Profile == nil || after.Profile.Name != "nvidia-a100" {
		t.Fatalf("inventory after snapshot = %#v, want candidate profile without report", after)
	}
	if fleet.Decisions[0].Before.Report == nil {
		t.Fatal("inventory before snapshot lost the captured report")
	}

	// The assessment is a pure function of the profile set and node identity:
	// report contents and heartbeat do not participate.
	base := operationSnapshot(now, decision.Request{})
	first := assessCandidateRuntimeContract(base, candidate)
	mutated := operationSnapshot(now, decision.Request{})
	mutated.Report.ProfileDigest, mutated.Report.ObservedAt, mutated.Report.Readiness = changedProfileDigest, now.Add(-time.Hour), types.AcceleratorReadinessNotReady
	mutated.Node.AgentLastSeen = time.Time{}
	second := assessCandidateRuntimeContract(mutated, candidate)
	if !reflect.DeepEqual(first.impact, second.impact) {
		t.Fatalf("impact depends on report or heartbeat contents:\n%#v\n%#v", first.impact, second.impact)
	}
}

func TestPreviewWithoutRuntimeContractImpactRemainsReadable(t *testing.T) {
	mgr, st, now := newOperationManager(t)
	defer func() { _ = st.Close() }()
	ctx := context.Background()
	legacy := `{"id":"preview-legacy","resource_version":1,"candidate_id":"candidate-legacy","candidate_digest":"sha256:legacy","inventory_snapshot_id":"inventory-legacy","evaluator_version":"v1","created_at":"2026-09-01T00:00:00Z","newly_eligible":null,"newly_blocked":null,"changed_observed_only":null,"unchanged":[{"node":"gpu-a","before":{"evaluator_version":"v1","state":"Eligible","reason_codes":[],"human_summary":"ok","evidence_refs":[],"limits":{},"config_digest":"sha256:live","evaluated_at":"2026-09-01T00:00:00Z","expires_at":"2026-09-01T00:05:00Z"},"after":{"evaluator_version":"v1","state":"Eligible","reason_codes":[],"human_summary":"ok","evidence_refs":[],"limits":{},"config_digest":"sha256:legacy","evaluated_at":"2026-09-01T00:00:00Z","expires_at":"2026-09-01T00:05:00Z"},"changed":false}]}`
	expires := now.Add(time.Hour)
	if err := st.CreateOperationalResource(ctx, &types.OperationalResource{
		Kind: types.ResourcePolicyImpactPreview, ID: "preview-legacy", State: "complete", Actor: "alice",
		ConfigDigest: "sha256:legacy", Payload: json.RawMessage(legacy), CreatedAt: now, UpdatedAt: now, ExpiresAt: &expires, Version: 1,
	}); err != nil {
		t.Fatal(err)
	}
	preview, err := mgr.GetPreview(ctx, "preview-legacy")
	if err != nil {
		t.Fatal(err)
	}
	if preview.RuntimeContractImpactVersion != "" || preview.RuntimeContractProfileChange != "" || len(preview.Unchanged) != 1 || preview.Unchanged[0].RuntimeContractImpact != nil {
		t.Fatalf("legacy preview = %#v, want no runtime contract impact fields", preview)
	}
	blob, _ := json.Marshal(preview)
	if strings.Contains(string(blob), "runtime_contract") {
		t.Fatalf("legacy preview re-encodes invented impact fields: %s", blob)
	}
}
