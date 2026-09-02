// Package sqlite is the default Store implementation, backed by SQLite in
// WAL mode via the pure-Go modernc.org/sqlite driver (no cgo, single static
// binary). All query, lease, and outbox logic lives in the shared sqlcore
// engine; this package owns opening, pragmas, migrations, and backup.
package sqlite

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"github.com/kubeneuron/kubeneuron/internal/store"
	"github.com/kubeneuron/kubeneuron/internal/store/sqlcore"
)

// Store implements store.Store on SQLite via the shared sqlcore engine.
type Store struct {
	*sqlcore.Core
	sqlDB *sql.DB
}

// PruneStats re-exports the shared retention statistics type.
type PruneStats = sqlcore.PruneStats

var _ store.Store = (*Store)(nil)
var _ store.RestorativeActionClaimer = (*Store)(nil)
var _ store.EventSink = (*Store)(nil)
var _ store.EventOutbox = (*Store)(nil)
var _ store.AcceleratorReportStore = (*Store)(nil)

// ts keeps package-internal tests readable; the canonical helper lives in
// sqlcore.
func ts(t time.Time) string { return sqlcore.TS(t) }

func sqliteCheckpoint(ctx context.Context, db *sql.DB) error {
	if _, err := db.ExecContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		return fmt.Errorf("checkpoint WAL after prune: %w", err)
	}
	return nil
}

//go:embed migrations
var migrationsFS embed.FS

// Open opens (creating if needed) the SQLite database at path and applies
// pending migrations. Use ":memory:" for tests.
func Open(path string) (*Store, error) {
	// synchronous(FULL) is explicit, not left to driver defaults: this store
	// is the durable authority for incidents, audit, and action leases, and
	// its crash-safety claims assume fsync-on-commit.
	dsn := fmt.Sprintf("file:%s?_pragma=journal_mode(WAL)&_pragma=synchronous(FULL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)", path)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	// SQLite allows a single writer; serialize access at the pool level.
	db.SetMaxOpenConns(1)
	var syncMode int
	if err := db.QueryRow(`PRAGMA synchronous`).Scan(&syncMode); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("verify synchronous pragma: %w", err)
	}
	if syncMode < 2 { // 2 = FULL
		_ = db.Close()
		return nil, fmt.Errorf("synchronous pragma is %d, want FULL (2)", syncMode)
	}
	s := &Store{Core: sqlcore.NewCore(db, nil, sqliteCheckpoint), sqlDB: db}
	if err := s.migrate(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

// migrate applies embedded migrations that are newer than the recorded
// schema version. Each migration runs in its own transaction and is recorded
// in schema_version, so statements are executed exactly once per database —
// future ALTER TABLE migrations must not be re-run against an up-to-date
// schema.
func (s *Store) migrate() error {
	if _, err := s.sqlDB.Exec(`CREATE TABLE IF NOT EXISTS schema_version (
		version    INTEGER PRIMARY KEY,
		applied_at TEXT NOT NULL
	)`); err != nil {
		return fmt.Errorf("creating schema_version: %w", err)
	}
	var current int
	if err := s.sqlDB.QueryRow(`SELECT COALESCE(MAX(version), 0) FROM schema_version`).Scan(&current); err != nil {
		return err
	}
	if current == 0 {
		// Databases created before schema_version existed already contain the
		// 0001 schema (every statement there is IF NOT EXISTS); baseline them
		// instead of assuming an empty database.
		var n int
		if err := s.sqlDB.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='incidents'`).Scan(&n); err != nil {
			return err
		}
		if n > 0 {
			if _, err := s.sqlDB.Exec(`INSERT INTO schema_version (version, applied_at) VALUES (1, ?)`, ts(time.Now())); err != nil {
				return err
			}
			current = 1
		}
	}
	names, err := fs.Glob(migrationsFS, "migrations/*.sql")
	if err != nil {
		return err
	}
	sort.Strings(names)
	// Refuse databases from a newer binary: this build cannot know what a
	// future migration changed, and writing through an older schema
	// understanding could corrupt data the newer binary depends on.
	if len(names) > 0 {
		newest, err := migrationVersion(names[len(names)-1])
		if err != nil {
			return err
		}
		if current > newest {
			return fmt.Errorf("database schema version %d is newer than this binary's %d; upgrade the binary instead of downgrading the store", current, newest)
		}
	}
	for _, name := range names {
		version, err := migrationVersion(name)
		if err != nil {
			return err
		}
		if version <= current {
			continue
		}
		sqlText, err := fs.ReadFile(migrationsFS, name)
		if err != nil {
			return err
		}
		tx, err := s.sqlDB.Begin()
		if err != nil {
			return err
		}
		if _, err := tx.Exec(string(sqlText)); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("migration %s: %w", name, err)
		}
		if _, err := tx.Exec(`INSERT INTO schema_version (version, applied_at) VALUES (?, ?)`, version, ts(time.Now())); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("recording migration %s: %w", name, err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("committing migration %s: %w", name, err)
		}
		current = version
	}
	return nil
}

// migrationVersion extracts the numeric prefix of a migration filename,
// e.g. "migrations/0002_foo.sql" -> 2.
func migrationVersion(name string) (int, error) {
	base := path.Base(name)
	prefix, _, ok := strings.Cut(base, "_")
	if !ok {
		return 0, fmt.Errorf("migration %s: name must be <version>_<description>.sql", name)
	}
	v, err := strconv.Atoi(prefix)
	if err != nil || v <= 0 {
		return 0, fmt.Errorf("migration %s: invalid version prefix %q", name, prefix)
	}
	return v, nil
}

// Close closes the database.
func (s *Store) Close() error { return s.sqlDB.Close() }

// BackupTo writes a transactionally consistent snapshot of the database to
// destPath via VACUUM INTO (safe against concurrent writers in WAL mode).
// destPath must not exist; the caller owns cleanup of the produced file.
func (s *Store) BackupTo(ctx context.Context, destPath string) error {
	if _, err := s.sqlDB.ExecContext(ctx, `VACUUM INTO ?`, destPath); err != nil {
		return fmt.Errorf("vacuum into backup: %w", err)
	}
	return nil
}
