package operations

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/kubeneuron/kubeneuron/internal/decision"
	"github.com/kubeneuron/kubeneuron/internal/store"
	"github.com/kubeneuron/kubeneuron/pkg/types"
)

const defaultAutonomyPlanTTL = 24 * time.Hour

// AutonomyPlanState is deliberately a lifecycle, not a boolean.  An operator
// can see whether a plan has merely been drafted/simulated, is waiting for a
// distinct second approver, is in a bounded canary, or was paused/rolled back.
type AutonomyPlanState string

const (
	AutonomyDraft            AutonomyPlanState = "Draft"
	AutonomySimulated        AutonomyPlanState = "Simulated"
	AutonomyAwaitingApproval AutonomyPlanState = "AwaitingApproval"
	AutonomyCanary           AutonomyPlanState = "Canary"
	AutonomyBaking           AutonomyPlanState = "Baking"
	AutonomyExpanding        AutonomyPlanState = "Expanding"
	AutonomyEnabled          AutonomyPlanState = "Enabled"
	AutonomyPaused           AutonomyPlanState = "Paused"
	AutonomyRolledBack       AutonomyPlanState = "RolledBack"
	AutonomyExpired          AutonomyPlanState = "Expired"
)

func (s AutonomyPlanState) terminal() bool {
	return s == AutonomyRolledBack || s == AutonomyExpired
}

type AutonomyEvidenceRequirements struct {
	MaxAge          time.Duration `json:"max_age"`
	RequiredSources []string      `json:"required_sources"`
}

type AutonomyGuardrails struct {
	MaintenanceWindows []string `json:"maintenance_windows,omitempty"`
	MaxConcurrentNodes int      `json:"max_concurrent_nodes"`
	MaxActionsPerHour  int      `json:"max_actions_per_hour"`
	ErrorBudget        int      `json:"error_budget"`
	NoActiveIncident   bool     `json:"no_active_incident"`
}

type AutonomyRolloutPolicy struct {
	CanaryNodes    int           `json:"canary_nodes"`
	BakeDuration   time.Duration `json:"bake_duration"`
	ExpansionSteps []int         `json:"expansion_steps,omitempty"`
}

type AutonomyApprovalRequirements struct {
	RequiredRoles    []string `json:"required_roles"`
	DistinctSubjects bool     `json:"distinct_subjects"`
}

type AutonomyApproval struct {
	Actor        string    `json:"actor"`
	Role         string    `json:"role"`
	ConfigDigest string    `json:"config_digest"`
	ApprovedAt   time.Time `json:"approved_at"`
}

// GPUAutonomyPlan is the durable REST representation of the Kubernetes CRD.
// Its spec is immutable once created; a material change creates a new plan and
// therefore a new simulation/approval cycle instead of silently retaining an
// approval for a different blast radius.
type GPUAutonomyPlan struct {
	ID              string                    `json:"id"`
	ResourceVersion int                       `json:"resource_version"`
	State           AutonomyPlanState         `json:"state"`
	Selector        map[string]string         `json:"selector"`
	PolicyRef       string                    `json:"policy_ref"`
	ProfileRef      string                    `json:"profile_ref"`
	AllowedActions  []types.AcceleratorAction `json:"allowed_actions"`
	// TargetDeviceID is frozen from the qualifying simulation for device-scoped
	// autonomy. It is not caller-selectable on a later rollout request.
	TargetDeviceID  string                       `json:"target_device_id,omitempty"`
	Evidence        AutonomyEvidenceRequirements `json:"evidence"`
	Guardrails      AutonomyGuardrails           `json:"guardrails"`
	Rollout         AutonomyRolloutPolicy        `json:"rollout"`
	Approvals       AutonomyApprovalRequirements `json:"approval_requirements"`
	ApprovalRecords []AutonomyApproval           `json:"approvals,omitempty"`
	SimulationID    string                       `json:"simulation_id,omitempty"`
	ConfigDigest    string                       `json:"config_digest"`
	RolloutID       string                       `json:"rollout_id,omitempty"`
	ExpiresAt       time.Time                    `json:"expires_at"`
	Actor           string                       `json:"actor"`
	Tenant          string                       `json:"tenant,omitempty"`
	Cluster         string                       `json:"cluster,omitempty"`
	CreatedAt       time.Time                    `json:"created_at"`
	UpdatedAt       time.Time                    `json:"updated_at"`
}

type AutonomyPlanRequest struct {
	Selector       map[string]string            `json:"selector"`
	PolicyRef      string                       `json:"policy_ref"`
	ProfileRef     string                       `json:"profile_ref"`
	AllowedActions []types.AcceleratorAction    `json:"allowed_actions"`
	Evidence       AutonomyEvidenceRequirements `json:"evidence"`
	Guardrails     AutonomyGuardrails           `json:"guardrails"`
	Rollout        AutonomyRolloutPolicy        `json:"rollout"`
	Approvals      AutonomyApprovalRequirements `json:"approval_requirements"`
	SimulationID   string                       `json:"simulation_id,omitempty"`
	ExpiresAt      time.Time                    `json:"expires_at"`
	Tenant         string                       `json:"tenant,omitempty"`
	Cluster        string                       `json:"cluster,omitempty"`
	IdempotencyKey string                       `json:"-"`
}

// AutonomyMutation binds every mutable lifecycle operation to both the
// version the operator reviewed and a request idempotency key. The explicit
// operation field is deliberately not a generic PATCH: pause, rollback,
// approval and simulation attachment have different audit and safety meaning.
type AutonomyMutation struct {
	ExpectedVersion int    `json:"resource_version,omitempty"`
	IdempotencyKey  string `json:"-"`
}

type AutonomyObservation struct {
	Node               string          `json:"node"`
	DecisionSnapshotID string          `json:"decision_snapshot_id"`
	Decision           decision.Result `json:"decision"`
	EffectID           string          `json:"effect_id,omitempty"`
	EffectState        string          `json:"effect_state,omitempty"`
	EffectSummary      string          `json:"effect_summary,omitempty"`
	At                 time.Time       `json:"at"`
}

// AutonomyBakeMeasurement is the post-effect decision captured at the end of
// a canary/bake interval. It binds the observed recovery evidence to the exact
// dispatched effect instead of treating a successful executor hand-off as a
// health signal.
type AutonomyBakeMeasurement struct {
	Node               string          `json:"node"`
	EffectID           string          `json:"effect_id"`
	DecisionSnapshotID string          `json:"decision_snapshot_id"`
	Decision           decision.Result `json:"decision"`
	EvidenceObservedAt time.Time       `json:"evidence_observed_at,omitempty"`
	Summary            string          `json:"summary,omitempty"`
	At                 time.Time       `json:"at"`
}

// AutonomyEffectRequest is the complete, already-admitted effect envelope
// handed to a hardware-qualified executor. It intentionally contains no live
// controller pointer: the executor receives the immutable decision it must
// honor, a bounded deadline, and a deterministic EffectID for idempotency.
type AutonomyEffectRequest struct {
	EffectID           string                       `json:"effect_id"`
	PlanID             string                       `json:"plan_id"`
	RolloutID          string                       `json:"rollout_id"`
	Node               string                       `json:"node"`
	Action             types.AcceleratorAction      `json:"action"`
	Scope              types.AcceleratorTargetScope `json:"scope"`
	TargetDeviceID     string                       `json:"target_device_id,omitempty"`
	DecisionSnapshotID string                       `json:"decision_snapshot_id"`
	Decision           decision.Result              `json:"decision"`
	ConfigDigest       string                       `json:"config_digest"`
	Deadline           time.Time                    `json:"deadline"`
}

// AutonomyEffectResult is a redacted executor outcome. Raw diagnostics must
// be retained behind the executor's own restricted evidence store, while this
// concise result belongs in the general console and audit trail.
type AutonomyEffectResult struct {
	State     string `json:"state"`
	Summary   string `json:"summary,omitempty"`
	Reference string `json:"reference,omitempty"`
}

// AutonomyEffectRecord is persisted before the executor call. It makes an
// effect recoverable across a controller restart and supplies the audit
// explorer a direct plan -> observation -> effect trace.
type AutonomyEffectRecord struct {
	ID                 string                       `json:"id"`
	ResourceVersion    int                          `json:"resource_version"`
	PlanID             string                       `json:"plan_id"`
	RolloutID          string                       `json:"rollout_id"`
	Tenant             string                       `json:"tenant,omitempty"`
	Cluster            string                       `json:"cluster,omitempty"`
	Node               string                       `json:"node"`
	Action             types.AcceleratorAction      `json:"action"`
	Scope              types.AcceleratorTargetScope `json:"scope"`
	TargetDeviceID     string                       `json:"target_device_id,omitempty"`
	DecisionSnapshotID string                       `json:"decision_snapshot_id"`
	Decision           decision.Result              `json:"decision"`
	ConfigDigest       string                       `json:"config_digest"`
	Deadline           time.Time                    `json:"deadline"`
	State              string                       `json:"state"`
	Summary            string                       `json:"summary,omitempty"`
	Reference          string                       `json:"reference,omitempty"`
	// DispatchStartedAt is a durable, single-dispatch claim. A controller that
	// dies after this claim pauses the plan instead of replaying an uncertain
	// hardware call; the executor's EffectID remains a second idempotency fence.
	DispatchStartedAt *time.Time `json:"dispatch_started_at,omitempty"`
	CreatedAt         time.Time  `json:"created_at"`
	UpdatedAt         time.Time  `json:"updated_at"`
}

type AutonomyRollout struct {
	ID               string                    `json:"id"`
	ResourceVersion  int                       `json:"resource_version"`
	PlanID           string                    `json:"plan_id"`
	Tenant           string                    `json:"tenant,omitempty"`
	Cluster          string                    `json:"cluster,omitempty"`
	State            AutonomyPlanState         `json:"state"`
	CanaryNodes      []string                  `json:"canary_nodes,omitempty"`
	ExpandedNodes    []string                  `json:"expanded_nodes,omitempty"`
	Observations     []AutonomyObservation     `json:"observations,omitempty"`
	BakeMeasurements []AutonomyBakeMeasurement `json:"bake_measurements,omitempty"`
	BakeStartedAt    *time.Time                `json:"bake_started_at,omitempty"`
	BakeMeasuredAt   *time.Time                `json:"bake_measured_at,omitempty"`
	BakeEffectIDs    []string                  `json:"bake_effect_ids,omitempty"`
	ActionWindowAt   *time.Time                `json:"action_window_started_at,omitempty"`
	ActionsThisHour  int                       `json:"actions_this_hour"`
	Failures         int                       `json:"failures"`
	ExecutionMode    string                    `json:"execution_mode"`
	Reason           string                    `json:"reason,omitempty"`
	CreatedAt        time.Time                 `json:"created_at"`
	UpdatedAt        time.Time                 `json:"updated_at"`
}

func (m *Manager) CreateAutonomyPlan(ctx context.Context, actor string, request AutonomyPlanRequest) (*GPUAutonomyPlan, bool, error) {
	if err := m.requireResources(); err != nil {
		return nil, false, err
	}
	if strings.TrimSpace(actor) == "" || strings.TrimSpace(request.IdempotencyKey) == "" {
		return nil, false, fmt.Errorf("actor and idempotency key are required")
	}
	// REST callers may omit an expiry, but an autonomy envelope must never be
	// unbounded.  Kubernetes CRs deliberately keep expiresAt required so a
	// Git-reviewed object is self-describing; the API supplies the conservative
	// product default before the request is digested and persisted.
	now := m.now().UTC()
	requestedExpiry := request.ExpiresAt.UTC()
	if requestedExpiry.IsZero() {
		request.ExpiresAt = now.Add(defaultAutonomyPlanTTL)
	} else {
		request.ExpiresAt = requestedExpiry
	}
	normalizeAutonomyRequest(&request)
	if err := validateAutonomyRequest(request, now); err != nil {
		return nil, false, err
	}
	// The resolved default expiry is part of the actual plan envelope and its
	// configuration digest. The *request* digest preserves an omitted expiry
	// as omitted, however, so a normal retry minutes later returns the first
	// durable plan instead of conflicting merely because "now + 24h" moved.
	requestForDigest := request
	requestForDigest.ExpiresAt = requestedExpiry
	requestDigest := digestJSON(autonomyRequestDigestInput(requestForDigest))
	planDigest := digestJSON(autonomyRequestDigestInput(request))
	var attachedSimulation *RemediationSimulation
	if request.SimulationID != "" {
		// Validate the referenced frozen authority before reserving a request
		// key. A typo, blocked simulation, or cross-scope attachment is a
		// review error the caller should be able to correct and retry.
		simulation, getErr := m.getSimulation(ctx, request.SimulationID)
		if getErr != nil {
			return nil, false, fmt.Errorf("autonomy plan simulation: %w", getErr)
		}
		attachedSimulation = simulation
		provisional := &GPUAutonomyPlan{
			AllowedActions: sortedActions(request.AllowedActions), Tenant: request.Tenant, Cluster: request.Cluster,
		}
		if err := validateSimulationForAutonomy(provisional, attachedSimulation); err != nil {
			return nil, false, err
		}
	}
	reservation := &types.OperationalIdempotencyRecord{
		Kind: types.ResourceAutonomyPlan, Actor: actor, Key: scopedIdempotencyKey("create", request.IdempotencyKey),
		RequestDigest: requestDigest, ResourceID: newID("autonomy"),
	}
	claimed, created, err := m.resources.PutOperationalIdempotency(ctx, reservation)
	if err != nil {
		return nil, false, err
	}
	if !created {
		if claimed.RequestDigest != requestDigest {
			return nil, false, ErrIdempotencyConflict
		}
		plan, err := m.getAutonomyPlan(ctx, claimed.ResourceID)
		return plan, true, err
	}
	plan := &GPUAutonomyPlan{
		ID: reservation.ResourceID, ResourceVersion: 1, State: AutonomyDraft, Selector: copySelector(request.Selector),
		PolicyRef: request.PolicyRef, ProfileRef: request.ProfileRef, AllowedActions: sortedActions(request.AllowedActions),
		Evidence: request.Evidence, Guardrails: request.Guardrails, Rollout: request.Rollout, Approvals: request.Approvals,
		ExpiresAt: request.ExpiresAt.UTC(), Actor: actor, Tenant: request.Tenant, Cluster: request.Cluster,
		CreatedAt: now, UpdatedAt: now,
	}
	if attachedSimulation != nil {
		// Recheck against the fully constructed plan even though the provisional
		// validation above has already rejected ordinary bad input. This keeps
		// the construction path itself self-contained if new immutable fields
		// are added to GPUAutonomyPlan later.
		plan.TargetDeviceID = strings.TrimSpace(attachedSimulation.DeviceID)
		if err := validateSimulationForAutonomy(plan, attachedSimulation); err != nil {
			return nil, false, err
		}
		plan.State, plan.SimulationID, plan.ConfigDigest = AutonomySimulated, attachedSimulation.ID, attachedSimulation.Decision.ConfigDigest
	} else {
		// The digest still binds the draft's declared envelope.  It is replaced
		// by the frozen simulation digest before any approval is accepted.
		plan.ConfigDigest = planDigest
	}
	payload, err := marshalPayload(plan)
	if err != nil {
		return nil, false, err
	}
	if err := m.resources.CreateOperationalResource(ctx, &types.OperationalResource{
		Kind: types.ResourceAutonomyPlan, ID: plan.ID, State: string(plan.State), Actor: actor,
		Tenant: plan.Tenant, Cluster: plan.Cluster, ConfigDigest: plan.ConfigDigest, Payload: payload,
		CreatedAt: now, UpdatedAt: now, ExpiresAt: &plan.ExpiresAt, Version: plan.ResourceVersion,
	}); err != nil {
		return nil, false, err
	}
	if err := m.audit(ctx, types.ResourceAutonomyPlan, plan.ID, actor, "create", request.IdempotencyKey, plan.SimulationID, nil, string(plan.State)); err != nil {
		return nil, false, err
	}
	return plan, false, nil
}

// AttachSimulation moves a draft into the explicit simulation gate.  A plan
// cannot skip directly to approval: the simulation's immutable config digest
// becomes the approval binding.
func (m *Manager) AttachSimulation(ctx context.Context, actor, planID, simulationID string, expectedVersion int) (*GPUAutonomyPlan, error) {
	if err := m.requireResources(); err != nil {
		return nil, err
	}
	resource, plan, err := m.loadAutonomyPlan(ctx, planID)
	if err != nil {
		return nil, err
	}
	if expectedVersion > 0 && expectedVersion != resource.Version {
		return nil, store.ErrOperationalConflict
	}
	if plan.State != AutonomyDraft {
		return nil, fmt.Errorf("%w: plan %q is %s", ErrInvalidState, planID, plan.State)
	}
	simulation, err := m.getSimulation(ctx, simulationID)
	if err != nil {
		return nil, err
	}
	if err := validateSimulationForAutonomy(plan, simulation); err != nil {
		return nil, err
	}
	plan.TargetDeviceID = strings.TrimSpace(simulation.DeviceID)
	plan.State, plan.SimulationID, plan.ConfigDigest, plan.UpdatedAt = AutonomySimulated, simulation.ID, simulation.Decision.ConfigDigest, m.now().UTC()
	if err := m.saveAutonomyPlan(ctx, resource, plan, actor); err != nil {
		return nil, err
	}
	if err := m.audit(ctx, types.ResourceAutonomyPlan, plan.ID, actor, "attach-simulation", "", simulation.ID, nil, "simulated"); err != nil {
		return nil, err
	}
	return plan, nil
}

// AttachSimulationRequest is the idempotent API/CLI variant of
// AttachSimulation. The plain method remains useful to the CRD reconciler,
// which already has Kubernetes resource-version retry semantics.
func (m *Manager) AttachSimulationRequest(ctx context.Context, actor, planID, simulationID string, mutation AutonomyMutation) (*GPUAutonomyPlan, bool, error) {
	if replay, plan, err := m.reserveAutonomyMutation(ctx, actor, planID, "attach-simulation", mutation, map[string]string{"simulation_id": simulationID}); err != nil || replay {
		return plan, replay, err
	}
	plan, err := m.AttachSimulation(ctx, actor, planID, simulationID, mutation.ExpectedVersion)
	return plan, false, err
}

// ApproveAutonomyPlan records one role-bound approval.  A subject may never
// satisfy two roles when distinctSubjects is set, and every approval carries
// the exact immutable simulation/config digest it reviewed.
func (m *Manager) ApproveAutonomyPlan(ctx context.Context, actor, planID, role string, expectedVersion int) (*GPUAutonomyPlan, error) {
	if err := m.requireResources(); err != nil {
		return nil, err
	}
	resource, plan, err := m.loadAutonomyPlan(ctx, planID)
	if err != nil {
		return nil, err
	}
	if expectedVersion > 0 && expectedVersion != resource.Version {
		return nil, store.ErrOperationalConflict
	}
	if plan.State != AutonomySimulated && plan.State != AutonomyAwaitingApproval {
		return nil, fmt.Errorf("%w: plan %q is %s", ErrInvalidState, planID, plan.State)
	}
	if !containsString(plan.Approvals.RequiredRoles, role) {
		return nil, fmt.Errorf("approval role %q is not required by plan %q", role, planID)
	}
	for _, approval := range plan.ApprovalRecords {
		if approval.Role == role {
			if approval.Actor == actor && approval.ConfigDigest == plan.ConfigDigest {
				return plan, nil // idempotent same actor/role click.
			}
			return nil, fmt.Errorf("%w: role %q is already approved", ErrInvalidState, role)
		}
		if plan.Approvals.DistinctSubjects && approval.Actor == actor {
			return nil, fmt.Errorf("%w: actor %q may not satisfy multiple approval roles", ErrInvalidState, actor)
		}
	}
	plan.ApprovalRecords = append(plan.ApprovalRecords, AutonomyApproval{Actor: actor, Role: role, ConfigDigest: plan.ConfigDigest, ApprovedAt: m.now().UTC()})
	plan.State, plan.UpdatedAt = AutonomyAwaitingApproval, m.now().UTC()
	if allRolesApproved(plan) {
		plan.State = AutonomyCanary
		rollout, err := m.createAutonomyRollout(ctx, actor, plan)
		if err != nil {
			return nil, err
		}
		plan.RolloutID = rollout.ID
	}
	if err := m.saveAutonomyPlan(ctx, resource, plan, actor); err != nil {
		return nil, err
	}
	if err := m.audit(ctx, types.ResourceAutonomyPlan, plan.ID, actor, "approve", "", plan.SimulationID, map[string]string{"role": role}, string(plan.State)); err != nil {
		return nil, err
	}
	return plan, nil
}

// ApproveAutonomyPlanRequest is the idempotent public mutation. Replaying a
// successful click returns the plan instead of recording a second approval or
// advancing a rollout twice.
func (m *Manager) ApproveAutonomyPlanRequest(ctx context.Context, actor, planID, role string, mutation AutonomyMutation) (*GPUAutonomyPlan, bool, error) {
	if replay, plan, err := m.reserveAutonomyMutation(ctx, actor, planID, "approve", mutation, map[string]string{"role": role}); err != nil || replay {
		return plan, replay, err
	}
	plan, err := m.ApproveAutonomyPlan(ctx, actor, planID, role, mutation.ExpectedVersion)
	return plan, false, err
}

func (m *Manager) PauseAutonomyPlan(ctx context.Context, actor, planID, reason string, expectedVersion int) (*GPUAutonomyPlan, error) {
	return m.transitionAutonomyPlan(ctx, actor, planID, AutonomyPaused, reason, expectedVersion)
}

func (m *Manager) PauseAutonomyPlanRequest(ctx context.Context, actor, planID, reason string, mutation AutonomyMutation) (*GPUAutonomyPlan, bool, error) {
	if replay, plan, err := m.reserveAutonomyMutation(ctx, actor, planID, "pause", mutation, map[string]string{"reason": reason}); err != nil || replay {
		return plan, replay, err
	}
	plan, err := m.PauseAutonomyPlan(ctx, actor, planID, reason, mutation.ExpectedVersion)
	return plan, false, err
}

func (m *Manager) RollbackAutonomyPlan(ctx context.Context, actor, planID, reason string, expectedVersion int) (*GPUAutonomyPlan, error) {
	return m.transitionAutonomyPlan(ctx, actor, planID, AutonomyRolledBack, reason, expectedVersion)
}

func (m *Manager) RollbackAutonomyPlanRequest(ctx context.Context, actor, planID, reason string, mutation AutonomyMutation) (*GPUAutonomyPlan, bool, error) {
	if replay, plan, err := m.reserveAutonomyMutation(ctx, actor, planID, "rollback", mutation, map[string]string{"reason": reason}); err != nil || replay {
		return plan, replay, err
	}
	plan, err := m.RollbackAutonomyPlan(ctx, actor, planID, reason, mutation.ExpectedVersion)
	return plan, false, err
}

// ResumeAutonomyPlan is intentionally narrow: a paused plan returns to a
// fresh canary, never directly to Enabled. This repeats live evidence capture
// before any future effect and preserves the original distinct approvals only
// while their bound config digest is unchanged and the plan has not expired.
func (m *Manager) ResumeAutonomyPlan(ctx context.Context, actor, planID, reason string, expectedVersion int) (*GPUAutonomyPlan, error) {
	resource, plan, err := m.loadAutonomyPlan(ctx, planID)
	if err != nil {
		return nil, err
	}
	if expectedVersion > 0 && expectedVersion != resource.Version {
		return nil, store.ErrOperationalConflict
	}
	if plan.State != AutonomyPaused {
		return nil, fmt.Errorf("%w: plan %q is %s", ErrInvalidState, planID, plan.State)
	}
	if !plan.ExpiresAt.After(m.now()) {
		return nil, fmt.Errorf("%w: plan %q has expired", ErrInvalidState, planID)
	}
	if !allRolesApproved(plan) {
		return nil, fmt.Errorf("%w: plan %q no longer has all required approvals", ErrInvalidState, planID)
	}
	if strings.TrimSpace(reason) == "" {
		return nil, fmt.Errorf("resume reason is required")
	}
	plan.State, plan.UpdatedAt = AutonomyCanary, m.now().UTC()
	if err := m.saveAutonomyPlan(ctx, resource, plan, actor); err != nil {
		return nil, err
	}
	if plan.RolloutID != "" {
		if err := m.updateAutonomyRollout(ctx, plan.RolloutID, AutonomyCanary, "resumed: "+reason, nil); err != nil {
			return nil, err
		}
	}
	if err := m.audit(ctx, types.ResourceAutonomyPlan, plan.ID, actor, "resume", "", plan.SimulationID, map[string]string{"reason": reason}, string(plan.State)); err != nil {
		return nil, err
	}
	return plan, nil
}

func (m *Manager) ResumeAutonomyPlanRequest(ctx context.Context, actor, planID, reason string, mutation AutonomyMutation) (*GPUAutonomyPlan, bool, error) {
	if replay, plan, err := m.reserveAutonomyMutation(ctx, actor, planID, "resume", mutation, map[string]string{"reason": reason}); err != nil || replay {
		return plan, replay, err
	}
	plan, err := m.ResumeAutonomyPlan(ctx, actor, planID, reason, mutation.ExpectedVersion)
	return plan, false, err
}

func (m *Manager) transitionAutonomyPlan(ctx context.Context, actor, planID string, target AutonomyPlanState, reason string, expectedVersion int) (*GPUAutonomyPlan, error) {
	if err := m.requireResources(); err != nil {
		return nil, err
	}
	resource, plan, err := m.loadAutonomyPlan(ctx, planID)
	if err != nil {
		return nil, err
	}
	if expectedVersion > 0 && expectedVersion != resource.Version {
		return nil, store.ErrOperationalConflict
	}
	if plan.State.terminal() {
		return nil, fmt.Errorf("%w: plan %q is already terminal (%s)", ErrInvalidState, planID, plan.State)
	}
	if strings.TrimSpace(reason) == "" {
		return nil, fmt.Errorf("%s reason is required", strings.ToLower(string(target)))
	}
	plan.State, plan.UpdatedAt = target, m.now().UTC()
	if err := m.saveAutonomyPlan(ctx, resource, plan, actor); err != nil {
		return nil, err
	}
	if plan.RolloutID != "" {
		_ = m.updateAutonomyRollout(ctx, plan.RolloutID, target, reason, nil)
	}
	if err := m.audit(ctx, types.ResourceAutonomyPlan, plan.ID, actor, strings.ToLower(string(target)), "", plan.SimulationID, map[string]string{"reason": reason}, string(target)); err != nil {
		return nil, err
	}
	return plan, nil
}

func (m *Manager) GetAutonomyPlan(ctx context.Context, id string) (*GPUAutonomyPlan, error) {
	if err := m.requireResources(); err != nil {
		return nil, err
	}
	_, plan, err := m.loadAutonomyPlan(ctx, id)
	return plan, err
}

// GetAutonomyRollout returns the durable canary/bake observations for one
// plan. It is deliberately plan-addressed rather than exposing arbitrary
// rollout IDs as a guessing surface in the console API.
func (m *Manager) GetAutonomyRollout(ctx context.Context, planID string) (*AutonomyRollout, error) {
	if err := m.requireResources(); err != nil {
		return nil, err
	}
	plan, err := m.getAutonomyPlan(ctx, planID)
	if err != nil {
		return nil, err
	}
	return m.getAutonomyRollout(ctx, plan.RolloutID)
}

func (m *Manager) ListAutonomyPlans(ctx context.Context, limit int) ([]*GPUAutonomyPlan, error) {
	return m.ListAutonomyPlansWithOptions(ctx, OperationalListOptions{Limit: limit, IncludeExpired: true})
}

// ListAutonomyPlansWithOptions is the stable, tenant/cluster-filtered listing
// used by the API and console. The legacy limit-only method remains for the
// reconcile loop and existing integrations.
func (m *Manager) ListAutonomyPlansWithOptions(ctx context.Context, options OperationalListOptions) ([]*GPUAutonomyPlan, error) {
	resources, err := m.listOperationalResources(ctx, types.ResourceAutonomyPlan, options)
	if err != nil {
		return nil, err
	}
	out := make([]*GPUAutonomyPlan, 0, len(resources))
	for _, resource := range resources {
		var plan GPUAutonomyPlan
		if err := json.Unmarshal(resource.Payload, &plan); err != nil {
			return nil, fmt.Errorf("autonomy plan %q is corrupt: %w", resource.ID, err)
		}
		plan.ResourceVersion = resource.Version
		out = append(out, &plan)
	}
	return out, nil
}

// ReconcileAutonomy advances the plan state machine and, when a deployment
// supplied a hardware-qualified executor, invokes it only through the
// persisted effect hand-off below. A stock controller has no such executor,
// so the identical canary path remains explicitly simulation-only rather than
// inventing an unreviewed actuator path around incident and agent safety.
func (m *Manager) ReconcileAutonomy(ctx context.Context) error {
	if err := m.requireSnapshots(); err != nil {
		return err
	}
	// Resource creation order is stable even as a plan changes state. Walk the
	// complete cursor stream so plan 501 cannot miss an expiry, failed bake, or
	// emergency evidence hold merely because the first page was full.
	options := OperationalListOptions{Limit: 500, IncludeExpired: true}
	for {
		plans, err := m.ListAutonomyPlansWithOptions(ctx, options)
		if err != nil {
			return err
		}
		for _, plan := range plans {
			if plan.State.terminal() {
				continue
			}
			if !plan.ExpiresAt.After(m.now()) {
				if _, err := m.transitionAutonomyPlan(ctx, "system", plan.ID, AutonomyExpired, "plan expiry elapsed", plan.ResourceVersion); err != nil && !errors.Is(err, store.ErrOperationalConflict) {
					return err
				}
				continue
			}
			if plan.State == AutonomyPaused || plan.State == AutonomyDraft || plan.State == AutonomySimulated || plan.State == AutonomyAwaitingApproval {
				continue
			}
			switch plan.State {
			case AutonomyCanary:
				if err := m.admitAutonomyCanary(ctx, plan); err != nil {
					target, reason := AutonomyPaused, err.Error()
					if rollout, rolloutErr := m.getAutonomyRollout(ctx, plan.RolloutID); rolloutErr == nil && rollout.Failures > plan.Guardrails.ErrorBudget {
						target, reason = AutonomyRolledBack, "canary exceeded error budget: "+err.Error()
					}
					if _, transitionErr := m.transitionAutonomyPlan(ctx, "system", plan.ID, target, reason, plan.ResourceVersion); transitionErr != nil && !errors.Is(transitionErr, store.ErrOperationalConflict) {
						return transitionErr
					}
				}
			case AutonomyBaking:
				rollout, err := m.getAutonomyRollout(ctx, plan.RolloutID)
				if err != nil {
					return err
				}
				if rollout.Failures > plan.Guardrails.ErrorBudget {
					if _, err := m.transitionAutonomyPlan(ctx, "system", plan.ID, AutonomyRolledBack, "canary exceeded error budget", plan.ResourceVersion); err != nil && !errors.Is(err, store.ErrOperationalConflict) {
						return err
					}
					continue
				}
				if rollout.BakeStartedAt != nil && !m.now().Before(rollout.BakeStartedAt.Add(plan.Rollout.BakeDuration)) {
					if rollout.BakeMeasuredAt == nil {
						measurements, failures, measureErr := m.measureAutonomyBake(ctx, plan, rollout)
						if measureErr != nil {
							if _, transitionErr := m.transitionAutonomyPlan(ctx, "system", plan.ID, AutonomyPaused, "bake measurement unavailable: "+measureErr.Error(), plan.ResourceVersion); transitionErr != nil && !errors.Is(transitionErr, store.ErrOperationalConflict) {
								return transitionErr
							}
							continue
						}
						measuredAt := m.now().UTC()
						rollout.BakeMeasurements = append(rollout.BakeMeasurements, measurements...)
						rollout.Failures += failures
						rollout.BakeMeasuredAt, rollout.UpdatedAt = &measuredAt, measuredAt
						if err := m.saveAutonomyRollout(ctx, rollout); err != nil {
							return err
						}
					}
					if rollout.Failures > plan.Guardrails.ErrorBudget {
						if _, err := m.transitionAutonomyPlan(ctx, "system", plan.ID, AutonomyRolledBack, "bake measurements exceeded error budget", plan.ResourceVersion); err != nil && !errors.Is(err, store.ErrOperationalConflict) {
							return err
						}
						continue
					}
					target := AutonomyEnabled
					if _, pending := nextAutonomyExpansionTarget(plan, rollout); pending {
						target = AutonomyExpanding
					}
					if err := m.moveAutonomyPlan(ctx, plan, target, "bake completed"); err != nil && !errors.Is(err, store.ErrOperationalConflict) {
						return err
					}
				}
			case AutonomyExpanding:
				if err := m.admitAutonomyExpansion(ctx, plan); err != nil && !errors.Is(err, store.ErrOperationalConflict) {
					return err
				}
			}
		}
		if len(plans) < options.Limit {
			return nil
		}
		last := plans[len(plans)-1]
		if !options.AfterCreatedAt.IsZero() && (last.CreatedAt.Before(options.AfterCreatedAt) || (last.CreatedAt.Equal(options.AfterCreatedAt) && last.ID <= options.AfterID)) {
			return fmt.Errorf("%w: autonomy reconciliation cursor did not advance", ErrIncompleteInventory)
		}
		options.AfterCreatedAt, options.AfterID = last.CreatedAt, last.ID
	}
}

func (m *Manager) admitAutonomyCanary(ctx context.Context, plan *GPUAutonomyPlan) error {
	if m.listNodes == nil {
		return fmt.Errorf("node inventory lister is unavailable")
	}
	nodes, err := m.listNodes(ctx)
	if err != nil {
		return err
	}
	selected := make([]string, 0, len(nodes))
	for _, node := range nodes {
		if node == nil {
			continue
		}
		if strings.TrimSpace(node.Name) == "" {
			return fmt.Errorf("autonomy inventory contains a node without a name")
		}
		if labelsMatch(plan.Selector, node.Labels) && operationalNodeScopeMatches(node, plan.Tenant, plan.Cluster) {
			selected = append(selected, node.Name)
		}
	}
	sort.Strings(selected)
	if len(selected) < plan.Rollout.CanaryNodes {
		return fmt.Errorf("canary requires %d eligible selector nodes, found %d", plan.Rollout.CanaryNodes, len(selected))
	}
	selected = selected[:plan.Rollout.CanaryNodes]
	rollout, err := m.admitAutonomyTargets(ctx, plan, selected)
	if err != nil {
		return err
	}
	now := m.now().UTC()
	rollout.CanaryNodes = append([]string(nil), selected...)
	rollout.BakeEffectIDs = latestAutonomyEffectIDs(rollout.Observations, len(selected))
	rollout.State, rollout.BakeStartedAt, rollout.BakeMeasuredAt, rollout.UpdatedAt = AutonomyBaking, &now, nil, now
	if err := m.saveAutonomyRollout(ctx, rollout); err != nil {
		return err
	}
	return m.moveAutonomyPlan(ctx, plan, AutonomyBaking, "all canary gates satisfied")
}

// admitAutonomyExpansion takes exactly one bounded expansion batch.  Each
// batch is followed by the declared bake interval, so a broad selector never
// turns a completed initial canary into an immediate fleet-wide switch.
func (m *Manager) admitAutonomyExpansion(ctx context.Context, plan *GPUAutonomyPlan) error {
	if m.listNodes == nil {
		return fmt.Errorf("node inventory lister is unavailable")
	}
	rollout, err := m.getAutonomyRollout(ctx, plan.RolloutID)
	if err != nil {
		return err
	}
	target, pending := nextAutonomyExpansionTarget(plan, rollout)
	if !pending {
		return m.moveAutonomyPlan(ctx, plan, AutonomyEnabled, "all bounded expansion steps completed")
	}
	nodes, err := m.listNodes(ctx)
	if err != nil {
		return err
	}
	used := make(map[string]struct{}, len(rollout.CanaryNodes)+len(rollout.ExpandedNodes))
	for _, node := range rollout.CanaryNodes {
		used[node] = struct{}{}
	}
	for _, node := range rollout.ExpandedNodes {
		used[node] = struct{}{}
	}
	available := make([]string, 0, len(nodes))
	for _, node := range nodes {
		if node == nil {
			continue
		}
		if strings.TrimSpace(node.Name) == "" {
			return fmt.Errorf("autonomy inventory contains a node without a name")
		}
		if labelsMatch(plan.Selector, node.Labels) && operationalNodeScopeMatches(node, plan.Tenant, plan.Cluster) {
			if _, alreadyUsed := used[node.Name]; !alreadyUsed {
				available = append(available, node.Name)
			}
		}
	}
	sort.Strings(available)
	need := target - len(used)
	if need <= 0 {
		return m.moveAutonomyPlan(ctx, plan, AutonomyBaking, "expansion target already recorded; starting bake")
	}
	if len(available) < need {
		return fmt.Errorf("expansion requires %d additional selector nodes, found %d", need, len(available))
	}
	// Effects are admitted serially, but this cap also bounds the number whose
	// fresh evidence can be consumed in one reconciliation cycle. The next
	// bake/expansion cycle is required before a larger target continues.
	if need > plan.Guardrails.MaxConcurrentNodes {
		need = plan.Guardrails.MaxConcurrentNodes
	}
	selected := available[:need]
	rollout, err = m.admitAutonomyTargets(ctx, plan, selected)
	if err != nil {
		return err
	}
	now := m.now().UTC()
	rollout.ExpandedNodes = append(rollout.ExpandedNodes, selected...)
	rollout.BakeEffectIDs = latestAutonomyEffectIDs(rollout.Observations, len(selected))
	rollout.State, rollout.BakeStartedAt, rollout.BakeMeasuredAt, rollout.UpdatedAt = AutonomyBaking, &now, nil, now
	if err := m.saveAutonomyRollout(ctx, rollout); err != nil {
		return err
	}
	return m.moveAutonomyPlan(ctx, plan, AutonomyBaking, fmt.Sprintf("expanded to %d selected nodes; starting bake", len(rollout.CanaryNodes)+len(rollout.ExpandedNodes)))
}

// admitAutonomyTargets re-evaluates and records every target immediately
// before an effect. It is shared by canary and expansion paths specifically to
// prevent a safer first batch from drifting into a weaker second-batch gate.
func (m *Manager) admitAutonomyTargets(ctx context.Context, plan *GPUAutonomyPlan, selected []string) (*AutonomyRollout, error) {
	rollout, err := m.getAutonomyRollout(ctx, plan.RolloutID)
	if err != nil {
		return nil, err
	}
	now := m.now().UTC()
	if rollout.ActionWindowAt == nil || !now.Before(rollout.ActionWindowAt.Add(time.Hour)) {
		rollout.ActionsThisHour = 0
		rollout.ActionWindowAt = &now
	}
	observations := make([]AutonomyObservation, 0, len(selected))
	targetDeviceID := strings.TrimSpace(plan.TargetDeviceID)
	if scopeForAutonomyAction(plan.AllowedActions[0]) == types.AcceleratorScopePhysicalDevice && targetDeviceID == "" {
		return nil, fmt.Errorf("%w: physical-device autonomy plan %q has no frozen target device", ErrInvalidState, plan.ID)
	}
	for _, node := range selected {
		// A plan can be paused, rolled back, expired, or revised while a
		// reconciliation batch is in progress. Re-read its durable envelope
		// immediately before capturing evidence so a stale list result cannot
		// authorize a later target. The effect hand-off repeats this check at
		// the final executor boundary.
		if err := m.ensureAutonomyEffectEnvelope(ctx, plan); err != nil {
			return nil, err
		}
		snapshot, err := m.buildAutonomySnapshot(ctx, plan, node, targetDeviceID, rollout.ActionsThisHour >= plan.Guardrails.MaxActionsPerHour)
		if err != nil {
			return nil, err
		}
		captured, err := m.captureDecision(ctx, "system", snapshot, "")
		if err != nil {
			return nil, err
		}
		observation := AutonomyObservation{Node: node, DecisionSnapshotID: captured.ID, Decision: captured.Decision, At: m.now().UTC()}
		if !captured.Decision.Permitted() {
			observations = append(observations, observation)
			rollout.Observations = append(rollout.Observations, observations...)
			rollout.UpdatedAt = now
			_ = m.saveAutonomyRollout(ctx, rollout)
			return nil, fmt.Errorf("autonomy node %s is %s", node, captured.Decision.HumanSummary)
		}
		if m.autonomyExecute == nil {
			// An absent executor is an intentional, visible simulation-only
			// rollout mode, never an implicit success claim about hardware.
			observation.EffectState = "simulation-only"
			observation.EffectSummary = "no hardware-qualified autonomy executor is configured"
			observations = append(observations, observation)
			continue
		}
		// Spend the hourly effect slot before handing control to external
		// hardware. Counting a failed/uncertain dispatch is conservative, and
		// persisting it here prevents two concurrent reconciliation attempts
		// from both seeing the final slot as free.
		if rollout.ActionsThisHour >= plan.Guardrails.MaxActionsPerHour {
			return nil, fmt.Errorf("autonomy action budget is exhausted")
		}
		rollout.ActionsThisHour++
		rollout.UpdatedAt = m.now().UTC()
		if err := m.saveAutonomyRollout(ctx, rollout); err != nil {
			return nil, err
		}
		effect, executeErr := m.executeAutonomyEffect(ctx, plan, rollout, captured)
		if effect == nil {
			return nil, fmt.Errorf("autonomy effect for node %s returned no record", node)
		}
		observation.EffectID, observation.EffectState, observation.EffectSummary = effect.ID, effect.State, effect.Summary
		observations = append(observations, observation)
		if executeErr != nil || effect.State == "failed" {
			rollout.Failures++
			rollout.Observations = append(rollout.Observations, observations...)
			rollout.UpdatedAt = now
			_ = m.saveAutonomyRollout(ctx, rollout)
			if executeErr != nil {
				return nil, fmt.Errorf("autonomy effect for node %s: %w", node, executeErr)
			}
			return nil, fmt.Errorf("autonomy effect for node %s failed: %s", node, effect.Summary)
		}
	}
	rollout.Observations = append(rollout.Observations, observations...)
	rollout.UpdatedAt = now
	return rollout, nil
}

// autonomyDecisionRequest builds the single evaluator request shape for an
// autonomous effect. Named maintenance windows turn into an explicit
// "must be inside this approved window" gate; the resolver is mandatory when
// a plan declares one so a missing controller integration cannot be read as an
// open maintenance window.
func (m *Manager) autonomyDecisionRequest(ctx context.Context, plan *GPUAutonomyPlan, node, targetDeviceID string) (decision.Request, bool, error) {
	request := decision.Request{
		Class: decision.ActionAutonomous, AcceleratorAction: plan.AllowedActions[0],
		Scope: scopeForAutonomyAction(plan.AllowedActions[0]), TargetDeviceID: targetDeviceID,
		ApprovalRequired: true, ApprovalGranted: true,
	}
	if len(plan.Guardrails.MaintenanceWindows) == 0 {
		return request, false, nil
	}
	if m.autonomyMaintenanceWindowActive == nil {
		return decision.Request{}, false, fmt.Errorf("%w: autonomy maintenance-window resolver is unavailable", ErrUnavailable)
	}
	active, err := m.autonomyMaintenanceWindowActive(ctx, node, append([]string(nil), plan.Guardrails.MaintenanceWindows...))
	if err != nil {
		return decision.Request{}, false, fmt.Errorf("resolve autonomy maintenance window for node %s: %w", node, err)
	}
	request.MaintenanceRequired = true
	request.AllowDuringMaintenance = true
	return request, active, nil
}

// buildAutonomySnapshot applies the plan's immutable envelope to a fresh
// controller-owned snapshot. Both the pre-effect admission and the bake
// measurement call this one helper so their evaluator inputs cannot drift.
func (m *Manager) buildAutonomySnapshot(ctx context.Context, plan *GPUAutonomyPlan, node, targetDeviceID string, budgetExhausted bool) (decision.Snapshot, error) {
	request, maintenanceActive, err := m.autonomyDecisionRequest(ctx, plan, node, targetDeviceID)
	if err != nil {
		return decision.Snapshot{}, err
	}
	snapshot, err := m.buildSnapshot(ctx, node, request)
	if err != nil {
		return decision.Snapshot{}, fmt.Errorf("capture autonomy node %s: %w", node, err)
	}
	// The controller snapshot normally reports any active maintenance window.
	// A plan that names specific windows is stricter: only a referenced,
	// currently matching window can satisfy its explicit maintenance gate.
	if len(plan.Guardrails.MaintenanceWindows) > 0 {
		snapshot.MaintenanceActive = maintenanceActive
	}
	snapshot.Request = request
	if err := requireSnapshotScope(snapshot, plan.Tenant, plan.Cluster); err != nil {
		return decision.Snapshot{}, err
	}
	// Keep the live digest intact. RequiredConfigDigest makes the shared
	// evaluator fail closed if the current controller configuration has changed
	// since the attached simulation and approvals were captured.
	snapshot.RequiredConfigDigest = plan.ConfigDigest
	snapshot.EvidenceMaxAge = plan.Evidence.MaxAge
	snapshot.RequiredEvidenceSources = append([]string(nil), plan.Evidence.RequiredSources...)
	snapshot.Limits.MaxConcurrentNodes = plan.Guardrails.MaxConcurrentNodes
	snapshot.Limits.MaxActionsPerHour = plan.Guardrails.MaxActionsPerHour
	snapshot.AutonomyBudgetExhausted = budgetExhausted
	return snapshot, nil
}

// measureAutonomyBake captures post-effect health evidence for every hardware
// effect admitted in the completed bake interval. A successful executor
// response proves only hand-off, not recovery: the report must be observed
// strictly after the effect's terminal update and pass the same evaluator.
// Simulation-only rollouts retain their explicit no-hardware semantics and
// therefore have no post-effect measurement to fabricate.
func (m *Manager) measureAutonomyBake(ctx context.Context, plan *GPUAutonomyPlan, rollout *AutonomyRollout) ([]AutonomyBakeMeasurement, int, error) {
	if rollout.ExecutionMode != "hardware-qualified" {
		return nil, 0, nil
	}
	if len(rollout.BakeEffectIDs) == 0 {
		return nil, 0, fmt.Errorf("hardware-qualified rollout has no effects in the current bake interval")
	}
	observations := make(map[string]AutonomyObservation, len(rollout.Observations))
	for _, observation := range rollout.Observations {
		if id := strings.TrimSpace(observation.EffectID); id != "" {
			observations[id] = observation
		}
	}
	measurements := make([]AutonomyBakeMeasurement, 0, len(rollout.BakeEffectIDs))
	failures := 0
	for _, effectID := range rollout.BakeEffectIDs {
		observation, ok := observations[effectID]
		if !ok {
			return nil, 0, fmt.Errorf("hardware-qualified rollout is missing observation for effect %q", effectID)
		}
		_, effect, err := m.loadAutonomyEffect(ctx, effectID)
		if err != nil {
			return nil, 0, fmt.Errorf("load autonomy effect %q: %w", observation.EffectID, err)
		}
		measurement := AutonomyBakeMeasurement{Node: observation.Node, EffectID: effect.ID, At: m.now().UTC()}
		switch effect.State {
		case "accepted", "completed":
			// Continue with a fresh evidence capture below.
		default:
			measurement.Decision = effect.Decision
			measurement.Summary = "effect is not in a successful terminal state: " + effect.State
			measurements = append(measurements, measurement)
			failures++
			continue
		}
		snapshot, err := m.buildAutonomySnapshot(ctx, plan, observation.Node, effect.TargetDeviceID, false)
		if err != nil {
			return nil, 0, err
		}
		captured, err := m.captureDecision(ctx, "system", snapshot, "")
		if err != nil {
			return nil, 0, fmt.Errorf("capture bake measurement for node %s: %w", observation.Node, err)
		}
		measurement.DecisionSnapshotID, measurement.Decision = captured.ID, captured.Decision
		if snapshot.Report != nil {
			measurement.EvidenceObservedAt = snapshot.Report.ObservedAt.UTC()
		}
		if !captured.Decision.Permitted() {
			measurement.Summary = captured.Decision.HumanSummary
			failures++
		} else if snapshot.Report == nil || !snapshot.Report.ObservedAt.After(effect.UpdatedAt) {
			measurement.Summary = "post-effect accelerator evidence was not observed after effect completion"
			failures++
		} else {
			measurement.Summary = "post-effect evidence passed the shared evaluator"
		}
		measurements = append(measurements, measurement)
	}
	return measurements, failures, nil
}

func nextAutonomyExpansionTarget(plan *GPUAutonomyPlan, rollout *AutonomyRollout) (int, bool) {
	used := make(map[string]struct{}, len(rollout.CanaryNodes)+len(rollout.ExpandedNodes))
	for _, node := range rollout.CanaryNodes {
		used[node] = struct{}{}
	}
	for _, node := range rollout.ExpandedNodes {
		used[node] = struct{}{}
	}
	for _, target := range plan.Rollout.ExpansionSteps {
		if len(used) < target {
			return target, true
		}
	}
	return 0, false
}

func latestAutonomyEffectIDs(observations []AutonomyObservation, count int) []string {
	if count <= 0 || len(observations) == 0 {
		return nil
	}
	start := len(observations) - count
	if start < 0 {
		start = 0
	}
	ids := make([]string, 0, len(observations)-start)
	for _, observation := range observations[start:] {
		if id := strings.TrimSpace(observation.EffectID); id != "" {
			ids = append(ids, id)
		}
	}
	return ids
}

func autonomyEffectID(planID, rolloutID, node string, action types.AcceleratorAction, targetDeviceID, configDigest string) string {
	sum := sha256.Sum256([]byte(strings.Join([]string{planID, rolloutID, node, string(action), targetDeviceID, configDigest}, "\x00")))
	return "autonomy-effect-" + hex.EncodeToString(sum[:12])
}

func (m *Manager) executeAutonomyEffect(ctx context.Context, plan *GPUAutonomyPlan, rollout *AutonomyRollout, captured *DecisionSnapshot) (*AutonomyEffectRecord, error) {
	targetDeviceID := strings.TrimSpace(plan.TargetDeviceID)
	if strings.TrimSpace(captured.Snapshot.Request.TargetDeviceID) != targetDeviceID {
		return nil, fmt.Errorf("%w: autonomy effect target device no longer matches its frozen plan", ErrInvalidState)
	}
	id := autonomyEffectID(plan.ID, rollout.ID, captured.Snapshot.Node.Name, plan.AllowedActions[0], targetDeviceID, plan.ConfigDigest)
	resource, effect, err := m.loadAutonomyEffect(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		now := m.now().UTC()
		effect = &AutonomyEffectRecord{
			ID: id, ResourceVersion: 1, PlanID: plan.ID, RolloutID: rollout.ID, Node: captured.Snapshot.Node.Name,
			Tenant: plan.Tenant, Cluster: plan.Cluster,
			Action: plan.AllowedActions[0], Scope: scopeForAutonomyAction(plan.AllowedActions[0]), TargetDeviceID: targetDeviceID,
			DecisionSnapshotID: captured.ID, Decision: captured.Decision, ConfigDigest: plan.ConfigDigest,
			Deadline: captured.Decision.ExpiresAt, State: "admitted", CreatedAt: now, UpdatedAt: now,
		}
		// Keep a terminal effect record until its parent envelope expires. The
		// decision deadline is deliberately shorter and still fences dispatch,
		// but using it as resource retention would let a retention sweep erase
		// the deterministic idempotency record while the plan is still live.
		// A later reconcile could then create the same effect ID and dispatch a
		// second device action.
		retentionExpiry := plan.ExpiresAt.UTC()
		payload, marshalErr := marshalPayload(effect)
		if marshalErr != nil {
			return effect, marshalErr
		}
		resource = &types.OperationalResource{
			Kind: types.ResourceAutonomyEffect, ID: id, State: effect.State, Actor: "system", Tenant: plan.Tenant, Cluster: plan.Cluster, ConfigDigest: plan.ConfigDigest,
			Payload: payload, CreatedAt: now, UpdatedAt: now, ExpiresAt: &retentionExpiry, Version: effect.ResourceVersion,
		}
		if createErr := m.resources.CreateOperationalResource(ctx, resource); createErr != nil {
			if !errors.Is(createErr, store.ErrOperationalConflict) {
				return effect, createErr
			}
			resource, effect, err = m.loadAutonomyEffect(ctx, id)
			if err != nil {
				return nil, err
			}
		} else if auditErr := m.audit(ctx, types.ResourceAutonomyEffect, id, "system", "admit-effect", "", captured.ID, map[string]string{"plan_id": plan.ID, "node": effect.Node}, "admitted"); auditErr != nil {
			return effect, auditErr
		} else {
			err = nil
		}
	}
	if err != nil {
		return nil, err
	}
	switch effect.State {
	case "accepted", "completed", "simulation-only":
		return effect, nil
	case "failed", "blocked":
		return effect, fmt.Errorf("previous effect attempt is %s: %s", effect.State, effect.Summary)
	case "executing":
		// Never guess whether a prior process reached the external executor.
		// The plan will pause on this error and requires an explicit reviewed
		// recovery path rather than replaying an uncertain device action.
		started := "an unknown time"
		if effect.DispatchStartedAt != nil {
			started = effect.DispatchStartedAt.UTC().Format(time.RFC3339Nano)
		}
		return effect, fmt.Errorf("previous effect dispatch is uncertain since %s", started)
	case "admitted":
		// Claim below.
	default:
		return effect, fmt.Errorf("autonomy effect %q has unsupported state %q", effect.ID, effect.State)
	}
	// Claim external dispatch durably before the final envelope check. Two
	// concurrent reconciliation loops race on this optimistic resource update;
	// only one can enter the executor, and a crashed claimant fails closed as
	// an uncertain dispatch rather than retrying a potentially live effect.
	now := m.now().UTC()
	effect.State, effect.DispatchStartedAt, effect.UpdatedAt = "executing", &now, now
	if saveErr := m.saveAutonomyEffect(ctx, resource, effect); saveErr != nil {
		return effect, saveErr
	}
	if auditErr := m.audit(ctx, types.ResourceAutonomyEffect, effect.ID, "system", "dispatch-effect", "", effect.DecisionSnapshotID, nil, "executing"); auditErr != nil {
		return effect, auditErr
	}
	// The durable admission record is created before this last fence so any
	// executor sees an idempotency key.  A pause/rollback/config revision that
	// won the race before this check means no hardware call is made; record the
	// reason rather than leaving an apparently runnable admission behind.
	if err := m.ensureAutonomyEffectEnvelope(ctx, plan); err != nil {
		if blockErr := m.blockAutonomyEffect(ctx, resource, effect, err.Error()); blockErr != nil {
			return effect, blockErr
		}
		return effect, err
	}
	if !effect.Deadline.After(m.now()) {
		err := fmt.Errorf("%w: autonomy effect %q decision evidence has expired", ErrInvalidState, effect.ID)
		if blockErr := m.blockAutonomyEffect(ctx, resource, effect, err.Error()); blockErr != nil {
			return effect, blockErr
		}
		return effect, err
	}
	result, executeErr := m.autonomyExecute(ctx, AutonomyEffectRequest{
		EffectID: effect.ID, PlanID: effect.PlanID, RolloutID: effect.RolloutID, Node: effect.Node,
		Action: effect.Action, Scope: effect.Scope, TargetDeviceID: effect.TargetDeviceID, DecisionSnapshotID: effect.DecisionSnapshotID,
		Decision: effect.Decision, ConfigDigest: effect.ConfigDigest, Deadline: effect.Deadline,
	})
	now = m.now().UTC()
	if executeErr != nil {
		effect.State, effect.Summary, effect.UpdatedAt = "failed", executeErr.Error(), now
	} else {
		effect.State = strings.TrimSpace(result.State)
		if effect.State == "" {
			effect.State = "accepted"
		}
		switch effect.State {
		case "accepted", "completed", "failed":
			// These are the only terminal hand-off outcomes an executor may
			// return. A queued/pending response would make the controller lose
			// the next-cycle evidence re-evaluation guarantee, so it fails
			// closed instead of being counted as a canary action.
		default:
			effect.State = "failed"
			if strings.TrimSpace(result.Summary) == "" {
				result.Summary = "executor returned an unsupported nonterminal effect state"
			}
		}
		effect.Summary, effect.Reference, effect.UpdatedAt = result.Summary, result.Reference, now
	}
	if saveErr := m.saveAutonomyEffect(ctx, resource, effect); saveErr != nil {
		return effect, saveErr
	}
	resultState := effect.State
	if executeErr != nil {
		resultState = "failed"
	}
	if auditErr := m.audit(ctx, types.ResourceAutonomyEffect, effect.ID, "system", "execute-effect", "", effect.DecisionSnapshotID, nil, resultState); auditErr != nil {
		return effect, auditErr
	}
	return effect, executeErr
}

// ensureAutonomyEffectEnvelope is the last durable-state fence before an
// autonomy effect. It intentionally checks the current record rather than
// trusting the plan that was loaded at the start of the reconciliation page:
// explicit pause/rollback and expiry must win over a delayed batch.
func (m *Manager) ensureAutonomyEffectEnvelope(ctx context.Context, expected *GPUAutonomyPlan) error {
	if expected == nil {
		return fmt.Errorf("%w: autonomy plan is unavailable", ErrInvalidState)
	}
	current, err := m.getAutonomyPlan(ctx, expected.ID)
	if err != nil {
		return err
	}
	if current.ResourceVersion != expected.ResourceVersion || current.State != expected.State || (current.State != AutonomyCanary && current.State != AutonomyExpanding) {
		return fmt.Errorf("%w: autonomy plan %q is no longer active (%s)", ErrInvalidState, expected.ID, current.State)
	}
	if current.RolloutID != expected.RolloutID {
		return fmt.Errorf("%w: autonomy plan %q rollout binding changed", ErrInvalidState, expected.ID)
	}
	if current.ConfigDigest != expected.ConfigDigest {
		return fmt.Errorf("%w: autonomy plan %q configuration binding changed", ErrInvalidState, expected.ID)
	}
	if !current.ExpiresAt.After(m.now()) {
		return fmt.Errorf("%w: autonomy plan %q has expired", ErrInvalidState, expected.ID)
	}
	if !allRolesApproved(current) {
		return fmt.Errorf("%w: autonomy plan %q no longer has required approvals", ErrInvalidState, expected.ID)
	}
	return nil
}

func (m *Manager) loadAutonomyEffect(ctx context.Context, id string) (*types.OperationalResource, *AutonomyEffectRecord, error) {
	resource, err := m.resources.GetOperationalResource(ctx, types.ResourceAutonomyEffect, id)
	if err != nil {
		return nil, nil, err
	}
	var effect AutonomyEffectRecord
	if err := json.Unmarshal(resource.Payload, &effect); err != nil {
		return nil, nil, fmt.Errorf("autonomy effect %q is corrupt: %w", id, err)
	}
	effect.ResourceVersion = resource.Version
	return resource, &effect, nil
}

func (m *Manager) saveAutonomyEffect(ctx context.Context, resource *types.OperationalResource, effect *AutonomyEffectRecord) error {
	effect.ResourceVersion = resource.Version + 1
	payload, err := marshalPayload(effect)
	if err != nil {
		return err
	}
	resource.State, resource.Payload, resource.ConfigDigest = effect.State, payload, effect.ConfigDigest
	if err := m.resources.UpdateOperationalResource(ctx, resource, resource.Version); err != nil {
		return err
	}
	effect.ResourceVersion = resource.Version
	return nil
}

func (m *Manager) blockAutonomyEffect(ctx context.Context, resource *types.OperationalResource, effect *AutonomyEffectRecord, reason string) error {
	now := m.now().UTC()
	effect.State, effect.Summary, effect.UpdatedAt = "blocked", reason, now
	if err := m.saveAutonomyEffect(ctx, resource, effect); err != nil {
		return err
	}
	return m.audit(ctx, types.ResourceAutonomyEffect, effect.ID, "system", "block-effect", "", effect.DecisionSnapshotID, nil, reason)
}

func (m *Manager) moveAutonomyPlan(ctx context.Context, expected *GPUAutonomyPlan, state AutonomyPlanState, reason string) error {
	if expected == nil {
		return fmt.Errorf("%w: autonomy plan is unavailable", ErrInvalidState)
	}
	resource, plan, err := m.loadAutonomyPlan(ctx, expected.ID)
	if err != nil {
		return err
	}
	// A reconciliation batch is only allowed to advance the exact plan revision
	// it evaluated. This lets an explicit pause or rollback win a concurrent
	// bake/expansion transition rather than being overwritten by stale work.
	if resource.Version != expected.ResourceVersion || plan.State != expected.State || plan.State.terminal() || plan.State == AutonomyPaused {
		return store.ErrOperationalConflict
	}
	plan.State, plan.UpdatedAt = state, m.now().UTC()
	if err := m.saveAutonomyPlan(ctx, resource, plan, "system"); err != nil {
		return err
	}
	if plan.RolloutID != "" {
		if err := m.updateAutonomyRollout(ctx, plan.RolloutID, state, reason, nil); err != nil {
			return err
		}
	}
	return m.audit(ctx, types.ResourceAutonomyPlan, expected.ID, "system", "reconcile", "", plan.SimulationID, map[string]string{"reason": reason}, string(state))
}

func (m *Manager) createAutonomyRollout(ctx context.Context, actor string, plan *GPUAutonomyPlan) (*AutonomyRollout, error) {
	now := m.now().UTC()
	executionMode := "simulation-only"
	if m.autonomyExecute != nil {
		executionMode = "hardware-qualified"
	}
	rollout := &AutonomyRollout{ID: newID("rollout"), ResourceVersion: 1, PlanID: plan.ID, Tenant: plan.Tenant, Cluster: plan.Cluster, State: AutonomyCanary, ExecutionMode: executionMode, CreatedAt: now, UpdatedAt: now}
	payload, err := marshalPayload(rollout)
	if err != nil {
		return nil, err
	}
	if err := m.resources.CreateOperationalResource(ctx, &types.OperationalResource{
		Kind: types.ResourceAutonomyRollout, ID: rollout.ID, State: string(rollout.State), Actor: actor,
		Tenant: plan.Tenant, Cluster: plan.Cluster, ConfigDigest: plan.ConfigDigest,
		Payload: payload, CreatedAt: now, UpdatedAt: now, ExpiresAt: &plan.ExpiresAt, Version: rollout.ResourceVersion,
	}); err != nil {
		return nil, err
	}
	if err := m.audit(ctx, types.ResourceAutonomyRollout, rollout.ID, actor, "create", "", plan.SimulationID, map[string]string{"plan_id": plan.ID}, string(rollout.State)); err != nil {
		return nil, err
	}
	return rollout, nil
}

func (m *Manager) loadAutonomyPlan(ctx context.Context, id string) (*types.OperationalResource, *GPUAutonomyPlan, error) {
	resource, err := m.resources.GetOperationalResource(ctx, types.ResourceAutonomyPlan, id)
	if err != nil {
		return nil, nil, err
	}
	var plan GPUAutonomyPlan
	if err := json.Unmarshal(resource.Payload, &plan); err != nil {
		return nil, nil, fmt.Errorf("autonomy plan %q is corrupt: %w", id, err)
	}
	plan.ResourceVersion = resource.Version
	return resource, &plan, nil
}

func (m *Manager) getAutonomyPlan(ctx context.Context, id string) (*GPUAutonomyPlan, error) {
	_, plan, err := m.loadAutonomyPlan(ctx, id)
	return plan, err
}

func (m *Manager) saveAutonomyPlan(ctx context.Context, resource *types.OperationalResource, plan *GPUAutonomyPlan, actor string) error {
	// Persist the version the caller will see after this transition in the
	// payload too. It makes optimistic-concurrency usable from REST/CLI without
	// exposing the store envelope as a second competing representation.
	plan.ResourceVersion = resource.Version + 1
	payload, err := marshalPayload(plan)
	if err != nil {
		return err
	}
	resource.State, resource.Actor, resource.ConfigDigest, resource.Payload = string(plan.State), actor, plan.ConfigDigest, payload
	if err := m.resources.UpdateOperationalResource(ctx, resource, resource.Version); err != nil {
		return err
	}
	plan.ResourceVersion = resource.Version
	return nil
}

func (m *Manager) getAutonomyRollout(ctx context.Context, id string) (*AutonomyRollout, error) {
	if id == "" {
		return nil, fmt.Errorf("autonomy rollout is missing")
	}
	resource, err := m.resources.GetOperationalResource(ctx, types.ResourceAutonomyRollout, id)
	if err != nil {
		return nil, err
	}
	var rollout AutonomyRollout
	if err := json.Unmarshal(resource.Payload, &rollout); err != nil {
		return nil, fmt.Errorf("autonomy rollout %q is corrupt: %w", id, err)
	}
	rollout.ResourceVersion = resource.Version
	return &rollout, nil
}

func (m *Manager) saveAutonomyRollout(ctx context.Context, rollout *AutonomyRollout) error {
	resource, err := m.resources.GetOperationalResource(ctx, types.ResourceAutonomyRollout, rollout.ID)
	if err != nil {
		return err
	}
	if rollout.ResourceVersion <= 0 || resource.Version != rollout.ResourceVersion {
		return store.ErrOperationalConflict
	}
	rollout.ResourceVersion = resource.Version + 1
	payload, err := marshalPayload(rollout)
	if err != nil {
		return err
	}
	resource.State, resource.Payload = string(rollout.State), payload
	if err := m.resources.UpdateOperationalResource(ctx, resource, resource.Version); err != nil {
		return err
	}
	rollout.ResourceVersion = resource.Version
	return nil
}

func (m *Manager) updateAutonomyRollout(ctx context.Context, id string, state AutonomyPlanState, reason string, observations []AutonomyObservation) error {
	rollout, err := m.getAutonomyRollout(ctx, id)
	if err != nil {
		return err
	}
	rollout.State, rollout.Reason, rollout.UpdatedAt = state, reason, m.now().UTC()
	if observations != nil {
		rollout.Observations = observations
	}
	return m.saveAutonomyRollout(ctx, rollout)
}

// reserveAutonomyMutation claims an idempotency key before a lifecycle write.
// The resource ID remains the plan ID, so a replay can always return the
// current durable plan without inventing a second mutable object. Callers
// validate their operation immediately after this reservation; the request
// digest includes all state-changing arguments and the observed version.
func (m *Manager) reserveAutonomyMutation(ctx context.Context, actor, planID, action string, mutation AutonomyMutation, params map[string]string) (bool, *GPUAutonomyPlan, error) {
	if err := m.requireResources(); err != nil {
		return false, nil, err
	}
	if strings.TrimSpace(actor) == "" || strings.TrimSpace(planID) == "" || strings.TrimSpace(mutation.IdempotencyKey) == "" {
		return false, nil, fmt.Errorf("actor, plan ID, and idempotency key are required")
	}
	digest := digestJSON(struct {
		PlanID          string            `json:"plan_id"`
		Action          string            `json:"action"`
		ExpectedVersion int               `json:"expected_version"`
		Params          map[string]string `json:"params"`
	}{PlanID: planID, Action: action, ExpectedVersion: mutation.ExpectedVersion, Params: params})
	storageKey := scopedIdempotencyKey(action, mutation.IdempotencyKey)
	if existing, lookupErr := m.resources.GetOperationalIdempotency(ctx, types.ResourceAutonomyPlan, actor, storageKey); lookupErr == nil {
		if existing.RequestDigest != digest || existing.ResourceID != planID {
			return false, nil, ErrIdempotencyConflict
		}
		plan, getErr := m.getAutonomyPlan(ctx, planID)
		return true, plan, getErr
	} else if !errors.Is(lookupErr, store.ErrNotFound) {
		return false, nil, lookupErr
	}
	// A stale browser resource_version or unknown plan is a normal validation
	// failure. Verify it before reserving a key so the user can reread and
	// retry the intended lifecycle operation with that same key.
	current, err := m.getAutonomyPlan(ctx, planID)
	if err != nil {
		return false, nil, err
	}
	if mutation.ExpectedVersion > 0 && mutation.ExpectedVersion != current.ResourceVersion {
		return false, nil, store.ErrOperationalConflict
	}
	record, created, err := m.resources.PutOperationalIdempotency(ctx, &types.OperationalIdempotencyRecord{
		Kind: types.ResourceAutonomyPlan, Actor: actor, Key: storageKey,
		RequestDigest: digest, ResourceID: planID,
	})
	if err != nil {
		return false, nil, err
	}
	if created {
		return false, nil, nil
	}
	if record.RequestDigest != digest || record.ResourceID != planID {
		return false, nil, ErrIdempotencyConflict
	}
	plan, err := m.getAutonomyPlan(ctx, planID)
	return true, plan, err
}

func validateAutonomyRequest(request AutonomyPlanRequest, now time.Time) error {
	if len(request.Selector) == 0 {
		return fmt.Errorf("autonomy selector is required and cannot select every node")
	}
	for key, value := range request.Selector {
		if strings.TrimSpace(key) == "" || strings.TrimSpace(value) == "" {
			return fmt.Errorf("autonomy selector contains an empty key or value")
		}
	}
	if !immutableReference(request.PolicyRef) || !immutableReference(request.ProfileRef) {
		return fmt.Errorf("autonomy policy_ref and profile_ref must name immutable revisions (name@sha256:digest or name#generation)")
	}
	if len(request.AllowedActions) != 1 {
		return fmt.Errorf("autonomy requires exactly one allowed action per plan; create a separate bounded plan for another action")
	}
	seenActions := map[types.AcceleratorAction]bool{}
	for _, action := range request.AllowedActions {
		if !validAutonomyAction(action) || seenActions[action] {
			return fmt.Errorf("autonomy allowed_actions must be nonempty and unique")
		}
		seenActions[action] = true
	}
	if request.Evidence.MaxAge <= 0 || len(request.Evidence.RequiredSources) == 0 {
		return fmt.Errorf("autonomy evidence.max_age and required_sources are required")
	}
	sources := map[string]bool{}
	for _, source := range request.Evidence.RequiredSources {
		if !validAutonomyEvidenceSource(source) || sources[source] {
			return fmt.Errorf("autonomy evidence.required_sources must be supported, nonempty, and unique")
		}
		sources[source] = true
	}
	if request.Guardrails.MaxConcurrentNodes <= 0 || request.Guardrails.MaxActionsPerHour <= 0 || request.Guardrails.ErrorBudget < 0 {
		return fmt.Errorf("autonomy guardrail limits must be positive (error budget nonnegative)")
	}
	if !request.Guardrails.NoActiveIncident {
		return fmt.Errorf("autonomy guardrails.no_active_incident must be true")
	}
	maintenanceWindows := map[string]bool{}
	for _, window := range request.Guardrails.MaintenanceWindows {
		window = strings.TrimSpace(window)
		if window == "" || maintenanceWindows[window] {
			return fmt.Errorf("autonomy guardrails.maintenance_windows must be nonempty and unique")
		}
		maintenanceWindows[window] = true
	}
	if request.Rollout.CanaryNodes <= 0 || request.Rollout.BakeDuration <= 0 {
		return fmt.Errorf("autonomy rollout.canary_nodes and bake_duration must be positive")
	}
	if request.Rollout.CanaryNodes > request.Guardrails.MaxConcurrentNodes {
		return fmt.Errorf("autonomy rollout.canary_nodes cannot exceed guardrails.max_concurrent_nodes")
	}
	previous := request.Rollout.CanaryNodes
	for _, target := range request.Rollout.ExpansionSteps {
		if target <= previous {
			return fmt.Errorf("autonomy rollout.expansion_steps must be strictly increasing and greater than canary_nodes")
		}
		previous = target
	}
	if len(request.Approvals.RequiredRoles) < 2 {
		return fmt.Errorf("autonomy requires at least two approval roles")
	}
	roles := map[string]bool{}
	for _, role := range request.Approvals.RequiredRoles {
		if strings.TrimSpace(role) == "" || roles[role] {
			return fmt.Errorf("autonomy approval roles must be nonempty and unique")
		}
		roles[role] = true
	}
	if !request.Approvals.DistinctSubjects {
		return fmt.Errorf("autonomy approvals must require distinct subjects")
	}
	if request.ExpiresAt.IsZero() || !request.ExpiresAt.After(now) {
		return fmt.Errorf("autonomy expires_at must be in the future")
	}
	return nil
}

func normalizeAutonomyRequest(request *AutonomyPlanRequest) {
	request.PolicyRef = strings.TrimSpace(request.PolicyRef)
	request.ProfileRef = strings.TrimSpace(request.ProfileRef)
	request.Tenant = strings.TrimSpace(request.Tenant)
	request.Cluster = strings.TrimSpace(request.Cluster)
	request.AllowedActions = sortedActions(request.AllowedActions)
	request.Evidence.RequiredSources = append([]string(nil), request.Evidence.RequiredSources...)
	for i := range request.Evidence.RequiredSources {
		request.Evidence.RequiredSources[i] = strings.ToLower(strings.TrimSpace(request.Evidence.RequiredSources[i]))
	}
	sort.Strings(request.Evidence.RequiredSources)
	request.Approvals.RequiredRoles = append([]string(nil), request.Approvals.RequiredRoles...)
	for i := range request.Approvals.RequiredRoles {
		request.Approvals.RequiredRoles[i] = strings.TrimSpace(request.Approvals.RequiredRoles[i])
	}
	sort.Strings(request.Approvals.RequiredRoles)
	request.Guardrails.MaintenanceWindows = append([]string(nil), request.Guardrails.MaintenanceWindows...)
	for i := range request.Guardrails.MaintenanceWindows {
		request.Guardrails.MaintenanceWindows[i] = strings.TrimSpace(request.Guardrails.MaintenanceWindows[i])
	}
	sort.Strings(request.Guardrails.MaintenanceWindows)
}

func immutableReference(value string) bool {
	value = strings.TrimSpace(value)
	if value == "" {
		return false
	}
	name, revision, ok := strings.Cut(value, "@sha256:")
	if ok {
		// The API deliberately validates the *immutable-reference shape*, not
		// one hash implementation's output length. Integrations may use an
		// opaque immutable revision after the sha256 marker in development and
		// test environments, while production policy tooling still supplies a
		// full digest.
		return strings.TrimSpace(name) != "" && strings.TrimSpace(revision) != ""
	}
	name, revision, ok = strings.Cut(value, "#")
	return ok && strings.TrimSpace(name) != "" && strings.TrimSpace(revision) != ""
}

// validateSimulationForAutonomy keeps a plan from borrowing the general
// "permitted" bit of an unrelated simulation.  The action/scope and tenancy
// are part of the authority envelope just as much as its decision digest; an
// old simulation (written before these fields existed) therefore fails closed.
func validateSimulationForAutonomy(plan *GPUAutonomyPlan, simulation *RemediationSimulation) error {
	if simulation == nil || !simulation.Decision.Permitted() {
		if simulation == nil {
			return fmt.Errorf("%w: autonomy plan simulation is unavailable", ErrInvalidState)
		}
		return fmt.Errorf("%w: autonomy plan simulation %q is blocked", ErrInvalidState, simulation.ID)
	}
	if len(plan.AllowedActions) != 1 || simulation.Action != plan.AllowedActions[0] {
		return fmt.Errorf("%w: simulation %q action %q does not match plan action %q", ErrInvalidState, simulation.ID, simulation.Action, plan.AllowedActions[0])
	}
	if simulation.Scope != scopeForAutonomyAction(plan.AllowedActions[0]) {
		return fmt.Errorf("%w: simulation %q scope %q does not match plan action scope %q", ErrInvalidState, simulation.ID, simulation.Scope, scopeForAutonomyAction(plan.AllowedActions[0]))
	}
	if simulation.Scope == types.AcceleratorScopePhysicalDevice && strings.TrimSpace(simulation.DeviceID) == "" {
		return fmt.Errorf("%w: device-scoped simulation %q has no target device", ErrInvalidState, simulation.ID)
	}
	if targetDeviceID := strings.TrimSpace(plan.TargetDeviceID); targetDeviceID != "" && targetDeviceID != strings.TrimSpace(simulation.DeviceID) {
		return fmt.Errorf("%w: simulation %q target device does not match the plan", ErrInvalidState, simulation.ID)
	}
	if simulation.Tenant != plan.Tenant || simulation.Cluster != plan.Cluster {
		return fmt.Errorf("%w: simulation %q tenant/cluster scope does not match the plan", ErrInvalidState, simulation.ID)
	}
	if strings.TrimSpace(simulation.Decision.ConfigDigest) == "" {
		return fmt.Errorf("%w: simulation %q has no configuration digest", ErrInvalidState, simulation.ID)
	}
	return nil
}

func validAutonomyEvidenceSource(source string) bool {
	switch strings.ToLower(strings.TrimSpace(source)) {
	case "agent", "controller", "dcgm":
		return true
	default:
		return false
	}
}

func validAutonomyAction(action types.AcceleratorAction) bool {
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

func autonomyRequestDigestInput(request AutonomyPlanRequest) any {
	request.IdempotencyKey = ""
	request.Selector = copySelector(request.Selector)
	normalizeAutonomyRequest(&request)
	return request
}

func allRolesApproved(plan *GPUAutonomyPlan) bool {
	roles := make(map[string]bool, len(plan.ApprovalRecords))
	for _, approval := range plan.ApprovalRecords {
		if approval.ConfigDigest == plan.ConfigDigest {
			roles[approval.Role] = true
		}
	}
	for _, role := range plan.Approvals.RequiredRoles {
		if !roles[role] {
			return false
		}
	}
	return true
}

func labelsMatch(selector, labels map[string]string) bool {
	for key, value := range selector {
		if labels[key] != value {
			return false
		}
	}
	return true
}

func scopeForAutonomyAction(action types.AcceleratorAction) types.AcceleratorTargetScope {
	if action == types.AcceleratorActionResetDevice {
		return types.AcceleratorScopePhysicalDevice
	}
	return types.AcceleratorScopeNode
}

func copySelector(source map[string]string) map[string]string {
	if len(source) == 0 {
		return nil
	}
	result := make(map[string]string, len(source))
	for key, value := range source {
		result[key] = value
	}
	return result
}

func sortedActions(actions []types.AcceleratorAction) []types.AcceleratorAction {
	result := append([]types.AcceleratorAction(nil), actions...)
	sort.Slice(result, func(i, j int) bool { return result[i] < result[j] })
	return result
}

func containsString(values []string, needle string) bool {
	for _, value := range values {
		if value == needle {
			return true
		}
	}
	return false
}
