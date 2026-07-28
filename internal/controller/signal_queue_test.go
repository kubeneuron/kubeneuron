package controller

import (
	"context"
	"io"
	"log/slog"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/kubeneuron/kubeneuron/internal/store"
	"github.com/kubeneuron/kubeneuron/internal/store/sqlite"
	"github.com/kubeneuron/kubeneuron/pkg/types"
)

func newQueueTestController() *Controller {
	return New(nil, nil, nil, nil, nil, nil, nil, nil,
		slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func fillSignals(c *Controller) int {
	n := 0
	for {
		select {
		case c.signals <- types.Signal{Class: types.ClassXIDApp}:
			n++
		default:
			return n
		}
	}
}

func TestHandleSignalCountsDrops(t *testing.T) {
	c := newQueueTestController()
	fillSignals(c)

	c.HandleSignal(nil, types.Signal{
		Class:    types.ClassXIDApp,
		Severity: types.SeverityWarning,
		Target:   types.Target{Node: "n1"},
	})
	if got := c.DroppedSignals(); got != 1 {
		t.Fatalf("DroppedSignals = %d, want 1", got)
	}
}

func TestHandleSignalCriticalWaitsForSpace(t *testing.T) {
	c := newQueueTestController()
	fillSignals(c)

	// Free one slot shortly after the critical signal starts waiting.
	go func() {
		time.Sleep(50 * time.Millisecond)
		<-c.signals
	}()
	done := make(chan struct{})
	go func() {
		c.HandleSignal(nil, types.Signal{
			Class:    types.ClassFellOffBus,
			Severity: types.SeverityCritical,
			Target:   types.Target{Node: "n1"},
		})
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(criticalEnqueueWait + time.Second):
		t.Fatal("critical signal did not enqueue after space freed")
	}
	if got := c.DroppedSignals(); got != 0 {
		t.Fatalf("DroppedSignals = %d, want 0 (critical signal must not be dropped once space frees)", got)
	}
}

func TestHandleSignalCriticalDropsAfterTimeout(t *testing.T) {
	c := newQueueTestController()
	fillSignals(c)

	start := time.Now()
	c.HandleSignal(nil, types.Signal{
		Class:    types.ClassFellOffBus,
		Severity: types.SeverityCritical,
		Target:   types.Target{Node: "n1"},
	})
	if elapsed := time.Since(start); elapsed < criticalEnqueueWait {
		t.Fatalf("critical signal gave up after %v, want at least %v", elapsed, criticalEnqueueWait)
	}
	if got := c.DroppedSignals(); got != 1 {
		t.Fatalf("DroppedSignals = %d, want 1", got)
	}
}

func TestHandleAgentEventSkipsReplayDuplicates(t *testing.T) {
	st, err := sqlite.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	c := New(st, st, nil, nil, nil, nil, nil, nil,
		slog.New(slog.NewTextHandler(io.Discard, nil)))

	ev := types.AgentEvent{EventID: "dup-1", Node: "n1", XID: 79, Timestamp: time.Now()}
	req := httptest.NewRequest("POST", "/api/v1/events", nil)
	if err := c.HandleAgentEvent(req, ev); err != nil {
		t.Fatal(err)
	}
	if err := c.HandleAgentEvent(req, ev); err != nil {
		t.Fatal(err)
	}
	// SQLite implements the durable EventOutbox, so classification and incident
	// mutation happen after the archive transaction rather than in the HTTP
	// handler.
	c.drainEventOutbox(context.Background())

	incidents, err := st.ListIncidents(context.Background(), store.IncidentFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(incidents) != 1 || incidents[0].SignalSeen != 1 {
		t.Fatalf("incidents after duplicate replay = %+v, want one signal seen once", incidents)
	}
}
