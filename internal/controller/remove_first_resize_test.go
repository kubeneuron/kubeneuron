// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"strings"
	"testing"
	"time"

	"github.com/kubeneuron/kubeneuron/internal/recoverymode"
	"github.com/kubeneuron/kubeneuron/internal/store"
	"github.com/kubeneuron/kubeneuron/internal/store/sqlite"
	"github.com/kubeneuron/kubeneuron/pkg/cloud"
	"github.com/kubeneuron/kubeneuron/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
)

// resizeNext leaves an incident whose step stopped after the delete, with
// the plan recorded as resizing and its resize not sent, on a pool one node
// short, and a new controller on it.
func resizeNext(t *testing.T) (*plannedPlatform, *Controller, *sqlite.Store, string) {
	t.Helper()
	p := newPlannedPlatform()
	c, st := plannedFixture(t, p)
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
	plan, err := p.PlanRemoveFirst(ctx, pin, successorOperation(inc))
	if err != nil {
		t.Fatal(err)
	}
	plan.Phase = cloud.RemoveFirstResizing
	if err := c.claimRemoveFirst(ctx, inc, plan); err != nil {
		t.Fatal(err)
	}
	p.set(func(p *plannedPlatform) { p.gone[pin.InstanceID], p.size = true, p.before-1 })
	return p, restartWith(t, c, st, p), st, id
}

// A resize the sweep would send after a restart is held like the step:
// nothing is sent, the pool's size is read, and the record says what it
// waits for and how to restore the capacity.
func TestHeldResizeNotesThePool(t *testing.T) {
	for _, tc := range []struct {
		name, waitingFor, text string
		hold                   func(*testing.T, *Controller)
	}{
		{"paused", "remediation to be resumed", "Setting it back is waiting for remediation to be resumed. To restore capacity: run kubeneuronctl resume.",
			func(_ *testing.T, r *Controller) { r.gate.Pause() }},
		{"dry-run", "policy " + autoPolicy + " to leave DryRun", "Setting it back is waiting for policy " + autoPolicy + " to leave DryRun. To restore capacity: set the mode of policy " + autoPolicy + " to Approval or Automatic.",
			func(t *testing.T, r *Controller) { r.SetEngine(modeEngine(t, recoverymode.DryRun, nil)) }},
		{"digest", "the published configuration to be loaded", "held: the controller has not yet loaded the published configuration",
			func(t *testing.T, r *Controller) {
				kube := fake.NewSimpleClientset()
				r.ConfigurePublishedConfig(kube.CoreV1().ConfigMaps("kube-neuron"), "kn")
				loadDigest(t, r, "before")
				publishDigest(t, kube, "after")
			}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, r, st, id := resizeNext(t)
			tc.hold(t, r)
			r.sweepRemoveFirst(t.Context())
			r.sweepRemoveFirst(t.Context())
			inc := mustIncident(t, st, id)
			if _, resizes := p.calls(inc.RemoveFirst.Operation); resizes != 0 || !inc.RemoveFirst.Open() {
				t.Fatalf("resizes=%d plan %+v", resizes, inc.RemoveFirst)
			}
			want := "pool gpu: 3 of 4 nodes — replacement of n1 is waiting for " + tc.waitingFor + " (incident " + id + ")"
			if got := types.RemoveFirstHeld(inc); got != want {
				t.Fatalf("held line %q, want %q", got, want)
			}
			if got := r.heldResizes()["gpu"]; len(got) != 1 || "pool gpu: "+got[0] != want {
				t.Fatalf("pool notes %q", got)
			}
			text := "Pool gpu has 3 of 4 nodes: node n1 was removed and the pool size has not been set back yet. " + strings.ReplaceAll(tc.text, "<id>", id)
			if n := strings.Count(dump(trail(t, st, id)), "remove-first-held"); n != 1 || !auditHas(trail(t, st, id), "remove-first-held", text) {
				t.Fatalf("held %d times, want once with %q%s", n, text, dump(trail(t, st, id)))
			}
		})
	}
}

// restarted runs the passes after a restart: the step is not decided again,
// as its node is gone, and the incident waits in EVALUATING.
func restarted(t *testing.T, r *Controller, st *sqlite.Store, id string) {
	t.Helper()
	for range 3 {
		r.reconcile(t.Context())
	}
	if inc := mustIncident(t, st, id); inc.State != types.StateEvaluating && inc.State != types.StateVerifying {
		t.Fatalf("state %s%s", inc.State, dump(trail(t, st, id)))
	}
}

// A restart before the resize was sent: under Automatic the sweep sends it
// once from EVALUATING, and the plan ends restored.
func TestRestartBeforeTheResizeSendsItOnce(t *testing.T) {
	p, r, st, id := resizeNext(t)
	restarted(t, r, st, id)
	r.sweepRemoveFirst(t.Context())
	inc := mustIncident(t, st, id)
	if _, resizes := p.calls(inc.RemoveFirst.Operation); resizes != 1 || inc.RemoveFirst.Outcome != types.RemoveFirstRestored {
		t.Fatalf("resizes=%d plan %+v%s", resizes, inc.RemoveFirst, dump(trail(t, st, id)))
	}
	if n := strings.Count(dump(trail(t, st, id)), "step-start: platform.replace_node"); n != 0 {
		t.Fatalf("the step was started again after the restart%s", dump(trail(t, st, id)))
	}
}

// Under Approval the resize needs an approval no step can ask for after the
// restart: the pool goes to an operator at once, with nothing sent.
func TestUnapprovableResizeNeedsAnOperatorAtOnce(t *testing.T) {
	p, r, st, id := resizeNext(t)
	r.SetEngine(modeEngine(t, recoverymode.Approval, nil))
	for range 3 {
		r.reconcile(t.Context())
	}
	inc := mustIncident(t, st, id)
	want := "Pool gpu has 3 of 4 nodes: node n1 was removed and the pool size was not set back. Policy " + autoPolicy +
		" requires approval for this step, and the approval can no longer be requested because the step was interrupted after the node was removed. Set the size of pool gpu back to 4 nodes, then run kubeneuronctl resolve " +
		id + " --cloud-operation-settled --reason \"<what you found>\""
	if _, resizes := p.calls(inc.RemoveFirst.Operation); resizes != 0 || inc.State != types.StateNeedsHuman ||
		inc.RemoveFirst.Outcome != types.RemoveFirstNeedsOperator || inc.RemoveFirst.Reason != want || inc.ApprovalEpoch != 0 {
		t.Fatalf("resizes=%d state %s round %d plan %+v", resizes, inc.State, inc.ApprovalEpoch, inc.RemoveFirst)
	}
	line := "pool gpu: 3 of 4 nodes — replacement of n1 needs the operator (incident " + id + ")"
	if got := types.RemoveFirstHeld(inc); got != line {
		t.Fatalf("status line %q", got)
	}
	r.sweepRemoveFirst(t.Context())
	if got := r.heldResizes()["gpu"]; len(got) != 1 || "pool gpu: "+got[0] != line {
		t.Fatalf("pool notes %q", got)
	}
}

// A pool someone set back while the resize was held ends the plan restored
// by its final state, with nothing sent.
func TestHeldResizeEndsOnARestoredPool(t *testing.T) {
	p, r, st, id := resizeNext(t)
	r.gate.Pause()
	r.sweepRemoveFirst(t.Context())
	p.set(func(p *plannedPlatform) { p.size = p.before })
	r.sweepRemoveFirst(t.Context())
	inc := mustIncident(t, st, id)
	if _, resizes := p.calls(inc.RemoveFirst.Operation); resizes != 0 || inc.RemoveFirst.Outcome != types.RemoveFirstRestored ||
		inc.RemoveFirst.Reason != "confirmed by the pool's final state, not by the cloud operation" {
		t.Fatalf("resizes=%d plan %+v", resizes, inc.RemoveFirst)
	}
}

// Once the hold is lifted the resize is sent, as a new mutation the policy
// allows, and the note is dropped.
func TestLiftedHoldSendsTheResize(t *testing.T) {
	p, r, st, id := resizeNext(t)
	r.gate.Pause()
	restarted(t, r, st, id)
	r.sweepRemoveFirst(t.Context())
	if inc := mustIncident(t, st, id); inc.RemoveFirst.State[types.RemoveFirstHeldFor] != "remediation to be resumed" {
		t.Fatalf("plan %+v", inc.RemoveFirst)
	}
	r.gate.Resume()
	r.sweepRemoveFirst(t.Context())
	inc := mustIncident(t, st, id)
	if _, resizes := p.calls(inc.RemoveFirst.Operation); resizes != 1 || inc.RemoveFirst.State[types.RemoveFirstHeldFor] != "" {
		t.Fatalf("resizes=%d plan %+v", resizes, inc.RemoveFirst)
	}
	r.sweepRemoveFirst(t.Context())
	if inc := mustIncident(t, st, id); inc.RemoveFirst.Outcome != types.RemoveFirstRestored || inc.RemoveFirst.Reason != "" {
		t.Fatalf("plan %+v", inc.RemoveFirst)
	}
}

// A resize held for more than an hour leaves the incident for an operator,
// with the size read and the command that settles it.
func TestHeldResizeNeedsAnOperatorAfterAnHour(t *testing.T) {
	p, r, st, id := resizeNext(t)
	r.gate.Pause()
	removeFirstDeadline = 0
	t.Cleanup(func() { removeFirstDeadline = time.Hour })
	r.sweepRemoveFirst(t.Context())
	inc := mustIncident(t, st, id)
	want := "Pool gpu has had 3 of 4 nodes for more than an hour: node n1 was removed and the pool size was not set back. Check the machine and the size of the pool, then run kubeneuronctl resolve " + id + " --cloud-operation-settled --reason \"<what you found>\""
	if _, resizes := p.calls(inc.RemoveFirst.Operation); resizes != 0 || inc.State != types.StateNeedsHuman || inc.RemoveFirst.Reason != want {
		t.Fatalf("resizes=%d state %s plan %+v", resizes, inc.State, inc.RemoveFirst)
	}
}
