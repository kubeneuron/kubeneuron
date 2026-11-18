// SPDX-License-Identifier: Apache-2.0

// Package gcp replaces the GKE node behind a GPU fault, on GKE Standard.
//
// It has no in-place restart: a reset or stop of a node pool's VM is a direct
// Compute Engine call GKE does not document for nodes, and a stopped member of
// the pool's managed instance group is recreated by the group. A node is
// replaced only through the node pool's managed instance group, deleting that
// one instance, and the pool's size is set back through the GKE API. Autopilot
// clusters are observe-only: GKE manages their nodes.
package gcp

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/kubeneuron/kubeneuron/pkg/cloud"
)

// providerName is this provider's registry key and matches spec.cloud.provider.
const providerName = "gcp"

// capabilities: replacement, adding the successor first on a fixed-size,
// single-zone pool. GKE has no documented single-node restart.
var capabilities = cloud.Capabilities{Replace: true, AddFirst: true}

// Option keys of cloud.Config.Options.
const (
	OptionProject  = "project"
	OptionLocation = "location"
	OptionCluster  = "cluster"
)

type settings struct {
	project, location, cluster string
	// tpuInfraHealth turns on the TPU health read from Cloud Monitoring.
	tpuInfraHealth bool
}

func settingsFrom(cfg cloud.Config) (settings, error) {
	s := settings{
		project:  cfg.Options[OptionProject],
		location: cfg.Options[OptionLocation],
		cluster:  cfg.Options[OptionCluster],

		tpuInfraHealth: cfg.Options[OptionTPUInfraHealth] == "true",
	}
	if s.project == "" || s.location == "" || s.cluster == "" {
		return s, errors.New("gcp: the GKE project, location and cluster name are required")
	}
	return s, nil
}

// Provider is the GCP cloud.Provider for GKE.
type Provider struct {
	settings
	api gkeAPI
	// monitoring is nil unless the TPU health read is turned on.
	monitoring monitoringAPI
	now        func() time.Time
}

var (
	_ cloud.Provider            = (*Provider)(nil)
	_ cloud.RepairAwareProvider = (*Provider)(nil)
)

func init() {
	cloud.Register(providerName, capabilities, func(ctx context.Context, cfg cloud.Config) (cloud.Provider, error) {
		s, err := settingsFrom(cfg)
		if err != nil {
			return nil, err
		}
		var p *Provider
		api, err := newSDKAPI(ctx, s)
		if err != nil {
			// Without credentials the controller still runs: every cloud call
			// fails with the cause, so each recovery is refused with a reason
			// instead of the controller restarting in a loop.
			p = newWithAPI(s, unavailableAPI{err})
		} else {
			p = newWithAPI(s, api)
		}
		if s.tpuInfraHealth {
			p.monitoring = newSDKMonitoring(ctx)
		}
		return p, nil
	})
}

// newWithAPI builds a Provider over an explicit GKE client, for tests.
func newWithAPI(s settings, api gkeAPI) *Provider {
	return &Provider{settings: s, api: api, now: time.Now}
}

// Name identifies this provider in the registry and the CR.
func (p *Provider) Name() string { return providerName }

// Capabilities reports replacement, with add-first.
func (p *Provider) Capabilities() cloud.Capabilities { return capabilities }

// InstanceID extracts "<project>/<zone>/<instance>" from a GKE node
// providerID.
func (p *Provider) InstanceID(providerID string) (string, error) {
	in, err := parseInstance(providerID)
	if err != nil {
		return "", err
	}
	return in.id(), nil
}

// CheckRecycle always refuses: GKE documents no restart of a single node, so
// a ladder goes straight to replacement.
func (p *Provider) CheckRecycle(context.Context, string) error {
	return fmt.Errorf("%w: GKE has no documented restart of a single node; use ReplaceNode", cloud.ErrRecycleNotViable)
}

// Recycle is never viable on GKE; see CheckRecycle.
func (p *Provider) Recycle(ctx context.Context, instanceID string) error {
	return p.CheckRecycle(ctx, instanceID)
}

// instance is a parsed Compute Engine instance.
type instance struct {
	project, zone, name string
}

func (in instance) id() string { return in.project + "/" + in.zone + "/" + in.name }

func (in instance) providerID() string { return "gce://" + in.id() }

// The cloud provider writes gce://<project>/<zone>/<instance>.
var gceInstance = regexp.MustCompile(`^([a-z][-a-z0-9.:]{4,61}[a-z0-9])/([a-z]+-[a-z]+[0-9]+-[a-z])/([a-z](?:[-a-z0-9]{0,61}[a-z0-9])?)$`)

func parseInstance(providerID string) (instance, error) {
	if providerID == "" {
		return instance{}, errors.New("has no providerID; the node is not backed by a cloud instance")
	}
	rest, ok := strings.CutPrefix(providerID, "gce://")
	if !ok {
		return instance{}, fmt.Errorf("providerID %q is not a Compute Engine instance", providerID)
	}
	return parseInstanceID(rest)
}

func parseInstanceID(id string) (instance, error) {
	m := gceInstance.FindStringSubmatch(id)
	if m == nil {
		return instance{}, fmt.Errorf("%q is not a Compute Engine instance", id)
	}
	return instance{project: m[1], zone: m[2], name: m[3]}, nil
}

// The reserved labels GKE puts on a node VM. They are lookup keys only: the
// instance must then be a member of the node pool's managed instance group.
const (
	labelCluster  = "goog-k8s-cluster-name"
	labelNodePool = "goog-k8s-node-pool-name"
)

// target is a node's instance, confirmed through GKE and Compute Engine to be
// a member of a node pool of this cluster.
type target struct {
	instance
	pool  nodePool
	group groupRef
	// members are the group's managed instances, read in the same lookup.
	members []member
}

// errGone is a target instance that no longer exists.
var errGone = errors.New("the instance no longer exists")

// resolve confirms that an instance is a member of a node pool of this
// cluster. Anything else — another project, another cluster's VM, an
// instance its pool's group does not list — is refused.
func (p *Provider) resolve(ctx context.Context, instanceID string) (target, cluster, error) {
	in, err := parseInstanceID(instanceID)
	if err != nil {
		return target{}, cluster{}, err
	}
	t := target{instance: in}
	if in.project != p.project {
		return t, cluster{}, fmt.Errorf("gcp: instance %s is outside project %s", instanceID, p.project)
	}
	vm, err := p.api.Instance(ctx, in)
	if errors.Is(err, errGone) {
		return t, cluster{}, err
	}
	if err != nil {
		return t, cluster{}, fmt.Errorf("gcp: reading instance %s: %w", instanceID, err)
	}
	if vm.Labels[labelCluster] != p.cluster || vm.Labels[labelNodePool] == "" {
		return t, cluster{}, fmt.Errorf("gcp: instance %s is not a node of GKE cluster %s", instanceID, p.cluster)
	}
	c, err := p.api.Cluster(ctx)
	if err != nil {
		return t, c, fmt.Errorf("gcp: reading GKE cluster %s: %w", p.cluster, err)
	}
	pl, ok := c.pool(vm.Labels[labelNodePool])
	if !ok {
		return t, c, fmt.Errorf("gcp: GKE cluster %s has no node pool %s", p.cluster, vm.Labels[labelNodePool])
	}
	t.pool = pl
	for _, g := range pl.Groups {
		if g.project == in.project && g.zone == in.zone {
			t.group = g
		}
	}
	if t.group.name == "" {
		return t, c, fmt.Errorf("gcp: node pool %s has no instance group in zone %s", pl.Name, in.zone)
	}
	if t.members, err = p.api.Members(ctx, t.group); err != nil {
		return t, c, fmt.Errorf("gcp: listing instance group %s: %w", t.group.name, err)
	}
	if _, ok := memberNamed(t.members, in.name); !ok {
		return t, c, fmt.Errorf("gcp: instance %s is not a member of node pool %s", instanceID, pl.Name)
	}
	return t, c, nil
}

func memberNamed(members []member, name string) (member, bool) {
	for _, m := range members {
		if m.Name == name {
			return m, true
		}
	}
	return member{}, false
}
