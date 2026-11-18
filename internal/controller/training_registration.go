// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"time"

	"github.com/kubeneuron/kubeneuron/internal/inventory"
	"github.com/kubeneuron/kubeneuron/internal/platform"
)

// TrainingNodeMonitored binds the sender's node to a recent agent registration
// in this controller's store. Discovery alone cannot establish that binding.
func (c *Controller) TrainingNodeMonitored(ctx context.Context, name string) bool {
	identity, ok := c.platform.(platform.NodeIdentity)
	if !ok || name == "" {
		return false
	}
	registered, err := c.store.GetNode(ctx, name)
	if err != nil || registered == nil || registered.UID == "" || registered.AgentLastSeen.IsZero() {
		return false
	}
	age := time.Since(registered.AgentLastSeen)
	if age < 0 || age > 3*inventory.Interval {
		return false
	}
	uid, exists, err := identity.NodeUID(ctx, name)
	return err == nil && exists && uid == registered.UID
}
