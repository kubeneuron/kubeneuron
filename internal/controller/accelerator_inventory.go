// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"encoding/json"
	"errors"
	"sync"

	"github.com/kubeneuron/kubeneuron/internal/platform"
	"github.com/kubeneuron/kubeneuron/internal/store"
	"github.com/kubeneuron/kubeneuron/pkg/types"
)

// inventorySync remembers what was last written for each node read from
// Kubernetes, so an unchanged node is not written on every pass.
type inventorySync struct {
	mu      sync.Mutex
	written map[string]string
}

// syncAcceleratorInventory records the nodes that advertise accelerator
// devices but have no registered agent, from their Kubernetes Node objects.
// A node whose agent registered is left as it is: the agent is the source of
// truth for it. Such a node has no agent heartbeat to go stale and no agent to
// run a step, so it raises no agent-down and an agent step on it fails before
// anything is queued. A failed read changes nothing.
func (c *Controller) syncAcceleratorInventory(ctx context.Context) {
	inv, ok := c.platform.(platform.AcceleratorInventory)
	if !ok {
		return
	}
	nodes, err := inv.AcceleratorNodes(ctx)
	if err != nil {
		if c.logOnce.first("inventory-read/" + err.Error()) {
			c.log.Warn("could not read the accelerator nodes", "err", err)
		}
		return
	}
	seen := map[string]string{}
	for i := range nodes {
		n := &nodes[i]
		digest, _ := json.Marshal(struct {
			UID     string
			Devices []types.DeviceCount
		}{n.UID, n.Devices})
		if c.inventory.unchanged(n.Name, string(digest)) {
			seen[n.Name] = string(digest)
			continue
		}
		existing, err := c.store.GetNode(ctx, n.Name)
		switch {
		case errors.Is(err, store.ErrNotFound):
		case err != nil:
			if c.logOnce.first("inventory-node/" + n.Name + "/" + err.Error()) {
				c.log.Warn("could not read a node's inventory", "node", n.Name, "err", err)
			}
			continue
		case !existing.AgentLastSeen.IsZero():
			// The agent's node; nothing to record, and nothing to re-read
			// while the Node object stays the same.
			seen[n.Name] = string(digest)
			continue
		}
		if err := c.store.UpsertKubernetesInventory(ctx, n); err != nil {
			if c.logOnce.first("inventory-write/" + n.Name + "/" + err.Error()) {
				c.log.Warn("could not record a node read from Kubernetes", "node", n.Name, "err", err)
			}
			continue
		}
		seen[n.Name] = string(digest)
	}
	c.inventory.keep(seen)
}

func (s *inventorySync) unchanged(node, digest string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.written[node] == digest
}

// keep replaces the remembered nodes with the ones read now.
func (s *inventorySync) keep(written map[string]string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.written = written
}
