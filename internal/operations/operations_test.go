package operations

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/kubeneuron/kubeneuron/internal/config"
	"github.com/kubeneuron/kubeneuron/internal/decision"
	"github.com/kubeneuron/kubeneuron/internal/store"
	"github.com/kubeneuron/kubeneuron/internal/store/sqlite"
	"github.com/kubeneuron/kubeneuron/pkg/types"
)

func operationProfile() *config.AcceleratorRuntimeProfile {
	return &config.AcceleratorRuntimeProfile{
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
}

func operationSnapshot(now time.Time, request decision.Request) decision.Snapshot {
	profile := operationProfile()
	return decision.Snapshot{
		Version:      decision.EvaluatorVersion,
		EvaluatedAt:  now,
		ConfigDigest: "sha256:live-config",
		Node:         types.Node{Name: "gpu-a", UID: "node-uid", Labels: map[string]string{"pool": "a100"}, AgentLastSeen: now.Add(-time.Minute)},
		Report: &types.AgentAcceleratorReport{
			Node: "gpu-a", NodeUID: "node-uid", Vendor: types.AcceleratorVendorNVIDIA,
			ObservedAt: now.Add(-time.Minute), ProfileDigest: profile.ProfileDigest,
			ProfileUID: profile.ProfileUID, ProfileGeneration: profile.ProfileGeneration,
			DriverVersion: profile.DriverVersion, RuntimeVersion: profile.RuntimeVersion,
			TopologySafety: types.AcceleratorTopologyVerifiedUnpartitioned,
			Readiness:      types.AcceleratorReadinessReady,
			Devices:        []types.AgentAcceleratorDevice{{ID: "GPU-a", Kind: types.AcceleratorDevicePhysical, Family: types.AcceleratorFamilyGPU}},
			Capabilities:   []types.AgentAcceleratorCapability{{Action: types.AcceleratorActionResetDevice, Scopes: []types.AcceleratorTargetScope{types.AcceleratorScopePhysicalDevice}}},
		},
		Profile: profile,
		Request: request,
	}
}

func newOperationManager(t *testing.T) (*Manager, *sqlite.Store, time.Time) {
	t.Helper()
	st, err := sqlite.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)
	mgr := New(Options{
		Resources: st,
		Workflow:  st,
		Now:       func() time.Time { return now },
		BuildSnapshot: func(_ context.Context, node string, request decision.Request) (decision.Snapshot, error) {
			if node != "gpu-a" {
				t.Fatalf("unexpected node %q", node)
			}
			return operationSnapshot(now, request), nil
		},
		ListNodes: func(context.Context) ([]*types.Node, error) {
			return []*types.Node{{Name: "gpu-a"}}, nil
		},
		CreateIncident: func(_ context.Context, signal types.Signal) (*types.Incident, error) {
			return &types.Incident{ID: "incident-1", Target: signal.Target, Class: signal.Class}, nil
		},
	})
	return mgr, st, now
}

func TestCandidatePreviewIsDurableAndIdempotent(t *testing.T) {
	mgr, st, _ := newOperationManager(t)
	defer func() { _ = st.Close() }()
	content := []byte(`
apiVersion: kubeneuron.io/v1alpha1
kind: CandidateConfiguration
accelerator_profiles:
  - name: nvidia-a100
    node_selector:
      pool: a100
    vendor: nvidia
    profile_digest: sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
    driver_version: "550.54.15"
    runtime_version: dcgm-3.3.5
    profile_uid: profile-uid
    profile_generation: 1
    max_report_age: 5m
    allowed_actions:
      - action: reset-device
        scopes: [physical-device]
        require_verified_unpartitioned_topology: true
`)
	candidate, replayed, err := mgr.CreateCandidate(context.Background(), "alice", CandidateUpload{Content: content, IdempotencyKey: "candidate-request"})
	if err != nil || replayed {
		t.Fatalf("create candidate = %#v replayed=%v err=%v", candidate, replayed, err)
	}
	again, replayed, err := mgr.CreateCandidate(context.Background(), "alice", CandidateUpload{Content: content, IdempotencyKey: "candidate-request"})
	if err != nil || !replayed || again.ID != candidate.ID {
		t.Fatalf("replay candidate = %#v replayed=%v err=%v", again, replayed, err)
	}
	preview, replayed, err := mgr.CreatePreview(context.Background(), "alice", candidate.ID, "preview-request")
	if err != nil || replayed {
		t.Fatalf("create preview = %#v replayed=%v err=%v", preview, replayed, err)
	}
	// The candidate profile is byte-for-byte the live profile, but it is not
	// deployed. The captured pre-deploy report attests the live profile, not
	// the candidate, so the hypothetical After answer must fail closed instead
	// of reading as an unchanged Eligible node.
	if preview.InventorySnapshotID == "" || len(preview.NewlyBlocked) != 1 || len(preview.Unchanged) != 0 {
		t.Fatalf("preview = %#v, want one fail-closed newly-blocked delta", preview)
	}
	delta := preview.NewlyBlocked[0]
	if delta.Before.State != decision.StateEligible || delta.After.State == decision.StateEligible || !delta.Changed {
		t.Fatalf("identical candidate profile delta = %#v, want Eligible before and a non-Eligible after", delta)
	}
	if delta.RuntimeContractImpact == nil || delta.RuntimeContractImpact.PostDeployAttestation != RuntimeContractAttestationFreshRequired {
		t.Fatalf("identical candidate profile impact = %#v, want FreshRequired", delta.RuntimeContractImpact)
	}
	againPreview, replayed, err := mgr.CreatePreview(context.Background(), "alice", candidate.ID, "preview-request")
	if err != nil || !replayed || againPreview.ID != preview.ID {
		t.Fatalf("preview replay = %#v replayed=%v err=%v", againPreview, replayed, err)
	}
}

func TestCandidateCompilerAcceptsNativeRuntimeProfileCR(t *testing.T) {
	mgr, st, _ := newOperationManager(t)
	defer func() { _ = st.Close() }()
	content := []byte(`
apiVersion: kubeneuron.io/v1alpha1
kind: AcceleratorRuntimeProfile
metadata:
  name: nvidia-a100
spec:
  nodeSelector:
    matchLabels:
      pool: a100
  vendor: nvidia
  profileDigest: sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
  driverVersion: "550.54.15"
  runtimeVersion: dcgm-3.3.5
  maxReportAge: 5m
  allowedActions:
    - action: reset-device
      scopes: [physical-device]
      requireVerifiedUnpartitionedTopology: true
`)
	candidate, replayed, err := mgr.CreateCandidate(context.Background(), "alice", CandidateUpload{Content: content, IdempotencyKey: "native-cr"})
	if err != nil || replayed {
		t.Fatalf("create native CR candidate = %#v replay=%v err=%v", candidate, replayed, err)
	}
	if len(candidate.Profiles) != 1 || !strings.HasPrefix(candidate.Profiles[0].ProfileUID, "candidate-") || candidate.ResourceVersion != 1 {
		t.Fatalf("compiled native CR candidate = %#v", candidate)
	}
	if _, _, err := mgr.CreateCandidate(context.Background(), "alice", CandidateUpload{Content: []byte("apiVersion: kubeneuron.io/v1alpha1\nkind: AcceleratorRuntimeProfile\nmetadata: {name: bad}\nspec: {unknown: true}\n"), IdempotencyKey: "bad-native-cr"}); err == nil {
		t.Fatal("unknown native CR field was accepted")
	}
}

func TestCandidateRevocationIsIdempotentAndPreventsPreview(t *testing.T) {
	mgr, st, _ := newOperationManager(t)
	defer func() { _ = st.Close() }()
	content := []byte(`
apiVersion: kubeneuron.io/v1alpha1
kind: CandidateConfiguration
accelerator_profiles:
  - name: nvidia-a100
    node_selector: {pool: a100}
    vendor: nvidia
    profile_digest: sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
    driver_version: "550.54.15"
    runtime_version: dcgm-3.3.5
    profile_uid: profile-uid
    profile_generation: 1
    max_report_age: 5m
    allowed_actions:
      - action: reset-device
        scopes: [physical-device]
        require_verified_unpartitioned_topology: true
`)
	candidate, _, err := mgr.CreateCandidate(context.Background(), "alice", CandidateUpload{Content: content, IdempotencyKey: "candidate-upload"})
	if err != nil {
		t.Fatal(err)
	}
	revoked, replayed, err := mgr.RevokeCandidate(context.Background(), "alice", candidate.ID, "replacement supersedes it", "candidate-revoke", candidate.ResourceVersion)
	if err != nil || replayed || revoked.State != "revoked" || revoked.RevokedAt == nil {
		t.Fatalf("revoke = %#v replay=%v err=%v", revoked, replayed, err)
	}
	replayedCandidate, replayed, err := mgr.RevokeCandidate(context.Background(), "alice", candidate.ID, "replacement supersedes it", "candidate-revoke", candidate.ResourceVersion)
	if err != nil || !replayed || replayedCandidate.ID != candidate.ID || replayedCandidate.State != "revoked" {
		t.Fatalf("revoke replay = %#v replay=%v err=%v", replayedCandidate, replayed, err)
	}
	if _, _, err := mgr.CreatePreview(context.Background(), "alice", candidate.ID, "revoked-preview"); err == nil || !strings.Contains(err.Error(), "invalid lifecycle") {
		t.Fatalf("preview of revoked candidate error = %v", err)
	}
}

func TestHealthCheckCancellationAndSimulationIncidentLink(t *testing.T) {
	mgr, st, now := newOperationManager(t)
	defer func() { _ = st.Close() }()
	ctx := context.Background()
	run, replayed, err := mgr.CreateHealthCheck(ctx, "alice", HealthCheckRequest{
		Node: "gpu-a", Profile: HealthCheckQuick, Reason: "verify recovery", IdempotencyKey: "health-request",
	})
	if err != nil || replayed || run.State != "queued" || run.ActionID == "" {
		t.Fatalf("health run = %#v replayed=%v err=%v", run, replayed, err)
	}
	resource, err := st.GetOperationalResource(ctx, types.ResourceHealthCheckRun, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	cancelled, err := mgr.CancelHealthCheck(ctx, "alice", run.ID, resource.Version)
	if err != nil || cancelled.State != "cancelled" {
		t.Fatalf("cancel health = %#v err=%v", cancelled, err)
	}

	simulation, replayed, err := mgr.CreateSimulation(ctx, "alice", SimulationRequest{
		Node: "gpu-a", DeviceID: "GPU-a", Action: types.AcceleratorActionResetDevice,
		Scope: types.AcceleratorScopePhysicalDevice, Class: types.ClassECCDBE,
		Rationale: "validate the frozen recovery path", IdempotencyKey: "simulation-request",
	})
	if err != nil || replayed || !simulation.Decision.Permitted() {
		t.Fatalf("simulation = %#v replayed=%v err=%v", simulation, replayed, err)
	}
	simResource, err := st.GetOperationalResource(ctx, types.ResourceRemediationSimulation, simulation.ID)
	if err != nil {
		t.Fatal(err)
	}
	incident, err := mgr.CreateIncidentFromSimulation(ctx, "alice", simulation.ID, "approved after review", simResource.Version)
	if err != nil || incident.ID != "incident-1" {
		t.Fatalf("incident = %#v err=%v", incident, err)
	}
	stored, err := mgr.GetSimulation(ctx, simulation.ID)
	if err != nil || stored.IncidentID != incident.ID {
		t.Fatalf("stored simulation = %#v err=%v", stored, err)
	}

	passive, _, err := mgr.CreateHealthCheck(ctx, "alice", HealthCheckRequest{
		Node: "gpu-a", Profile: HealthCheckPassive, Reason: "capture", IdempotencyKey: "passive-request",
	})
	if err != nil || passive.State != "completed" || !strings.HasPrefix(passive.EvidenceDigest, "sha256:") || !passive.Deadline.Equal(now.Add(maxQuickDiagnosticTimeout)) {
		t.Fatalf("passive = %#v err=%v", passive, err)
	}
}

func TestAutonomyPlanRequiresDistinctApprovalsAndReevaluatesCanary(t *testing.T) {
	mgr, st, now := newOperationManager(t)
	defer func() { _ = st.Close() }()
	ctx := context.Background()
	// The canary selector is part of the safety envelope, so the fixture's
	// inventory must prove the node belongs to it.
	mgr.listNodes = func(context.Context) ([]*types.Node, error) {
		return []*types.Node{{Name: "gpu-a", Labels: map[string]string{"pool": "a100"}}}, nil
	}
	simulation, _, err := mgr.CreateSimulation(ctx, "alice", SimulationRequest{
		Node: "gpu-a", DeviceID: "GPU-a", Action: types.AcceleratorActionResetDevice,
		Scope: types.AcceleratorScopePhysicalDevice, Class: types.ClassECCDBE,
		Rationale: "qualify narrow autonomy", IdempotencyKey: "autonomy-simulation",
	})
	if err != nil || !simulation.Decision.Permitted() {
		t.Fatalf("simulation = %#v err=%v", simulation, err)
	}
	plan, replayed, err := mgr.CreateAutonomyPlan(ctx, "author", AutonomyPlanRequest{
		Selector: map[string]string{"pool": "a100"}, PolicyRef: "ecc-reset@sha256:policy", ProfileRef: "nvidia-a100#1",
		AllowedActions: []types.AcceleratorAction{types.AcceleratorActionResetDevice},
		Evidence:       AutonomyEvidenceRequirements{MaxAge: 5 * time.Minute, RequiredSources: []string{"agent"}},
		Guardrails:     AutonomyGuardrails{MaxConcurrentNodes: 1, MaxActionsPerHour: 2, ErrorBudget: 0, NoActiveIncident: true},
		Rollout:        AutonomyRolloutPolicy{CanaryNodes: 1, BakeDuration: time.Minute},
		Approvals:      AutonomyApprovalRequirements{RequiredRoles: []string{"platform", "safety"}, DistinctSubjects: true},
		SimulationID:   simulation.ID, ExpiresAt: now.Add(time.Hour), IdempotencyKey: "autonomy-plan",
	})
	if err != nil || replayed || plan.State != AutonomySimulated || plan.ResourceVersion != 1 {
		t.Fatalf("plan = %#v replay=%v err=%v", plan, replayed, err)
	}
	if _, err := mgr.ApproveAutonomyPlan(ctx, "same-person", plan.ID, "platform", plan.ResourceVersion); err != nil {
		t.Fatalf("first approval: %v", err)
	}
	plan, err = mgr.GetAutonomyPlan(ctx, plan.ID)
	if err != nil || plan.State != AutonomyAwaitingApproval {
		t.Fatalf("after first approval = %#v err=%v", plan, err)
	}
	if _, err := mgr.ApproveAutonomyPlan(ctx, "same-person", plan.ID, "safety", plan.ResourceVersion); err == nil {
		t.Fatal("one actor satisfied both required roles")
	}
	plan, err = mgr.GetAutonomyPlan(ctx, plan.ID)
	if err != nil {
		t.Fatal(err)
	}
	plan, err = mgr.ApproveAutonomyPlan(ctx, "safety-person", plan.ID, "safety", plan.ResourceVersion)
	if err != nil || plan.State != AutonomyCanary || plan.RolloutID == "" {
		t.Fatalf("second approval = %#v err=%v", plan, err)
	}
	if err := mgr.ReconcileAutonomy(ctx); err != nil {
		t.Fatal(err)
	}
	plan, err = mgr.GetAutonomyPlan(ctx, plan.ID)
	if err != nil || plan.State != AutonomyBaking {
		t.Fatalf("canary reconciliation = %#v err=%v", plan, err)
	}
	rollout, err := mgr.GetAutonomyRollout(ctx, plan.ID)
	if err != nil || len(rollout.Observations) != 1 || !rollout.Observations[0].Decision.Permitted() {
		t.Fatalf("rollout = %#v err=%v", rollout, err)
	}
	staleBaking := *plan
	paused, err := mgr.PauseAutonomyPlan(ctx, "operator", plan.ID, "hold for review", plan.ResourceVersion)
	if err != nil {
		t.Fatalf("pause plan: %v", err)
	}
	if err := mgr.moveAutonomyPlan(ctx, &staleBaking, AutonomyEnabled, "stale bake completion"); !errors.Is(err, store.ErrOperationalConflict) {
		t.Fatalf("stale transition after pause = %v, want optimistic conflict", err)
	}
	current, err := mgr.GetAutonomyPlan(ctx, plan.ID)
	if err != nil || current.State != AutonomyPaused {
		t.Fatalf("pause must win stale reconciliation: %#v err=%v", current, err)
	}
	resumed, err := mgr.ResumeAutonomyPlan(ctx, "operator", plan.ID, "fresh review", paused.ResourceVersion)
	if err != nil {
		t.Fatalf("resume plan: %v", err)
	}
	staleCanary := *resumed
	rolledBack, err := mgr.RollbackAutonomyPlan(ctx, "operator", plan.ID, "emergency stop", resumed.ResourceVersion)
	if err != nil {
		t.Fatalf("rollback plan: %v", err)
	}
	if err := mgr.moveAutonomyPlan(ctx, &staleCanary, AutonomyBaking, "stale canary completion"); !errors.Is(err, store.ErrOperationalConflict) {
		t.Fatalf("stale transition after rollback = %v, want optimistic conflict", err)
	}
	if rolledBack.State != AutonomyRolledBack {
		t.Fatalf("rollback must win stale reconciliation: %#v", rolledBack)
	}
}

func TestAutonomyCanaryUsesPersistedIdempotentEffectHandOff(t *testing.T) {
	mgr, st, now := newOperationManager(t)
	defer func() { _ = st.Close() }()
	ctx := context.Background()
	current := now
	mgr.now = func() time.Time { return current }
	mgr.listNodes = func(context.Context) ([]*types.Node, error) {
		return []*types.Node{{Name: "gpu-a", Labels: map[string]string{"pool": "a100"}}}, nil
	}
	calls := 0
	mgr.autonomyExecute = func(_ context.Context, effect AutonomyEffectRequest) (AutonomyEffectResult, error) {
		calls++
		if effect.EffectID == "" || effect.DecisionSnapshotID == "" || effect.Action != types.AcceleratorActionResetDevice || effect.TargetDeviceID != "GPU-a" {
			t.Fatalf("effect request = %#v", effect)
		}
		return AutonomyEffectResult{State: "accepted", Summary: "qualified executor accepted", Reference: "effect://test/1"}, nil
	}
	simulation, _, err := mgr.CreateSimulation(ctx, "alice", SimulationRequest{
		Node: "gpu-a", DeviceID: "GPU-a", Action: types.AcceleratorActionResetDevice, Scope: types.AcceleratorScopePhysicalDevice,
		Class: types.ClassECCDBE, Rationale: "qualify executor hand-off", IdempotencyKey: "effect-simulation",
	})
	if err != nil {
		t.Fatal(err)
	}
	plan, _, err := mgr.CreateAutonomyPlan(ctx, "author", AutonomyPlanRequest{
		Selector: map[string]string{"pool": "a100"}, PolicyRef: "ecc@sha256:policy", ProfileRef: "a100#1",
		AllowedActions: []types.AcceleratorAction{types.AcceleratorActionResetDevice},
		Evidence:       AutonomyEvidenceRequirements{MaxAge: 5 * time.Minute, RequiredSources: []string{"agent"}},
		Guardrails:     AutonomyGuardrails{MaxConcurrentNodes: 1, MaxActionsPerHour: 2, ErrorBudget: 0, NoActiveIncident: true},
		Rollout:        AutonomyRolloutPolicy{CanaryNodes: 1, BakeDuration: time.Minute},
		Approvals:      AutonomyApprovalRequirements{RequiredRoles: []string{"platform", "safety"}, DistinctSubjects: true},
		SimulationID:   simulation.ID, ExpiresAt: now.Add(time.Hour), IdempotencyKey: "effect-plan",
	})
	if err != nil {
		t.Fatal(err)
	}
	plan, err = mgr.ApproveAutonomyPlan(ctx, "platform", plan.ID, "platform", plan.ResourceVersion)
	if err != nil {
		t.Fatal(err)
	}
	plan, err = mgr.ApproveAutonomyPlan(ctx, "safety", plan.ID, "safety", plan.ResourceVersion)
	if err != nil {
		t.Fatal(err)
	}
	if err := mgr.ReconcileAutonomy(ctx); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("executor calls = %d, want one", calls)
	}
	rollout, err := mgr.GetAutonomyRollout(ctx, plan.ID)
	if err != nil || rollout.ExecutionMode != "hardware-qualified" || rollout.ActionsThisHour != 1 || len(rollout.Observations) != 1 || rollout.Observations[0].EffectID == "" {
		t.Fatalf("rollout = %#v err=%v", rollout, err)
	}
	effect, err := st.GetOperationalResource(ctx, types.ResourceAutonomyEffect, rollout.Observations[0].EffectID)
	if err != nil || effect.State != "accepted" {
		t.Fatalf("effect resource = %#v err=%v", effect, err)
	}
	if effect.ExpiresAt == nil || !effect.ExpiresAt.Equal(plan.ExpiresAt) {
		t.Fatalf("effect must be retained through the plan envelope: effect=%#v plan=%#v", effect.ExpiresAt, plan.ExpiresAt)
	}
	// An executor acknowledgement is not itself a successful bake. The current
	// fixture's report predates the effect, so the first matured bake must
	// persist that missing post-effect evidence and roll back at error budget 0.
	current = current.Add(time.Minute)
	if err := mgr.ReconcileAutonomy(ctx); err != nil {
		t.Fatal(err)
	}
	plan, err = mgr.GetAutonomyPlan(ctx, plan.ID)
	if err != nil || plan.State != AutonomyRolledBack {
		t.Fatalf("stale post-effect evidence must roll back bake: %#v err=%v", plan, err)
	}
	rollout, err = mgr.GetAutonomyRollout(ctx, plan.ID)
	if err != nil || len(rollout.BakeMeasurements) != 1 || !strings.Contains(rollout.BakeMeasurements[0].Summary, "not observed after") {
		t.Fatalf("bake measurement = %#v err=%v", rollout, err)
	}
}

func TestAutonomyNeverExecutesAfterConfigurationBindingChanges(t *testing.T) {
	mgr, st, now := newOperationManager(t)
	defer func() { _ = st.Close() }()
	ctx := context.Background()
	mgr.listNodes = func(context.Context) ([]*types.Node, error) {
		return []*types.Node{{Name: "gpu-a", Labels: map[string]string{"pool": "a100"}}}, nil
	}
	configDigest := "sha256:live-config"
	mgr.buildSnapshot = func(_ context.Context, node string, request decision.Request) (decision.Snapshot, error) {
		snapshot := operationSnapshot(now, request)
		snapshot.Node.Name, snapshot.Report.Node = node, node
		snapshot.ConfigDigest = configDigest
		return snapshot, nil
	}
	executorCalls := 0
	mgr.autonomyExecute = func(context.Context, AutonomyEffectRequest) (AutonomyEffectResult, error) {
		executorCalls++
		return AutonomyEffectResult{State: "accepted"}, nil
	}
	simulation, _, err := mgr.CreateSimulation(ctx, "alice", SimulationRequest{
		Node: "gpu-a", DeviceID: "GPU-a", Action: types.AcceleratorActionResetDevice,
		Scope: types.AcceleratorScopePhysicalDevice, Class: types.ClassECCDBE,
		Rationale: "freeze approved revision", IdempotencyKey: "changed-config-simulation",
	})
	if err != nil || !simulation.Decision.Permitted() {
		t.Fatalf("simulation = %#v err=%v", simulation, err)
	}
	plan, _, err := mgr.CreateAutonomyPlan(ctx, "author", AutonomyPlanRequest{
		Selector: map[string]string{"pool": "a100"}, PolicyRef: "ecc@sha256:policy", ProfileRef: "a100#1",
		AllowedActions: []types.AcceleratorAction{types.AcceleratorActionResetDevice},
		Evidence:       AutonomyEvidenceRequirements{MaxAge: 5 * time.Minute, RequiredSources: []string{"agent"}},
		Guardrails:     AutonomyGuardrails{MaxConcurrentNodes: 1, MaxActionsPerHour: 1, ErrorBudget: 0, NoActiveIncident: true},
		Rollout:        AutonomyRolloutPolicy{CanaryNodes: 1, BakeDuration: time.Minute},
		Approvals:      AutonomyApprovalRequirements{RequiredRoles: []string{"platform", "safety"}, DistinctSubjects: true},
		SimulationID:   simulation.ID, ExpiresAt: now.Add(time.Hour), IdempotencyKey: "changed-config-plan",
	})
	if err != nil {
		t.Fatal(err)
	}
	plan, err = mgr.ApproveAutonomyPlan(ctx, "platform", plan.ID, "platform", plan.ResourceVersion)
	if err != nil {
		t.Fatal(err)
	}
	plan, err = mgr.ApproveAutonomyPlan(ctx, "safety", plan.ID, "safety", plan.ResourceVersion)
	if err != nil {
		t.Fatal(err)
	}

	// The live controller configuration changes after the approval/simulation
	// were frozen. The canary must retain the blocked observation rather than
	// replacing this value with the plan's historical digest.
	configDigest = "sha256:replacement-config"
	if err := mgr.ReconcileAutonomy(ctx); err != nil {
		t.Fatal(err)
	}
	if executorCalls != 0 {
		t.Fatalf("executor calls = %d, want zero after config change", executorCalls)
	}
	plan, err = mgr.GetAutonomyPlan(ctx, plan.ID)
	if err != nil || plan.State != AutonomyPaused {
		t.Fatalf("plan = %#v err=%v, want paused", plan, err)
	}
	rollout, err := mgr.GetAutonomyRollout(ctx, plan.ID)
	if err != nil || len(rollout.Observations) != 1 {
		t.Fatalf("rollout = %#v err=%v", rollout, err)
	}
	if got := rollout.Observations[0].Decision.ReasonCodes; len(got) != 1 || got[0] != decision.ReasonConfigurationChanged {
		t.Fatalf("rollout reasons = %#v, want configuration change", got)
	}
}

func TestDiagnosticResultSummaryNeverCopiesRawOutput(t *testing.T) {
	state, outcome, digest := diagnosticActionResultSummary(&types.ActionResult{
		OK: true, Output: "token=top-secret\\nworkload=private-job",
	})
	if state != "completed" || !strings.HasPrefix(digest, "sha256:") {
		t.Fatalf("summary = state=%q digest=%q", state, digest)
	}
	if strings.Contains(outcome, "top-secret") || strings.Contains(outcome, "private-job") {
		t.Fatalf("raw diagnostic output leaked into summary %q", outcome)
	}
	state, outcome, digest = diagnosticActionResultSummary(&types.ActionResult{OK: false, Error: "driver unsupported: token=still-secret"})
	if state != "failed" || !strings.Contains(outcome, "unsupported") || !strings.HasPrefix(digest, "sha256:") || strings.Contains(outcome, "still-secret") {
		t.Fatalf("failed summary = state=%q outcome=%q digest=%q", state, outcome, digest)
	}
}

func TestAutonomyExpansionBakesEachBoundedBatch(t *testing.T) {
	mgr, st, now := newOperationManager(t)
	defer func() { _ = st.Close() }()
	ctx := context.Background()
	current := now
	mgr.now = func() time.Time { return current }
	mgr.listNodes = func(context.Context) ([]*types.Node, error) {
		return []*types.Node{
			{Name: "gpu-a", Labels: map[string]string{"pool": "a100"}},
			{Name: "gpu-b", Labels: map[string]string{"pool": "a100"}},
			{Name: "gpu-c", Labels: map[string]string{"pool": "a100"}},
		}, nil
	}
	mgr.buildSnapshot = func(_ context.Context, node string, request decision.Request) (decision.Snapshot, error) {
		snapshot := operationSnapshot(current, request)
		snapshot.Node.Name, snapshot.Node.UID = node, "uid-"+node
		snapshot.Report.Node, snapshot.Report.NodeUID = node, snapshot.Node.UID
		if request.TargetDeviceID == "" {
			snapshot.Report.Devices[0].ID = "GPU-" + node
		}
		return snapshot, nil
	}
	simulation, _, err := mgr.CreateSimulation(ctx, "alice", SimulationRequest{
		Node: "gpu-a", DeviceID: "GPU-a", Action: types.AcceleratorActionResetDevice,
		Scope: types.AcceleratorScopePhysicalDevice, Class: types.ClassECCDBE,
		Rationale: "qualify expansion", IdempotencyKey: "expansion-simulation",
	})
	if err != nil {
		t.Fatal(err)
	}
	plan, _, err := mgr.CreateAutonomyPlan(ctx, "author", AutonomyPlanRequest{
		Selector: map[string]string{"pool": "a100"}, PolicyRef: "ecc@sha256:policy", ProfileRef: "a100#1",
		AllowedActions: []types.AcceleratorAction{types.AcceleratorActionResetDevice},
		Evidence:       AutonomyEvidenceRequirements{MaxAge: 5 * time.Minute, RequiredSources: []string{"agent"}},
		Guardrails:     AutonomyGuardrails{MaxConcurrentNodes: 1, MaxActionsPerHour: 10, ErrorBudget: 0, NoActiveIncident: true},
		Rollout:        AutonomyRolloutPolicy{CanaryNodes: 1, BakeDuration: time.Minute, ExpansionSteps: []int{2, 3}},
		Approvals:      AutonomyApprovalRequirements{RequiredRoles: []string{"platform", "safety"}, DistinctSubjects: true},
		SimulationID:   simulation.ID, ExpiresAt: current.Add(time.Hour), IdempotencyKey: "expansion-plan",
	})
	if err != nil {
		t.Fatal(err)
	}
	plan, err = mgr.ApproveAutonomyPlan(ctx, "platform", plan.ID, "platform", plan.ResourceVersion)
	if err != nil {
		t.Fatal(err)
	}
	plan, err = mgr.ApproveAutonomyPlan(ctx, "safety", plan.ID, "safety", plan.ResourceVersion)
	if err != nil {
		t.Fatal(err)
	}
	if err := mgr.ReconcileAutonomy(ctx); err != nil { // canary -> baking
		t.Fatal(err)
	}
	current = current.Add(time.Minute)
	if err := mgr.ReconcileAutonomy(ctx); err != nil { // baking -> expanding
		t.Fatal(err)
	}
	if err := mgr.ReconcileAutonomy(ctx); err != nil { // one-node expansion -> baking
		t.Fatal(err)
	}
	current = current.Add(time.Minute)
	if err := mgr.ReconcileAutonomy(ctx); err != nil { // baking -> expanding
		t.Fatal(err)
	}
	if err := mgr.ReconcileAutonomy(ctx); err != nil { // second bounded batch -> baking
		t.Fatal(err)
	}
	current = current.Add(time.Minute)
	if err := mgr.ReconcileAutonomy(ctx); err != nil { // final bake -> enabled
		t.Fatal(err)
	}
	plan, err = mgr.GetAutonomyPlan(ctx, plan.ID)
	if err != nil || plan.State != AutonomyEnabled {
		t.Fatalf("expanded plan = %#v err=%v", plan, err)
	}
	rollout, err := mgr.GetAutonomyRollout(ctx, plan.ID)
	if err != nil || len(rollout.CanaryNodes) != 1 || len(rollout.ExpandedNodes) != 2 || len(rollout.Observations) != 3 {
		t.Fatalf("expanded rollout = %#v err=%v", rollout, err)
	}
	if rollout.Observations[0].Node != "gpu-a" || rollout.Observations[1].Node != "gpu-b" || rollout.Observations[2].Node != "gpu-c" {
		t.Fatalf("expansion order = %#v", rollout.Observations)
	}
}

func TestAutonomyPlanRejectsSimulationFromDifferentScope(t *testing.T) {
	mgr, st, now := newOperationManager(t)
	defer func() { _ = st.Close() }()
	ctx := context.Background()
	mgr.buildSnapshot = func(_ context.Context, node string, request decision.Request) (decision.Snapshot, error) {
		snapshot := operationSnapshot(now, request)
		snapshot.Node.Name, snapshot.Report.Node = node, node
		snapshot.Node.Labels["kubeneuron.io/tenant"] = "tenant-a"
		return snapshot, nil
	}
	simulation, _, err := mgr.CreateSimulation(ctx, "alice", SimulationRequest{
		Node: "gpu-a", DeviceID: "GPU-a", Action: types.AcceleratorActionResetDevice,
		Scope: types.AcceleratorScopePhysicalDevice, Class: types.ClassECCDBE,
		Rationale: "reset only", Tenant: "tenant-a", IdempotencyKey: "scope-simulation",
	})
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = mgr.CreateAutonomyPlan(ctx, "author", AutonomyPlanRequest{
		Selector: map[string]string{"pool": "a100"}, PolicyRef: "ecc@sha256:policy", ProfileRef: "a100#1",
		AllowedActions: []types.AcceleratorAction{types.AcceleratorActionRestartRuntime},
		Evidence:       AutonomyEvidenceRequirements{MaxAge: 5 * time.Minute, RequiredSources: []string{"agent"}},
		Guardrails:     AutonomyGuardrails{MaxConcurrentNodes: 1, MaxActionsPerHour: 1, ErrorBudget: 0, NoActiveIncident: true},
		Rollout:        AutonomyRolloutPolicy{CanaryNodes: 1, BakeDuration: time.Minute},
		Approvals:      AutonomyApprovalRequirements{RequiredRoles: []string{"platform", "safety"}, DistinctSubjects: true},
		SimulationID:   simulation.ID, Tenant: "tenant-a", ExpiresAt: now.Add(time.Hour), IdempotencyKey: "wrong-action-plan",
	})
	if err == nil || !strings.Contains(err.Error(), "does not match plan action") {
		t.Fatalf("different-action simulation error = %v", err)
	}
}

func TestAutonomyNamedMaintenanceWindowIsARequiredLiveGate(t *testing.T) {
	mgr, st, now := newOperationManager(t)
	defer func() { _ = st.Close() }()
	ctx := context.Background()
	plan := &GPUAutonomyPlan{
		ConfigDigest:   "sha256:live-config",
		AllowedActions: []types.AcceleratorAction{types.AcceleratorActionResetDevice},
		Guardrails:     AutonomyGuardrails{MaintenanceWindows: []string{"scheduled-maintenance"}},
		Approvals:      AutonomyApprovalRequirements{RequiredRoles: []string{"platform", "safety"}, DistinctSubjects: true},
		ApprovalRecords: []AutonomyApproval{
			{Actor: "platform-person", Role: "platform", ConfigDigest: "sha256:live-config", ApprovedAt: now},
			{Actor: "safety-person", Role: "safety", ConfigDigest: "sha256:live-config", ApprovedAt: now},
		},
	}
	if _, _, err := mgr.autonomyDecisionRequest(ctx, plan, "gpu-a", "GPU-a"); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("missing maintenance resolver = %v, want unavailable", err)
	}
	fullyGranted := func(request decision.Request) bool {
		return request.MaintenanceRequired && request.AllowDuringMaintenance && request.ApprovalRequired &&
			request.ApprovalGranted && request.ElevatedAuthorizationGranted && request.DisruptionBudgetApproved
	}
	mgr.autonomyMaintenanceWindowActive = func(_ context.Context, node string, refs []string) (bool, error) {
		if node != "gpu-a" || len(refs) != 1 || refs[0] != "scheduled-maintenance" {
			t.Fatalf("maintenance resolver input = node %q refs %#v", node, refs)
		}
		return false, nil
	}
	request, active, err := mgr.autonomyDecisionRequest(ctx, plan, "gpu-a", "GPU-a")
	if err != nil || active || !fullyGranted(request) {
		t.Fatalf("closed named window request = %#v active=%v err=%v", request, active, err)
	}
	closed := operationSnapshot(now, request)
	closed.MaintenanceActive = active
	if got := decision.Evaluate(closed); got.Permitted() || len(got.ReasonCodes) != 1 || got.ReasonCodes[0] != decision.ReasonMaintenanceWindowClosed {
		t.Fatalf("closed named window decision = %#v, want MaintenanceWindowClosed", got)
	}

	mgr.autonomyMaintenanceWindowActive = func(context.Context, string, []string) (bool, error) { return true, nil }
	request, active, err = mgr.autonomyDecisionRequest(ctx, plan, "gpu-a", "GPU-a")
	if err != nil || !active || !fullyGranted(request) {
		t.Fatalf("open named window request = %#v active=%v err=%v", request, active, err)
	}
	open := operationSnapshot(now, request)
	open.MaintenanceActive = active
	if got := decision.Evaluate(open); !got.Permitted() {
		t.Fatalf("open named window decision = %#v, want eligible", got)
	}

	// The open window is necessary, never sufficient: the grants come from the
	// plan's current digest-bound approvals, so a plan missing one required
	// role must still be answered with ApprovalMissing.
	partial := *plan
	partial.ApprovalRecords = plan.ApprovalRecords[:1]
	request, active, err = mgr.autonomyDecisionRequest(ctx, &partial, "gpu-a", "GPU-a")
	if err != nil || !active || !request.MaintenanceRequired || request.ApprovalGranted ||
		request.ElevatedAuthorizationGranted || request.DisruptionBudgetApproved {
		t.Fatalf("partially approved request = %#v active=%v err=%v", request, active, err)
	}
	unapproved := operationSnapshot(now, request)
	unapproved.MaintenanceActive = active
	if got := decision.Evaluate(unapproved); got.Permitted() || len(got.ReasonCodes) != 1 || got.ReasonCodes[0] != decision.ReasonApprovalMissing {
		t.Fatalf("partially approved decision = %#v, want ApprovalMissing", got)
	}
}

func TestAutonomyNamedMaintenanceWindowAdmitsApprovedCanaryOnlyWhileOpen(t *testing.T) {
	mgr, st, now := newOperationManager(t)
	defer func() { _ = st.Close() }()
	ctx := context.Background()
	current := now
	mgr.now = func() time.Time { return current }
	mgr.listNodes = func(context.Context) ([]*types.Node, error) {
		return []*types.Node{{Name: "gpu-a", Labels: map[string]string{"pool": "a100"}}}, nil
	}
	windowOpen := false
	mgr.autonomyMaintenanceWindowActive = func(_ context.Context, node string, refs []string) (bool, error) {
		if node != "gpu-a" || len(refs) != 1 || refs[0] != "scheduled-maintenance" {
			t.Fatalf("maintenance resolver input = node %q refs %#v", node, refs)
		}
		return windowOpen, nil
	}
	simulation, _, err := mgr.CreateSimulation(ctx, "alice", SimulationRequest{
		Node: "gpu-a", DeviceID: "GPU-a", Action: types.AcceleratorActionResetDevice,
		Scope: types.AcceleratorScopePhysicalDevice, Class: types.ClassECCDBE,
		Rationale: "qualify window-bound autonomy", IdempotencyKey: "window-simulation",
	})
	if err != nil || !simulation.Decision.Permitted() {
		t.Fatalf("simulation = %#v err=%v", simulation, err)
	}
	plan, _, err := mgr.CreateAutonomyPlan(ctx, "author", AutonomyPlanRequest{
		Selector: map[string]string{"pool": "a100"}, PolicyRef: "ecc-reset@sha256:policy", ProfileRef: "nvidia-a100#1",
		AllowedActions: []types.AcceleratorAction{types.AcceleratorActionResetDevice},
		Evidence:       AutonomyEvidenceRequirements{MaxAge: 5 * time.Minute, RequiredSources: []string{"agent"}},
		Guardrails: AutonomyGuardrails{
			MaintenanceWindows: []string{"scheduled-maintenance"},
			MaxConcurrentNodes: 1, MaxActionsPerHour: 2, ErrorBudget: 0, NoActiveIncident: true,
		},
		Rollout:      AutonomyRolloutPolicy{CanaryNodes: 1, BakeDuration: time.Minute},
		Approvals:    AutonomyApprovalRequirements{RequiredRoles: []string{"platform", "safety"}, DistinctSubjects: true},
		SimulationID: simulation.ID, ExpiresAt: now.Add(time.Hour), IdempotencyKey: "window-plan",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := mgr.ApproveAutonomyPlan(ctx, "platform-person", plan.ID, "platform", plan.ResourceVersion); err != nil {
		t.Fatalf("platform approval: %v", err)
	}
	plan, err = mgr.GetAutonomyPlan(ctx, plan.ID)
	if err != nil {
		t.Fatal(err)
	}
	plan, err = mgr.ApproveAutonomyPlan(ctx, "safety-person", plan.ID, "safety", plan.ResourceVersion)
	if err != nil || plan.State != AutonomyCanary {
		t.Fatalf("approved plan = %#v err=%v", plan, err)
	}

	// Approvals alone do not open the envelope: with every referenced window
	// closed the canary is recorded as blocked and the plan pauses.
	if err := mgr.ReconcileAutonomy(ctx); err != nil {
		t.Fatal(err)
	}
	plan, err = mgr.GetAutonomyPlan(ctx, plan.ID)
	if err != nil || plan.State != AutonomyPaused {
		t.Fatalf("closed-window reconciliation = %#v err=%v", plan, err)
	}
	rollout, err := mgr.GetAutonomyRollout(ctx, plan.ID)
	if err != nil || len(rollout.Observations) != 1 {
		t.Fatalf("closed-window rollout = %#v err=%v", rollout, err)
	}
	blocked := rollout.Observations[0].Decision
	if blocked.Permitted() || len(blocked.ReasonCodes) != 1 || blocked.ReasonCodes[0] != decision.ReasonMaintenanceWindowClosed {
		t.Fatalf("closed-window observation = %#v, want MaintenanceWindowClosed", blocked)
	}

	// The same approved plan is admitted once a referenced window is open for
	// the node, and the simulation-only canary proceeds through its bake.
	plan, err = mgr.ResumeAutonomyPlan(ctx, "operator", plan.ID, "window is scheduled", plan.ResourceVersion)
	if err != nil || plan.State != AutonomyCanary {
		t.Fatalf("resumed plan = %#v err=%v", plan, err)
	}
	windowOpen = true
	if err := mgr.ReconcileAutonomy(ctx); err != nil {
		t.Fatal(err)
	}
	plan, err = mgr.GetAutonomyPlan(ctx, plan.ID)
	if err != nil || plan.State != AutonomyBaking {
		t.Fatalf("open-window reconciliation = %#v err=%v", plan, err)
	}
	rollout, err = mgr.GetAutonomyRollout(ctx, plan.ID)
	if err != nil || len(rollout.Observations) != 2 {
		t.Fatalf("open-window rollout = %#v err=%v", rollout, err)
	}
	admitted := rollout.Observations[1]
	if !admitted.Decision.Permitted() || admitted.EffectState != "simulation-only" {
		t.Fatalf("open-window observation = %#v, want permitted simulation-only canary", admitted)
	}
	current = current.Add(time.Minute)
	if err := mgr.ReconcileAutonomy(ctx); err != nil {
		t.Fatal(err)
	}
	plan, err = mgr.GetAutonomyPlan(ctx, plan.ID)
	if err != nil || plan.State != AutonomyEnabled {
		t.Fatalf("post-bake plan = %#v err=%v", plan, err)
	}
}

func TestOperationalRequestsRejectNodeOutsideDeclaredTenantScope(t *testing.T) {
	mgr, st, _ := newOperationManager(t)
	defer func() { _ = st.Close() }()
	_, _, err := mgr.CreateSimulation(context.Background(), "alice", SimulationRequest{
		Node: "gpu-a", DeviceID: "GPU-a", Action: types.AcceleratorActionResetDevice,
		Scope: types.AcceleratorScopePhysicalDevice, Class: types.ClassECCDBE,
		Rationale: "tenant fence", Tenant: "tenant-a", IdempotencyKey: "tenant-fence",
	})
	if err == nil || !strings.Contains(err.Error(), "outside requested tenant/cluster scope") {
		t.Fatalf("cross-tenant simulation error = %v", err)
	}
}

func TestPreviewRetryKeyIsNotConsumedByIncompleteInventory(t *testing.T) {
	mgr, st, now := newOperationManager(t)
	defer func() { _ = st.Close() }()
	ctx := context.Background()
	content := []byte(`
apiVersion: kubeneuron.io/v1alpha1
kind: CandidateConfiguration
accelerator_profiles:
  - name: nvidia-a100
    node_selector: {pool: a100}
    vendor: nvidia
    profile_digest: sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
    driver_version: "550.54.15"
    runtime_version: dcgm-3.3.5
    profile_uid: profile-uid
    profile_generation: 1
    max_report_age: 5m
    allowed_actions:
      - action: reset-device
        scopes: [physical-device]
        require_verified_unpartitioned_topology: true
`)
	candidate, _, err := mgr.CreateCandidate(ctx, "alice", CandidateUpload{Content: content, IdempotencyKey: "preview-candidate"})
	if err != nil {
		t.Fatal(err)
	}
	ready := false
	mgr.buildSnapshot = func(_ context.Context, node string, request decision.Request) (decision.Snapshot, error) {
		snapshot := operationSnapshot(now, request)
		snapshot.Node.Name, snapshot.Report.Node = node, node
		if !ready {
			snapshot.Report.ObservedAt = now.Add(-time.Hour)
		}
		return snapshot, nil
	}
	if _, _, err := mgr.CreatePreview(ctx, "alice", candidate.ID, "retry-after-evidence"); !errors.Is(err, ErrIncompleteInventory) {
		t.Fatalf("incomplete preview error = %v, want incomplete inventory", err)
	}
	ready = true
	preview, replayed, err := mgr.CreatePreview(ctx, "alice", candidate.ID, "retry-after-evidence")
	if err != nil || replayed || preview.ID == "" {
		t.Fatalf("recovered preview = %#v replayed=%v err=%v", preview, replayed, err)
	}
}

// newPreviewCandidate uploads a candidate that matches the operationSnapshot
// profile so CreatePreview reaches inventory capture. The returned builder log
// records every node CreatePreview asked BuildSnapshot about.
func newPreviewCandidate(t *testing.T, mgr *Manager, now time.Time) (*CandidateConfiguration, *[]string) {
	t.Helper()
	content := []byte(`
apiVersion: kubeneuron.io/v1alpha1
kind: CandidateConfiguration
accelerator_profiles:
  - name: nvidia-a100
    node_selector: {pool: a100}
    vendor: nvidia
    profile_digest: sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
    driver_version: "550.54.15"
    runtime_version: dcgm-3.3.5
    profile_uid: profile-uid
    profile_generation: 1
    max_report_age: 5m
    allowed_actions:
      - action: reset-device
        scopes: [physical-device]
        require_verified_unpartitioned_topology: true
`)
	candidate, _, err := mgr.CreateCandidate(context.Background(), "alice", CandidateUpload{Content: content, IdempotencyKey: "preview-candidate"})
	if err != nil {
		t.Fatal(err)
	}
	built := &[]string{}
	mgr.buildSnapshot = func(_ context.Context, node string, request decision.Request) (decision.Snapshot, error) {
		*built = append(*built, node)
		snapshot := operationSnapshot(now, request)
		snapshot.Node.Name, snapshot.Node.UID = node, "uid-"+node
		snapshot.Report.Node, snapshot.Report.NodeUID = node, snapshot.Node.UID
		return snapshot, nil
	}
	return candidate, built
}

func TestCreatePreviewRejectsIncompleteInventoryBeforeCaptureOrReservation(t *testing.T) {
	// Each inventory places the defect where the sort comparator would have
	// dereferenced it before validation: last, first, and between valid nodes.
	for name, nodes := range map[string][]*types.Node{
		"nil last":   {{Name: "gpu-a"}, nil},
		"nil first":  {nil, {Name: "gpu-b"}, {Name: "gpu-a"}},
		"blank name": {{Name: "gpu-b"}, {Name: "  "}, {Name: "gpu-a"}},
		"empty name": {{Name: "gpu-b"}, {Name: ""}, {Name: "gpu-a"}},
		"only nil":   {nil},
	} {
		t.Run(name, func(t *testing.T) {
			mgr, st, now := newOperationManager(t)
			defer func() { _ = st.Close() }()
			ctx := context.Background()
			candidate, built := newPreviewCandidate(t, mgr, now)
			mgr.listNodes = func(context.Context) ([]*types.Node, error) { return nodes, nil }

			preview, replayed, err := mgr.CreatePreview(ctx, "alice", candidate.ID, "incomplete-inventory")
			if !errors.Is(err, ErrIncompleteInventory) || preview != nil || replayed {
				t.Fatalf("incomplete inventory = (%#v, %v, %v), want ErrIncompleteInventory and no preview", preview, replayed, err)
			}
			if len(*built) != 0 {
				t.Fatalf("built snapshots for %v before rejecting the inventory; an incomplete inventory must not be evaluated at all", *built)
			}
			if _, lookupErr := st.GetOperationalIdempotency(ctx, types.ResourcePolicyImpactPreview, "alice", scopedIdempotencyKey("preview", "incomplete-inventory")); !errors.Is(lookupErr, store.ErrNotFound) {
				t.Fatalf("idempotency lookup after rejection = %v, want ErrNotFound: the retry key must not be reserved", lookupErr)
			}
			previews, err := mgr.ListPreviews(ctx, OperationalListOptions{})
			if err != nil || len(previews) != 0 {
				t.Fatalf("durable previews after rejection = (%v, %v), want none", previews, err)
			}

			// The same key remains usable once the inventory is complete.
			mgr.listNodes = func(context.Context) ([]*types.Node, error) {
				return []*types.Node{{Name: "gpu-a", Labels: map[string]string{"pool": "a100"}}}, nil
			}
			preview, replayed, err = mgr.CreatePreview(ctx, "alice", candidate.ID, "incomplete-inventory")
			if err != nil || replayed || preview == nil || preview.ID == "" {
				t.Fatalf("recovered preview = (%#v, %v, %v)", preview, replayed, err)
			}
		})
	}
}

func TestCreatePreviewDoesNotReorderListerInventory(t *testing.T) {
	mgr, st, now := newOperationManager(t)
	defer func() { _ = st.Close() }()
	ctx := context.Background()
	candidate, built := newPreviewCandidate(t, mgr, now)
	nodes := []*types.Node{
		{Name: "gpu-c", Labels: map[string]string{"pool": "a100"}},
		{Name: "gpu-a", Labels: map[string]string{"pool": "a100"}},
		{Name: "gpu-b", Labels: map[string]string{"pool": "a100"}},
	}
	mgr.listNodes = func(context.Context) ([]*types.Node, error) { return nodes, nil }

	preview, replayed, err := mgr.CreatePreview(ctx, "alice", candidate.ID, "ordered-inventory")
	if err != nil || replayed || preview == nil {
		t.Fatalf("preview = (%#v, %v, %v)", preview, replayed, err)
	}
	if got := strings.Join(*built, ","); got != "gpu-a,gpu-b,gpu-c" {
		t.Fatalf("preview captured nodes in order %q, want stable name order gpu-a,gpu-b,gpu-c", got)
	}
	if nodes[0].Name != "gpu-c" || nodes[1].Name != "gpu-a" || nodes[2].Name != "gpu-b" {
		t.Fatalf("lister inventory was reordered in place: %v %v %v", nodes[0].Name, nodes[1].Name, nodes[2].Name)
	}
	total := len(preview.NewlyEligible) + len(preview.NewlyBlocked) + len(preview.ChangedObservedOnly) + len(preview.Unchanged)
	if total != 3 {
		t.Fatalf("preview covered %d nodes, want all 3: %#v", total, preview)
	}
}

func TestHealthCheckDefaultDeadlineAndCapacityRejectionRemainRetryable(t *testing.T) {
	mgr, st, now := newOperationManager(t)
	defer func() { _ = st.Close() }()
	ctx := context.Background()
	current := now
	mgr.now = func() time.Time { return current }
	first, replayed, err := mgr.CreateHealthCheck(ctx, "alice", HealthCheckRequest{
		Node: "gpu-a", Profile: HealthCheckQuick, Reason: "first", IdempotencyKey: "same-default-deadline",
	})
	if err != nil || replayed || first.State != "queued" {
		t.Fatalf("first health check = %#v replayed=%v err=%v", first, replayed, err)
	}
	current = current.Add(time.Minute)
	again, replayed, err := mgr.CreateHealthCheck(ctx, "alice", HealthCheckRequest{
		Node: "gpu-a", Profile: HealthCheckQuick, Reason: "first", IdempotencyKey: "same-default-deadline",
	})
	if err != nil || !replayed || again.ID != first.ID || !again.Deadline.Equal(first.Deadline) {
		t.Fatalf("default-deadline replay = %#v replayed=%v err=%v", again, replayed, err)
	}
	if _, _, err := mgr.CreateHealthCheck(ctx, "alice", HealthCheckRequest{
		Node: "gpu-a", Profile: HealthCheckQuick, Reason: "second", IdempotencyKey: "capacity-retry",
	}); err == nil || !strings.Contains(err.Error(), "concurrency limit") {
		t.Fatalf("capacity rejection = %v", err)
	}
	resource, err := st.GetOperationalResource(ctx, types.ResourceHealthCheckRun, first.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := mgr.CancelHealthCheck(ctx, "alice", first.ID, resource.Version); err != nil {
		t.Fatal(err)
	}
	second, replayed, err := mgr.CreateHealthCheck(ctx, "alice", HealthCheckRequest{
		Node: "gpu-a", Profile: HealthCheckQuick, Reason: "second", IdempotencyKey: "capacity-retry",
	})
	if err != nil || replayed || second.State != "queued" {
		t.Fatalf("capacity retry = %#v replayed=%v err=%v", second, replayed, err)
	}
}

func TestAutonomyDefaultExpiryRetryAndEffectDispatchClaim(t *testing.T) {
	mgr, st, now := newOperationManager(t)
	defer func() { _ = st.Close() }()
	ctx := context.Background()
	current := now
	mgr.now = func() time.Time { return current }
	planRequest := AutonomyPlanRequest{
		Selector: map[string]string{"pool": "a100"}, PolicyRef: "ecc@sha256:policy", ProfileRef: "a100#1",
		AllowedActions: []types.AcceleratorAction{types.AcceleratorActionResetDevice},
		Evidence:       AutonomyEvidenceRequirements{MaxAge: 5 * time.Minute, RequiredSources: []string{"agent"}},
		Guardrails:     AutonomyGuardrails{MaxConcurrentNodes: 1, MaxActionsPerHour: 2, ErrorBudget: 0, NoActiveIncident: true},
		Rollout:        AutonomyRolloutPolicy{CanaryNodes: 1, BakeDuration: time.Minute},
		Approvals:      AutonomyApprovalRequirements{RequiredRoles: []string{"platform", "safety"}, DistinctSubjects: true},
		IdempotencyKey: "default-expiry-plan",
	}
	plan, replayed, err := mgr.CreateAutonomyPlan(ctx, "author", planRequest)
	if err != nil || replayed || !plan.ExpiresAt.Equal(now.Add(defaultAutonomyPlanTTL)) {
		t.Fatalf("default-expiry plan = %#v replayed=%v err=%v", plan, replayed, err)
	}
	current = current.Add(time.Minute)
	again, replayed, err := mgr.CreateAutonomyPlan(ctx, "author", planRequest)
	if err != nil || !replayed || again.ID != plan.ID || !again.ExpiresAt.Equal(plan.ExpiresAt) {
		t.Fatalf("default-expiry replay = %#v replayed=%v err=%v", again, replayed, err)
	}

	// Build a fully approved plan and exercise the effect hand-off directly:
	// a second reconciler must observe the persisted dispatch claim rather than
	// invoking the external executor with the same effect ID.
	simulation, _, err := mgr.CreateSimulation(ctx, "alice", SimulationRequest{
		Node: "gpu-a", DeviceID: "GPU-a", Action: types.AcceleratorActionResetDevice,
		Scope: types.AcceleratorScopePhysicalDevice, Class: types.ClassECCDBE,
		Rationale: "qualify one dispatch", IdempotencyKey: "dispatch-simulation",
	})
	if err != nil {
		t.Fatal(err)
	}
	active, _, err := mgr.CreateAutonomyPlan(ctx, "author", AutonomyPlanRequest{
		Selector: map[string]string{"pool": "a100"}, PolicyRef: "ecc@sha256:policy", ProfileRef: "a100#1",
		AllowedActions: []types.AcceleratorAction{types.AcceleratorActionResetDevice},
		Evidence:       AutonomyEvidenceRequirements{MaxAge: 5 * time.Minute, RequiredSources: []string{"agent"}},
		Guardrails:     AutonomyGuardrails{MaxConcurrentNodes: 1, MaxActionsPerHour: 2, ErrorBudget: 0, NoActiveIncident: true},
		Rollout:        AutonomyRolloutPolicy{CanaryNodes: 1, BakeDuration: time.Minute},
		Approvals:      AutonomyApprovalRequirements{RequiredRoles: []string{"platform", "safety"}, DistinctSubjects: true},
		SimulationID:   simulation.ID, ExpiresAt: current.Add(time.Hour), IdempotencyKey: "dispatch-plan",
	})
	if err != nil {
		t.Fatal(err)
	}
	active, err = mgr.ApproveAutonomyPlan(ctx, "platform", active.ID, "platform", active.ResourceVersion)
	if err != nil {
		t.Fatal(err)
	}
	active, err = mgr.ApproveAutonomyPlan(ctx, "safety", active.ID, "safety", active.ResourceVersion)
	if err != nil {
		t.Fatal(err)
	}
	rollout, err := mgr.GetAutonomyRollout(ctx, active.ID)
	if err != nil {
		t.Fatal(err)
	}
	captured, err := mgr.captureDecision(ctx, "system", operationSnapshot(current, decision.Request{
		Class: decision.ActionAutonomous, AcceleratorAction: types.AcceleratorActionResetDevice,
		Scope: types.AcceleratorScopePhysicalDevice, TargetDeviceID: "GPU-a", ApprovalRequired: true, ApprovalGranted: true,
	}), "dispatch-capture")
	if err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	release := make(chan struct{})
	calls := 0
	mgr.autonomyExecute = func(context.Context, AutonomyEffectRequest) (AutonomyEffectResult, error) {
		calls++
		close(started)
		<-release
		return AutonomyEffectResult{State: "accepted"}, nil
	}
	firstResult := make(chan error, 1)
	go func() {
		_, callErr := mgr.executeAutonomyEffect(ctx, active, rollout, captured)
		firstResult <- callErr
	}()
	<-started
	if _, err := mgr.executeAutonomyEffect(ctx, active, rollout, captured); err == nil || !strings.Contains(err.Error(), "uncertain") {
		t.Fatalf("second dispatch error = %v, want persisted uncertain claim", err)
	}
	close(release)
	if err := <-firstResult; err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("executor calls = %d, want one", calls)
	}
}
