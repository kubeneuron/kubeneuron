// SPDX-License-Identifier: Apache-2.0
package controller

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/kubeneuron/kubeneuron/internal/playbook"
	"github.com/kubeneuron/kubeneuron/internal/recoverymode"
	"github.com/kubeneuron/kubeneuron/internal/store"
	"github.com/kubeneuron/kubeneuron/pkg/cloud"
	"github.com/kubeneuron/kubeneuron/pkg/types"
)

func TestRepairFollowerDoesNotCordonOrRequestApproval(t *testing.T) {
	p := newSuccessorPlatform()
	c, st, notifier := successorFixture(t, recoverymode.Approval, p, recoverymode.RemoveFirst)
	ctx := t.Context()
	configureRepairClasses(t, c)
	if err := c.IngestSignal(ctx, signal(types.ClassAcceleratorFault, "n1", "")); err != nil {
		t.Fatal(err)
	}
	first, err := st.GetOpenIncident(ctx, types.Target{Node: "n1"}, types.ClassAcceleratorFault)
	if err != nil {
		t.Fatal(err)
	}
	waitSettled(t, c, st, first.ID, func(i *types.Incident) bool { return i.State == types.StateAwaitingApproval && i.StepIndex == 1 })

	followerSignal := signal(types.ClassFellOffBus, "n1", "GPU-1")
	if err := c.IngestSignal(ctx, followerSignal); err != nil {
		t.Fatal(err)
	}
	follower, err := st.GetOpenIncident(ctx, followerSignal.Target, followerSignal.Class)
	if err != nil {
		t.Fatal(err)
	}
	step(t, c, st, follower.ID, 20)
	if got := len(notifier.approvals); got != 1 {
		t.Errorf("approval requests %d, want only the owner's drain request", got)
	}
	waitSettled(t, c, st, first.ID, func(i *types.Incident) bool {
		if i.State == types.StateAwaitingApproval {
			if err := c.DecideApproval(ctx, i.ID, "alice", "test", types.ApprovalApproved, i.ApprovalEpoch, "", ""); err != nil {
				t.Fatal(err)
			}
		}
		return i.State == types.StateVerifying
	})
	step(t, c, st, follower.ID, 20)
	if got := len(notifier.approvals); got != 2 {
		t.Errorf("approval requests %d, want the owner's drain and replace", got)
	}
	checkCalls := func() {
		p.mu.Lock()
		defer p.mu.Unlock()
		if owners := p.cordoned["n1"]; len(owners) != 1 || owners[0] != first.ID {
			t.Errorf("cordon owners %v, want only %s", owners, first.ID)
		}
		if len(p.drained) != 1 || len(p.plain) != 1 {
			t.Errorf("drains %v replacements %v, want one each", p.drained, p.plain)
		}
	}
	var waits int
	for _, entry := range trail(t, st, follower.ID) {
		if entry.Action == "wait-for-node-repair" {
			waits++
			if entry.Params["owner_id"] != first.ID {
				t.Fatalf("wait recorded owner %q, want %q", entry.Params["owner_id"], first.ID)
			}
		}
	}
	if waits != 1 {
		t.Fatalf("wait audit entries = %d, want one", waits)
	}
	checkCalls()
	owner, err := st.GetIncident(ctx, first.ID)
	if err != nil {
		t.Fatal(err)
	}
	// Supply the same inventory evidence that live cloud verification records.
	if err := c.transition(ctx, owner, types.StateResolved, "system", "resolve", "verified",
		map[string]string{"infrastructure_evidence": "verified_recycle_inventory"}); err != nil {
		t.Fatal(err)
	}
	waitSettled(t, c, st, follower.ID, func(i *types.Incident) bool { return i.State == types.StateVerifying })
	checkCalls()
	if err := c.IngestSignal(ctx, followerSignal); err != nil {
		t.Fatal(err)
	}
	got := step(t, c, st, follower.ID, 20)
	if got.State != types.StateNeedsHuman || !auditHas(trail(t, st, follower.ID), "quarantine", first.ID) {
		t.Fatalf("recurring follower signal: state=%s audit=%s", got.State, dump(trail(t, st, follower.ID)))
	}
	checkCalls()
}

func configureRepairClasses(t *testing.T, c *Controller) {
	t.Helper()
	engine := c.runtimeConfig(t.Context()).Engine
	book, _ := engine.Playbook("cloud-replace")
	pol, _ := engine.PolicyFor(types.ClassFellOffBus, "")
	other := pol
	other.Name = "node-fault"
	other.Class = types.ClassAcceleratorFault
	engine, err := playbook.NewEngine(map[string]*playbook.Playbook{book.Name: book}, []playbook.Policy{pol, other})
	if err != nil {
		t.Fatal(err)
	}
	c.SetEngine(engine)
}

func TestAutomaticRepairFollowerDoesNotRepeatReplacement(t *testing.T) {
	p := newSuccessorPlatform()
	c, st, notifier := successorFixture(t, recoverymode.Automatic, p, recoverymode.RemoveFirst)
	ctx := t.Context()
	configureRepairClasses(t, c)
	owner := openIncidentFor(t, c, types.ClassFellOffBus)
	waitSettled(t, c, st, owner, func(i *types.Incident) bool { return i.StepIndex == 1 })
	second := signal(types.ClassAcceleratorFault, "n1", "")
	if err := c.IngestSignal(ctx, second); err != nil {
		t.Fatal(err)
	}
	follower, err := st.GetOpenIncident(ctx, second.Target, second.Class)
	if err != nil {
		t.Fatal(err)
	}
	step(t, c, st, follower.ID, 10)
	c = restartWith(t, c, st, p)
	// Reconcile the follower first after leadership changes.
	step(t, c, st, follower.ID, 10)
	waitSettled(t, c, st, owner, func(i *types.Incident) bool { return i.State == types.StateVerifying })
	step(t, c, st, follower.ID, 20)
	if got := mustIncident(t, st, follower.ID); got.State != types.StateEvaluating || got.StepIndex != 0 {
		t.Fatalf("follower reached %s step %d", got.State, got.StepIndex)
	}
	// An operator closing the owner does not verify replacement for the follower.
	if err := c.ResolveIncident(ctx, owner, "alice", "closed manually"); err != nil {
		t.Fatal(err)
	}
	step(t, c, st, follower.ID, 20)
	for _, entry := range trail(t, st, follower.ID) {
		if entry.Action == "verify-owner-replacement" {
			t.Fatal("manual closure used as verified replacement evidence")
		}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if owners := p.cordoned["n1"]; len(owners) != 1 || owners[0] != owner || len(p.drained) != 1 || len(p.plain) != 1 || len(notifier.approvals) != 0 {
		t.Fatalf("cordon owners=%v drains=%v replacements=%v approvals=%d", owners, p.drained, p.plain, len(notifier.approvals))
	}
}

func TestInventoryNodeUIDSharesRepairApprovalWithoutCloudIdentity(t *testing.T) {
	c, st, notifier := walkFixture(t, false)
	ctx := t.Context()
	if err := st.UpsertNode(ctx, &types.Node{Name: "n1", UID: "inventory-uid"}); err != nil {
		t.Fatal(err)
	}
	book := &playbook.Playbook{Name: "repair", Target: "node", Steps: []playbook.Step{
		{Name: "cordon", Action: "platform.cordon", Approval: "required"},
	}}
	classes := []types.ProblemClass{types.ClassFellOffBus, types.ClassAcceleratorFault}
	var policies []playbook.Policy
	for _, class := range classes {
		policies = append(policies, playbook.Policy{Name: string(class), Class: class, Playbook: book.Name, Mode: recoverymode.Approval})
	}
	engine, err := playbook.NewEngine(map[string]*playbook.Playbook{book.Name: book}, policies)
	if err != nil {
		t.Fatal(err)
	}
	c.SetEngine(engine)
	var owner string
	for _, class := range classes {
		sig := signal(class, "n1", "")
		if err := c.IngestSignal(ctx, sig); err != nil {
			t.Fatal(err)
		}
		inc, err := st.GetOpenIncident(ctx, sig.Target, class)
		if err != nil {
			t.Fatal(err)
		}
		if inc.CloudIdentity != nil || inc.RepairNodeUID != "inventory-uid" {
			t.Fatalf("unexpected identity for %s", class)
		}
		got := step(t, c, st, inc.ID, 10)
		if owner == "" {
			owner = inc.ID
			if got.State != types.StateAwaitingApproval {
				t.Fatalf("owner state = %s", got.State)
			}
			continue
		}
		status := types.RecoveryAvailabilityFromAudit(trail(t, st, inc.ID))
		if got.State != types.StateEvaluating || status == nil || status.Source != "node-repair" || !strings.Contains(status.Reason, owner) {
			t.Fatalf("follower state=%s recovery=%+v", got.State, status)
		}
	}
	if len(notifier.approvals) != 1 {
		t.Fatalf("approval requests = %d, want one", len(notifier.approvals))
	}
	// An incident opened on a different node loses its inventory record
	// before its first step. It must stop before requesting approval.
	if err := st.UpsertNode(ctx, &types.Node{Name: "n2", UID: "other-uid"}); err != nil {
		t.Fatal(err)
	}
	sig := signal(classes[0], "n2", "GPU-new")
	if err := c.IngestSignal(ctx, sig); err != nil {
		t.Fatal(err)
	}
	third, err := st.GetOpenIncident(ctx, sig.Target, sig.Class)
	if err != nil {
		t.Fatal(err)
	}
	c.store = missingRepairInventory{st, "n2"}
	if got := step(t, c, st, third.ID, 10); got.State != types.StateNeedsHuman || len(notifier.approvals) != 1 {
		t.Fatalf("missing node: state=%s approvals=%d", got.State, len(notifier.approvals))
	}
	c.SetTimings(0, time.Nanosecond)
	drive(t, c, st, owner, types.StateExpired)
	// A newly opened incident can now claim the original node incarnation.
	if err := st.WithTx(ctx, func(tx store.Tx) error {
		inc := &types.Incident{ID: "next", Target: types.Target{Node: "n1"}, Class: types.ClassThermal, State: types.StateOpen}
		if err := tx.CreateIncident(ctx, inc); err != nil {
			return err
		}
		got, err := tx.ClaimNodeRemediation(ctx, "n1", "inventory-uid", inc.ID)
		if err == nil && got != inc.ID {
			t.Errorf("after expiration owner=%q, want %q", got, inc.ID)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}

}

type missingRepairInventory struct {
	store.Store
	missing string
}

func (s missingRepairInventory) GetNode(ctx context.Context, node string) (*types.Node, error) {
	if node == s.missing {
		return nil, store.ErrNotFound
	}
	return s.Store.GetNode(ctx, node)
}

func TestInventoryFollowerVerifiesAfterOriginalNodeDisappears(t *testing.T) {
	c, st, _ := walkFixture(t, false)
	ctx := t.Context()
	target := types.Target{Node: "n1"}
	if err := st.WithTx(ctx, func(tx store.Tx) error {
		for _, id := range []string{"owner", "follower"} {
			inc := &types.Incident{ID: id, Target: target, RepairNodeUID: "original", Class: types.ClassFellOffBus, State: types.StateEvaluating, Playbook: "fell-off-bus"}
			if id == "owner" {
				inc.CloudIdentity = &cloud.NodeIdentity{NodeUID: "original"}
				inc.Class = types.ClassAcceleratorFault
				inc.State = types.StateResolved
			}
			if err := tx.CreateIncident(ctx, inc); err != nil {
				return err
			}
			if _, err := tx.ClaimNodeRemediation(ctx, target.Node, inc.RepairNodeUID, id); err != nil {
				return err
			}
		}
		if err := tx.AppendAudit(ctx, &types.AuditEntry{IncidentID: "owner", Result: auditStepOutcomeResult("platform.replace_node", false, true)}); err != nil {
			return err
		}
		if err := tx.AppendAudit(ctx, &types.AuditEntry{IncidentID: "owner", Action: "resolve", FromState: types.StateVerifying, ToState: types.StateResolved, Params: map[string]string{"infrastructure_evidence": "verified_recycle_inventory"}}); err != nil {
			return err
		}
		return tx.ReleaseNodeRemediation(ctx, "owner")
	}); err != nil {
		t.Fatal(err)
	}
	c.store = missingRepairInventory{st, "n1"}
	waitSettled(t, c, st, "follower", func(i *types.Incident) bool { return i.State == types.StateVerifying })
	for _, entry := range trail(t, st, "follower") {
		if entry.Action == "verify-owner-replacement" && entry.Params["owner_id"] == "owner" {
			return
		}
	}
	t.Fatal("missing verification transition linked to original owner")
}

func TestSameRepairNodeUsesRecordedInventoryUID(t *testing.T) {
	target := types.Target{Node: "worker"}
	owner := &types.Incident{Target: target, CloudIdentity: &cloud.NodeIdentity{NodeUID: "original"}}
	follower := &types.Incident{Target: target, RepairNodeUID: "original"}
	if !sameRepairNode(follower, owner) {
		t.Fatal("matching inventory UID did not identify the owner node")
	}
	for _, uid := range []string{"replacement", ""} {
		follower.RepairNodeUID = uid
		if sameRepairNode(follower, owner) {
			t.Fatalf("matched owner against inventory UID %q", uid)
		}
	}
}

type missingRepairTx struct {
	store.Tx
	missing string
}

func (s missingRepairInventory) WithTx(ctx context.Context, f func(store.Tx) error) error {
	return s.Store.WithTx(ctx, func(tx store.Tx) error { return f(missingRepairTx{tx, s.missing}) })
}

func (s missingRepairTx) GetNode(ctx context.Context, node string) (*types.Node, error) {
	if node == s.missing {
		return nil, store.ErrNotFound
	}
	return s.Tx.GetNode(ctx, node)
}

func TestPolicyHeldIncidentLeavesRepairAvailable(t *testing.T) {
	p := newSuccessorPlatform()
	p.unavailable = cloud.ErrAddFirstUnavailable
	c, st, _ := successorFixture(t, recoverymode.Automatic, p, recoverymode.AddFirst)
	ctx := t.Context()
	engine := c.runtimeConfig(ctx).Engine
	book, _ := engine.Playbook("cloud-replace")
	a, _ := engine.PolicyFor(types.ClassFellOffBus, "")
	b := a
	b.Name, b.Class, b.ReplacementStrategy = "remove-first", types.ClassAcceleratorFault, recoverymode.RemoveFirst
	engine, err := playbook.NewEngine(map[string]*playbook.Playbook{book.Name: book}, []playbook.Policy{a, b})
	if err != nil {
		t.Fatal(err)
	}
	c.SetEngine(engine)
	held := openOn(t, c, "n1")
	step(t, c, st, held, 20)
	if !auditHas(trail(t, st, held), "recovery-unavailable", "") {
		t.Fatal("expected add-first policy hold")
	}
	sig := signal(b.Class, "n1", "")
	if err := c.IngestSignal(ctx, sig); err != nil {
		t.Fatal(err)
	}
	other, err := st.GetOpenIncident(ctx, sig.Target, b.Class)
	if err != nil {
		t.Fatal(err)
	}
	waitSettled(t, c, st, other.ID, func(i *types.Incident) bool { return i.State == types.StateVerifying })
	p.mu.Lock()
	defer p.mu.Unlock()
	if ids := p.cordoned["n1"]; len(ids) != 1 || ids[0] != other.ID || len(p.drained) != 1 || len(p.plain) != 1 {
		t.Fatalf("cordons=%v drains=%v replacements=%v", ids, p.drained, p.plain)
	}
}
