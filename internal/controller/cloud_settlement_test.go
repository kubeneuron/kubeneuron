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
