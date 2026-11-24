// SPDX-License-Identifier: Apache-2.0

package sqlite

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kubeneuron/kubeneuron/pkg/types"
)

func TestNormalizeTimestampsResumesAfterDatabaseError(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "events.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	ctx := context.Background()
	_, err = s.sqlDB.ExecContext(ctx, `WITH RECURSIVE seq(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM seq WHERE i<513)
 INSERT INTO events(node,xid,timestamp) SELECT 'node',31,'2026-01-01T00:00:00.1234Z' FROM seq`)
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.sqlDB.ExecContext(ctx, `CREATE TRIGGER interrupt_normalization BEFORE UPDATE ON events
 WHEN (SELECT count(*) FROM events WHERE length(timestamp)=30)>=512
 BEGIN SELECT RAISE(ABORT,'simulated database failure'); END`)
	if err != nil {
		t.Fatal(err)
	}
	count, err := s.NormalizeTimestamps(ctx)
	if err == nil || !strings.Contains(err.Error(), "events.timestamp") || count != 512 {
		t.Fatalf("first pass: count %d, error %v", count, err)
	}
	if _, err = s.sqlDB.ExecContext(ctx, "DROP TRIGGER interrupt_normalization"); err != nil {
		t.Fatal(err)
	}
	for _, want := range []int64{1, 0} {
		count, err = s.NormalizeTimestamps(ctx)
		if count != want || err != nil {
			t.Fatalf("resume: count %d, want %d, error %v", count, want, err)
		}
	}
}

func TestLegacyEventOrderAndRollbackReader(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "legacy.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	const second = "2026-01-02T03:04:05"
	for xid, fraction := range []string{".123401", ".1234", "", ".1", ".123400000"} {
		if _, err := s.SQL.Exec("INSERT INTO events(node,xid,timestamp) VALUES ('node',?,?)", xid, second+fraction+"Z"); err != nil {
			t.Fatal(err)
		}
	}
	n, err := s.NormalizeTimestamps(context.Background())
	if err != nil || n != 4 {
		t.Fatalf("updated %d timestamps: %v", n, err)
	}
	rows, err := s.SQL.Query("SELECT xid,timestamp FROM events ORDER BY timestamp,id")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	for _, want := range []int{2, 3, 1, 4, 0} {
		var xid int
		var stamp string
		if !rows.Next() {
			t.Fatalf("missing event %d: %v", want, rows.Err())
		}
		if err := rows.Scan(&xid, &stamp); err != nil {
			t.Fatal(err)
		}
		parsed, err := time.Parse(time.RFC3339Nano, stamp)
		if err != nil || xid != want || parsed.UTC().Format("2006-01-02T15:04:05.000000000Z") != stamp {
			t.Fatalf("expected event %d with fixed UTC time, got %d %q: %v", want, xid, stamp, err)
		}
	}
	if rows.Next() || rows.Err() != nil {
		t.Fatalf("unexpected rows or error: %v", rows.Err())
	}
}

func TestNormalizeLeavesOperationalAuditUntouched(t *testing.T) {
	s, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	ctx := t.Context()
	at, err := time.Parse(time.RFC3339Nano, "2026-01-02T03:04:05.1234Z")
	if err != nil {
		t.Fatal(err)
	}
	event := types.OperationalAuditEvent{Time: at, Kind: types.ResourceCandidateConfiguration, ResourceID: "test", Actor: "operator", Action: "upload"}
	if err := s.AppendOperationalAudit(ctx, &event); err != nil {
		t.Fatal(err)
	}
	_, err = s.SQL.Exec("CREATE TRIGGER forbid_audit_update BEFORE UPDATE ON operational_audit BEGIN SELECT RAISE(ABORT,'unexpected audit rewrite'); END")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.NormalizeTimestamps(ctx); err != nil {
		t.Fatal(err)
	}
	var stamp, hash string
	if err := s.SQL.QueryRow("SELECT time,hash FROM operational_audit WHERE id=?", event.ID).Scan(&stamp, &hash); err != nil {
		t.Fatal(err)
	}
	if stamp != at.Format(time.RFC3339Nano) || hash != event.Hash {
		t.Fatalf("unexpected audit record: %s, %s", stamp, hash)
	}
}
