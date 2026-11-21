// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"cmp"
	"context"
	"errors"
	"slices"
	"time"

	"github.com/kubeneuron/kubeneuron/internal/platform"
	"github.com/kubeneuron/kubeneuron/internal/playbook"
	"github.com/kubeneuron/kubeneuron/internal/recoverymode"
	"github.com/kubeneuron/kubeneuron/pkg/cloud"
	"github.com/kubeneuron/kubeneuron/pkg/types"
)

// recordPools uses the same complete Kubernetes node list as inventory sync.
// Reading the operator endpoint does not trigger another Kubernetes list.
func (s *inventorySync) recordPools(nodes []types.Node, reports []*types.AgentAcceleratorReport, engine *playbook.Engine, now time.Time, check func(types.Node) error) {
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
	type nodeKey struct{ name, uid string }
	vendors := map[nodeKey][]types.AcceleratorVendor{}
	for _, report := range reports {
		if report != nil && report.NodeUID != "" && report.Vendor.Valid() {
			key := nodeKey{report.Node, report.NodeUID}
			vendors[key] = append(vendors[key], report.Vendor)
		}
	}
	generic := poolVendorOrder(engine, "")

	for _, n := range nodes {
		reported := vendors[nodeKey{n.Name, n.UID}]
		if len(n.Devices) == 0 && len(n.GPUs) == 0 && len(reported) == 0 {
			continue
		}
		for _, provider := range poolLabels {
			if name := n.Labels[provider.label]; name != "" {
				key := poolKey{provider.cloud, name}
				if counts[key] == 0 {
					representatives[key] = n
				}
				counts[key]++
				orders[key] = mergePoolOrder(orders[key], generic)
				for _, vendor := range reported {
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
	checksLeft := 4
	for key, count := range counts {
		row := types.PoolAvailability{Pool: key.name, Cloud: key.cloud, AcceleratorNodes: count, Status: "unknown", ReplacementPolicy: cmp.Or(orders[key], "none")}
		switch {
		case row.ReplacementPolicy == "none":
			row.Status, row.ReasonCode, row.Reason = "not applicable", "no-replacement-policy", "no remediation policy replaces these nodes"
		case row.ReplacementPolicy == "RemoveFirst":
			row.Status = ""
		case previous[key].CheckedAt != nil && (now.Sub(*previous[key].CheckedAt) < 10*time.Minute || (checksLeft == 0 && check != nil)):
			cached := previous[key]
			cached.ReplacementPolicy = row.ReplacementPolicy
			cached.AcceleratorNodes = count
			row = cached
		case check == nil:
			row.ReasonCode, row.Reason = "no-cloud-provider", "no cloud provider configured"
		case checksLeft > 0:
			checksLeft--
			row.CheckedAt = &now
			err := check(representatives[key])
			enabled := true
			if row.Cloud == "EKS" {
				caps, _ := cloud.DeclaredCapabilities("aws")
				enabled = caps.AddFirst
			}
			row.CapabilityEnabled = enabled
			var refusal *cloud.PoolRefusal
			var hold *cloud.RecoveryHold
			switch {
			case err == nil && !enabled:
				row.Status, row.ReasonCode, row.Reason = "unavailable", "eks-add-first-disabled", "add-first on EKS is not enabled in this release"
			case err == nil:
				row.Status = "available"
			case errors.As(err, &hold):
				row.Status, row.ReasonCode, row.Reason, row.Action = "unavailable", hold.Code, hold.Reason, hold.Action
				if hold.PoolReason != "" {
					row.Reason, row.Action = hold.PoolReason, hold.PoolAction
				}
				if hold.Code == cloud.HoldReadFailed || hold.Code == cloud.HoldUnconfirmed {
					row.Status = "unknown"
				}
			case errors.As(err, &refusal):
				row.Status, row.ReasonCode, row.Reason = "unavailable", refusal.Code, refusal.Reason
				row.Action = poolRefusalAction(refusal)
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
	if p, ok := c.platform.(interface{ CloudRecyclingConfigured() bool }); ok && !p.CloudRecyclingConfigured() {
		return nil
	}
	reader, ok := c.platform.(interface {
		platform.CloudNodeObserver
		AddFirstAvailable(context.Context, cloud.NodeIdentity) error
	})
	if !ok {
		return nil
	}
	return func(n types.Node) error {
		ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		ref, err := reader.CloudNode(ctx, n.Name)
		if err != nil {
			return err
		}
		if n.UID == "" || ref.Identity.NodeUID != n.UID {
			return errors.New("pool node identity changed")
		}
		if n.Labels["eks.amazonaws.com/nodegroup"] != "" {
			if assessment, ok := c.platform.(interface {
				AssessAddFirstPool(context.Context, cloud.NodeIdentity) error
			}); ok {
				return assessment.AssessAddFirstPool(ctx, ref.Identity)
			}
		}
		return reader.AddFirstAvailable(ctx, ref.Identity)
	}
}

func poolRefusalAction(refusal *cloud.PoolRefusal) string {
	if refusal.Action != "" {
		return refusal.Action
	}
	const alternative = ", or set RemoveFirst in the remediation policy that applies to these nodes"
	switch refusal.Code {
	case cloud.PoolAutopilot:
		return "use a GKE Standard node pool for KubeNeuron-managed replacement"
	case cloud.PoolAutoscaled:
		return "use a fixed-size pool" + alternative
	case cloud.PoolMultiZone:
		return "use a single-zone node pool" + alternative
	case cloud.PoolInstanceMissing:
		return "verify that the node belongs to the configured AKS agent pool"
	default:
		return "check the pool configuration"
	}
}
