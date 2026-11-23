// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/kubeneuron/kubeneuron/internal/cloud/removefirst"
	"github.com/kubeneuron/kubeneuron/internal/platform"
	"github.com/kubeneuron/kubeneuron/internal/recoverymode"
	"github.com/kubeneuron/kubeneuron/pkg/cloud"
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
	guard, ok := c.platform.(platform.GuardedNodeRecycler)
	if !ok {
		return nil, fmt.Errorf("cloud recovery unavailable: platform has no guarded mutation contract")
	}
	if inc.CloudIdentity == nil {
		return nil, fmt.Errorf("cloud recovery unavailable: %s", c.missingCloudIdentityReason(inc.ID))
	}
	pin := *inc.CloudIdentity
	if err := pin.Validate(); err != nil {
		return nil, fmt.Errorf("cloud recovery unavailable: %w", err)
	}
	if pin.Node != inc.Target.Node {
		return nil, fmt.Errorf("cloud recovery unavailable: incident target differs from pinned node")
	}
	node := inc.Target.Node
	if replace {
		if err := c.recordReplacedNodeDevices(ctx, inc, pin); err != nil {
			return nil, fmt.Errorf("platform.replace_node: recording the node's devices: %w", err)
		}
	}
	if replace && (successorReady(inc) || c.runtimeConfig(ctx).Engine != nil &&
		c.incidentPolicy(ctx, inc).ReplacementStrategy.Effective() == recoverymode.AddFirst) {
		return c.replaceWithSuccessor(ctx, inc, pin)
	}
	if replace {
		if err := c.recordReplacementPool(ctx, inc, pin); err != nil {
			return nil, fmt.Errorf("platform.replace_node: recording the pool: %w", err)
		}
		if rec := inc.RemoveFirst; rec != nil && rec.Version != 0 && (rec.Open() || rec.Operation == successorOperation(inc)) {
			if rec.Version == 1 && rec.Outcome == types.RemoveFirstDeleteConfirmed {
				return okResult(inc, "confirmed removal of the original machine"), nil
			}
			return nil, fmt.Errorf("platform.replace_node: check the recorded outcome of remove-first operation %s before another delete", rec.Operation)
		}
		if planned, ok := c.platform.(platform.PlannedRemoveFirstRecycler); ok {
			if res, handled, err := c.replaceRemoveFirst(ctx, inc, planned, pin); handled {
				return res, err
			}
		}
		var named string
		var err error
		if target, ok := guard.(interface {
			SupportsDeleteIntent() bool
			ReplacePinnedNodeWithIntent(context.Context, cloud.NodeIdentity, func(string, string, string) error) (string, error)
		}); ok && target.SupportsDeleteIntent() {
			named, err = target.ReplacePinnedNodeWithIntent(ctx, pin, func(pool, machine, successor string) error {
				if pool == "" {
					rows, err := c.store.AuditTrail(ctx, inc.ID)
					if err != nil {
						return err
					}
					for i := len(rows) - 1; i >= 0; i-- {
						if rows[i].Action == replacementPoolAction && rows[i].Params["node_uid"] == pin.NodeUID {
							pool = rows[i].Params["pool"]
							break
						}
					}
				}
				return c.claimDeleteIntent(ctx, inc, pin, pool, machine, successor)
			})
			if errors.Is(err, removefirst.ErrRefused) && inc.RemoveFirst.Open() {
				err = errors.Join(err, c.settleRefusedDelete(ctx, inc))
			} else if err == nil && inc.RemoveFirst.Open() {
				err = c.endRemoveFirst(ctx, inc, types.RemoveFirstDeleteConfirmed, "")
			}
		} else if namer, ok := guard.(platform.NamedNodeReplacer); ok {
			named, err = namer.ReplacePinnedNodeNamed(ctx, pin)
		} else {
			err = guard.ReplacePinnedNode(ctx, pin)
		}
		if err != nil {
			if errors.Is(err, cloud.ErrReplacementCapacity) {
				return nil, fmt.Errorf("platform.replace_node: replacement capacity unavailable: %w", err)
			}
			return nil, fmt.Errorf("platform.replace_node: %w", err)
		}
		if named != "" {
			// Not written, the replacement is still found by its pool.
			_ = c.recordReplacementInstance(ctx, inc, pin, named)
		}
		// Termination is not proof of replacement or accelerator recovery.
		return okResult(inc, "terminated the instance behind "+node+"; the autoscaler will replace it"), nil
	}

	verified, ok := c.platform.(platform.VerifiedNodeRecycler)
	if !ok {
		return nil, fmt.Errorf("cloud recovery unavailable: platform has no verified recycle result contract")
	}
	if _, hasDeadline := ctx.Deadline(); !hasDeadline {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, defaultRecycleWaitTimeout)
		defer cancel()
	}
	result, err := verified.RecyclePinnedNodeVerified(ctx, pin)
	if err != nil {
		return nil, fmt.Errorf("platform.recycle_node: %w", err)
	}
	if err := c.repinAfterRecycle(ctx, inc, result); err != nil {
		return nil, fmt.Errorf("persisting verified recycle identity: %w", err)
	}
	return okResult(inc, "recycled the pinned instance behind "+node+"; new Ready boot verified; accelerator recovery remains unverified"), nil

}

// replaceWithSuccessor removes the pinned node once its accepted successor
// has taken over, and closes the successor operation.
func (c *Controller) replaceWithSuccessor(ctx context.Context, inc *types.Incident, pin cloud.NodeIdentity) (*types.ActionResult, error) {
	succ, ok := c.platform.(platform.SuccessorNodeRecycler)
	if !ok || !successorReady(inc) {
		return nil, fmt.Errorf("platform.replace_node: no prepared replacement is ready for %s, and the policy does not name remove-first", pin.Node)
	}
	rec := inc.Successor
	if err := succ.ReplacePinnedNodeWithSuccessor(ctx, pin, rec.Successor); err != nil {
		if errors.Is(err, cloud.ErrNeedsOperator) {
			// The old instance may be gone: the successor stays, and nothing
			// more is changed automatically.
			if serr := c.endSuccessor(ctx, inc, types.SuccessorHandedOver, err.Error()); serr != nil {
				return nil, fmt.Errorf("platform.replace_node: %w; recording the hand-over failed: %v", err, serr)
			}
		}
		return nil, fmt.Errorf("platform.replace_node: %w", err)
	}
	if err := c.endSuccessor(ctx, inc, types.SuccessorReplaced, ""); err != nil {
		return nil, fmt.Errorf("recording the replacement of %s: %w", pin.Node, err)
	}
	return okResult(inc, fmt.Sprintf("removed the instance behind %s; successor %s took over (operation %s)", pin.Node, rec.Node, rec.Operation)), nil
}

// defaultRecycleWaitTimeout bounds verified recycle when the step has no timeout.
var defaultRecycleWaitTimeout = 20 * time.Minute
