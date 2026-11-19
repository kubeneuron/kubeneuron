// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"testing"

	"github.com/kubeneuron/kubeneuron/internal/playbook"
	"github.com/kubeneuron/kubeneuron/internal/recoverymode"
	"github.com/kubeneuron/kubeneuron/internal/store/sqlite"
	"github.com/kubeneuron/kubeneuron/pkg/types"
)

// A second incident on a node that another incident is replacing waits, then
// takes the owner's verified successor as its own evidence: it is checked on
// the new node and does not ask for a second replacement.
func TestFollowerTakesTheOwnersVerifiedSuccessor(t *testing.T) {
	c, st, p, owner, follower := followOwnerReplacement(t, recoverymode.AddFirst, successorNodeName)
	target := follower.Target
	inc := untilState(t, c, st, follower.ID, func(inc *types.Incident) bool {
		return inc.State == types.StateVerifying || inc.State.Halted()
	})
	if inc.State != types.StateVerifying {
		t.Fatalf("follower state = %s%s", inc.State, dump(trail(t, st, follower.ID)))
	}
	var proof map[string]string
	for _, e := range trail(t, st, follower.ID) {
		if e.Action == "verify-owner-replacement" {
			proof = e.Params
		}
	}
	if proof["owner_id"] != owner || proof["replacement_node"] != successorNodeName ||
		proof["replacement_node_uid"] != "uid-"+successorNodeName || proof["replacement_correlation"] != "successor" {
		t.Fatalf("follower's verification = %v", proof)
	}
	if inc.Target != target {
		t.Fatalf("follower target changed from %+v to %+v", target, inc.Target)
	}
	step(t, c, st, follower.ID, 10)
	p.mu.Lock()
	defer p.mu.Unlock()
	if n := len(p.replacedBy) + len(p.plain); n != 1 {
		t.Fatalf("%d replacements for two incidents on one node, want 1", n)
	}
}

// A node found only as the one that joined the pool is not proof enough for
// a second incident: it goes to an operator, without a second replacement.
func TestFollowerOfAPoolReplacementGoesToAnOperator(t *testing.T) {
	c, st, p, _, follower := followOwnerReplacement(t, recoverymode.RemoveFirst, poolNodeName)
	inc := untilState(t, c, st, follower.ID, func(inc *types.Incident) bool {
		return inc.State == types.StateVerifying || inc.State.Halted()
	})
	if inc.State != types.StateNeedsHuman {
		t.Fatalf("follower state = %s%s", inc.State, dump(trail(t, st, follower.ID)))
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if n := len(p.replacedBy) + len(p.plain); n != 1 {
		t.Fatalf("%d replacements for two incidents on one node, want 1", n)
	}
}

// followOwnerReplacement opens an owner incident and, once it runs, a second
// one on the same node, then brings the owner to RESOLVED on newNode.
func followOwnerReplacement(t *testing.T, strategy recoverymode.Strategy, newNode string) (*Controller, *sqlite.Store, *poolPlatform, string, *types.Incident) {
	t.Helper()
	ctx := context.Background()
	p := &poolPlatform{successorPlatform: newSuccessorPlatform()}
	c, st, _ := successorFixture(t, recoverymode.Automatic, p.successorPlatform, strategy)
	c.platform = p
	engine := c.runtimeConfig(ctx).Engine
	book, _ := engine.Playbook("cloud-replace")
	a, _ := engine.PolicyFor(types.ClassFellOffBus, "")
	b := a
	b.Name, b.Class, b.ReplacementStrategy = "accelerator-fault", types.ClassAcceleratorFault, strategy
	engine, err := playbook.NewEngine(map[string]*playbook.Playbook{book.Name: book}, []playbook.Policy{a, b})
	if err != nil {
		t.Fatal(err)
	}
	c.SetEngine(engine)
	registerSuccessorAgent(t, st, 8)
	registerOldNode(t, st, 8, types.AcceleratorVendorNVIDIA)

	owner := openVendorIncident(t, c, "n1", types.AcceleratorVendorNVIDIA)
	untilState(t, c, st, owner, func(inc *types.Incident) bool { return inc.State == types.StateExecuting })
	if err := c.ingest(ctx, signal(types.ClassAcceleratorFault, "n1", "")); err != nil {
		t.Fatal(err)
	}
	follower, err := st.GetOpenIncident(ctx, types.Target{Node: "n1"}, types.ClassAcceleratorFault)
	if err != nil {
		t.Fatal(err)
	}
	untilState(t, c, st, follower.ID, func(*types.Incident) bool {
		return auditHas(trail(t, st, follower.ID), "wait-for-node-repair", "")
	})
	untilState(t, c, st, owner, func(inc *types.Incident) bool { return inc.State == types.StateVerifying })
	if newNode != successorNodeName {
		registerAgentAs(t, st, newNode, "uid-"+newNode)
	}
	reportFrom(t, st, newNode, "uid-"+newNode, types.AcceleratorVendorNVIDIA, types.AcceleratorReadinessReady)
	if inc := untilState(t, c, st, owner, func(inc *types.Incident) bool { return inc.State != types.StateVerifying }); inc.State != types.StateResolved {
		t.Fatalf("owner state = %s%s", inc.State, dump(trail(t, st, owner)))
	}
	return c, st, p, owner, follower
}
