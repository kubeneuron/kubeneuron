package controller

import (
	"context"
	"fmt"
	"time"

	"github.com/kubeneuron/kubeneuron/internal/store"
	"github.com/kubeneuron/kubeneuron/pkg/types"

	"github.com/kubeneuron/kubeneuron/internal/platform"
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
	// The PLATFORM first, deliberately, and the store only as a fallback.
	//
	// This is the blast-radius decision — the one lookup in the system where a
	// stale answer is a safety failure rather than a display glitch. Reading
	// the store's cached copy first got both directions wrong. A cached label
	// that still matches lets a destructive step run on a node the operator
	// has just taken OUT of the declared radius, which is the direction that
	// costs somebody a machine. A cached label that has not caught up yet
	// refuses a node the operator just brought IN — and a resolved non-match
	// quarantines rather than retrying, so that incident never recovers on its
	// own either.
	//
	// The cost is one lookup per destructive step, and on Kubernetes it is not
	// even an apiserver call: ListNodes is served from the watch-maintained
	// informer cache, so this is both fresher and cheaper than it looks.
	// An UNFILTERED read of the exact node, when the platform can do one. This
	// is the only source that answers the question actually being asked: what
	// labels does this machine carry right now. ListNodes below is the GPU
	// inventory, and a node that has dropped out of it — a device plugin
	// restarting, a driver reloading — would otherwise make confinement
	// permanently unresolvable and hold every destructive step forever, in
	// exactly the conditions under which remediation is wanted.
	if labeler, ok := c.platform.(platform.NodeLabeler); ok && c.platform != nil {
		labels, found, err := labeler.NodeLabels(ctx, node)
		switch {
		case err == nil && found:
			return labels, nil
		case err == nil && !found:
			// A resolved absence: the machine is gone. That is an answer, and
			// it cannot match a non-empty selector.
			return nil, fmt.Errorf("node %s does not exist", node)
		}
		// A platform error falls through to the sources below rather than
		// becoming an unresolvable answer on a blip.
	}
	if c.platform != nil {
		nodes, err := c.platform.ListNodes(ctx)
		if err == nil {
			for _, n := range nodes {
				if n.Name != node {
					continue
				}
				if n.Labels == nil {
					return map[string]string{}, nil
				}
				return n.Labels, nil
			}
			// The platform answered and this node is not in it. Fall through
			// to the store rather than concluding anything: a GPU-less node,
			// or one the platform filters out of its GPU inventory, still has
			// labels worth checking before a destructive step.
		}
		// A platform error falls through too — a blip must not become an
		// unresolvable answer while the store still holds a usable one.
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
