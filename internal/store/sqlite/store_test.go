package sqlite

import (
	"context"
	"database/sql"
	"errors"

	"io/fs"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kubeneuron/kubeneuron/internal/store"
	"github.com/kubeneuron/kubeneuron/pkg/types"
)

func testIncident(id string, opened time.Time) *types.Incident {
	return &types.Incident{
		ID:             id,
		Target:         types.Target{Node: "node-a", GPUUUID: "GPU-1"},
		Class:          types.ClassECCDBE,
		State:          types.StateOpen,
		SignalSeen:     1,
		OpenedAt:       opened,
		UpdatedAt:      opened,
		StateChangedAt: opened,
	}
}

func TestWithTxCommitsAtomically(t *testing.T) {
	s, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	ctx := context.Background()
	now := time.Now()

	inc := testIncident("inc-1", now)
	err = s.WithTx(ctx, func(tx store.Tx) error {
		if err := tx.CreateIncident(ctx, inc); err != nil {
			return err
		}
		return tx.AppendAudit(ctx, &types.AuditEntry{
			IncidentID: inc.ID, Time: now,
			FromState: inc.State, ToState: inc.State,
			Actor: "system", Action: "open",
		})
	})
	if err != nil {
		t.Fatalf("WithTx: %v", err)
	}
	if _, err := s.GetIncident(ctx, "inc-1"); err != nil {
		t.Fatalf("incident not visible after commit: %v", err)
	}
	trail, err := s.AuditTrail(ctx, "inc-1")
	if err != nil || len(trail) != 1 {
		t.Fatalf("audit trail after commit: %v entries, err %v", len(trail), err)
	}
}

func TestWithTxRollsBackEverything(t *testing.T) {
	s, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	ctx := context.Background()

	boom := errors.New("boom")
	err = s.WithTx(ctx, func(tx store.Tx) error {
		if err := tx.CreateIncident(ctx, testIncident("inc-rb", time.Now())); err != nil {
			return err
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("WithTx error = %v, want %v", err, boom)
	}
	if _, err := s.GetIncident(ctx, "inc-rb"); err != store.ErrNotFound {
		t.Fatalf("incident visible after rollback: err = %v, want ErrNotFound", err)
	}
}

func TestUpdateIncidentOptimisticConcurrency(t *testing.T) {
	s, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	ctx := context.Background()

	inc := testIncident("inc-oc", time.Now())
	if err := s.CreateIncident(ctx, inc); err != nil {
		t.Fatal(err)
	}

	// Two writers snapshot the same version.
	a, err := s.GetIncident(ctx, "inc-oc")
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.GetIncident(ctx, "inc-oc")
	if err != nil {
		t.Fatal(err)
	}
	if a.Version != 0 {
		t.Fatalf("fresh incident version = %d, want 0", a.Version)
	}

	// The first writer wins and its in-memory version advances.
	a.SignalSeen = 7
	if err := s.UpdateIncident(ctx, a); err != nil {
		t.Fatalf("first update = %v", err)
	}
	if a.Version != 1 {
		t.Fatalf("version after successful update = %d, want 1", a.Version)
	}

	// The second writer holds the stale version 0 and must be told it conflicts
	// rather than silently clobbering the winner's write.
	b.SignalSeen = 99
	if err := s.UpdateIncident(ctx, b); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("stale update = %v, want ErrConflict", err)
	}
	got, err := s.GetIncident(ctx, "inc-oc")
	if err != nil {
		t.Fatal(err)
	}
	if got.SignalSeen != 7 {
		t.Fatalf("SignalSeen = %d, want 7 (the winning write must survive)", got.SignalSeen)
	}

	// A genuinely absent incident is ErrNotFound, never ErrConflict.
	ghost := testIncident("inc-ghost", time.Now())
	if err := s.UpdateIncident(ctx, ghost); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("update of absent incident = %v, want ErrNotFound", err)
	}
}

func TestStateChangedAtSurvivesSignalBumps(t *testing.T) {
	s, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	ctx := context.Background()
	opened := time.Now().Add(-time.Hour).Truncate(time.Millisecond)

	inc := testIncident("inc-sc", opened)
	if err := s.CreateIncident(ctx, inc); err != nil {
		t.Fatal(err)
	}
	// A duplicate signal bumps UpdatedAt but not StateChangedAt.
	inc.SignalSeen++
	inc.UpdatedAt = time.Now()
	if err := s.UpdateIncident(ctx, inc); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetIncident(ctx, "inc-sc")
	if err != nil {
		t.Fatal(err)
	}
	if !got.StateChangedAt.Equal(opened) {
		t.Fatalf("StateChangedAt = %v, want opened time %v", got.StateChangedAt, opened)
	}
	if !got.UpdatedAt.After(opened) {
		t.Fatalf("UpdatedAt not bumped: %v", got.UpdatedAt)
	}
}

func TestCreateIncidentDefaultsStateChangedAt(t *testing.T) {
	s, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	ctx := context.Background()
	opened := time.Now().Truncate(time.Millisecond)

	inc := testIncident("inc-def", opened)
	inc.StateChangedAt = time.Time{}
	if err := s.CreateIncident(ctx, inc); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetIncident(ctx, "inc-def")
	if err != nil {
		t.Fatal(err)
	}
	if !got.StateChangedAt.Equal(opened) {
		t.Fatalf("StateChangedAt = %v, want fallback to OpenedAt %v", got.StateChangedAt, opened)
	}
}

func TestMigrationsRunOncePerVersion(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	var maxVersion int
	if err := s.sqlDB.QueryRow(`SELECT MAX(version) FROM schema_version`).Scan(&maxVersion); err != nil {
		t.Fatal(err)
	}
	if maxVersion < 2 {
		t.Fatalf("schema version = %d, want >= 2", maxVersion)
	}
	_ = s.Close()

	// Reopening must not re-run migrations (an ALTER TABLE would fail).
	s2, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer func() { _ = s2.Close() }()
	var count int
	if err := s2.sqlDB.QueryRow(`SELECT COUNT(*) FROM schema_version WHERE version=?`, maxVersion).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("version %d recorded %d times, want 1", maxVersion, count)
	}
}

func TestMigrateBaselinesLegacyDatabase(t *testing.T) {
	// A database created before schema_version existed has the 0001 schema
	// but no version bookkeeping. Opening it must baseline version 1 and then
	// apply every later migration, including ALTER TABLE profile-revision
	// fields added after the accelerator report table was introduced.
	path := filepath.Join(t.TempDir(), "legacy.db")
	raw, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	initSQL, err := fs.ReadFile(migrationsFS, "migrations/0001_init.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(string(initSQL)); err != nil {
		t.Fatal(err)
	}
	_ = raw.Close()

	s, err := Open(path)
	if err != nil {
		t.Fatalf("opening legacy database: %v", err)
	}
	defer func() { _ = s.Close() }()
	var hasColumn int
	err = s.sqlDB.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('incidents') WHERE name='state_changed_at'`).Scan(&hasColumn)
	if err != nil {
		t.Fatal(err)
	}
	if hasColumn != 1 {
		t.Fatal("state_changed_at column missing after legacy upgrade")
	}
	for _, column := range []string{"profile_uid", "profile_generation", "node_uid"} {
		var present int
		if err := s.sqlDB.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('accelerator_reports') WHERE name=?`, column).Scan(&present); err != nil {
			t.Fatal(err)
		}
		if present != 1 {
			t.Fatalf("%s column missing after legacy upgrade", column)
		}
	}
	// Legacy rows scan with a fallback instead of a zero StateChangedAt.
	ctx := context.Background()
	if err := s.CreateIncident(ctx, testIncident("inc-legacy", time.Now())); err != nil {
		t.Fatal(err)
	}
	if _, err := s.sqlDB.Exec(`UPDATE incidents SET state_changed_at='' WHERE id='inc-legacy'`); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetIncident(ctx, "inc-legacy")
	if err != nil {
		t.Fatal(err)
	}
	if got.StateChangedAt.IsZero() {
		t.Fatal("StateChangedAt zero for legacy row, want UpdatedAt fallback")
	}
	if !got.StateChangedAt.Equal(got.UpdatedAt) {
		t.Fatalf("StateChangedAt = %v, want UpdatedAt %v", got.StateChangedAt, got.UpdatedAt)
	}
}

func TestMigrateExistingAcceleratorReportDatabaseAddsIdentityColumns(t *testing.T) {
	path := filepath.Join(t.TempDir(), "accelerator-v7.db")
	raw, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	for _, migration := range []string{
		"migrations/0001_init.sql",
		"migrations/0002_incident_state_changed_at.sql",
		"migrations/0003_event_dedup.sql",
		"migrations/0004_action_queue.sql",
		"migrations/0005_action_leases.sql",
		"migrations/0006_event_outbox.sql",
		"migrations/0007_accelerator_reports.sql",
	} {
		data, err := fs.ReadFile(migrationsFS, migration)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := raw.Exec(string(data)); err != nil {
			t.Fatalf("apply %s: %v", migration, err)
		}
	}
	if _, err := raw.Exec(`CREATE TABLE schema_version (version INTEGER PRIMARY KEY, applied_at TEXT NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`INSERT INTO schema_version (version, applied_at) VALUES (7, '2026-07-24T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}

	s, err := Open(path)
	if err != nil {
		t.Fatalf("opening v7 accelerator database: %v", err)
	}
	defer func() { _ = s.Close() }()
	for _, column := range []string{"profile_uid", "profile_generation", "node_uid"} {
		var present int
		if err := s.sqlDB.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('accelerator_reports') WHERE name=?`, column).Scan(&present); err != nil {
			t.Fatal(err)
		}
		if present != 1 {
			t.Fatalf("%s column missing after v7 upgrade", column)
		}
	}
	var nodeUIDColumn int
	if err := s.sqlDB.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('nodes') WHERE name='node_uid'`).Scan(&nodeUIDColumn); err != nil {
		t.Fatal(err)
	}
	if nodeUIDColumn != 1 {
		t.Fatal("node_uid column missing from nodes after v7 upgrade")
	}
}

func TestMigrationVersionParsing(t *testing.T) {
	for _, tc := range []struct {
		name    string
		want    int
		wantErr bool
	}{
		{"migrations/0001_init.sql", 1, false},
		{"migrations/0002_incident_state_changed_at.sql", 2, false},
		{"migrations/12_x.sql", 12, false},
		{"migrations/noversion.sql", 0, true},
		{"migrations/abc_x.sql", 0, true},
	} {
		got, err := migrationVersion(tc.name)
		if tc.wantErr != (err != nil) {
			t.Errorf("%s: err = %v, wantErr = %v", tc.name, err, tc.wantErr)
		}
		if got != tc.want {
			t.Errorf("%s: version = %d, want %d", tc.name, got, tc.want)
		}
	}
}

func TestWriteEventDeduplicatesByEventID(t *testing.T) {
	s, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	ctx := context.Background()

	ev := &types.AgentEvent{EventID: "abc123", Node: "n1", XID: 79, Timestamp: time.Now()}
	fresh, err := s.WriteEvent(ctx, ev)
	if err != nil || !fresh {
		t.Fatalf("first WriteEvent = %v, %v; want fresh insert", fresh, err)
	}
	fresh, err = s.WriteEvent(ctx, ev)
	if err != nil {
		t.Fatal(err)
	}
	if fresh {
		t.Fatal("replayed event with same EventID must report fresh=false")
	}

	// Events without an ID (legacy agents) are never deduplicated.
	for i := 0; i < 2; i++ {
		fresh, err = s.WriteEvent(ctx, &types.AgentEvent{Node: "n1", XID: 63, Timestamp: time.Now()})
		if err != nil || !fresh {
			t.Fatalf("legacy event write %d = %v, %v; want fresh insert", i, fresh, err)
		}
	}
}

func TestApplyNodeConfigPauses(t *testing.T) {
	s, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	ctx := context.Background()

	// Existing registered node plus one not yet registered.
	if err := s.UpsertAgentRegistration(ctx, &types.Node{Name: "n1", AgentLastSeen: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if err := s.ApplyNodeConfigPauses(ctx, []string{"n1", "n-future"}); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"n1", "n-future"} {
		n, err := s.GetNode(ctx, name)
		if err != nil || !n.Paused {
			t.Fatalf("node %s paused = %v, err %v", name, n, err)
		}
	}

	// Removing a node from the set unpauses it; the other stays paused.
	if err := s.ApplyNodeConfigPauses(ctx, []string{"n-future"}); err != nil {
		t.Fatal(err)
	}
	n1, _ := s.GetNode(ctx, "n1")
	nf, _ := s.GetNode(ctx, "n-future")
	if n1.Paused || !nf.Paused {
		t.Fatalf("pause set = n1:%v n-future:%v, want false/true", n1.Paused, nf.Paused)
	}

	// Registration heartbeats must not clobber config-owned pause state.
	if err := s.UpsertAgentRegistration(ctx, &types.Node{Name: "n-future", AgentLastSeen: time.Now()}); err != nil {
		t.Fatal(err)
	}
	nf, _ = s.GetNode(ctx, "n-future")
	if !nf.Paused {
		t.Fatal("registration must preserve config-owned pause")
	}
}

func TestPruneEnforcesRetentionBoundaries(t *testing.T) {
	s, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	ctx := context.Background()
	old := time.Now().Add(-48 * time.Hour)
	fresh := time.Now()

	// Old archived event whose outbox item is done; fresh event still pending.
	if _, err := s.ArchiveAndEnqueueEvent(ctx, &types.AgentEvent{EventID: "ev-old", Node: "n1", XID: 79, Timestamp: old}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ArchiveAndEnqueueEvent(ctx, &types.AgentEvent{EventID: "ev-new", Node: "n1", XID: 63, Timestamp: fresh}); err != nil {
		t.Fatal(err)
	}
	// Mark the old outbox item done and age both rows' bookkeeping.
	for _, q := range []string{
		`UPDATE event_outbox SET state='done', updated_at=? WHERE event_row_id IN (SELECT id FROM events WHERE event_id='ev-old')`,
		`UPDATE events SET timestamp=? WHERE event_id='ev-old'`,
	} {
		if _, err := s.sqlDB.Exec(q, old.UTC().Format(time.RFC3339Nano)); err != nil {
			t.Fatal(err)
		}
	}

	// One completed old action, one pending old action.
	if err := s.EnqueueAction(ctx, "n1", types.Action{IncidentID: "inc-a", ID: "act-done", Type: types.ActionRunDiag}); err != nil {
		t.Fatal(err)
	}
	if err := s.CompleteAction(ctx, "act-done", types.ActionResult{ActionID: "act-done", OK: true}); err != nil {
		t.Fatal(err)
	}
	if err := s.EnqueueAction(ctx, "n1", types.Action{IncidentID: "inc-a", ID: "act-live", Type: types.ActionRunDiag}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.sqlDB.Exec(`UPDATE actions SET updated_at=?`, old.UTC().Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}

	// Terminal old incident with audit, live incident untouched.
	oldInc := testIncident("inc-old", old)
	oldInc.State = types.StateResolved
	liveInc := testIncident("inc-live", fresh)
	for _, inc := range []*types.Incident{oldInc, liveInc} {
		if err := s.CreateIncident(ctx, inc); err != nil {
			t.Fatal(err)
		}
		if err := s.AppendAudit(ctx, &types.AuditEntry{IncidentID: inc.ID, Time: inc.OpenedAt, FromState: inc.State, ToState: inc.State, Actor: "system", Action: "open"}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.sqlDB.Exec(`UPDATE incidents SET updated_at=? WHERE id='inc-old'`, old.UTC().Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}

	// Data-retention only: audit history stays.
	stats, err := s.Prune(ctx, 24*time.Hour, 0)
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if stats.Outbox != 1 || stats.Events != 1 || stats.Actions != 1 {
		t.Fatalf("data prune stats = %+v, want 1 outbox, 1 event, 1 action", stats)
	}
	if stats.Incidents != 0 || stats.Audit != 0 {
		t.Fatalf("audit must survive without opt-in: %+v", stats)
	}
	var count int
	if err := s.sqlDB.QueryRow(`SELECT COUNT(*) FROM events`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("events remaining = %d (err %v), want only the fresh one", count, err)
	}
	if err := s.sqlDB.QueryRow(`SELECT COUNT(*) FROM actions WHERE id='act-live'`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("pending action must survive: %d (err %v)", count, err)
	}

	// Opt-in audit retention: terminal old incident and its history go.
	stats, err = s.Prune(ctx, 0, 24*time.Hour)
	if err != nil {
		t.Fatalf("audit Prune: %v", err)
	}
	if stats.Incidents != 1 || stats.Audit != 1 {
		t.Fatalf("audit prune stats = %+v, want 1 incident and 1 audit row", stats)
	}
	if _, err := s.GetIncident(ctx, "inc-live"); err != nil {
		t.Fatalf("live incident must survive: %v", err)
	}
	if _, err := s.GetIncident(ctx, "inc-old"); err == nil {
		t.Fatal("old terminal incident must be pruned")
	}
}

func TestOpenEnforcesFullSynchronousMode(t *testing.T) {
	s, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	var mode int
	if err := s.sqlDB.QueryRow(`PRAGMA synchronous`).Scan(&mode); err != nil {
		t.Fatal(err)
	}
	if mode != 2 {
		t.Fatalf("synchronous = %d, want FULL (2)", mode)
	}
}

// An older binary must refuse a database written by a newer one instead of
// silently writing through an outdated schema understanding.
func TestOpenRefusesNewerSchemaVersion(t *testing.T) {
	path := filepath.Join(t.TempDir(), "future.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.sqlDB.Exec(`INSERT INTO schema_version (version, applied_at) VALUES (9999, ?)`, ts(time.Now())); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(path); err == nil || !strings.Contains(err.Error(), "newer than this binary") {
		t.Fatalf("Open must refuse a future schema, got %v", err)
	}
}
