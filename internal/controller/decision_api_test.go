package controller

import (
	"context"
	"testing"
	"time"

	"github.com/kubeneuron/kubeneuron/internal/decision"
	"github.com/kubeneuron/kubeneuron/pkg/types"
)

func TestBuildDecisionSnapshotFencesIncidentAndLeasedActionOwnership(t *testing.T) {
	c, st := newIngestTestController(t)
	ctx := context.Background()
	now := time.Now().UTC()
	for _, node := range []string{"incident-node", "lease-node"} {
		if err := st.UpsertNode(ctx, &types.Node{Name: node, UID: node + "-uid", AgentLastSeen: now}); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.CreateIncident(ctx, &types.Incident{
		ID: "active-incident", Target: types.Target{Node: "incident-node", GPUUUID: "GPU-a"},
		Class: types.ClassECCDBE, State: types.StateExecuting, Playbook: "ecc-dbe",
		RemediationSlotHeld: true, SignalSeen: 1, OpenedAt: now, UpdatedAt: now, StateChangedAt: now,
	}); err != nil {
		t.Fatal(err)
	}

	snapshot, err := c.BuildDecisionSnapshot(ctx, "incident-node", decision.Request{Class: decision.ActionAutonomous})
	if err != nil {
		t.Fatal(err)
	}
	if !snapshot.IncidentConflict || !snapshot.OwnershipConflict {
		t.Fatalf("active incident snapshot must fence both conflict dimensions: %#v", snapshot)
	}

	if err := st.EnqueueAction(ctx, "lease-node", types.Action{ID: "active-lease", Type: types.ActionRunDiag, Timeout: time.Minute}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.ClaimNextAction(ctx, "lease-node", "boot-a", time.Minute); err != nil {
		t.Fatal(err)
	}
	snapshot, err = c.BuildDecisionSnapshot(ctx, "lease-node", decision.Request{Class: decision.ActionAutonomous})
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.IncidentConflict || !snapshot.OwnershipConflict {
		t.Fatalf("live action lease must independently fence ownership: %#v", snapshot)
	}
}
