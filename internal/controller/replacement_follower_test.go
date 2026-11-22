// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"testing"

	"github.com/kubeneuron/kubeneuron/internal/cloud/resizediff"
	"github.com/kubeneuron/kubeneuron/internal/playbook"
	"github.com/kubeneuron/kubeneuron/internal/recoverymode"
	"github.com/kubeneuron/kubeneuron/internal/store/sqlite"
	"github.com/kubeneuron/kubeneuron/pkg/cloud"
	"github.com/kubeneuron/kubeneuron/pkg/types"
)

// A second incident on a node that another incident is replacing waits, then
// takes the owner's verified successor as its own evidence: it is checked on
// the new node and does not ask for a second replacement.
func TestFollowerTakesTheOwnersVerifiedSuccessor(t *testing.T) {
	c, st, p, owner, follower := followOwnerReplacement(t, recoverymode.AddFirst, successorNodeName, "")
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
	c, st, p, _, follower := followOwnerReplacement(t, recoverymode.RemoveFirst, poolNodeName, "")
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

// The instance the pool's resize added, verified as the owner's
// replacement, is the follower's evidence too: it is checked on that node.
func TestFollowerTakesTheOwnersResizeDiffReplacement(t *testing.T) {
	const named = "aws:///zone/i-n10"
	c, st, p, owner, follower := followOwnerReplacement(t, recoverymode.RemoveFirst, poolNodeName, named)
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
	if proof["owner_id"] != owner || proof["replacement_node"] != poolNodeName || proof["replacement_node_uid"] != "uid-"+poolNodeName ||
		proof["replacement_correlation"] != replacementByResizeDiff || proof["replacement_provider_id"] != named {
		t.Fatalf("follower's verification = %v", proof)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if n := len(p.replacedBy) + len(p.plain); n != 1 {
		t.Fatalf("%d replacements for two incidents on one node, want 1", n)
	}
}

// A resolution that names an instance the owner never recorded is not
// evidence for the follower: it goes to an operator.
func TestFollowerRefusesAnUnrecordedResizeDiffInstance(t *testing.T) {
	inc := &types.Incident{ID: "owner"}
	trail := []*types.AuditEntry{{Action: replacementInstanceAction, Params: map[string]string{"provider_id": "aws:///zone/i-n10"}}}
	if !ownerFoundInstance(inc, trail, "aws:///zone/i-n10") {
		t.Fatal("the recorded instance was not found")
	}
	if ownerFoundInstance(inc, trail, "aws:///zone/i-n11") {
		t.Fatal("an instance the owner did not record was accepted")
	}
	trail[0].DryRun = true
	if ownerFoundInstance(inc, trail, "aws:///zone/i-n10") {
		t.Fatal("a dry-run row was accepted")
	}
	plan := &types.RemoveFirstRecord{RemoveFirstPlan: cloud.RemoveFirstPlan{Phase: cloud.RemoveFirstDone,
		State: resizediff.WithInstance(nil, "gce://p/us-central1-a/n10")}, Outcome: types.RemoveFirstRestored}
	inc.RemoveFirst = plan
	if !ownerFoundInstance(inc, nil, "gce://p/us-central1-a/n10") {
		t.Fatal("the instance of a restored plan was not found")
	}
	plan.Outcome = types.RemoveFirstNeedsOperator
	if ownerFoundInstance(inc, nil, "gce://p/us-central1-a/n10") {
		t.Fatal("the instance of a plan left for an operator was accepted")
	}
}

// followOwnerReplacement opens an owner incident and, once it runs, a second
// one on the same node, then brings the owner to RESOLVED on newNode. named
// is the instance the owner's replacement reports its pool's resize added.
func followOwnerReplacement(t *testing.T, strategy recoverymode.Strategy, newNode, named string) (*Controller, *sqlite.Store, *poolPlatform, string, *types.Incident) {
	t.Helper()
	ctx := context.Background()
	p := &poolPlatform{successorPlatform: newSuccessorPlatform(), named: named}
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
