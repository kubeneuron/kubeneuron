// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/kubeneuron/kubeneuron/internal/metrics"
	"github.com/kubeneuron/kubeneuron/internal/playbook"
	"github.com/kubeneuron/kubeneuron/pkg/types"
)

// cloudHealthPlatform serves Node health and scripted points from the
// cloud's monitoring for one TPU node.
type cloudHealthPlatform struct {
	healthPlatform
	cmu      sync.Mutex
	enabled  bool
	points   []types.CloudHealthPoint
	cloudErr string
	calls    int
}

func (p *cloudHealthPlatform) CloudAcceleratorHealthEnabled() bool { return p.enabled }

func (p *cloudHealthPlatform) CloudAcceleratorHealth(context.Context) ([]types.NodeHealthSnapshot, error) {
	p.cmu.Lock()
	defer p.cmu.Unlock()
	p.calls++
	snap := tpuHealthNode(p.clock)
	snap.ReadAt = p.clock.Now()
	snap.Cloud = append([]types.CloudHealthPoint(nil), p.points...)
	if p.cloudErr != "" {
		snap.Cloud, snap.CloudError = nil, p.cloudErr
		return []types.NodeHealthSnapshot{snap}, errors.New(p.cloudErr + ": scripted")
	}
	return []types.NodeHealthSnapshot{snap}, nil
}

// sample appends a point ago before now.
func (p *cloudHealthPlatform) sample(ago time.Duration, status, category string) {
	p.cmu.Lock()
	defer p.cmu.Unlock()
	p.points = append(p.points, types.CloudHealthPoint{At: p.clock.Now().Add(-ago), Status: status, Category: category})
}

func (p *cloudHealthPlatform) callCount() int {
	p.cmu.Lock()
	defer p.cmu.Unlock()
	return p.calls
}

func tpuHealthNode(clock *fakeClock) types.NodeHealthSnapshot {
	return types.NodeHealthSnapshot{Node: "tpu-1", UID: "uid-tpu-1", CreatedAt: clock.Now().Add(-24 * time.Hour),
		Devices: []types.DeviceCount{{Resource: "google.com/tpu", Capacity: 4, Allocatable: 4}}}
}

func cloudHealthFixture(t *testing.T) (*Controller, *cloudHealthPlatform, *fakeClock) {
	t.Helper()
	c, _, _, clock := healthFixture(t)
	plat := &cloudHealthPlatform{healthPlatform: healthPlatform{clock: clock}, enabled: true}
	plat.set(tpuHealthNode(clock))
	c.platform = plat
	return c, plat, clock
}

func openTPUIncident(t *testing.T, c *Controller) *types.Incident {
	t.Helper()
	inc, err := c.store.GetOpenIncident(context.Background(), types.Target{Node: "tpu-1"}, types.ClassAcceleratorFault)
	if err != nil {
		return nil
	}
	return inc
}

// Two samples of a bad chip open one incident with the cloud's evidence; one
// sample read on many passes opens nothing.
func TestCloudHealthBadChipOpensAnIncident(t *testing.T) {
	c, plat, clock := cloudHealthFixture(t)
	ctx := context.Background()
	plat.sample(3*time.Minute, "UNHEALTHY", "BAD_TPU_CHIP")
	for range 12 {
		c.pollNodeHealth(ctx)
		clock.Advance(10 * time.Second)
	}
	if inc := openTPUIncident(t, c); inc != nil {
		t.Fatalf("one sample opened an incident: %+v", inc)
	}
	plat.sample(2*time.Minute, "UNHEALTHY", "BAD_TPU_CHIP")
	c.pollNodeHealth(ctx)
	inc := openTPUIncident(t, c)
	if inc == nil {
		t.Fatal("two samples of a bad chip opened no incident")
	}
	trail, err := c.store.AuditTrail(ctx, inc.ID)
	if err != nil || len(trail) == 0 {
		t.Fatalf("trail %v, %v", trail, err)
	}
	got := trail[0].Params
	for k, v := range map[string]string{"health_source": "cloud-monitoring", "profile": "google-tpu", "rule": "gcp-infra-health",
		"signal": "gcp-infra-health=UNHEALTHY/BAD_TPU_CHIP", "reason": "BAD_TPU_CHIP"} {
		if got[k] != v {
			t.Errorf("evidence %s = %q, want %q (all: %v)", k, got[k], v, got)
		}
	}
}

// The cloud is asked at most once a minute; between reads the last answer
// stands.
func TestCloudHealthIsReadOnceAMinute(t *testing.T) {
	c, plat, clock := cloudHealthFixture(t)
	ctx := context.Background()
	plat.sample(time.Minute, "HEALTHY", "")
	for range 6 {
		c.pollNodeHealth(ctx)
		clock.Advance(10 * time.Second)
	}
	if n := plat.callCount(); n != 1 {
		t.Fatalf("%d reads in one minute, want 1", n)
	}
	c.pollNodeHealth(ctx)
	if n := plat.callCount(); n != 2 {
		t.Fatalf("%d reads after a minute, want 2", n)
	}
}

// Turned off, nothing is asked of the cloud and the source does not hold
// node steps.
func TestCloudHealthOffAsksNothing(t *testing.T) {
	c, plat, _ := cloudHealthFixture(t)
	plat.enabled = false
	c.pollNodeHealth(context.Background())
	if n := plat.callCount(); n != 0 {
		t.Fatalf("%d reads while turned off", n)
	}
	if got := c.healthRecoveryRefusal("tpu-1", "platform.cordon"); got != "" {
		t.Fatalf("refused while the source is off: %q", got)
	}
}

// A stale or missing answer neither confirms a pending fault nor clears it;
// a fresh HEALTHY sample clears it.
func TestCloudHealthOnlyHealthyClearsAPendingFault(t *testing.T) {
	c, plat, clock := cloudHealthFixture(t)
	ctx := context.Background()
	pending := func() bool {
		c.health.mu.Lock()
		defer c.health.mu.Unlock()
		for k := range c.health.pending {
			if strings.HasPrefix(k, "cloud-monitoring/tpu-1/") {
				return true
			}
		}
		return false
	}
	plat.sample(3*time.Minute, "UNHEALTHY", "BAD_TPU_CHIP")
	plat.sample(2*time.Minute, "UNHEALTHY", "BAD_TPU_CHIP")
	c.pollNodeHealth(ctx)
	if !pending() {
		t.Fatal("no pending fault after two bad samples")
	}
	// Ten minutes on, the newest sample is stale: unknown holds it.
	clock.Advance(9 * time.Minute)
	c.pollNodeHealth(ctx)
	if !pending() {
		t.Fatal("a stale answer cleared the pending fault")
	}
	plat.cmu.Lock()
	plat.cloudErr = "api-error"
	plat.cmu.Unlock()
	clock.Advance(time.Minute)
	c.pollNodeHealth(ctx)
	if !pending() {
		t.Fatal("a failed read cleared the pending fault")
	}
	plat.cmu.Lock()
	plat.cloudErr = ""
	plat.cmu.Unlock()
	plat.sample(time.Minute, "HEALTHY", "")
	clock.Advance(5 * time.Minute)
	c.pollNodeHealth(ctx)
	if pending() {
		t.Fatal("a fresh HEALTHY sample did not clear the pending fault")
	}
}

// Without permission the TPU nodes are unknown, nothing opens, and the
// source counts as read: a lasting 403 does not hold every node step.
func TestCloudHealthPermissionDeniedIsUnknown(t *testing.T) {
	c, plat, clock := cloudHealthFixture(t)
	ctx := context.Background()
	plat.sample(3*time.Minute, "UNHEALTHY", "BAD_TPU_CHIP")
	plat.sample(2*time.Minute, "UNHEALTHY", "BAD_TPU_CHIP")
	plat.cloudErr = "permission-denied"
	before := testutil.ToFloat64(metrics.CloudHealthReads.WithLabelValues("permission-denied"))
	for range 3 {
		c.pollNodeHealth(ctx)
		clock.Advance(5 * time.Minute)
	}
	if inc := openTPUIncident(t, c); inc != nil {
		t.Fatalf("a failed read opened an incident: %+v", inc)
	}
	if got := testutil.ToFloat64(metrics.AcceleratorHealthNodes.WithLabelValues("cloud-monitoring", "google-tpu", "unknown")); got != 1 {
		t.Errorf("unknown TPU nodes %v, want 1", got)
	}
	if got := testutil.ToFloat64(metrics.CloudHealthReads.WithLabelValues("permission-denied")) - before; got < 2 {
		t.Errorf("permission-denied reads %v, want each attempt counted", got)
	}
	if got := c.healthRecoveryRefusal("tpu-1", "platform.cordon"); got != "" {
		t.Fatalf("a failed cloud read holds node steps: %q", got)
	}
}

// A degraded or networking-issue node is counted as ignored and opens
// nothing.
func TestCloudHealthIgnoredIsCounted(t *testing.T) {
	for _, tc := range []struct{ status, category string }{{"DEGRADED", ""}, {"UNHEALTHY", "NETWORKING_ISSUE"}} {
		c, plat, clock := cloudHealthFixture(t)
		ctx := context.Background()
		plat.sample(3*time.Minute, tc.status, tc.category)
		plat.sample(2*time.Minute, tc.status, tc.category)
		c.pollNodeHealth(ctx)
		clock.Advance(time.Minute)
		c.pollNodeHealth(ctx)
		if inc := openTPUIncident(t, c); inc != nil {
			t.Fatalf("%s %s opened an incident: %+v", tc.status, tc.category, inc)
		}
		if got := testutil.ToFloat64(metrics.AcceleratorHealthNodes.WithLabelValues("cloud-monitoring", "google-tpu", "ignored")); got != 1 {
			t.Errorf("%s %s: ignored TPU nodes %v, want 1", tc.status, tc.category, got)
		}
	}
}

// A new leader acts on nothing the cloud said before it: node steps are
// held until its own read, which asks the cloud again even within the
// minute.
func TestCloudHealthNewLeaderReadsAgain(t *testing.T) {
	c, plat, clock := cloudHealthFixture(t)
	ctx := context.Background()
	plat.sample(time.Minute, "HEALTHY", "")
	c.pollNodeHealth(ctx)
	if got := c.healthRecoveryRefusal("tpu-1", "platform.cordon"); got != "" {
		t.Fatalf("refused after both sources were read: %q", got)
	}
	c.health.reset()
	step := &playbook.Step{Name: "s", Action: "platform.cordon"}
	err := c.requireCloudRecovery(ctx, &types.Incident{ID: "i", Target: types.Target{Node: "tpu-1"}}, step)
	if err == nil || !strings.Contains(err.Error(), "has not been read since this controller became leader") {
		t.Fatalf("a new term acts before its own read: %v", err)
	}
	clock.Advance(10 * time.Second)
	c.pollNodeHealth(ctx)
	if n := plat.callCount(); n != 2 {
		t.Fatalf("%d reads: the new term reused the last term's answer", n)
	}
	if err := c.requireCloudRecovery(ctx, &types.Incident{ID: "i", Target: types.Target{Node: "tpu-1"}}, step); err != nil {
		t.Fatalf("still refused after the new term's read: %v", err)
	}
}
