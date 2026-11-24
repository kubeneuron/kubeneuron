// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"testing"
	"time"

	"github.com/kubeneuron/kubeneuron/internal/store"
	"github.com/kubeneuron/kubeneuron/pkg/types"
)

func TestSettleCloudOperation(t *testing.T) {
	at := time.Now()
	const reason = "checked group activity"
	for _, outcome := range []string{"", types.SuccessorHandedOver} {
		inc := &types.Incident{ID: "inc", Successor: &types.SuccessorRecord{Outcome: outcome}}
		if err := settleCloudOperation(inc, "  ", at); err == nil || inc.Successor.Outcome != outcome {
			t.Fatal("empty reason accepted or mutated plan")
		}
		if err := settleCloudOperation(inc, reason, at); err != nil {
			t.Fatal(err)
		}
		if s := inc.Successor; s.Outcome != "settled-by-operator" || s.Reason != reason || !s.EndedAt.Equal(at) {
			t.Fatalf("successor = %+v", s)
		}
		if err := settleCloudOperation(inc, reason, at); err == nil || err.Error() != "Incident inc has no open cloud operation to mark as settled." {
			t.Fatalf("settled plan: %v", err)
		}
	}
	for _, outcome := range []string{"", types.RemoveFirstNeedsOperator} {
		inc := &types.Incident{ID: "inc", RemoveFirst: &types.RemoveFirstRecord{Outcome: outcome}}
		if err := settleCloudOperation(inc, reason, at); err != nil {
			t.Fatal(err)
		}
		if r := inc.RemoveFirst; r.Outcome != "settled-by-operator" || r.Reason != reason || !r.EndedAt.Equal(at) {
			t.Fatalf("remove-first = %+v", r)
		}
	}
}

func TestVerifiedClosureRetainsOwnerForHandedOverCloudPlan(t *testing.T) {
	for _, kind := range []string{"replace", "remove-first"} {
		t.Run(kind, func(t *testing.T) {
			c, st, _ := walkFixture(t, false)
			ctx := t.Context()
			owner := &types.Incident{ID: "owner", Target: types.Target{Node: "n1"}, RepairNodeUID: "uid", Class: types.ClassFellOffBus, State: types.StateVerifying}
			if kind == "replace" {
				owner.Successor = &types.SuccessorRecord{Outcome: types.SuccessorHandedOver}
			} else {
				owner.RemoveFirst = &types.RemoveFirstRecord{Outcome: types.RemoveFirstNeedsOperator}
			}
			follower := &types.Incident{ID: "new", Target: owner.Target, RepairNodeUID: "uid", Class: types.ClassAcceleratorFault, State: types.StateEvaluating}
			for _, inc := range []*types.Incident{owner, follower} {
				if err := st.CreateIncident(ctx, inc); err != nil {
					t.Fatal(err)
				}
			}
			claim := func(id string) string {
				t.Helper()
				var got string
				err := st.WithTx(ctx, func(tx store.Tx) error {
					var err error
					got, err = tx.ClaimNodeRemediation(ctx, "n1", "uid", id, time.Now())
					return err
				})
				if err != nil {
					t.Fatal(err)
				}
				return got
			}
			if got := claim(owner.ID); got != owner.ID {
				t.Fatal(got)
			}
			if err := c.transition(ctx, mustIncident(t, st, owner.ID), types.StateResolved, "system", "resolve", "verified", nil); err != nil {
				t.Fatal(err)
			}
			if got := claim(follower.ID); got != owner.ID {
				t.Fatalf("new claim returned %s, want original owner", got)
			}
			const reason = "checked the cloud operation"
			resolved, replayed, err := c.ResolveSettledCloudOperation(ctx, owner.ID, "alice", reason, 0, "confirm")
			if err != nil || replayed || resolved.State != types.StateResolved {
				t.Fatalf("settle: %v %v %v", resolved, replayed, err)
			}
			if _, replayed, err = c.ResolveSettledCloudOperation(ctx, owner.ID, "alice", reason, 0, "confirm"); err != nil || !replayed {
				t.Fatalf("retry: %v %v", replayed, err)
			}
			if got := claim(follower.ID); got != follower.ID {
				t.Fatalf("after settlement owner = %s", got)
			}
			entries := trail(t, st, owner.ID)
			var count int
			for _, entry := range entries {
				if entry.Action == "cloud-operation-settled" && entry.Actor == "alice" && entry.Result == reason {
					count++
				}
			}
			if count != 1 {
				t.Fatalf("settlement audit entries = %d", count)
			}

		})
	}
}

func TestEarlierSuccessorSettlementWhileExecuting(t *testing.T) {
	c, st, _ := walkFixture(t, false)
	ctx := t.Context()
	inc := &types.Incident{ID: "active", Target: types.Target{Node: "n1"}, State: types.StateExecuting, Attempt: 1, RemediationSlotHeld: true}
	inc.Successor = &types.SuccessorRecord{Outcome: types.SuccessorHandedOver}
	inc.Successor.Operation = successorOperation(inc)
	inc.Attempt++
	if err := st.CreateIncident(ctx, inc); err != nil {
		t.Fatal(err)
	}
	for _, wantReplay := range []bool{false, true} {
		got, replay, err := c.ResolveSettledCloudOperation(ctx, inc.ID, "operator", "checked pool", 0, "settle-earlier")
		if err != nil {
			t.Fatal(err)
		}
		if replay != wantReplay || got.State != types.StateExecuting || !got.RemediationSlotHeld || got.Successor.Outcome != "settled-by-operator" {
			t.Fatalf("unexpected settlement: replay=%t incident=%+v", replay, got)
		}
	}
	entries := trail(t, st, inc.ID)
	if len(entries) != 1 || entries[0].Action != "earlier-successor-settled" || entries[0].Params["operation"] != inc.Successor.Operation {
		t.Fatalf("unexpected settlement audit: %v", entries)
	}
}

func TestCurrentExecutingSuccessorCannotBeSettled(t *testing.T) {
	c, st, _ := walkFixture(t, false)
	ctx := t.Context()
	inc := &types.Incident{ID: "current", State: types.StateExecuting, Attempt: 1, RemediationSlotHeld: true}
	inc.Successor = &types.SuccessorRecord{Outcome: ""}
	inc.Successor.Operation = successorOperation(inc)
	if err := st.CreateIncident(ctx, inc); err != nil {
		t.Fatal(err)
	}
	_, _, err := c.ResolveSettledCloudOperation(ctx, inc.ID, "operator", "checked pool", 0, "confirm")
	if err == nil || err.Error() != "the current replacement is still active; check the incident again when it needs an operator" {
		t.Fatalf("unexpected error %v", err)
	}
	got := mustIncident(t, st, inc.ID)
	if got.State != types.StateExecuting || !got.RemediationSlotHeld || got.Successor.Outcome != "" || len(trail(t, st, inc.ID)) != 0 {
		t.Fatalf("refused settlement changed incident %s", inc.ID)
	}
}

func TestPlanningWithEarlierHandedOverSuccessorParksIncident(t *testing.T) {
	c, st, _ := walkFixture(t, false)
	ctx := t.Context()
	inc := &types.Incident{ID: "planning", Target: types.Target{Node: "n1"}, State: types.StateExecuting, Attempt: 2}
	inc.Successor = &types.SuccessorRecord{Outcome: types.SuccessorHandedOver, Reason: "legacy provider diagnostic"}
	inc.Successor.Operation = "planning-a1"
	inc.Successor.Pool = "train"
	if err := st.CreateIncident(ctx, inc); err != nil {
		t.Fatal(err)
	}
	want := `KubeNeuron could not confirm that the replacement node for n1 was removed from pool train. The pool may have one node more than planned, and that node is billed. Check which machines remain in pool train and its size. Then run kubeneuronctl resolve ` + inc.ID + ` --cloud-operation-settled --reason "<what you found>"`
	if err := c.planSuccessor(ctx, inc); err == nil || err.Error() != want {
		t.Fatalf("planning returned %v, want operator handover", err)
	}
	got := mustIncident(t, st, inc.ID)
	if got.State != types.StateNeedsHuman || got.Successor.Operation != "planning-a1" || got.Successor.Outcome != types.SuccessorHandedOver {
		t.Fatalf("unexpected state after planning: %+v", got)
	}
	if !auditHas(trail(t, st, inc.ID), "quarantine", want) {
		t.Fatal("missing operator command in quarantine audit")
	}
	got, _, err := c.ResolveSettledCloudOperation(ctx, inc.ID, "operator", "checked pool", 0, "settle-parked")
	if err != nil || got.State != types.StateResolved {
		t.Fatalf("parked settlement: incident=%v err=%v", got, err)
	}
}

func TestSettleCurrentSuccessorAfterDurableHandover(t *testing.T) {
	for _, reason := range []string{"operator cleanup interrupted before quarantine", "provider needs operator", "cannot confirm successor identity during cleanup", "the node it was to replace is gone, so the successor is kept"} {
		t.Run(reason, func(t *testing.T) {
			c, st, _ := walkFixture(t, false)
			ctx := t.Context()
			inc := &types.Incident{ID: "interrupted", State: types.StateExecuting, Attempt: 1, RemediationSlotHeld: true, Successor: &types.SuccessorRecord{}}
			inc.Successor.Operation = successorOperation(inc)
			if err := st.CreateIncident(ctx, inc); err != nil {
				t.Fatal(err)
			}
			if err := c.endSuccessor(ctx, inc, types.SuccessorHandedOver, reason); err != nil {
				t.Fatal(err)
			}
			saved := mustIncident(t, st, inc.ID)
			if saved.State != types.StateExecuting || saved.Successor.Outcome != types.SuccessorHandedOver {
				t.Fatalf("unexpected persisted handover: %+v", saved)
			}
			got, _, err := c.ResolveSettledCloudOperation(ctx, inc.ID, "alice", "checked pool", 0, "settled")
			if err != nil {
				t.Fatal(err)
			}
			if got.State != types.StateResolved || got.RemediationSlotHeld || got.Successor.Outcome != "settled-by-operator" {
				t.Fatalf("unexpected settlement: %+v", got)
			}
		})
	}
}

func TestSettleHandedOverSuccessorWithoutOperation(t *testing.T) {
	c, st, _ := walkFixture(t, false)
	ctx := t.Context()
	inc := &types.Incident{ID: "missing-operation", State: types.StateExecuting, Attempt: 2, RemediationSlotHeld: true,
		Successor: &types.SuccessorRecord{Outcome: types.SuccessorHandedOver}}
	if err := st.CreateIncident(ctx, inc); err != nil {
		t.Fatal(err)
	}
	got, _, err := c.ResolveSettledCloudOperation(ctx, inc.ID, "alice", "checked pool", 0, "settled")
	if err != nil {
		t.Fatal(err)
	}
	if got.State != types.StateResolved || got.Successor.Outcome != "settled-by-operator" || got.RemediationSlotHeld {
		t.Fatalf("unexpected settlement: %+v", got)
	}
}
