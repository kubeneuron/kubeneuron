// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/kubeneuron/kubeneuron/internal/cloud/resizediff"
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
// the provider named it (add-first); it is the one instance new in the
// target's pool between before the first change and after the resize
// (remove-first on a fixed-size pool the provider compares); or it is the
// one node of the target's pool created after the removal (remove-first).
const (
	replacementBySuccessor  = "successor"
	replacementByResizeDiff = "resize-diff"
	replacementByPool       = "pool"
)

// replacementPoolAction is the audit row that records, before a remove-first
// delete, the pool the target's place is looked for in afterwards.
const replacementPoolAction = "replacement-pool"

// replacementInstanceAction is the audit row that records, after a
// remove-first replacement the provider carried out in one call, the instance
// it found new in the pool.
const replacementInstanceAction = "replacement-instance"

// replacedDevicesAction is the audit row that records, before the
// replacement changes anything, whether the replaced node's agent ever
// reported accelerators of the incident's vendor: a new node is asked for no
// more evidence than the node it replaces gave.
const replacedDevicesAction = "replaced-node-devices"

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
	// members are the UIDs of the pool's nodes before the delete; nil when
	// they could not be read.
	members []string
	// instance is the providerID of the one instance the provider found new
	// in the pool after the resize; "" when it found none.
	instance string
	// agent is whether the replaced node ran an agent, so its replacement is
	// expected to.
	agent bool
	// capacity is the accelerator resources the replaced node advertised
	// before the delete: what the new node is compared against when the
	// incident recorded no accelerators of its own. Nil when not recorded.
	capacity []types.IncidentAccelerator
	// since is when the new node's evidence may start: the successor's
	// acceptance, or the moment before the remove-first delete.
	since time.Time
	// noDevices is whether the replaced node's agent never reported
	// accelerators of the incident's vendor before the replacement. False also when this
	// was not recorded, as for a replacement started by an older release.
	noDevices bool
}

// recordReplacedNodeDevices writes, once per node and before the replacement
// changes anything, whether the agent of the node being replaced ever
// reported accelerators of the incident's vendor. The node and its report may
// be gone by the time the new node is verified.
func (c *Controller) recordReplacedNodeDevices(ctx context.Context, inc *types.Incident, pin cloud.NodeIdentity) error {
	vendor, reported := replacementReportVendor(inc)
	reports, ok := c.store.(store.AcceleratorReportStore)
	if !reported || !ok {
		return nil
	}
	trail, err := c.store.AuditTrail(ctx, inc.ID)
	if err != nil {
		return err
	}
	// The first row for the node stands: a retried step or a restarted
	// controller does not ask again, after the fault had longer to hide the
	// devices.
	for _, e := range trail {
		if e.Action == replacedDevicesAction && e.Params["node_uid"] == pin.NodeUID {
			return nil
		}
	}
	// Any report from this node counts, even one without devices: a fault
	// that hides the devices leaves its agent still reporting. So does a
	// registration that lists accelerators.
	report, err := reports.GetAcceleratorReport(ctx, pin.Node, vendor)
	if errors.Is(err, store.ErrNotFound) {
		report, err = nil, nil
	}
	if err != nil {
		return err
	}
	devices := report != nil && report.NodeUID == pin.NodeUID
	if !devices {
		registered, err := c.store.GetNode(ctx, pin.Node)
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			return err
		}
		devices = registered != nil && registered.UID == pin.NodeUID && len(registered.GPUs) > 0
	}
	result := fmt.Sprintf("the agent on %s has reported %s accelerators; the new node's report is checked", pin.Node, vendor)
	if !devices {
		result = fmt.Sprintf("the agent on %s never reported %s accelerators; the new node is checked by its readiness and advertised accelerators unless it reports devices", pin.Node, vendor)
	}
	return c.store.AppendAudit(ctx, &types.AuditEntry{
		IncidentID: inc.ID, Time: time.Now(), FromState: inc.State, ToState: inc.State, Actor: "system",
		Action: replacedDevicesAction, DryRun: inc.DryRun, Result: result,
		Params: map[string]string{"node": pin.Node, "node_uid": pin.NodeUID, "vendor": string(vendor), "devices": strconv.FormatBool(devices)},
	})
}

// recordReplacementPool writes, before a remove-first delete, which pool the
// target belongs to and the UIDs of the pool's nodes: once it is deleted, its
// labels are gone with it, and a node whose UID is not in that set is one
// that joined after. It also records whether the target ran an agent, and
// the accelerator resources it advertised, for an incident that recorded none
// of its own (an agent that does not inventory its vendor's devices). A pool
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
	params := map[string]string{"label": label, "pool": pool, "node": pin.Node, "node_uid": pin.NodeUID}
	if lister, ok := c.platform.(platform.PoolNodeLister); ok {
		members, err := lister.PoolNodes(ctx, label, pool)
		if err != nil {
			return err
		}
		uids := make([]string, 0, len(members)+1)
		for _, n := range members {
			uids = append(uids, n.UID)
		}
		if !slices.Contains(uids, pin.NodeUID) {
			uids = append(uids, pin.NodeUID)
		}
		slices.Sort(uids)
		params["member_uids"] = strings.Join(uids, ",")
	}
	if registered, err := c.store.GetNode(ctx, pin.Node); err == nil && registered != nil &&
		registered.InventorySource != types.InventoryKubernetes && !registered.AgentLastSeen.IsZero() {
		params["agent"] = "true"
	}
	if expected, _, err := c.expectedAccelerators(ctx, inc); err == nil && len(expected) > 0 {
		counts := make([]string, 0, len(expected))
		for name, n := range expected {
			counts = append(counts, fmt.Sprintf("%s=%d", name, n))
		}
		slices.Sort(counts)
		params["capacity"] = strings.Join(counts, ",")
	}
	return c.store.AppendAudit(ctx, &types.AuditEntry{
		IncidentID: inc.ID, Time: time.Now(), FromState: inc.State, ToState: inc.State, Actor: "system",
		Action: replacementPoolAction, DryRun: inc.DryRun,
		Result: fmt.Sprintf("removing %s from pool %s; the node that joins the pool after it is looked for there", pin.Node, pool),
		Params: params,
	})
}

// recordReplacementInstance writes the instance the provider found new in
// the pool once a remove-first replacement it carried out in one call
// returned.
func (c *Controller) recordReplacementInstance(ctx context.Context, inc *types.Incident, pin cloud.NodeIdentity, providerID string) error {
	return c.store.AppendAudit(ctx, &types.AuditEntry{
		IncidentID: inc.ID, Time: time.Now(), FromState: inc.State, ToState: inc.State, Actor: "system",
		Action: replacementInstanceAction, DryRun: inc.DryRun,
		Result: fmt.Sprintf("the pool of %s lists one new instance after the resize", pin.Node),
		Params: map[string]string{"node": pin.Node, "provider_id": providerID},
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
	var pool, named, devices *types.AuditEntry
	for _, e := range trail {
		if e.DryRun {
			continue
		}
		switch {
		case e.Result == auditStepStartResult(replace) || e.Result == auditExecutingResult(replace) ||
			e.Result == auditStepStartResult("platform.recycle_node"):
			found, named = nil, nil
			if e.Result == auditStepStartResult("platform.recycle_node") {
				pool = nil
			}
		case e.Result == auditStepOutcomeResult(replace, false, true):
			found = &cloudReplacement{since: e.Time}
		case e.Action == replacementPoolAction:
			pool = e
		case e.Action == replacementInstanceAction:
			named = e
		case e.Action == replacedDevicesAction && devices == nil && inc.CloudIdentity != nil && e.Params["node_uid"] == inc.CloudIdentity.NodeUID:
			// Bound to the node, not to the attempt: the first row stands.
			devices = e
		}
	}
	if found == nil {
		return nil, nil
	}
	if devices != nil {
		found.noDevices = devices.Params["devices"] == "false"
	}
	if s := inc.Successor; s != nil && s.Outcome == types.SuccessorReplaced && s.ReadyAt != nil {
		// The successor's agent registered before it was accepted.
		found.successor, found.since, found.agent = s, *s.ReadyAt, true
		return found, nil
	}
	if pool != nil {
		found.label, found.pool, found.since = pool.Params["label"], pool.Params["pool"], pool.Time
		found.agent = pool.Params["agent"] == "true"
		if uids := pool.Params["member_uids"]; uids != "" {
			found.members = strings.Split(uids, ",")
		}
		found.capacity = parseCapacity(pool.Params["capacity"])
		found.instance = resizeDiffInstance(inc, pool, named)
	}
	return found, nil
}

// resizeDiffInstance is the instance the provider found new in the pool for
// this replacement: from the row written after a replacement made in one
// call, or from this attempt's finished remove-first plan; "" when neither
// names one.
func resizeDiffInstance(inc *types.Incident, pool, named *types.AuditEntry) string {
	if named != nil && named.Params["node"] == pool.Params["node"] {
		return named.Params["provider_id"]
	}
	if rec := inc.RemoveFirst; rec != nil && rec.Outcome == types.RemoveFirstRestored && rec.Operation == successorOperation(inc) {
		return resizediff.FromPlan(rec.RemoveFirstPlan)
	}
	return ""
}

// verifyReplacement verifies the node that took the target's place: it is
// there, Ready, the one the replacement is tied to, and its agent reports its
// accelerators since. It returns what was confirmed, or why it waits, and the
// evidence the resolution records.
func (c *Controller) verifyReplacement(ctx context.Context, inc *types.Incident, r *cloudReplacement) (bool, string, map[string]string) {
	node, found, how, reason := c.replacementNode(ctx, inc, r)
	if reason != "" {
		return false, reason, nil
	}
	ok, depth := c.verifyReplacementInventory(ctx, inc, node, r)
	if !ok {
		return false, depth, nil
	}
	evidence := map[string]string{
		"infrastructure_evidence": replacementInfrastructureEvidence,
		"replacement_node":        node.Name,
		"replacement_node_uid":    node.UID,
		"replacement_correlation": how,
	}
	if how == replacementByResizeDiff {
		evidence["replacement_provider_id"] = r.instance
	}
	return true, found + " " + depth, evidence
}

// replacementNode finds the node that took the target's place, says in one
// sentence how it was found, and names the correlation.
func (c *Controller) replacementNode(ctx context.Context, inc *types.Incident, r *cloudReplacement) (node platform.SuccessorNode, found, how, reason string) {
	readCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	if s := r.successor; s != nil {
		plat, ok := c.platform.(platform.SuccessorNodeRecycler)
		if !ok {
			return platform.SuccessorNode{}, "", "", "The successor node cannot be read on this platform."
		}
		node, err := plat.SuccessorNode(readCtx, s.ProviderID)
		switch {
		case err != nil:
			return node, "", "", fmt.Sprintf("New node %s could not be read.", s.Node)
		case !node.Found:
			return node, "", "", fmt.Sprintf("New node %s is not in the cluster.", s.Node)
		case node.Name != s.Node || node.UID != s.NodeUID:
			return node, "", "", fmt.Sprintf("New node %s is no longer the node the cloud operation named.", s.Node)
		}
		return node, fmt.Sprintf("Replaced by node %s, named by the cloud operation.", node.Name) + successorPlacement(s, inc.Target.Node), replacementBySuccessor, ""
	}
	if r.pool == "" {
		return platform.SuccessorNode{}, "", "", "The replaced node's pool was not recorded, so no node can be tied to the replacement."
	}
	if r.members == nil {
		return platform.SuccessorNode{}, "", "", fmt.Sprintf("The nodes of pool %s before the removal were not recorded, so no node can be tied to the replacement.", r.pool)
	}
	lister, ok := c.platform.(platform.PoolNodeLister)
	if !ok {
		return platform.SuccessorNode{}, "", "", "The pool's nodes cannot be read on this platform."
	}
	members, err := lister.PoolNodes(readCtx, r.label, r.pool)
	if err != nil {
		return platform.SuccessorNode{}, "", "", fmt.Sprintf("The nodes of pool %s could not be read.", r.pool)
	}
	if node, ok := c.resizeDiffNode(readCtx, r, members); ok {
		return node, fmt.Sprintf("Replaced by node %s, the only instance that joined pool %s during the resize.", node.Name, r.pool), replacementByResizeDiff, ""
	}
	// A node is new by its UID: a group can recreate an instance under the
	// old node's name, and the cluster's and the controller's clocks differ.
	var joined []platform.SuccessorNode
	for _, n := range members {
		if n.UID != "" && !slices.Contains(r.members, n.UID) {
			joined = append(joined, n)
		}
	}
	switch len(joined) {
	case 0:
		return platform.SuccessorNode{}, "", "", fmt.Sprintf("No node joined pool %s after the removal.", r.pool)
	case 1:
	default:
		return platform.SuccessorNode{}, "", "", fmt.Sprintf("Several nodes joined pool %s after the removal. KubeNeuron could not determine which one replaced the node.", r.pool)
	}
	node = joined[0]
	return node, fmt.Sprintf("Node %s is Ready. It was the only node that joined pool %s after the removal.", node.Name, r.pool), replacementByPool, ""
}

// resizeDiffNode is the node of the instance the provider found new in the
// pool, when it is in the pool now and was not in it before the delete;
// false sends the search to the pool alone.
func (c *Controller) resizeDiffNode(ctx context.Context, r *cloudReplacement, members []platform.SuccessorNode) (platform.SuccessorNode, bool) {
	finder, ok := c.platform.(platform.SuccessorNodeRecycler)
	if r.instance == "" || !ok {
		return platform.SuccessorNode{}, false
	}
	node, err := finder.SuccessorNode(ctx, r.instance)
	if err != nil || !node.Found || node.UID == "" || slices.Contains(r.members, node.UID) {
		return platform.SuccessorNode{}, false
	}
	for _, m := range members {
		if m.UID == node.UID && m.Name == node.Name {
			return node, true
		}
	}
	return platform.SuccessorNode{}, false
}

// parseCapacity reads the capacity param of a replacement-pool row; nil when
// it is empty or malformed.
func parseCapacity(param string) []types.IncidentAccelerator {
	if param == "" {
		return nil
	}
	var out []types.IncidentAccelerator
	for _, kv := range strings.Split(param, ",") {
		name, count, ok := strings.Cut(kv, "=")
		n, err := strconv.ParseInt(count, 10, 64)
		if !ok || name == "" || err != nil || n <= 0 {
			return nil
		}
		out = append(out, types.IncidentAccelerator{Resource: name, Count: n})
	}
	return out
}

// lacksCapacity says why node offers fewer accelerators than want, which
// whom names, or "" when it offers at least as many.
func lacksCapacity(want []types.IncidentAccelerator, whom string, node platform.SuccessorNode) string {
	for _, a := range want {
		if node.Allocatable[a.Resource] < a.Count {
			return fmt.Sprintf("New node %s has %d allocatable %s, fewer than the %d %s.", node.Name, node.Allocatable[a.Resource], a.Resource, a.Count, whom)
		}
	}
	return ""
}

// replacementReportVendor is the vendor whose agent report the new node is
// expected to send, from the accelerators the incident covered, and false
// when no agent of this build sends a report for them. An incident that
// recorded neither reads NVIDIA, as verifyRuntimeEvidence does.
func replacementReportVendor(inc *types.Incident) (types.AcceleratorVendor, bool) {
	if inc.Vendor.Valid() && reportingVendors[inc.Vendor] {
		return inc.Vendor, true
	}
	for _, a := range inc.Accelerators {
		switch {
		case strings.HasPrefix(a.Resource, "nvidia.com/"):
			return types.AcceleratorVendorNVIDIA, true
		case strings.HasPrefix(a.Resource, "aws.amazon.com/neuron"):
			return types.AcceleratorVendorAWS, true
		}
	}
	if inc.Vendor.Valid() || len(inc.Accelerators) > 0 {
		return inc.Vendor, false
	}
	return types.AcceleratorVendorNVIDIA, true
}

// verifyReplacementInventory reads the new node's accelerators from its
// agent's report, at the depth the vendor allows. Where no report can come
// (a vendor no agent of this build reports, or a pool whose nodes run no
// agent), it confirms the node's readiness and its advertised accelerators,
// and the agent's heartbeat when the replaced node ran one.
func (c *Controller) verifyReplacementInventory(ctx context.Context, inc *types.Incident, node platform.SuccessorNode, r *cloudReplacement) (bool, string) {
	if !node.Ready {
		return false, fmt.Sprintf("New node %s is not Ready.", node.Name)
	}
	if node.Unschedulable {
		return false, fmt.Sprintf("New node %s is not schedulable.", node.Name)
	}
	want, whom := inc.Accelerators, "the incident covered"
	if len(want) == 0 && r.successor == nil {
		// The incident recorded none: the replaced node's advertised
		// capacity is the base, no stricter than the node itself.
		want, whom = r.capacity, "the replaced node advertised"
	}
	if reason := lacksCapacity(want, whom, node); reason != "" {
		return false, reason
	}
	vendor, reported := replacementReportVendor(inc)
	now := time.Now()
	reports, ok := c.store.(store.AcceleratorReportStore)
	var report *types.AgentAcceleratorReport
	if reported && r.agent && ok {
		report, _ = reports.GetAcceleratorReport(ctx, node.Name, vendor)
		if report != nil && (report.NodeUID != node.UID || !report.ObservedAt.After(r.since) ||
			report.ObservedAt.After(now) || now.Sub(report.ObservedAt) > verifyEvidenceMaxAge) {
			report = nil
		}
	}
	// A node whose agent never reported is not asked for a report from the
	// new node; one that sends devices anyway is checked by them.
	if !reported || !r.agent || r.noDevices && (report == nil || !inventoryMatches(report, "", r.since, now)) {
		if len(want) == 0 && r.successor == nil {
			return false, fmt.Sprintf("The incident recorded no accelerators to compare new node %s against.", node.Name)
		}
		if r.agent {
			registered, err := c.store.GetNode(ctx, node.Name)
			if err != nil || registered == nil || registered.UID != node.UID ||
				!registered.AgentLastSeen.After(r.since) || now.Sub(registered.AgentLastSeen) > verifyEvidenceMaxAge {
				return false, fmt.Sprintf("No agent on new node %s has reported since the replacement.", node.Name)
			}
		}
		return true, "Verification used Kubernetes node readiness and advertised accelerator capacity."
	}
	if !ok {
		return false, "The accelerator report store is unavailable."
	}
	if report == nil {
		return false, fmt.Sprintf("No fresh accelerator report from new node %s.", node.Name)
	}
	if !inventoryMatches(report, "", r.since, now) {
		return false, fmt.Sprintf("New node %s reports no accelerator devices.", node.Name)
	}
	switch report.Readiness {
	case types.AcceleratorReadinessDegraded:
		return true, "Accelerator inventory and driver probe observed; DCGM not present, device health not checked."
	case types.AcceleratorReadinessObserved:
		return true, "Device presence observed; runtime not attested."
	}
	return true, "Fresh accelerator inventory and driver health observed."
}
