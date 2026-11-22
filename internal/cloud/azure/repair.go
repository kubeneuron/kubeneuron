// SPDX-License-Identifier: Apache-2.0

package azure

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/kubeneuron/kubeneuron/pkg/cloud"
)

// autoRepairSource is the event source of AKS node auto-repair.
const autoRepairSource = "aks-auto-repair"

// autoRepairWindow is how long an auto-repair event keeps a node off limits:
// AKS documents a repair sequence of up to an hour.
const autoRepairWindow = time.Hour

// NodeEventSources asks the platform for AKS auto-repair's node events.
func (p *Provider) NodeEventSources() []string { return []string{autoRepairSource} }

// replaceActions are what Replace calls on an agent pool.
var replaceActions = []string{
	"Microsoft.ContainerService/managedClusters/agentPools/read",
	"Microsoft.ContainerService/managedClusters/agentPools/write",
	"Microsoft.ContainerService/managedClusters/agentPools/deleteMachines/action",
	"Microsoft.ContainerService/managedClusters/agentPools/machines/read",
}

// member confirms the node's pinned instance is a machine of this cluster.
func (p *Provider) member(ctx context.Context, ref cloud.NodeRef) (target, error) {
	if err := ref.Identity.Validate(); err != nil {
		return target{}, err
	}
	id, err := p.InstanceID(ref.Identity.ProviderID)
	if err != nil || id != ref.Identity.InstanceID {
		return target{}, errors.New("pinned AKS identity is inconsistent")
	}
	t, _, err := p.resolve(ctx, id)
	if err != nil {
		return t, err
	}
	if t.machine == "" {
		return t, fmt.Errorf("azure: instance %s is not a machine of agent pool %s", id, t.pool)
	}
	if !strings.EqualFold(t.machine, ref.Identity.Node) {
		return t, fmt.Errorf("azure: machine %s is not node %s", t.machine, ref.Identity.Node)
	}
	return t, nil
}

// RepairPolicy: AKS node auto-repair cannot be turned off, and it acts only on
// a node that is NotReady. KubeNeuron handles GPU faults on nodes that stay
// Ready and stands back once one is not.
func (p *Provider) RepairPolicy(ctx context.Context, ref cloud.NodeRef) (cloud.RepairPolicy, error) {
	if _, err := p.member(ctx, ref); err != nil {
		return cloud.RepairPolicy{Mode: cloud.RepairUnknown}, err
	}
	return cloud.RepairPolicy{Mode: cloud.RepairNotReadyOnly, Reason: "AKS node auto-repair is always on and acts only on NotReady nodes"}, nil
}

// OpenRepair is Known only for a Ready node whose events were read, with no
// recent auto-repair event, and an agent pool with no operation running. An
// event proves a repair; their absence alone does not prove none, hence the
// Ready condition and the pool state beside them.
func (p *Provider) OpenRepair(ctx context.Context, ref cloud.NodeRef) (cloud.RepairObservation, error) {
	t, err := p.member(ctx, ref)
	if err != nil {
		return cloud.RepairObservation{}, err
	}
	if !ref.Ready {
		return cloud.RepairObservation{Reason: "node is NotReady; AKS auto-repair may act on it"}, nil
	}
	if !ref.EventsKnown {
		return cloud.RepairObservation{Reason: "the node's events could not be read in full, so an AKS auto-repair cannot be excluded"}, nil
	}
	now := time.Now()
	var evidence []cloud.RepairEvidence
	for _, e := range ref.Events {
		// An event without a time may be recent.
		if e.Source == autoRepairSource && (e.Time.IsZero() || now.Sub(e.Time) < autoRepairWindow) {
			evidence = append(evidence, cloud.RepairEvidence{Source: e.Source, Resource: ref.Identity.Node, Detail: e.Reason, ObservedAt: e.Time})
		}
	}
	if len(evidence) > 0 {
		return cloud.RepairObservation{Known: true, Active: true, Reason: "AKS auto-repair acted on the node within the last hour", Evidence: evidence}, nil
	}
	pl, err := p.api.Pool(ctx, t.pool)
	if err != nil {
		return cloud.RepairObservation{}, fmt.Errorf("azure: reading agent pool %s: %w", t.pool, err)
	}
	if !strings.EqualFold(pl.ProvisioningState, "Succeeded") {
		return cloud.RepairObservation{Reason: fmt.Sprintf("agent pool %s has an operation running (state %q)", t.pool, pl.ProvisioningState)}, nil
	}
	return cloud.RepairObservation{Known: true, Reason: "node Ready, no AKS auto-repair within the last hour, agent pool settled"}, nil
}

// CheckReplacePermission reads the controller's effective permissions on the
// instance's agent pool: ARM has no dry run, and a fixed-size pool is grown
// before the delete, so a missing delete right must show before anything.
func (p *Provider) CheckReplacePermission(ctx context.Context, instanceID string) error {
	if instanceID == "" {
		return errors.New("missing pinned instance ID")
	}
	t, _, err := p.resolve(ctx, instanceID)
	if err != nil {
		return err
	}
	perms, err := p.api.Permissions(ctx, p.poolScope(t.pool))
	if err != nil {
		return permissionReadHold(t.pool, fmt.Errorf("azure: reading permissions on agent pool %s: %w", t.pool, err))
	}
	var missing []string
	for _, action := range replaceActions {
		if !allowed(perms, action) {
			missing = append(missing, action)
		}
	}
	if len(missing) > 0 {
		list := strings.Join(missing, ", ")
		return &cloud.RecoveryHold{Code: cloud.HoldPermissionMissing,
			Reason:     fmt.Sprintf("The controller's identity lacks %s on agent pool %s.", list, t.pool),
			Action:     "Grant these permissions to the controller's identity on the agent pool (see docs/cloud-azure.md). KubeNeuron checks again every minute.",
			PoolReason: "the controller lacks " + list + " on the agent pool",
			PoolAction: "grant these permissions on the agent pool (docs/cloud-azure.md)"}
	}
	return nil
}

// permissionReadHold says why the permission check could not be made. The ARM
// error carries the request URL, with the subscription and resource group, so
// it stays in Cause, for the controller log; only its code is shown.
func permissionReadHold(pool string, err error) error {
	answer := "the Azure Resource Manager call failed"
	if codes := errorCodes(err); len(codes) > 0 && codes[0] != "" {
		answer = "Azure Resource Manager answered " + codes[0]
	}
	return &cloud.RecoveryHold{Code: cloud.HoldReadFailed, Cause: err,
		Reason:     fmt.Sprintf("KubeNeuron could not read the controller's permissions on agent pool %s: %s.", pool, answer),
		Action:     "KubeNeuron checks again every minute.",
		PoolReason: "could not read permissions: " + answer}
}

// allowed applies Azure RBAC to one action: some permission grants it and that
// same permission does not exclude it. Deny assignments are not listed here and
// still apply at call time.
func allowed(perms []permission, action string) bool {
	for _, perm := range perms {
		if matchesAny(perm.Actions, action) && !matchesAny(perm.NotActions, action) {
			return true
		}
	}
	return false
}

func matchesAny(patterns []string, action string) bool {
	for _, pattern := range patterns {
		if wildcard(strings.ToLower(pattern), strings.ToLower(action)) {
			return true
		}
	}
	return false
}

// wildcard matches an RBAC action pattern, where * stands for any run of
// characters, slashes included.
func wildcard(pattern, s string) bool {
	parts := strings.Split(pattern, "*")
	if len(parts) == 1 {
		return pattern == s
	}
	if !strings.HasPrefix(s, parts[0]) {
		return false
	}
	s = s[len(parts[0]):]
	for _, part := range parts[1 : len(parts)-1] {
		i := strings.Index(s, part)
		if i < 0 {
			return false
		}
		s = s[i+len(part):]
	}
	return strings.HasSuffix(s, parts[len(parts)-1])
}
