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
func (s *inventorySync) recordPools(nodes []types.Node, reports []*types.AgentAcceleratorReport, engine *playbook.Engine, now time.Time, check func(n types.Node, permissionChecked bool) error, replacement func(types.Node) (cloud.RecoveryEligibility, *cloud.RecoveryHold, error)) {
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
		removeFirst := row.ReplacementPolicy == "RemoveFirst"
		if removeFirst {
			row.Status = ""
		}
		switch {
		case row.ReplacementPolicy == "none":
			row.Status, row.ReasonCode, row.Reason = "not applicable", "no-replacement-policy", "no remediation policy replaces these nodes"
		case previous[key].CheckedAt != nil && previous[key].ReplacementPolicy == row.ReplacementPolicy &&
			(now.Sub(*previous[key].CheckedAt) < 10*time.Minute || (checksLeft == 0 && check != nil)):
			cached := previous[key]
			cached.AcceleratorNodes = count
			row = cached
		case check == nil:
			row.ReplacementStatus, row.ReplacementReasonCode, row.ReplacementReason = "unknown", "no-cloud-provider", "no cloud provider configured"
			if !removeFirst {
				row.ReasonCode, row.Reason = "no-cloud-provider", "no cloud provider configured"
			}
		case checksLeft > 0:
			checksLeft--
			row.CheckedAt = &now
			// One check covers the pool in both orders: first what holds any
			// replacement, as an incident's own eligibility check would, then
			// what add-first needs on top.
			permissionChecked := false
			if replacement != nil {
				v, hold, err := replacement(representatives[key])
				replacementVerdict(&row, v, hold, err)
				// The eligibility check already asked the cloud for the
				// replace permissions; add-first does not ask again.
				permissionChecked = err == nil && v.ReplaceAllowed
			}
			if removeFirst {
				break
			}
			if row.ReplacementStatus == "unavailable" {
				// What holds every replacement holds add-first too; the
				// table shows it once, in REPLACEMENT.
				row.Status, row.ReasonCode = "unavailable", row.ReplacementReasonCode
				row.Reason, row.Action = row.ReplacementReason, row.ReplacementAction
				break
			}
			err := check(representatives[key], permissionChecked)
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

// replacementVerdict records whether an incident on the pool could replace
// its node at all, from the same eligibility an incident checks.
// hold, when the cloud's own repair is on, is that policy's hold.
func replacementVerdict(row *types.PoolAvailability, v cloud.RecoveryEligibility, hold *cloud.RecoveryHold, err error) {
	if err != nil || v.Mode != cloud.RepairOn {
		hold = nil
	}
	switch {
	case err == nil && cloudRecoveryAllowed(v, "platform.replace_node"):
		row.ReplacementStatus = "available"
	case hold != nil || errors.As(err, &hold):
		row.ReplacementStatus, row.ReplacementReasonCode = "unavailable", hold.Code
		row.ReplacementReason, row.ReplacementAction = hold.Reason, hold.Action
		if hold.PoolReason != "" {
			row.ReplacementReason, row.ReplacementAction = hold.PoolReason, hold.PoolAction
		}
		if hold.Code == cloud.HoldReadFailed || hold.Code == cloud.HoldUnconfirmed || hold.Transient() {
			row.ReplacementStatus = "unknown"
		}
	case err != nil:
		row.ReplacementStatus, row.ReplacementReasonCode = "unknown", "read-failed"
		row.ReplacementReason = "could not read pool configuration; check provider access and permissions"
	case v.Mode == cloud.RepairOn:
		row.ReplacementStatus, row.ReplacementReasonCode, row.ReplacementReason = "unavailable", cloud.HoldNativeRepair, v.Reason
	default:
		// Of the moment (a cloud repair in progress, a NotReady node), or a
		// provider that does not replace: an incident would wait, not fail.
		row.ReplacementStatus, row.ReplacementReasonCode, row.ReplacementReason = "unknown", "not-eligible-now", v.Reason
	}
}

// replacementChecker asks the eligibility an incident asks before it replaces
// a node, for a pool's representative node; nil without a cloud provider.
func (c *Controller) replacementChecker(ctx context.Context) func(types.Node) (cloud.RecoveryEligibility, *cloud.RecoveryHold, error) {
	if p, ok := c.platform.(interface{ CloudRecyclingConfigured() bool }); ok && !p.CloudRecyclingConfigured() {
		return nil
	}
	reader, ok := c.platform.(interface {
		platform.CloudNodeObserver
		PoolRecoveryEligibility(context.Context, cloud.NodeIdentity) (cloud.RecoveryEligibility, *cloud.RecoveryHold, error)
	})
	if !ok {
		return nil
	}
	return func(n types.Node) (cloud.RecoveryEligibility, *cloud.RecoveryHold, error) {
		ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		ref, err := reader.CloudNode(ctx, n.Name)
		if err != nil {
			return cloud.RecoveryEligibility{}, nil, err
		}
		if n.UID == "" || ref.Identity.NodeUID != n.UID {
			return cloud.RecoveryEligibility{}, nil, errors.New("pool node identity changed")
		}
		return reader.PoolRecoveryEligibility(ctx, ref.Identity)
	}
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

func (c *Controller) poolChecker(ctx context.Context) func(types.Node, bool) error {
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
	return func(n types.Node, permissionChecked bool) error {
		ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		ref, err := reader.CloudNode(ctx, n.Name)
		if err != nil {
			return err
		}
		if n.UID == "" || ref.Identity.NodeUID != n.UID {
			return errors.New("pool node identity changed")
		}
		// "available" means the incident path would not hold on permissions
		// either: GKE checks them with Resource Manager, which can be off.
		if permission, ok := c.platform.(interface {
			ReplacePermission(context.Context, cloud.NodeIdentity) error
		}); ok && !permissionChecked {
			if err := permission.ReplacePermission(ctx, ref.Identity); err != nil {
				return err
			}
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
