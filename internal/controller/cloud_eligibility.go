// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"fmt"

	"github.com/kubeneuron/kubeneuron/internal/platform"
	"github.com/kubeneuron/kubeneuron/pkg/cloud"
	"github.com/kubeneuron/kubeneuron/pkg/types"
)

// cloudRecoveryEligibility is the shared pre-authorization query. A negative
// verdict is execution unavailability, not a request for human authorization.
// It does not change incident state or dispatch a side effect. Callers retain
// the reason for status, and must not turn an error into a positive verdict.
// GuardedNodeRecycler repeats these checks at the cloud mutation boundary.
func (c *Controller) cloudRecoveryEligibility(ctx context.Context, inc *types.Incident) (cloud.RecoveryEligibility, error) {
	v := cloud.RecoveryEligibility{Mode: cloud.RepairUnknown}
	if inc.CloudIdentity == nil {
		v.Reason = "incident has no pinned cloud identity; recovery unavailable"
		return v, nil
	}
	v.Identity = *inc.CloudIdentity
	if inc.Target.Node != v.Identity.Node {
		v.Reason = "incident target does not match pinned cloud identity"
		return v, fmt.Errorf("%s", v.Reason)
	}
	guard, ok := c.platform.(platform.GuardedNodeRecycler)
	if !ok {
		v.Reason = "platform has no guarded cloud recovery contract"
		return v, nil
	}
	return guard.RecoveryEligibility(ctx, v.Identity)
}
