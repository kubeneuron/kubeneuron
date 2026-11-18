// SPDX-License-Identifier: Apache-2.0

package gcp

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
)

const (
	testProject  = "ml-project"
	testLocation = "us-central1"
	testCluster  = "training"
	testPool     = "gpu"
	testZone     = "us-central1-a"
)

func testName(zone string, i int) string {
	return fmt.Sprintf("gke-training-gpu-%s-%04d", zone[len(zone)-1:], i)
}

func testInstance(i int) string { return testProject + "/" + testZone + "/" + testName(testZone, i) }

// fakeGroup is one zone of the pool.
type fakeGroup struct {
	ref     groupRef
	target  int32
	members []member
	stable  bool
	next    int
}

func (g *fakeGroup) grow(to int32, errs []string) {
	for int32(len(g.members)) < to {
		m := member{Name: testName(g.ref.zone, g.next), Action: "NONE", Status: "RUNNING"}
		if len(errs) > 0 {
			m = member{Name: m.Name, Action: "CREATING", Errors: errs}
		}
		g.members = append(g.members, m)
		g.next++
	}
}

// fakeGKE is one node pool of one cluster, with a managed instance group per
// zone. A resize sets every zone's size and fills it; a delete removes the
// named members and shrinks their group.
type fakeGKE struct {
	autopilot, autoscaling, autoRepair bool
	groups                             []*fakeGroup
	ops                                []operation
	labels                             map[string]string
	maintenance                        string
	permissions                        []string

	setSizeErr   error
	createErrors []string
	deleteErr    error
	// hold leaves the next resize running, with no new member, until
	// Deliver; lose applies the next resize and fails its answer.
	hold, held, lose bool
	requests         int
	deleted          []string
	// now is GKE's clock: the start time of new operations.
	now time.Time

	calls []string
}

func newFakeGKE(n int, zones ...string) *fakeGKE {
	if len(zones) == 0 {
		zones = []string{testZone}
	}
	f := &fakeGKE{now: time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC), autoRepair: true, labels: map[string]string{labelCluster: testCluster, labelNodePool: testPool}, permissions: fixedPoolPermissions}
	for _, z := range zones {
		g := &fakeGroup{ref: groupRef{project: testProject, zone: z, name: "gke-training-gpu-" + z[len(z)-1:] + "-grp"}, target: int32(n), stable: true}
		g.grow(int32(n), nil)
		f.groups = append(f.groups, g)
	}
	return f
}

func (f *fakeGKE) group(ref groupRef) (*fakeGroup, error) {
	for _, g := range f.groups {
		if g.ref == ref {
			return g, nil
		}
	}
	return nil, fmt.Errorf("no instance group %v", ref)
}

func (f *fakeGKE) Cluster(context.Context) (cluster, error) {
	pl := nodePool{Name: testPool, Autoscaling: f.autoscaling, AutoRepair: f.autoRepair}
	for _, g := range f.groups {
		pl.Groups = append(pl.Groups, g.ref)
	}
	return cluster{Autopilot: f.autopilot, Pools: []nodePool{{Name: "system"}, pl}}, nil
}

func (f *fakeGKE) Instance(_ context.Context, in instance) (vm, error) {
	if slices.Contains(f.deleted, in.name) {
		return vm{}, errGone
	}
	return vm{Labels: f.labels, Maintenance: f.maintenance}, nil
}

func (f *fakeGKE) Group(_ context.Context, ref groupRef) (group, error) {
	g, err := f.group(ref)
	if err != nil {
		return group{}, err
	}
	return group{TargetSize: g.target, Stable: g.stable}, nil
}

func (f *fakeGKE) Members(_ context.Context, ref groupRef) ([]member, error) {
	g, err := f.group(ref)
	if err != nil {
		return nil, err
	}
	return slices.Clone(g.members), nil
}

func (f *fakeGKE) Operations(context.Context) ([]operation, error) { return slices.Clone(f.ops), nil }

func (f *fakeGKE) Operation(_ context.Context, name string) (operation, error) {
	for _, op := range f.ops {
		if op.Name == name {
			return op, nil
		}
	}
	return operation{}, errors.New("no such operation")
}

// poolLink is a node pool's target link as GKE writes it, with the project
// number and the zones path.
func poolLink(pool string) string {
	return "https://container.googleapis.com/v1/projects/123456/zones/" + testLocation + "/clusters/" + testCluster + "/nodePools/" + pool
}

func (f *fakeGKE) addOp(typ, status, target string) *operation {
	f.ops = append(f.ops, operation{Name: fmt.Sprintf("operation-%d", len(f.ops)+1), Type: typ, Status: status, Target: target, Start: f.now})
	return &f.ops[len(f.ops)-1]
}

func (f *fakeGKE) SetPoolSize(_ context.Context, pool string, size int32) (string, error) {
	f.calls = append(f.calls, fmt.Sprintf("setSize %s %d", pool, size))
	if pool != testPool {
		return "", errors.New("no such pool")
	}
	if f.setSizeErr != nil {
		return "", f.setSizeErr
	}
	if size > f.groups[0].target {
		f.requests++
	}
	op := f.addOp(opSetSize, "RUNNING", poolLink(pool))
	name := op.Name
	for _, g := range f.groups {
		g.target = size
	}
	if f.hold {
		f.hold, f.held = false, true
	} else {
		f.fill()
		op.Status = opDone
	}
	if f.lose {
		f.lose = false
		return "", errors.New("context deadline exceeded")
	}
	return name, nil
}

func (f *fakeGKE) fill() {
	for _, g := range f.groups {
		g.grow(g.target, f.createErrors)
	}
}

// The cloudtest.SuccessorCloud hooks.
func (f *fakeGKE) HoldNextRequest() { f.hold = true }
func (f *fakeGKE) LoseNextAnswer()  { f.lose = true }
func (f *fakeGKE) Requests() int    { return f.requests }
func (f *fakeGKE) Size() int        { return int(f.groups[0].target) }
func (f *fakeGKE) Deliver() {
	if f.held {
		f.held = false
		f.fill()
		for i := range f.ops {
			f.ops[i].Status = opDone
		}
	}
}

func (f *fakeGKE) DeleteInstances(_ context.Context, ref groupRef, names []string, requestID string) error {
	f.calls = append(f.calls, "delete "+ref.zone+" "+strings.Join(names, ","))
	if requestID == "" {
		return errors.New("no request ID")
	}
	if f.deleteErr != nil {
		return f.deleteErr
	}
	g, err := f.group(ref)
	if err != nil {
		return err
	}
	for _, n := range names {
		i := slices.IndexFunc(g.members, func(m member) bool { return m.Name == n })
		if i < 0 {
			return fmt.Errorf("instance %s is not a member", n)
		}
		g.members = slices.Delete(g.members, i, i+1)
		g.target--
		f.deleted = append(f.deleted, n)
	}
	return nil
}

func (f *fakeGKE) Permissions(_ context.Context, want []string) ([]string, error) {
	var held []string
	for _, p := range want {
		if slices.Contains(f.permissions, p) {
			held = append(held, p)
		}
	}
	return held, nil
}

func testProvider(f *fakeGKE) *Provider {
	return newWithAPI(settings{project: testProject, location: testLocation, cluster: testCluster}, f)
}
