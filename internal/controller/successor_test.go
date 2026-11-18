// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kubeneuron/kubeneuron/internal/platform"
	"github.com/kubeneuron/kubeneuron/internal/playbook"
	"github.com/kubeneuron/kubeneuron/internal/recoverymode"
	"github.com/kubeneuron/kubeneuron/internal/safety"
	"github.com/kubeneuron/kubeneuron/internal/store/sqlite"
	"github.com/kubeneuron/kubeneuron/internal/topology"
	"github.com/kubeneuron/kubeneuron/pkg/cloud"
	"github.com/kubeneuron/kubeneuron/pkg/types"
)

const successorNodeName = "n9"

// successorPlatform is a cloud platform whose provider prepares successors in
// a simulated pool. Every cloud mutation is recorded.
type successorPlatform struct {
	autoCloudPlatform

	mu sync.Mutex
	// unavailable: CheckAddFirst's refusal; capacity / ambiguous: what the
	// request answers; createAfter: polls before the successor appears.
	unavailable          error
	capacity, ambiguous  bool
	createAfter          int
	polls                int
	requests             int
	nodeReady            bool
	successorAllocatable int64
	successorWorkload    bool
	releaseErr           error

	created    time.Time
	cordoned   map[string][]string
	drained    []string
	released   []cloud.Successor
	replacedBy []cloud.Successor
	plain      []string // ReplacePinnedNode: remove-first
}

func newSuccessorPlatform() *successorPlatform {
	return &successorPlatform{nodeReady: true, successorAllocatable: 8, cordoned: map[string][]string{}}
}

func (p *successorPlatform) CloudRecyclingConfigured() bool                 { return true }
func (p *successorPlatform) CheckRecycleNode(context.Context, string) error { return nil }
func (p *successorPlatform) RecycleNode(context.Context, string) error      { return nil }
func (p *successorPlatform) ReplaceNode(context.Context, string) error      { return nil }

func (p *successorPlatform) AddFirstAvailable(context.Context, cloud.NodeIdentity) error {
	return p.unavailable
}

func (p *successorPlatform) PrepareSuccessor(_ context.Context, pin cloud.NodeIdentity, s cloud.Successor) (cloud.Successor, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	switch s.Phase {
	case "":
		s.Phase, s.Pool = cloud.SuccessorPlanned, "train"
		s.State = map[string]string{"members": "4"}
	case cloud.SuccessorPlanned:
		p.requests++
		if p.capacity {
			return s, fmt.Errorf("pool train: %w", cloud.ErrReplacementCapacity)
		}
		s.Phase = cloud.SuccessorRequested
	case cloud.SuccessorRequested:
		if p.ambiguous {
			return s, fmt.Errorf("two new instances in pool train: %w", cloud.ErrNeedsOperator)
		}
		p.polls++
		if p.polls > p.createAfter {
			s.Phase, s.ProviderID = cloud.SuccessorCreated, "aws:///zone/i-"+successorNodeName
			p.created = time.Now()
		}
	}
	return s, nil
}

func (p *successorPlatform) SuccessorNode(_ context.Context, providerID string) (platform.SuccessorNode, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if providerID == "" || p.created.IsZero() {
		return platform.SuccessorNode{}, nil
	}
	return platform.SuccessorNode{Found: true, Name: successorNodeName, UID: "uid-" + successorNodeName,
		CreatedAt: p.created, Ready: p.nodeReady, Allocatable: map[string]int64{topology.ResourceWholeGPU: p.successorAllocatable}}, nil
}

func (p *successorPlatform) ReleaseSuccessor(_ context.Context, s cloud.Successor) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.releaseErr != nil {
		return p.releaseErr
	}
	p.released = append(p.released, s)
	return nil
}

func (p *successorPlatform) ReplacePinnedNodeWithSuccessor(_ context.Context, pin cloud.NodeIdentity, s cloud.Successor) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if s.Instance != pin.InstanceID {
		return errors.New("wrong instance")
	}
	p.replacedBy = append(p.replacedBy, s)
	return nil
}

func (p *successorPlatform) ReplacePinnedNode(_ context.Context, pin cloud.NodeIdentity) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.plain = append(p.plain, pin.InstanceID)
	return nil
}

func (p *successorPlatform) NodeResources(_ context.Context, node string) (topology.Resources, bool, error) {
	return topology.Resources{Capacity: map[string]int64{topology.ResourceWholeGPU: 8}, Allocatable: map[string]int64{topology.ResourceWholeGPU: 8}}, true, nil
}

func (p *successorPlatform) NodeExists(context.Context, string) (bool, error) { return true, nil }

func (p *successorPlatform) CordonForOwner(_ context.Context, node, owner, _ string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.cordoned[node] = append(p.cordoned[node], owner)
	return nil
}

func (p *successorPlatform) ReleaseCordonOwners(_ context.Context, node string, owners []string) (bool, int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	var kept []string
	for _, o := range p.cordoned[node] {
		drop := false
		for _, r := range owners {
			drop = drop || o == r
		}
		if !drop {
			kept = append(kept, o)
		}
	}
	p.cordoned[node] = kept
	return len(kept) == 0, len(kept), nil
}

func (p *successorPlatform) Drain(_ context.Context, node string, _ platform.DrainOptions) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.drained = append(p.drained, node)
	return nil
}

func (p *successorPlatform) NodeWorkloads(_ context.Context, node string) ([]platform.Workload, error) {
	out := []platform.Workload{{Name: "dcgm-exporter", Namespace: "gpu", DrainExclusion: platform.DrainExclusionInfrastructure}}
	if node == successorNodeName && p.successorWorkload {
		out = append(out, platform.Workload{Name: "trainer-0", Namespace: "ml"})
	}
	return out, nil
}

func (p *successorPlatform) cordonedNode(node string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.cordoned[node]) > 0
}

func (p *successorPlatform) counts() (requests, released, replaced, plain, drained int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.requests, len(p.released), len(p.replacedBy), len(p.plain), len(p.drained)
}

// successorFixture is a live installation (nothing simulated) whose policy
// adds first and whose playbook cordons, drains and replaces.
func successorFixture(t *testing.T, mode recoverymode.Mode, p *successorPlatform, strategy recoverymode.Strategy) (*Controller, *sqlite.Store, *recordingNotifier) {
	return successorFixtureTimeout(t, mode, p, strategy, 0)
}

func successorFixtureTimeout(t *testing.T, mode recoverymode.Mode, p *successorPlatform, strategy recoverymode.Strategy, timeout time.Duration) (*Controller, *sqlite.Store, *recordingNotifier) {
	t.Helper()
	st, err := sqlite.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ctx := context.Background()
	for i := 1; i <= 4; i++ {
		if err := st.UpsertNode(ctx, &types.Node{Name: nodeName(i), Labels: map[string]string{"pool": "train"}}); err != nil {
			t.Fatal(err)
		}
	}
	live := safety.Limits{MaxConcurrentRemediations: 8, MaxConcurrentReboots: 8}
	notifier := &recordingNotifier{}
	c := New(st, st, nil, safety.NewGate(live), safety.NewFlapDetector(100, time.Hour), p, nil, notifier,
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := c.InstallRuntimeConfig(RuntimeConfig{SafetyLimits: &live}); err != nil {
		t.Fatal(err)
	}
	book := &playbook.Playbook{Name: "cloud-replace", Target: "node", Steps: []playbook.Step{
		{Name: "cordon", Action: "platform.cordon"},
		{Name: "drain", Action: "platform.drain", Approval: "required"},
		{Name: "replace", Action: "platform.replace_node", Approval: "required"},
	}}
	pol := playbook.Policy{Name: autoPolicy, Class: types.ClassFellOffBus, Playbook: book.Name, Mode: mode,
		ReplacementStrategy: strategy, SuccessorTimeout: timeout}
	if mode == recoverymode.Automatic {
		pol.Automatic = autoSettings()
	}
	engine, err := playbook.NewEngine(map[string]*playbook.Playbook{book.Name: book}, []playbook.Policy{pol})
	if err != nil {
		t.Fatal(err)
	}
	c.SetEngine(engine)
	c.SetTimings(time.Millisecond, 0)
	c.successorPoll = time.Millisecond
	return c, st, notifier
}

// registerSuccessorAgent has the successor's agent report its accelerators.
func registerSuccessorAgent(t *testing.T, st *sqlite.Store, gpus int) {
	t.Helper()
	node := &types.Node{Name: successorNodeName, UID: "uid-" + successorNodeName, AgentLastSeen: time.Now().Add(time.Second)}
	for i := 0; i < gpus; i++ {
		node.GPUs = append(node.GPUs, types.GPUInfo{UUID: fmt.Sprintf("GPU-new-%d", i), Index: i})
	}
	if err := st.UpsertAgentRegistration(context.Background(), node); err != nil {
		t.Fatal(err)
	}
}

func waitSettled(t *testing.T, c *Controller, st *sqlite.Store, id string, done func(*types.Incident) bool) *types.Incident {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		c.reconcile(context.Background())
		inc := mustIncident(t, st, id)
		if done(inc) {
			return inc
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("condition not reached%s", dump(trail(t, st, id)))
	return nil
}

func auditHas(entries []*types.AuditEntry, action, text string) bool {
	for _, e := range entries {
		if e.Action == action && strings.Contains(e.Result, text) {
			return true
		}
	}
	return false
}

func TestAddFirstPreparesTheSuccessorBeforeTheCordon(t *testing.T) {
	p := newSuccessorPlatform()
	p.createAfter = 2
	c, st, _ := successorFixture(t, recoverymode.Automatic, p, "")
	registerSuccessorAgent(t, st, 8)
	id := openOn(t, c, "n1")
	inc := waitSettled(t, c, st, id, func(inc *types.Incident) bool { return inc.State.Halted() || inc.State == types.StateVerifying })
	entries := trail(t, st, id)
	if inc.State.Halted() && inc.State != types.StateResolved {
		t.Fatalf("state = %s%s", inc.State, dump(entries))
	}
	requests, released, replaced, plain, drained := p.counts()
	if requests != 1 || replaced != 1 || released != 0 || plain != 0 || drained != 1 {
		t.Fatalf("requests=%d replaced=%d released=%d plain=%d drained=%d, want one request, one replacement by the successor%s",
			requests, replaced, released, plain, drained, dump(entries))
	}
	s := inc.Successor
	if s == nil || s.Outcome != types.SuccessorReplaced || s.Node != successorNodeName || s.ReadyAt == nil || s.EndedAt == nil {
		t.Fatalf("successor = %+v", s)
	}
	// The successor was accepted before the node was cordoned.
	ready, cordon := -1, -1
	for i, e := range entries {
		if e.Action == "successor-ready" && ready < 0 {
			ready = i
		}
		if e.Action == "cordon" && strings.HasPrefix(e.Result, "cordoned") && cordon < 0 {
			cordon = i
		}
	}
	if ready < 0 || cordon < 0 || ready > cordon {
		t.Fatalf("successor-ready at %d, cordon at %d: the successor must be ready first%s", ready, cordon, dump(entries))
	}
}

func TestAddFirstNoCapacityLeavesTheNodeAlone(t *testing.T) {
	p := newSuccessorPlatform()
	p.capacity = true
	c, st, _ := successorFixture(t, recoverymode.Automatic, p, "")
	id := openOn(t, c, "n1")
	inc := waitSettled(t, c, st, id, func(inc *types.Incident) bool { return inc.State.Halted() })
	entries := trail(t, st, id)
	if p.cordonedNode("n1") {
		t.Fatalf("the node was cordoned without a successor%s", dump(entries))
	}
	if _, _, replaced, plain, drained := p.counts(); replaced+plain+drained != 0 {
		t.Fatalf("the node was drained or removed without a successor%s", dump(entries))
	}
	if !auditHas(entries, "cordon", "replacement capacity unavailable") {
		t.Fatalf("no step result names the missing capacity%s", dump(entries))
	}
	if s := inc.Successor; s == nil || s.Outcome != types.SuccessorCapacityDenied {
		t.Fatalf("successor = %+v, want capacity-denied", s)
	}
}

func TestAddFirstRejectsASuccessorWithoutItsAccelerators(t *testing.T) {
	for name, setup := range map[string]func(*successorPlatform, *sqlite.Store, *testing.T){
		"no agent inventory": func(_ *successorPlatform, _ *sqlite.Store, _ *testing.T) {},
		"inventory, no GPUs": func(_ *successorPlatform, st *sqlite.Store, t *testing.T) { registerSuccessorAgent(t, st, 0) },
		"fewer allocatable GPUs": func(p *successorPlatform, st *sqlite.Store, t *testing.T) {
			p.successorAllocatable = 4
			registerSuccessorAgent(t, st, 8)
		},
		"node not Ready": func(p *successorPlatform, st *sqlite.Store, t *testing.T) {
			p.nodeReady = false
			registerSuccessorAgent(t, st, 8)
		},
	} {
		t.Run(name, func(t *testing.T) {
			p := newSuccessorPlatform()
			c, st, _ := successorFixture(t, recoverymode.Automatic, p, "")
			setup(p, st, t)
			id := openOn(t, c, "n1")
			waitSettled(t, c, st, id, func(inc *types.Incident) bool {
				return inc.Successor != nil && inc.Successor.Node != "" && inc.Successor.Reason != ""
			})
			inc := mustIncident(t, st, id)
			if inc.Successor.ReadyAt != nil || p.cordonedNode("n1") {
				t.Fatalf("an unfit successor was accepted: %+v%s", inc.Successor, dump(trail(t, st, id)))
			}
		})
	}
}

func TestAddFirstTimeoutRemovesAnIdleSuccessor(t *testing.T) {
	for _, busy := range []bool{false, true} {
		t.Run(fmt.Sprintf("workload on successor=%t", busy), func(t *testing.T) {
			p := newSuccessorPlatform()
			p.nodeReady = false // never ready
			p.successorWorkload = busy
			c, st, _ := successorFixtureTimeout(t, recoverymode.Automatic, p, "", 50*time.Millisecond)
			id := openOn(t, c, "n1")
			inc := waitSettled(t, c, st, id, func(inc *types.Incident) bool { return inc.Successor != nil && !inc.Successor.Open() })
			entries := trail(t, st, id)
			_, released, replaced, plain, drained := p.counts()
			if replaced+plain+drained != 0 || p.cordonedNode("n1") {
				t.Fatalf("the node was disrupted after the timeout%s", dump(entries))
			}
			if busy {
				if released != 0 || inc.Successor.Outcome != types.SuccessorHandedOver || p.cordonedNode(successorNodeName) {
					t.Fatalf("a busy successor: released=%d outcome=%s cordoned=%t, want it kept, in service, for an operator",
						released, inc.Successor.Outcome, p.cordonedNode(successorNodeName))
				}
				return
			}
			if released != 1 || inc.Successor.Outcome != types.SuccessorReleased {
				t.Fatalf("released=%d outcome=%s, want the idle successor removed%s", released, inc.Successor.Outcome, dump(entries))
			}
		})
	}
}

func TestAddFirstAmbiguousSuccessorGoesToAnOperator(t *testing.T) {
	p := newSuccessorPlatform()
	p.ambiguous = true
	c, st, _ := successorFixture(t, recoverymode.Automatic, p, "")
	id := openOn(t, c, "n1")
	inc := waitSettled(t, c, st, id, func(inc *types.Incident) bool { return inc.Successor != nil && !inc.Successor.Open() })
	if _, released, _, _, _ := p.counts(); released != 0 || inc.Successor.Outcome != types.SuccessorHandedOver {
		t.Fatalf("released=%d outcome=%s, want nothing removed and an operator named", released, inc.Successor.Outcome)
	}
	if p.cordonedNode("n1") {
		t.Fatal("the node was cordoned without a successor")
	}
}

func TestAddFirstUnavailablePoolHoldsWithTheReason(t *testing.T) {
	p := newSuccessorPlatform()
	p.unavailable = fmt.Errorf("%w: agent pool train is at its maximum size", cloud.ErrAddFirstUnavailable)
	c, st, _ := successorFixture(t, recoverymode.Automatic, p, "")
	id := openOn(t, c, "n1")
	inc := step(t, c, st, id, 20)
	entries := trail(t, st, id)
	if inc.State != types.StateEvaluating || inc.StepIndex != 0 || inc.Successor != nil {
		t.Fatalf("state=%s step=%d successor=%+v, want held before the cordon%s", inc.State, inc.StepIndex, inc.Successor, dump(entries))
	}
	if !auditHas(entries, "recovery-unavailable", "maximum size") {
		t.Fatalf("the hold does not carry the provider's reason%s", dump(entries))
	}
}

func TestRemoveFirstStillReplacesDirectly(t *testing.T) {
	p := newSuccessorPlatform()
	c, st, _ := successorFixture(t, recoverymode.Automatic, p, recoverymode.RemoveFirst)
	id := openOn(t, c, "n1")
	inc := waitSettled(t, c, st, id, func(inc *types.Incident) bool { return inc.State.Halted() || inc.State == types.StateVerifying })
	if requests, _, replaced, plain, _ := p.counts(); requests != 0 || replaced != 0 || plain != 1 || inc.Successor != nil {
		t.Fatalf("requests=%d replaced=%d plain=%d successor=%+v, want a direct remove-first replacement", requests, replaced, plain, inc.Successor)
	}
}

func TestAddFirstApprovalNamesTheExtraNodeBeforeAnyRequest(t *testing.T) {
	p := newSuccessorPlatform()
	c, st, notifier := successorFixture(t, recoverymode.Approval, p, "")
	id := openOn(t, c, "n1")
	inc := step(t, c, st, id, 20)
	entries := trail(t, st, id)
	if inc.State != types.StateAwaitingApproval || inc.StepIndex != 0 || len(notifier.approvals) != 1 {
		t.Fatalf("state=%s step=%d approvals=%d, want the cordon parked for approval%s", inc.State, inc.StepIndex, len(notifier.approvals), dump(entries))
	}
	if requests, _, _, _, _ := p.counts(); requests != 0 {
		t.Fatalf("%d requests before approval", requests)
	}
	if !auditHas(entries, "cordon", "temporary additional node in pool train") {
		t.Fatalf("the approval round does not say a billed node is added%s", dump(entries))
	}
	// Rejected: nothing was requested, so nothing is left to remove.
	if err := c.DecideApproval(context.Background(), id, "bob", "test", types.ApprovalRejected, inc.ApprovalEpoch, "no", ""); err != nil {
		t.Fatal(err)
	}
	inc = waitSettled(t, c, st, id, func(inc *types.Incident) bool { return inc.Successor != nil && !inc.Successor.Open() })
	if requests, released, _, _, _ := p.counts(); requests != 0 || released != 0 || inc.Successor.Outcome != types.SuccessorNotCreated {
		t.Fatalf("requests=%d released=%d outcome=%s", requests, released, inc.Successor.Outcome)
	}
}

func TestAddFirstManualResolveDuringOverlapRemovesTheSuccessor(t *testing.T) {
	p := newSuccessorPlatform()
	c, st, _ := successorFixture(t, recoverymode.Approval, p, "")
	registerSuccessorAgent(t, st, 8)
	id := openOn(t, c, "n1")
	inc := step(t, c, st, id, 20)
	if err := c.DecideApproval(context.Background(), id, "bob", "test", types.ApprovalApproved, inc.ApprovalEpoch, "ok", ""); err != nil {
		t.Fatal(err)
	}
	// Two nodes run: the successor is ready, the drain waits for its own approval.
	waitSettled(t, c, st, id, func(inc *types.Incident) bool { return inc.State == types.StateAwaitingApproval && inc.StepIndex == 1 })
	if err := c.ResolveIncident(context.Background(), id, "bob", "fixed by hand"); err != nil {
		t.Fatal(err)
	}
	inc = waitSettled(t, c, st, id, func(inc *types.Incident) bool { return inc.Successor != nil && !inc.Successor.Open() })
	if _, released, _, _, _ := p.counts(); released != 1 || inc.Successor.Outcome != types.SuccessorReleased {
		t.Fatalf("released=%d outcome=%s, want the idle successor removed%s", released, inc.Successor.Outcome, dump(trail(t, st, id)))
	}
}

func TestAddFirstRestartContinuesTheSameOperation(t *testing.T) {
	p := newSuccessorPlatform()
	p.createAfter = 1_000_000 // stays requested
	c, st, _ := successorFixture(t, recoverymode.Automatic, p, "")
	id := openOn(t, c, "n1")
	// The first process dies mid-wait: its context ends and nothing it does
	// afterwards reaches the store.
	ctx, crash := context.WithCancel(context.Background())
	deadline := time.Now().Add(10 * time.Second)
	for {
		c.reconcile(ctx)
		if inc := mustIncident(t, st, id); inc.Successor != nil && inc.Successor.Phase == cloud.SuccessorRequested {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("never requested%s", dump(trail(t, st, id)))
		}
		time.Sleep(2 * time.Millisecond)
	}
	crash()
	for c.isInFlight(id) {
		time.Sleep(time.Millisecond)
	}
	p.mu.Lock()
	p.createAfter = 0
	p.mu.Unlock()
	live := safety.Limits{MaxConcurrentRemediations: 8, MaxConcurrentReboots: 8}
	restarted := New(st, st, nil, safety.NewGate(live), safety.NewFlapDetector(100, time.Hour), p, nil, &recordingNotifier{},
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := restarted.InstallRuntimeConfig(RuntimeConfig{SafetyLimits: &live}); err != nil {
		t.Fatal(err)
	}
	restarted.SetEngine(c.runtimeConfig(context.Background()).Engine)
	restarted.SetTimings(time.Millisecond, 0)
	restarted.successorPoll = time.Millisecond
	registerSuccessorAgent(t, st, 8)
	inc := waitSettled(t, restarted, st, id, func(inc *types.Incident) bool { return inc.State.Halted() || inc.State == types.StateVerifying })
	if requests, _, replaced, _, _ := p.counts(); requests != 1 || replaced != 1 {
		t.Fatalf("requests=%d replaced=%d, want the one request continued after the restart%s", requests, replaced, dump(trail(t, st, id)))
	}
	if inc.Successor.Outcome != types.SuccessorReplaced {
		t.Fatalf("successor = %+v", inc.Successor)
	}
}

func TestAddFirstDryRunCreatesNothingAndSaysWhatWouldBeAdded(t *testing.T) {
	p := newSuccessorPlatform()
	c, st, _ := successorFixture(t, recoverymode.Automatic, p, "")
	dry := safety.Limits{MaxConcurrentRemediations: 8, MaxConcurrentReboots: 8, DryRun: true}
	engine := c.runtimeConfig(context.Background()).Engine
	if err := c.InstallRuntimeConfig(RuntimeConfig{SafetyLimits: &dry}); err != nil {
		t.Fatal(err)
	}
	c.SetEngine(engine)
	id := openOn(t, c, "n1")
	inc := waitSettled(t, c, st, id, func(inc *types.Incident) bool { return inc.State.Halted() || inc.State == types.StateVerifying })
	entries := trail(t, st, id)
	if requests, _, replaced, plain, drained := p.counts(); requests+replaced+plain+drained != 0 || inc.Successor != nil {
		t.Fatalf("a dry run changed the cloud or recorded a successor: %+v%s", inc.Successor, dump(entries))
	}
	if !auditHas(entries, "cordon", "temporary additional node would be created") {
		t.Fatalf("the simulated cordon does not say a node would be added%s", dump(entries))
	}
	if !auditHas(entries, "replace", "DRY-RUN") {
		t.Fatalf("the replacement was not simulated%s", dump(entries))
	}
}
