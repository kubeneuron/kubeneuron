// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/kubeneuron/kubeneuron/internal/platform"
	"github.com/kubeneuron/kubeneuron/internal/store"
	"github.com/kubeneuron/kubeneuron/pkg/cloud"
	"github.com/kubeneuron/kubeneuron/pkg/types"
)

// Infrastructure evidence recorded on the resolve transition of a cloud
// recovery: the target's own new boot, or the node that took its place.
const (
	recycleInfrastructureEvidence     = "verified_recycle_inventory"
	replacementInfrastructureEvidence = "verified_replacement_node"
)

// How the node that took the target's place was tied to the replacement:
// the provider named it (add-first), or it is the one node of the target's
// pool created after the removal (remove-first).
const (
	replacementBySuccessor = "successor"
	replacementByPool      = "pool"
)

// replacementPoolAction is the audit row that records, before a remove-first
// delete, the pool the target's place is looked for in afterwards.
const replacementPoolAction = "replacement-pool"

// poolLabels are the node labels that name a managed node pool, per cloud.
var poolLabels = []struct{ cloud, label string }{
	{"AKS", "kubernetes.azure.com/agentpool"},
	{"EKS", "eks.amazonaws.com/nodegroup"},
	{"GKE", "cloud.google.com/gke-nodepool"},
}

// reportingVendors are the vendors an agent of this build sends an
// accelerator report for. A replacement of another vendor's node is verified
// on the node and its agent alone, and the resolution says so.
var reportingVendors = map[types.AcceleratorVendor]bool{
	types.AcceleratorVendorNVIDIA: true,
	types.AcceleratorVendorAWS:    true,
}

// cloudReplacement is a replacement of the incident's target that the cloud
// carried out: the old node is gone, and the node that took its place is
// what verification reads.
type cloudReplacement struct {
	// successor is the add-first operation that replaced the target; nil for
	// a remove-first replacement.
	successor *types.SuccessorRecord
	// label and pool name the target's pool, recorded before a remove-first
	// delete; empty when it could not be read.
	label, pool string
	// since is when the new node's evidence may start: the successor's
	// acceptance, or the moment before the remove-first delete.
	since time.Time
}

// recordReplacementPool writes, before a remove-first delete, which pool the
// target belongs to: once it is deleted, its labels are gone with it. A pool
// already recorded for this replacement step is not written again.
func (c *Controller) recordReplacementPool(ctx context.Context, inc *types.Incident, pin cloud.NodeIdentity) error {
	observer, ok := c.platform.(platform.CloudNodeObserver)
	if !ok {
		return nil
	}
	ref, err := observer.CloudNode(ctx, pin.Node)
	if err != nil {
		return nil
	}
	var label, pool string
	for _, l := range poolLabels {
		if v := ref.Labels[l.label]; v != "" {
			label, pool = l.label, v
			break
		}
	}
	if pool == "" {
		return nil
	}
	trail, err := c.store.AuditTrail(ctx, inc.ID)
	if err != nil {
		return err
	}
	for i := len(trail) - 1; i >= 0; i-- {
		e := trail[i]
		if e.Action == replacementPoolAction && e.Params["label"] == label && e.Params["pool"] == pool {
			return nil
		}
		if e.Result == auditStepStartResult("platform.replace_node") {
			break
		}
	}
	return c.store.AppendAudit(ctx, &types.AuditEntry{
		IncidentID: inc.ID, Time: time.Now(), FromState: inc.State, ToState: inc.State, Actor: "system",
		Action: replacementPoolAction, DryRun: inc.DryRun,
		Result: fmt.Sprintf("removing %s from pool %s; the node that takes its place is looked for there", pin.Node, pool),
		Params: map[string]string{"label": label, "pool": pool, "node": pin.Node, "node_uid": pin.NodeUID},
	})
}

// findCloudReplacement reads whether the incident's last cloud step replaced
// the target and succeeded. A replacement whose result is not known yet is
// not one: it is left to the recycle check, which holds it.
func (c *Controller) findCloudReplacement(ctx context.Context, inc *types.Incident) (*cloudReplacement, error) {
	trail, err := c.store.AuditTrail(ctx, inc.ID)
	if err != nil {
		return nil, err
	}
	const replace = "platform.replace_node"
	var found *cloudReplacement
	var pool *types.AuditEntry
	for _, e := range trail {
		if e.DryRun {
			continue
		}
		switch {
		case e.Result == auditStepStartResult(replace) || e.Result == auditExecutingResult(replace) ||
			e.Result == auditStepStartResult("platform.recycle_node"):
			found = nil
			if e.Result == auditStepStartResult("platform.recycle_node") {
				pool = nil
			}
		case e.Result == auditStepOutcomeResult(replace, false, true):
			found = &cloudReplacement{since: e.Time}
		case e.Action == replacementPoolAction:
			pool = e
		}
	}
	if found == nil {
		return nil, nil
	}
	if s := inc.Successor; s != nil && s.Outcome == types.SuccessorReplaced && s.ReadyAt != nil {
		found.successor, found.since = s, *s.ReadyAt
		return found, nil
	}
	if pool != nil {
		found.label, found.pool, found.since = pool.Params["label"], pool.Params["pool"], pool.Time
	}
	return found, nil
}

// verifyReplacement verifies the node that took the target's place: it is
// there, Ready, the one the replacement is tied to, and its agent reports its
// accelerators since. It returns the depth of what was confirmed, or why it
// waits, and the evidence the resolution records.
func (c *Controller) verifyReplacement(ctx context.Context, inc *types.Incident, r *cloudReplacement) (bool, string, map[string]string) {
	node, how, reason := c.replacementNode(ctx, inc, r)
	if reason != "" {
		return false, reason, nil
	}
	ok, depth := c.verifyReplacementInventory(ctx, inc, node, r.since)
	if !ok {
		return false, depth, nil
	}
	return true, fmt.Sprintf("replaced by node %s (%s): %s", node.Name, how, depth), map[string]string{
		"infrastructure_evidence": replacementInfrastructureEvidence,
		"replacement_node":        node.Name,
		"replacement_node_uid":    node.UID,
		"replacement_correlation": map[bool]string{true: replacementBySuccessor, false: replacementByPool}[r.successor != nil],
	}
}

// replacementNode finds the node that took the target's place, and says how
// it is tied to the replacement.
func (c *Controller) replacementNode(ctx context.Context, inc *types.Incident, r *cloudReplacement) (platform.SuccessorNode, string, string) {
	readCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	if s := r.successor; s != nil {
		plat, ok := c.platform.(platform.SuccessorNodeRecycler)
		if !ok {
			return platform.SuccessorNode{}, "", "the successor node cannot be read on this platform"
		}
		node, err := plat.SuccessorNode(readCtx, s.ProviderID)
		switch {
		case err != nil:
			return node, "", "the successor node could not be read"
		case !node.Found:
			return node, "", fmt.Sprintf("successor node %s is not in the cluster", s.Node)
		case node.Name != s.Node || node.UID != s.NodeUID:
			return node, "", fmt.Sprintf("the node at the successor's instance is no longer %s as accepted", s.Node)
		}
		return node, "successor of operation " + s.Operation, ""
	}
	if r.pool == "" {
		return platform.SuccessorNode{}, "", "the replaced node's pool was not recorded, so no node can be tied to the replacement"
	}
	lister, ok := c.platform.(platform.PoolNodeLister)
	if !ok {
		return platform.SuccessorNode{}, "", "the pool's nodes cannot be read on this platform"
	}
	members, err := lister.PoolNodes(readCtx, r.label, r.pool)
	if err != nil {
		return platform.SuccessorNode{}, "", fmt.Sprintf("the nodes of pool %s could not be read", r.pool)
	}
	var created []platform.SuccessorNode
	for _, n := range members {
		if n.Name == inc.Target.Node || (inc.CloudIdentity != nil && n.UID == inc.CloudIdentity.NodeUID) || !n.CreatedAt.After(r.since) {
			continue
		}
		created = append(created, n)
	}
	switch len(created) {
	case 0:
		return platform.SuccessorNode{}, "", fmt.Sprintf("no node of pool %s was created after the removal", r.pool)
	case 1:
	default:
		names := make([]string, 0, len(created))
		for _, n := range created {
			names = append(names, n.Name)
		}
		return platform.SuccessorNode{}, "", fmt.Sprintf("ambiguous: %d nodes of pool %s were created after the removal (%s)", len(created), r.pool, strings.Join(names, ", "))
	}
	node := created[0]
	for _, a := range inc.Accelerators {
		if node.Allocatable[a.Resource] < a.Count {
			return node, "", fmt.Sprintf("node %s has %d allocatable %s, fewer than the %d the incident covered", node.Name, node.Allocatable[a.Resource], a.Resource, a.Count)
		}
	}
	return node, "the only node of pool " + r.pool + " created after the removal", ""
}

// verifyReplacementInventory reads the new node's accelerators from its
// agent's report, at the depth the incident's vendor allows. A vendor no
// agent of this build reports is verified on the node being Ready and its
// agent's heartbeat, and the depth says the devices were not checked.
func (c *Controller) verifyReplacementInventory(ctx context.Context, inc *types.Incident, node platform.SuccessorNode, since time.Time) (bool, string) {
	if !node.Ready {
		return false, fmt.Sprintf("replacement node %s is not Ready", node.Name)
	}
	if node.Unschedulable {
		return false, fmt.Sprintf("replacement node %s is not schedulable", node.Name)
	}
	vendor := inc.Vendor
	if !vendor.Valid() {
		vendor = types.AcceleratorVendorNVIDIA
	}
	now := time.Now()
	if !reportingVendors[vendor] {
		registered, err := c.store.GetNode(ctx, node.Name)
		if err != nil || registered == nil || registered.UID != node.UID ||
			!registered.AgentLastSeen.After(since) || now.Sub(registered.AgentLastSeen) > verifyEvidenceMaxAge {
			return false, fmt.Sprintf("no agent on replacement node %s has reported since the replacement", node.Name)
		}
		return true, fmt.Sprintf("the node is Ready and its agent reports; this build reads no %s accelerator report, so its devices were not checked", vendor)
	}
	reports, ok := c.store.(store.AcceleratorReportStore)
	if !ok {
		return false, "accelerator report store is unavailable"
	}
	report, err := reports.GetAcceleratorReport(ctx, node.Name, vendor)
	if err != nil || report == nil || report.NodeUID != node.UID || !report.ObservedAt.After(since) ||
		report.ObservedAt.After(now) || now.Sub(report.ObservedAt) > verifyEvidenceMaxAge {
		return false, fmt.Sprintf("no fresh %s accelerator report from replacement node %s since the replacement", vendor, node.Name)
	}
	if !inventoryMatches(report, "", since, now) {
		return false, fmt.Sprintf("the %s accelerator report of replacement node %s shows no ready devices (readiness %s)", vendor, node.Name, report.Readiness)
	}
	switch report.Readiness {
	case types.AcceleratorReadinessDegraded:
		return true, "accelerator inventory and driver probe observed; DCGM not present, device health not checked"
	case types.AcceleratorReadinessObserved:
		return true, "fresh accelerator inventory observed: device presence observed; runtime not attested"
	}
	return true, "fresh accelerator inventory and driver health observed"
}
