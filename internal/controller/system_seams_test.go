package controller

// System-level tests through the real wiring: HandleAgentEvent → durable
// archive + outbox → drainEventOutbox → classification of the row READ BACK
// from the store → incident. Round 5's critical finding (the fault envelope
// dropped on the outbox round trip) survived four review rounds because every
// test classified events in memory and never crossed this seam; these tests
// exist so that class of regression fails loudly.

import (
	"context"
	"io"
	"log/slog"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/kubeneuron/kubeneuron/internal/playbook"
	"github.com/kubeneuron/kubeneuron/internal/store"
	"github.com/kubeneuron/kubeneuron/pkg/types"
)

func pipelineEngine(t *testing.T) *playbook.Engine {
	t.Helper()
	books := map[string]*playbook.Playbook{
		"pb-ecc":  {Name: "pb-ecc", Target: "gpu", Steps: []playbook.Step{{Name: "observe", Action: "notify.observe"}}},
		"pb-lost": {Name: "pb-lost", Target: "node", Steps: []playbook.Step{{Name: "observe", Action: "notify.observe"}}},
	}
	engine, err := playbook.NewEngine(books, []playbook.Policy{
		{Class: types.ClassECCDBE, Playbook: "pb-ecc"},
		{Class: types.ClassFellOffBus, Playbook: "pb-lost"},
	})
	if err != nil {
		t.Fatal(err)
	}
	return engine
}

func neutralFaultEvent(id string) types.AgentEvent {
	return types.AgentEvent{
		EventID: id, Node: "n1", GPUIndex: 2, GPUUUID: "GPU-ecc", PCIAddr: "0000:65:00.0",
		XID: 0,
		Fault: &types.FaultSignal{
			Vendor: "nvidia", Source: "nvidia-smi", Code: "ecc-dbe",
			Attributes: map[string]string{"volatile_uncorrectable_ecc": "2"},
		},
		Raw: "nvidia-smi -q fallback", Timestamp: time.Now().UTC(),
	}
}

// Both detection sources — a native kmsg XID and the nvidia-smi/DCGM neutral
// fault — must open their incidents through the full durable pipeline, not
// through an in-memory shortcut. The controller acknowledges the event before
// classifying, so everything the classifier needs must survive the store.
func TestBothDetectionSourcesOpenIncidentsThroughTheDurablePipeline(t *testing.T) {
	c, st, _ := miniController(t, pipelineEngine(t))
	ctx := context.Background()

	xidEvent := types.AgentEvent{
		EventID: "ev-xid", Node: "n1", GPUIndex: 0, GPUUUID: "GPU-bus",
		XID: 79, Raw: "NVRM: Xid (PCI:0000:00:1e): 79", Timestamp: time.Now().UTC(),
	}
	for _, ev := range []types.AgentEvent{xidEvent, neutralFaultEvent("ev-fault")} {
		if err := c.HandleAgentEvent(httptest.NewRequest("POST", "/api/v1/events", nil), ev); err != nil {
			t.Fatalf("HandleAgentEvent(%s): %v", ev.EventID, err)
		}
	}

	// Acknowledged but not yet classified: the incidents must not exist yet —
	// classification happens on the durable item, not on the request path.
	if incidents, err := st.ListIncidents(ctx, store.IncidentFilter{}); err != nil || len(incidents) != 0 {
		t.Fatalf("before drain: incidents = %v, %v; want none (classification is post-archive)", incidents, err)
	}

	c.drainEventOutbox(ctx)

	incidents, err := st.ListIncidents(ctx, store.IncidentFilter{})
	if err != nil {
		t.Fatal(err)
	}
	byClass := map[types.ProblemClass]*types.Incident{}
	for _, inc := range incidents {
		byClass[inc.Class] = inc
	}
	if len(incidents) != 2 || byClass[types.ClassFellOffBus] == nil || byClass[types.ClassECCDBE] == nil {
		t.Fatalf("after drain: incidents = %+v, want one fell-off-bus and one ecc-dbe", incidents)
	}
	if got := byClass[types.ClassECCDBE].Playbook; got != "pb-ecc" {
		t.Fatalf("neutral-fault incident selected playbook %q, want pb-ecc — the fallback source must reach policy selection intact", got)
	}
	if got := byClass[types.ClassECCDBE].Target; got.GPUUUID != "GPU-ecc" {
		t.Fatalf("neutral-fault incident target = %+v, want the GPU carried through the store", got)
	}
}

// At-least-once delivery composed with the durable pipeline: a replayed event
// (same EventID) must not double-count, whether the replay lands before or
// after the original was drained.
func TestReplayedEventsCannotDoubleCountThroughThePipeline(t *testing.T) {
	c, st, _ := miniController(t, pipelineEngine(t))
	ctx := context.Background()

	ev := neutralFaultEvent("ev-dup")
	if err := c.HandleAgentEvent(httptest.NewRequest("POST", "/api/v1/events", nil), ev); err != nil {
		t.Fatal(err)
	}
	// Replay before the drain.
	if err := c.HandleAgentEvent(httptest.NewRequest("POST", "/api/v1/events", nil), ev); err != nil {
		t.Fatal(err)
	}
	c.drainEventOutbox(ctx)
	// Replay after the drain.
	if err := c.HandleAgentEvent(httptest.NewRequest("POST", "/api/v1/events", nil), ev); err != nil {
		t.Fatal(err)
	}
	c.drainEventOutbox(ctx)

	incidents, err := st.ListIncidents(ctx, store.IncidentFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(incidents) != 1 {
		t.Fatalf("incidents = %+v, want exactly one despite replays", incidents)
	}
	if incidents[0].SignalSeen != 1 {
		t.Fatalf("SignalSeen = %d, want 1 — a replayed EventID must not attach as a fresh signal", incidents[0].SignalSeen)
	}
}

// The acknowledgment contract across a controller death: a 2xx to the agent is
// safe BECAUSE the archived event survives the process. A new controller over
// the same store must classify and open the incident the dead one never got
// to — through nothing but the durable state.
func TestArchivedEventSurvivesControllerRestartAndStillOpensItsIncident(t *testing.T) {
	first, st, _ := miniController(t, pipelineEngine(t))
	ctx := context.Background()

	if err := first.HandleAgentEvent(httptest.NewRequest("POST", "/api/v1/events", nil), neutralFaultEvent("ev-orphan")); err != nil {
		t.Fatal(err)
	}
	// The first controller dies here: no drain ever runs on it.

	restarted := New(st, st, pipelineEngine(t), nil, nil, nil, nil, &recordingNotifier{},
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	restarted.drainEventOutbox(ctx)

	incidents, err := st.ListIncidents(ctx, store.IncidentFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(incidents) != 1 || incidents[0].Class != types.ClassECCDBE {
		t.Fatalf("incidents after restart = %+v, want the orphaned event's ecc-dbe incident", incidents)
	}
}
