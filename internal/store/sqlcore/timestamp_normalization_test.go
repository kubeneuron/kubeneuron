// SPDX-License-Identifier: Apache-2.0

package sqlcore

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/kubeneuron/kubeneuron/pkg/types"
	_ "modernc.org/sqlite"
)

func TestNormalizedAuditTimePreservesHistoricalHash(t *testing.T) {
	const want = "sha256:98323562197a5e0d2b1398a24fa378ae6437908d5bfd26f3cd537ae775805c87"
	for _, stamp := range []string{"2026-01-02T03:04:05.1234Z", "2026-01-02T03:04:05.123400000Z"} {
		at, err := time.Parse(time.RFC3339Nano, stamp)
		if err != nil {
			t.Fatal(err)
		}
		got := operationalAuditHash(types.OperationalAuditEvent{ResourceID: "r", Time: at})
		if got != want {
			t.Fatalf("hash for %s = %s, want %s", stamp, got, want)
		}
	}
}

type cancelAfterBatch struct {
	DBTX
	cancel context.CancelFunc
}

func (d cancelAfterBatch) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	result, err := d.DBTX.ExecContext(ctx, query, args...)
	if err == nil {
		d.cancel()
	}
	return result, err
}
func TestNormalizeColumnResumesAfterContextCancellation(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.Exec("CREATE TABLE events(id INTEGER PRIMARY KEY, timestamp TEXT)"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`WITH RECURSIVE seq(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM seq WHERE i<513)
 INSERT INTO events SELECT i,'2020-01-01T00:00:00Z' FROM seq`); err != nil {
		t.Fatal(err)
	}
	core := NewCore(db, nil, nil)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	core.db = cancelAfterBatch{core.db, cancel}
	const legacy = "length(timestamp)=20"
	n, err := core.normalizeColumn(ctx, "events", "id", "timestamp", legacy)
	if n != 512 || !errors.Is(err, context.Canceled) {
		t.Fatalf("interrupted: count %d, error %v", n, err)
	}
	core = NewCore(db, nil, nil)
	for _, want := range []int64{1, 0} {
		n, err = core.normalizeColumn(t.Context(), "events", "id", "timestamp", legacy)
		if n != want || err != nil {
			t.Fatalf("restart: count %d, want %d, error %v", n, want, err)
		}
	}
}
