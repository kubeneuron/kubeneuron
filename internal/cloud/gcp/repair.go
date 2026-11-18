// SPDX-License-Identifier: Apache-2.0

package gcp

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/kubeneuron/kubeneuron/pkg/cloud"
)

// maintenanceLabel is set by GKE on a node while its host is under
// maintenance.
const maintenanceLabel = "cloud.google.com/active-node-maintenance"

// basePermissions are what Replace and add-first read and call on an
// autoscaled pool; fixedPoolPermissions add setting the pool's size.
var (
	basePermissions = []string{
		"container.clusters.get",
		"container.operations.list",
		"container.operations.get",
		"compute.instances.get",
		"compute.instanceGroupManagers.get",
		"compute.instanceGroupManagers.update",
	}
	fixedPoolPermissions = append(slices.Clone(basePermissions), "container.clusters.update")
)

// member confirms the node's pinned instance is a member of a node pool of
// this cluster.
func (p *Provider) member(ctx context.Context, ref cloud.NodeRef) (target, cluster, error) {
	if err := ref.Identity.Validate(); err != nil {
		return target{}, cluster{}, err
	}
	id, err := p.InstanceID(ref.Identity.ProviderID)
	if err != nil || id != ref.Identity.InstanceID {
		return target{}, cluster{}, errors.New("pinned GKE identity is inconsistent")
	}
	t, c, err := p.resolve(ctx, id)
	if err != nil {
		return t, c, err
	}
	if t.name != ref.Identity.Node {
		return t, c, fmt.Errorf("gcp: instance %s is not node %s", t.name, ref.Identity.Node)
	}
	return t, c, nil
}

// RepairPolicy: on GKE Standard, node auto-repair acts on a node that is
// NotReady, or reports no status, or has run out of boot disk; KubeNeuron
// handles GPU faults on nodes that stay Ready and stands back once one is
// not. A pool without auto-repair is off. Autopilot is observe-only.
func (p *Provider) RepairPolicy(ctx context.Context, ref cloud.NodeRef) (cloud.RepairPolicy, error) {
	t, c, err := p.member(ctx, ref)
	if err != nil {
		return cloud.RepairPolicy{Mode: cloud.RepairUnknown}, err
	}
	switch {
	case c.Autopilot:
		return cloud.RepairPolicy{Mode: cloud.RepairOn, Reason: "GKE Autopilot manages its nodes; observe-only"}, nil
	case t.pool.AutoRepair:
		return cloud.RepairPolicy{Mode: cloud.RepairNotReadyOnly, Reason: fmt.Sprintf("GKE node auto-repair is on for node pool %s and acts only on nodes that are not Ready", t.pool.Name)}, nil
	}
	return cloud.RepairPolicy{Mode: cloud.RepairOff, Reason: fmt.Sprintf("GKE node auto-repair is off for node pool %s", t.pool.Name)}, nil
}

// OpenRepair is Known only for a Ready node without host maintenance, in a
// pool with no GKE operation running: an auto-repair of the pool, or of the
// cluster, is a repair under way; any other operation on the pool may change
// it under the replacement.
func (p *Provider) OpenRepair(ctx context.Context, ref cloud.NodeRef) (cloud.RepairObservation, error) {
	t, _, err := p.member(ctx, ref)
	if err != nil {
		return cloud.RepairObservation{}, err
	}
	if !ref.Ready {
		return cloud.RepairObservation{Reason: "node is NotReady; GKE auto-repair may act on it"}, nil
	}
	now := p.now()
	if ref.Labels[maintenanceLabel] == "true" {
		return maintenance(ref.Identity.Node, maintenanceLabel, now), nil
	}
	v, err := p.api.Instance(ctx, t.instance)
	if err != nil {
		return cloud.RepairObservation{}, fmt.Errorf("gcp: reading instance %s: %w", t.id(), err)
	}
	if v.Maintenance != "" {
		return maintenance(ref.Identity.Node, "upcoming host maintenance: "+v.Maintenance, now), nil
	}
	ops, err := p.api.Operations(ctx)
	if err != nil {
		return cloud.RepairObservation{}, fmt.Errorf("gcp: listing GKE operations: %w", err)
	}
	if op, ok := p.repairing(ops, t.pool.Name); ok {
		return cloud.RepairObservation{Known: true, Active: true, Reason: "a GKE node auto-repair is running",
			Evidence: []cloud.RepairEvidence{{Source: "gke-operations", Resource: t.pool.Name, Detail: op.Type + " " + op.Status, ObservedAt: now}}}, nil
	}
	if op, ok := p.running(ops, t.pool.Name); ok {
		return cloud.RepairObservation{Reason: fmt.Sprintf("node pool %s has GKE operation %s running (%s)", t.pool.Name, op.Name, op.Type)}, nil
	}
	return cloud.RepairObservation{Known: true, Reason: "node Ready, no host maintenance, no GKE operation running on the node pool"}, nil
}

func maintenance(node, detail string, now time.Time) cloud.RepairObservation {
	return cloud.RepairObservation{Known: true, Active: true, Reason: "the node's host is under or due for maintenance; no recovery step starts",
		Evidence: []cloud.RepairEvidence{{Source: "gce-host-maintenance", Resource: node, Detail: detail, ObservedAt: now}}}
}

// onPool reports whether an operation targets the pool, or a node of it.
func (p *Provider) onPool(op operation, pool string) bool {
	path := "/clusters/" + p.cluster + "/nodePools/" + pool
	return strings.HasSuffix(op.Target, path) || strings.Contains(op.Target, path+"/")
}

// onCluster reports whether an operation targets the cluster itself.
func (p *Provider) onCluster(op operation) bool {
	return strings.HasSuffix(op.Target, "/clusters/"+p.cluster)
}

// repairing finds a node auto-repair that is not done, of the pool or of the
// whole cluster: GKE may run several at once and names one node of many.
func (p *Provider) repairing(ops []operation, pool string) (operation, bool) {
	for _, op := range ops {
		if op.Type == opAutoRepair && op.Status != opDone && (p.onPool(op, pool) || p.onCluster(op)) {
			return op, true
		}
	}
	return operation{}, false
}

// running finds any operation on the pool that is not done.
func (p *Provider) running(ops []operation, pool string) (operation, bool) {
	if op, ok := p.repairing(ops, pool); ok {
		return op, true
	}
	for _, op := range ops {
		if op.Status != opDone && p.onPool(op, pool) {
			return op, true
		}
	}
	return operation{}, false
}

// CheckReplacePermission asks Resource Manager which of the permissions the
// replacement needs the controller holds on the project: nothing is changed
// before a missing one shows. A grant on narrower resources is not seen
// here, so the role is granted on the project.
func (p *Provider) CheckReplacePermission(ctx context.Context, instanceID string) error {
	if instanceID == "" {
		return errors.New("missing pinned instance ID")
	}
	t, _, err := p.resolve(ctx, instanceID)
	if err != nil {
		return err
	}
	want := fixedPoolPermissions
	if t.pool.Autoscaling {
		want = basePermissions
	} else if _, err := p.zoneSize(ctx, t.pool); err != nil {
		// Shown before the node is cordoned, not after it is drained.
		return err
	}
	held, err := p.api.Permissions(ctx, want)
	if err != nil {
		return fmt.Errorf("gcp: testing permissions on project %s: %w", p.project, err)
	}
	var missing []string
	for _, perm := range want {
		if !slices.Contains(held, perm) {
			missing = append(missing, perm)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("gcp: missing on project %s: %s", p.project, strings.Join(missing, ", "))
	}
	return nil
}
