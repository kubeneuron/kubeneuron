// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/kubeneuron/kubeneuron/internal/detect"
	"github.com/kubeneuron/kubeneuron/internal/playbook"
	"github.com/kubeneuron/kubeneuron/internal/safety"
	"github.com/kubeneuron/kubeneuron/internal/store"
	"github.com/kubeneuron/kubeneuron/internal/store/sqlite"
	"github.com/kubeneuron/kubeneuron/pkg/cloud"
	"github.com/kubeneuron/kubeneuron/pkg/types"
)

// machines answers for the machine behind each node with one live read: a
// node without an entry has no Node, and err fails every read.
type machines struct {
	mmu  sync.Mutex
	uids map[string]string
	err  error
}

func (m *machines) CloudNode(_ context.Context, node string) (cloud.NodeRef, error) {
	m.mmu.Lock()
	defer m.mmu.Unlock()
	if m.err != nil {
		return cloud.NodeRef{}, m.err
	}
	uid, ok := m.uids[node]
	if !ok {
		return cloud.NodeRef{}, fmt.Errorf("nodes %q not found", node)
	}
	return cloud.NodeRef{Identity: machineOf(node, uid)}, nil
}

func machineOf(node, uid string) cloud.NodeIdentity {
	return cloud.NodeIdentity{Node: node, NodeUID: uid, ProviderID: "aws:///az/i-" + uid, InstanceID: "i-" + uid, BootID: "boot-" + uid}
}

type replacedNodePlatform struct {
	basePlatform
	*machines
}

var replacedAt = time.Date(2026, 11, 25, 4, 32, 0, 0, time.UTC)

// replacing is an incident that replaced node-a, the machine uid-a, in
// state: add-first, its successor recorded with outcome.
func replacing(state types.IncidentState, outcome string) *types.Incident {
	pin := machineOf("node-a", "uid-a")
	return &types.Incident{
		ID: "replacing", Target: types.Target{Node: "node-a"}, Class: types.ClassFellOffBus, State: state,
		Playbook: "cloud-replace", StepIndex: 2, Attempt: 1, SignalSeen: 1,
		OpenedAt: replacedAt, UpdatedAt: replacedAt, StateChangedAt: replacedAt, LastSignalAt: replacedAt,
		CloudIdentity: &pin,
		Successor: &types.SuccessorRecord{Successor: cloud.Successor{
			Operation: "op-1", Instance: pin.InstanceID, Phase: cloud.SuccessorCreated, ProviderID: "aws:///az/i-new",
		}, Outcome: outcome},
	}
}

// removingFirst is the same incident replacing node-a remove-first: a
// targeted delete (version 1) of target, or a GKE plan (version 0) of
// instance, in phase with outcome.
func removingFirst(state types.IncidentState, version int, phase cloud.RemoveFirstPhase, outcome string, target cloud.NodeIdentity, instance string) *types.Incident {
	inc := replacing(state, "")
	inc.Successor = nil
	inc.RemoveFirst = &types.RemoveFirstRecord{
		RemoveFirstPlan: cloud.RemoveFirstPlan{Operation: "op-1", Instance: instance, Pool: "gpu", Phase: phase, PlannedAt: replacedAt},
		Version:         version, Outcome: outcome,
	}
	if version == 1 {
		inc.RemoveFirst.DeleteTarget = &target
	}
	return inc
}

func replacedNodeFixture(t *testing.T, inc *types.Incident) (*Controller, *sqlite.Store, *recordingNotifier, *machines) {
	t.Helper()
	st, err := sqlite.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	engine, err := playbook.NewEngine(map[string]*playbook.Playbook{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	m := &machines{uids: map[string]string{"node-a": "uid-a", "node-b": "uid-b"}}
	rec := &recordingNotifier{}
	c := New(st, st, engine, safety.NewGate(safety.Limits{MaxConcurrentRemediations: 8, MaxConcurrentReboots: 8}),
		safety.NewFlapDetector(1, 24*time.Hour), &replacedNodePlatform{machines: m}, nil, rec,
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := st.CreateIncident(context.Background(), inc); err != nil {
		t.Fatal(err)
	}
	return c, st, rec, m
}

// agentDown is what vmalert sends for node while its agent does not answer.
func agentDown(node string) types.Signal {
	return types.Signal{
		Target: types.Target{Node: node}, Class: types.ClassAgentDown, Severity: types.SeverityWarning,
		Source: types.SourceAlertmanager, ObservedAt: time.Now(),
		Evidence: map[string]string{"alertname": "KubeNeuronAgentDown"},
	}
}

func replacedNodeSignals(t *testing.T, st store.Store, id string) []*types.AuditEntry {
	t.Helper()
	trail, err := st.AuditTrail(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	var out []*types.AuditEntry
	for _, e := range trail {
		if e.Action == replacedNodeSignalAction {
			out = append(out, e)
		}
	}
	return out
}

func incidentsOn(t *testing.T, st store.Store) []*types.Incident {
	t.Helper()
	all, err := st.ListIncidents(context.Background(), store.IncidentFilter{})
	if err != nil {
		t.Fatal(err)
	}
	return all
}

// The EKS T4 run on a398c516: after each replacement removed the machine,
// vmalert reported agent-down for the Node the cloud had not deleted yet.
// Each repeat of it, and each further fault of that machine, is recorded once
// on the incident that removed it; nothing opens and nobody is notified.
func TestSignalOfARemovedMachineIsRecordedOnItsReplacement(t *testing.T) {
	cases := map[string]*types.Incident{
		"add-first":             replacing(types.StateVerifying, types.SuccessorReplaced),
		"add-first, executing":  replacing(types.StateExecuting, types.SuccessorReplaced),
		"targeted delete":       removingFirst(types.StateVerifying, 1, cloud.RemoveFirstDone, types.RemoveFirstDeleteConfirmed, machineOf("node-a", "uid-a"), "i-uid-a"),
		"GKE plan, restored":    removingFirst(types.StateVerifying, 0, cloud.RemoveFirstDone, types.RemoveFirstRestored, cloud.NodeIdentity{}, "i-uid-a"),
		"GKE plan, resizing":    removingFirst(types.StateExecuting, 0, cloud.RemoveFirstResizing, "", cloud.NodeIdentity{}, "i-uid-a"),
		"after an escalation":   replacing(types.StateEvaluating, types.SuccessorReplaced),
		"awaiting the next one": replacing(types.StateAwaitingApproval, types.SuccessorReplaced),
	}
	for name, inc := range cases {
		t.Run(name, func(t *testing.T) {
			c, st, rec, _ := replacedNodeFixture(t, inc)
			ctx := context.Background()
			before, err := st.GetIncident(ctx, inc.ID)
			if err != nil {
				t.Fatal(err)
			}
			xid := signal(types.ClassDriverHang, "node-a", "GPU-1")
			for _, sig := range []types.Signal{agentDown("node-a"), agentDown("node-a"), xid, xid} {
				if err := c.ingest(ctx, sig); err != nil {
					t.Fatal(err)
				}
			}

			if all := incidentsOn(t, st); len(all) != 1 {
				t.Fatalf("%d incidents, want only the replacement: %+v", len(all), all[1:])
			}
			if len(rec.events) != 0 {
				t.Fatalf("notified %+v, want nothing", rec.events)
			}
			got := replacedNodeSignals(t, st, inc.ID)
			if len(got) != 2 {
				t.Fatalf("%d recorded signals, want agent-down and the device fault once each: %+v", len(got), got)
			}
			down, fault := got[0], got[1]
			if down.Params["signal_class"] != "agent-down" || down.Params["signal_source"] != "alertmanager" ||
				down.Params["signal_node_uid"] != "uid-a" || down.Params["alertname"] != "KubeNeuronAgentDown" ||
				down.Actor != "system" || down.FromState != inc.State || down.ToState != inc.State {
				t.Errorf("agent-down recorded as %+v", down)
			}
			if want := "agent-down was reported for node-a after this incident removed its machine. It is recorded here and no separate incident was opened"; down.Result != want {
				t.Errorf("result %q, want %q", down.Result, want)
			}
			if fault.Params["signal_class"] != string(types.ClassDriverHang) || fault.Params["signal_gpu_uuid"] != "GPU-1" || fault.Params["xid"] != "79" {
				t.Errorf("device fault recorded as %+v", fault)
			}

			// Only the audit trail changed: the verification window and its
			// recurrence check read these fields, and so does retention.
			after, err := st.GetIncident(ctx, inc.ID)
			if err != nil {
				t.Fatal(err)
			}
			if after.Version != before.Version || after.SignalSeen != before.SignalSeen ||
				!after.UpdatedAt.Equal(before.UpdatedAt) || !after.LastSignalAt.Equal(before.LastSignalAt) ||
				!after.StateChangedAt.Equal(before.StateChangedAt) || after.State != before.State {
				t.Errorf("the replacement changed from %+v to %+v", before, after)
			}
		})
	}
}

// A signal opens its incident as before unless the incident on its node has
// removed that very machine and is still being worked on.
func TestSignalOpensAsBeforeUnlessItsMachineWasRemoved(t *testing.T) {
	pin := machineOf("node-a", "uid-a")
	cases := []struct {
		name    string
		inc     *types.Incident
		uids    map[string]string
		readErr error
		sig     types.Signal
	}{
		{name: "the replacement waits for a person", inc: replacing(types.StateNeedsHuman, types.SuccessorReplaced)},
		{name: "the replacement ended", inc: replacing(types.StateResolved, types.SuccessorReplaced)},
		{name: "the successor is not in yet", inc: replacing(types.StateExecuting, "")},
		{name: "the successor was released", inc: replacing(types.StateEvaluating, types.SuccessorReleased)},
		{name: "no successor was created", inc: replacing(types.StateEvaluating, types.SuccessorNotCreated)},
		{name: "no capacity for a successor", inc: replacing(types.StateEvaluating, types.SuccessorCapacityDenied)},
		{name: "the successor needs an operator", inc: replacing(types.StateExecuting, types.SuccessorHandedOver)},
		{name: "the delete is being sent", inc: removingFirst(types.StateExecuting, 1, cloud.RemoveFirstDeleting, "", pin, "i-uid-a")},
		{name: "the delete was refused", inc: removingFirst(types.StateEvaluating, 1, cloud.RemoveFirstDeleting, types.RemoveFirstNotStarted, pin, "i-uid-a")},
		{name: "the delete needs an operator", inc: removingFirst(types.StateExecuting, 1, cloud.RemoveFirstDeleting, types.RemoveFirstNeedsOperator, pin, "i-uid-a")},
		{name: "another machine was deleted", inc: removingFirst(types.StateVerifying, 1, cloud.RemoveFirstDone, types.RemoveFirstDeleteConfirmed, machineOf("node-a", "uid-other"), "i-uid-other")},
		{name: "the GKE plan is only planned", inc: removingFirst(types.StateExecuting, 0, cloud.RemoveFirstPlanned, "", cloud.NodeIdentity{}, "i-uid-a")},
		{name: "the GKE plan is deleting", inc: removingFirst(types.StateExecuting, 0, cloud.RemoveFirstDeleting, "", cloud.NodeIdentity{}, "i-uid-a")},
		{name: "the GKE plan did not start", inc: removingFirst(types.StateEvaluating, 0, cloud.RemoveFirstPlanned, types.RemoveFirstNotStarted, cloud.NodeIdentity{}, "i-uid-a")},
		{name: "the GKE plan removed another instance", inc: removingFirst(types.StateVerifying, 0, cloud.RemoveFirstDone, types.RemoveFirstRestored, cloud.NodeIdentity{}, "i-uid-other")},
		{name: "a new machine has the name", inc: replacing(types.StateVerifying, types.SuccessorReplaced), uids: map[string]string{"node-a": "uid-new"}},
		{name: "the Node is gone", inc: replacing(types.StateVerifying, types.SuccessorReplaced), uids: map[string]string{}},
		{name: "the Node cannot be read", inc: replacing(types.StateVerifying, types.SuccessorReplaced), readErr: errors.New("context deadline exceeded")},
		{name: "the cloud deleted the Node", inc: replacing(types.StateVerifying, types.SuccessorReplaced), uids: map[string]string{}},
		{name: "another node", inc: replacing(types.StateVerifying, types.SuccessorReplaced), sig: agentDown("node-b")},
		{name: "a person raised it", inc: replacing(types.StateVerifying, types.SuccessorReplaced), sig: types.Signal{
			Target: types.Target{Node: "node-a"}, Class: types.ClassAgentDown, Severity: types.SeverityWarning, Source: types.SourceManual}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, st, rec, m := replacedNodeFixture(t, tc.inc)
			if tc.uids != nil {
				m.uids = tc.uids
			}
			m.err = tc.readErr
			sig := tc.sig
			if sig.Class == "" {
				sig = agentDown("node-a")
			}
			ctx := context.Background()
			if err := c.ingest(ctx, sig); err != nil {
				t.Fatal(err)
			}
			opened, err := st.GetOpenIncident(ctx, sig.Target, sig.Class)
			if err != nil {
				t.Fatalf("no incident opened for %s on %s: %v", sig.Class, sig.Target.Node, err)
			}
			if got := replacedNodeSignals(t, st, tc.inc.ID); len(got) != 0 {
				t.Errorf("recorded on the replacement: %+v", got)
			}
			if !rec.hasMessage("incident opened: agent-down on " + sig.Target.Node) {
				t.Errorf("no notification for %s, got %+v", opened.ID, rec.events)
			}
		})
	}
}

// accelerator-unallocatable comes from node health once allocatable stayed
// below capacity for five minutes. On the Node of a removed machine it is
// recorded on the replacement, and the reads after it add nothing.
func TestUnallocatableOfARemovedMachineIsRecordedOnItsReplacement(t *testing.T) {
	c, st, plat, clock := healthFixture(t)
	ctx := context.Background()
	m := &machines{uids: map[string]string{"trn-1": "uid-trn-1"}}
	c.platform = struct {
		*healthPlatform
		*machines
	}{plat, m}
	pin := machineOf("trn-1", "uid-trn-1")
	inc := replacing(types.StateVerifying, types.SuccessorReplaced)
	inc.Target.Node, inc.CloudIdentity, inc.Successor.Instance = "trn-1", &pin, pin.InstanceID
	if err := st.CreateIncident(ctx, inc); err != nil {
		t.Fatal(err)
	}

	node := neuronNode(clock, "False", "")
	node.Devices[0].Allocatable = 0
	for range 40 {
		node.Conditions[0].LastHeartbeatTime = clock.Now()
		plat.set(node)
		c.pollNodeHealth(ctx)
		clock.Advance(10 * time.Second)
	}

	if got := openHealthIncident(t, st, types.ClassAcceleratorUnallocatable); got != nil {
		t.Fatalf("opened %+v for the removed machine", got)
	}
	got := replacedNodeSignals(t, st, inc.ID)
	if len(got) != 1 || got[0].Params["signal_class"] != string(types.ClassAcceleratorUnallocatable) ||
		got[0].Params["signal_source"] != string(types.SourceNodeHealth) || got[0].Params["signal_node_uid"] != "uid-trn-1" {
		t.Fatalf("recorded %+v, want accelerator-unallocatable once", got)
	}

	// The replacement is handed to a person: the next read opens it as before.
	inc, err := st.GetIncident(ctx, inc.ID)
	if err != nil {
		t.Fatal(err)
	}
	inc.State = types.StateNeedsHuman
	if err := st.UpdateIncident(ctx, inc); err != nil {
		t.Fatal(err)
	}
	node.Conditions[0].LastHeartbeatTime = clock.Now()
	plat.set(node)
	c.pollNodeHealth(ctx)
	if got := openHealthIncident(t, st, types.ClassAcceleratorUnallocatable); got == nil {
		t.Fatal("the next read opened nothing once the replacement waited for a person")
	}
}

// A recorded signal is not an incident: the flap detector, the circuit
// breaker and the policy limits, which count incidents and the steps they
// start, do not see it.
func TestRecordedSignalCountsTowardNoLimit(t *testing.T) {
	inc := replacing(types.StateExecuting, types.SuccessorReplaced)
	c, st, _, _ := replacedNodeFixture(t, inc)
	ctx := context.Background()
	for range 3 {
		if err := c.ingest(ctx, agentDown("node-a")); err != nil {
			t.Fatal(err)
		}
	}
	if len(replacedNodeSignals(t, st, inc.ID)) != 1 {
		t.Fatal("agent-down was not recorded on the replacement")
	}

	// With a threshold of one, a single resolve and reopen of the pair would
	// flap: none was recorded.
	if c.flap.RecordReopen(types.Target{Node: "node-a"}, types.ClassAgentDown) {
		t.Error("the flap detector counted the recorded signal")
	}
	open, err := c.openIncidents(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(open) != 1 || open[0].ID != inc.ID {
		t.Errorf("the breaker and the concurrency limit see %+v, want only the replacement", open)
	}
	// The cool-down and the concurrency limit count entries into EXECUTING;
	// the record leaves the state as it is, even while executing.
	if started, err := c.authorisedBy(ctx, inc.ID, "system", time.Time{}); err != nil || started {
		t.Errorf("the record counted as a started step: %v, %v", started, err)
	}
}

// verifyingFor has inc verifying its replacement since d ago, inside the
// quiet window.
func verifyingFor(inc *types.Incident, d time.Duration) *types.Incident {
	since := time.Now().Add(-d)
	inc.UpdatedAt, inc.StateChangedAt, inc.LastSignalAt = since, since, since
	return inc
}

// eccAlert is what vmalert sends for a double-bit ECC error of gpu on node.
func eccAlert(node, gpu string) types.Signal {
	sig, _ := detect.SignalFromAlert(detect.Alert{Status: "firing", StartsAt: time.Now(),
		Labels: map[string]string{"alertname": "GpuDoubleBitEccVolatile", "node": node, "UUID": gpu, "gpu": "0"}})
	return sig
}

// recurrence is the cause recorded when verification failed on a signal.
const recurrence = "verification failed: signal recurred during quiet window"

// While the incident verifies the replacement, the machine it removed reports
// the incident's own fault again: its agent until the machine stops, vmalert
// from the exporter's last samples. That is not the fault coming back on the
// node that took its place.
func TestFaultOfTheRemovedMachineIsNoRecurrence(t *testing.T) {
	removals := map[string]func() *types.Incident{
		"add-first": func() *types.Incident { return replacing(types.StateVerifying, types.SuccessorReplaced) },
		"targeted delete": func() *types.Incident {
			return removingFirst(types.StateVerifying, 1, cloud.RemoveFirstDone, types.RemoveFirstDeleteConfirmed, machineOf("node-a", "uid-a"), "i-uid-a")
		},
		"GKE plan": func() *types.Incident {
			return removingFirst(types.StateVerifying, 0, cloud.RemoveFirstDone, types.RemoveFirstRestored, cloud.NodeIdentity{}, "i-uid-a")
		},
	}
	sources := map[string]struct {
		class types.ProblemClass
		sig   types.Signal
	}{
		"agent":   {types.ClassFellOffBus, signal(types.ClassFellOffBus, "node-a", "GPU-1")},
		"vmalert": {types.ClassECCDBE, eccAlert("node-a", "GPU-1")},
	}
	for removal, incident := range removals {
		for source, s := range sources {
			t.Run(removal+", "+source, func(t *testing.T) {
				inc := verifyingFor(incident(), time.Minute)
				inc.Target, inc.Class = types.Target{Node: "node-a", GPUUUID: "GPU-1"}, s.class
				c, st, rec, _ := replacedNodeFixture(t, inc)
				ctx := context.Background()
				for range 2 {
					if err := c.ingest(ctx, s.sig); err != nil {
						t.Fatal(err)
					}
				}
				if err := c.advance(ctx, mustIncident(t, st, inc.ID)); err != nil {
					t.Fatal(err)
				}

				after := mustIncident(t, st, inc.ID)
				if after.State != types.StateVerifying || !after.LastSignalAt.Equal(inc.LastSignalAt) || after.SignalSeen != inc.SignalSeen {
					t.Fatalf("state %s, signals %d, last signal moved %v: the removed machine's fault counted as a recurrence%s",
						after.State, after.SignalSeen, !after.LastSignalAt.Equal(inc.LastSignalAt), dump(trail(t, st, inc.ID)))
				}
				if len(rec.events) != 0 {
					t.Errorf("notified %+v, want nothing", rec.events)
				}
				got := replacedNodeSignals(t, st, inc.ID)
				if len(got) != 1 || got[0].Params["signal_class"] != string(s.class) || got[0].Params["signal_node_uid"] != "uid-a" ||
					got[0].Params["signal_source"] != string(s.sig.Source) || got[0].Params["signal_gpu_uuid"] != "GPU-1" {
					t.Fatalf("recorded %+v, want the fault once", got)
				}
				if want := string(s.class) + " was reported again for node-a after this incident removed its machine. It is recorded here and does not count as a recurrence"; got[0].Result != want {
					t.Errorf("result %q, want %q", got[0].Result, want)
				}
			})
		}
	}
}

// accelerator-fault from node health goes to replacement in the cloud policy
// pack. Node health reports a fault again while its incident verifies, and
// the removed machine's Node keeps the condition, fresh for ten minutes after
// its last heartbeat, until the cloud deletes the Node.
func TestNodeHealthOfTheRemovedMachineIsNoRecurrence(t *testing.T) {
	c, st, plat, clock := healthFixture(t)
	ctx := context.Background()
	c.platform = struct {
		*healthPlatform
		*machines
	}{plat, &machines{uids: map[string]string{"trn-1": "uid-trn-1"}}}
	pin := machineOf("trn-1", "uid-trn-1")
	inc := verifyingFor(replacing(types.StateVerifying, types.SuccessorReplaced), time.Minute)
	inc.Target.Node, inc.Class, inc.CloudIdentity, inc.Successor.Instance = "trn-1", types.ClassAcceleratorFault, &pin, pin.InstanceID
	if err := st.CreateIncident(ctx, inc); err != nil {
		t.Fatal(err)
	}

	plat.set(neuronNode(clock, "True", neuronDMA))
	for range 6 {
		c.pollNodeHealth(ctx)
		clock.Advance(10 * time.Second)
	}
	current, err := st.GetIncident(ctx, inc.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.advance(ctx, current); err != nil {
		t.Fatal(err)
	}

	after, err := st.GetIncident(ctx, inc.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.State != types.StateVerifying || !after.LastSignalAt.Equal(inc.LastSignalAt) {
		trail, _ := st.AuditTrail(ctx, inc.ID)
		t.Fatalf("state %s, last signal moved %v: the removed Node's fault counted as a recurrence%s",
			after.State, !after.LastSignalAt.Equal(inc.LastSignalAt), dump(trail))
	}
	got := replacedNodeSignals(t, st, inc.ID)
	if len(got) != 1 || got[0].Params["signal_class"] != string(types.ClassAcceleratorFault) ||
		got[0].Params["signal_source"] != string(types.SourceNodeHealth) || got[0].Params["signal_node_uid"] != "uid-trn-1" {
		t.Fatalf("recorded %+v, want accelerator-fault once", got)
	}
}

// The fault the incident verifies against is a recurrence whenever it may
// not be the removed machine's: from a new machine under the name, from a
// person, from a Node that could not be read or is no longer there, or
// before any machine was removed.
func TestFaultThatMayNotBeTheRemovedMachinesIsARecurrence(t *testing.T) {
	cases := []struct {
		name    string
		inc     *types.Incident
		uids    map[string]string
		readErr error
		sig     types.Signal
	}{
		{name: "a new machine has the name", inc: replacing(types.StateVerifying, types.SuccessorReplaced), uids: map[string]string{"node-a": "uid-new"}},
		{name: "a new machine after the delete", inc: removingFirst(types.StateVerifying, 1, cloud.RemoveFirstDone, types.RemoveFirstDeleteConfirmed, machineOf("node-a", "uid-a"), "i-uid-a"), uids: map[string]string{"node-a": "uid-new"}},
		{name: "the Node cannot be read", inc: replacing(types.StateVerifying, types.SuccessorReplaced), readErr: errors.New("context deadline exceeded")},
		{name: "a person raised it", inc: replacing(types.StateVerifying, types.SuccessorReplaced), sig: types.Signal{
			Target: types.Target{Node: "node-a", GPUUUID: "GPU-1"}, Class: types.ClassFellOffBus, Severity: types.SeverityCritical, Source: types.SourceManual}},
		{name: "no machine was removed", inc: replacing(types.StateVerifying, "")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			inc := verifyingFor(tc.inc, time.Minute)
			inc.Target = types.Target{Node: "node-a", GPUUUID: "GPU-1"}
			c, st, _, m := replacedNodeFixture(t, inc)
			if tc.uids != nil {
				m.uids = tc.uids
			}
			m.err = tc.readErr
			sig := tc.sig
			if sig.Class == "" {
				sig = signal(types.ClassFellOffBus, "node-a", "GPU-1")
			}
			ctx := context.Background()
			if err := c.ingest(ctx, sig); err != nil {
				t.Fatal(err)
			}
			if err := c.advance(ctx, mustIncident(t, st, inc.ID)); err != nil {
				t.Fatal(err)
			}
			after := mustIncident(t, st, inc.ID)
			if entries := trail(t, st, inc.ID); after.State == types.StateVerifying || !auditHasParam(entries, "quarantine", "cause", recurrence) {
				t.Fatalf("state %s, want the verification failed on the recurrence%s", after.State, dump(entries))
			}
			if got := replacedNodeSignals(t, st, inc.ID); len(got) != 0 {
				t.Errorf("recorded as the removed machine's: %+v", got)
			}
		})
	}
}
