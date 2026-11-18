// SPDX-License-Identifier: Apache-2.0

package gcp

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"strings"

	compute "cloud.google.com/go/compute/apiv1"
	"cloud.google.com/go/compute/apiv1/computepb"
	container "cloud.google.com/go/container/apiv1"
	"cloud.google.com/go/container/apiv1/containerpb"
	"cloud.google.com/go/iam/apiv1/iampb"
	resourcemanager "cloud.google.com/go/resourcemanager/apiv3"
	"github.com/googleapis/gax-go/v2/apierror"
	"google.golang.org/api/iterator"
	"google.golang.org/grpc/codes"
)

// cluster is what this package reads from a GKE cluster.
type cluster struct {
	Autopilot bool
	Pools     []nodePool
}

func (c cluster) pool(name string) (nodePool, bool) {
	for _, pl := range c.Pools {
		if pl.Name == name {
			return pl, true
		}
	}
	return nodePool{}, false
}

// nodePool is one GKE node pool and the managed instance groups behind it,
// one per zone.
type nodePool struct {
	Name        string
	Autoscaling bool
	AutoRepair  bool
	Groups      []groupRef
}

// groupRef names a zonal managed instance group.
type groupRef struct {
	project, zone, name string
}

// url is the group's partial URL, as a node pool lists it.
func (g groupRef) url() string {
	return fmt.Sprintf("projects/%s/zones/%s/instanceGroupManagers/%s", g.project, g.zone, g.name)
}

func (g groupRef) instance(name string) instance {
	return instance{project: g.project, zone: g.zone, name: name}
}

var groupURL = regexp.MustCompile(`(?:^|/)projects/([^/]+)/zones/([^/]+)/instanceGroup(?:Manager)?s/([^/]+)$`)

func parseGroup(url string) (groupRef, error) {
	m := groupURL.FindStringSubmatch(url)
	if m == nil {
		return groupRef{}, fmt.Errorf("%q is not a zonal instance group", url)
	}
	return groupRef{project: m[1], zone: m[2], name: m[3]}, nil
}

// group is a managed instance group's size and whether it has settled.
type group struct {
	TargetSize int32
	Stable     bool
}

// member is one managed instance of a group. Errors are the codes of its last
// creation attempt.
type member struct {
	Name   string
	Action string // currentAction: NONE once the instance is up
	Status string // instanceStatus: RUNNING once the VM runs
	Errors []string
}

// vm is what this package reads from a Compute Engine instance.
type vm struct {
	Labels map[string]string
	// Maintenance is the instance's upcoming host maintenance, if any.
	Maintenance string
}

// operation is one GKE operation of the cluster's location. Target is its
// target link; Error the code and message of a failed one.
type operation struct {
	Name, Type, Status, Target string
	ErrorCode                  codes.Code
	Error                      string
}

const (
	opDone       = "DONE"
	opAutoRepair = "AUTO_REPAIR_NODES"
	opSetSize    = "SET_NODE_POOL_SIZE"
)

// gkeAPI is every Google Cloud call this package makes. It is an interface so
// tests substitute a fake and no test ever reaches Google Cloud. Node pool
// sizes change only through GKE; the only Compute Engine change is deleting a
// named instance from its pool's group.
type gkeAPI interface {
	Cluster(ctx context.Context) (cluster, error)
	// Instance reads a VM; a missing one is errGone.
	Instance(ctx context.Context, in instance) (vm, error)
	Group(ctx context.Context, g groupRef) (group, error)
	Members(ctx context.Context, g groupRef) ([]member, error)
	// Operations lists the GKE operations of the cluster's location.
	Operations(ctx context.Context) ([]operation, error)
	Operation(ctx context.Context, name string) (operation, error)
	// SetPoolSize asks GKE to set the node count of every zone of a pool and
	// returns the operation without waiting for it.
	SetPoolSize(ctx context.Context, pool string, size int32) (string, error)
	// DeleteInstances deletes the named members of a group, which shrinks it
	// by as many, and waits for Compute Engine to finish. requestID makes a
	// resent request the same request.
	DeleteInstances(ctx context.Context, g groupRef, names []string, requestID string) error
	// Permissions returns which of the permissions the caller has on the
	// project.
	Permissions(ctx context.Context, permissions []string) ([]string, error)
}

// unavailableAPI stands in for the Google SDK when the controller has no
// credential: every call fails with why.
type unavailableAPI struct{ err error }

func (u unavailableAPI) Cluster(context.Context) (cluster, error)       { return cluster{}, u.err }
func (u unavailableAPI) Instance(context.Context, instance) (vm, error) { return vm{}, u.err }
func (u unavailableAPI) Group(context.Context, groupRef) (group, error) { return group{}, u.err }
func (u unavailableAPI) Members(context.Context, groupRef) ([]member, error) {
	return nil, u.err
}
func (u unavailableAPI) Operations(context.Context) ([]operation, error) { return nil, u.err }
func (u unavailableAPI) Operation(context.Context, string) (operation, error) {
	return operation{}, u.err
}
func (u unavailableAPI) SetPoolSize(context.Context, string, int32) (string, error) {
	return "", u.err
}
func (u unavailableAPI) DeleteInstances(context.Context, groupRef, []string, string) error {
	return u.err
}
func (u unavailableAPI) Permissions(context.Context, []string) ([]string, error) { return nil, u.err }

// sdkAPI implements gkeAPI over the Google Cloud client libraries.
type sdkAPI struct {
	settings
	clusters  *container.ClusterManagerClient
	instances *compute.InstancesClient
	groups    *compute.InstanceGroupManagersClient
	projects  *resourcemanager.ProjectsClient
}

// newSDKAPI builds the clients on Application Default Credentials: on GKE,
// the controller's Workload Identity Federation token from the metadata
// server. No key is read from the KubeNeuron resource or its Secrets.
func newSDKAPI(ctx context.Context, s settings) (*sdkAPI, error) {
	clusters, err := container.NewClusterManagerRESTClient(ctx)
	if err != nil {
		return nil, fmt.Errorf("gcp: GKE client (are Application Default Credentials available to the controller pod?): %w", err)
	}
	instances, err := compute.NewInstancesRESTClient(ctx)
	if err != nil {
		return nil, fmt.Errorf("gcp: Compute Engine client: %w", err)
	}
	groups, err := compute.NewInstanceGroupManagersRESTClient(ctx)
	if err != nil {
		return nil, fmt.Errorf("gcp: Compute Engine client: %w", err)
	}
	projects, err := resourcemanager.NewProjectsRESTClient(ctx)
	if err != nil {
		return nil, fmt.Errorf("gcp: Resource Manager client: %w", err)
	}
	return &sdkAPI{settings: s, clusters: clusters, instances: instances, groups: groups, projects: projects}, nil
}

func (a *sdkAPI) clusterName() string {
	return fmt.Sprintf("projects/%s/locations/%s/clusters/%s", a.project, a.location, a.cluster)
}

func (a *sdkAPI) Cluster(ctx context.Context) (cluster, error) {
	out, err := a.clusters.GetCluster(ctx, &containerpb.GetClusterRequest{Name: a.clusterName()})
	if err != nil {
		return cluster{}, err
	}
	c := cluster{Autopilot: out.GetAutopilot().GetEnabled()}
	for _, np := range out.GetNodePools() {
		pl := nodePool{
			Name:        np.GetName(),
			Autoscaling: np.GetAutoscaling().GetEnabled(),
			AutoRepair:  np.GetManagement().GetAutoRepair(),
		}
		for _, url := range np.GetInstanceGroupUrls() {
			g, err := parseGroup(url)
			if err != nil {
				return cluster{}, fmt.Errorf("node pool %s: %w", pl.Name, err)
			}
			pl.Groups = append(pl.Groups, g)
		}
		c.Pools = append(c.Pools, pl)
	}
	return c, nil
}

func (a *sdkAPI) Instance(ctx context.Context, in instance) (vm, error) {
	out, err := a.instances.Get(ctx, &computepb.GetInstanceRequest{Project: in.project, Zone: in.zone, Instance: in.name})
	if notFound(err) {
		return vm{}, errGone
	}
	if err != nil {
		return vm{}, err
	}
	v := vm{Labels: out.GetLabels()}
	if m := out.GetResourceStatus().GetUpcomingMaintenance(); m != nil {
		v.Maintenance = strings.TrimSpace(m.GetType() + " " + m.GetWindowStartTime())
		if v.Maintenance == "" {
			v.Maintenance = "scheduled"
		}
	}
	return v, nil
}

func (a *sdkAPI) Group(ctx context.Context, g groupRef) (group, error) {
	out, err := a.groups.Get(ctx, &computepb.GetInstanceGroupManagerRequest{Project: g.project, Zone: g.zone, InstanceGroupManager: g.name})
	if err != nil {
		return group{}, err
	}
	return group{TargetSize: out.GetTargetSize(), Stable: out.GetStatus().GetIsStable()}, nil
}

func (a *sdkAPI) Members(ctx context.Context, g groupRef) ([]member, error) {
	var all []member
	it := a.groups.ListManagedInstances(ctx, &computepb.ListManagedInstancesInstanceGroupManagersRequest{Project: g.project, Zone: g.zone, InstanceGroupManager: g.name})
	for {
		mi, err := it.Next()
		if errors.Is(err, iterator.Done) {
			return all, nil
		}
		if err != nil {
			return nil, err
		}
		name := mi.GetName()
		if name == "" {
			name = mi.GetInstance()[strings.LastIndex(mi.GetInstance(), "/")+1:]
		}
		if name == "" {
			return nil, fmt.Errorf("instance group %s lists a member without a name", g.name)
		}
		m := member{Name: name, Action: mi.GetCurrentAction(), Status: mi.GetInstanceStatus()}
		for _, e := range mi.GetLastAttempt().GetErrors().GetErrors() {
			m.Errors = append(m.Errors, e.GetCode())
		}
		all = append(all, m)
	}
}

func toOperation(op *containerpb.Operation) operation {
	o := operation{Name: op.GetName(), Type: op.GetOperationType().String(), Status: op.GetStatus().String(), Target: op.GetTargetLink()}
	if e := op.GetError(); e != nil && e.GetCode() != 0 {
		o.ErrorCode, o.Error = codes.Code(e.GetCode()), e.GetMessage()
	}
	return o
}

func (a *sdkAPI) Operations(ctx context.Context) ([]operation, error) {
	out, err := a.clusters.ListOperations(ctx, &containerpb.ListOperationsRequest{Parent: fmt.Sprintf("projects/%s/locations/%s", a.project, a.location)})
	if err != nil {
		return nil, err
	}
	if len(out.GetMissingZones()) > 0 {
		return nil, fmt.Errorf("GKE could not list operations in zones %s", strings.Join(out.GetMissingZones(), ", "))
	}
	ops := make([]operation, 0, len(out.GetOperations()))
	for _, op := range out.GetOperations() {
		ops = append(ops, toOperation(op))
	}
	return ops, nil
}

func (a *sdkAPI) Operation(ctx context.Context, name string) (operation, error) {
	op, err := a.clusters.GetOperation(ctx, &containerpb.GetOperationRequest{Name: fmt.Sprintf("projects/%s/locations/%s/operations/%s", a.project, a.location, name)})
	if err != nil {
		return operation{}, err
	}
	return toOperation(op), nil
}

func (a *sdkAPI) SetPoolSize(ctx context.Context, pool string, size int32) (string, error) {
	op, err := a.clusters.SetNodePoolSize(ctx, &containerpb.SetNodePoolSizeRequest{Name: a.clusterName() + "/nodePools/" + pool, NodeCount: size})
	if err != nil {
		return "", err
	}
	return op.GetName(), nil
}

func (a *sdkAPI) DeleteInstances(ctx context.Context, g groupRef, names []string, requestID string) error {
	urls := make([]string, 0, len(names))
	for _, n := range names {
		urls = append(urls, fmt.Sprintf("projects/%s/zones/%s/instances/%s", g.project, g.zone, n))
	}
	op, err := a.groups.DeleteInstances(ctx, &computepb.DeleteInstancesInstanceGroupManagerRequest{
		Project: g.project, Zone: g.zone, InstanceGroupManager: g.name, RequestId: &requestID,
		InstanceGroupManagersDeleteInstancesRequestResource: &computepb.InstanceGroupManagersDeleteInstancesRequest{Instances: urls},
	})
	if err != nil {
		return err
	}
	return op.Wait(ctx)
}

func (a *sdkAPI) Permissions(ctx context.Context, permissions []string) ([]string, error) {
	out, err := a.projects.TestIamPermissions(ctx, &iampb.TestIamPermissionsRequest{Resource: "projects/" + a.project, Permissions: permissions})
	if err != nil {
		return nil, err
	}
	return out.GetPermissions(), nil
}

func notFound(err error) bool {
	var ae *apierror.APIError
	return errors.As(err, &ae) && (ae.HTTPCode() == http.StatusNotFound || ae.GRPCStatus().Code() == codes.NotFound)
}

// capacityCodes are the Compute Engine error codes of a creation attempt that
// found no capacity or quota.
var capacityCodes = []string{
	"ZONE_RESOURCE_POOL_EXHAUSTED", "ZONE_RESOURCE_POOL_EXHAUSTED_WITH_DETAILS",
	"QUOTA_EXCEEDED", "RESOURCE_POOL_EXHAUSTED", "STOCKOUT",
}

func capacityCode(code string) bool { return slices.Contains(capacityCodes, code) }

// capacityError is a GKE or Compute Engine call refused for quota or
// capacity. A rate limit is neither: it is retried.
func capacityError(err error) bool {
	var ae *apierror.APIError
	if !errors.As(err, &ae) || strings.Contains(ae.Reason(), "RATE_LIMIT") {
		return false
	}
	return ae.GRPCStatus().Code() == codes.ResourceExhausted || ae.HTTPCode() == http.StatusTooManyRequests || capacityCode(ae.Reason())
}
