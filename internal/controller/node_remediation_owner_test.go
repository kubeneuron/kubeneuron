// SPDX-License-Identifier: Apache-2.0
package controller

import (
	"testing"

	"github.com/kubeneuron/kubeneuron/internal/playbook"
	"github.com/kubeneuron/kubeneuron/internal/recoverymode"
	"github.com/kubeneuron/kubeneuron/pkg/types"
)

func TestRepairFollowerDoesNotCordonOrRequestApproval(t *testing.T) {
	p := newSuccessorPlatform()
	c, st, notifier := successorFixture(t, recoverymode.Approval, p, recoverymode.RemoveFirst)
	ctx := t.Context()
	engine := c.runtimeConfig(ctx).Engine
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
	if err := c.IngestSignal(ctx, signal(other.Class, "n1", "")); err != nil {
		t.Fatal(err)
	}
	first, err := st.GetOpenIncident(ctx, types.Target{Node: "n1"}, other.Class)
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
	p.mu.Lock()
	defer p.mu.Unlock()
	if owners := p.cordoned["n1"]; len(owners) != 1 || owners[0] != first.ID {
		t.Errorf("cordon owners %v, want only %s", owners, first.ID)
	}
	if got := len(notifier.approvals); got != 1 {
		t.Errorf("approval requests %d, want only the owner's drain request", got)
	}
}
