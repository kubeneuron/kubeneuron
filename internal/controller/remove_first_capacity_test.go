// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/kubeneuron/kubeneuron/internal/cloud/removefirst"
	"github.com/kubeneuron/kubeneuron/internal/store"
	"github.com/kubeneuron/kubeneuron/internal/store/sqlite"
	"github.com/kubeneuron/kubeneuron/pkg/cloud"
	"github.com/kubeneuron/kubeneuron/pkg/types"
)

// growPlatform answers reads of a recorded grow with reads in turn (the last
// repeats), and of the delete with deleteState.
type growPlatform struct {
	*plannedPlatform
	reads       []removefirst.GrowObservation
	readErr     error
	growReads   int
	deleteState removefirst.Observation
	settle      removefirst.SettleResult
	settleErr   error
	settles     int
}

func (p *growPlatform) ObservePinnedGrow(context.Context, removefirst.Grow) (removefirst.GrowObservation, error) {
	i := min(p.growReads, len(p.reads)-1)
	p.growReads++
	return p.reads[i], p.readErr
}

func (p *growPlatform) ObservePinnedDelete(context.Context, cloud.NodeIdentity, string, string) (removefirst.Observation, error) {
	return p.deleteState, nil
}

func (p *growPlatform) SettlePinnedPool(context.Context, string, string) (removefirst.SettleResult, error) {
	p.settles++
	return p.settle, p.settleErr
}

var testGrow = removefirst.Grow{Pool: "gpu", Machine: "machine-old", Before: 2, Target: 3, Marker: "machine-old:2"}

// growing opens an incident on n1 with a recorded grow, started age ago.
func growing(t *testing.T, p *growPlatform, age time.Duration) (*Controller, *sqlite.Store, string) {
	t.Helper()
	c, st := plannedFixture(t, p.plannedPlatform)
	c.platform = p
	id := openOn(t, c, "n1")
	inc := mustIncident(t, st, id)
	inc.State = types.StateExecuting
	if err := st.UpdateIncident(t.Context(), inc); err != nil {
		t.Fatal(err)
	}
	inc = mustIncident(t, st, id)
	if err := c.claimGrowIntent(t.Context(), inc, *inc.CloudIdentity, testGrow); err != nil {
		t.Fatal(err)
	}
	if age > 0 {
		inc = mustIncident(t, st, id)
		inc.RemoveFirst.PlannedAt = time.Now().Add(-age)
		if err := st.UpdateIncident(t.Context(), inc); err != nil {
			t.Fatal(err)
		}
	}
	return restartWith(t, c, st, p), st, id
}

// The grow is recorded before it is sent and the delete continues the same
// record: one open plan, one start time, one audit row each.
func TestDeleteIntentContinuesTheRecordedGrow(t *testing.T) {
	p := &growPlatform{plannedPlatform: newPlannedPlatform()}
	c, st := plannedFixture(t, p.plannedPlatform)
	id := openOn(t, c, "n1")
	inc := mustIncident(t, st, id)
	pin := *inc.CloudIdentity
	if err := c.claimGrowIntent(t.Context(), inc, pin, testGrow); err != nil {
		t.Fatal(err)
	}
	started := inc.RemoveFirst.PlannedAt
	if err := c.claimDeleteIntent(t.Context(), inc, pin, "gpu", "machine-old", "azure:///vm/next"); err != nil {
		t.Fatal(err)
	}
	got := mustIncident(t, st, id).RemoveFirst
	if !got.Open() || got.Phase != cloud.RemoveFirstDeleting || !got.PlannedAt.Equal(started) ||
		got.State[stateMarker] != testGrow.Marker || got.State[stateCountTarget] != "3" {
		t.Fatalf("record = %+v", got)
	}
	if err := c.claimDeleteIntent(t.Context(), inc, pin, "gpu", "machine-old", ""); err == nil {
		t.Fatal("accepted a second delete intent after the grow")
	}
	actions, _ := trailActions(t, st, id)
	if n := strings.Count(strings.Join(actions, " "), growIntentAction); n != 1 {
		t.Fatalf("grow intents = %d: %v", n, actions)
	}
}

// A pool that does not show the grow yet may still get it: the request may
// have been on its way. Only after the delay is it taken as never sent.
func TestUnseenGrowIsWaitedForBeforeNotStarted(t *testing.T) {
	p := &growPlatform{plannedPlatform: newPlannedPlatform(), reads: []removefirst.GrowObservation{
		{State: removefirst.GrowNotApplied, Size: 2},
		{State: removefirst.GrowApplied, Size: 3},
	}}
	r, st, id := growing(t, p, 0)
	r.sweepRemoveFirst(t.Context())
	if rec := mustIncident(t, st, id).RemoveFirst; !rec.Open() || rec.Phase != removefirst.PhaseGrowing {
		t.Fatalf("closed a grow that may still arrive: %+v", rec)
	}
	r.sweepRemoveFirst(t.Context())
	if rec := mustIncident(t, st, id).RemoveFirst; !rec.Open() || rec.Phase != cloud.RemoveFirstDeleting {
		t.Fatalf("a grow that arrived late did not move on to the delete: %+v", rec)
	}
	if p.resizes["gpu"] != 0 || len(p.deletes) != 0 {
		t.Fatalf("the sweep changed the cloud: resizes=%v deletes=%v", p.resizes, p.deletes)
	}
}

func TestGrowNeverSentEndsNotStartedAfterTheDelay(t *testing.T) {
	p := &growPlatform{plannedPlatform: newPlannedPlatform(), reads: []removefirst.GrowObservation{{State: removefirst.GrowNotApplied, Size: 2}}}
	r, st, id := growing(t, p, growSettleDelay+time.Second)
	r.sweepRemoveFirst(t.Context())
	got := mustIncident(t, st, id)
	if got.RemoveFirst.Outcome != types.RemoveFirstNotStarted || got.RemoveFirst.Reason != "Pool gpu was not resized and still has 2 nodes. Nothing was changed." {
		t.Fatalf("record = %+v", got.RemoveFirst)
	}
	if got.State == types.StateNeedsHuman {
		t.Fatal("an unchanged pool was handed to an operator")
	}
}

func TestGrowThatDoesNotMatchThePoolNeedsAnOperator(t *testing.T) {
	for _, tc := range []struct {
		name string
		obs  removefirst.GrowObservation
		want string
	}{
		{"at the grown size", removefirst.GrowObservation{State: removefirst.GrowChanged, Size: 3, Detail: "it is marked for another replacement"},
			"Pool gpu now has 3 nodes; it had 2 before the replacement. Machine machine-old was not removed."},
		{"other size", removefirst.GrowObservation{State: removefirst.GrowChanged, Size: 5, Detail: "it has 5 nodes"},
			"Pool gpu now has 5 nodes; it had 2 before the replacement. Machine machine-old was not removed."},
		{"unread size", removefirst.GrowObservation{State: removefirst.GrowChanged, Detail: "its last update failed"},
			"Check its current size."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := &growPlatform{plannedPlatform: newPlannedPlatform(), reads: []removefirst.GrowObservation{tc.obs}}
			r, st, id := growing(t, p, 0)
			r.sweepRemoveFirst(t.Context())
			got := mustIncident(t, st, id)
			if got.State != types.StateNeedsHuman || got.RemoveFirst.Outcome != types.RemoveFirstNeedsOperator ||
				!strings.Contains(got.RemoveFirst.Reason, "("+tc.obs.Detail+"), so it was not repeated."+" "+tc.want) ||
				!strings.Contains(got.RemoveFirst.Reason, "kubeneuronctl resolve "+id+" --cloud-operation-settled") {
				t.Fatalf("state=%s record=%+v", got.State, got.RemoveFirst)
			}
			if p.resizes["gpu"] != 0 {
				t.Fatal("grew the pool again")
			}
		})
	}
}

func TestUnreadableGrowExpiresAfterTheDeadline(t *testing.T) {
	p := &growPlatform{plannedPlatform: newPlannedPlatform(), reads: []removefirst.GrowObservation{{}}}
	r, st, id := growing(t, p, 0)
	r.sweepRemoveFirst(t.Context())
	if !mustIncident(t, st, id).RemoveFirst.Open() {
		t.Fatal("expired before the deadline")
	}
	inc := mustIncident(t, st, id)
	inc.RemoveFirst.PlannedAt = time.Now().Add(-removeFirstDeadline - time.Second)
	if err := st.UpdateIncident(t.Context(), inc); err != nil {
		t.Fatal(err)
	}
	r.sweepRemoveFirst(t.Context())
	got := mustIncident(t, st, id)
	if got.State != types.StateNeedsHuman || !strings.Contains(got.RemoveFirst.Reason,
		"The resize of pool gpu to replace machine machine-old was started more than an hour ago, and KubeNeuron could not confirm that it completed. Check its current size.") {
		t.Fatalf("state=%s record=%+v", got.State, got.RemoveFirst)
	}
}

// A grow the cloud refused outright frees the pool at once, but only when a
// read shows it unchanged.
func TestRefusedGrowClosesOnlyOnAnUnchangedPool(t *testing.T) {
	for _, tc := range []struct {
		obs  removefirst.GrowState
		open bool
	}{{removefirst.GrowNotApplied, false}, {removefirst.GrowUnknown, true}, {removefirst.GrowApplied, true}} {
		p := &growPlatform{plannedPlatform: newPlannedPlatform(), reads: []removefirst.GrowObservation{{State: tc.obs}}}
		r, st, id := growing(t, p, 0)
		inc := mustIncident(t, st, id)
		if err := r.settleRefusedGrow(t.Context(), inc); err != nil {
			t.Fatal(err)
		}
		got := mustIncident(t, st, id).RemoveFirst
		if got.Open() != tc.open {
			t.Fatalf("%q: record = %+v", tc.obs, got)
		}
		if !tc.open && got.Reason != "The cloud provider refused to resize pool gpu to replace machine machine-old. Nothing was changed." {
			t.Fatal(got.Reason)
		}
	}
	// An unread pool after a refusal is not said to be unchanged.
	p := &growPlatform{plannedPlatform: newPlannedPlatform(), reads: []removefirst.GrowObservation{{}}, readErr: errors.New("timeout")}
	r, st, id := growing(t, p, 0)
	if err := r.settleRefusedGrow(t.Context(), mustIncident(t, st, id)); err != nil {
		t.Fatal(err)
	}
	got := mustIncident(t, st, id)
	if got.State != types.StateNeedsHuman || !strings.HasPrefix(got.RemoveFirst.Reason, "The cloud provider refused to resize pool gpu to replace machine machine-old. Check its current size. Check the machine") {
		t.Fatalf("state=%s record=%+v", got.State, got.RemoveFirst)
	}
}

// A delete confirmed after a restart finishes the pool grown for it.
func TestConfirmedDeleteSettlesTheGrownPool(t *testing.T) {
	t.Run("back to size", func(t *testing.T) {
		p := &growPlatform{plannedPlatform: newPlannedPlatform(), deleteState: removefirst.Deleted,
			settle: removefirst.SettleResult{Size: 2, Before: 2, Untagged: true}}
		r, st, id := growing(t, p, 0)
		inc := mustIncident(t, st, id)
		if err := r.claimDeleteIntent(t.Context(), inc, *inc.CloudIdentity, "gpu", "machine-old", ""); err != nil {
			t.Fatal(err)
		}
		r.sweepRemoveFirst(t.Context())
		// The tag is written even while paused, so the audit says so.
		if got := mustIncident(t, st, id).RemoveFirst; got.Outcome != types.RemoveFirstDeleteConfirmed || p.settles != 1 ||
			got.Reason != "The kubeneuron-replacing tag was removed from pool gpu, which is back to 2 nodes." {
			t.Fatalf("settles=%d record=%+v", p.settles, got)
		}
	})
	t.Run("one node more", func(t *testing.T) {
		p := &growPlatform{plannedPlatform: newPlannedPlatform(), deleteState: removefirst.Deleted,
			settle: removefirst.SettleResult{Size: 3, Before: 2, Extra: true}, settleErr: cloud.ErrNeedsOperator}
		r, st, id := growing(t, p, 0)
		inc := mustIncident(t, st, id)
		if err := r.claimDeleteIntent(t.Context(), inc, *inc.CloudIdentity, "gpu", "machine-old", ""); err != nil {
			t.Fatal(err)
		}
		r.sweepRemoveFirst(t.Context())
		got := mustIncident(t, st, id)
		if got.State != types.StateNeedsHuman || !strings.HasPrefix(got.RemoveFirst.Reason,
			"Machine machine-old was removed from pool gpu. Pool gpu was resized to 3 before the removal and now has one node more than planned.") {
			t.Fatalf("state=%s record=%+v", got.State, got.RemoveFirst)
		}
	})
}

// capacityGrowPlatform is a fixed-size pool whose grow the cloud accepts and
// then fails for lack of capacity.
type capacityGrowPlatform struct {
	*growPlatform
	grows int
}

func (p *capacityGrowPlatform) PlanRemoveFirst(context.Context, cloud.NodeIdentity, string) (cloud.RemoveFirstPlan, error) {
	return cloud.RemoveFirstPlan{}, cloud.ErrRemoveFirstUnplanned
}

func (p *capacityGrowPlatform) SupportsDeleteIntent() bool { return true }

func (p *capacityGrowPlatform) ReplacePinnedNodeWithIntent(context.Context, cloud.NodeIdentity, func(string, string, string) error) (string, error) {
	return "", errors.New("grows go through the intents")
}

func (p *capacityGrowPlatform) ReplacePinnedNodeWithIntents(_ context.Context, _ cloud.NodeIdentity, intents removefirst.Intents) (string, error) {
	if err := intents.Grow(testGrow); err != nil {
		return "", err
	}
	p.grows++
	return "", fmt.Errorf("azure: growing agent pool gpu to replace machine-old: %w: AllocationFailed", cloud.ErrReplacementCapacity)
}

// A grow the cloud accepted and then failed for lack of capacity fails the
// step as any capacity shortage does. A pool read unchanged is freed at
// once; a pool left changed is reported as that failure, not as a grow
// whose outcome was lost.
func TestAcceptedGrowThatRanOutOfCapacity(t *testing.T) {
	for _, tc := range []struct {
		name  string
		obs   removefirst.GrowObservation
		human bool
		want  string
	}{
		{"unchanged", removefirst.GrowObservation{State: removefirst.GrowNotApplied, Size: 2}, false,
			"The resize of pool gpu to replace machine machine-old failed in the cloud (no capacity or quota for this machine type). The cloud could not add a machine of this type; its answer does not tell capacity from quota. Check the account's quota for this machine type first, then the provider's status for the zone. Nothing was changed."},
		{"left failed", removefirst.GrowObservation{State: removefirst.GrowChanged, Detail: "its last update failed"}, true,
			"The resize of pool gpu to replace machine machine-old failed in the cloud (no capacity or quota for this machine type). The cloud could not add a machine of this type; its answer does not tell capacity from quota. Check the account's quota for this machine type first, then the provider's status for the zone. Machine machine-old was not removed. Check its current size. Check the machine"},
		{"left grown", removefirst.GrowObservation{State: removefirst.GrowChanged, Size: 3, Detail: "it has 3 nodes"}, true,
			"The resize of pool gpu to replace machine machine-old failed in the cloud (no capacity or quota for this machine type). The cloud could not add a machine of this type; its answer does not tell capacity from quota. Check the account's quota for this machine type first, then the provider's status for the zone. Machine machine-old was not removed. Pool gpu now has 3 nodes; it had 2 before the replacement. Check the machine"},
		{"left at its size", removefirst.GrowObservation{State: removefirst.GrowChanged, Size: 2, Detail: "it is now autoscaled"}, true,
			"The resize of pool gpu to replace machine machine-old failed in the cloud (no capacity or quota for this machine type). The cloud could not add a machine of this type; its answer does not tell capacity from quota. Check the account's quota for this machine type first, then the provider's status for the zone. Machine machine-old was not removed. Pool gpu still has 2 nodes. Check the machine"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := &capacityGrowPlatform{growPlatform: &growPlatform{plannedPlatform: newPlannedPlatform(), reads: []removefirst.GrowObservation{tc.obs}}}
			c, st := plannedFixture(t, p.plannedPlatform)
			c.platform = p
			id := openOn(t, c, "n1")
			_, err := c.recycleNodeStep(t.Context(), mustIncident(t, st, id), true)
			if !errors.Is(err, cloud.ErrReplacementCapacity) || !strings.Contains(err.Error(), "replacement capacity unavailable") {
				t.Fatalf("err = %v, want the capacity reason", err)
			}
			if got := mustIncident(t, st, id); got.State == types.StateNeedsHuman {
				t.Fatal("the step handed the capacity shortage to an operator")
			}
			c.sweepRemoveFirst(t.Context())
			got := mustIncident(t, st, id)
			if got.State == types.StateNeedsHuman != tc.human || !strings.HasPrefix(got.RemoveFirst.Reason, tc.want) {
				t.Fatalf("state=%s record=%+v", got.State, got.RemoveFirst)
			}
			if p.grows != 1 || p.resizes["gpu"] != 0 {
				t.Fatalf("grows=%d resizes=%v", p.grows, p.resizes)
			}
		})
	}
}

// lostGrowPlatform answers a repeated delete by deleting the machine.
type lostGrowPlatform struct {
	*growPlatform
	deleteErr error
	retries   int
}

func (p *lostGrowPlatform) ObservePinnedDelete(context.Context, cloud.NodeIdentity, string, string) (removefirst.Observation, error) {
	return p.deleteState, p.deleteErr
}

func (p *lostGrowPlatform) RetryPinnedDelete(context.Context, cloud.NodeIdentity, string, string, removefirst.Observation) error {
	p.retries++
	p.deleteState = removefirst.Deleted
	return nil
}

// lostGrow stops a controller after the grow of the replacement step was
// sent and before its delete, and starts another one with its log in out.
func lostGrow(t *testing.T, p *lostGrowPlatform, out *bytes.Buffer) (*Controller, *sqlite.Store, string) {
	t.Helper()
	c, st := plannedFixture(t, p.plannedPlatform)
	c.platform = p
	id := openOn(t, c, "n1")
	ctx := t.Context()
	inc := mustIncident(t, st, id)
	inc.State, inc.StepIndex, inc.Playbook = types.StateExecuting, 2, "cloud-replace"
	if err := st.UpdateIncident(ctx, inc); err != nil {
		t.Fatal(err)
	}
	inc = mustIncident(t, st, id)
	pin := *inc.CloudIdentity
	if err := st.WithTx(ctx, func(tx store.Tx) error {
		_, err := tx.ClaimNodeRemediation(ctx, pin.Node, pin.NodeUID, id, time.Now())
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := c.claimGrowIntent(ctx, inc, pin, testGrow); err != nil {
		t.Fatal(err)
	}
	r := restartWith(t, c, st, p)
	r.log = slog.New(slog.NewTextHandler(out, nil))
	return r, st, id
}

// The grow's answer was lost: the new controller finds the grow and sends
// the delete once.
func TestRestartAfterTheGrowSendsTheDelete(t *testing.T) {
	p := &lostGrowPlatform{growPlatform: &growPlatform{plannedPlatform: newPlannedPlatform(),
		reads: []removefirst.GrowObservation{{State: removefirst.GrowApplied, Size: 3}}, deleteState: removefirst.Listed}}
	var out bytes.Buffer
	r, st, id := lostGrow(t, p, &out)
	for range 3 {
		r.reconcile(t.Context())
		time.Sleep(5 * time.Millisecond)
	}
	got := mustIncident(t, st, id)
	if p.retries != 1 || got.State != types.StateVerifying || got.RemoveFirst.Outcome != types.RemoveFirstDeleteConfirmed {
		t.Fatalf("retries=%d state=%s %s", p.retries, got.State, dump(trail(t, st, id)))
	}
}

// A delete the sweep does not send says why in the controller log, once for
// each reason.
func TestUnsentDeleteIsLoggedOncePerReason(t *testing.T) {
	p := &lostGrowPlatform{growPlatform: &growPlatform{plannedPlatform: newPlannedPlatform(),
		reads: []removefirst.GrowObservation{{State: removefirst.GrowApplied, Size: 3}}, deleteState: removefirst.Listed},
		deleteErr: errors.New("node n1 not found")}
	var out bytes.Buffer
	r, _, _ := lostGrow(t, p, &out)
	ctx := t.Context()
	for range 3 {
		r.sweepRemoveFirst(ctx)
	}
	p.deleteErr = nil
	r.gate.Pause()
	for range 3 {
		r.sweepRemoveFirst(ctx)
	}
	log := out.String()
	if p.retries != 0 || strings.Count(log, "node n1 not found") != 1 || strings.Count(log, "remove-first repeat not sent") != 1 ||
		!strings.Contains(log, "reason=pause") {
		t.Fatalf("retries=%d log:\n%s", p.retries, log)
	}
}
