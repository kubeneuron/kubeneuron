// SPDX-License-Identifier: Apache-2.0

// Package azure replaces the AKS machine behind a GPU node.
//
// AKS has no supported way to restart one node: a reboot, deallocate or
// reimage of the VMSS instance is a direct VMSS call, and those leave the
// cluster unsupported. So this provider declares no in-place reinitialize and
// replaces a node only through the AKS agent pool API — delete that machine
// from its pool — after the controller has cordoned and drained it.
package azure

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"

	"github.com/kubeneuron/kubeneuron/pkg/cloud"
)

// providerName is this provider's registry key and matches spec.cloud.provider.
const providerName = "azure"

// capabilities: replacement only. AKS has no supported single-node restart.
var capabilities = cloud.Capabilities{Replace: true}

// Option keys of cloud.Config.Options.
const (
	OptionSubscription  = "subscriptionID"
	OptionResourceGroup = "resourceGroup"
	OptionCluster       = "cluster"
)

type settings struct {
	subscription, resourceGroup, cluster string
}

func settingsFrom(cfg cloud.Config) (settings, error) {
	s := settings{
		subscription:  cfg.Options[OptionSubscription],
		resourceGroup: cfg.Options[OptionResourceGroup],
		cluster:       cfg.Options[OptionCluster],
	}
	if s.subscription == "" || s.resourceGroup == "" || s.cluster == "" {
		return s, errors.New("azure: the AKS subscription ID, resource group and cluster name are required")
	}
	return s, nil
}

// Provider is the Azure cloud.Provider for AKS.
type Provider struct {
	settings
	api aksAPI

	mu        sync.Mutex
	nodeGroup string // the cluster's node resource group, read once
}

var (
	_ cloud.Provider            = (*Provider)(nil)
	_ cloud.RepairAwareProvider = (*Provider)(nil)
	_ cloud.NodeEventsAware     = (*Provider)(nil)
)

func init() {
	cloud.Register(providerName, capabilities, func(_ context.Context, cfg cloud.Config) (cloud.Provider, error) {
		s, err := settingsFrom(cfg)
		if err != nil {
			return nil, err
		}
		api, err := newSDKAPI(s)
		if err != nil {
			// Without a workload identity the controller still runs: every
			// cloud call fails with the cause, so each recovery is refused
			// with a reason instead of the controller restarting in a loop.
			return &Provider{settings: s, api: unavailableAPI{err}}, nil
		}
		return &Provider{settings: s, api: api}, nil
	})
}

// newWithAPI builds a Provider over an explicit AKS client, for tests.
func newWithAPI(s settings, api aksAPI) *Provider { return &Provider{settings: s, api: api} }

// Name identifies this provider in the registry and the CR.
func (p *Provider) Name() string { return providerName }

// Capabilities reports replacement only.
func (p *Provider) Capabilities() cloud.Capabilities { return capabilities }

// InstanceID extracts the VMSS instance from an AKS node providerID. The ID is
// the ARM ID of the instance, which is how AKS names the machine behind it.
func (p *Provider) InstanceID(providerID string) (string, error) {
	t, err := parseInstance(providerID)
	if err != nil {
		return "", err
	}
	return t.id, nil
}

// CheckRecycle always refuses: AKS has no supported single-node restart, so a
// ladder goes straight to replacement.
func (p *Provider) CheckRecycle(context.Context, string) error {
	return fmt.Errorf("%w: AKS has no supported restart of a single node; use ReplaceNode", cloud.ErrRecycleNotViable)
}

// Recycle is never viable on AKS; see CheckRecycle.
func (p *Provider) Recycle(ctx context.Context, instanceID string) error {
	return p.CheckRecycle(ctx, instanceID)
}

// instance is a parsed VMSS instance ID.
type instance struct {
	id, subscription, resourceGroup, scaleSet, index string
}

// The cloud provider writes azure:///subscriptions/<sub>/resourceGroups/<rg>/
// providers/Microsoft.Compute/virtualMachineScaleSets/<vmss>/virtualMachines/<n>.
// A standalone VM (an availability set or a VirtualMachines pool) does not
// match, and fails closed.
var vmssInstance = regexp.MustCompile(`(?i)^/subscriptions/([^/]+)/resourceGroups/([^/]+)/providers/Microsoft\.Compute/virtualMachineScaleSets/([^/]+)/virtualMachines/([0-9]+)$`)

func parseInstance(providerID string) (instance, error) {
	if providerID == "" {
		return instance{}, errors.New("has no providerID; the node is not backed by a cloud instance")
	}
	rest, ok := strings.CutPrefix(providerID, "azure://")
	if !ok {
		return instance{}, fmt.Errorf("providerID %q is not an Azure instance", providerID)
	}
	m := vmssInstance.FindStringSubmatch(rest)
	if m == nil {
		return instance{}, fmt.Errorf("providerID %q is not an AKS scale set instance", providerID)
	}
	return instance{id: rest, subscription: m[1], resourceGroup: m[2], scaleSet: m[3], index: m[4]}, nil
}

// AKS names a VMSS pool's scale set aks-<pool>-<number>-vmss. The pool name
// read from it is only a lookup key: the instance must then appear as a
// machine of that pool of this cluster, or nothing is done.
var aksScaleSet = regexp.MustCompile(`^aks-([a-z][a-z0-9]{0,11})-[0-9]+-vmss$`)

// target is a node's machine, confirmed through AKS to belong to this cluster.
type target struct {
	instance
	pool    string
	machine string
}

// resolve confirms that an instance is a machine of an agent pool of this
// cluster, through AKS. Anything else — another subscription, another
// cluster's node resource group, a scale set AKS does not list — is refused.
func (p *Provider) resolve(ctx context.Context, instanceID string) (target, []machine, error) {
	t := target{}
	inst, err := parseInstance("azure://" + instanceID)
	if err != nil {
		return t, nil, err
	}
	t.instance = inst
	if !strings.EqualFold(inst.subscription, p.subscription) {
		return t, nil, fmt.Errorf("azure: instance %s is outside subscription %s", instanceID, p.subscription)
	}
	nodeGroup, err := p.nodeResourceGroup(ctx)
	if err != nil {
		return t, nil, err
	}
	if !strings.EqualFold(inst.resourceGroup, nodeGroup) {
		return t, nil, fmt.Errorf("azure: instance %s is not in the node resource group of AKS cluster %s", instanceID, p.cluster)
	}
	m := aksScaleSet.FindStringSubmatch(strings.ToLower(inst.scaleSet))
	if m == nil {
		return t, nil, fmt.Errorf("azure: scale set %q is not an AKS agent pool", inst.scaleSet)
	}
	t.pool = m[1]
	machines, err := p.api.Machines(ctx, t.pool)
	if err != nil {
		return t, nil, fmt.Errorf("azure: listing machines of agent pool %s: %w", t.pool, err)
	}
	for _, mc := range machines {
		if strings.EqualFold(mc.ResourceID, instanceID) {
			t.machine = mc.Name
		}
	}
	return t, machines, nil
}

func (p *Provider) nodeResourceGroup(ctx context.Context) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.nodeGroup != "" {
		return p.nodeGroup, nil
	}
	rg, err := p.api.NodeResourceGroup(ctx)
	if err != nil {
		return "", fmt.Errorf("azure: reading AKS cluster %s: %w", p.cluster, err)
	}
	p.nodeGroup = rg
	return rg, nil
}

// poolScope is the ARM ID of an agent pool, the scope of its permissions.
func (p *Provider) poolScope(pool string) string {
	return fmt.Sprintf("/subscriptions/%s/resourceGroups/%s/providers/Microsoft.ContainerService/managedClusters/%s/agentPools/%s",
		p.subscription, p.resourceGroup, p.cluster, pool)
}
