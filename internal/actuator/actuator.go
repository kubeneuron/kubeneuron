// Package actuator abstracts HOW an action is executed on a node. The
// primary actuator everywhere is the kubeneuron-agent RPC (identical on K8s
// and bare metal); SSH and IPMI/Redfish are bare-metal fallbacks for when
// the agent is unreachable (e.g. a GPU falling off the bus wedged the node).
//
// The controller composes actuators in preference order and always wraps
// them in DryRun when dry-run mode is active.
package actuator

import (
	"context"
	"fmt"
	"time"

	"github.com/kubeneuron/kubeneuron/pkg/types"
)

// Actuator executes actions on nodes.
type Actuator interface {
	// Name identifies the actuator in logs and audit entries:
	// "agent" | "ssh" | "ipmi" | "dry-run(<inner>)".
	Name() string
	// Capabilities lists the action types this actuator can execute.
	Capabilities() []types.ActionType
	// Execute runs the action on the node. Implementations must be
	// idempotent with respect to Action.ID: re-executing a completed action
	// ID must be a no-op returning the original result where possible.
	Execute(ctx context.Context, node types.Node, a types.Action) (*types.ActionResult, error)
	// Healthy reports whether this actuator can currently reach the node.
	Healthy(ctx context.Context, node types.Node) error
}

// DryRun wraps an actuator and turns every Execute into a logged no-op. It
// reports the inner actuator's capabilities so playbook planning behaves
// identically in dry-run mode.
type DryRun struct {
	Inner Actuator
	// When is consulted on EVERY Execute. Nil means always simulate.
	//
	// It is a predicate rather than a bool because this wrapper used to be
	// installed conditionally, once, at process start — and the controller
	// reloads its configuration in place rather than rolling its Deployment,
	// precisely so a mode change does not need a restart. So an installation
	// that started in DryRun and was later switched to Enabled kept this
	// wrapper forever: controller-side platform steps (cordon, drain, evict,
	// replace_node) executed for real while every agent step returned a
	// successful "DRY-RUN: would execute ..." — the GPU never reset, the node
	// never rebooted, the step counted as executed, the quiet window quiet
	// because the node had been drained, and the incident RESOLVED with the
	// fault untouched and the node handed back to the scheduler.
	//
	// Wrap unconditionally and decide here, and that cannot happen.
	When func() bool
}

// undoActions are executed even in dry-run, because simulating them is not
// "changing nothing" — it is leaving a change already made in place forever.
//
// restore_accelerator_host reverses quiesce_accelerator_host: the agent stopped
// the persistence daemon and turned persistence mode off so a reset could
// proceed. Only this action puts the node back. Wrapping the actuator
// unconditionally — correct for everything else — made the janitor's restore
// return a synthetic OK the moment an operator switched a running installation
// to DryRun to STOP damage. The janitor then treated the node as restored,
// cleared the durable marker that would have retried it, and never looked
// again: the node's GPU monitoring stays off permanently, silently, on the one
// path whose whole purpose is to undo.
//
// The rule is narrow on purpose. An action belongs here only if KubeNeuron
// made the change it reverses, so refusing to run it cannot cause a state the
// installation did not already create.
var undoActions = map[types.ActionType]bool{
	types.ActionRestoreAcceleratorHost: true,
}

func (d *DryRun) simulating(a types.Action) bool {
	if undoActions[a.Type] {
		return false
	}
	return d.When == nil || d.When()
}

// simulatingAny answers for no particular action, which is all Name() can ask.
func (d *DryRun) simulatingAny() bool { return d.When == nil || d.When() }

var _ Actuator = (*DryRun)(nil)

// Name implements Actuator.
func (d *DryRun) Name() string {
	if !d.simulatingAny() {
		return d.Inner.Name()
	}
	return "dry-run(" + d.Inner.Name() + ")"
}

// Capabilities implements Actuator.
func (d *DryRun) Capabilities() []types.ActionType { return d.Inner.Capabilities() }

// Execute records what would have happened without doing it.
func (d *DryRun) Execute(ctx context.Context, node types.Node, a types.Action) (*types.ActionResult, error) {
	if !d.simulating(a) {
		return d.Inner.Execute(ctx, node, a)
	}
	now := time.Now()
	return &types.ActionResult{
		ActionID:   a.ID,
		OK:         true,
		Output:     fmt.Sprintf("DRY-RUN: would execute %s on %s via %s", a.Type, node.Name, d.Inner.Name()),
		StartedAt:  now,
		FinishedAt: now,
	}, nil
}

// Healthy delegates to the inner actuator.
func (d *DryRun) Healthy(ctx context.Context, node types.Node) error {
	return d.Inner.Healthy(ctx, node)
}

// Chain tries actuators in order, using the first healthy one that has the
// capability. This encodes the "agent first, SSH/IPMI fallback" policy.
type Chain struct {
	Actuators []Actuator
}

var _ Actuator = (*Chain)(nil)

// Name implements Actuator.
func (c *Chain) Name() string { return "chain" }

// Capabilities is the union of all chained actuators' capabilities.
func (c *Chain) Capabilities() []types.ActionType {
	seen := map[types.ActionType]bool{}
	var out []types.ActionType
	for _, a := range c.Actuators {
		for _, t := range a.Capabilities() {
			if !seen[t] {
				seen[t] = true
				out = append(out, t)
			}
		}
	}
	return out
}

// Execute picks the first healthy, capable actuator.
func (c *Chain) Execute(ctx context.Context, node types.Node, a types.Action) (*types.ActionResult, error) {
	var lastErr error
	for _, act := range c.Actuators {
		if !hasCapability(act, a.Type) {
			continue
		}
		if err := act.Healthy(ctx, node); err != nil {
			lastErr = fmt.Errorf("%s unhealthy for %s: %w", act.Name(), node.Name, err)
			continue
		}
		return act.Execute(ctx, node, a)
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("no actuator supports %s", a.Type)
	}
	return nil, lastErr
}

// Healthy reports success when any chained actuator is healthy.
func (c *Chain) Healthy(ctx context.Context, node types.Node) error {
	var lastErr error
	for _, act := range c.Actuators {
		if lastErr = act.Healthy(ctx, node); lastErr == nil {
			return nil
		}
	}
	return lastErr
}

func hasCapability(a Actuator, t types.ActionType) bool {
	for _, c := range a.Capabilities() {
		if c == t {
			return true
		}
	}
	return false
}
