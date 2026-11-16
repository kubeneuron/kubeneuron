// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"

	"github.com/kubeneuron/kubeneuron/internal/playbook"
	"github.com/kubeneuron/kubeneuron/pkg/types"
)

// authorizeAndStartStep is the decision boundary shared by evaluation and
// approval resumption. Authorization never bypasses startStep's live safety
// checks. Policy authorization can extend this boundary without changing the
// recovery ladder or treating a parked state as proof of consent.
func (c *Controller) authorizeAndStartStep(ctx context.Context, inc *types.Incident, step *playbook.Step) error {
	if inc.State == types.StateAwaitingApproval {
		return c.resumeAwaitingApproval(ctx, inc)
	}
	if step.NeedsApproval() {
		return c.parkForApproval(ctx, inc, step, "step requires human approval")
	}
	return c.startStep(ctx, inc, step, "system")
}
