// SPDX-License-Identifier: Apache-2.0

package controller

// Audit #22, A5/A10/A11/A12: the per-node pause holds before every step,
// the first decision of a round is final, an expired request cannot be
// decided, and an unreadable pause state holds instead of running.

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/kubeneuron/kubeneuron/internal/store"
	"github.com/kubeneuron/kubeneuron/internal/store/sqlite"
	"github.com/kubeneuron/kubeneuron/pkg/types"
)

func setNodePaused(t *testing.T, st *sqlite.Store, name string, paused bool) {
	t.Helper()
	ctx := context.Background()
	n, err := st.GetNode(ctx, name)
	if err != nil {
		t.Fatal(err)
	}
	n.Paused = paused
	if err := st.UpsertNode(ctx, n); err != nil {
		t.Fatal(err)
	}
}

func incidentState(t *testing.T, st *sqlite.Store, id string) *types.Incident {
	t.Helper()
	inc, err := st.GetIncident(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return inc
}

// A5: an approved step must not start while its node is paused; the approval
// stays recorded and the step starts once the pause is lifted.
func TestApprovedStepWaitsForNodePause(t *testing.T) {
	c, st, id := approvalEpochFixture(t)
	ctx := context.Background()
	if err := c.DecideApproval(ctx, id, "alice", "cli", types.ApprovalApproved, 0, "", ""); err != nil {
		t.Fatal(err)
	}
	setNodePaused(t, st, "n1", true)
	c.reconcile(ctx)
	c.reconcile(ctx)
	if inc := incidentState(t, st, id); inc.State != types.StateAwaitingApproval || inc.StepIndex != 0 {
		t.Fatalf("paused node: state=%s step=%d, want the approved step held", inc.State, inc.StepIndex)
	}
	if _, err := st.FirstApprovalDecision(ctx, id, 1); err != nil {
		t.Fatalf("the approval must stay recorded while paused: %v", err)
	}
	setNodePaused(t, st, "n1", false)
	c.reconcile(ctx)
	if inc := incidentState(t, st, id); inc.State == types.StateAwaitingApproval {
		t.Fatalf("after unpause: state=%s, want the approved step started", inc.State)
	}
}

// A10: a reject is final. A second decision is refused by the API, and a
// later approve that raced past that check is ignored by the walk.
func TestFirstDecisionOfARoundIsFinal(t *testing.T) {
	c, st, id := approvalEpochFixture(t)
	ctx := context.Background()
	if err := c.DecideApproval(ctx, id, "bob", "cli", types.ApprovalRejected, 0, "", ""); err != nil {
		t.Fatal(err)
	}
	err := c.DecideApproval(ctx, id, "alice", "cli", types.ApprovalApproved, 0, "", "")
	if err == nil || !strings.Contains(err.Error(), "already decided") {
		t.Fatalf("second decision = %v, want it refused", err)
	}
	// The racing approve: recorded with the round's identity after the reject.
	request, err := st.GetApprovalRequest(ctx, id, 1)
	if err != nil {
		t.Fatal(err)
	}
	late := *request
	late.Decision, late.Actor, late.Channel, late.At = types.ApprovalApproved, "alice", "cli", time.Now()
	if err := st.RecordApproval(ctx, &late); err != nil {
		t.Fatal(err)
	}
	c.reconcile(ctx)
	if inc := incidentState(t, st, id); inc.State != types.StateNeedsHuman {
		t.Fatalf("state=%s, want NEEDS_HUMAN: the reject cannot be overridden", inc.State)
	}
}

func backdatePark(t *testing.T, c *Controller, st *sqlite.Store, id string) time.Duration {
	t.Helper()
	ttl := c.runtimeConfig(context.Background()).ApprovalTTL
	inc := incidentState(t, st, id)
	inc.StateChangedAt = time.Now().Add(-ttl - time.Minute)
	if err := st.UpdateIncident(context.Background(), inc); err != nil {
		t.Fatal(err)
	}
	return ttl
}

// A11: a request older than the TTL cannot be decided, even before the walk
// has moved it to EXPIRED.
func TestExpiredRequestCannotBeDecided(t *testing.T) {
	c, st, id := approvalEpochFixture(t)
	backdatePark(t, c, st, id)
	err := c.DecideApproval(context.Background(), id, "alice", "cli", types.ApprovalApproved, 0, "", "")
	if err == nil || !strings.Contains(err.Error(), "expired") {
		t.Fatalf("decision on an expired request = %v, want it refused", err)
	}
}

// A11: a decision recorded after the expiry (a click racing the walk) is not
// honored; the round expires and nothing runs.
func TestDecisionAfterExpiryIsIgnored(t *testing.T) {
	c, st, id := approvalEpochFixture(t)
	ctx := context.Background()
	backdatePark(t, c, st, id)
	request, err := st.GetApprovalRequest(ctx, id, 1)
	if err != nil {
		t.Fatal(err)
	}
	late := *request
	late.Decision, late.Actor, late.Channel, late.At = types.ApprovalApproved, "alice", "cli", time.Now()
	if err := st.RecordApproval(ctx, &late); err != nil {
		t.Fatal(err)
	}
	c.reconcile(ctx)
	if inc := incidentState(t, st, id); inc.State != types.StateExpired {
		t.Fatalf("state=%s, want EXPIRED: a late approval must not execute", inc.State)
	}
}

type nodeReadFailingStore struct{ *sqlite.Store }

func (s nodeReadFailingStore) GetNode(context.Context, string) (*types.Node, error) {
	return nil, errors.New("database is locked")
}

// A12: when the pause state cannot be read, the walk holds instead of
// treating the node as unpaused.
func TestUnreadablePauseStateHolds(t *testing.T) {
	c, st, id := approvalEpochFixture(t)
	ctx := context.Background()
	if err := c.DecideApproval(ctx, id, "alice", "cli", types.ApprovalApproved, 0, "", ""); err != nil {
		t.Fatal(err)
	}
	c.store = nodeReadFailingStore{st}
	c.reconcile(ctx)
	if inc := incidentState(t, st, id); inc.State != types.StateAwaitingApproval || inc.StepIndex != 0 {
		t.Fatalf("unreadable pause: state=%s step=%d, want the step held", inc.State, inc.StepIndex)
	}
	c.store = st
	c.reconcile(ctx)
	if inc := incidentState(t, st, id); inc.State == types.StateAwaitingApproval {
		t.Fatalf("readable again: state=%s, want the step started", inc.State)
	}
}

// A12: a node missing from the inventory has no pause to honor.
func TestNodeMissingFromInventoryIsNotHeld(t *testing.T) {
	c, _, _ := approvalEpochFixture(t)
	if c.pauseHolds(context.Background(), &types.Incident{ID: "x", Target: types.Target{Node: "not-in-inventory"}}) {
		t.Fatal("a node absent from the inventory must not be held")
	}
	if _, err := c.store.GetNode(context.Background(), "not-in-inventory"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("precondition: GetNode = %v, want ErrNotFound", err)
	}
}

func TestMissingParkedStepPreservesDecisionAndTTLOrder(t *testing.T) {
	c, st, id := approvalEpochFixture(t)
	ctx := context.Background()
	inc := incidentState(t, st, id)
	inc.Playbook = "removed"
	if err := st.UpdateIncident(ctx, inc); err != nil {
		t.Fatal(err)
	}
	if err := c.advanceAwaitingApproval(ctx, inc); err != nil {
		t.Fatal(err)
	}
	if got := incidentState(t, st, id); got.State != types.StateAwaitingApproval {
		t.Fatalf("state=%s", got.State)
	}
	backdatePark(t, c, st, id)
	inc = incidentState(t, st, id)
	if err := c.advanceAwaitingApproval(ctx, inc); err != nil {
		t.Fatal(err)
	}
	if got := incidentState(t, st, id); got.State != types.StateExpired {
		t.Fatalf("state=%s", got.State)
	}
}
