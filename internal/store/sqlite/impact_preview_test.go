// SPDX-License-Identifier: LicenseRef-KubeNeuron

package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"io/fs"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/kubeneuron/kubeneuron/internal/store"
	"github.com/kubeneuron/kubeneuron/pkg/types"
)

func impactIncident(t *testing.T, s *Store, id string) {
	t.Helper()
	now := time.Now()
	if err := s.CreateIncident(context.Background(), &types.Incident{
		ID: id, Target: types.Target{Node: "n1"}, Class: types.ClassECCDBE, State: types.StateAwaitingApproval,
		Playbook: "pb", OpenedAt: now, UpdatedAt: now, StateChangedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
}

// Previews are appended per (incident, epoch, computed_at); the newest by
// computed_at is the round's current one; a replay of the same triple is a
// conflict; an unavailable record keeps its reason and no payload.
func TestImpactPreviewsAreDurablePerRoundSQLite(t *testing.T) {
	s, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	ctx := context.Background()
	impactIncident(t, s, "inc-impact")

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
	other := &types.ImpactPreview{IncidentID: "inc-impact", Epoch: 2, ComputedAt: t0.Add(2 * time.Minute), Version: "impact-preview/v1", Digest: "sha256:c", Payload: []byte(`{"round":2}`)}
	if err := s.PutImpactPreview(ctx, other); err != nil {
		t.Fatal(err)
	}

	latest, err := s.LatestImpactPreview(ctx, "inc-impact", 1)
	if err != nil {
		t.Fatal(err)
	}
	if latest.Digest != "sha256:b" || latest.UnavailableReason != "WorkloadListingTimeout" || len(latest.Payload) != 0 || !latest.ComputedAt.Equal(second.ComputedAt) {
		t.Fatalf("latest = %+v, want the unavailable second record", latest)
	}
	all, err := s.ListImpactPreviews(ctx, "inc-impact", 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 || all[0].Digest != "sha256:b" || all[1].Digest != "sha256:a" || string(all[1].Payload) != `{"round":1}` {
		t.Fatalf("list = %+v, want newest first with payloads intact", all)
	}
	if _, err := s.LatestImpactPreview(ctx, "inc-impact", 3); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("empty round err = %v, want ErrNotFound", err)
	}
	if _, err := s.LatestImpactPreview(ctx, "inc-none", 1); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("unknown incident err = %v, want ErrNotFound", err)
	}
}

// A decision row carries the digest it was made against; pre-upgrade and
// digest-less rows read back empty.
func TestApprovalRecordsImpactDigestSQLite(t *testing.T) {
	s, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	ctx := context.Background()
	impactIncident(t, s, "inc-digest")
	if err := s.RecordApproval(ctx, &types.Approval{IncidentID: "inc-digest", StepName: "reboot", Decision: types.ApprovalRequested, Actor: "system", Channel: "system", At: time.Now(), ParkEpoch: 1}); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordApproval(ctx, &types.Approval{IncidentID: "inc-digest", StepName: "reboot", Decision: types.ApprovalApproved, Actor: "alice", Channel: "api", At: time.Now(), ParkEpoch: 1, ImpactDigest: "sha256:seen"}); err != nil {
		t.Fatal(err)
	}
	request, err := s.GetApprovalRequest(ctx, "inc-digest", 1)
	if err != nil {
		t.Fatal(err)
	}
	if request.ImpactDigest != "" {
		t.Fatalf("request digest = %q, want empty", request.ImpactDigest)
	}
	decision, err := s.LatestApprovalDecision(ctx, "inc-digest", 1)
	if err != nil {
		t.Fatal(err)
	}
	if decision.ImpactDigest != "sha256:seen" {
		t.Fatalf("decision digest = %q, want sha256:seen", decision.ImpactDigest)
	}
}

// Retention removes a terminal incident's previews with its approvals and
// audit, and leaves an open incident's alone.
func TestPruneRemovesImpactPreviewsWithTerminalIncidentsSQLite(t *testing.T) {
	s, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	ctx := context.Background()
	old := time.Now().Add(-48 * time.Hour)
	for _, inc := range []*types.Incident{
		{ID: "inc-old", Target: types.Target{Node: "n1"}, Class: types.ClassECCDBE, State: types.StateResolved, Playbook: "pb", OpenedAt: old, UpdatedAt: old, StateChangedAt: old},
		{ID: "inc-live", Target: types.Target{Node: "n1"}, Class: types.ClassECCDBE, State: types.StateAwaitingApproval, Playbook: "pb", OpenedAt: old, UpdatedAt: old, StateChangedAt: old},
	} {
		if err := s.CreateIncident(ctx, inc); err != nil {
			t.Fatal(err)
		}
		if err := s.PutImpactPreview(ctx, &types.ImpactPreview{IncidentID: inc.ID, Epoch: 1, ComputedAt: old, Version: "impact-preview/v1", Digest: "sha256:" + inc.ID, Payload: []byte(`{}`)}); err != nil {
			t.Fatal(err)
		}
	}
	// The resolved incident's updated_at must predate the cutoff; CreateIncident
	// may stamp its own time, so pin it.
	if _, err := s.sqlDB.Exec(`UPDATE incidents SET updated_at=? WHERE id='inc-old'`, ts(old)); err != nil {
		t.Fatal(err)
	}
	stats, err := s.Prune(ctx, 0, 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if stats.ImpactPreviews != 1 || stats.Incidents != 1 {
		t.Fatalf("prune stats = %+v, want one preview and one incident removed", stats)
	}
	if _, err := s.LatestImpactPreview(ctx, "inc-old", 1); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("pruned preview err = %v, want ErrNotFound", err)
	}
	if _, err := s.LatestImpactPreview(ctx, "inc-live", 1); err != nil {
		t.Fatalf("live incident's preview was pruned: %v", err)
	}
}

// A database at the previous head gains the table and the column exactly
// once, with existing approval rows reading an empty digest.
func TestMigrateImpactPreviewsFromPreviousHead(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v23.db")
	raw, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	names, err := fs.Glob(migrationsFS, "migrations/*.sql")
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(names)
	for _, name := range names {
		if name >= "migrations/0024_impact_previews.sql" {
			break
		}
		data, readErr := fs.ReadFile(migrationsFS, name)
		if readErr != nil {
			t.Fatal(readErr)
		}
		if _, execErr := raw.Exec(string(data)); execErr != nil {
			t.Fatalf("apply %s: %v", name, execErr)
		}
	}
	if _, err := raw.Exec(`CREATE TABLE schema_version (version INTEGER PRIMARY KEY, applied_at TEXT NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`INSERT INTO schema_version (version, applied_at) VALUES (23, '2026-10-19T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`INSERT INTO incidents (id, node, gpu_uuid, gpu_index, class, state, playbook, step_index, attempt, dry_run, signals_seen, opened_at, updated_at, state_changed_at, version)
		VALUES ('inc-v23', 'n1', '', 0, 'ecc-dbe', 'AWAITING_APPROVAL', 'pb', 0, 0, 0, 1, '2026-10-19T00:00:00Z', '2026-10-19T00:00:00Z', '2026-10-19T00:00:00Z', 1)`); err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`INSERT INTO approvals (incident_id, step_name, decision, actor, channel, at, park_epoch)
		VALUES ('inc-v23', 'reboot', 'approved', 'alice', 'cli', '2026-10-19T00:00:00Z', 1)`); err != nil {
		t.Fatal(err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}

	s, err := Open(path)
	if err != nil {
		t.Fatalf("opening v23 database: %v", err)
	}
	defer func() { _ = s.Close() }()
	decision, err := s.LatestApprovalDecision(context.Background(), "inc-v23", 1)
	if err != nil {
		t.Fatal(err)
	}
	if decision.ImpactDigest != "" {
		t.Fatalf("pre-upgrade decision digest = %q, want empty", decision.ImpactDigest)
	}
	if err := s.PutImpactPreview(context.Background(), &types.ImpactPreview{IncidentID: "inc-v23", Epoch: 1, ComputedAt: time.Now(), Version: "impact-preview/v1", Digest: "sha256:x", Payload: []byte(`{}`)}); err != nil {
		t.Fatalf("the migrated database must accept previews: %v", err)
	}
}

// computed_at is stored as text with trailing zeros trimmed, so its lexical
// order is not its time order: "12:00:00Z" sorts after "12:00:00.5Z". The
// newest record must be decided on the parsed time, never on the text.
func TestLatestImpactPreviewIsNewestByTimeNotByText(t *testing.T) {
	s, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	ctx := context.Background()
	impactIncident(t, s, "inc-order")
	older := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	newer := older.Add(500 * time.Millisecond)
	for _, p := range []*types.ImpactPreview{
		{IncidentID: "inc-order", Epoch: 1, ComputedAt: newer, Version: "impact-preview/v1", Digest: "sha256:newer", Payload: []byte(`{}`)},
		{IncidentID: "inc-order", Epoch: 1, ComputedAt: older, Version: "impact-preview/v1", Digest: "sha256:older", Payload: []byte(`{}`)},
	} {
		if err := s.PutImpactPreview(ctx, p); err != nil {
			t.Fatal(err)
		}
	}
	latest, err := s.LatestImpactPreview(ctx, "inc-order", 1)
	if err != nil {
		t.Fatal(err)
	}
	if latest.Digest != "sha256:newer" {
		t.Fatalf("latest = %s computed at %s, want sha256:newer (the text order would pick the older row)", latest.Digest, latest.ComputedAt)
	}
	all, err := s.ListImpactPreviews(ctx, "inc-order", 1)
	if err != nil || len(all) != 2 || all[0].Digest != "sha256:newer" || all[1].Digest != "sha256:older" {
		t.Fatalf("list = %+v err=%v, want newest first by time", all, err)
	}
}

// An idempotency record carries the digest of what its key produced, and a
// database at the previous head gains the column with existing rows empty.
func TestOperationalIdempotencyRecordsItsResult(t *testing.T) {
	s, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	ctx := context.Background()
	rec, created, err := s.PutOperationalIdempotency(ctx, &types.OperationalIdempotencyRecord{
		Kind: types.ResourceIncidentOperation, Actor: "alice", Key: "impact-recompute:k1",
		RequestDigest: "sha256:req", ResourceID: "inc-1", ResultDigest: "sha256:result",
	})
	if err != nil || !created || rec.ResultDigest != "sha256:result" {
		t.Fatalf("put = %+v created=%v err=%v", rec, created, err)
	}
	got, err := s.GetOperationalIdempotency(ctx, types.ResourceIncidentOperation, "alice", "impact-recompute:k1")
	if err != nil || got.ResultDigest != "sha256:result" {
		t.Fatalf("get = %+v err=%v", got, err)
	}
	again, created, err := s.PutOperationalIdempotency(ctx, &types.OperationalIdempotencyRecord{
		Kind: types.ResourceIncidentOperation, Actor: "alice", Key: "impact-recompute:k1",
		RequestDigest: "sha256:req", ResourceID: "inc-1", ResultDigest: "sha256:other",
	})
	if err != nil || created || again.ResultDigest != "sha256:result" {
		t.Fatalf("second put = %+v created=%v err=%v, want the first result kept", again, created, err)
	}
}
