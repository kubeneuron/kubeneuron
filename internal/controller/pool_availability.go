// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"cmp"
	"context"
	"errors"
	"github.com/kubeneuron/kubeneuron/internal/platform"
	"github.com/kubeneuron/kubeneuron/pkg/cloud"
	"slices"
	"time"

	"github.com/kubeneuron/kubeneuron/internal/playbook"
	"github.com/kubeneuron/kubeneuron/internal/recoverymode"

	"github.com/kubeneuron/kubeneuron/pkg/types"
)

// recordPools uses the same complete Kubernetes node list as inventory sync.
// Reading the operator endpoint does not trigger another Kubernetes list.
func (s *inventorySync) recordPools(nodes []types.Node, engine *playbook.Engine, now time.Time, check func(types.Node) error) {
	type poolKey struct{ cloud, name string }
	s.mu.Lock()
	previous := map[poolKey]types.PoolAvailability{}
	for _, row := range s.pools {
		previous[poolKey{row.Cloud, row.Pool}] = row
	}
	s.mu.Unlock()
	representatives := map[poolKey]types.Node{}
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
				if counts[key] == 0 {
					representatives[key] = n
				}
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
		row := types.PoolAvailability{Pool: key.name, Cloud: key.cloud, AcceleratorNodes: count, Status: "unknown", ReplacementPolicy: cmp.Or(orders[key], "AddFirst")}
		switch {
		case row.ReplacementPolicy == "RemoveFirst":
			row.Status = ""
		case previous[key].CheckedAt != nil && now.Sub(*previous[key].CheckedAt) < 10*time.Minute:
			cached := previous[key]
			cached.ReplacementPolicy = row.ReplacementPolicy
			cached.AcceleratorNodes = count
			row = cached
		case row.Cloud == "EKS":
			// The release disables EKS add-first regardless of pool configuration.
			row.CheckedAt = &now
			row.Status = "unavailable"
			row.ReasonCode = "eks-add-first-disabled"
			row.Reason = "add-first on EKS is not enabled in this release"
		case check != nil:
			row.CheckedAt = &now
			err := check(representatives[key])
			var refusal *cloud.PoolRefusal
			switch {
			case err == nil:
				row.Status = "available"
				row.CapabilityEnabled = true
			case errors.As(err, &refusal):
				row.CapabilityEnabled = true
				row.Status, row.ReasonCode, row.Reason = "unavailable", refusal.Code, refusal.Reason
				row.Action = "use an eligible pool, or set RemoveFirst in the remediation policy that applies to these nodes"
			default:
				row.ReasonCode, row.Reason = "read-failed", "could not read pool configuration; check provider access and permissions"
			}
		}
		rows = append(rows, row)
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

func (c *Controller) poolChecker(ctx context.Context) func(types.Node) error {
	reader, ok := c.platform.(interface {
		platform.CloudNodeObserver
		AddFirstAvailable(context.Context, cloud.NodeIdentity) error
	})
	if !ok {
		return nil
	}
	return func(n types.Node) error {
		ref, err := reader.CloudNode(ctx, n.Name)
		if err != nil {
			return err
		}
		if n.UID == "" || ref.Identity.NodeUID != n.UID {
			return errors.New("pool node identity changed")
		}
		return reader.AddFirstAvailable(ctx, ref.Identity)
	}
}
