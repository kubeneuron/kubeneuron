// SPDX-License-Identifier: Apache-2.0

package azure

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"strconv"
	"strings"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/runtime"
	aks "github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/containerservice/armcontainerservice/v9"
)

const (
	testSub     = "11111111-2222-3333-4444-555555555555"
	testRG      = "ml"
	testCluster = "training"
	testNodeRG  = "MC_ml_training_eastus"
	testPool    = "gpu"
	testVMSS    = "aks-gpu-12345678-vmss"
)

func testInstance(i int) string {
	return fmt.Sprintf("/subscriptions/%s/resourceGroups/mc_ml_training_eastus/providers/Microsoft.Compute/virtualMachineScaleSets/%s/virtualMachines/%d", testSub, testVMSS, i)
}

func testMachine(i int) machine {
	return machine{
		Name:       fmt.Sprintf("%s%06s", testVMSS, strconv.FormatInt(int64(i), 36)),
		ResourceID: strings.Replace(testInstance(i), "mc_ml_training_eastus", testNodeRG, 1),
	}
}

// fakeAKS is one agent pool of one cluster. A grown pool gets a new machine;
// a deleted machine shrinks it, unless shrinkOnDelete is off.
type fakeAKS struct {
	count       int32
	autoscaling bool
	state       string
	tags        map[string]string
	version     int
	machines    []machine
	next        int

	growErr        error
	growsMachine   bool
	// hold leaves the next growth scaling, with no machine, until Deliver;
	// lose applies the next growth and fails its answer.
	hold, held, lose bool
	requests         int
	deleteErr        error
	shrinkOnDelete bool
	permissions    []permission

	calls []string
}

func newFakeAKS(n int) *fakeAKS {
	f := &fakeAKS{count: int32(n), state: "Succeeded", tags: map[string]string{"team": "ml"}, growsMachine: true, shrinkOnDelete: true, next: n}
	for i := range n {
		f.machines = append(f.machines, testMachine(i))
	}
	return f
}

func (f *fakeAKS) NodeResourceGroup(context.Context) (string, error) { return testNodeRG, nil }

func (f *fakeAKS) Pool(_ context.Context, name string) (pool, error) {
	if name != testPool {
		return pool{}, errors.New("no such pool")
	}
	return pool{Count: f.count, Autoscaling: f.autoscaling, ProvisioningState: f.state, Tags: maps.Clone(f.tags),
		ETag: strconv.Itoa(f.version), raw: aks.AgentPool{Properties: &aks.ManagedClusterAgentPoolProfileProperties{}}}, nil
}

func (f *fakeAKS) Machines(_ context.Context, name string) ([]machine, error) {
	if name != testPool {
		return nil, errors.New("no such pool")
	}
	return append([]machine(nil), f.machines...), nil
}

func (f *fakeAKS) WritePool(_ context.Context, name string, p pool, count int32, tags map[string]string) error {
	f.calls = append(f.calls, fmt.Sprintf("write count=%d tag=%q", count, tags[replacingTag]))
	if p.ETag != strconv.Itoa(f.version) {
		return errors.New("412 precondition failed")
	}
	if tags["team"] != "ml" {
		return errors.New("write dropped a tag it did not own")
	}
	if count > f.count {
		f.requests++
	}
	if count > f.count && f.hold {
		f.hold, f.held = false, true
		f.state, f.count, f.tags = "Scaling", count, maps.Clone(tags)
		f.version++
		return f.answer()
	}
	if count > f.count && f.growErr != nil {
		f.state, f.count, f.tags = "Failed", count, maps.Clone(tags)
		f.version++
		return f.growErr
	}
	for ; f.count < count; f.count++ {
		if f.growsMachine {
			f.machines = append(f.machines, testMachine(f.next))
			f.next++
		}
	}
	f.count = count
	f.tags = maps.Clone(tags)
	f.version++
	return f.answer()
}

func (f *fakeAKS) answer() error {
	if f.lose {
		f.lose = false
		return errors.New("context deadline exceeded")
	}
	return nil
}

// The cloudtest.SuccessorCloud hooks.
func (f *fakeAKS) HoldNextRequest() { f.hold = true }
func (f *fakeAKS) LoseNextAnswer()  { f.lose = true }
func (f *fakeAKS) Requests() int    { return f.requests }
func (f *fakeAKS) Size() int        { return int(f.count) }
func (f *fakeAKS) Deliver() {
	if f.held {
		f.held = false
		f.machines = append(f.machines, testMachine(f.next))
		f.next++
		f.state = "Succeeded"
		f.version++
	}
}

func (f *fakeAKS) DeleteMachines(_ context.Context, name string, names []string) error {
	f.calls = append(f.calls, "delete "+strings.Join(names, ","))
	if f.deleteErr != nil {
		return f.deleteErr
	}
	for _, n := range names {
		for i, m := range f.machines {
			if m.Name == n {
				f.machines = append(f.machines[:i], f.machines[i+1:]...)
				if f.shrinkOnDelete {
					f.count--
				}
				break
			}
		}
	}
	f.version++
	return nil
}

func (f *fakeAKS) Permissions(_ context.Context, scope string) ([]permission, error) {
	if !strings.HasSuffix(scope, "/managedClusters/"+testCluster+"/agentPools/"+testPool) {
		return nil, fmt.Errorf("unexpected scope %s", scope)
	}
	return f.permissions, nil
}

// armFailure is an ARM error response with an outer and inner code.
func armFailure(outer, inner string) error {
	body := fmt.Sprintf(`{"error":{"code":%q,"message":"scale failed","details":[{"code":%q,"message":"cores quota"}]}}`, outer, inner)
	req, _ := http.NewRequest(http.MethodPut, "https://management.azure.com/x", nil)
	return runtime.NewResponseError(&http.Response{
		StatusCode: http.StatusBadRequest, Header: http.Header{"Content-Type": {"application/json"}},
		Body: io.NopCloser(bytes.NewBufferString(body)), Request: req,
	})
}

func testProvider(f *fakeAKS) *Provider {
	return newWithAPI(settings{subscription: testSub, resourceGroup: testRG, cluster: testCluster}, f)
}
