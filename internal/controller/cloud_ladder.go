// SPDX-License-Identifier: Apache-2.0

package controller

import "context"

// Include escalation targets so the first cordon/drain cannot run before the
// cloud recovery path's eligibility is known.
func (c *Controller) cloudRecoveryLadder(ctx context.Context, name string) bool {
	path := c.cloudRecoveryPath(ctx, name)
	return path.recycle || path.replace
}
