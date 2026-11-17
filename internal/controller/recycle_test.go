// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/kubeneuron/kubeneuron/internal/playbook"
	"github.com/kubeneuron/kubeneuron/internal/safety"
	"github.com/kubeneuron/kubeneuron/internal/store/sqlite"
	"github.com/kubeneuron/kubeneuron/pkg/cloud"
	"github.com/kubeneuron/kubeneuron/pkg/types"
)

// recyclePlatform is a platform that can recycle/replace, and records the calls.
type recyclePlatform struct {
	stackPlatform
	configured         bool
	nodeReady          bool
	recycled, replaced string
	replacedBoot       string
	checkErr           error
}

func (p *recyclePlatform) CloudRecyclingConfigured() bool                  { return p.configured }
func (p *recyclePlatform) NodeReady(context.Context, string) (bool, error) { return p.nodeReady, nil }
func (p *recyclePlatform) CheckRecycleNode(context.Context, string) error  { return p.checkErr }
func (p *recyclePlatform) RecycleNode(_ context.Context, node string) error {
	p.recycled = node
	return nil
}
func (p *recyclePlatform) ReplaceNode(_ context.Context, node string) error {
	p.replaced = node
	return nil
}

func (p *recyclePlatform) CloudNode(context.Context, string) (cloud.NodeRef, error) {
	return cloud.NodeRef{Identity: *recycleTestPin()}, nil
}
func (p *recyclePlatform) RecoveryEligibility(_ context.Context, pin cloud.NodeIdentity) (cloud.RecoveryEligibility, error) {
	return cloud.RecoveryEligibility{Identity: pin, Mode: cloud.RepairOff, ReplaceAllowed: p.checkErr == nil, RecycleAllowed: p.checkErr == nil}, p.checkErr
}
func (p *recyclePlatform) RecyclePinnedNode(_ context.Context, pin cloud.NodeIdentity) error {
	p.recycled = pin.InstanceID
	return nil
}
func (p *recyclePlatform) RecyclePinnedNodeVerified(ctx context.Context, pin cloud.NodeIdentity) (cloud.RecycleResult, error) {
	p.recycled = pin.InstanceID
	if !p.nodeReady {
		<-ctx.Done()
		return cloud.RecycleResult{}, ctx.Err()
	}
	after := pin
	after.BootID = "boot-after-recycle"
	return cloud.RecycleResult{Before: pin, After: after, CompletedAt: time.Now()}, nil
}
func (p *recyclePlatform) ReplacePinnedNode(_ context.Context, pin cloud.NodeIdentity) error {
	p.replaced = pin.InstanceID
	p.replacedBoot = pin.BootID
	return nil
}
func recycleTestPin() *cloud.NodeIdentity {
	return &cloud.NodeIdentity{Node: "node-a", NodeUID: "uid-a", ProviderID: "aws:///zone/i-old", InstanceID: "i-old", BootID: "boot-a"}
}

func recycleController(t *testing.T, p *recyclePlatform) *Controller {
	t.Helper()
	return New(nil, nil, nil, nil, nil, p, nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func TestRecycleNodeStepCallsThePlatform(t *testing.T) {
	p := &recyclePlatform{configured: true, nodeReady: true}
	c := recycleController(t, p)
	st, err := sqlite.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	c.store = st
	inc := &types.Incident{ID: "i", Target: types.Target{Node: "node-a"}, CloudIdentity: recycleTestPin(), State: types.StateExecuting, OpenedAt: time.Now(), StateChangedAt: time.Now()}

	if err := st.CreateIncident(context.Background(), inc); err != nil {
		t.Fatal(err)
	}
	if _, err := c.recycleNodeStep(context.Background(), inc, false); err != nil {
		t.Fatal(err)
	}
	if p.recycled != "i-old" {
		t.Fatalf("recycled %q, want pinned i-old", p.recycled)
	}

	saved, err := st.GetIncident(context.Background(), inc.ID)
	if err != nil {
		t.Fatal(err)
	}
	if saved.CloudIdentity.BootID != "boot-after-recycle" {
		t.Fatal("new boot not persisted")
	}
	inc = saved
	if _, err := c.recycleNodeStep(context.Background(), inc, true); err != nil {
		t.Fatal(err)
	}
	if p.replaced != "i-old" || p.replacedBoot != "boot-after-recycle" {
		t.Fatalf("replaced %q, want pinned i-old", p.replaced)
	}
}

// A GPU node whose reset was refused (no PCI reset) must not be reported
// recycled when no cloud provider can actually restart it.
func TestRecycleNodeFailsClosedWithoutCloudProvider(t *testing.T) {
	c := recycleController(t, &recyclePlatform{configured: false})
	inc := &types.Incident{ID: "i", Target: types.Target{Node: "node-a"}, CloudIdentity: recycleTestPin()}
	if _, err := c.recycleNodeStep(context.Background(), inc, false); err == nil {
		t.Fatal("recycle must fail closed when no cloud provider is configured")
	}
}

// A recycle is not done when the instance is merely powered on: the node must
// rejoin. Otherwise the next step (verify) fires on a stale heartbeat, which is
// exactly what failed on a live managed node group. With a node that never
// returns Ready, the step must fail rather than report success.
func TestRecycleWaitsForNodeToRejoin(t *testing.T) {
	p := &recyclePlatform{configured: true, nodeReady: false}
	c := recycleController(t, p)
	inc := &types.Incident{ID: "i", Target: types.Target{Node: "node-a"}, CloudIdentity: recycleTestPin()}
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // deadline already passed: the node never becomes Ready
	if _, err := c.recycleNodeStep(ctx, inc, false); err == nil {
		t.Fatal("recycle must not report success while the node has not rejoined")
	}
	if p.recycled != "i-old" {
		t.Fatal("the instance should still have been recycled before the wait")
	}
}

// ReplaceNode does not wait for the old node: it is terminated and the
// vanished-node reconciler closes the incident as replaced.
func TestReplaceDoesNotWaitForNode(t *testing.T) {
	p := &recyclePlatform{configured: true, nodeReady: false}
	c := recycleController(t, p)
	inc := &types.Incident{ID: "i", Target: types.Target{Node: "node-a"}, CloudIdentity: recycleTestPin()}
	if _, err := c.recycleNodeStep(context.Background(), inc, true); err != nil {
		t.Fatalf("replace must not block on node readiness: %v", err)
	}
}

// N3: the provider capability admits RecycleNode at compile time, but
// viability is per-instance — an autoscaling-group member is terminated by
// its group the moment it stops. A definitive non-viability verdict must
// escalate the ladder at admission, BEFORE a human is asked to approve a
// recycle that cannot work; anything less rediscovers the gap by timeout.
func TestUnrecyclableNodeEscalatesBeforeApprovalIsRequested(t *testing.T) {
	book := &playbook.Playbook{
		Name: "recycle", Target: "node",
		Steps: []playbook.Step{{Name: "recycle", Action: "platform.recycle_node", Approval: "required"}},
	}
	// The policy enables the restart through the cloud API (off by
	// default).
	engine, err := playbook.NewEngine(map[string]*playbook.Playbook{"recycle": book}, []playbook.Policy{
		{Class: types.ClassFellOffBus, Playbook: "recycle", RecycleEnabled: true},
	})
	if err != nil {
		t.Fatal(err)
	}

	run := func(t *testing.T, checkErr error) (*types.Incident, *recordingNotifier) {
		t.Helper()
		st, err := sqlite.Open(":memory:")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = st.Close() })
		p := &recyclePlatform{configured: true, checkErr: checkErr}
		notifier := &recordingNotifier{}
		c := New(st, st, engine, safety.NewGate(safety.Limits{MaxConcurrentRemediations: 4, MaxConcurrentReboots: 1}),
			nil, p, nil, notifier, slog.New(slog.NewTextHandler(io.Discard, nil)))
		ctx := context.Background()
		inc := &types.Incident{
			ID: "inc-recycle", Target: types.Target{Node: "node-a"}, CloudIdentity: recycleTestPin(),
			Class: types.ClassFellOffBus, State: types.StateEvaluating,
			Playbook: "recycle", OpenedAt: time.Now(), UpdatedAt: time.Now(), StateChangedAt: time.Now(),
		}
		if err := st.CreateIncident(ctx, inc); err != nil {
			t.Fatal(err)
		}
		if err := c.advanceEvaluating(ctx, inc); err != nil {
			t.Fatal(err)
		}
		got, err := st.GetIncident(ctx, inc.ID)
		if err != nil {
			t.Fatal(err)
		}
		return got, notifier
	}

	t.Run("definitive non-viability escalates", func(t *testing.T) {
		inc, notifier := run(t, fmt.Errorf("aws: instance i-1 belongs to autoscaling group %q: %w", "gpu-mng", cloud.ErrRecycleNotViable))
		if inc.State != types.StateNeedsHuman {
			t.Fatalf("state = %s, want NEEDS_HUMAN (escalate with no ladder quarantines), never AWAITING_APPROVAL", inc.State)
		}
		if len(notifier.approvals) != 0 {
			t.Fatal("no approval may be requested for a recycle that provably cannot work")
		}
	})
	t.Run("viable instance parks for approval", func(t *testing.T) {
		inc, notifier := run(t, nil)
		if inc.State != types.StateAwaitingApproval {
			t.Fatalf("state = %s, want AWAITING_APPROVAL", inc.State)
		}
		if len(notifier.approvals) != 1 {
			t.Fatalf("approvals requested = %d, want 1", len(notifier.approvals))
		}
	})
	t.Run("transient lookup failure holds without approval", func(t *testing.T) {
		inc, _ := run(t, errors.New("aws: describing i-1: throttled"))
		if inc.State != types.StateEvaluating {
			t.Fatalf("state = %s, want EVALUATING while provider eligibility is unknown", inc.State)
		}
	})
}

func TestCloudMutationRejectsMissingOrForeignPin(t *testing.T) {
	for _, replace := range []bool{false, true} {
		for _, pin := range []*cloud.NodeIdentity{nil, {}, {Node: "other", NodeUID: "u", ProviderID: "p", InstanceID: "i", BootID: "b"}} {
			p := &recyclePlatform{configured: true, nodeReady: true}
			c := recycleController(t, p)
			inc := &types.Incident{Target: types.Target{Node: "node-a"}, CloudIdentity: pin}
			if _, err := c.recycleNodeStep(context.Background(), inc, replace); err == nil {
				t.Fatal("invalid pin admitted")
			}
			if p.recycled != "" || p.replaced != "" {
				t.Fatal("invalid pin reached cloud mutation")
			}
		}
	}
}
