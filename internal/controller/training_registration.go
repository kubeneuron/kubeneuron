// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"time"

	"github.com/kubeneuron/kubeneuron/pkg/types"

	"github.com/kubeneuron/kubeneuron/internal/inventory"
	"github.com/kubeneuron/kubeneuron/internal/platform"
)

// TrainingNodeMonitored binds the sender's node to the live UID of its inventory row.
// Agent-owned rows also require a recent heartbeat.
func (c *Controller) TrainingNodeMonitored(ctx context.Context, name string) bool {
	identity, ok := c.platform.(platform.NodeIdentity)
	if !ok || name == "" {
		return false
	}
	registered, err := c.store.GetNode(ctx, name)
	if err != nil || registered == nil || registered.UID == "" {
		return false
	}
	if registered.InventorySource != types.InventoryKubernetes {
		age := time.Since(registered.AgentLastSeen)
		if age < 0 || age > 3*inventory.Interval {
			return false
		}
	}
	uid, exists, err := identity.NodeUID(ctx, name)
	return err == nil && exists && uid == registered.UID
}
