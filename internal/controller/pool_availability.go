// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"cmp"
	"slices"

	"github.com/kubeneuron/kubeneuron/internal/playbook"
	"github.com/kubeneuron/kubeneuron/internal/recoverymode"

	"github.com/kubeneuron/kubeneuron/pkg/types"
)

// recordPools uses the same complete Kubernetes node list as inventory sync.
// Reading the operator endpoint does not trigger another Kubernetes list.
func (s *inventorySync) recordPools(nodes []types.Node, engine *playbook.Engine) {
	type poolKey struct{ cloud, name string }
	counts := map[poolKey]int{}
	orders := map[poolKey]string{}
	vendorOrders := map[types.AcceleratorVendor]string{}
	vendorFor := map[string]types.AcceleratorVendor{"nvidia.com/gpu": "nvidia", "amd.com/gpu": "amd", "google.com/tpu": "google"}

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
				key := poolKey{provider.cloud, name}
				counts[key]++
				for _, device := range n.Devices {
					vendor := vendorFor[device.Resource]
					order, found := vendorOrders[vendor]
					if !found {
						order = poolVendorOrder(engine, vendor)
						vendorOrders[vendor] = order
					}
					orders[key] = mergePoolOrder(orders[key], order)
				}
				break
			}
		}
	}
	rows := make([]types.PoolAvailability, 0, len(counts))
	for key, count := range counts {
		rows = append(rows, types.PoolAvailability{Pool: key.name, Cloud: key.cloud, AcceleratorNodes: count, Status: "unknown", ReplacementPolicy: cmp.Or(orders[key], "AddFirst")})
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

func mergePoolOrder(current, next string) string {
	if current != "" && next != "" && current != next {
		return "mixed"
	}
	return cmp.Or(current, next)
}

func poolVendorOrder(engine *playbook.Engine, vendor types.AcceleratorVendor) string {
	var order string
	if engine != nil {
		for _, class := range engine.PolicyClasses() {
			policy, ok := engine.PolicyFor(class, vendor)
			if ok && cloudRecoveryPath(engine, policy.Playbook).replace {
				next := "AddFirst"
				if policy.ReplacementStrategy.Effective() == recoverymode.RemoveFirst {
					next = "RemoveFirst"
				}
				order = mergePoolOrder(order, next)
			}
		}
	}
	return order
}
