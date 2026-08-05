package controller

import (
	"context"
	"strings"

	"github.com/kubeneuron/kubeneuron/internal/platform"
	"github.com/kubeneuron/kubeneuron/internal/store"
	"github.com/kubeneuron/kubeneuron/pkg/types"
)

// This file is the scheduler-feedback seam: a node with an open incident stops
// attracting NEW GPU work before any playbook cordons it, and stops being
// marked the moment that incident halts.
//
// Why it exists: between the first fault signal and the cordon step there is a
// window — observation, threshold, evaluation, often a human approval — during
// which the scheduler keeps placing fresh jobs on a device that is already
// failing. Every job started in that window is one more workload the
// remediation then has to evict. The taint closes the window and takes nothing
// away: PreferNoSchedule/NoSchedule affect placement only, so nothing already
// running on the node is disturbed.
//
// Why it is opt-in and off by default: scheduling is the one thing every
// workload in the cluster depends on, and a fault class that turns out to be
// benign would otherwise start steering work away from healthy hardware
// without anybody having asked for that.

// SetDegradedTaintPolicy installs the compiled spec.safety.taintDegradedNodes.
// Test/dev hook; the reload path installs a whole RuntimeConfig snapshot.
func (c *Controller) SetDegradedTaintPolicy(policy DegradedTaintPolicy) {
	c.mutateRuntimeConfig(func(rc *RuntimeConfig) { rc.DegradedTaint = policy })
}

func (c *Controller) degradedTaintPolicy(ctx context.Context) DegradedTaintPolicy {
	return c.runtimeConfig(ctx).DegradedTaint
}

// nodeTainter returns the platform's taint implementation, or nil when the
// platform has no scheduler to talk to (baremetal, the nil platform in tests).
func (c *Controller) nodeTainter() platform.NodeTainter {
	tainter, ok := c.platform.(platform.NodeTainter)
	if !ok {
		return nil
	}
	return tainter
}

// degradedTaintValue is what the mark says about itself. The incident's problem
// class, not its ID: a taint value is a Kubernetes label value (63 characters,
// a restricted alphabet), incident IDs embed a node name and can exceed both,
// and "why is this node marked" is answered better by "ecc-dbe" than by an ID
// nobody can read. Ownership is carried by the KEY, which is namespaced to this
// product, so nothing depends on the value being unique.
func degradedTaintValue(inc *types.Incident) string {
	value := sanitizeTaintValue(string(inc.Class))
	if value == "" {
		return "incident"
	}
	return value
}

// sanitizeTaintValue reduces a class to something the API will accept, so a
// future class name with an unexpected character cannot make every taint write
// fail with a validation error at 3am.
func sanitizeTaintValue(in string) string {
	var b strings.Builder
	for _, r := range in {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	value := strings.Trim(b.String(), "-_.")
	if len(value) > 63 {
		value = strings.Trim(value[:63], "-_.")
	}
	return value
}

// syncDegradedTaint is the fast path, called after a state transition commits.
// It marks a node as its incident starts being worked and clears the mark when
// that incident halts. Both directions are best-effort by design: the janitor
// below reconciles from what the CLUSTER says, so a failure here costs
// timeliness and never correctness.
func (c *Controller) syncDegradedTaint(ctx context.Context, inc *types.Incident, from, to types.IncidentState) {
	tainter := c.nodeTainter()
	policy := c.degradedTaintPolicy(ctx)
	if tainter == nil || inc.Target.Node == "" || !policy.Enabled {
		// Disabled installs — the overwhelming majority — leave this path
		// costing nothing: no store query, no apiserver call, per transition.
		// Cleanup after the switch is turned OFF belongs to the janitor, which
		// is the only place that can find marks this process never placed.
		return
	}
	switch {
	case to.Halted():
		// Removed on EVERY halting state, including NEEDS_HUMAN — deliberately
		// unlike the cordon, which a human-owned incident keeps. A cordon is a
		// judgement that the node is unfit and a human will come back to it; this
		// mark's whole lifetime is "an incident is being worked", and nothing
		// would ever come back to clear it. The node a human still owns stays out
		// of service by its cordon, which is the real capacity guard.
		//
		// One incident halting does not mean the NODE is done: a bad node
		// commonly carries several, one per GPU or class. Ask before removing,
		// or a two-incident node would be un-marked and re-marked on every
		// resolution — visible to the scheduler as a node flickering in and out
		// of preference.
		if others, err := c.otherOpenIncidentsOnNode(ctx, inc); err != nil || others {
			if err != nil {
				c.log.Warn("cannot tell whether other incidents still hold this node; leaving its degraded taint to the janitor",
					"node", inc.Target.Node, "incident", inc.ID, "err", err)
			}
			return
		}
		if err := tainter.RemoveDegradedTaint(ctx, inc.Target.Node); err != nil {
			c.log.Warn("removing the degraded taint failed; the janitor will retry",
				"node", inc.Target.Node, "incident", inc.ID, "err", err)
		}
	case from == types.StateOpen:
		// The first non-terminal state the walk routes a fresh incident into —
		// OBSERVING for observe-first classes, EVALUATING for the rest. Marking
		// here rather than at open keeps it off incidents that are quarantined
		// as flapping on their very first pass.
		if err := tainter.ApplyDegradedTaint(ctx, inc.Target.Node, degradedTaintValue(inc), policy.Effect); err != nil {
			c.log.Warn("marking a node degraded failed; the janitor will retry",
				"node", inc.Target.Node, "incident", inc.ID, "err", err)
			return
		}
		c.log.Info("marked a node degraded so the scheduler stops sending it new work",
			"node", inc.Target.Node, "incident", inc.ID, "effect", policy.Effect)
	}
}

// reconcileDegradedTaints converges the cluster's marks onto the truth in the
// store, in both directions, on every janitor pass.
//
// This is the janitor that makes the feature safe to ship. The transition hook
// above can miss in three ordinary ways — a controller that died between the
// mark and the incident closing, an apiserver blip during either write, and an
// incident that was already open when the feature was switched on — and every
// one of them leaves a node marked with nothing left running to unmark it.
// Reading the marks back from the cluster rather than remembering them is what
// makes recovery independent of this process's lifetime, exactly as the cordon
// janitor does.
func (c *Controller) reconcileDegradedTaints(ctx context.Context) {
	tainter := c.nodeTainter()
	if tainter == nil {
		return
	}
	marked, err := tainter.DegradedTaintedNodes(ctx)
	if err != nil {
		c.log.Warn("listing degraded-tainted nodes failed, will retry", "err", err)
		return
	}
	policy := c.degradedTaintPolicy(ctx)
	// Disabled is not "do nothing": a switch turned back off (or an
	// installation downgraded to a configuration without the key) must take its
	// marks with it, or the fleet stays biased against those nodes forever with
	// no configuration left that explains why. This is the only work the
	// janitor does while the feature is off, and on the overwhelmingly common
	// never-enabled install the list is empty and it does nothing at all.
	if !policy.Enabled {
		for _, node := range marked {
			c.removeDegradedTaint(ctx, tainter, node, "the degraded-node taint is not enabled")
		}
		return
	}
	open, err := c.nodesUnderOpenIncidents(ctx)
	if err != nil {
		// A store outage is not evidence that an incident closed. Removing a
		// mark on that basis would put a failing node back in the scheduler's
		// rotation precisely when the control plane cannot see what is wrong
		// with it.
		c.log.Warn("reading open incidents failed, leaving degraded taints as they are", "err", err)
		return
	}
	isMarked := make(map[string]bool, len(marked))
	for _, node := range marked {
		isMarked[node] = true
		if _, stillOpen := open[node]; !stillOpen {
			c.removeDegradedTaint(ctx, tainter, node, "no incident on it is open any more")
		}
	}
	for node, inc := range open {
		if isMarked[node] {
			continue
		}
		if err := tainter.ApplyDegradedTaint(ctx, node, degradedTaintValue(inc), policy.Effect); err != nil {
			c.log.Warn("marking a node degraded failed, will retry", "node", node, "incident", inc.ID, "err", err)
			continue
		}
		c.log.Info("marked a node degraded from the janitor",
			"node", node, "incident", inc.ID, "effect", policy.Effect)
	}
}

// otherOpenIncidentsOnNode reports whether any incident OTHER than this one is
// still non-halted on the same node.
func (c *Controller) otherOpenIncidentsOnNode(ctx context.Context, inc *types.Incident) (bool, error) {
	incidents, err := c.store.ListIncidents(ctx, store.IncidentFilter{
		Node: inc.Target.Node,
		States: []types.IncidentState{
			types.StateOpen, types.StateObserving, types.StateEvaluating,
			types.StateAwaitingApproval, types.StateExecuting, types.StateVerifying,
		},
	})
	if err != nil {
		return false, err
	}
	for _, other := range incidents {
		if other.ID != inc.ID {
			return true, nil
		}
	}
	return false, nil
}

// nodesUnderOpenIncidents maps each node with a non-halted incident to one of
// those incidents (which one is immaterial: the value only names the mark).
func (c *Controller) nodesUnderOpenIncidents(ctx context.Context) (map[string]*types.Incident, error) {
	incidents, err := c.store.ListIncidents(ctx, store.IncidentFilter{
		States: []types.IncidentState{
			types.StateOpen, types.StateObserving, types.StateEvaluating,
			types.StateAwaitingApproval, types.StateExecuting, types.StateVerifying,
		},
	})
	if err != nil {
		return nil, err
	}
	out := make(map[string]*types.Incident, len(incidents))
	for _, inc := range incidents {
		if inc.Target.Node == "" {
			continue
		}
		if _, seen := out[inc.Target.Node]; !seen {
			out[inc.Target.Node] = inc
		}
	}
	return out, nil
}

func (c *Controller) removeDegradedTaint(ctx context.Context, tainter platform.NodeTainter, node, why string) {
	if err := tainter.RemoveDegradedTaint(ctx, node); err != nil {
		c.log.Warn("removing a degraded taint failed, will retry", "node", node, "err", err)
		return
	}
	c.log.Info("removed a degraded taint", "node", node, "reason", why)
}
