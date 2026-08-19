package actuator

import (
	"context"
	"strings"
	"testing"

	"github.com/kubeneuron/kubeneuron/pkg/types"
)

type countingActuator struct {
	calls int
}

func (c *countingActuator) Name() string { return "counting" }
func (c *countingActuator) Capabilities() []types.ActionType {
	return []types.ActionType{types.ActionReboot, types.ActionRestoreAcceleratorHost}
}
func (c *countingActuator) Healthy(context.Context, types.Node) error { return nil }
func (c *countingActuator) Execute(context.Context, types.Node, types.Action) (*types.ActionResult, error) {
	c.calls++
	return &types.ActionResult{OK: true, Output: "really rebooted"}, nil
}

// TestDryRunFollowsTheLiveMode covers enabling a running installation.
//
// The wrapper used to be installed only when the gate said dry-run at process
// START. The controller reloads its configuration in place — the operator keeps
// the config digest off the pod template on purpose, because a rollout
// deadlocks under leader election — so switching executionMode to Enabled never
// restarts the process, and the wrapper stayed on forever.
//
// The result was worse than "nothing happens", because it was not uniform:
// controller-side platform steps (cordon, drain, evict, replace_node) go
// straight to the platform and went live immediately, while every agent step
// returned a SUCCESSFUL "DRY-RUN: would execute ..." So the ladder cordoned and
// drained the node for real, counted the reboot as executed, passed
// verification on a quiet window that was quiet because the node had been
// drained, resolved the incident, and handed the machine back to the scheduler
// with the fault untouched.
func TestDryRunFollowsTheLiveMode(t *testing.T) {
	inner := &countingActuator{}
	dryRun := true
	d := &DryRun{Inner: inner, When: func() bool { return dryRun }}
	ctx := context.Background()
	node := types.Node{Name: "node-a"}
	act := types.Action{ID: "a1", Type: types.ActionReboot}

	res, err := d.Execute(ctx, node, act)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(res.Output, "DRY-RUN:") || inner.calls != 0 {
		t.Fatalf("dry-run mode reached the inner actuator: output=%q calls=%d", res.Output, inner.calls)
	}
	if !strings.HasPrefix(d.Name(), "dry-run(") {
		t.Fatalf("Name() = %q, want it to say dry-run while simulating", d.Name())
	}

	// The operator enables the installation. No restart happens.
	dryRun = false

	res, err = d.Execute(ctx, node, act)
	if err != nil {
		t.Fatal(err)
	}
	if inner.calls != 1 {
		t.Fatalf("after enabling, the action was still simulated (%q); every agent step — reboot, reset, "+
			"driver reinstall — would report success while the fault went untouched, and the ladder would "+
			"resolve the incident and give the node back", res.Output)
	}
	if d.Name() != "counting" {
		// Not because the audit reads it — audit rows carry the step name and
		// the incident's own dry-run flag, never the actuator's name. The one
		// consumer is Chain.Execute's error string, so this is about a
		// diagnostic staying true, not about corrupting a record.
		t.Fatalf("Name() = %q, want the inner name once it is really executing", d.Name())
	}
}

// TestDryRunWithNoPredicateAlwaysSimulates keeps the zero value safe: a
// wrapper built without a predicate must never reach the inner actuator.
func TestDryRunWithNoPredicateAlwaysSimulates(t *testing.T) {
	inner := &countingActuator{}
	d := &DryRun{Inner: inner}
	if _, err := d.Execute(context.Background(), types.Node{Name: "n"}, types.Action{ID: "a"}); err != nil {
		t.Fatal(err)
	}
	if inner.calls != 0 {
		t.Fatal("a DryRun wrapper with no predicate executed for real")
	}
}

// TestDryRunNeverSimulatesAnUndo covers the janitor's host restore.
//
// The wrapper became unconditional so that enabling a running installation
// would stop leaving agent steps simulated. That was right for every action
// that MAKES a change and wrong for the one that reverses one. A playbook
// quiesces the accelerator host — persistence daemon stopped, persistence mode
// off — and only restore_accelerator_host puts it back. Simulate that and the
// janitor reads a synthetic OK as success, clears the durable marker that
// would have retried it, and never looks at the node again: its GPU monitoring
// stays off permanently.
//
// "A dry-run installation changes nothing" is a promise about what KubeNeuron
// DOES, not a licence to abandon what it already did.
func TestDryRunNeverSimulatesAnUndo(t *testing.T) {
	inner := &countingActuator{}
	d := &DryRun{Inner: inner, When: func() bool { return true }}

	res, err := d.Execute(context.Background(), types.Node{Name: "n"},
		types.Action{ID: "a", Type: types.ActionRestoreAcceleratorHost})
	if err != nil {
		t.Fatal(err)
	}
	if inner.calls != 1 {
		t.Fatalf("restore_accelerator_host was simulated (%q); the node it undoes for stays quiesced forever "+
			"and the janitor clears the marker that would have retried", res.Output)
	}

	// And the ordinary case is unaffected: a change-making action still
	// simulates under the same wrapper.
	before := inner.calls
	if _, err := d.Execute(context.Background(), types.Node{Name: "n"},
		types.Action{ID: "b", Type: types.ActionReboot}); err != nil {
		t.Fatal(err)
	}
	if inner.calls != before {
		t.Fatal("the undo exemption leaked to reboot; dry-run must still simulate a change")
	}
}
