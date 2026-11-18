// SPDX-License-Identifier: Apache-2.0

package kubernetes

import (
	"context"
	"fmt"

	"github.com/kubeneuron/kubeneuron/internal/platform"
	"github.com/kubeneuron/kubeneuron/pkg/cloud"
)

var _ platform.PlannedRemoveFirstRecycler = (*Platform)(nil)

func (p *Platform) plannedRemoveFirst() (cloud.PlannedRemoveFirstProvider, error) {
	provider, ok := p.recycler.(cloud.PlannedRemoveFirstProvider)
	if !ok {
		return nil, fmt.Errorf("%w: the cloud provider replaces by removing only", cloud.ErrRemoveFirstUnplanned)
	}
	return provider, nil
}

// replaceEligible is nil while the pinned node may be replaced.
func (p *Platform) replaceEligible(ctx context.Context, pin cloud.NodeIdentity) error {
	v, err := p.RecoveryEligibility(ctx, pin)
	if err != nil {
		return err
	}
	if !v.ReplaceAllowed {
		return fmt.Errorf("cloud replacement unavailable: %s", v.Reason)
	}
	return nil
}

func (p *Platform) PlanRemoveFirst(ctx context.Context, pin cloud.NodeIdentity, operation string) (cloud.RemoveFirstPlan, error) {
	provider, err := p.plannedRemoveFirst()
	if err != nil {
		return cloud.RemoveFirstPlan{}, err
	}
	if err := pin.Validate(); err != nil {
		return cloud.RemoveFirstPlan{}, err
	}
	if err := p.replaceEligible(ctx, pin); err != nil {
		return cloud.RemoveFirstPlan{}, err
	}
	return provider.PlanRemoveFirst(ctx, pin.InstanceID, operation)
}

func (p *Platform) ContinueRemoveFirst(ctx context.Context, pin cloud.NodeIdentity, plan cloud.RemoveFirstPlan) (cloud.RemoveFirstPlan, error) {
	provider, err := p.plannedRemoveFirst()
	if err != nil {
		return plan, err
	}
	if plan.Instance != pin.InstanceID {
		return plan, fmt.Errorf("remove-first operation %s replaces %s, not the pinned %s", plan.Operation, plan.Instance, pin.InstanceID)
	}
	if plan.Phase == cloud.RemoveFirstPlanned {
		// The step to deleting commits to the delete: the node must still
		// be the pinned one and eligible. Later steps act on a node that may
		// be gone.
		if err := p.replaceEligible(ctx, pin); err != nil {
			return plan, err
		}
	}
	return provider.ContinueRemoveFirst(ctx, plan)
}
