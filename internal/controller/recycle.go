package controller

import (
	"context"
	"fmt"
	"time"

	"github.com/kubeneuron/kubeneuron/internal/platform"
	"github.com/kubeneuron/kubeneuron/pkg/types"
)

// recycleNodeStep stops and starts (recycle) or terminates (replace) the cloud
// instance behind the incident's node.
//
// This is the cloud-native stand-in for a GPU reset. On a virtualized instance
// a hardware reset is impossible — the hypervisor withholds the PCI reset — so
// the remediation reinitializes the instance instead: stop/start re-establishes
// the GPU passthrough on the same node, and terminate hands the node to the
// autoscaler. Both restart the whole VM, so the compiler already forces
// approval on the playbook step.
//
// It fails closed when the platform cannot recycle: a platform with no cloud
// provider, or one that is not a cloud platform at all, must not silently
// succeed on a node whose GPU was never actually cleared.
func (c *Controller) recycleNodeStep(ctx context.Context, inc *types.Incident, replace bool) (*types.ActionResult, error) {
	recycler, ok := c.platform.(platform.NodeRecycler)
	if !ok || !recycler.CloudRecyclingConfigured() {
		verb := "recycle"
		if replace {
			verb = "replace"
		}
		return nil, fmt.Errorf("platform.%s_node: no cloud provider is configured for %q; a virtualized GPU node needs one to stand in for a hardware reset",
			verb, c.platform.Name())
	}
	node := inc.Target.Node
	if replace {
		if err := recycler.ReplaceNode(ctx, node); err != nil {
			return nil, fmt.Errorf("platform.replace_node: %w", err)
		}
		// The node is on its way out. The vanished-node reconciler closes the
		// incident as replaced once the Node object disappears.
		return okResult(inc, "terminated the instance behind "+node+"; the autoscaler will replace it"), nil
	}
	if err := recycler.RecycleNode(ctx, node); err != nil {
		return nil, fmt.Errorf("platform.recycle_node: %w", err)
	}
	// The instance is powered on, but the node has not rejoined yet: the OS,
	// kubelet and agent take minutes more. Wait for the node to be Ready before
	// reporting success, or the next step (verify) fires on a stale heartbeat.
	// Measured live: without this, verify failed the moment recycle returned.
	//
	// Bound the wait. A recycle_node step with no timeout hands waitNodeReady an
	// unbounded context, so a node that never rejoins (e.g. a managed node group
	// that replaced it instead) would hold the incident in EXECUTING until the
	// controller restarts. When the step carries no deadline, apply a default so
	// the wait fails the step instead of spinning forever; the failure escalates
	// (to ReplaceNode, per the message below).
	if _, hasDeadline := ctx.Deadline(); !hasDeadline {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, defaultRecycleWaitTimeout)
		defer cancel()
	}
	if err := c.waitNodeReady(ctx, node); err != nil {
		return nil, fmt.Errorf("platform.recycle_node: instance restarted but %w", err)
	}
	return okResult(inc, "recycled the instance behind "+node+" (stop/start); the GPU passthrough was reinitialized and the node rejoined"), nil
}

// waitNodeReady polls until the node's Ready condition is True or the context
// deadline (the step timeout) passes. On a self-managed node a recycle keeps
// the node, so it comes back; on a managed node group it does not — the ASG
// replaces it — and this times out, which is the honest signal that RecycleNode
// was the wrong action there and ReplaceNode should have been used.
func (c *Controller) waitNodeReady(ctx context.Context, node string) error {
	checker, ok := c.platform.(platform.NodeRecycler)
	if !ok {
		return nil
	}
	for {
		ready, err := checker.NodeReady(ctx, node)
		if err == nil && ready {
			return nil
		}
		timer := time.NewTimer(recycleReadyPollInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return fmt.Errorf("node %s did not become Ready in time (on a managed node group use ReplaceNode, not RecycleNode): %w", node, ctx.Err())
		case <-timer.C:
		}
	}
}

// recycleReadyPollInterval paces the wait for a recycled node to rejoin.
// A var (not const) only so tests can shorten it.
var recycleReadyPollInterval = 10 * time.Second

// defaultRecycleWaitTimeout bounds waitNodeReady when the recycle_node step
// declares no timeout of its own, so the incident can never hold EXECUTING
// forever on a node that never comes back. Generous enough for an OS+kubelet
// reboot to complete, short enough that a node that will never rejoin fails the
// step and escalates within a bounded time. A var (not const) only so tests can
// shorten it.
var defaultRecycleWaitTimeout = 20 * time.Minute
