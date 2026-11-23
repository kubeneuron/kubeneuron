// SPDX-License-Identifier: Apache-2.0

package gcp

import (
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/kubeneuron/kubeneuron/internal/cloud/removefirst"
	"github.com/kubeneuron/kubeneuron/internal/cloud/resizediff"
	"github.com/kubeneuron/kubeneuron/pkg/cloud"
)

const removeFirstOp = "inc-1-a1"

// restarted is the plan as a restarted controller reads it back from the
// store, continued by a new provider.
func restarted(t *testing.T, f *fakeGKE, plan cloud.RemoveFirstPlan) (*Provider, cloud.RemoveFirstPlan) {
	t.Helper()
	raw, err := json.Marshal(plan)
	if err != nil {
		t.Fatal(err)
	}
	var back cloud.RemoveFirstPlan
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatal(err)
	}
	return testProvider(f), back
}

func planRemoveFirst(t *testing.T, f *fakeGKE) cloud.RemoveFirstPlan {
	t.Helper()
	plan, err := testProvider(f).PlanRemoveFirst(t.Context(), testInstance(1), removeFirstOp)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Phase != cloud.RemoveFirstPlanned || len(f.calls) != 0 {
		t.Fatalf("plan %+v wrote %q", plan, f.calls)
	}
	return plan
}

// step continues the plan once, as after a restart, and wants the phase.
func step(t *testing.T, f *fakeGKE, plan cloud.RemoveFirstPlan, want cloud.RemoveFirstPhase) cloud.RemoveFirstPlan {
	t.Helper()
	p, plan := restarted(t, f, plan)
	next, err := p.ContinueRemoveFirst(t.Context(), plan)
	if err != nil {
		t.Fatalf("continue from %q: %v", plan.Phase, err)
	}
	if next.Phase != want {
		t.Fatalf("phase %q from %q, want %q", next.Phase, plan.Phase, want)
	}
	return next
}

// resize continues a plan whose instance is gone through the resize: the
// first step only returns resizing, for the caller to record, and sends
// nothing; the second sends it.
func resize(t *testing.T, f *fakeGKE, plan cloud.RemoveFirstPlan) cloud.RemoveFirstPlan {
	t.Helper()
	calls := len(f.calls)
	plan = step(t, f, plan, cloud.RemoveFirstResizing)
	if len(f.calls) != calls || plan.State[stateOperation] != "" {
		t.Fatalf("the step to resizing called %q", f.calls[calls:])
	}
	plan = step(t, f, plan, cloud.RemoveFirstResizing)
	if plan.State[stateOperation] == "" || plan.State[stateOperationLocation] != testLocation {
		t.Fatalf("resize not recorded: %v", plan.State)
	}
	return plan
}

func sizes(f *fakeGKE) []int32 {
	var out []int32
	for _, g := range f.groups {
		out = append(out, g.target)
	}
	return out
}

// The instance is deleted by name, then the pool is set back to its size;
// every phase is continued by a restarted provider from the recorded plan.
func TestRemoveFirstDeletesThenRestoresTheSize(t *testing.T) {
	f := newFakeGKE(3)
	plan := planRemoveFirst(t, f)
	if plan.State[stateSize] != "3" || !strings.Contains(plan.Restore, "--node-pool gpu --num-nodes 3 --location us-central1") {
		t.Fatalf("plan %+v", plan)
	}
	plan = step(t, f, plan, cloud.RemoveFirstDeleting)
	if len(f.calls) != 0 {
		t.Fatalf("the step to deleting called %q", f.calls)
	}
	plan = step(t, f, plan, cloud.RemoveFirstDeleting)
	if !slices.Equal(f.calls, []string{"delete " + testZone + " " + testName(testZone, 1)}) {
		t.Fatalf("calls %q", f.calls)
	}
	plan = resize(t, f, plan)
	if f.calls[len(f.calls)-1] != "setSize gpu 3" || plan.State[stateOperation] == "" {
		t.Fatalf("calls %q state %v", f.calls, plan.State)
	}
	plan = step(t, f, plan, cloud.RemoveFirstDone)
	step(t, f, plan, cloud.RemoveFirstDone)
	if len(f.calls) != 2 || f.groups[0].target != 3 {
		t.Fatalf("calls %q target %d", f.calls, f.groups[0].target)
	}
}

// A pool across zones of the same size is restored in the zone that lost
// the instance; the other zones stay as they are.
func TestRemoveFirstAcrossEqualZones(t *testing.T) {
	f := newFakeGKE(2, testZone, "us-central1-b")
	if err := testProvider(f).CheckRemoveFirst(t.Context(), testInstance(1)); err != nil {
		t.Fatal(err)
	}
	if err := testProvider(f).CheckReplacePermission(t.Context(), testInstance(1)); err != nil {
		t.Fatal(err)
	}
	plan := step(t, f, step(t, f, planRemoveFirst(t, f), cloud.RemoveFirstDeleting), cloud.RemoveFirstDeleting)
	if !slices.Equal(sizes(f), []int32{1, 2}) {
		t.Fatalf("sizes %v", sizes(f))
	}
	plan = resize(t, f, plan)
	step(t, f, plan, cloud.RemoveFirstDone)
	if !slices.Equal(sizes(f), []int32{2, 2}) || f.calls[1] != "setSize gpu 2" {
		t.Fatalf("sizes %v calls %q", sizes(f), f.calls)
	}
}

// Zones of different sizes cannot be set back one at a time: refused before
// the cordon and at the plan, with nothing changed.
func TestRemoveFirstRefusesUnequalZones(t *testing.T) {
	f := newFakeGKE(2, testZone, "us-central1-b")
	f.groups[1].target = 3
	f.groups[1].grow(3, nil)
	p := testProvider(f)
	if err := p.CheckRemoveFirst(t.Context(), testInstance(1)); !errors.Is(err, cloud.ErrRemoveFirstUnavailable) {
		t.Fatalf("CheckRemoveFirst = %v", err)
	}
	if err := p.CheckReplacePermission(t.Context(), testInstance(1)); !errors.Is(err, cloud.ErrRemoveFirstUnavailable) {
		t.Fatalf("CheckReplacePermission = %v", err)
	}
	if _, err := p.PlanRemoveFirst(t.Context(), testInstance(1), removeFirstOp); !errors.Is(err, cloud.ErrRemoveFirstUnavailable) || len(f.calls) != 0 {
		t.Fatalf("plan = %v, calls %q", err, f.calls)
	}
}

// An autoscaled pool needs no plan (Replace only deletes); Autopilot has none.
func TestRemoveFirstPlanOnlyForFixedPools(t *testing.T) {
	if _, err := testProvider(autoscaled(3)).PlanRemoveFirst(t.Context(), testInstance(1), removeFirstOp); !errors.Is(err, cloud.ErrRemoveFirstUnplanned) {
		t.Fatalf("autoscaled: %v", err)
	}
	f := newFakeGKE(3)
	f.autopilot = true
	if _, err := testProvider(f).PlanRemoveFirst(t.Context(), testInstance(1), removeFirstOp); !errors.Is(err, cloud.ErrRemoveFirstUnavailable) {
		t.Fatalf("autopilot: %v", err)
	}
}

// A delete whose answer is lost is the same delete when resent: the
// instance gone, the plan goes on to the resize; a delete still running is
// waited for.
func TestRemoveFirstLostDeleteAnswer(t *testing.T) {
	f := newFakeGKE(3)
	plan := step(t, f, planRemoveFirst(t, f), cloud.RemoveFirstDeleting)
	f.loseDelete = true
	next, err := testProvider(f).ContinueRemoveFirst(t.Context(), plan)
	if err == nil || next.Phase != cloud.RemoveFirstDeleting {
		t.Fatalf("lost answer: %+v, %v", next, err)
	}
	next = resize(t, f, next)
	step(t, f, next, cloud.RemoveFirstDone)
	if !slices.Equal(f.calls, []string{"delete " + testZone + " " + testName(testZone, 1), "setSize gpu 3"}) {
		t.Fatalf("calls %q", f.calls)
	}

	f = newFakeGKE(3)
	plan = step(t, f, planRemoveFirst(t, f), cloud.RemoveFirstDeleting)
	f.slowDelete = true
	if next, err = testProvider(f).ContinueRemoveFirst(t.Context(), plan); err == nil || next.Phase != cloud.RemoveFirstDeleting {
		t.Fatalf("slow delete: %+v, %v", next, err)
	}
	f.calls = nil
	next = step(t, f, next, cloud.RemoveFirstDeleting)
	if len(f.calls) != 0 {
		t.Fatalf("a running delete was not waited for: %q", f.calls)
	}
	f.finishDeletes()
	next = resize(t, f, next)
	step(t, f, next, cloud.RemoveFirstDone)
	if !slices.Equal(f.calls, []string{"setSize gpu 3"}) || f.groups[0].target != 3 {
		t.Fatalf("calls %q target %d", f.calls, f.groups[0].target)
	}
}

// A controller that stopped during a call continues from the phase it had
// recorded before: the delete or the resize is not made twice. A plan
// recorded as planned reached nothing.
func TestRemoveFirstRestartDuringEachCall(t *testing.T) {
	f := newFakeGKE(3)
	deleting := step(t, f, planRemoveFirst(t, f), cloud.RemoveFirstDeleting)
	step(t, f, deleting, cloud.RemoveFirstDeleting) // the delete; never recorded
	resizing := step(t, f, deleting, cloud.RemoveFirstResizing)
	step(t, f, resizing, cloud.RemoveFirstResizing) // the resize; never recorded
	done := step(t, f, resizing, cloud.RemoveFirstDone)
	if !slices.Equal(f.calls, []string{"delete " + testZone + " " + testName(testZone, 1), "setSize gpu 3"}) {
		t.Fatalf("calls %q", f.calls)
	}
	if done.State[stateEvidence] != EvidenceFinalState {
		t.Fatalf("evidence %q, want the final state: no operation was recorded", done.State[stateEvidence])
	}
}

// resizing is a plan whose instance is gone, recorded as resizing.
func resizing(t *testing.T, f *fakeGKE) cloud.RemoveFirstPlan {
	t.Helper()
	plan := step(t, f, step(t, f, planRemoveFirst(t, f), cloud.RemoveFirstDeleting), cloud.RemoveFirstDeleting)
	return step(t, f, plan, cloud.RemoveFirstResizing)
}

// A resize whose answer is lost, or that is still running, is waited for and
// not sent again; a resize that failed to be sent is sent again.
func TestRemoveFirstLostAndFailedResize(t *testing.T) {
	f := newFakeGKE(3)
	plan := resizing(t, f)
	f.lose, f.hold = true, true
	next, err := testProvider(f).ContinueRemoveFirst(t.Context(), plan)
	if err == nil || next.Phase != cloud.RemoveFirstResizing || next.State[stateOperation] != "" {
		t.Fatalf("lost answer: %+v, %v", next, err)
	}
	next = step(t, f, next, cloud.RemoveFirstResizing)
	f.Deliver()
	done := step(t, f, next, cloud.RemoveFirstDone)
	if n := strings.Count(strings.Join(f.calls, "\n"), "setSize"); n != 1 || done.State[stateEvidence] != EvidenceFinalState {
		t.Fatalf("calls %q evidence %q", f.calls, done.State[stateEvidence])
	}

	f = newFakeGKE(3)
	plan = resizing(t, f)
	f.setSizeErr = errors.New("503")
	if next, err = testProvider(f).ContinueRemoveFirst(t.Context(), plan); err == nil || next.Phase != cloud.RemoveFirstResizing {
		t.Fatalf("failed resize: %+v, %v", next, err)
	}
	f.setSizeErr = nil
	next = step(t, f, next, cloud.RemoveFirstResizing)
	done = step(t, f, next, cloud.RemoveFirstDone)
	if n := strings.Count(strings.Join(f.calls, "\n"), "setSize gpu 3"); n != 2 || f.groups[0].target != 3 || done.State[stateEvidence] != EvidenceOperation {
		t.Fatalf("calls %q target %d evidence %q", f.calls, f.groups[0].target, done.State[stateEvidence])
	}
}

// A recorded resize is never sent again: one that ended with an error, or
// that finished with the pool still short, needs an operator; one no longer
// readable is decided by the sizes alone.
func TestRemoveFirstRecordedResizeIsNotResent(t *testing.T) {
	f := newFakeGKE(3)
	plan := step(t, f, resizing(t, f), cloud.RemoveFirstResizing)
	f.ops[len(f.ops)-1].ErrorCode = 9
	f.calls = nil
	if _, err := testProvider(f).ContinueRemoveFirst(t.Context(), plan); !errors.Is(err, cloud.ErrNeedsOperator) || len(f.calls) != 0 {
		t.Fatalf("operation with an error: %v, calls %q", err, f.calls)
	}

	f = newFakeGKE(3)
	plan = step(t, f, resizing(t, f), cloud.RemoveFirstResizing)
	f.groups[0].target = 2
	f.calls = nil
	if _, err := testProvider(f).ContinueRemoveFirst(t.Context(), plan); !errors.Is(err, cloud.ErrNeedsOperator) || len(f.calls) != 0 {
		t.Fatalf("finished operation, pool short: %v, calls %q", err, f.calls)
	}

	f = newFakeGKE(3)
	plan = step(t, f, resizing(t, f), cloud.RemoveFirstResizing)
	f.ops = nil
	done := step(t, f, plan, cloud.RemoveFirstDone)
	if done.State[stateEvidence] != EvidenceFinalState {
		t.Fatalf("evidence %q, want the final state", done.State[stateEvidence])
	}

	f = newFakeGKE(3)
	plan = step(t, f, resizing(t, f), cloud.RemoveFirstResizing)
	f.ops, f.groups[0].target = nil, 2
	f.calls = nil
	step(t, f, plan, cloud.RemoveFirstResizing)
	if len(f.calls) != 0 {
		t.Fatalf("calls %q: a recorded resize was sent again", f.calls)
	}
}

// Between the delete and the resize another operation on the pool is waited
// for; a resize by someone else that left the pool at its size or larger
// ends the plan with no call, and one that left another zone at another size
// needs an operator, with the command that sets the size back.
func TestRemoveFirstForeignResizeBetweenPhases(t *testing.T) {
	f := newFakeGKE(2, testZone, "us-central1-b")
	plan := step(t, f, step(t, f, planRemoveFirst(t, f), cloud.RemoveFirstDeleting), cloud.RemoveFirstDeleting)
	op := f.addOp(opSetSize, "RUNNING", poolLink(testPool))
	f.calls = nil
	plan = step(t, f, plan, cloud.RemoveFirstDeleting)
	if len(f.calls) != 0 {
		t.Fatalf("calls %q while another operation runs", f.calls)
	}
	op.Status = opDone
	f.groups[0].target, f.groups[1].target = 3, 3
	step(t, f, plan, cloud.RemoveFirstDone)
	if len(f.calls) != 0 || !slices.Equal(sizes(f), []int32{3, 3}) {
		t.Fatalf("calls %q sizes %v: a larger pool was lowered", f.calls, sizes(f))
	}

	f = newFakeGKE(2, testZone, "us-central1-b")
	plan = step(t, f, step(t, f, planRemoveFirst(t, f), cloud.RemoveFirstDeleting), cloud.RemoveFirstDeleting)
	f.groups[1].target = 3
	f.calls = nil
	_, err := testProvider(f).ContinueRemoveFirst(t.Context(), plan)
	if !errors.Is(err, cloud.ErrNeedsOperator) || !strings.Contains(err.Error(), "gcloud container clusters resize training --node-pool gpu --num-nodes 2") || len(f.calls) != 0 {
		t.Fatalf("%v, calls %q", err, f.calls)
	}
}

// A pool changed after the plan and before the delete is refused with
// nothing changed.
func TestRemoveFirstRefusesAPoolChangedBeforeTheDelete(t *testing.T) {
	f := newFakeGKE(3)
	plan := planRemoveFirst(t, f)
	f.groups[0].target = 4
	if _, err := testProvider(f).ContinueRemoveFirst(t.Context(), plan); !errors.Is(err, cloud.ErrRemoveFirstUnavailable) || len(f.calls) != 0 {
		t.Fatalf("%v, calls %q", err, f.calls)
	}
	f = newFakeGKE(3)
	plan = planRemoveFirst(t, f)
	f.autoscaling = true
	if _, err := testProvider(f).ContinueRemoveFirst(t.Context(), plan); !errors.Is(err, cloud.ErrRemoveFirstUnavailable) || len(f.calls) != 0 {
		t.Fatalf("autoscaling on: %v, calls %q", err, f.calls)
	}
}

// A delete Compute Engine accepted, whose member is still listed as it was,
// is the plan's own: it is resent with the same request ID, not handed to an
// operator, and the pool is set back once.
func TestRemoveFirstAcceptedDeleteNotShownYet(t *testing.T) {
	f := newFakeGKE(3)
	plan := step(t, f, planRemoveFirst(t, f), cloud.RemoveFirstDeleting)
	f.acceptDelete = true
	next, err := testProvider(f).ContinueRemoveFirst(t.Context(), plan)
	if err == nil || errors.Is(err, cloud.ErrNeedsOperator) || f.groups[0].target != 2 {
		t.Fatalf("accepted delete: %+v, %v, target %d", next, err, f.groups[0].target)
	}
	next = step(t, f, next, cloud.RemoveFirstDeleting)
	next = resize(t, f, next)
	step(t, f, next, cloud.RemoveFirstDone)
	if f.groups[0].target != 3 || strings.Count(strings.Join(f.calls, "\n"), "setSize") != 1 {
		t.Fatalf("calls %q target %d", f.calls, f.groups[0].target)
	}
}

// A plan whose state names a group the pool does not have, or a pool with
// no groups, is refused or handed over, never a panic.
func TestRemoveFirstMalformedPlanOrPool(t *testing.T) {
	f := newFakeGKE(3)
	plan := step(t, f, planRemoveFirst(t, f), cloud.RemoveFirstDeleting)
	bad := plan
	bad.State = withState(plan.State, stateGroup, "projects/ml-project/zones/us-central1-a/instanceGroupManagers/other")
	if _, err := testProvider(f).ContinueRemoveFirst(t.Context(), bad); !errors.Is(err, cloud.ErrNeedsOperator) || len(f.calls) != 0 {
		t.Fatalf("unknown group: %v, calls %q", err, f.calls)
	}
	empty := &fakeGKE{labels: f.labels}
	p := testProvider(empty)
	if err := p.unequalZones(t.Context(), nodePool{Name: testPool}); !errors.Is(err, cloud.ErrRemoveFirstUnavailable) {
		t.Fatalf("no groups: %v", err)
	}
	if _, err := p.ContinueRemoveFirst(t.Context(), plan); err == nil {
		t.Fatal("a pool without groups continued the plan")
	}
}

// runRemoveFirst drives a plan from planned to done, calling between before
// the resize is asked for, and returns the finished plan.
func runRemoveFirst(t *testing.T, f *fakeGKE, between func()) cloud.RemoveFirstPlan {
	t.Helper()
	plan := step(t, f, step(t, f, planRemoveFirst(t, f), cloud.RemoveFirstDeleting), cloud.RemoveFirstDeleting)
	if between != nil {
		between()
	}
	for i := 0; plan.Phase != cloud.RemoveFirstDone; i++ {
		p, back := restarted(t, f, plan)
		next, err := p.ContinueRemoveFirst(t.Context(), back)
		if err != nil || i > 3 {
			t.Fatalf("continue from %q: %+v, %v", plan.Phase, next, err)
		}
		plan = next
	}
	return plan
}

// The group's members are recorded before the delete; once the size is back,
// its one new member is the instance that replaced the deleted one.
func TestRemoveFirstNamesTheOneNewMember(t *testing.T) {
	f := newFakeGKE(3)
	deleting := step(t, f, planRemoveFirst(t, f), cloud.RemoveFirstDeleting)
	if want := strings.Join([]string{testName(testZone, 0), testName(testZone, 1), testName(testZone, 2)}, ","); deleting.State[stateMembers] != want {
		t.Fatalf("members before the delete %q, want %q", deleting.State[stateMembers], want)
	}
	if got := resizediff.FromPlan(deleting); got != "" {
		t.Fatalf("an open plan names %q", got)
	}
	plan := resize(t, f, step(t, f, deleting, cloud.RemoveFirstDeleting))
	if got := resizediff.FromPlan(plan); got != "" {
		t.Fatalf("a resizing plan names %q", got)
	}
	plan = step(t, f, plan, cloud.RemoveFirstDone)
	if got, want := resizediff.FromPlan(plan), "gce://"+testProject+"/"+testZone+"/"+testName(testZone, 3); got != want {
		t.Fatalf("new instance %q, want %q", got, want)
	}
}

// No instance is named when the comparison cannot single one out: two new
// members, none, a group too large to record, or a plan recorded without the
// members (by an earlier release).
func TestRemoveFirstNamesNoInstanceWhenNotExactlyOne(t *testing.T) {
	cases := map[string]struct {
		f       *fakeGKE
		between func(f *fakeGKE)
	}{
		"two new members": {f: newFakeGKE(3), between: func(f *fakeGKE) {
			f.groups[0].target = 4 // resized past the plan by someone else
			f.fill()
		}},
		"pool resized larger by someone else": {f: newFakeGKE(2, testZone, "us-central1-b"), between: func(f *fakeGKE) {
			f.groups[0].target, f.groups[1].target = 3, 3
			f.fill()
		}},
		"no new member": {f: newFakeGKE(3), between: func(f *fakeGKE) {
			f.groups[0].target = 3 // set back by someone else, no instance listed yet
		}},
		"group above the limit": {f: newFakeGKE(resizediff.MaxMembers + 1)},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			var between func()
			if tc.between != nil {
				between = func() { tc.between(tc.f) }
			}
			plan := runRemoveFirst(t, tc.f, between)
			if got := resizediff.FromPlan(plan); got != "" {
				t.Fatalf("named %q", got)
			}
		})
	}

	t.Run("plan without members", func(t *testing.T) {
		f := newFakeGKE(3)
		plan := step(t, f, planRemoveFirst(t, f), cloud.RemoveFirstDeleting)
		delete(plan.State, stateMembers)
		plan = step(t, f, plan, cloud.RemoveFirstDeleting)
		plan = step(t, f, resize(t, f, plan), cloud.RemoveFirstDone)
		if got := resizediff.FromPlan(plan); got != "" {
			t.Fatalf("named %q", got)
		}
	})
}

// A held resize reads the pool's size and changes nothing; the plan's
// operation key is the one the controller reads.
func TestObserveRemoveFirstSize(t *testing.T) {
	if stateOperation != removefirst.StateResizeOperation {
		t.Fatalf("operation key %q, controller reads %q", stateOperation, removefirst.StateResizeOperation)
	}
	f := newFakeGKE(2, testZone, "us-central1-b")
	plan := resizing(t, f)
	f.calls = nil
	size, err := testProvider(f).ObserveRemoveFirstSize(t.Context(), plan)
	if err != nil || size != (removefirst.PoolSize{Current: 3, Planned: 4}) || len(f.calls) != 0 {
		t.Fatalf("size %+v, %v, calls %q", size, err, f.calls)
	}
	if !removefirst.ResizeMaySend(plan) {
		t.Fatal("a plan recorded as resizing with no operation may send the resize")
	}
}
