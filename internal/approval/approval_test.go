package approval

import (
	"context"
	"testing"
	"time"

	"github.com/kubeneuron/kubeneuron/internal/store"
	"github.com/kubeneuron/kubeneuron/internal/store/sqlite"
	"github.com/kubeneuron/kubeneuron/pkg/types"
)

func TestExpiredAnchorsToStateChange(t *testing.T) {
	m := New(nil, time.Hour)
	parked := time.Now().Add(-2 * time.Hour)

	inc := &types.Incident{
		State:          types.StateAwaitingApproval,
		StateChangedAt: parked,
		// A signal storm keeps bumping UpdatedAt; it must not postpone expiry.
		UpdatedAt: time.Now(),
	}
	if !m.Expired(inc) {
		t.Fatal("incident awaiting approval past TTL must be expired despite recent UpdatedAt")
	}

	fresh := &types.Incident{
		State:          types.StateAwaitingApproval,
		StateChangedAt: time.Now().Add(-time.Minute),
		UpdatedAt:      time.Now(),
	}
	if m.Expired(fresh) {
		t.Fatal("incident within TTL must not be expired")
	}

	other := &types.Incident{
		State:          types.StateExecuting,
		StateChangedAt: parked,
	}
	if m.Expired(other) {
		t.Fatal("only AWAITING_APPROVAL incidents can expire")
	}
}

func decideTestStore(t *testing.T) store.Store {
	t.Helper()
	s, err := sqlite.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestDecideRecordsOnlyForAwaitingApproval(t *testing.T) {
	st := decideTestStore(t)
	ctx := context.Background()
	now := time.Now()
	inc := &types.Incident{
		ID: "inc-1", Target: types.Target{Node: "n1", GPUUUID: "GPU-1"},
		Class: types.ClassECCDBE, State: types.StateAwaitingApproval,
		OpenedAt: now, UpdatedAt: now, StateChangedAt: now,
	}
	if err := st.CreateIncident(ctx, inc); err != nil {
		t.Fatal(err)
	}

	m := New(st, time.Hour)
	if err := m.Decide(ctx, "inc-1", StepIdentity{StepName: "reboot", StepAction: "agent.reboot", PlaybookName: "pb", StepHash: "h"}, "alice", "api", types.ApprovalApproved); err != nil {
		t.Fatalf("Decide: %v", err)
	}
	recorded, err := st.LatestApproval(ctx, "inc-1")
	if err != nil {
		t.Fatal(err)
	}
	if recorded.Decision != types.ApprovalApproved || recorded.Actor != "alice" ||
		recorded.Channel != "api" || recorded.StepName != "reboot" ||
		recorded.StepAction != "agent.reboot" || recorded.PlaybookName != "pb" || recorded.StepHash != "h" {
		t.Fatalf("approval = %+v", recorded)
	}

	// A decision for an unknown incident fails loudly.
	if err := m.Decide(ctx, "missing", StepIdentity{StepName: "reboot"}, "alice", "api", types.ApprovalApproved); err == nil {
		t.Fatal("decision for an unknown incident must fail")
	}

	// A decision for an incident not awaiting approval is rejected: nothing
	// is pending, so recording one would fabricate an audit trail entry.
	executing := &types.Incident{
		ID: "inc-2", Target: types.Target{Node: "n2", GPUUUID: "GPU-2"},
		Class: types.ClassECCDBE, State: types.StateExecuting,
		OpenedAt: now, UpdatedAt: now, StateChangedAt: now,
	}
	if err := st.CreateIncident(ctx, executing); err != nil {
		t.Fatal(err)
	}
	if err := m.Decide(ctx, "inc-2", StepIdentity{StepName: "reboot"}, "alice", "api", types.ApprovalRejected); err == nil {
		t.Fatal("decision outside AWAITING_APPROVAL must be rejected")
	}
	if _, err := st.LatestApproval(ctx, "inc-2"); err == nil {
		t.Fatal("rejected decision must not be recorded")
	}
}
