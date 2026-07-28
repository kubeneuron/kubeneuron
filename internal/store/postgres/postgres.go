// Package postgres is the production Store implementation for HA
// controllers. All query, lease, and outbox logic lives in the shared
// sqlcore engine; this package owns connection setup and migrations.
// Backups use pg_dump/PITR (documented in operations), not the SQLite
// snapshot endpoint.
package postgres

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

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/kubeneuron/kubeneuron/internal/store"
	"github.com/kubeneuron/kubeneuron/internal/store/sqlcore"
)

//go:embed migrations
var migrationsFS embed.FS

// Store implements store.Store on PostgreSQL via the shared sqlcore engine.
type Store struct {
	*sqlcore.Core
	sqlDB *sql.DB
}

// PruneStats re-exports the shared retention statistics type.
type PruneStats = sqlcore.PruneStats

var _ store.Store = (*Store)(nil)
var _ store.EventSink = (*Store)(nil)
var _ store.EventOutbox = (*Store)(nil)
var _ store.AcceleratorReportStore = (*Store)(nil)

// Open connects to PostgreSQL at dsn and applies pending migrations. The
// caller owns DSN secrecy (pass it via file/secret, never argv).
func Open(ctx context.Context, dsn string) (*Store, error) {
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(16)
	db.SetMaxIdleConns(4)
	db.SetConnMaxLifetime(time.Hour)
	pingCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := db.PingContext(pingCtx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("postgres ping: %w", err)
	}
	s := &Store{Core: sqlcore.NewCore(db, sqlcore.RebindQuestion, nil), sqlDB: db}
	if err := s.migrate(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

// Close closes the connection pool.
func (s *Store) Close() error { return s.sqlDB.Close() }

// migrate applies embedded migrations newer than the recorded schema
// version, one transaction each, and refuses databases written by a newer
// binary — the same contract as the SQLite store.
func (s *Store) migrate(ctx context.Context) error {
	// A session advisory lock serializes concurrent starts (two controller
	// replicas racing on first boot).
	const migrationLock = 762_804_291
	lockConn, err := s.sqlDB.Conn(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = lockConn.Close() }()
	if _, err := lockConn.ExecContext(ctx, `SELECT pg_advisory_lock($1)`, migrationLock); err != nil {
		return fmt.Errorf("acquire migration lock: %w", err)
	}
	defer func() {
		_, _ = lockConn.ExecContext(ctx, `SELECT pg_advisory_unlock($1)`, migrationLock)
	}()

	if _, err := s.sqlDB.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_version (
		version    INTEGER PRIMARY KEY,
		applied_at TEXT NOT NULL
	)`); err != nil {
		return fmt.Errorf("creating schema_version: %w", err)
	}
	var current int
	if err := s.sqlDB.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(version), 0) FROM schema_version`).Scan(&current); err != nil {
		return err
	}
	names, err := fs.Glob(migrationsFS, "migrations/*.sql")
	if err != nil {
		return err
	}
	sort.Strings(names)
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
		tx, err := s.sqlDB.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, string(sqlText)); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("migration %s: %w", name, err)
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO schema_version (version, applied_at) VALUES ($1, $2)`,
			version, sqlcore.TS(time.Now())); err != nil {
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
