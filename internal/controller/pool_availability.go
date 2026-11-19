// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"cmp"
	"slices"

	"github.com/kubeneuron/kubeneuron/pkg/types"
)

// recordPools uses the same complete Kubernetes node list as inventory sync.
// Reading the operator endpoint does not trigger another Kubernetes list.
func (s *inventorySync) recordPools(nodes []types.Node) {
	type poolKey struct{ cloud, name string }
	counts := map[poolKey]int{}
	for _, n := range nodes {
		if len(n.Devices) == 0 && len(n.GPUs) == 0 {
			continue
		}
		for _, provider := range []struct{ cloud, label string }{
			{"AKS", "kubernetes.azure.com/agentpool"},
			{"EKS", "eks.amazonaws.com/nodegroup"},
			{"GKE", "cloud.google.com/gke-nodepool"},
		} {
			if name := n.Labels[provider.label]; name != "" {
				counts[poolKey{provider.cloud, name}]++
				break
			}
		}
	}
	rows := make([]types.PoolAvailability, 0, len(counts))
	for key, count := range counts {
		rows = append(rows, types.PoolAvailability{Pool: key.name, Cloud: key.cloud, AcceleratorNodes: count, Status: "unknown"})
	}
	slices.SortFunc(rows, func(a, b types.PoolAvailability) int {
		return cmp.Or(cmp.Compare(a.Cloud, b.Cloud), cmp.Compare(a.Pool, b.Pool))
	})
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pools = rows
}

func (c *Controller) PoolAvailability() types.PoolAvailabilityView {
	c.inventory.mu.Lock()
	defer c.inventory.mu.Unlock()
	return types.PoolAvailabilityView{Pools: append([]types.PoolAvailability{}, c.inventory.pools...)}
}
