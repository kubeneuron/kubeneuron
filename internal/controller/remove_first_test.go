// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kubeneuron/kubeneuron/internal/cloud/removefirst"
	"github.com/kubeneuron/kubeneuron/internal/platform"
	"github.com/kubeneuron/kubeneuron/internal/recoverymode"
	"github.com/kubeneuron/kubeneuron/internal/safety"
	"github.com/kubeneuron/kubeneuron/internal/store"
	"github.com/kubeneuron/kubeneuron/internal/store/sqlite"
	"github.com/kubeneuron/kubeneuron/pkg/cloud"
	"github.com/kubeneuron/kubeneuron/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
)

// plannedPlatform is a cloud platform on one fixed-size pool whose size the
// provider sets back after a delete, through a plan.
type plannedPlatform struct {
	*successorPlatform

	pmu    sync.Mutex
	before int
	size   int
	gone   map[string]bool
	// hold keeps a plan in its phase: planned, or deleting once the
	// instance is gone.
	holdPlanned, holdResize bool
	// loseDelete / loseResize fail the answer of the next call, which is
	// made; needsOperator fails the next resize for good.
	loseDelete, loseResize, needsOperator bool
	deletes, resizes                      map[string]int
	// atDelete runs before each delete is sent.
	atDelete func(op string)
}

func newPlannedPlatform() *plannedPlatform {
	return &plannedPlatform{successorPlatform: newSuccessorPlatform(), before: 4, size: 4, gone: map[string]bool{},
		deletes: map[string]int{}, resizes: map[string]int{}}
}

func (p *plannedPlatform) PlanRemoveFirst(_ context.Context, pin cloud.NodeIdentity, op string) (cloud.RemoveFirstPlan, error) {
	p.pmu.Lock()
	defer p.pmu.Unlock()
	return cloud.RemoveFirstPlan{Operation: op, Instance: pin.InstanceID, Pool: "gpu", Phase: cloud.RemoveFirstPlanned, PlannedAt: time.Now(),
		Restore: fmt.Sprintf("resize gpu --num-nodes %d", p.size), State: map[string]string{"size": fmt.Sprint(p.size)}}, nil
}

func (p *plannedPlatform) ContinueRemoveFirst(_ context.Context, _ cloud.NodeIdentity, plan cloud.RemoveFirstPlan) (cloud.RemoveFirstPlan, error) {
	p.pmu.Lock()
	switch plan.Phase {
	case cloud.RemoveFirstPlanned:
		if p.holdPlanned {
			p.pmu.Unlock()
			return plan, nil
		}
		plan.Phase = cloud.RemoveFirstDeleting
		p.pmu.Unlock()
		return plan, nil
	case cloud.RemoveFirstDone:
		p.pmu.Unlock()
		return plan, nil
	}
	if !p.gone[plan.Instance] {
		at := p.atDelete
		p.pmu.Unlock()
		if at != nil {
			at(plan.Operation)
		}
		p.pmu.Lock()
		defer p.pmu.Unlock()
		p.deletes[plan.Operation]++
		p.gone[plan.Instance] = true
		p.size--
		if p.loseDelete {
			p.loseDelete = false
			return plan, errors.New("context deadline exceeded")
		}
		return plan, nil
	}
	defer p.pmu.Unlock()
	if p.size >= p.before {
		plan.Phase = cloud.RemoveFirstDone
		return plan, nil
	}
	if p.holdResize && plan.Phase == cloud.RemoveFirstResizing {
		return plan, nil
	}
	if p.needsOperator {
		return plan, fmt.Errorf("pool changed; to set the size back: %s: %w", plan.Restore, cloud.ErrNeedsOperator)
	}
	plan.Phase = cloud.RemoveFirstResizing
	p.resizes[plan.Operation]++
	if !p.holdResize {
		p.size = p.before
	}
	if p.loseResize {
		p.loseResize = false
		return plan, errors.New("context deadline exceeded")
	}
	plan.State = map[string]string{"size": plan.State["size"], "operation": "op-" + fmt.Sprint(p.resizes[plan.Operation])}
	return plan, nil
}

func (p *plannedPlatform) set(f func(*plannedPlatform)) {
	p.pmu.Lock()
	defer p.pmu.Unlock()
	f(p)
}

func (p *plannedPlatform) calls(op string) (deletes, resizes int) {
	p.pmu.Lock()
	defer p.pmu.Unlock()
	return p.deletes[op], p.resizes[op]
}

func plannedFixture(t *testing.T, p *plannedPlatform) (*Controller, *sqlite.Store) {
	t.Helper()
	c, st, _ := successorFixture(t, recoverymode.Automatic, p.successorPlatform, recoverymode.RemoveFirst)
	c.platform = p
	return c, st
}

// restartWith is a new controller process on the same store and platform.
func restartWith(t *testing.T, c *Controller, st *sqlite.Store, p platform.Platform) *Controller {
	t.Helper()
	live := safety.Limits{MaxConcurrentRemediations: 8, MaxConcurrentReboots: 8}
	r := New(st, st, nil, safety.NewGate(live), safety.NewFlapDetector(100, time.Hour), p, nil, &recordingNotifier{},
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := r.InstallRuntimeConfig(RuntimeConfig{SafetyLimits: &live}); err != nil {
		t.Fatal(err)
	}
	r.SetEngine(c.runtimeConfig(context.Background()).Engine)
	qualifyAutoProfile(t, r)
	r.SetTimings(time.Millisecond, 0)
	r.successorPoll = time.Millisecond
	return r
}

// crashWhen runs reconcile passes until cond holds, then ends the process:
// its context is cancelled and its step goroutines stop.
func crashWhen(t *testing.T, c *Controller, st *sqlite.Store, id string, cond func(*types.Incident) bool) {
	t.Helper()
	ctx, crash := context.WithCancel(context.Background())
	deadline := time.Now().Add(10 * time.Second)
	for !cond(mustIncident(t, st, id)) {
		if time.Now().After(deadline) {
			t.Fatalf("condition not reached%s", dump(trail(t, st, id)))
		}
		c.reconcile(ctx)
		time.Sleep(2 * time.Millisecond)
	}
	crash()
	for c.isInFlight(id) {
		time.Sleep(time.Millisecond)
	}
}

func planPhase(phase cloud.RemoveFirstPhase) func(*types.Incident) bool {
	return func(inc *types.Incident) bool { return inc.RemoveFirst.Open() && inc.RemoveFirst.Phase == phase }
}

func planEnded(inc *types.Incident) bool { return inc.RemoveFirst != nil && !inc.RemoveFirst.Open() }

// The plan is on the incident before the delete is sent; the delete and the
// resize are made once, the plan ends restored, and the plain delete-only
// replacement is not used.
func TestRemoveFirstPlanRecordedBeforeTheDelete(t *testing.T) {
	p := newPlannedPlatform()
	c, st := plannedFixture(t, p)
	id := openOn(t, c, "n1")
	var recorded []cloud.RemoveFirstPhase
	p.set(func(p *plannedPlatform) {
		p.atDelete = func(string) {
			if inc := mustIncident(t, st, id); inc.RemoveFirst != nil {
				recorded = append(recorded, inc.RemoveFirst.Phase)
			}
		}
	})
	inc := waitSettled(t, c, st, id, planEnded)
	op := inc.RemoveFirst.Operation
	if deletes, resizes := p.calls(op); deletes != 1 || resizes != 1 || len(recorded) != 1 || recorded[0] != cloud.RemoveFirstDeleting {
		t.Fatalf("deletes=%d resizes=%d recorded at the delete %v", deletes, resizes, recorded)
	}
	if inc.RemoveFirst.Outcome != types.RemoveFirstRestored || p.size != 4 {
		t.Fatalf("plan %+v size %d", inc.RemoveFirst, p.size)
	}
	if _, _, _, plain, _ := p.counts(); plain != 0 {
		t.Fatalf("plain replace %d", plain)
	}
	if !auditHas(trail(t, st, id), "remove-first-planned", "planned operation") {
		t.Fatalf("no plan in the trail%s", dump(trail(t, st, id)))
	}
}

// A controller that stops in any phase is followed by one that finishes the
// plan from the record: nothing is deleted or resized twice. A plan that
// stopped at planned reached nothing; it is ended and made again.
func TestRemoveFirstRestartInEveryPhase(t *testing.T) {
	for _, tc := range []struct {
		phase cloud.RemoveFirstPhase
		hold  func(*plannedPlatform, bool)
	}{
		{cloud.RemoveFirstPlanned, func(p *plannedPlatform, on bool) { p.holdPlanned = on }},
		{cloud.RemoveFirstResizing, func(p *plannedPlatform, on bool) { p.holdResize = on }},
	} {
		t.Run(string(tc.phase), func(t *testing.T) {
			p := newPlannedPlatform()
			c, st := plannedFixture(t, p)
			id := openOn(t, c, "n1")
			p.set(func(p *plannedPlatform) { tc.hold(p, true) })
			crashWhen(t, c, st, id, planPhase(tc.phase))
			p.set(func(p *plannedPlatform) {
				tc.hold(p, false)
				if tc.phase == cloud.RemoveFirstResizing {
					p.size = p.before - 1 // the held resize never ran
				}
			})
			r := restartWith(t, c, st, p)
			inc := waitSettled(t, r, st, id, func(inc *types.Incident) bool {
				return inc.RemoveFirst.Outcome == types.RemoveFirstRestored
			})
			deletes, resizes := p.calls(inc.RemoveFirst.Operation)
			want := 1
			if tc.phase == cloud.RemoveFirstResizing {
				want = 2 // the held one, then the one sent again
			}
			if deletes != 1 || resizes != want || p.size != p.before {
				t.Fatalf("deletes=%d resizes=%d size=%d%s", deletes, resizes, p.size, dump(trail(t, st, id)))
			}
			if tc.phase == cloud.RemoveFirstPlanned && !auditHas(trail(t, st, id), "remove-first-not-started", "nothing was changed") {
				t.Fatalf("the unstarted plan was not ended%s", dump(trail(t, st, id)))
			}
		})
	}
}

// A controller that stops while the delete is in flight, its answer lost,
// continues from deleting: the instance gone, it only sets the size back.
func TestRemoveFirstRestartAfterALostDeleteAnswer(t *testing.T) {
	p := newPlannedPlatform()
	c, st := plannedFixture(t, p)
	id := openOn(t, c, "n1")
	p.set(func(p *plannedPlatform) { p.loseDelete, p.holdResize = true, true })
	crashWhen(t, c, st, id, func(*types.Incident) bool {
		p.pmu.Lock()
		defer p.pmu.Unlock()
		return len(p.deletes) == 1
	})
	p.set(func(p *plannedPlatform) { p.holdResize = false })
	r := restartWith(t, c, st, p)
	inc := waitSettled(t, r, st, id, func(inc *types.Incident) bool { return inc.RemoveFirst.Outcome == types.RemoveFirstRestored })
	if deletes, _ := p.calls(inc.RemoveFirst.Operation); deletes != 1 || p.size != p.before {
		t.Fatalf("deletes=%d size=%d", deletes, p.size)
	}
}

// A resize whose answer is lost is continued, not given up: the plan ends
// restored.
func TestRemoveFirstLostResizeAnswer(t *testing.T) {
	p := newPlannedPlatform()
	p.loseResize = true
	c, st := plannedFixture(t, p)
	id := openOn(t, c, "n1")
	inc := waitSettled(t, c, st, id, planEnded)
	if inc.RemoveFirst.Outcome != types.RemoveFirstRestored || p.size != p.before {
		t.Fatalf("plan %+v size %d", inc.RemoveFirst, p.size)
	}
}

// A pool that cannot be set back ends the plan for an operator, with the
// command that sets the size back.
func TestRemoveFirstNeedsOperatorKeepsTheCommand(t *testing.T) {
	p := newPlannedPlatform()
	p.needsOperator = true
	c, st := plannedFixture(t, p)
	id := openOn(t, c, "n1")
	inc := waitSettled(t, c, st, id, planEnded)
	if inc.RemoveFirst.Outcome != types.RemoveFirstNeedsOperator || !strings.Contains(inc.RemoveFirst.Reason, "resize gpu --num-nodes 4") {
		t.Fatalf("plan %+v", inc.RemoveFirst)
	}
	if !auditHas(trail(t, st, id), "remove-first-needs-operator", "resize gpu --num-nodes 4") {
		t.Fatalf("trail%s", dump(trail(t, st, id)))
	}
}

// A second incident on the same pool waits, with nothing planned or
// deleted, until the first plan has set the pool back.
func TestRemoveFirstOnePlanPerPool(t *testing.T) {
	p := newPlannedPlatform()
	c, st := plannedFixture(t, p)
	first := openOn(t, c, "n1")
	p.set(func(p *plannedPlatform) { p.holdResize = true })
	waitSettled(t, c, st, first, planPhase(cloud.RemoveFirstResizing))
	second := openOn(t, c, "n2")
	// Two nodes with one class share the fault: the second is approved.
	approved := func(inc *types.Incident) bool {
		if inc.State == types.StateAwaitingApproval {
			if err := c.DecideApproval(context.Background(), second, "alice", "cli", types.ApprovalApproved, inc.ApprovalEpoch, "", ""); err != nil {
				t.Fatal(err)
			}
		}
		return false
	}
	deadline := time.Now().Add(300 * time.Millisecond)
	for time.Now().Before(deadline) {
		approved(mustIncident(t, st, second))
		c.reconcile(context.Background())
		time.Sleep(2 * time.Millisecond)
	}
	if inc := mustIncident(t, st, second); inc.State != types.StateExecuting {
		t.Fatalf("the second incident is %s, want waiting in its replace step%s", inc.State, dump(trail(t, st, second)))
	}
	if inc := mustIncident(t, st, second); inc.RemoveFirst != nil {
		t.Fatalf("a second plan on the pool: %+v%s", inc.RemoveFirst, dump(trail(t, st, second)))
	}
	if deletes, _ := p.calls(successorOperation(mustIncident(t, st, second))); deletes != 0 {
		t.Fatalf("the second node was deleted while the pool was short")
	}
	p.set(func(p *plannedPlatform) { p.holdResize = false; p.size = p.before - 1 })
	waitSettled(t, c, st, first, planEnded)
	inc := waitSettled(t, c, st, second, func(inc *types.Incident) bool { approved(inc); return planEnded(inc) })
	if inc.RemoveFirst.Outcome != types.RemoveFirstRestored || p.size != p.before {
		t.Fatalf("second plan %+v size %d", inc.RemoveFirst, p.size)
	}
}

// A plan that stops making progress past the delete, with no error (another
// GKE operation that never ends, a delete that never finishes), is left for
// an operator at the deadline, with the command.
func TestRemoveFirstDeadlineWithoutProgress(t *testing.T) {
	defer func(d time.Duration) { removeFirstDeadline = d }(removeFirstDeadline)
	removeFirstDeadline = 50 * time.Millisecond
	p := newPlannedPlatform()
	p.holdResize = true
	c, st := plannedFixture(t, p)
	id := openOn(t, c, "n1")
	inc := waitSettled(t, c, st, id, planEnded)
	if inc.RemoveFirst.Outcome != types.RemoveFirstNeedsOperator || !strings.Contains(inc.RemoveFirst.Reason, "no progress") ||
		!strings.Contains(inc.RemoveFirst.Reason, "resize gpu --num-nodes 4") {
		t.Fatalf("plan %+v", inc.RemoveFirst)
	}
}

// An open plan the reconcile pass cannot continue, for want of the pinned
// identity, is left for an operator, not skipped for good.
func TestRemoveFirstSweepWithoutIdentity(t *testing.T) {
	p := newPlannedPlatform()
	c, st := plannedFixture(t, p)
	ctx := context.Background()
	now := time.Now()
	inc := &types.Incident{ID: "rf-orphan", Target: types.Target{Node: "n3"}, Class: types.ClassFellOffBus, State: types.StateResolved,
		OpenedAt: now, UpdatedAt: now, StateChangedAt: now,
		RemoveFirst: &types.RemoveFirstRecord{RemoveFirstPlan: cloud.RemoveFirstPlan{Operation: "rf-orphan-a1", Instance: "i-3", Pool: "gpu",
			Phase: cloud.RemoveFirstDeleting, PlannedAt: now, Restore: "resize gpu --num-nodes 4"}}}
	if err := st.CreateIncident(ctx, inc); err != nil {
		t.Fatal(err)
	}
	c.sweepRemoveFirst(ctx)
	got := mustIncident(t, st, inc.ID)
	if got.RemoveFirst.Outcome != types.RemoveFirstNeedsOperator || !strings.Contains(got.RemoveFirst.Reason, "resize gpu --num-nodes 4") {
		t.Fatalf("plan %+v", got.RemoveFirst)
	}
}

// The check for an open plan on the pool is made in the transaction that
// writes the plan: a plan the in-process lock never saw (one a previous
// leader wrote) still keeps a second one out.
func TestRemoveFirstPoolCheckInTheTransaction(t *testing.T) {
	p := newPlannedPlatform()
	c, st := plannedFixture(t, p)
	ctx := context.Background()
	now := time.Now()
	other := &types.Incident{ID: "rf-other", Target: types.Target{Node: "n3"}, Class: types.ClassFellOffBus, State: types.StateExecuting,
		OpenedAt: now, UpdatedAt: now, StateChangedAt: now,
		RemoveFirst: &types.RemoveFirstRecord{RemoveFirstPlan: cloud.RemoveFirstPlan{Operation: "rf-other-a1", Instance: "i-3", Pool: "gpu",
			Phase: cloud.RemoveFirstResizing, PlannedAt: now}}}
	mine := &types.Incident{ID: "rf-mine", Target: types.Target{Node: "n1"}, Class: types.ClassFellOffBus, State: types.StateExecuting,
		OpenedAt: now, UpdatedAt: now, StateChangedAt: now}
	for _, inc := range []*types.Incident{other, mine} {
		if err := st.CreateIncident(ctx, inc); err != nil {
			t.Fatal(err)
		}
	}
	err := c.saveRemoveFirst(ctx, mine, &types.RemoveFirstRecord{RemoveFirstPlan: cloud.RemoveFirstPlan{Operation: "rf-mine-a1", Pool: "gpu",
		Phase: cloud.RemoveFirstPlanned, PlannedAt: now}}, "remove-first-planned", "")
	if !errors.Is(err, errPoolPlanOpen) {
		t.Fatalf("saved a second open plan on the pool: %v", err)
	}
	if got := mustIncident(t, st, mine.ID); got.RemoveFirst != nil {
		t.Fatalf("plan written: %+v", got.RemoveFirst)
	}
}

func TestDeleteIntentCannotBeClaimedTwice(t *testing.T) {
	c, st := plannedFixture(t, newPlannedPlatform())
	id := openOn(t, c, "n1")
	inc := mustIncident(t, st, id)
	pin := *inc.CloudIdentity
	if err := c.claimDeleteIntent(t.Context(), inc, pin, "gpu", "old-machine", "named-successor"); err != nil {
		t.Fatal(err)
	}
	if err := c.claimDeleteIntent(t.Context(), inc, pin, "gpu", "other-machine", ""); err == nil {
		t.Fatal("accepted a second delete intent")
	}
	got := mustIncident(t, st, id).RemoveFirst
	if got == nil || got.MachineName != "old-machine" || got.DeleteTarget == nil || *got.DeleteTarget != pin {
		t.Fatalf("lost original delete target: %+v", got)
	}
	count := 0
	for _, e := range trail(t, st, id) {
		if e.Action == deleteIntentAction {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("got %d delete intent audit entries", count)
	}
}

func TestRestartFindsConfirmedDeleteWithoutStepOutcome(t *testing.T) {
	c, st := plannedFixture(t, newPlannedPlatform())
	id := openOn(t, c, "n1")
	inc := mustIncident(t, st, id)
	pin := *inc.CloudIdentity
	ctx := t.Context()
	if err := c.recordReplacementPool(ctx, inc, pin); err != nil {
		t.Fatal(err)
	}
	if err := c.claimDeleteIntent(ctx, inc, pin, "gpu", "old", "azure:///vm/next"); err != nil {
		t.Fatal(err)
	}
	if err := c.endRemoveFirst(ctx, inc, types.RemoveFirstDeleteConfirmed, ""); err != nil {
		t.Fatal(err)
	}
	r := restartWith(t, c, st, c.platform)
	reloaded := mustIncident(t, st, id)
	found, err := r.findCloudReplacement(ctx, reloaded)
	if err != nil || found == nil {
		t.Fatalf("confirmed delete after restart: %v %v", found, err)
	}
	if got := resizeDiffInstance(reloaded, &types.AuditEntry{}, nil); got != "azure:///vm/next" {
		t.Fatalf("successor ID = %q", got)
	}
	reloaded.Attempt++
	found, err = r.findCloudReplacement(ctx, reloaded)
	if err != nil || found != nil {
		t.Fatalf("previous attempt supplied a replacement: %v %v", found, err)
	}
}

type ambiguousMachinePlatform struct{ *plannedPlatform }

func (*ambiguousMachinePlatform) ObservePinnedDelete(context.Context, cloud.NodeIdentity, string, string) (removefirst.Observation, error) {
	return removefirst.IdentityChanged, nil
}

func TestRestartParksReusedAKSMachineName(t *testing.T) {
	p := &ambiguousMachinePlatform{newPlannedPlatform()}
	c, st := plannedFixture(t, p.plannedPlatform)
	id := openOn(t, c, "n1")
	inc := mustIncident(t, st, id)
	inc.State = types.StateExecuting
	inc.CloudIdentity.ProviderID = "azure:///private/subscription/old"
	if err := st.UpdateIncident(t.Context(), inc); err != nil {
		t.Fatal(err)
	}
	inc = mustIncident(t, st, id)
	if err := c.claimDeleteIntent(t.Context(), inc, *inc.CloudIdentity, "gpu", "machine-old", ""); err != nil {
		t.Fatal(err)
	}
	r := restartWith(t, c, st, p)
	r.sweepRemoveFirst(t.Context())
	got := mustIncident(t, st, id)
	if got.State != types.StateNeedsHuman || got.RemoveFirst.Outcome != types.RemoveFirstNeedsOperator {
		t.Fatalf("state=%s outcome=%s", got.State, got.RemoveFirst.Outcome)
	}
	if !strings.Contains(got.RemoveFirst.Reason, "kubeneuronctl resolve "+id+" --cloud-operation-settled") || strings.Contains(got.RemoveFirst.Reason, "subscription") {
		t.Fatal(got.RemoveFirst.Reason)
	}
}

func TestUnknownDeleteExpiresFromOpeningTime(t *testing.T) {
	p := newPlannedPlatform() // No observation available.
	c, st := plannedFixture(t, p)
	id := openOn(t, c, "n1")
	inc := mustIncident(t, st, id)
	inc.State = types.StateExecuting
	if err := st.UpdateIncident(t.Context(), inc); err != nil {
		t.Fatal(err)
	}
	inc = mustIncident(t, st, id)
	if err := c.claimDeleteIntent(t.Context(), inc, *inc.CloudIdentity, "gpu", "machine", ""); err != nil {
		t.Fatal(err)
	}
	r := restartWith(t, c, st, p)
	r.sweepRemoveFirst(t.Context())
	inc = mustIncident(t, st, id)
	if !inc.RemoveFirst.Open() {
		t.Fatal("deadline restarted or expired too early")
	}
	inc.RemoveFirst.PlannedAt = time.Now().Add(-time.Hour)
	if err := st.UpdateIncident(t.Context(), inc); err != nil {
		t.Fatal(err)
	}
	r.sweepRemoveFirst(t.Context())
	got := mustIncident(t, st, id)
	if got.State != types.StateNeedsHuman || got.RemoveFirst.Outcome != types.RemoveFirstNeedsOperator || !strings.Contains(got.RemoveFirst.Reason, "could not confirm its completion") {
		t.Fatalf("state=%s record=%+v", got.State, got.RemoveFirst)
	}
}

type retryCountingPlatform struct {
	*plannedPlatform
	reads, deletes int
	state          removefirst.Observation
}

func (p *retryCountingPlatform) ObservePinnedDelete(context.Context, cloud.NodeIdentity, string, string) (removefirst.Observation, error) {
	p.reads++
	return p.state, nil
}
func (p *retryCountingPlatform) RetryPinnedDelete(context.Context, cloud.NodeIdentity, string, string, removefirst.Observation) error {
	p.deletes++
	p.state = removefirst.Deleted
	return nil
}

func TestRestartRechecksDeleteAuthority(t *testing.T) {
	for _, name := range []string{"eks", "aks", "digest", "paused", "dry-run", "approval", "lost-owner"} {
		t.Run(name, func(t *testing.T) {
			p := &retryCountingPlatform{plannedPlatform: newPlannedPlatform(), state: removefirst.SameMachine}
			if name == "aks" {
				p.state = removefirst.Listed
			}
			c, st := plannedFixture(t, p.plannedPlatform)
			id := openOn(t, c, "n1")
			ctx := t.Context()
			inc := mustIncident(t, st, id)
			inc.State, inc.StepIndex, inc.Playbook = types.StateExecuting, 2, "cloud-replace"
			if err := st.UpdateIncident(ctx, inc); err != nil {
				t.Fatal(err)
			}
			inc = mustIncident(t, st, id)
			pin := *inc.CloudIdentity
			if name != "lost-owner" {
				if err := st.WithTx(ctx, func(tx store.Tx) error {
					_, err := tx.ClaimNodeRemediation(ctx, pin.Node, pin.NodeUID, id, time.Now())
					return err
				}); err != nil {
					t.Fatal(err)
				}
			}
			if err := c.claimDeleteIntent(ctx, inc, pin, "train", pin.InstanceID, ""); err != nil {
				t.Fatal(err)
			}
			r := restartWith(t, c, st, p)
			switch name {
			case "paused":
				r.gate.Pause()
			case "dry-run":
				cfg := *r.runtime.Load()
				limits := *cfg.SafetyLimits
				limits.DryRun = true
				cfg.SafetyLimits = &limits
				if err := r.InstallRuntimeConfig(cfg); err != nil {
					t.Fatal(err)
				}
			case "approval":
				r.SetEngine(modeEngine(t, recoverymode.Approval, nil))
			case "digest":
				kube := fake.NewSimpleClientset()
				r.ConfigurePublishedConfig(kube.CoreV1().ConfigMaps("kube-neuron"), "kn")
				loadDigest(t, r, "before")
				publishDigest(t, kube, "after")
			}
			r.sweepRemoveFirst(ctx)
			want := 0
			if name == "eks" || name == "aks" {
				want = 1
			}
			if p.reads != 1 || p.deletes != want {
				t.Fatalf("reads=%d deletes=%d want deletes=%d", p.reads, p.deletes, want)
			}
			if name == "lost-owner" && mustIncident(t, st, id).State != types.StateNeedsHuman {
				t.Fatal("lost owner did not park")
			}
		})
	}
}
