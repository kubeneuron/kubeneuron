// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/kubeneuron/kubeneuron/internal/store"
	"github.com/kubeneuron/kubeneuron/pkg/types"

	"github.com/kubeneuron/kubeneuron/internal/platform"
)

// This file is node resolution: how the controller answers "what do I know
// about this node right now" — labels (for confinement), inventory identity
// (for evidence gates), pause state, and maintenance windows. Kubernetes label
// reads use the live platform.

// activeMaintenanceWindow reports whether any currently active window
// covers the node, returning the window name. When a selector window's node
// labels cannot be resolved, the check fails toward holding automation —
// during declared maintenance, not acting is the safe direction.
func (c *Controller) activeMaintenanceWindow(ctx context.Context, node string) (string, bool) {
	windows := c.runtimeConfig(ctx).Windows
	if len(windows) == 0 {
		return "", false
	}
	now := time.Now()
	var labels map[string]string
	labelsResolved := false
	for _, w := range windows {
		if !w.ActiveAt(now) {
			continue
		}
		if len(w.MatchLabels) == 0 {
			return w.Name, true
		}
		if !labelsResolved {
			labels = c.nodeLabels(ctx, node)
			labelsResolved = true
		}
		if labels == nil {
			c.log.Warn("cannot resolve node labels; holding automation during selector window",
				"node", node, "window", w.Name)
			return w.Name, true
		}
		if w.MatchesLabels(labels) {
			return w.Name, true
		}
	}
	return "", false
}

// autonomyMaintenanceWindowActive answers the narrower question required by
// GPUAutonomyPlan: whether one of the plan's declared window references is
// active and covers the target node. A missing selector inventory answer is an
// error so the autonomy adapter fails closed rather than treating it as an
// open window.
func (c *Controller) autonomyMaintenanceWindowActive(ctx context.Context, node string, references []string) (bool, error) {
	if len(references) == 0 {
		return false, nil
	}
	wanted := make(map[string]struct{}, len(references))
	for _, reference := range references {
		if reference = strings.TrimSpace(reference); reference != "" {
			wanted[reference] = struct{}{}
		}
	}
	if len(wanted) == 0 {
		return false, fmt.Errorf("autonomy maintenance window references are empty")
	}
	now := time.Now()
	var labels map[string]string
	labelsResolved := false
	for _, window := range c.runtimeConfig(ctx).Windows {
		if _, selected := wanted[window.Name]; !selected || !window.ActiveAt(now) {
			continue
		}
		if len(window.MatchLabels) == 0 {
			return true, nil
		}
		if !labelsResolved {
			labels = c.nodeLabels(ctx, node)
			labelsResolved = true
		}
		if labels == nil {
			return false, fmt.Errorf("autonomy maintenance window %q cannot resolve labels for node %q", window.Name, node)
		}
		if window.MatchesLabels(labels) {
			return true, nil
		}
	}
	return false, nil
}

// nodeLabels returns nil when the current labels cannot be resolved.
func (c *Controller) nodeLabels(ctx context.Context, node string) map[string]string {
	labels, _ := c.nodeLabelsForConfinement(ctx, node)
	return labels
}

// Resolve an exact node first: the platform's accelerator list can omit a
// live node while its device plugin is restarting. A read error must hold
// the decision. Only platforms without an exact lookup use the
// inventory fallback.
func (c *Controller) nodeLabelsForConfinement(ctx context.Context, node string) (map[string]string, error) {
	if labeler, ok := c.platform.(platform.NodeLabeler); ok {
		labels, found, err := labeler.NodeLabels(ctx, node)
		if err != nil {
			return nil, err
		}
		if !found {
			return nil, fmt.Errorf("node %s does not exist", node)
		}
		if labels == nil {
			labels = map[string]string{}
		}
		return labels, nil
	}
	if c.platform != nil {
		nodes, err := c.platform.ListNodes(ctx)
		if err != nil {
			return nil, err
		}
		for _, n := range nodes {
			if n.Name == node {
				if n.Labels == nil {
					return map[string]string{}, nil
				}
				return n.Labels, nil
			}
		}
	}
	if n, err := c.store.GetNode(ctx, node); err == nil && len(n.Labels) > 0 {
		return n.Labels, nil
	}
	if c.platform == nil {
		return nil, fmt.Errorf("node %s labels are unavailable (no platform configured and not labeled in inventory)", node)
	}
	return nil, fmt.Errorf("node %s is not present in the current inventory", node)
}

// acceleratorNode resolves the current inventory object used for a
// profile-gated accelerator action. A live platform result is authoritative
// when available: falling back to SQLite after a Kubernetes list failure
// would allow stale node-name reuse during an outage. Store fallback exists
// only for non-platform controller tests and integrations that explicitly
// persist an immutable identity.
func (c *Controller) acceleratorNode(ctx context.Context, node string) (*types.Node, bool) {
	if c.platform != nil {
		nodes, err := c.platform.ListNodes(ctx)
		if err != nil {
			return nil, false
		}
		for i := range nodes {
			if nodes[i].Name == node {
				return &nodes[i], true
			}
		}
		return nil, false
	}
	n, err := c.store.GetNode(ctx, node)
	if err != nil {
		return nil, false
	}
	return n, true
}

func (c *Controller) nodePaused(ctx context.Context, name string) (bool, error) {
	n, err := c.store.GetNode(ctx, name)
	if err != nil {
		return false, err
	}
	return n.Paused, nil
}

// pauseHolds reports whether the incident's node must not advance: it is
// paused, or its pause state cannot be read. The second is deliberate — a
// store error is not evidence that the operator's pause was lifted, so the
// walk holds and retries on the next tick (fail closed). A node that is not
// in the inventory at all has no pause to honor.
func (c *Controller) pauseHolds(ctx context.Context, inc *types.Incident) bool {
	paused, err := c.nodePaused(ctx, inc.Target.Node)
	if errors.Is(err, store.ErrNotFound) {
		return false
	}
	if err != nil {
		c.log.Warn("cannot read the node's pause state; holding the incident",
			"incident", inc.ID, "node", inc.Target.Node, "err", err)
		return true
	}
	return paused
}

// nodeFor loads inventory for actuation, falling back to a name-only node
// (Kubernetes actuation needs nothing more).
func (c *Controller) nodeFor(ctx context.Context, name string) (types.Node, error) {
	n, err := c.store.GetNode(ctx, name)
	if err == store.ErrNotFound {
		return types.Node{Name: name}, nil
	}
	if err != nil {
		return types.Node{}, err
	}
	return *n, nil
}
