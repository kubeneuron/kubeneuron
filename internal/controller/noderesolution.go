package controller

import (
	"context"
	"fmt"
	"time"

	"github.com/kubeneuron/kubeneuron/internal/store"
	"github.com/kubeneuron/kubeneuron/pkg/types"
)

// This file is node resolution: how the controller answers "what do I know
// about this node right now" — labels (for confinement), inventory identity
// (for evidence gates), pause state, and maintenance windows. Reads prefer
// the durable store and fall back to the platform, whose kubernetes
// implementation serves from an informer-backed cache.

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

// nodeLabels resolves node labels from inventory, falling back to the
// platform; nil means the labels could not be determined.
func (c *Controller) nodeLabels(ctx context.Context, node string) map[string]string {
	if n, err := c.store.GetNode(ctx, node); err == nil && len(n.Labels) > 0 {
		return n.Labels
	}
	if c.platform == nil {
		return nil
	}
	nodes, err := c.platform.ListNodes(ctx)
	if err != nil {
		return nil
	}
	for _, n := range nodes {
		if n.Name == node {
			if n.Labels == nil {
				return map[string]string{}
			}
			return n.Labels
		}
	}
	return nil
}

// nodeLabelsForConfinement resolves node labels for the destructive-scope
// check, distinguishing a resolved answer from an unresolvable one. Unlike
// nodeLabels (which collapses every failure to nil), it returns an error when
// the labels cannot be determined right now — a transient store/platform
// failure, or a node momentarily absent from inventory — so the caller can hold
// and retry instead of quarantining on a passing apiserver blip. A node found
// with no labels is a resolved answer (empty map, no error): it simply cannot
// match a non-empty selector.
func (c *Controller) nodeLabelsForConfinement(ctx context.Context, node string) (map[string]string, error) {
	if n, err := c.store.GetNode(ctx, node); err == nil && len(n.Labels) > 0 {
		return n.Labels, nil
	}
	if c.platform == nil {
		return nil, fmt.Errorf("node %s labels are unavailable (no platform configured and not labeled in inventory)", node)
	}
	nodes, err := c.platform.ListNodes(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing nodes: %w", err)
	}
	for _, n := range nodes {
		if n.Name == node {
			if n.Labels == nil {
				return map[string]string{}, nil
			}
			return n.Labels, nil
		}
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
