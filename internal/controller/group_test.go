// SPDX-License-Identifier: LicenseRef-KubeNeuron

package controller

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/kubeneuron/kubeneuron/internal/checkpoint"
	"github.com/kubeneuron/kubeneuron/internal/metrics"
	"github.com/kubeneuron/kubeneuron/internal/platform"
)

func grouped(name, ns, node, group string) platform.Workload {
	return platform.Workload{
		Name: name, Namespace: ns, Kind: "Pod", UsesGPU: true, UID: "uid-" + name, Node: node,
		Annotations: map[string]string{checkpoint.AnnotationOptIn: checkpoint.OptInValue, checkpoint.AnnotationGroup: group},
	}
}

func groupPolicy(crossNode bool, maxSize int) checkpoint.Policy {
	p := enabledPolicy()
	p.Groups = checkpoint.GroupPolicy{Enabled: true, MaxGroupSize: maxSize, CrossNode: crossNode}
	return p
}

func groupCounts() map[checkpoint.Outcome]float64 {
	out := map[checkpoint.Outcome]float64{}
	for _, o := range append(checkpointOutcomes, checkpoint.OutcomeSkipped) {
		out[o] = testutil.ToFloat64(metrics.CheckpointGroupRequests.WithLabelValues(string(o)))
	}
	return out
}

// groupFixture: rank-0 and rank-1 on n1 (the disrupted node), rank-2 and
// rank-3 on n2, all in group run-42; the drain on n1 touches the two local
// ranks.
func groupFixture(t *testing.T, policy checkpoint.Policy) (*Controller, *checkpointPlatform, *fakeClock, func()) {
	t.Helper()
	local := []platform.Workload{grouped("rank-0", "training", "n1", "run-42"), grouped("rank-1", "training", "n1", "run-42")}
	remote := []platform.Workload{grouped("rank-2", "training", "n2", "run-42"), grouped("rank-3", "training", "n2", "run-42")}
	plat := &checkpointPlatform{basePlatform: basePlatform{workloads: local}, members: append(append([]platform.Workload{}, local...), remote...)}
	// Every request is acknowledged on the first look, so the step settles
	// without waiting; what matters here is who was asked.
	plat.observe = func(w platform.Workload, _ int) checkpoint.Observation {
		return stampedAck(w, fixtureIncidentID, time.Now().Add(time.Minute))
	}
	c, inc, clock := checkpointFixture(t, plat, policy)
	return c, plat, clock, func() { runDrain(t, c, inc) }
}

func requests(plat *checkpointPlatform) []string {
	var out []string
	for _, call := range plat.Calls() {
		if strings.HasPrefix(call, "request ") {
			out = append(out, strings.TrimPrefix(call, "request "))
		}
	}
	return out
}

// crossNode off: the group is the members on the disrupted node, which the
// step asks anyway; peers elsewhere are not asked and are counted skipped.
func TestGroupPeersOnOtherNodesAreNotAskedWhenCrossNodeIsOff(t *testing.T) {
	c, plat, _, drain := groupFixture(t, groupPolicy(false, 64))
	before := groupCounts()
	drain()
	got := requests(plat)
	if len(got) != 2 || got[0] != "training/rank-0@n1" || got[1] != "training/rank-1@n1" {
		t.Fatalf("requests = %v, want only the two node-local ranks", got)
	}
	after := groupCounts()
	if after[checkpoint.OutcomeSkipped]-before[checkpoint.OutcomeSkipped] != 2 {
		t.Fatalf("skipped peers moved by %v, want 2", after[checkpoint.OutcomeSkipped]-before[checkpoint.OutcomeSkipped])
	}
	inc, _ := c.store.GetIncident(context.Background(), fixtureIncidentID)
	requireAudit(t, auditResults(t, c, inc), "checkpoint group training/run-42: 4 member(s), 2 on n1, requesting 0 peer(s)")
	requireAudit(t, auditResults(t, c, inc), "2 on other node(s) not asked (crossNode off) training/rank-2, training/rank-3")
}

// crossNode on: peers on other nodes get the request, confined to their own
// node, and are waited for; the disruption still happens only on n1.
func TestGroupPeersAreRequestedOnTheirOwnNodeWhenCrossNodeIsOn(t *testing.T) {
	c, plat, _, drain := groupFixture(t, groupPolicy(true, 64))
	before := groupCounts()
	drain()
	got := requests(plat)
	if len(got) != 4 || got[2] != "training/rank-2@n2" || got[3] != "training/rank-3@n2" {
		t.Fatalf("requests = %v, want the two peers requested on n2 after the local ranks", got)
	}
	for _, call := range plat.Calls() {
		if strings.HasPrefix(call, "drain ") && call != "drain n1" {
			t.Fatalf("a drain reached another node: %v", plat.Calls())
		}
		if strings.HasPrefix(call, "evict ") {
			t.Fatalf("a peer was evicted: %v", plat.Calls())
		}
	}
	after := groupCounts()
	if after[checkpoint.OutcomeAcknowledged]-before[checkpoint.OutcomeAcknowledged] != 2 {
		t.Fatalf("peer acknowledgements moved by %v, want 2", after[checkpoint.OutcomeAcknowledged]-before[checkpoint.OutcomeAcknowledged])
	}
	inc, _ := c.store.GetIncident(context.Background(), fixtureIncidentID)
	results := auditResults(t, c, inc)
	requireAudit(t, results, "checkpoint group training/run-42: 4 member(s), 2 on n1, requesting 2 peer(s) training/rank-2, training/rank-3")
	requireAudit(t, results, "peer training/rank-2=acknowledged")
	// The step's own metric counts only the node's candidates.
	if got := testutil.ToFloat64(metrics.CheckpointRequests.WithLabelValues(string(checkpoint.OutcomeAcknowledged))); got < 2 {
		t.Fatalf("step acknowledgements = %v", got)
	}
}

// A group above maxGroupSize contributes no peers: the step asks only the
// node's members, counts the skip, and says so.
func TestGroupTooLargeIsSkippedNotPartiallyAsked(t *testing.T) {
	c, plat, _, drain := groupFixture(t, groupPolicy(true, 3))
	before := groupCounts()
	drain()
	if got := requests(plat); len(got) != 2 {
		t.Fatalf("requests = %v, want only the node-local ranks", got)
	}
	after := groupCounts()
	if after[checkpoint.OutcomeSkipped]-before[checkpoint.OutcomeSkipped] != 1 {
		t.Fatalf("skipped moved by %v, want 1 for the group", after[checkpoint.OutcomeSkipped]-before[checkpoint.OutcomeSkipped])
	}
	inc, _ := c.store.GetIncident(context.Background(), fixtureIncidentID)
	requireAudit(t, auditResults(t, c, inc), "checkpoint group training/run-42: skipped (group_too_large: more than 3 members); asking only the 2 member(s) on n1")
}

// A peer's deadline is the same formula as a candidate's, from the same
// request time: never later than min(maxWait, budget) after it, whatever the
// peer requested.
func TestGroupPeerDeadlineNeverExtendsTheWindow(t *testing.T) {
	c, plat, clock, drain := groupFixture(t, groupPolicy(true, 64))
	plat.mu.Lock()
	for i := range plat.members {
		if plat.members[i].Name == "rank-3" {
			plat.members[i].Annotations[checkpoint.AnnotationMaxWait] = "24h"
		}
	}
	plat.mu.Unlock()
	var deadlines = map[string]time.Time{}
	plat.request = func(w platform.Workload, req checkpoint.Request, _ int) (checkpoint.Request, error) {
		deadlines[w.Name] = req.DeadlineAt
		return req, nil
	}
	start := clock.Now()
	drain()
	policy := groupPolicy(true, 64)
	for name, deadline := range deadlines {
		if deadline.Sub(start) > policy.MaxWait {
			t.Fatalf("%s deadline %v after the request time exceeds maxWait %v", name, deadline.Sub(start), policy.MaxWait)
		}
	}
	if d, ok := deadlines["rank-3"]; !ok || d.Sub(start) != policy.MaxWait {
		t.Fatalf("rank-3 asked for 24h and got %v, want exactly maxWait", d.Sub(start))
	}
	_ = c
}

// With groups off nothing is listed and nothing beyond the node is asked.
func TestGroupsOffListsNothing(t *testing.T) {
	_, plat, _, drain := groupFixture(t, enabledPolicy())
	drain()
	for _, call := range plat.Calls() {
		if strings.HasPrefix(call, "group ") {
			t.Fatalf("groups off still listed: %v", plat.Calls())
		}
	}
	if got := requests(plat); len(got) != 2 {
		t.Fatalf("requests = %v", got)
	}
}
