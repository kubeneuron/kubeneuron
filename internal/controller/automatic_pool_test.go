// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/kubeneuron/kubeneuron/internal/platform"
	"github.com/kubeneuron/kubeneuron/internal/playbook"
	"github.com/kubeneuron/kubeneuron/internal/recoverymode"
	"github.com/kubeneuron/kubeneuron/internal/store/sqlite"
	"github.com/kubeneuron/kubeneuron/pkg/cloud"
	"github.com/kubeneuron/kubeneuron/pkg/types"
)

// clusterNodesPlatform is the automatic fixture's cloud platform with the
// cluster's node objects: a node is Ready while it exists.
type clusterNodesPlatform struct {
	autoCloudPlatform
	nodes    map[string]types.Node
	notReady map[string]bool
}

func clusterOf(nodes ...types.Node) *clusterNodesPlatform {
	p := &clusterNodesPlatform{nodes: map[string]types.Node{}}
	for _, n := range nodes {
		p.nodes[n.Name] = n
	}
	return p
}

func trainNode(name, uid string) types.Node {
	return types.Node{Name: name, UID: uid, Labels: map[string]string{"pool": "train"}}
}

func (p *clusterNodesPlatform) NodeInventory(context.Context) ([]types.Node, error) {
	var out []types.Node
	for _, n := range p.nodes {
		out = append(out, n)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (p *clusterNodesPlatform) NodeLabels(_ context.Context, node string) (map[string]string, bool, error) {
	n, ok := p.nodes[node]
	return n.Labels, ok, nil
}

func (p *clusterNodesPlatform) NodeReady(_ context.Context, node string) (bool, error) {
	_, ok := p.nodes[node]
	return ok && !p.notReady[node], nil
}

// registerAgentWithLabels records node with labels, as the inventory does,
// then as its agent registers it; nil labels record none. The store keeps
// those labels from then on.
func registerAgentWithLabels(t *testing.T, st *sqlite.Store, node, uid string, labels map[string]string) {
	t.Helper()
	ctx := context.Background()
	if labels != nil {
		if err := st.UpsertNode(ctx, &types.Node{Name: node, UID: uid, Labels: labels}); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.UpsertAgentRegistration(ctx, &types.Node{Name: node, UID: uid, AgentLastSeen: time.Now()}); err != nil {
		t.Fatal(err)
	}
}

// authorisedOpen records an incident on node that the policy is recovering:
// open, verifying with a step the policy started. Its class is not the fixture's, so
// the two faults are not read as one shared fault.
func authorisedOpen(t *testing.T, st *sqlite.Store, id, node string, pin *cloud.NodeIdentity) {
	t.Helper()
	ctx := context.Background()
	now := time.Now()
	if err := st.CreateIncident(ctx, &types.Incident{ID: id, Class: types.ClassDriverHang, Target: types.Target{Node: node},
		State: types.StateVerifying, CloudIdentity: pin, OpenedAt: now, UpdatedAt: now, StateChangedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := st.AppendAudit(ctx, &types.AuditEntry{IncidentID: id, Time: now, FromState: types.StateEvaluating, ToState: types.StateExecuting,
		Actor: policyActor(autoPolicy), Action: "replace", Result: "step-start: platform.replace_node"}); err != nil {
		t.Fatal(err)
	}
}

// The store keeps the row of a replaced node after the node is deleted. The
// breaker counts the nodes that exist: the first fault on the one node left
// in the pool is recovered automatically.
func TestBreakerDoesNotCountADeletedNode(t *testing.T) {
	c, st, notifier := autoFixture(t, recoverymode.Automatic, autoSettings(), 0)
	registerAgentWithLabels(t, st, "n0", "uid-n0", map[string]string{"pool": "train"})
	registerAgentWithLabels(t, st, "n1", "uid-n1", map[string]string{"pool": "train"})
	c.platform = clusterOf(trainNode("n1", "uid-n1"))
	id := openOn(t, c, "n1")
	if inc := step(t, c, st, id, 200); inc.State != types.StateResolved || len(notifier.approvals) != 0 {
		t.Fatalf("state=%s audit=%s, want an automatic recovery", inc.State, parkReason(t, st, id))
	}
}

// Two of two nodes that exist unhealthy trip the breaker, also when the store
// recorded one of them without its pool label: the label it carries now
// counts.
func TestBreakerTripsOnTheNodesThatExist(t *testing.T) {
	c, st, _ := autoFixture(t, recoverymode.Automatic, autoSettings(), 1)
	registerAgentWithLabels(t, st, "n2", "uid-n2", nil)
	c.platform = clusterOf(trainNode("n1", "uid-n1"), trainNode("n2", "uid-n2"))
	first := openOn(t, c, "n1")
	setNodePaused(t, st, "n1", true) // keeps the first incident open
	step(t, c, st, first, 10)
	id := openOn(t, c, "n2")
	const want = "circuit breaker: 2 of 2 nodes in the pool are unhealthy, above 50%"
	if inc := step(t, c, st, id, 50); inc.State != types.StateAwaitingApproval || !strings.Contains(parkReason(t, st, id), want) {
		t.Fatalf("state=%s audit=%s, want %q", inc.State, parkReason(t, st, id), want)
	}
}

// A node recreated under the same name with a new UID is the node that
// exists: an open incident about the earlier node of that name does not make
// the Ready new one unhealthy.
func TestBreakerDoesNotCountTheEarlierNodeOfAName(t *testing.T) {
	c, st, notifier := autoFixture(t, recoverymode.Automatic, autoSettings(), 0)
	registerAgentWithLabels(t, st, "n1", "uid-n1", map[string]string{"pool": "train"})
	registerAgentWithLabels(t, st, "n2", "uid-n2", map[string]string{"pool": "train"})
	c.platform = clusterOf(trainNode("n1", "uid-n1-new"), trainNode("n2", "uid-n2"))
	c.SetTimings(time.Hour, 0) // the earlier node's recovery stays verifying
	authorisedOpen(t, st, "replacing-n1", "n1", &cloud.NodeIdentity{Node: "n1", NodeUID: "uid-n1"})
	id := openOn(t, c, "n2")
	inc := waitSettled(t, c, st, id, func(inc *types.Incident) bool {
		return inc.State == types.StateAwaitingApproval || inc.State == types.StateVerifying || inc.State.Halted()
	})
	if inc.State != types.StateVerifying || len(notifier.approvals) != 0 {
		t.Fatalf("state=%s audit=%s, want an automatic recovery", inc.State, parkReason(t, st, id))
	}
}

// recordPool records the group the policy authorised the incident's step
// in.
func recordPool(t *testing.T, st *sqlite.Store, id, group string) {
	t.Helper()
	recordPoolUnder(t, st, id, "group", group)
}

// recordPoolUnder records the pool the policy authorised the incident's step
// in, by the pool label it used then.
func recordPoolUnder(t *testing.T, st *sqlite.Store, id, label, pool string) {
	t.Helper()
	if err := st.AppendAudit(context.Background(), &types.AuditEntry{IncidentID: id, Time: time.Now(), FromState: types.StateExecuting,
		ToState: types.StateExecuting, Actor: policyActor(autoPolicy), Action: policyAuthorisedAction,
		Params: map[string]string{"pool_label": label, "pool": pool}, Result: "authorised by policy " + autoPolicy}); err != nil {
		t.Fatal(err)
	}
}

func groupLabels(group string) map[string]string {
	return map[string]string{"pool": "train", "group": group}
}

// The per-pool limit counts an open recovery in the pool its authorisation
// recorded, wherever its node is now. A recovery authorised before the pool
// was recorded, or recorded under a pool label the policy no longer uses,
// counts by its node's labels now, and once its node is gone,
// unless its recorded labels place it in another pool of the policy. When
// the counted recovery ends, the step goes ahead.
func TestPoolLimitCountsTheRecoveriesOfThePool(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T, st *sqlite.Store, p *clusterNodesPlatform) string
		held  bool
	}{
		{name: "recorded in the pool, node gone, no labels recorded", held: true, setup: func(t *testing.T, st *sqlite.Store, _ *clusterNodesPlatform) string {
			registerAgentWithLabels(t, st, "n0", "uid-n0", nil)
			authorisedOpen(t, st, "replacing-n0", "n0", &cloud.NodeIdentity{Node: "n0", NodeUID: "uid-n0"})
			recordPool(t, st, "replacing-n0", "a")
			return "replacing-n0"
		}},
		{name: "recorded in another pool, node relabelled into this one", setup: func(t *testing.T, st *sqlite.Store, p *clusterNodesPlatform) string {
			registerAgentWithLabels(t, st, "n3", "uid-n3", groupLabels("a"))
			p.nodes["n3"] = types.Node{Name: "n3", UID: "uid-n3", Labels: groupLabels("a")}
			authorisedOpen(t, st, "recovering-n3", "n3", nil)
			recordPool(t, st, "recovering-n3", "b")
			return "recovering-n3"
		}},
		{name: "recorded by another pool label, node gone, no labels recorded", held: true, setup: func(t *testing.T, st *sqlite.Store, _ *clusterNodesPlatform) string {
			registerAgentWithLabels(t, st, "n0", "uid-n0", nil)
			authorisedOpen(t, st, "replacing-n0", "n0", &cloud.NodeIdentity{Node: "n0", NodeUID: "uid-n0"})
			recordPoolUnder(t, st, "replacing-n0", "rack", "r1")
			return "replacing-n0"
		}},
		{name: "not recorded, relabelled into the pool", held: true, setup: func(t *testing.T, st *sqlite.Store, p *clusterNodesPlatform) string {
			registerAgentWithLabels(t, st, "n3", "uid-n3", groupLabels("b"))
			p.nodes["n3"] = types.Node{Name: "n3", UID: "uid-n3", Labels: groupLabels("a")}
			authorisedOpen(t, st, "recovering-n3", "n3", nil)
			return "recovering-n3"
		}},
		{name: "not recorded, node gone", held: true, setup: func(t *testing.T, st *sqlite.Store, _ *clusterNodesPlatform) string {
			registerAgentWithLabels(t, st, "n0", "uid-n0", groupLabels("a"))
			authorisedOpen(t, st, "replacing-n0", "n0", &cloud.NodeIdentity{Node: "n0", NodeUID: "uid-n0"})
			return "replacing-n0"
		}},
		{name: "not recorded, node gone, no labels recorded", held: true, setup: func(t *testing.T, st *sqlite.Store, _ *clusterNodesPlatform) string {
			registerAgentWithLabels(t, st, "n0", "uid-n0", nil)
			authorisedOpen(t, st, "replacing-n0", "n0", &cloud.NodeIdentity{Node: "n0", NodeUID: "uid-n0"})
			return "replacing-n0"
		}},
		{name: "not recorded, node gone, labels the policy does not select", held: true, setup: func(t *testing.T, st *sqlite.Store, _ *clusterNodesPlatform) string {
			registerAgentWithLabels(t, st, "n0", "uid-n0", map[string]string{"group": "b"})
			authorisedOpen(t, st, "replacing-n0", "n0", &cloud.NodeIdentity{Node: "n0", NodeUID: "uid-n0"})
			return "replacing-n0"
		}},
		{name: "not recorded, node gone from another pool", setup: func(t *testing.T, st *sqlite.Store, _ *clusterNodesPlatform) string {
			registerAgentWithLabels(t, st, "n0", "uid-n0", groupLabels("b"))
			authorisedOpen(t, st, "replacing-n0", "n0", &cloud.NodeIdentity{Node: "n0", NodeUID: "uid-n0"})
			return "replacing-n0"
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			settings := autoSettings()
			settings.PoolLabel = "group"
			settings.MaxConcurrentNodes, settings.MaxConcurrentNodesPerPool, settings.MaxUnhealthyPercent = 8, 1, 100
			c, st, _ := autoFixture(t, recoverymode.Automatic, settings, 0)
			c.SetTimings(time.Hour, 0) // the recovery being counted stays verifying
			registerAgentWithLabels(t, st, "n1", "uid-n1", groupLabels("a"))
			p := clusterOf(types.Node{Name: "n1", UID: "uid-n1", Labels: groupLabels("a")})
			c.platform = p
			counted := tc.setup(t, st, p)
			id := openOn(t, c, "n1")
			const want = "the policy is already recovering 1 node(s) in this pool, its limit"
			inc := step(t, c, st, id, 20)
			if held := inc.State == types.StateEvaluating && strings.Contains(parkReason(t, st, id), want); held != tc.held {
				t.Fatalf("state=%s audit=%s, want held=%v", inc.State, parkReason(t, st, id), tc.held)
			}
			if !tc.held {
				return
			}
			if err := c.ResolveIncident(context.Background(), counted, "token:alice", "replacement done"); err != nil {
				t.Fatal(err)
			}
			if inc := step(t, c, st, id, 50); inc.State == types.StateEvaluating || inc.State == types.StateAwaitingApproval {
				t.Fatalf("state=%s audit=%s, want the step to go ahead once the counted recovery ended", inc.State, parkReason(t, st, id))
			}
		})
	}
}

// A recovery authorised before its pool was recorded counts, once its node
// is gone, in the pool of the labels its node had when it went: the
// inventory recorded them while it existed, not those its agent registered
// with.
func TestPoolLimitCountsAGoneNodeByItsLastLabels(t *testing.T) {
	settings := autoSettings()
	settings.PoolLabel = "group"
	settings.MaxConcurrentNodes, settings.MaxConcurrentNodesPerPool, settings.MaxUnhealthyPercent = 8, 1, 100
	c, st, _ := autoFixture(t, recoverymode.Automatic, settings, 0)
	c.SetTimings(time.Hour, 0) // the recovery being counted stays verifying
	registerAgentWithLabels(t, st, "n1", "uid-n1", groupLabels("a"))
	registerAgentWithLabels(t, st, "n0", "uid-n0", groupLabels("b"))
	p := clusterOf(types.Node{Name: "n1", UID: "uid-n1", Labels: groupLabels("a")},
		types.Node{Name: "n0", UID: "uid-n0", Labels: groupLabels("a")})
	c.platform = p
	c.syncAcceleratorInventory(context.Background())
	delete(p.nodes, "n0")
	authorisedOpen(t, st, "replacing-n0", "n0", &cloud.NodeIdentity{Node: "n0", NodeUID: "uid-n0"})
	id := openOn(t, c, "n1")
	const want = "the policy is already recovering 1 node(s) in this pool, its limit"
	if inc := step(t, c, st, id, 20); inc.State != types.StateEvaluating || !strings.Contains(parkReason(t, st, id), want) {
		t.Fatalf("state=%s audit=%s, want held by the recovery of n0", inc.State, parkReason(t, st, id))
	}
}

// replacingCluster is the successor platform with the cluster's node
// objects: the successor joins the pool once it is created, and the node a
// replacement removes leaves it.
type replacingCluster struct {
	*successorPlatform
	// joining is how many more reads see the created successor NotReady.
	joining int
}

func (p *replacingCluster) SuccessorNode(ctx context.Context, providerID string) (platform.SuccessorNode, error) {
	n, err := p.successorPlatform.SuccessorNode(ctx, providerID)
	p.mu.Lock()
	defer p.mu.Unlock()
	if n.Found && p.joining > 0 {
		p.joining--
		n.Ready = false
	}
	return n, err
}

func (p *replacingCluster) present() map[string]types.Node {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := map[string]types.Node{}
	gone := map[string]bool{}
	for _, s := range p.replacedBy {
		gone[strings.TrimPrefix(s.Instance, "i-")] = true
	}
	for _, instance := range p.plain {
		gone[strings.TrimPrefix(instance, "i-")] = true
	}
	for _, n := range []string{"n1", successorNodeName} {
		if !gone[n] && (n == "n1" || !p.created.IsZero()) {
			out[n] = trainNode(n, "uid-"+n)
		}
	}
	return out
}

func (p *replacingCluster) NodeInventory(context.Context) ([]types.Node, error) {
	var out []types.Node
	for _, n := range p.present() {
		out = append(out, n)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (p *replacingCluster) NodeLabels(_ context.Context, node string) (map[string]string, bool, error) {
	n, ok := p.present()[node]
	return n.Labels, ok, nil
}

func (p *replacingCluster) NodeReady(_ context.Context, node string) (bool, error) {
	_, ok := p.present()[node]
	p.mu.Lock()
	defer p.mu.Unlock()
	return ok && (node != successorNodeName || p.joining == 0), nil
}

// Two faults one after the other in a pool of one node: A is replaced add
// first, then B, on the node that took A's place, is authorised by the
// policy through its remove-first replacement. The store still records A's
// node, and records B's node without its pool label.
func TestSecondFaultInASmallPoolAfterAReplacement(t *testing.T) {
	p := &replacingCluster{successorPlatform: newSuccessorPlatform()}
	c, st, notifier := successorFixture(t, recoverymode.Automatic, p.successorPlatform, "")
	c.platform = p
	registerSuccessorAgent(t, st, 8)
	registerOldAgent(t, st, 8)
	first := openOn(t, c, "n1")
	if inc := waitSettled(t, c, st, first, func(inc *types.Incident) bool { return inc.State.Halted() }); inc.State != types.StateResolved {
		t.Fatalf("A: state=%s%s", inc.State, dump(trail(t, st, first)))
	}
	if _, ok := p.present()["n1"]; ok {
		t.Fatal("precondition: A's node is still in the cluster")
	}
	if !auditHasParam(trail(t, st, first), policyAuthorisedAction, "pool", "train") {
		t.Fatalf("A: no step recorded the pool it was authorised in%s", dump(trail(t, st, first)))
	}

	book := &playbook.Playbook{Name: "cloud-replace", Target: "node", Steps: []playbook.Step{
		{Name: "cordon", Action: "platform.cordon"},
		{Name: "drain", Action: "platform.drain", Approval: "required"},
		{Name: "replace", Action: "platform.replace_node", Approval: "required"},
	}}
	engine, err := playbook.NewEngine(map[string]*playbook.Playbook{book.Name: book}, []playbook.Policy{{Name: autoPolicy,
		Class: types.ClassFellOffBus, Playbook: book.Name, Mode: recoverymode.Automatic, Automatic: autoSettings(),
		ReplacementStrategy: recoverymode.RemoveFirst}})
	if err != nil {
		t.Fatal(err)
	}
	c.SetEngine(engine)
	second := openOn(t, c, successorNodeName)
	inc := waitSettled(t, c, st, second, func(inc *types.Incident) bool {
		return inc.State == types.StateAwaitingApproval || inc.State == types.StateVerifying || inc.State.Halted()
	})
	entries := trail(t, st, second)
	if inc.State == types.StateAwaitingApproval || auditHas(entries, "authorise", "circuit breaker") || len(notifier.approvals) != 0 {
		t.Fatalf("B: state=%s, want the policy to authorise it%s", inc.State, dump(entries))
	}
	if _, _, _, plain, _ := p.counts(); plain != 1 {
		t.Fatalf("B: %d remove-first replacements, want 1%s", plain, dump(entries))
	}
}

// An add-first successor is NotReady for a while after its node object
// appears. A is authorised before it is requested and waits for it inside
// the step, then resolves; the breaker does not trip.
func TestAddFirstSuccessorJoiningNotReady(t *testing.T) {
	p := &replacingCluster{successorPlatform: newSuccessorPlatform(), joining: 5}
	c, st, notifier := successorFixture(t, recoverymode.Automatic, p.successorPlatform, "")
	c.platform = p
	registerSuccessorAgent(t, st, 8)
	registerOldAgent(t, st, 8)
	id := openOn(t, c, "n1")
	inc := waitSettled(t, c, st, id, func(inc *types.Incident) bool { return inc.State.Halted() })
	entries := trail(t, st, id)
	if inc.State != types.StateResolved || len(notifier.approvals) != 0 || !auditHas(entries, "successor-waiting", "is not Ready") {
		t.Fatalf("state=%s, want RESOLVED after the successor waited NotReady%s", inc.State, dump(entries))
	}
}

// Another fault in the pool while a successor joins: the successor an open
// add-first replacement has not accepted, and a node KubeNeuron has no record
// of, are not counted, NotReady as they are. An accepted successor that is
// NotReady is.
func TestBreakerDoesNotCountAJoiningNode(t *testing.T) {
	now := time.Now()
	for _, tc := range []struct {
		name      string
		recorded  bool
		successor *types.SuccessorRecord
		trips     bool
	}{
		{name: "successor not accepted", recorded: true,
			successor: &types.SuccessorRecord{Successor: cloud.Successor{Operation: "replacing-n1-a0"}, Node: "n9", NodeUID: "uid-n9"}},
		{name: "node not recorded"},
		{name: "successor accepted", recorded: true, trips: true,
			successor: &types.SuccessorRecord{Successor: cloud.Successor{Operation: "replacing-n1-a0"}, Node: "n9", NodeUID: "uid-n9", ReadyAt: &now}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, st, _ := autoFixture(t, recoverymode.Automatic, autoSettings(), 4)
			c.SetTimings(time.Hour, 0) // A stays verifying
			p := clusterOf(trainNode("n1", "uid-n1"), trainNode("n2", "uid-n2"), trainNode("n3", "uid-n3"), trainNode("n4", "uid-n4"), trainNode("n9", "uid-n9"))
			p.notReady = map[string]bool{"n9": true}
			c.platform = p
			if tc.recorded {
				registerAgentWithLabels(t, st, "n9", "uid-n9", nil)
			}
			authorisedOpen(t, st, "replacing-n1", "n1", &cloud.NodeIdentity{Node: "n1", NodeUID: "uid-n1"})
			if tc.successor != nil {
				inc := mustIncident(t, st, "replacing-n1")
				inc.Successor = tc.successor
				if err := st.UpdateIncident(context.Background(), inc); err != nil {
					t.Fatal(err)
				}
			}
			// Two of the four nodes that are members have an open incident:
			// at 50%, not above it.
			id := openOn(t, c, "n3")
			inc := waitSettled(t, c, st, id, func(inc *types.Incident) bool {
				return inc.State == types.StateAwaitingApproval || inc.State == types.StateVerifying || inc.State.Halted()
			})
			if tripped := strings.Contains(parkReason(t, st, id), "circuit breaker: 3 of 5 nodes"); tripped != tc.trips || !tc.trips && inc.State != types.StateVerifying {
				t.Fatalf("state=%s audit=%s, want tripped=%v", inc.State, parkReason(t, st, id), tc.trips)
			}
		})
	}
}
