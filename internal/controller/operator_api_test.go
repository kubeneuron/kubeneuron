package controller

import (
	"context"
	"errors"
	"testing"

	"github.com/kubeneuron/kubeneuron/internal/store"
	"github.com/kubeneuron/kubeneuron/pkg/types"
)

func TestIncidentAcknowledgementAndResolutionAreIdempotent(t *testing.T) {
	c, st := newIngestTestController(t)
	ctx := context.Background()
	if err := c.ingest(ctx, signal(types.ClassECCDBE, "gpu-a", "GPU-a")); err != nil {
		t.Fatal(err)
	}
	incidents, err := st.ListIncidents(ctx, store.IncidentFilter{})
	if err != nil || len(incidents) != 1 {
		t.Fatalf("incidents=%#v err=%v", incidents, err)
	}
	initial := incidents[0]
	acknowledged, replayed, err := c.AcknowledgeIncident(ctx, initial.ID, "alice", "taking ownership", initial.Version, "ack-key")
	if err != nil || replayed || acknowledged.Version <= initial.Version {
		t.Fatalf("acknowledge=%#v replay=%v err=%v", acknowledged, replayed, err)
	}
	again, replayed, err := c.AcknowledgeIncident(ctx, initial.ID, "alice", "taking ownership", initial.Version, "ack-key")
	if err != nil || !replayed || again.Version != acknowledged.Version {
		t.Fatalf("ack replay=%#v replay=%v err=%v", again, replayed, err)
	}
	if _, _, err := c.AcknowledgeIncident(ctx, initial.ID, "alice", "different reason", initial.Version, "ack-key"); !errors.Is(err, store.ErrOperationalConflict) {
		t.Fatalf("reused acknowledgement key error=%v, want conflict", err)
	}

	resolved, replayed, err := c.ResolveIncidentRequest(ctx, initial.ID, "bob", "replaced hardware", acknowledged.Version, "resolve-key")
	if err != nil || replayed || resolved.State != types.StateResolved {
		t.Fatalf("resolve=%#v replay=%v err=%v", resolved, replayed, err)
	}
	resolvedAgain, replayed, err := c.ResolveIncidentRequest(ctx, initial.ID, "bob", "replaced hardware", acknowledged.Version, "resolve-key")
	if err != nil || !replayed || resolvedAgain.State != types.StateResolved {
		t.Fatalf("resolve replay=%#v replay=%v err=%v", resolvedAgain, replayed, err)
	}
	events, err := st.ListOperationalAudit(ctx, types.ResourceIncidentOperation, initial.ID, 10)
	if err != nil || len(events) != 2 || events[0].Action != "acknowledge" || events[1].Action != "resolve" || events[1].PrevHash != events[0].Hash {
		t.Fatalf("incident operation audit=%#v err=%v", events, err)
	}
}
