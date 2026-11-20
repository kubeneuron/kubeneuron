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
	replaceErr           error
	replaceAttempts      int
	// oldGPUs is what the replaced node advertises; dropAfterPlan sets it to
	// 0 once the operation is planned. prepareErrs / nodeErrs: transient
	// failures of the next requested-phase polls and successor node reads.
	oldGPUs               int64
	oldNoResource         bool
	dropAfterPlan         bool
	plans                 int
	prepareErrs, nodeErrs int

	// removeFirstRefusal: RemoveFirstAvailable's answer.
	removeFirstRefusal error

	created    time.Time
	cordoned   map[string][]string
	drained    []string
	released   []cloud.Successor
	replacedBy []cloud.Successor
	plain      []string // ReplacePinnedNode: remove-first
}

func newSuccessorPlatform() *successorPlatform {
	return &successorPlatform{nodeReady: true, successorAllocatable: 8, oldGPUs: 8, cordoned: map[string][]string{}}
}

func (p *successorPlatform) CloudRecyclingConfigured() bool                 { return true }
func (p *successorPlatform) CheckRecycleNode(context.Context, string) error { return nil }
func (p *successorPlatform) RecycleNode(context.Context, string) error      { return nil }
func (p *successorPlatform) ReplaceNode(context.Context, string) error      { return nil }

func (p *successorPlatform) AddFirstAvailable(context.Context, cloud.NodeIdentity) error {
	return p.unavailable
}

func (p *successorPlatform) RemoveFirstAvailable(context.Context, cloud.NodeIdentity) error {
	return p.removeFirstRefusal
}

func (p *successorPlatform) PrepareSuccessor(_ context.Context, pin cloud.NodeIdentity, s cloud.Successor) (cloud.Successor, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	switch s.Phase {
	case "":
		p.plans++
		s.Phase, s.Pool = cloud.SuccessorPlanned, "train"
		s.State = map[string]string{"members": "4"}
		if p.dropAfterPlan {
			p.oldGPUs = 0
		}
	case cloud.SuccessorPlanned:
		p.requests++
		if p.capacity {
			return s, fmt.Errorf("pool train: %w", cloud.ErrReplacementCapacity)
		}
		s.Phase = cloud.SuccessorRequested
	case cloud.SuccessorRequested:
		if p.prepareErrs > 0 {
			p.prepareErrs--
			return s, errors.New("the cloud API timed out")
		}
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
	if p.nodeErrs > 0 {
		p.nodeErrs--
		return platform.SuccessorNode{}, errors.New("the node list timed out")
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
	p.replaceAttempts++
	if p.replaceErr != nil {
		return p.replaceErr
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
	p.mu.Lock()
	defer p.mu.Unlock()
	n := p.oldGPUs
	if p.oldNoResource {
		return topology.Resources{}, true, nil
	}
	return topology.Resources{Capacity: map[string]int64{topology.ResourceWholeGPU: n}, Allocatable: map[string]int64{topology.ResourceWholeGPU: n}}, true, nil
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
		qualifyAutoProfile(t, c)
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
	c, st, notifier := successorFixture(t, recoverymode.Automatic, p, "")
	id := openOn(t, c, "n1")
	inc := step(t, c, st, id, 20)
	entries := trail(t, st, id)
	if inc.State != types.StateEvaluating || inc.StepIndex != 0 || inc.Successor != nil {
		t.Fatalf("state=%s step=%d successor=%+v, want held before the cordon%s", inc.State, inc.StepIndex, inc.Successor, dump(entries))
	}
	if !auditHas(entries, "recovery-unavailable", "maximum size") {
		t.Fatalf("the hold does not carry the provider's reason%s", dump(entries))
	}

	notifications := 0
	for _, event := range notifier.events {
		if strings.Contains(event.Message, "maximum size") {
			notifications++
		}
	}
	if notifications != 1 {
		t.Fatalf("got %d pool hold notifications after 20 passes", notifications)
	}
	for _, action := range []string{"notify-pending", "notified"} {
		count := 0
		for _, entry := range entries {
			if entry.Action == action {
				count++
			}
		}
		if count != 1 {
			t.Fatalf("got %d %s rows", count, action)
		}
	}
	// Advance the cached observation past its TTL without changing the incident.
	expireCheck := func() {
		c.addFirst.mu.Lock()
		defer c.addFirst.mu.Unlock()
		key := successorOperation(inc)
		entry := c.addFirst.entries[key]
		entry.at = time.Now().Add(-2 * addFirstCheckTTL)
		c.addFirst.entries[key] = entry
	}
	p.unavailable = fmt.Errorf("%w: node group spans several zones", cloud.ErrAddFirstUnavailable)
	expireCheck()
	step(t, c, st, id, 10)
	notices := 0
	for _, event := range notifier.events {
		if strings.Contains(event.Message, "several zones") {
			notices++
		}
	}
	if notices != 1 || p.cordonedNode("n1") {
		t.Fatalf("changed reason: notices=%d cordoned=%v", notices, p.cordonedNode("n1"))
	}
	if requests, released, replaced, plain, drained := p.counts(); requests+released+replaced+plain+drained != 0 {
		t.Fatal("provider acted during the hold")
	}
	registerSuccessorAgent(t, st, 8)
	p.unavailable = nil
	expireCheck()
	inc = waitSettled(t, c, st, id, func(i *types.Incident) bool { return i.State.Halted() || i.State == types.StateVerifying })
	if inc.Successor == nil || inc.Successor.Outcome != types.SuccessorReplaced || !auditHas(trail(t, st, id), "recovery-available-again", "") {
		t.Fatalf("corrected pool did not resume the existing incident: %s%s", inc.State, dump(trail(t, st, id)))
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

// A provider that cannot replace the node remove-first says so before the
// cordon: the node is never drained for a replacement that cannot run.
func TestRemoveFirstRefusedHoldsBeforeTheCordon(t *testing.T) {
	p := newSuccessorPlatform()
	p.removeFirstRefusal = fmt.Errorf("%w: node pool gpu is fixed-size", cloud.ErrRemoveFirstUnavailable)
	c, st, _ := successorFixture(t, recoverymode.Automatic, p, recoverymode.RemoveFirst)
	id := openOn(t, c, "n1")
	inc := step(t, c, st, id, 20)
	entries := trail(t, st, id)
	if inc.State != types.StateEvaluating || inc.StepIndex != 0 || p.cordonedNode("n1") {
		t.Fatalf("state=%s step=%d cordoned=%v, want held before the cordon%s", inc.State, inc.StepIndex, p.cordonedNode("n1"), dump(entries))
	}
	if !auditHas(entries, "recovery-unavailable", "fixed-size") || !auditHas(entries, "recovery-unavailable", "not cordoned or drained") {
		t.Fatalf("the hold does not carry the provider's reason%s", dump(entries))
	}
	if _, _, replaced, plain, drained := p.counts(); replaced != 0 || plain != 0 || drained != 0 {
		t.Fatalf("replaced=%d plain=%d drained=%d, want nothing", replaced, plain, drained)
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
	qualifyAutoProfile(t, restarted)
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
	qualifyAutoProfile(t, c)
	id := openOn(t, c, "n1")
	inc := waitSettled(t, c, st, id, func(inc *types.Incident) bool { return inc.State.Halted() || inc.State == types.StateVerifying })
	entries := trail(t, st, id)
	if requests, _, replaced, plain, drained := p.counts(); requests+replaced+plain+drained != 0 || inc.Successor != nil {
		t.Fatalf("a dry run changed the cloud or recorded a successor: %+v%s", inc.Successor, dump(entries))
	}
	if !auditHas(entries, "cordon", "creates one temporary additional node in the node's pool") {
		t.Fatalf("the simulated cordon does not say a node would be added%s", dump(entries))
	}
	if !auditHas(entries, "replace", "DRY-RUN") {
		t.Fatalf("the replacement was not simulated%s", dump(entries))
	}
}

func TestAddFirstRetriesTransientCloudErrors(t *testing.T) {
	p := newSuccessorPlatform()
	p.prepareErrs, p.nodeErrs = 2, 1
	c, st, _ := successorFixture(t, recoverymode.Automatic, p, "")
	registerSuccessorAgent(t, st, 8)
	id := openOn(t, c, "n1")
	inc := waitSettled(t, c, st, id, func(inc *types.Incident) bool { return inc.State.Halted() || inc.State == types.StateVerifying })
	entries := trail(t, st, id)
	requests, _, replaced, plain, _ := p.counts()
	if requests != 1 || replaced != 1 || plain != 0 || inc.Successor.Outcome != types.SuccessorReplaced {
		t.Fatalf("requests=%d replaced=%d plain=%d successor=%+v, want one request and a replacement after the retries%s",
			requests, replaced, plain, inc.Successor, dump(entries))
	}
	if !auditHas(entries, "successor-retrying", "timed out") {
		t.Fatalf("the retries are not recorded%s", dump(entries))
	}
}

func TestAddFirstComparesAgainstTheNodeAsPlanned(t *testing.T) {
	// The device plugin stops counting the lost devices after the plan.
	p := newSuccessorPlatform()
	p.dropAfterPlan = true
	c, st, _ := successorFixture(t, recoverymode.Automatic, p, "")
	registerSuccessorAgent(t, st, 8)
	id := openOn(t, c, "n1")
	inc := waitSettled(t, c, st, id, func(inc *types.Incident) bool { return inc.State.Halted() || inc.State == types.StateVerifying })
	if _, _, replaced, _, _ := p.counts(); replaced != 1 || inc.Successor.Expected[topology.ResourceWholeGPU] != 8 {
		t.Fatalf("replaced=%d successor=%+v, want a replacement checked against the 8 GPUs planned%s", replaced, inc.Successor, dump(trail(t, st, id)))
	}
}

func TestAddFirstTakesTheAgentCountWhenTheNodeAdvertisesNone(t *testing.T) {
	p := newSuccessorPlatform()
	p.oldGPUs = 0
	p.successorAllocatable = 4
	c, st, _ := successorFixture(t, recoverymode.Automatic, p, "")
	old := &types.Node{Name: "n1", AgentLastSeen: time.Now()}
	for i := range 8 {
		old.GPUs = append(old.GPUs, types.GPUInfo{UUID: fmt.Sprintf("GPU-old-%d", i), Index: i})
	}
	if err := st.UpsertAgentRegistration(context.Background(), old); err != nil {
		t.Fatal(err)
	}
	registerSuccessorAgent(t, st, 8)
	id := openOn(t, c, "n1")
	inc := waitSettled(t, c, st, id, func(inc *types.Incident) bool {
		return inc.Successor != nil && inc.Successor.Reason != "" || inc.State.Halted()
	})
	if s := inc.Successor; s.Expected[topology.ResourceWholeGPU] != 8 || s.ExpectedGPUs != 8 || !strings.Contains(s.Reason, "fewer than the 8") {
		t.Fatalf("successor = %+v, want 8 GPUs expected from the agent and a 4-GPU successor refused%s", s, dump(trail(t, st, id)))
	}
	if p.cordonedNode("n1") {
		t.Fatal("the node was cordoned for a smaller successor")
	}
}

func TestAddFirstHoldsWithNothingToCompareAgainst(t *testing.T) {
	p := newSuccessorPlatform()
	p.oldGPUs = 0
	c, st, _ := successorFixture(t, recoverymode.Automatic, p, "")
	id := openOn(t, c, "n1")
	inc := step(t, c, st, id, 20)
	entries := trail(t, st, id)
	if inc.State != types.StateEvaluating || inc.Successor != nil || p.plans != 0 || p.requests != 0 {
		t.Fatalf("state=%s successor=%+v plans=%d requests=%d, want held before any plan%s", inc.State, inc.Successor, p.plans, p.requests, dump(entries))
	}
	if !auditHas(entries, "recovery-unavailable", "advertises no accelerator resources") {
		t.Fatalf("the hold does not say why%s", dump(entries))
	}
}

func TestAddFirstNeedsAllocatableDevicesWhenOnlyTheAgentCountIsKnown(t *testing.T) {
	p := newSuccessorPlatform()
	p.oldNoResource = true
	p.successorAllocatable = 0
	c, st, _ := successorFixture(t, recoverymode.Automatic, p, "")
	old := &types.Node{Name: "n1", AgentLastSeen: time.Now()}
	for i := range 8 {
		old.GPUs = append(old.GPUs, types.GPUInfo{UUID: fmt.Sprintf("GPU-old-%d", i), Index: i})
	}
	if err := st.UpsertAgentRegistration(context.Background(), old); err != nil {
		t.Fatal(err)
	}
	registerSuccessorAgent(t, st, 8)
	id := openOn(t, c, "n1")
	inc := waitSettled(t, c, st, id, func(inc *types.Incident) bool {
		return inc.Successor != nil && inc.Successor.Reason != "" || inc.State.Halted()
	})
	if s := inc.Successor; len(s.Expected) != 0 || s.ExpectedGPUs != 8 || !strings.Contains(s.Reason, "0 allocatable accelerator resources") {
		t.Fatalf("successor = %+v, want a successor with nothing allocatable refused%s", s, dump(trail(t, st, id)))
	}
	if p.cordonedNode("n1") || inc.Successor.ReadyAt != nil {
		t.Fatal("a successor with only agent inventory was accepted")
	}
}

func TestAddFirstUnknownDeleteKeepsTheSuccessor(t *testing.T) {
	p := newSuccessorPlatform()
	p.replaceErr = fmt.Errorf("deleting the old machine: the result is unknown: %w", cloud.ErrNeedsOperator)
	c, st, _ := successorFixture(t, recoverymode.Automatic, p, "")
	configureRepairClasses(t, c)
	registerSuccessorAgent(t, st, 8)
	id := openOn(t, c, "n1")
	inc := waitSettled(t, c, st, id, func(inc *types.Incident) bool { return inc.Successor != nil && !inc.Successor.Open() })
	for range 5 {
		c.reconcile(context.Background())
	}
	if _, released, _, _, _ := p.counts(); released != 0 || inc.Successor.Outcome != types.SuccessorHandedOver {
		t.Fatalf("released=%d successor=%+v, want the successor kept for an operator%s", released, inc.Successor, dump(trail(t, st, id)))
	}
	c = restartWith(t, c, st, p)
	sig := signal(types.ClassAcceleratorFault, "n1", "")
	if err := c.IngestSignal(t.Context(), sig); err != nil {
		t.Fatal(err)
	}
	follower, err := st.GetOpenIncident(t.Context(), sig.Target, sig.Class)
	if err != nil {
		t.Fatal(err)
	}
	got := step(t, c, st, follower.ID, 20)
	assessment := types.RecoveryAvailabilityFromAudit(trail(t, st, follower.ID))
	if got.State != types.StateEvaluating || assessment == nil || assessment.Source != "node-repair" || !strings.Contains(assessment.Reason, id) {
		t.Fatalf("follower state=%s assessment=%+v", got.State, assessment)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.replaceAttempts != 1 || len(p.drained) != 1 || len(p.plain) != 0 {
		t.Fatalf("replace attempts=%d drains=%d remove-first calls=%d", p.replaceAttempts, len(p.drained), len(p.plain))
	}
}

func TestReleaseRequestedSuccessorChecksWorkload(t *testing.T) {
	for _, busy := range []bool{false, true} {
		t.Run(fmt.Sprintf("busy=%t", busy), func(t *testing.T) {
			p := newSuccessorPlatform()
			p.createAfter = 0
			p.successorWorkload = busy
			c, st, _ := successorFixture(t, recoverymode.Automatic, p, "")
			id := openOn(t, c, "n1")
			inc := mustIncident(t, st, id)
			inc.CloudIdentity = &cloud.NodeIdentity{InstanceID: "i-old"}
			rec := &types.SuccessorRecord{Successor: cloud.Successor{Operation: "cancel", Instance: "i-old", Pool: "train", Phase: cloud.SuccessorRequested, RequestedAt: time.Now()}}
			if err := c.saveSuccessor(t.Context(), inc, rec, "successor-requested", "fixture"); err != nil {
				t.Fatal(err)
			}
			if err := c.releaseSuccessor(t.Context(), inc, "cancelled"); err != nil {
				t.Fatal(err)
			}
			saved := mustIncident(t, st, id).Successor
			if saved.ProviderID == "" {
				t.Fatal("missing saved successor identity")
			}
			_, released, _, _, _ := p.counts()
			if busy && (released != 0 || saved.Outcome != types.SuccessorHandedOver) || !busy && (released != 1 || saved.Outcome != types.SuccessorReleased) {
				t.Fatalf("released=%d outcome=%s", released, saved.Outcome)
			}
		})
	}
}

func TestReportIncludesHeldAddFirstIncidentAfterManualClosure(t *testing.T) {
	p := newSuccessorPlatform()
	p.unavailable = fmt.Errorf("%w: no room below maxSize", cloud.ErrAddFirstUnavailable)
	c, st, _ := successorFixture(t, recoverymode.Automatic, p, "")
	id := openOn(t, c, "n1")
	step(t, c, st, id, 10)
	if !auditHas(trail(t, st, id), "notified", "no room below maxSize") {
		t.Fatal("hold was not established")
	}
	readReport := func() *types.RecoveryReport {
		t.Helper()
		r, err := c.RecoveryReport(t.Context(), time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		if r.Incidents != 1 || r.Recovered != 0 || r.RecoveredGPUHours != 0 || len(r.IncidentDetails) != 1 || r.IncidentDetails[0].ID != id {
			t.Fatalf("unexpected recovery accounting: %+v", r)
		}
		return r
	}
	r := readReport()
	if len(r.Open) != 1 || r.Open[0].ID != id || r.ObservedOnly != 0 {
		t.Fatalf("held incident: %+v", r)
	}
	if err := c.ResolveIncident(t.Context(), id, "operator", "closing without a replacement"); err != nil {
		t.Fatal(err)
	}
	r = readReport()
	if len(r.Open) != 0 || r.ObservedOnly != 1 || r.IncidentDetails[0].RecoverySeconds != nil {
		t.Fatalf("manually closed incident: %+v", r)
	}
}

func TestMultiZoneIncidentHoldsBeforeCordon(t *testing.T) {
	const action = "Use a single-zone node pool, or set RemoveFirst in the remediation policy that applies to these nodes."
	for name, reason := range map[string]string{"AKS": "AKS chooses the zone of a new node in this pool", "GKE": "add-first would add a node in every zone of this pool"} {
		t.Run(name, func(t *testing.T) {
			p := newSuccessorPlatform()
			p.unavailable = &cloud.PoolRefusal{Code: cloud.PoolMultiZone, Reason: reason}
			c, st, notifier := successorFixture(t, recoverymode.Automatic, p, "")
			id := openOn(t, c, "n1")
			inc := step(t, c, st, id, 20)
			if inc.State != types.StateEvaluating || inc.StepIndex != 0 || inc.Successor != nil || p.cordonedNode("n1") || !auditHas(trail(t, st, id), "recovery-unavailable", reason) {
				t.Fatalf("unexpected hold: %s", dump(trail(t, st, id)))
			}
			notices := 0
			for _, e := range notifier.events {
				if strings.Contains(e.Message, reason) && strings.Contains(e.Message, action) {
					notices++
				}
			}
			if notices != 1 {
				t.Fatalf("expected one notification with reason and action, got %+v", notifier.events)
			}
		})
	}
}
