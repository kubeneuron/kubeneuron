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

// inventorySync remembers the Node objects last recorded, so an unchanged
// node is not written on every pass.
type inventorySync struct {
	mu      sync.Mutex
	written map[string]string
}

// syncAcceleratorInventory records the nodes that advertise accelerator
// devices but have no registered agent, from their Kubernetes Node objects,
// and removes the rows of such nodes once their Node object is gone. A node
// whose agent registered is left as it is: the agent is the source of truth
// for it. A node without an agent has no agent heartbeat to go stale and no
// agent to run a step, so it raises no agent-down and an agent step on it
// fails before anything is queued. A failed read changes nothing.
func (c *Controller) syncAcceleratorInventory(ctx context.Context) {
	inv, ok := c.platform.(platform.AcceleratorInventory)
	if !ok {
		return
	}
	nodes, err := inv.NodeInventory(ctx)
	if err != nil {
		if c.logOnce.first("inventory-read/" + err.Error()) {
			c.log.Warn("could not read the node inventory", "err", err)
		}
		return
	}
	present := map[string]bool{}
	seen := map[string]string{}
	for i := range nodes {
		n := &nodes[i]
		present[n.Name] = true
		digest := inventoryDigest(n)
		if c.inventory.unchanged(n.Name, digest) {
			seen[n.Name] = digest
			continue
		}
		existing, err := c.store.GetNode(ctx, n.Name)
		switch {
		case errors.Is(err, store.ErrNotFound):
			existing = nil
		case err != nil:
			if c.logOnce.first("inventory-node/" + n.Name + "/" + err.Error()) {
				c.log.Warn("could not read a node's inventory", "node", n.Name, "err", err)
			}
			continue
		case !existing.AgentLastSeen.IsZero():
			// The agent's node; nothing to record, and nothing to re-read
			// while the Node object stays the same.
			seen[n.Name] = digest
			continue
		}
		record := *n
		if existing != nil && existing.InventorySource == types.InventoryKubernetes {
			record.Devices = keepLostDevices(n.Devices, existing.Devices)
		}
		if len(record.Devices) == 0 {
			// Not an accelerator node.
			seen[n.Name] = digest
			continue
		}
		if err := c.store.UpsertKubernetesInventory(ctx, &record); err != nil {
			if c.logOnce.first("inventory-write/" + n.Name + "/" + err.Error()) {
				c.log.Warn("could not record a node read from Kubernetes", "node", n.Name, "err", err)
			}
			continue
		}
		seen[n.Name] = digest
	}
	c.inventory.keep(seen)
	c.removeVanishedInventory(ctx, present)
}

// keepLostDevices is the node's devices now, plus every device resource it
// advertised before and no longer does, kept at its last known capacity with
// nothing allocatable. A device plugin that stops takes its resource off the
// Node; the node still has the devices, and pods cannot use them.
func keepLostDevices(now, before []types.DeviceCount) []types.DeviceCount {
	out := append([]types.DeviceCount(nil), now...)
	for _, b := range before {
		found := false
		for _, d := range now {
			if d.Resource == b.Resource {
				found = true
				break
			}
		}
		if !found {
			b.Allocatable = 0
			out = append(out, b)
		}
	}
	return out
}

// removeVanishedInventory deletes the rows read from Kubernetes whose Node
// object was not in the read: the node is gone, and its row must not stay in
// a pool or a report. Rows an agent registered are not touched.
func (c *Controller) removeVanishedInventory(ctx context.Context, present map[string]bool) {
	rows, err := c.store.ListNodes(ctx)
	if err != nil {
		if c.logOnce.first("inventory-list/" + err.Error()) {
			c.log.Warn("could not list the node inventory", "err", err)
		}
		return
	}
	for _, row := range rows {
		if row.InventorySource != types.InventoryKubernetes || !row.AgentLastSeen.IsZero() || present[row.Name] {
			continue
		}
		if err := c.store.DeleteKubernetesInventory(ctx, row.Name); err != nil {
			if c.logOnce.first("inventory-delete/" + row.Name + "/" + err.Error()) {
				c.log.Warn("could not remove a vanished node's inventory", "node", row.Name, "err", err)
			}
		}
	}
}

func inventoryDigest(n *types.Node) string {
	data, _ := json.Marshal(struct {
		UID     string
		Labels  map[string]string
		Devices []types.DeviceCount
	}{n.UID, n.Labels, n.Devices})
	return string(data)
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
