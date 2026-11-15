// SPDX-License-Identifier: Apache-2.0

package operations

import (
	"context"
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/kubeneuron/kubeneuron/internal/decision"
	"github.com/kubeneuron/kubeneuron/internal/store"
	"github.com/kubeneuron/kubeneuron/internal/store/postgres"
	"github.com/kubeneuron/kubeneuron/internal/store/sqlite"
	"github.com/kubeneuron/kubeneuron/pkg/types"
)

// atomicityBackend is one real store the atomicity tests run against.
type atomicityBackend struct {
	name string
	open func(t *testing.T) (store.OperationalTransactionalStore, store.Store)
}

func atomicityBackends() []atomicityBackend {
	backends := []atomicityBackend{{name: "sqlite", open: func(t *testing.T) (store.OperationalTransactionalStore, store.Store) {
		st, err := sqlite.Open(":memory:")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = st.Close() })
		return st, st
	}}}
	if dsn := os.Getenv("KUBENEURON_TEST_POSTGRES_DSN"); dsn != "" {
		backends = append(backends, atomicityBackend{name: "postgres", open: func(t *testing.T) (store.OperationalTransactionalStore, store.Store) {
			st, err := postgres.Open(context.Background(), dsn)
			if err != nil {
				t.Fatalf("postgres: %v", err)
			}
			t.Cleanup(func() { _ = st.Close() })
			for _, table := range []string{"operational_audit", "operational_audit_heads", "operational_idempotency", "operational_resources", "actions", "incidents", "nodes"} {
				if _, err := st.SQL.Exec("TRUNCATE TABLE " + table + " CASCADE"); err != nil {
					t.Fatalf("truncate %s: %v", table, err)
				}
			}
			return st, st
		}})
	}
	return backends
}

func forEachBackend(t *testing.T, run func(t *testing.T, mgr *Manager, faulty *faultyQualificationStore, workflow store.Store, now time.Time)) {
	for _, backend := range atomicityBackends() {
		t.Run(backend.name, func(t *testing.T) {
			resources, workflow := backend.open(t)
			faulty := &faultyQualificationStore{OperationalTransactionalStore: resources}
			now := time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)
			mgr := New(Options{
				Resources: faulty,
				Workflow:  workflow,
				Now:       func() time.Time { return now },
				BuildSnapshot: func(_ context.Context, node string, request decision.Request) (decision.Snapshot, error) {
					return operationSnapshot(now, request), nil
				},
				ListNodes: func(context.Context) ([]*types.Node, error) {
					return []*types.Node{{Name: "gpu-a", Labels: map[string]string{"pool": "a100"}}}, nil
				},
				CreateIncident: func(_ context.Context, signal types.Signal) (*types.Incident, error) {
					return &types.Incident{ID: "incident-1", Target: signal.Target, Class: signal.Class}, nil
				},
			})
			run(t, mgr, faulty, workflow, now)
		})
	}
}

func atomicitySimulation(t *testing.T, mgr *Manager, key string) *RemediationSimulation {
	t.Helper()
	simulation, _, err := mgr.CreateSimulation(context.Background(), "alice", SimulationRequest{
		Node: "gpu-a", DeviceID: "GPU-a", Action: types.AcceleratorActionResetDevice,
		Scope: types.AcceleratorScopePhysicalDevice, Class: types.ClassECCDBE,
		Rationale: "atomicity", IdempotencyKey: key,
	})
	if err != nil || !simulation.Decision.Permitted() {
		t.Fatalf("simulation = %#v err=%v", simulation, err)
	}
	return simulation
}

func atomicityPlan(t *testing.T, mgr *Manager, now time.Time) *GPUAutonomyPlan {
	t.Helper()
	simulation := atomicitySimulation(t, mgr, "sim-for-plan")
	plan, _, err := mgr.CreateAutonomyPlan(context.Background(), "author", AutonomyPlanRequest{
		Selector: map[string]string{"pool": "a100"}, PolicyRef: "ecc-reset@sha256:policy", ProfileRef: "nvidia-a100#1",
		AllowedActions: []types.AcceleratorAction{types.AcceleratorActionResetDevice},
		Evidence:       AutonomyEvidenceRequirements{MaxAge: 5 * time.Minute, RequiredSources: []string{"agent"}},
		Guardrails:     AutonomyGuardrails{MaxConcurrentNodes: 1, MaxActionsPerHour: 2, NoActiveIncident: true},
		Rollout:        AutonomyRolloutPolicy{CanaryNodes: 1, BakeDuration: time.Minute},
		Approvals:      AutonomyApprovalRequirements{RequiredRoles: []string{"platform", "safety"}, DistinctSubjects: true},
		SimulationID:   simulation.ID, ExpiresAt: now.Add(time.Hour), IdempotencyKey: "plan",
	})
	if err != nil {
		t.Fatal(err)
	}
	return plan
}

func auditCount(t *testing.T, mgr *Manager, kind types.OperationalResourceKind, id, action string) int {
	t.Helper()
	events, err := mgr.resources.ListOperationalAudit(context.Background(), kind, id, 100)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, e := range events {
		if e.Action == action {
			n++
		}
	}
	return n
}

// A6, the audit's reproducer: a pause that fails validation used to leave
// its key behind, and the retry answered replay=true with the plan unchanged.
func TestFailedMutationLeavesNoKeyToReplay(t *testing.T) {
	forEachBackend(t, func(t *testing.T, mgr *Manager, _ *faultyQualificationStore, _ store.Store, now time.Time) {
		ctx := context.Background()
		plan := atomicityPlan(t, mgr, now)
		mutation := AutonomyMutation{IdempotencyKey: "pause-1"}
		for attempt := 0; attempt < 2; attempt++ {
			got, replay, err := mgr.PauseAutonomyPlanRequest(ctx, "operator", plan.ID, "", mutation)
			if err == nil || replay || got != nil {
				t.Fatalf("attempt %d: pause with no reason = %#v replay=%v err=%v; want a plain failure, never a replay", attempt, got, replay, err)
			}
		}
		if _, err := mgr.resources.GetOperationalIdempotency(ctx, types.ResourceAutonomyPlan, "operator", scopedIdempotencyKey("pause", "pause-1")); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("a failed pause left its key: %v", err)
		}
		paused, replay, err := mgr.PauseAutonomyPlanRequest(ctx, "operator", plan.ID, "incident", AutonomyMutation{IdempotencyKey: "pause-2"})
		if err != nil || replay || paused.State != AutonomyPaused {
			t.Fatalf("valid pause = %#v replay=%v err=%v", paused, replay, err)
		}
		again, replay, err := mgr.PauseAutonomyPlanRequest(ctx, "operator", plan.ID, "incident", AutonomyMutation{IdempotencyKey: "pause-2"})
		if err != nil || !replay || again.State != AutonomyPaused {
			t.Fatalf("replay of the committed pause = %#v replay=%v err=%v", again, replay, err)
		}
		if n := auditCount(t, mgr, types.ResourceAutonomyPlan, plan.ID, "paused"); n != 1 {
			t.Fatalf("pause audit events = %d, want 1", n)
		}
	})
}

// A7: a failure between the change and its audit event leaves neither, and a
// retry with the same key then executes for real.
func TestFailureBetweenChangeAndAuditLeavesNeither(t *testing.T) {
	forEachBackend(t, func(t *testing.T, mgr *Manager, faulty *faultyQualificationStore, _ store.Store, now time.Time) {
		ctx := context.Background()
		plan := atomicityPlan(t, mgr, now)
		mutation := AutonomyMutation{IdempotencyKey: "approve-1", ExpectedVersion: plan.ResourceVersion}
		faulty.failAuditAt, faulty.failAuditErr = 1, errors.New("injected audit failure")
		if _, _, err := mgr.ApproveAutonomyPlanRequest(ctx, "alice", plan.ID, "platform", mutation); err == nil {
			t.Fatal("approve with a failing audit append succeeded")
		}
		faulty.clear()
		stored, err := mgr.GetAutonomyPlan(ctx, plan.ID)
		if err != nil {
			t.Fatal(err)
		}
		if stored.ResourceVersion != plan.ResourceVersion || len(stored.ApprovalRecords) != 0 || stored.State != plan.State {
			t.Fatalf("plan after a failed approve = v%d %s %d approvals; the change committed without its audit event",
				stored.ResourceVersion, stored.State, len(stored.ApprovalRecords))
		}
		approved, replay, err := mgr.ApproveAutonomyPlanRequest(ctx, "alice", plan.ID, "platform", mutation)
		if err != nil || replay || len(approved.ApprovalRecords) != 1 {
			t.Fatalf("retry after the failure = %#v replay=%v err=%v; want a real execution", approved, replay, err)
		}
		if n := auditCount(t, mgr, types.ResourceAutonomyPlan, plan.ID, "approve"); n != 1 {
			t.Fatalf("approve audit events = %d, want 1", n)
		}

		// The same for a create: the decision snapshot, the simulation and both
		// audit events go together.
		faulty.failAuditAt, faulty.failAuditErr = 2, errors.New("injected audit failure")
		if _, _, err := mgr.CreateSimulation(ctx, "alice", SimulationRequest{
			Node: "gpu-a", DeviceID: "GPU-a", Action: types.AcceleratorActionResetDevice,
			Scope: types.AcceleratorScopePhysicalDevice, Class: types.ClassECCDBE, Rationale: "r", IdempotencyKey: "sim-x",
		}); err == nil {
			t.Fatal("simulation with a failing audit append succeeded")
		}
		faulty.clear()
		if _, err := mgr.resources.GetOperationalIdempotency(ctx, types.ResourceRemediationSimulation, "alice", scopedIdempotencyKey("simulate", "sim-x")); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("a failed simulation left its key: %v", err)
		}
		sim, replay, err := mgr.CreateSimulation(ctx, "alice", SimulationRequest{
			Node: "gpu-a", DeviceID: "GPU-a", Action: types.AcceleratorActionResetDevice,
			Scope: types.AcceleratorScopePhysicalDevice, Class: types.ClassECCDBE, Rationale: "r", IdempotencyKey: "sim-x",
		})
		if err != nil || replay || sim == nil {
			t.Fatalf("simulation retry = %#v replay=%v err=%v", sim, replay, err)
		}
	})
}

// Concurrent duplicates of one request execute once; the others replay it.
func TestConcurrentDuplicateRequestsExecuteOnce(t *testing.T) {
	forEachBackend(t, func(t *testing.T, mgr *Manager, _ *faultyQualificationStore, _ store.Store, now time.Time) {
		ctx := context.Background()
		plan := atomicityPlan(t, mgr, now)
		const callers = 6
		var wg sync.WaitGroup
		var mu sync.Mutex
		executed, replayed := 0, 0
		var failures []error
		for i := 0; i < callers; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_, replay, err := mgr.RollbackAutonomyPlanRequest(ctx, "operator", plan.ID, "bad canary", AutonomyMutation{IdempotencyKey: "rollback-1"})
				mu.Lock()
				defer mu.Unlock()
				switch {
				case err != nil:
					failures = append(failures, err)
				case replay:
					replayed++
				default:
					executed++
				}
			}()
		}
		wg.Wait()
		if executed != 1 {
			t.Fatalf("executed %d, replayed %d, failed %v; want exactly one execution", executed, replayed, failures)
		}
		for _, err := range failures {
			// A loser on PostgreSQL may see the winner's committed row version.
			if !errors.Is(err, store.ErrOperationalConflict) {
				t.Fatalf("unexpected failure: %v", err)
			}
		}
		if n := auditCount(t, mgr, types.ResourceAutonomyPlan, plan.ID, "rolledback"); n != 1 {
			t.Fatalf("rollback audit events = %d, want 1", n)
		}
	})
}

// A store that cannot commit atomically is refused for mutations instead of
// being written one statement at a time.
func TestMutationsRequireATransactionalStore(t *testing.T) {
	st, err := sqlite.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	mgr := New(Options{Resources: plainOperationalStore{st}, Workflow: st})
	if _, _, err := mgr.CreateCandidate(context.Background(), "alice", CandidateUpload{IdempotencyKey: "k", Content: []byte("apiVersion: kubeneuron.io/v1alpha1\nkind: CandidateConfiguration\n")}); err == nil {
		t.Fatal("a non-transactional store accepted a mutation")
	}
}
