// SPDX-License-Identifier: Apache-2.0

package azure

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/runtime"
	aks "github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/containerservice/armcontainerservice/v9"

	"github.com/kubeneuron/kubeneuron/internal/cloud/removefirst"
)

// pool is what this package reads from an AKS agent pool. raw is the pool as
// returned, so a scale writes back everything it did not mean to change.
type pool struct {
	Zones             []string
	Count             int32
	Autoscaling       bool
	ProvisioningState string
	Tags              map[string]string
	// ETag is the pool's version; a write carries it in If-Match, so it never
	// overwrites a change made after the read.
	ETag string
	raw  aks.AgentPool
}

// machine is one AKS machine of an agent pool: its name (the node name on a
// VMSS pool) and the ARM ID of the VMSS instance behind it.
type machine struct {
	Name       string
	ResourceID string
}

// providerID is the machine as its node's spec.providerID names it, up to
// case: AKS spells the node resource group differently there.
func (m machine) providerID() string { return "azure://" + m.ResourceID }

// permission is one entry of the caller's effective permissions at a scope.
type permission struct {
	Actions    []string `json:"actions"`
	NotActions []string `json:"notActions"`
}

// aksAPI is every AKS call this package makes. It is an interface so tests
// substitute a fake and no test ever reaches Azure. Every call is an AKS
// resource provider or authorization call: nothing here touches the VMSS
// directly, which would leave the cluster unsupported.
type aksAPI interface {
	NodeResourceGroup(ctx context.Context) (string, error)
	Pool(ctx context.Context, name string) (pool, error)
	Machines(ctx context.Context, pool string) ([]machine, error)
	// WritePool writes count and tags onto the pool as last read, only if it is
	// still that version, and waits for AKS to finish.
	WritePool(ctx context.Context, name string, p pool, count int32, tags map[string]string) error
	// DeleteMachines removes the named machines and waits for AKS to finish.
	DeleteMachines(ctx context.Context, pool string, names []string) error
	// Permissions lists the caller's effective permissions at an ARM scope.
	Permissions(ctx context.Context, scope string) ([]permission, error)
}

// sdkAPI implements aksAPI over the Azure SDK, authenticated as the
// controller's workload identity.
type sdkAPI struct {
	subscription, resourceGroup, cluster string
	pools                                *aks.AgentPoolsClient
	machines                             *aks.MachinesClient
	clusters                             *aks.ManagedClustersClient
	pipeline                             runtime.Pipeline
	endpoint                             string
	pollFrequency                        time.Duration
}

// unavailableAPI stands in for the Azure SDK when the controller has no
// credential: every call fails with why.
type unavailableAPI struct{ err error }

func (u unavailableAPI) NodeResourceGroup(context.Context) (string, error) { return "", u.err }
func (u unavailableAPI) Pool(context.Context, string) (pool, error)        { return pool{}, u.err }
func (u unavailableAPI) Machines(context.Context, string) ([]machine, error) {
	return nil, u.err
}
func (u unavailableAPI) WritePool(context.Context, string, pool, int32, map[string]string) error {
	return u.err
}
func (u unavailableAPI) DeleteMachines(context.Context, string, []string) error { return u.err }
func (u unavailableAPI) Permissions(context.Context, string) ([]permission, error) {
	return nil, u.err
}

// permissionsAPIVersion is the Microsoft.Authorization version of
// Permissions - List For Resource.
const permissionsAPIVersion = "2022-04-01"

func (a *sdkAPI) NodeResourceGroup(ctx context.Context) (string, error) {
	out, err := a.clusters.Get(ctx, a.resourceGroup, a.cluster, nil)
	if err != nil {
		return "", err
	}
	if out.Properties == nil || out.Properties.NodeResourceGroup == nil {
		return "", errors.New("AKS cluster has no node resource group")
	}
	return *out.Properties.NodeResourceGroup, nil
}

func (a *sdkAPI) Pool(ctx context.Context, name string) (pool, error) {
	out, err := a.pools.Get(ctx, a.resourceGroup, a.cluster, name, nil)
	if err != nil {
		return pool{}, err
	}
	props := out.Properties
	if props == nil || props.Count == nil {
		return pool{}, fmt.Errorf("agent pool %q has no node count", name)
	}
	p := pool{Count: *props.Count, raw: out.AgentPool, Tags: map[string]string{}}
	for _, zone := range props.AvailabilityZones {
		if zone != nil {
			p.Zones = append(p.Zones, *zone)
		}
	}
	p.Autoscaling = props.EnableAutoScaling != nil && *props.EnableAutoScaling
	if props.ProvisioningState != nil {
		p.ProvisioningState = *props.ProvisioningState
	}
	if props.ETag != nil {
		p.ETag = *props.ETag
	}
	for k, v := range props.Tags {
		if v != nil {
			p.Tags[k] = *v
		}
	}
	return p, nil
}

func (a *sdkAPI) Machines(ctx context.Context, poolName string) ([]machine, error) {
	var all []machine
	pager := a.machines.NewListPager(a.resourceGroup, a.cluster, poolName, nil)
	for pager.More() {
		page, err := pager.NextPage(ctx)
		if err != nil {
			return nil, err
		}
		for _, m := range page.Value {
			if m == nil || m.Name == nil || m.Properties == nil || m.Properties.ResourceID == nil {
				return nil, fmt.Errorf("agent pool %q lists a machine without a name or instance", poolName)
			}
			all = append(all, machine{Name: *m.Name, ResourceID: *m.Properties.ResourceID})
		}
	}
	return all, nil
}

func (a *sdkAPI) WritePool(ctx context.Context, name string, p pool, count int32, tags map[string]string) error {
	if p.raw.Properties == nil || p.ETag == "" {
		return fmt.Errorf("agent pool %q has no version to write against", name)
	}
	body := p.raw
	props := *body.Properties
	props.Count = &count
	props.Tags = map[string]*string{}
	for k, v := range tags {
		props.Tags[k] = &v
	}
	body.Properties = &props
	poller, err := a.pools.BeginCreateOrUpdate(ctx, a.resourceGroup, a.cluster, name, body, &aks.AgentPoolsClientBeginCreateOrUpdateOptions{IfMatch: &p.ETag})
	if err != nil {
		return refused(err)
	}
	_, err = poller.PollUntilDone(ctx, &runtime.PollUntilDoneOptions{Frequency: a.pollFrequency})
	return err
}

func (a *sdkAPI) DeleteMachines(ctx context.Context, poolName string, names []string) error {
	params := aks.AgentPoolDeleteMachinesParameter{}
	for _, n := range names {
		params.MachineNames = append(params.MachineNames, &n)
	}
	poller, err := a.pools.BeginDeleteMachines(ctx, a.resourceGroup, a.cluster, poolName, params, nil)
	if err != nil {
		return refused(err)
	}
	_, err = poller.PollUntilDone(ctx, &runtime.PollUntilDoneOptions{Frequency: a.pollFrequency})
	return err
}

func (a *sdkAPI) Permissions(ctx context.Context, scope string) ([]permission, error) {
	url := a.endpoint + strings.TrimSuffix(scope, "/") + "/providers/Microsoft.Authorization/permissions"
	var all []permission
	for first := true; url != ""; first = false {
		req, err := runtime.NewRequest(ctx, http.MethodGet, url)
		if err != nil {
			return nil, err
		}
		if first {
			q := req.Raw().URL.Query()
			q.Set("api-version", permissionsAPIVersion)
			req.Raw().URL.RawQuery = q.Encode()
		}
		req.Raw().Header.Set("Accept", "application/json")
		resp, err := a.pipeline.Do(req)
		if err != nil {
			return nil, err
		}
		if !runtime.HasStatusCode(resp, http.StatusOK) {
			return nil, runtime.NewResponseError(resp)
		}
		var page struct {
			Value    []permission `json:"value"`
			NextLink string       `json:"nextLink"`
		}
		if err := runtime.UnmarshalAsJSON(resp, &page); err != nil {
			return nil, err
		}
		all = append(all, page.Value...)
		// The next page goes out with the bearer token, so follow it only on
		// the Resource Manager endpoint the first request went to.
		if page.NextLink != "" && !strings.HasPrefix(page.NextLink, a.endpoint+"/") {
			return nil, fmt.Errorf("permissions list points its next page off %s", a.endpoint)
		}
		url = page.NextLink
	}
	return all, nil
}

// errorCodes are the ARM error code of an Azure response error and of any inner
// error its body carries; AKS wraps the cause of a failed scale (a quota, an
// allocation failure) in a generic outer code.
func errorCodes(err error) []string {
	var re *azcore.ResponseError
	if !errors.As(err, &re) {
		return nil
	}
	codes := []string{re.ErrorCode}
	if re.RawResponse == nil || re.RawResponse.Body == nil {
		return codes
	}
	body, rerr := runtime.Payload(re.RawResponse)
	if rerr != nil {
		return codes
	}
	var doc struct {
		Error armError `json:"error"`
	}
	if json.Unmarshal(body, &doc) == nil {
		codes = append(codes, doc.Error.codes()...)
	}
	return codes
}

type armError struct {
	Code    string     `json:"code"`
	Details []armError `json:"details"`
}

func (e armError) codes() []string {
	out := []string{e.Code}
	for _, d := range e.Details {
		out = append(out, d.codes()...)
	}
	return out
}

// refused marks an initial ARM request that was answered with a rejection:
// the request was not accepted, so nothing it asked for will happen.
func refused(err error) error {
	var response *azcore.ResponseError
	if errors.As(err, &response) {
		switch response.StatusCode {
		case http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden, http.StatusPreconditionFailed, http.StatusTooManyRequests:
			return fmt.Errorf("%w: %w", removefirst.ErrRefused, err)
		}
	}
	return err
}
