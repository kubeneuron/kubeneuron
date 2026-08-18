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
	return []types.ActionType{types.ActionReboot}
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
		t.Fatalf("Name() = %q, want the inner name once it is really executing — this string reaches the audit trail", d.Name())
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
