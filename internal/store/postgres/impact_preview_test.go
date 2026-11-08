package postgres

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/kubeneuron/kubeneuron/internal/store"
	"github.com/kubeneuron/kubeneuron/pkg/types"
)

// The PostgreSQL twin of the SQLite impact-preview tests; skipped without
// KUBENEURON_TEST_POSTGRES_DSN like every test in this package.
func TestImpactPreviewsAreDurablePerRound(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	now := time.Now()
	if err := s.CreateIncident(ctx, &types.Incident{
		ID: "inc-impact", Target: types.Target{Node: "n1"}, Class: types.ClassECCDBE, State: types.StateAwaitingApproval,
		Playbook: "pb", OpenedAt: now, UpdatedAt: now, StateChangedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	t0 := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	first := &types.ImpactPreview{IncidentID: "inc-impact", Epoch: 1, ComputedAt: t0, Version: "impact-preview/v1", Digest: "sha256:a", Payload: []byte(`{"round":1}`)}
	if err := s.PutImpactPreview(ctx, first); err != nil {
		t.Fatal(err)
	}
	if err := s.PutImpactPreview(ctx, first); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("replay err = %v, want ErrConflict", err)
	}
	second := &types.ImpactPreview{IncidentID: "inc-impact", Epoch: 1, ComputedAt: t0.Add(time.Minute), Version: "impact-preview/v1", Digest: "sha256:b", UnavailableReason: "WorkloadListingTimeout"}
	if err := s.PutImpactPreview(ctx, second); err != nil {
		t.Fatal(err)
	}
	latest, err := s.LatestImpactPreview(ctx, "inc-impact", 1)
	if err != nil {
		t.Fatal(err)
	}
	if latest.Digest != "sha256:b" || latest.UnavailableReason != "WorkloadListingTimeout" || len(latest.Payload) != 0 {
		t.Fatalf("latest = %+v, want the unavailable second record", latest)
	}
	all, err := s.ListImpactPreviews(ctx, "inc-impact", 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 || all[0].Digest != "sha256:b" || string(all[1].Payload) != `{"round":1}` {
		t.Fatalf("list = %+v, want newest first", all)
	}
	if _, err := s.LatestImpactPreview(ctx, "inc-impact", 2); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("empty round err = %v, want ErrNotFound", err)
	}
	if err := s.RecordApproval(ctx, &types.Approval{IncidentID: "inc-impact", StepName: "reboot", Decision: types.ApprovalApproved, Actor: "alice", Channel: "api", At: now, ParkEpoch: 1, ImpactDigest: "sha256:b"}); err != nil {
		t.Fatal(err)
	}
	decision, err := s.LatestApprovalDecision(ctx, "inc-impact", 1)
	if err != nil {
		t.Fatal(err)
	}
	if decision.ImpactDigest != "sha256:b" {
		t.Fatalf("decision digest = %q, want sha256:b", decision.ImpactDigest)
	}
}
