package controller

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/kubeneuron/kubeneuron/internal/config"
	"github.com/kubeneuron/kubeneuron/internal/notify"
	"github.com/kubeneuron/kubeneuron/internal/playbook"
	"github.com/kubeneuron/kubeneuron/internal/safety"
	"github.com/kubeneuron/kubeneuron/internal/store"
	"github.com/kubeneuron/kubeneuron/internal/store/sqlite"
	"github.com/kubeneuron/kubeneuron/pkg/types"
)

// newIngestTestController wires a controller against the real shipped
// policies/playbooks and an in-memory store.
func newIngestTestController(t *testing.T) (*Controller, *sqlite.Store) {
	t.Helper()
	st, err := sqlite.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })

	books, err := playbook.LoadDir("../../configs/playbooks")
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load("../../configs/policies.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var policies []playbook.Policy
	for _, p := range cfg.Policies {
		policies = append(policies, playbook.Policy{Class: p.Match.Class, Playbook: p.Playbook, Params: p.Params})
	}
	engine, err := playbook.NewEngine(books, policies)
	if err != nil {
		t.Fatal(err)
	}
	gate := safety.NewGate(safety.Limits{MaxConcurrentRemediations: 2, MaxConcurrentReboots: 1, DryRun: true})
	c := New(st, st, engine, gate, safety.NewFlapDetector(3, 24*time.Hour), nil, nil,
		&notify.Log{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))},
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	return c, st
}

func signal(class types.ProblemClass, node, gpu string) types.Signal {
	return types.Signal{
		Target:     types.Target{Node: node, GPUUUID: gpu},
		Class:      class,
		Severity:   types.SeverityCritical,
		Source:     types.SourceAgentEvent,
		Evidence:   map[string]string{"xid": "79"},
		ObservedAt: time.Now(),
	}
}

func TestIngestOpensIncidentWithPlaybookAndAudit(t *testing.T) {
	c, st := newIngestTestController(t)
	ctx := context.Background()

	if err := c.ingest(ctx, signal(types.ClassFellOffBus, "n1", "GPU-1")); err != nil {
		t.Fatal(err)
	}
	incidents, err := st.ListIncidents(ctx, store.IncidentFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(incidents) != 1 {
		t.Fatalf("incidents = %d, want 1", len(incidents))
	}
	inc := incidents[0]
	if inc.State != types.StateOpen || inc.Playbook != "fell-off-bus" || !inc.DryRun {
		t.Fatalf("incident = %+v, want OPEN with fell-off-bus playbook in dry-run", inc)
	}
	trail, err := st.AuditTrail(ctx, inc.ID)
	if err != nil || len(trail) != 1 || trail[0].Action != "open" {
		t.Fatalf("audit trail = %+v, %v; want one open entry", trail, err)
	}
}

func TestIngestDeduplicatesToOneOpenIncident(t *testing.T) {
	c, st := newIngestTestController(t)
	ctx := context.Background()

	sig := signal(types.ClassFellOffBus, "n1", "GPU-1")
	for i := 0; i < 3; i++ {
		if err := c.ingest(ctx, sig); err != nil {
			t.Fatal(err)
		}
	}
	incidents, err := st.ListIncidents(ctx, store.IncidentFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(incidents) != 1 {
		t.Fatalf("incidents = %d, want 1 (dedup by target+class)", len(incidents))
	}
	if incidents[0].SignalSeen != 3 {
		t.Fatalf("SignalSeen = %d, want 3", incidents[0].SignalSeen)
	}
	if !incidents[0].StateChangedAt.Equal(incidents[0].OpenedAt) {
		t.Fatal("duplicate signals must not move StateChangedAt")
	}
}

func TestIngestSeparatesTargetsAndClasses(t *testing.T) {
	c, st := newIngestTestController(t)
	ctx := context.Background()

	for _, sig := range []types.Signal{
		signal(types.ClassFellOffBus, "n1", "GPU-1"),
		signal(types.ClassFellOffBus, "n1", "GPU-2"), // other GPU
		signal(types.ClassECCDBE, "n1", "GPU-1"),     // other class
		signal(types.ClassFellOffBus, "n2", "GPU-1"), // other node
	} {
		if err := c.ingest(ctx, sig); err != nil {
			t.Fatal(err)
		}
	}
	incidents, err := st.ListIncidents(ctx, store.IncidentFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(incidents) != 4 {
		t.Fatalf("incidents = %d, want 4 distinct (target, class) pairs", len(incidents))
	}
}

func TestIngestWithoutPolicyOpensObserveOnlyIncident(t *testing.T) {
	c, st := newIngestTestController(t)
	ctx := context.Background()

	// agent-down has no policy binding in the shipped config.
	if err := c.ingest(ctx, signal(types.ClassAgentDown, "n1", "")); err != nil {
		t.Fatal(err)
	}
	incidents, err := st.ListIncidents(ctx, store.IncidentFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(incidents) != 1 || incidents[0].Playbook != "" {
		t.Fatalf("incidents = %+v, want one observe-only incident without playbook", incidents)
	}
}
