// SPDX-License-Identifier: Apache-2.0

package postgres

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/kubeneuron/kubeneuron/pkg/types"
)

// Delivery marks belong to one round and are kept per channel: a mark for an
// earlier round is ignored, each channel is recorded once, an incident write
// from an older copy leaves them alone, and a new round starts with none.
func TestApprovalNotifiedMarksOnlyTheCurrentRound(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	now := time.Now()
	inc := &types.Incident{
		ID: "inc-notified", Target: types.Target{Node: "node-a"}, Class: types.ClassECCDBE,
		State: types.StateAwaitingApproval, ApprovalEpoch: 2, OpenedAt: now, UpdatedAt: now, StateChangedAt: now,
	}
	if err := s.CreateIncident(ctx, inc); err != nil {
		t.Fatal(err)
	}
	accepted := func(epoch int) []string {
		t.Helper()
		got, err := s.GetIncident(ctx, inc.ID)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for ch := range got.ApprovalNotified.Accepted(epoch) {
			out = append(out, ch)
		}
		slices.Sort(out)
		return out
	}
	if err := s.MarkApprovalNotified(ctx, inc.ID, 1, "slack"); err != nil || accepted(1) != nil || accepted(2) != nil {
		t.Fatalf("an earlier round was marked: err=%v round1=%v round2=%v", err, accepted(1), accepted(2))
	}
	for _, ch := range []string{"slack", "pagerduty", "slack"} {
		if err := s.MarkApprovalNotified(ctx, inc.ID, 2, ch); err != nil {
			t.Fatal(err)
		}
	}
	if got := accepted(2); !slices.Equal(got, []string{"pagerduty", "slack"}) {
		t.Fatalf("round 2 accepted by %v, want pagerduty and slack once each", got)
	}
	stale := *inc // holds no delivery marks
	stale.UpdatedAt = time.Now()
	if err := s.UpdateIncident(ctx, &stale); err != nil {
		t.Fatal(err)
	}
	if got := accepted(2); len(got) != 2 {
		t.Fatalf("an incident write dropped delivery marks: %v", got)
	}
	stale.ApprovalEpoch = 3
	if err := s.UpdateIncident(ctx, &stale); err != nil {
		t.Fatal(err)
	}
	if got := accepted(3); got != nil {
		t.Fatalf("round 3 starts accepted by %v, want none", got)
	}
	if err := s.MarkApprovalNotified(ctx, "missing", 1, "slack"); err == nil {
		t.Fatal("marking a missing incident succeeded")
	}
}
