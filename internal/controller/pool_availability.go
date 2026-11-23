// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/kubeneuron/kubeneuron/internal/config"
	"github.com/kubeneuron/kubeneuron/internal/platform"
	"github.com/kubeneuron/kubeneuron/internal/playbook"
	"github.com/kubeneuron/kubeneuron/internal/recoverymode"
	"github.com/kubeneuron/kubeneuron/pkg/cloud"
	"github.com/kubeneuron/kubeneuron/pkg/types"
)

// recordPools uses the same complete Kubernetes node list as inventory sync.
// Reading the operator endpoint does not trigger another Kubernetes list.
func (s *inventorySync) recordPools(nodes []types.Node, reports []*types.AgentAcceleratorReport, rc *RuntimeConfig, now time.Time, check func(n types.Node, permissionChecked bool, zone recoverymode.SuccessorZone, zones *[]string) error, replacement func(types.Node) (cloud.RecoveryEligibility, *cloud.RecoveryHold, error)) {
	engine := rc.Engine
	type poolKey struct{ cloud, name string }
	s.mu.Lock()
	previous := map[poolKey]types.PoolAvailability{}
	for _, row := range s.pools {
		previous[poolKey{row.Cloud, row.Pool}] = row
	}
	s.mu.Unlock()
	representatives := map[poolKey]types.Node{}

	qualified := map[poolKey]*poolQualification{}
	known := map[string]bool{}
	for _, profile := range rc.AcceleratorProfiles {
		known[profile.Name] = true
	}
	counts := map[poolKey]int{}
	orders := map[poolKey]string{}
	sameZoneRequired := map[poolKey]bool{}
	type selection struct {
		order    string
		sameZone bool
	}
	vendorOrders := map[types.AcceleratorVendor]selection{}
	type nodeKey struct{ name, uid string }
	vendors := map[nodeKey][]types.AcceleratorVendor{}
	for _, report := range reports {
		if report != nil && report.NodeUID != "" && report.Vendor.Valid() {
			key := nodeKey{report.Node, report.NodeUID}
			vendors[key] = append(vendors[key], report.Vendor)
		}
	}
	generic, genericSameZone := poolVendorOrder(engine, "")

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
				if qualified[key] == nil {
					qualified[key] = &poolQualification{warnings: map[string]int{}}
				}
				applicableVendors := slices.Clone(reported)
				for _, device := range n.Devices {
					if device.Resource == "nvidia.com/gpu" && !slices.Contains(applicableVendors, types.AcceleratorVendorNVIDIA) {
						applicableVendors = append(applicableVendors, types.AcceleratorVendorNVIDIA)
					}
				}
				if len(applicableVendors) == 0 {
					applicableVendors = []types.AcceleratorVendor{""}
				}
				orders[key] = mergePoolOrder(orders[key], generic)
				sameZoneRequired[key] = sameZoneRequired[key] || genericSameZone
				before := qualified[key].assessments
				for _, vendor := range applicableVendors {
					qualified[key].assess(rc, known, n.Labels, vendor)
					order, found := vendorOrders[vendor]
					if !found {
						order.order, order.sameZone = poolVendorOrder(engine, vendor)
						vendorOrders[vendor] = order
					}
					orders[key] = mergePoolOrder(orders[key], order.order)
					sameZoneRequired[key] = sameZoneRequired[key] || order.sameZone
				}
				if qualified[key].assessments > before {
					qualified[key].nodes++
				}
				break
			}
		}
	}
	rows := make([]types.PoolAvailability, 0, len(counts))
	checksLeft := 4
	for key, count := range counts {
		row := types.PoolAvailability{Pool: key.name, Cloud: key.cloud, AcceleratorNodes: count, Status: "unknown", ReplacementPolicy: cmp.Or(orders[key], "none")}
		zone := recoverymode.SameZone
		if !sameZoneRequired[key] && (row.ReplacementPolicy == "AddFirst" || row.ReplacementPolicy == "mixed") {
			zone = recoverymode.AnyPoolZone
		}
		row.SuccessorZone = string(zone)
		removeFirst := row.ReplacementPolicy == "RemoveFirst"
		if removeFirst {
			row.Status = ""
		}
		switch {
		case row.ReplacementPolicy == "none":
			row.Status, row.ReasonCode, row.Reason = "not applicable", "no-replacement-policy", "no remediation policy replaces these nodes"
		case previous[key].CheckedAt != nil && previous[key].ReplacementPolicy == row.ReplacementPolicy && previous[key].SuccessorZone == row.SuccessorZone &&
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
			err := check(representatives[key], permissionChecked, zone, &row.Zones)
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
		// Every node counted under key initializes qualified[key] in the same pass.
		q := qualified[key]
		slices.Sort(q.missing)
		row.MissingQualifiedProfiles = q.missing
		row.AutomaticQualificationReasons = nil
		for reason, count := range q.warnings {
			if count < q.assessments || q.nodes < row.AcceleratorNodes {
				reason = "for some of this pool's nodes: " + strings.Replace(reason, "this pool's nodes", "the node", 1)
			}
			row.AutomaticQualificationReasons = append(row.AutomaticQualificationReasons, reason)
		}
		slices.Sort(row.AutomaticQualificationReasons)
		rows = append(rows, row)
	}
	slices.SortFunc(rows, func(a, b types.PoolAvailability) int {
		return cmp.Or(cmp.Compare(a.Cloud, b.Cloud), cmp.Compare(a.Pool, b.Pool))
	})
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pools = rows
}

type poolQualification struct {
	missing     []string
	warnings    map[string]int
	assessments int
	nodes       int
}

func (q *poolQualification) assess(rc *RuntimeConfig, known map[string]bool, labels map[string]string, vendor types.AcceleratorVendor) {
	if rc.Engine == nil {
		return
	}
	for _, class := range rc.Engine.PolicyClasses() {
		policy, ok := rc.Engine.PolicyFor(class, vendor)
		if !ok || policy.Mode != recoverymode.Automatic || policy.Automatic == nil || !policy.Automatic.SelectsNode(labels) || !cloudRecoveryPath(rc.Engine, policy.Playbook).replace {
			continue
		}
		q.assessments++
		var absent []string
		for _, name := range policy.Automatic.QualifiedProfiles {
			if !known[name] {
				absent = append(absent, name)
				if !slices.Contains(q.missing, name) {
					q.missing = append(q.missing, name)
				}
			}
		}
		if reason := poolQualificationReason(rc, policy.Automatic, labels, vendor, absent); reason != "" {
			q.warnings[reason]++
		}
	}
}

func poolQualificationReason(rc *RuntimeConfig, auto *recoverymode.Settings, labels map[string]string, vendor types.AcceleratorVendor, missing []string) string {
	if len(auto.QualifiedProfiles) == 0 {
		return "the policy lists no profile in spec.automatic.qualifiedProfiles"
	}
	match, err := qualificationProfile(rc.AcceleratorProfiles, labels, vendor)
	if err == nil && auto.Qualifies(match.Name) {
		return ""
	}
	if len(missing) == len(auto.QualifiedProfiles) {
		slices.Sort(missing)
		return types.MissingProfileMessage(missing)
	}
	var overlap *config.OverlappingProfiles
	switch {
	case errors.Is(err, config.ErrNoAcceleratorRuntimeProfile):
		return "no AcceleratorRuntimeProfile selects this pool's nodes"
	case errors.As(err, &overlap):
		return fmt.Sprintf("more than one AcceleratorRuntimeProfile selects this pool's nodes (%s and %s)", overlap.First, overlap.Second)
	case err != nil:
		return "correct the invalid AcceleratorRuntimeProfile: " + err.Error()
	default:
		return "node profile " + match.Name + " is not listed in spec.automatic.qualifiedProfiles"
	}
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
	pools := append([]types.PoolAvailability{}, c.inventory.pools...)
	c.inventory.mu.Unlock()
	held := c.heldResizes()
	for i := range pools {
		pools[i].ReplacementHeld = held[pools[i].Pool]
	}
	return types.PoolAvailabilityView{Pools: pools}
}

func mergePoolOrder(current, next string) string {
	if current != "" && next != "" && current != next {
		return "mixed"
	}
	return cmp.Or(current, next)
}

func poolVendorOrder(engine *playbook.Engine, vendor types.AcceleratorVendor) (string, bool) {
	var sameZone bool
	var order string
	if engine != nil {
		for _, class := range engine.PolicyClasses() {
			policy, ok := engine.PolicyFor(class, vendor)
			if ok && cloudRecoveryPath(engine, policy.Playbook).replace {
				next := "AddFirst"
				if policy.ReplacementStrategy.Effective() == recoverymode.RemoveFirst {
					next = "RemoveFirst"
				} else if policy.SuccessorZone != recoverymode.AnyPoolZone {
					sameZone = true
				}
				order = mergePoolOrder(order, next)
			}
		}
	}
	return order, sameZone
}

func (c *Controller) poolChecker(ctx context.Context) func(types.Node, bool, recoverymode.SuccessorZone, *[]string) error {
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
	return func(n types.Node, permissionChecked bool, zone recoverymode.SuccessorZone, zones *[]string) error {
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
			if aware, ok := c.platform.(interface {
				AssessAddFirstPoolInZone(context.Context, cloud.NodeIdentity, recoverymode.SuccessorZone, *[]string) error
			}); ok {
				return aware.AssessAddFirstPoolInZone(ctx, ref.Identity, zone, zones)
			}
			if assessment, ok := c.platform.(interface {
				AssessAddFirstPool(context.Context, cloud.NodeIdentity) error
			}); ok {
				return assessment.AssessAddFirstPool(ctx, ref.Identity)
			}
		}
		if aware, ok := reader.(interface {
			AddFirstAvailableInZone(context.Context, cloud.NodeIdentity, recoverymode.SuccessorZone, *[]string) error
		}); ok {
			return aware.AddFirstAvailableInZone(ctx, ref.Identity, zone, zones)
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
