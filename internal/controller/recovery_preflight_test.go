// SPDX-License-Identifier: Apache-2.0

package controller

// #56: the policy's recovery settings are checked where the provider's
// eligibility is, before the first disruptive step; a recycle the policy or
// the provider does not allow is skipped, not held and not failed.

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/kubeneuron/kubeneuron/internal/playbook"
	"github.com/kubeneuron/kubeneuron/internal/recoverymode"
	"github.com/kubeneuron/kubeneuron/internal/store/sqlite"
	"github.com/kubeneuron/kubeneuron/pkg/types"
)

func ladderEngine(t *testing.T, mode recoverymode.Mode, strategy recoverymode.Strategy, recycle bool, books ...*playbook.Playbook) *playbook.Engine {
	t.Helper()
	m := map[string]*playbook.Playbook{}
	for _, b := range books {
		m[b.Name] = b
	}
	pol := playbook.Policy{Name: autoPolicy, Class: types.ClassFellOffBus, Playbook: books[0].Name, Mode: mode,
		ReplacementStrategy: strategy, RecycleEnabled: recycle}
	if mode == recoverymode.Automatic {
		pol.Automatic = autoSettings()
	}
	engine, err := playbook.NewEngine(m, []playbook.Policy{pol})
	if err != nil {
		t.Fatal(err)
	}
	return engine
}

func cordonDrainThen(name, action, escalate string) *playbook.Playbook {
	book := &playbook.Playbook{Name: name, Target: "node", Steps: []playbook.Step{
		{Name: "cordon", Action: "platform.cordon"},
		{Name: "drain", Action: "platform.drain", Approval: "required"},
		{Name: strings.TrimPrefix(action, "platform."), Action: action, Approval: "required"},
	}}
	book.OnFailure.EscalateTo = escalate
	return book
}

func trail(t *testing.T, st *sqlite.Store, id string) []*types.AuditEntry {
	t.Helper()
	entries, err := st.AuditTrail(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return entries
}

func dump(entries []*types.AuditEntry) string {
	var b strings.Builder
	for _, e := range entries {
		fmt.Fprintf(&b, "\n  %s->%s %s %s: %s", e.FromState, e.ToState, e.Actor, e.Action, e.Result)
	}
	return b.String()
}

func TestNoUsableRecoveryHoldsBeforeCordon(t *testing.T) {
	for _, mode := range []recoverymode.Mode{recoverymode.Approval, recoverymode.Automatic} {
		t.Run(string(mode), func(t *testing.T) {
			c, st, notifier := autoFixture(t, mode, autoSettings(), 4)
			// add-first (the default) and no recycle on the path: nothing can run.
			c.SetEngine(ladderEngine(t, mode, "", false, cordonDrainThen("cloud-replace", "platform.replace_node", "")))
			id := openOn(t, c, "n1")
			inc := step(t, c, st, id, 30)
			entries := trail(t, st, id)
			if inc.State != types.StateEvaluating || inc.StepIndex != 0 || len(notifier.approvals) != 0 {
				t.Fatalf("state=%s step=%d approvals=%v, want the cordon held without asking anyone%s", inc.State, inc.StepIndex, notifier.approvals, dump(entries))
			}
			held := 0
			for _, e := range entries {
				if e.ToState == types.StateExecuting {
					t.Fatalf("a step started on a path with no usable recovery%s", dump(entries))
				}
				if e.Action == "recovery-unavailable" {
					held++
					if !strings.Contains(e.Result, "the node is not cordoned or drained") || !strings.Contains(e.Result, "add-first") {
						t.Fatalf("result=%q, want the reason and that the node was left alone", e.Result)
					}
				}
			}
			if held != 1 {
				t.Fatalf("%d recovery-unavailable rows, want exactly one across 30 passes%s", held, dump(entries))
			}

			// The policy names remove-first: the hold lifts, with a row saying so.
			c.SetEngine(ladderEngine(t, mode, recoverymode.RemoveFirst, false, cordonDrainThen("cloud-replace", "platform.replace_node", "")))
			step(t, c, st, id, 3)
			entries = trail(t, st, id)
			lifted := false
			for _, e := range entries {
				lifted = lifted || e.Action == "recovery-available"
			}
			if !lifted {
				t.Fatalf("no recovery-available row after the policy changed%s", dump(entries))
			}
		})
	}
}

func TestAddFirstHoldsBeforeCordonEvenWithRecycleEnabled(t *testing.T) {
	c, st, _ := autoFixture(t, recoverymode.Automatic, autoSettings(), 4)
	// The recycle could run, but a failed recycle falls back to a replace
	// that cannot: the drain must not happen for it.
	c.SetEngine(ladderEngine(t, recoverymode.Automatic, "", true,
		cordonDrainThen("cloud-recycle", "platform.recycle_node", "cloud-replace"),
		cordonDrainThen("cloud-replace", "platform.replace_node", "")))
	id := openOn(t, c, "n1")
	inc := step(t, c, st, id, 30)
	entries := trail(t, st, id)
	if inc.State != types.StateEvaluating || inc.StepIndex != 0 || inc.Playbook != "cloud-recycle" {
		t.Fatalf("state=%s step=%d playbook=%s, want the cordon held%s", inc.State, inc.StepIndex, inc.Playbook, dump(entries))
	}
	for _, e := range entries {
		if e.ToState == types.StateExecuting {
			t.Fatalf("a step started with an add-first replace on the path%s", dump(entries))
		}
	}
	if !strings.Contains(dump(entries), "add-first") {
		t.Fatalf("want the add-first reason%s", dump(entries))
	}
}

func TestRecycleOnlyPathWithoutRecycleEnabledHoldsBeforeCordon(t *testing.T) {
	c, st, _ := autoFixture(t, recoverymode.Automatic, autoSettings(), 4)
	c.SetEngine(ladderEngine(t, recoverymode.Automatic, recoverymode.RemoveFirst, false, cordonDrainThen("cloud-recycle", "platform.recycle_node", "")))
	id := openOn(t, c, "n1")
	inc := step(t, c, st, id, 30)
	entries := trail(t, st, id)
	if inc.State != types.StateEvaluating || inc.StepIndex != 0 || !strings.Contains(dump(entries), "recycle.enabled") {
		t.Fatalf("state=%s step=%d, want the cordon held naming recycle.enabled%s", inc.State, inc.StepIndex, dump(entries))
	}
}

func TestSkippedRecycleContinuesWithTheEscalation(t *testing.T) {
	c, st, _ := autoFixture(t, recoverymode.Automatic, autoSettings(), 4)
	c.SetEngine(ladderEngine(t, recoverymode.Automatic, recoverymode.RemoveFirst, false,
		cordonDrainThen("cloud-recycle", "platform.recycle_node", "cloud-replace"),
		cordonDrainThen("cloud-replace", "platform.replace_node", "")))
	id := openOn(t, c, "n1")
	inc := step(t, c, st, id, 100)
	entries := trail(t, st, id)
	if inc.State != types.StateResolved || inc.Playbook != "cloud-replace" || inc.Attempt != 0 {
		t.Fatalf("state=%s playbook=%s attempt=%d, want resolved by the escalation%s", inc.State, inc.Playbook, inc.Attempt, dump(entries))
	}
	skipped := -1
	for i, e := range entries {
		if e.Action == "recycle-skipped" {
			skipped = i
			if !strings.Contains(e.Result, "continuing with cloud-replace (from cloud-recycle)") {
				t.Fatalf("result=%q", e.Result)
			}
		}
		if strings.Contains(e.Action, "recycle_node") || strings.Contains(e.Result, "platform.recycle_node") {
			t.Fatalf("a skipped recycle was started%s", dump(entries))
		}
	}
	if skipped < 0 {
		t.Fatalf("no recycle-skipped row%s", dump(entries))
	}
	// Not an escalation and not a spent recovery attempt.
	for _, e := range entries {
		if e.Action == "escalate" {
			t.Fatalf("a skip was recorded as an escalation%s", dump(entries))
		}
	}
	// Before the rung started: nothing of cloud-recycle ran.
	for _, e := range entries[:skipped] {
		if e.ToState == types.StateExecuting {
			t.Fatalf("cloud-recycle started before it was skipped%s", dump(entries))
		}
	}
	// A skip is not a recovery: nothing between the drain and the skip
	// reaches VERIFYING.
	if prev := entries[skipped-1]; prev.ToState == types.StateVerifying || entries[skipped].ToState != types.StateEvaluating {
		t.Fatalf("the skip went through VERIFYING%s", dump(entries))
	}
}

func TestSkippedRecycleWithNothingAfterItHoldsBeforeCordon(t *testing.T) {
	c, st, _ := autoFixture(t, recoverymode.Automatic, autoSettings(), 4)
	// Recycle-only, no escalation. The policy allows the recycle; the
	// provider refuses it, which is known before the cordon.
	c.SetEngine(ladderEngine(t, recoverymode.Automatic, recoverymode.RemoveFirst, true, cordonDrainThen("cloud-recycle", "platform.recycle_node", "")))
	c.platform = &autoCloudPlatform{recycleRefused: true}
	id := openOn(t, c, "n1")
	inc := step(t, c, st, id, 30)
	entries := trail(t, st, id)
	assertHeldBeforeCordon(t, inc, entries, "does not allow a restart")
}

func TestRecycleSkipAlongACycleHoldsWithoutSpendingAttempts(t *testing.T) {
	c, st, _ := autoFixture(t, recoverymode.Automatic, autoSettings(), 4)
	// A -> B -> A, both recycle-only; the provider refuses the recycle.
	engine := ladderEngine(t, recoverymode.Automatic, recoverymode.RemoveFirst, true,
		cordonDrainThen("a", "platform.recycle_node", "b"),
		cordonDrainThen("b", "platform.recycle_node", "a"))
	c.SetEngine(engine)
	c.platform = &autoCloudPlatform{recycleRefused: true}
	id := openOn(t, c, "n1")
	inc := step(t, c, st, id, 30)
	entries := trail(t, st, id)
	assertHeldBeforeCordon(t, inc, entries, "the escalation path returns to a")
	if inc.Playbook != "a" {
		t.Fatalf("playbook=%s, want it left on a%s", inc.Playbook, dump(entries))
	}
}

func assertHeldBeforeCordon(t *testing.T, inc *types.Incident, entries []*types.AuditEntry, reason string) {
	t.Helper()
	if inc.State != types.StateEvaluating || inc.StepIndex != 0 || inc.Attempt != 0 {
		t.Fatalf("state=%s step=%d attempt=%d, want held at step 0 with no attempt spent%s", inc.State, inc.StepIndex, inc.Attempt, dump(entries))
	}
	for _, e := range entries {
		if e.ToState == types.StateExecuting || e.Action == "escalate" || e.Action == "recycle-skipped" {
			t.Fatalf("want nothing started, escalated or moved%s", dump(entries))
		}
	}
	if !strings.Contains(dump(entries), reason) {
		t.Fatalf("want %q in the trail%s", reason, dump(entries))
	}
}

func TestProviderRefusedRecycleIsSkipped(t *testing.T) {
	c, st, _ := autoFixture(t, recoverymode.Automatic, autoSettings(), 4)
	c.SetEngine(replaceEngine(t, recoverymode.RemoveFirst, true))
	c.platform = &autoCloudPlatform{recycleRefused: true}
	id := openOn(t, c, "n1")
	inc := step(t, c, st, id, 100)
	entries := trail(t, st, id)
	if inc.State != types.StateResolved {
		t.Fatalf("state=%s, want the replace to run after the skip%s", inc.State, dump(entries))
	}
	found := false
	for _, e := range entries {
		if e.Action == "recycle-skipped" {
			found = strings.Contains(e.Result, "the cloud provider does not allow a restart") && strings.Contains(e.Result, "continuing with step replace")
		}
	}
	if !found {
		t.Fatalf("want a recycle-skipped row naming the provider%s", dump(entries))
	}
}

func TestRecycleRefusedAfterTheRungStartedEscalates(t *testing.T) {
	c, st, _ := autoFixture(t, recoverymode.Automatic, autoSettings(), 4)
	c.SetEngine(ladderEngine(t, recoverymode.Automatic, recoverymode.RemoveFirst, true,
		cordonDrainThen("cloud-recycle", "platform.recycle_node", "cloud-replace"),
		cordonDrainThen("cloud-replace", "platform.replace_node", "")))
	provider := &autoCloudPlatform{}
	c.platform = provider
	id := openOn(t, c, "n1")
	ctx := context.Background()
	for i := 0; i < 100; i++ {
		inc := mustIncident(t, st, id)
		if inc.StepIndex == 2 && inc.State == types.StateEvaluating {
			break
		}
		c.reconcile(ctx)
	}
	if inc := mustIncident(t, st, id); inc.StepIndex != 2 || inc.Playbook != "cloud-recycle" {
		t.Fatalf("step=%d playbook=%s, want cloud-recycle waiting at its recycle%s", inc.StepIndex, inc.Playbook, dump(trail(t, st, id)))
	}
	// The provider changes its answer after the cordon and drain ran.
	provider.recycleRefused = true
	inc := step(t, c, st, id, 100)
	entries := trail(t, st, id)
	if inc.Playbook != "cloud-replace" || inc.Attempt != 1 || !strings.Contains(dump(entries), "recycle not applicable after the rung started") {
		t.Fatalf("playbook=%s attempt=%d, want a counted escalation to cloud-replace%s", inc.Playbook, inc.Attempt, dump(entries))
	}
}
