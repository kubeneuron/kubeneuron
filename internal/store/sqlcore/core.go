// Package sqlcore is the dialect-shared implementation of the KubeNeuron
// store: every query, lease, and outbox rule lives here exactly once, and
// the SQLite and PostgreSQL packages supply only open/migrate/dialect glue.
// SQL stays in the portable subset both engines execute identically
// (ON CONFLICT, RETURNING, TEXT timestamps).
package sqlcore

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/kubeneuron/kubeneuron/internal/store"
	"github.com/kubeneuron/kubeneuron/pkg/types"
)

// DBTX is the subset of *sql.DB and *sql.Tx the Queries need, so every
// statement can run either standalone or inside a transaction.
type DBTX interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// Queries holds every statement implementation over a DBTX.
type Queries struct {
	db DBTX
}

// rebindDB rewrites placeholders for the dialect before delegating; the
// identity function serves engines that accept '?' natively.
type rebindDB struct {
	inner  DBTX
	rebind func(string) string
}

func (r rebindDB) ExecContext(ctx context.Context, q string, args ...any) (sql.Result, error) {
	return r.inner.ExecContext(ctx, r.rebind(q), args...)
}
func (r rebindDB) QueryContext(ctx context.Context, q string, args ...any) (*sql.Rows, error) {
	return r.inner.QueryContext(ctx, r.rebind(q), args...)
}
func (r rebindDB) QueryRowContext(ctx context.Context, q string, args ...any) *sql.Row {
	return r.inner.QueryRowContext(ctx, r.rebind(q), args...)
}

// Core is the engine-shared store implementation. Checkpoint is the
// dialect's post-prune maintenance hook (WAL checkpoint on SQLite; a no-op
// on PostgreSQL).
type Core struct {
	Queries
	SQL        *sql.DB
	rebind     func(string) string
	Checkpoint func(ctx context.Context, db *sql.DB) error
}

// NewCore wires a Core over db. rebind may be nil for engines that accept
// '?' placeholders natively; checkpoint may be nil for engines that need no
// post-prune maintenance.
func NewCore(db *sql.DB, rebind func(string) string, checkpoint func(ctx context.Context, db *sql.DB) error) *Core {
	if rebind == nil {
		rebind = func(q string) string { return q }
	}
	if checkpoint == nil {
		checkpoint = func(context.Context, *sql.DB) error { return nil }
	}
	return &Core{
		Queries:    Queries{db: rebindDB{inner: db, rebind: rebind}},
		SQL:        db,
		rebind:     rebind,
		Checkpoint: checkpoint,
	}
}

// txQueries scopes the shared Queries to one transaction.
func (c *Core) txQueries(tx *sql.Tx) *Queries {
	return &Queries{db: rebindDB{inner: tx, rebind: c.rebind}}
}

// wrap applies the dialect rebind to direct transaction statements.
func (c *Core) wrap(tx *sql.Tx) DBTX { return rebindDB{inner: tx, rebind: c.rebind} }

// TS formats a timestamp the way every table stores it.
func TS(t time.Time) string { return ts(t) }

// RebindQuestion converts '?' placeholders to PostgreSQL's $1..$N. The
// shared queries contain no literal question marks inside SQL strings.
func RebindQuestion(q string) string {
	var b strings.Builder
	n := 0
	for i := 0; i < len(q); i++ {
		if q[i] == '?' {
			n++
			fmt.Fprintf(&b, "$%d", n)
			continue
		}
		b.WriteByte(q[i])
	}
	return b.String()
}

func (c *Core) WithTx(ctx context.Context, fn func(store.Tx) error) error {
	tx, err := c.SQL.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if err := fn(c.txQueries(tx)); err != nil {
		if rbErr := tx.Rollback(); rbErr != nil && rbErr != sql.ErrTxDone {
			return fmt.Errorf("%w (rollback failed: %v)", err, rbErr)
		}
		return err
	}
	return tx.Commit()
}

func (c *Core) SaveSafetyState(kind string, payload []byte) error {
	_, err := c.db.ExecContext(context.Background(),
		`INSERT INTO safety_state (kind, payload, updated_at) VALUES (?, ?, ?)
		 ON CONFLICT(kind) DO UPDATE SET payload=excluded.payload, updated_at=excluded.updated_at`,
		kind, string(payload), ts(time.Now()))
	return err
}

func (c *Core) LoadSafetyState(kind string) ([]byte, error) {
	var payload string
	err := c.db.QueryRowContext(context.Background(),
		`SELECT payload FROM safety_state WHERE kind=?`, kind).Scan(&payload)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return []byte(payload), nil
}

type PruneStats struct {
	Events    int64
	Outbox    int64
	Actions   int64
	Incidents int64
	Audit     int64
	Approvals int64
}

func (c *Core) Prune(ctx context.Context, dataRetention, auditRetention time.Duration) (PruneStats, error) {
	var stats PruneStats
	now := time.Now()

	if dataRetention > 0 {
		cutoff := ts(now.Add(-dataRetention))
		err := c.WithTx(ctx, func(tx store.Tx) error {
			q := tx.(*Queries)
			var err error
			// Order respects the event_outbox -> events foreign key.
			if stats.Outbox, err = q.execCount(ctx,
				`DELETE FROM event_outbox WHERE state='done' AND updated_at < ?`, cutoff); err != nil {
				return err
			}
			if stats.Events, err = q.execCount(ctx,
				`DELETE FROM events WHERE timestamp < ?
				 AND id NOT IN (SELECT event_row_id FROM event_outbox)`, cutoff); err != nil {
				return err
			}
			if stats.Actions, err = q.execCount(ctx,
				`DELETE FROM actions WHERE state='done' AND updated_at < ?`, cutoff); err != nil {
				return err
			}
			return nil
		})
		if err != nil {
			return stats, fmt.Errorf("prune operational data: %w", err)
		}
	}

	if auditRetention > 0 {
		cutoff := ts(now.Add(-auditRetention))
		const terminalOld = `SELECT id FROM incidents
			WHERE state IN ('RESOLVED','EXPIRED') AND updated_at < ?`
		err := c.WithTx(ctx, func(tx store.Tx) error {
			q := tx.(*Queries)
			var err error
			if stats.Approvals, err = q.execCount(ctx,
				`DELETE FROM approvals WHERE incident_id IN (`+terminalOld+`)`, cutoff); err != nil {
				return err
			}
			if stats.Audit, err = q.execCount(ctx,
				`DELETE FROM audit_log WHERE incident_id IN (`+terminalOld+`)`, cutoff); err != nil {
				return err
			}
			if stats.Incidents, err = q.execCount(ctx,
				`DELETE FROM incidents WHERE state IN ('RESOLVED','EXPIRED') AND updated_at < ?`, cutoff); err != nil {
				return err
			}
			return nil
		})
		if err != nil {
			return stats, fmt.Errorf("prune audit history: %w", err)
		}
	}

	if err := c.Checkpoint(ctx, c.SQL); err != nil {
		return stats, err
	}
	return stats, nil
}

func (q *Queries) execCount(ctx context.Context, query string, args ...any) (int64, error) {
	res, err := q.db.ExecContext(ctx, query, args...)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// --- incidents ---

func (q *Queries) CreateIncident(ctx context.Context, inc *types.Incident) error {
	stateChanged := inc.StateChangedAt
	if stateChanged.IsZero() {
		stateChanged = inc.OpenedAt
	}
	_, err := q.db.ExecContext(ctx, `
		INSERT INTO incidents (id, node, gpu_uuid, gpu_index, class, state, playbook,
		                       step_index, attempt, dry_run, signals_seen, opened_at, updated_at, state_changed_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		inc.ID, inc.Target.Node, inc.Target.GPUUUID, inc.Target.GPUIndex,
		string(inc.Class), string(inc.State), inc.Playbook,
		inc.StepIndex, inc.Attempt, b2i(inc.DryRun), inc.SignalSeen,
		ts(inc.OpenedAt), ts(inc.UpdatedAt), ts(stateChanged))
	return err
}

func (q *Queries) UpdateIncident(ctx context.Context, inc *types.Incident) error {
	var resolved any
	if inc.ResolvedAt != nil {
		resolved = ts(*inc.ResolvedAt)
	}
	res, err := q.db.ExecContext(ctx, `
		UPDATE incidents SET state=?, playbook=?, step_index=?, attempt=?, dry_run=?,
		                     signals_seen=?, updated_at=?, state_changed_at=?, resolved_at=?
		WHERE id=?`,
		string(inc.State), inc.Playbook, inc.StepIndex, inc.Attempt, b2i(inc.DryRun),
		inc.SignalSeen, ts(inc.UpdatedAt), ts(inc.StateChangedAt), resolved, inc.ID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return store.ErrNotFound
	}
	return nil
}

func (q *Queries) GetIncident(ctx context.Context, id string) (*types.Incident, error) {
	row := q.db.QueryRowContext(ctx, incidentSelect+` WHERE id=?`, id)
	return scanIncident(row)
}

func (q *Queries) GetOpenIncident(ctx context.Context, target types.Target, class types.ProblemClass) (*types.Incident, error) {
	row := q.db.QueryRowContext(ctx, incidentSelect+`
		WHERE node=? AND gpu_uuid=? AND class=? AND state NOT IN ('RESOLVED','EXPIRED')`,
		target.Node, target.GPUUUID, string(class))
	return scanIncident(row)
}

func (q *Queries) ListIncidents(ctx context.Context, f store.IncidentFilter) ([]*types.Incident, error) {
	qs := incidentSelect + ` WHERE 1=1`
	var args []any
	if len(f.States) > 0 {
		qs += ` AND state IN (?` + repeat(",?", len(f.States)-1) + `)`
		for _, st := range f.States {
			args = append(args, string(st))
		}
	}
	if f.Node != "" {
		qs += ` AND node=?`
		args = append(args, f.Node)
	}
	qs += ` ORDER BY opened_at DESC`
	if f.Limit > 0 {
		qs += ` LIMIT ?`
		args = append(args, f.Limit)
	}
	rows, err := q.db.QueryContext(ctx, qs, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []*types.Incident
	for rows.Next() {
		inc, err := scanIncident(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, inc)
	}
	return out, rows.Err()
}

func (q *Queries) CountPendingActions(ctx context.Context) (int, error) {
	var n int
	err := q.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM actions WHERE state='pending'`).Scan(&n)
	return n, err
}

func (q *Queries) CountIncidentsByState(ctx context.Context) (map[types.IncidentState]int, error) {
	rows, err := q.db.QueryContext(ctx, `SELECT state, COUNT(*) FROM incidents GROUP BY state`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := map[types.IncidentState]int{}
	for rows.Next() {
		var state string
		var n int
		if err := rows.Scan(&state, &n); err != nil {
			return nil, err
		}
		out[types.IncidentState(state)] = n
	}
	return out, rows.Err()
}

const incidentSelect = `
	SELECT id, node, gpu_uuid, gpu_index, class, state, playbook,
	       step_index, attempt, dry_run, signals_seen, opened_at, updated_at, state_changed_at, resolved_at
	FROM incidents`

type rowScanner interface{ Scan(dest ...any) error }

func scanIncident(r rowScanner) (*types.Incident, error) {
	var inc types.Incident
	var class, state, opened, updated, stateChanged string
	var dryRun int
	var resolved sql.NullString
	err := r.Scan(&inc.ID, &inc.Target.Node, &inc.Target.GPUUUID, &inc.Target.GPUIndex,
		&class, &state, &inc.Playbook, &inc.StepIndex, &inc.Attempt, &dryRun,
		&inc.SignalSeen, &opened, &updated, &stateChanged, &resolved)
	if err == sql.ErrNoRows {
		return nil, store.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	inc.Class = types.ProblemClass(class)
	inc.State = types.IncidentState(state)
	inc.DryRun = dryRun != 0
	inc.OpenedAt = parseTS(opened)
	inc.UpdatedAt = parseTS(updated)
	inc.StateChangedAt = parseTS(stateChanged)
	if inc.StateChangedAt.IsZero() {
		// Rows written before migration 0002 have no state-change timestamp;
		// fall back to the last update rather than the zero time.
		inc.StateChangedAt = inc.UpdatedAt
	}
	if resolved.Valid {
		t := parseTS(resolved.String)
		inc.ResolvedAt = &t
	}
	return &inc, nil
}

// --- audit ---

func (q *Queries) AppendAudit(ctx context.Context, e *types.AuditEntry) error {
	params, _ := json.Marshal(e.Params)
	_, err := q.db.ExecContext(ctx, `
		INSERT INTO audit_log (incident_id, time, from_state, to_state, actor, action, params, result, dry_run)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		e.IncidentID, ts(e.Time), string(e.FromState), string(e.ToState),
		e.Actor, e.Action, string(params), e.Result, b2i(e.DryRun))
	return err
}

func (q *Queries) AuditTrail(ctx context.Context, incidentID string) ([]*types.AuditEntry, error) {
	rows, err := q.db.QueryContext(ctx, `
		SELECT id, incident_id, time, from_state, to_state, actor, action, params, result, dry_run
		FROM audit_log WHERE incident_id=? ORDER BY id`, incidentID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []*types.AuditEntry
	for rows.Next() {
		var e types.AuditEntry
		var t, from, to, params string
		var dryRun int
		if err := rows.Scan(&e.ID, &e.IncidentID, &t, &from, &to, &e.Actor, &e.Action, &params, &e.Result, &dryRun); err != nil {
			return nil, err
		}
		e.Time = parseTS(t)
		e.FromState = types.IncidentState(from)
		e.ToState = types.IncidentState(to)
		e.DryRun = dryRun != 0
		_ = json.Unmarshal([]byte(params), &e.Params)
		out = append(out, &e)
	}
	return out, rows.Err()
}

// --- approvals ---

func (q *Queries) RecordApproval(ctx context.Context, a *types.Approval) error {
	_, err := q.db.ExecContext(ctx, `
		INSERT INTO approvals (incident_id, step_name, decision, actor, channel, at)
		VALUES (?, ?, ?, ?, ?, ?)`,
		a.IncidentID, a.StepName, string(a.Decision), a.Actor, a.Channel, ts(a.At))
	return err
}

func (q *Queries) LatestApproval(ctx context.Context, incidentID string) (*types.Approval, error) {
	row := q.db.QueryRowContext(ctx, `
		SELECT incident_id, step_name, decision, actor, channel, at
		FROM approvals WHERE incident_id=? ORDER BY id DESC LIMIT 1`, incidentID)
	var a types.Approval
	var decision, at string
	err := row.Scan(&a.IncidentID, &a.StepName, &decision, &a.Actor, &a.Channel, &at)
	if err == sql.ErrNoRows {
		return nil, store.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	a.Decision = types.ApprovalDecision(decision)
	a.At = parseTS(at)
	return &a, nil
}

// --- nodes ---

func (q *Queries) UpsertNode(ctx context.Context, n *types.Node) error {
	labels, _ := json.Marshal(n.Labels)
	gpus, _ := json.Marshal(n.GPUs)
	var lastSeen any
	if !n.AgentLastSeen.IsZero() {
		lastSeen = ts(n.AgentLastSeen)
	}
	_, err := q.db.ExecContext(ctx, `
		INSERT INTO nodes (name, node_uid, platform, labels, ssh_addr, bmc_addr, gpus, boot_id, paused, agent_last_seen)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(name) DO UPDATE SET
			node_uid=CASE WHEN excluded.node_uid<>'' THEN excluded.node_uid ELSE nodes.node_uid END,
			platform=excluded.platform, labels=excluded.labels,
			ssh_addr=excluded.ssh_addr, bmc_addr=excluded.bmc_addr,
			gpus=excluded.gpus, boot_id=excluded.boot_id,
			paused=excluded.paused, agent_last_seen=excluded.agent_last_seen`,
		n.Name, n.UID, n.Platform, string(labels), n.SSHAddr, n.BMCAddr, string(gpus),
		n.BootID, b2i(n.Paused), lastSeen)
	return err
}

func (q *Queries) UpsertAgentRegistration(ctx context.Context, n *types.Node) error {
	gpus, _ := json.Marshal(n.GPUs)
	var lastSeen any
	if !n.AgentLastSeen.IsZero() {
		lastSeen = ts(n.AgentLastSeen)
	}
	_, err := q.db.ExecContext(ctx, `
		INSERT INTO nodes (name, node_uid, platform, gpus, boot_id, agent_last_seen)
		VALUES (?, ?, 'agent', ?, ?, ?)
		ON CONFLICT(name) DO UPDATE SET
			node_uid=CASE WHEN excluded.node_uid<>'' THEN excluded.node_uid ELSE nodes.node_uid END,
			gpus=excluded.gpus, boot_id=excluded.boot_id,
			agent_last_seen=excluded.agent_last_seen`,
		n.Name, n.UID, string(gpus), n.BootID, lastSeen)
	return err
}

func (c *Core) ApplyNodeConfigPauses(ctx context.Context, pausedNodes []string) error {
	tx, err := c.SQL.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := c.wrap(tx).ExecContext(ctx, `UPDATE nodes SET paused=0 WHERE paused=1`); err != nil {
		return err
	}
	for _, name := range pausedNodes {
		if _, err := c.wrap(tx).ExecContext(ctx, `
			INSERT INTO nodes (name, platform, paused) VALUES (?, 'config', 1)
			ON CONFLICT(name) DO UPDATE SET paused=1`, name); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (q *Queries) GetNode(ctx context.Context, name string) (*types.Node, error) {
	row := q.db.QueryRowContext(ctx, nodeSelect+` WHERE name=?`, name)
	return scanNode(row)
}

func (q *Queries) ListNodes(ctx context.Context) ([]*types.Node, error) {
	rows, err := q.db.QueryContext(ctx, nodeSelect+` ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []*types.Node
	for rows.Next() {
		n, err := scanNode(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

const nodeSelect = `
	SELECT name, node_uid, platform, labels, ssh_addr, bmc_addr, gpus, boot_id, paused, agent_last_seen
	FROM nodes`

func scanNode(r rowScanner) (*types.Node, error) {
	var n types.Node
	var labels, gpus string
	var paused int
	var lastSeen sql.NullString
	err := r.Scan(&n.Name, &n.UID, &n.Platform, &labels, &n.SSHAddr, &n.BMCAddr, &gpus, &n.BootID, &paused, &lastSeen)
	if err == sql.ErrNoRows {
		return nil, store.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	n.Paused = paused != 0
	_ = json.Unmarshal([]byte(labels), &n.Labels)
	_ = json.Unmarshal([]byte(gpus), &n.GPUs)
	if lastSeen.Valid {
		n.AgentLastSeen = parseTS(lastSeen.String)
	}
	return &n, nil
}

// --- accelerator runtime reports ---

func (c *Core) UpsertAcceleratorReport(ctx context.Context, report *types.AgentAcceleratorReport) error {
	if report == nil {
		return fmt.Errorf("upsert accelerator report: report is required")
	}
	if err := report.Validate(); err != nil {
		return fmt.Errorf("upsert accelerator report: %w", err)
	}
	payload, err := marshalAcceleratorReport(report)
	if err != nil {
		return fmt.Errorf("upsert accelerator report: %w", err)
	}

	tx, err := c.SQL.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin accelerator report upsert: %w", err)
	}
	rollback := func(cause error) error {
		if rbErr := tx.Rollback(); rbErr != nil && rbErr != sql.ErrTxDone {
			return fmt.Errorf("%w (rollback failed: %v)", cause, rbErr)
		}
		return cause
	}

	var current acceleratorReportPayload
	err = c.wrap(tx).QueryRowContext(ctx, acceleratorReportPayloadSelect+` WHERE node=? AND vendor=?`, report.Node, string(report.Vendor)).Scan(
		&current.ObservedAtNS, &current.NodeUID, &current.ProfileDigest, &current.ProfileUID, &current.ProfileGeneration, &current.Readiness,
		&current.ReasonsJSON, &current.DevicesJSON, &current.DriverVersion,
		&current.RuntimeVersion, &current.TopologySafety, &current.CapabilitiesJSON)
	switch {
	case err == sql.ErrNoRows:
		if _, err := c.wrap(tx).ExecContext(ctx, `
			INSERT INTO accelerator_reports (
				node, vendor, node_uid, observed_at_ns, profile_digest, profile_uid, profile_generation, readiness, reasons_json,
				devices_json, driver_version, runtime_version, topology_safety, capabilities_json
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			report.Node, string(report.Vendor), payload.NodeUID, payload.ObservedAtNS, payload.ProfileDigest, payload.ProfileUID, payload.ProfileGeneration,
			payload.Readiness, payload.ReasonsJSON, payload.DevicesJSON,
			payload.DriverVersion, payload.RuntimeVersion, payload.TopologySafety,
			payload.CapabilitiesJSON); err != nil {
			return rollback(fmt.Errorf("insert accelerator report: %w", err))
		}
	case err != nil:
		return rollback(fmt.Errorf("read current accelerator report: %w", err))
	case payload.ObservedAtNS < current.ObservedAtNS:
		return rollback(store.ErrStaleAcceleratorReport)
	case payload.ObservedAtNS == current.ObservedAtNS:
		if payload.equal(current) {
			return tx.Commit()
		}
		return rollback(store.ErrStaleAcceleratorReport)
	default:
		// The timestamp predicate is repeated in SQL. It makes a concurrent
		// newer writer win even if this transaction read an earlier snapshot.
		res, err := c.wrap(tx).ExecContext(ctx, `
			UPDATE accelerator_reports SET
				node_uid=?, observed_at_ns=?, profile_digest=?, profile_uid=?, profile_generation=?, readiness=?, reasons_json=?,
				devices_json=?, driver_version=?, runtime_version=?, topology_safety=?,
				capabilities_json=?
			WHERE node=? AND vendor=? AND observed_at_ns < ?`,
			payload.NodeUID, payload.ObservedAtNS, payload.ProfileDigest, payload.ProfileUID, payload.ProfileGeneration, payload.Readiness,
			payload.ReasonsJSON, payload.DevicesJSON, payload.DriverVersion,
			payload.RuntimeVersion, payload.TopologySafety, payload.CapabilitiesJSON,
			report.Node, string(report.Vendor), payload.ObservedAtNS)
		if err != nil {
			return rollback(fmt.Errorf("update accelerator report: %w", err))
		}
		if n, err := res.RowsAffected(); err != nil {
			return rollback(fmt.Errorf("check accelerator report update: %w", err))
		} else if n != 1 {
			return rollback(store.ErrStaleAcceleratorReport)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit accelerator report upsert: %w", err)
	}
	return nil
}

func (q *Queries) GetAcceleratorReport(ctx context.Context, node string, vendor types.AcceleratorVendor) (*types.AgentAcceleratorReport, error) {
	row := q.db.QueryRowContext(ctx, acceleratorReportSelect+` WHERE node=? AND vendor=?`, node, string(vendor))
	return scanAcceleratorReport(row)
}

func (q *Queries) ListAcceleratorReports(ctx context.Context, node string) ([]*types.AgentAcceleratorReport, error) {
	query := acceleratorReportSelect
	var args []any
	if node != "" {
		query += ` WHERE node=?`
		args = append(args, node)
	}
	query += ` ORDER BY node, vendor`
	rows, err := q.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var reports []*types.AgentAcceleratorReport
	for rows.Next() {
		report, err := scanAcceleratorReport(rows)
		if err != nil {
			return nil, err
		}
		reports = append(reports, report)
	}
	return reports, rows.Err()
}

const acceleratorReportSelect = `
	SELECT node, vendor, node_uid, observed_at_ns, profile_digest, profile_uid, profile_generation, readiness, reasons_json,
	       devices_json, driver_version, runtime_version, topology_safety, capabilities_json
	FROM accelerator_reports`

const acceleratorReportPayloadSelect = `
	SELECT observed_at_ns, node_uid, profile_digest, profile_uid, profile_generation, readiness, reasons_json, devices_json,
	       driver_version, runtime_version, topology_safety, capabilities_json
	FROM accelerator_reports`

type acceleratorReportPayload struct {
	ObservedAtNS      int64
	NodeUID           string
	ProfileDigest     string
	ProfileUID        string
	ProfileGeneration int64
	Readiness         string
	ReasonsJSON       string
	DevicesJSON       string
	DriverVersion     string
	RuntimeVersion    string
	TopologySafety    string
	CapabilitiesJSON  string
}

func (p acceleratorReportPayload) equal(other acceleratorReportPayload) bool {
	return p == other
}

func marshalAcceleratorReport(report *types.AgentAcceleratorReport) (acceleratorReportPayload, error) {
	reasons, err := json.Marshal(report.ReadinessReasons)
	if err != nil {
		return acceleratorReportPayload{}, fmt.Errorf("marshal readiness reasons: %w", err)
	}
	devices, err := json.Marshal(report.Devices)
	if err != nil {
		return acceleratorReportPayload{}, fmt.Errorf("marshal devices: %w", err)
	}
	capabilities, err := json.Marshal(report.Capabilities)
	if err != nil {
		return acceleratorReportPayload{}, fmt.Errorf("marshal capabilities: %w", err)
	}
	return acceleratorReportPayload{
		ObservedAtNS:      report.ObservedAt.UnixNano(),
		NodeUID:           report.NodeUID,
		ProfileDigest:     report.ProfileDigest,
		ProfileUID:        report.ProfileUID,
		ProfileGeneration: report.ProfileGeneration,
		Readiness:         string(report.Readiness),
		ReasonsJSON:       string(reasons),
		DevicesJSON:       string(devices),
		DriverVersion:     report.DriverVersion,
		RuntimeVersion:    report.RuntimeVersion,
		TopologySafety:    string(report.TopologySafety),
		CapabilitiesJSON:  string(capabilities),
	}, nil
}

func scanAcceleratorReport(r rowScanner) (*types.AgentAcceleratorReport, error) {
	var report types.AgentAcceleratorReport
	var vendor string
	var observedAtNS int64
	var reasons, devices, capabilities string
	var readiness, topologySafety string
	err := r.Scan(&report.Node, &vendor, &report.NodeUID, &observedAtNS, &report.ProfileDigest, &report.ProfileUID, &report.ProfileGeneration,
		&readiness, &reasons, &devices, &report.DriverVersion,
		&report.RuntimeVersion, &topologySafety, &capabilities)
	if err == sql.ErrNoRows {
		return nil, store.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	report.Vendor = types.AcceleratorVendor(vendor)
	report.ObservedAt = time.Unix(0, observedAtNS).UTC()
	report.Readiness = types.AcceleratorReadiness(readiness)
	report.TopologySafety = types.AcceleratorTopologySafety(topologySafety)
	if err := json.Unmarshal([]byte(reasons), &report.ReadinessReasons); err != nil {
		return nil, fmt.Errorf("accelerator report %s/%s: corrupt readiness reasons: %w", report.Node, report.Vendor, err)
	}
	if err := json.Unmarshal([]byte(devices), &report.Devices); err != nil {
		return nil, fmt.Errorf("accelerator report %s/%s: corrupt devices: %w", report.Node, report.Vendor, err)
	}
	if err := json.Unmarshal([]byte(capabilities), &report.Capabilities); err != nil {
		return nil, fmt.Errorf("accelerator report %s/%s: corrupt capabilities: %w", report.Node, report.Vendor, err)
	}
	if err := report.Validate(); err != nil {
		return nil, fmt.Errorf("accelerator report %s/%s: corrupt persisted report: %w", report.Node, report.Vendor, err)
	}
	return &report, nil
}

// --- action queue ---

func (q *Queries) EnqueueAction(ctx context.Context, node, incidentID string, a types.Action) error {
	params, err := json.Marshal(a.Params)
	if err != nil {
		return err
	}
	now := ts(time.Now())
	_, err = q.db.ExecContext(ctx, `
		INSERT INTO actions (id, node, incident_id, type, params, timeout_ns, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO NOTHING`,
		a.ID, node, incidentID, string(a.Type), string(params), int64(a.Timeout), now, now)
	return err
}

func (c *Core) ClaimNextAction(ctx context.Context, node, bootID string, leaseDuration time.Duration) (*types.QueuedAction, error) {
	if leaseDuration <= 0 {
		return nil, fmt.Errorf("claim action: lease duration must be positive")
	}
	token, err := newLeaseToken()
	if err != nil {
		return nil, fmt.Errorf("claim action: generate lease token: %w", err)
	}
	now := time.Now()
	nowNS := now.UnixNano()
	minimumLeaseNS := int64(leaseDuration)
	row := c.db.QueryRowContext(ctx, `
		UPDATE actions
		SET state='leased', lease_token=?,
		    lease_expires_at_ns=? + (CASE WHEN timeout_ns > ? THEN timeout_ns ELSE ? END),
		    attempts=attempts+1, executor_boot_id=?,
		    updated_at=?
		WHERE id = (
			SELECT candidate.id
			FROM actions AS candidate
			WHERE candidate.node=?
			  AND (candidate.state='pending'
			       OR (candidate.state='leased' AND candidate.lease_expires_at_ns <= ?))
			  AND NOT EXISTS (
				SELECT 1 FROM actions AS held
				WHERE held.node=candidate.node
				  AND held.state='leased'
				  AND held.lease_expires_at_ns > ?
			  )
			ORDER BY candidate.created_at, candidate.id
			LIMIT 1
		)
		RETURNING id, node, incident_id, type, params, timeout_ns, state, result,
		          lease_token, lease_expires_at_ns, attempts, executor_boot_id`,
		token, nowNS, minimumLeaseNS, minimumLeaseNS, bootID, ts(now), node, nowNS, nowNS)
	return scanAction(row)
}

// CancelPendingActionsForIncident tombstones the incident's undelivered
// actions. Only 'pending' work is cancellable: a leased action may already
// be executing on the node, and pretending it was revoked would lie about a
// possible side effect. Returns how many actions were cancelled.
func (q *Queries) CancelPendingActionsForIncident(ctx context.Context, incidentID string) (int64, error) {
	if incidentID == "" {
		return 0, fmt.Errorf("cancel actions: incident ID is required")
	}
	out, err := q.db.ExecContext(ctx, `
		UPDATE actions SET state='cancelled', updated_at=?
		WHERE incident_id=? AND state='pending'`, ts(time.Now()), incidentID)
	if err != nil {
		return 0, err
	}
	return out.RowsAffected()
}

func (q *Queries) NextPendingAction(ctx context.Context, node string) (*types.QueuedAction, error) {
	row := q.db.QueryRowContext(ctx, actionSelect+`
		WHERE node=? AND state='pending' ORDER BY created_at, id LIMIT 1`, node)
	return scanAction(row)
}

func (q *Queries) CompleteClaimedAction(ctx context.Context, actionID, leaseToken, bootID string, res types.ActionResult) error {
	if leaseToken == "" {
		return store.ErrLeaseLost
	}
	blob, err := json.Marshal(res)
	if err != nil {
		return err
	}
	now := time.Now()
	// The boot guard binds the result to the node boot that claimed the
	// action: a result posted after an unnoticed reboot is not evidence
	// that the side effect completed on the boot that started it.
	out, err := q.db.ExecContext(ctx, `
		UPDATE actions
		SET state='done', result=?, lease_token='', lease_expires_at_ns=0, updated_at=?
		WHERE id=? AND state='leased' AND lease_token=? AND lease_expires_at_ns > ?
		  AND (executor_boot_id='' OR ?='' OR executor_boot_id=?)`,
		string(blob), ts(now), actionID, leaseToken, now.UnixNano(), bootID, bootID)
	if err != nil {
		return err
	}
	if n, _ := out.RowsAffected(); n > 0 {
		return nil
	}
	// Preserve ErrNotFound for an unknown ID; distinguish a boot mismatch
	// from a wrong/expired lease so operators see the reboot.
	current, err := q.GetAction(ctx, actionID)
	if err != nil {
		return err
	}
	if bootID != "" && current.ExecutorBootID != "" && current.ExecutorBootID != bootID &&
		current.LeaseToken == leaseToken {
		return store.ErrExecutorBootMismatch
	}
	return store.ErrLeaseLost
}

func (q *Queries) CompleteAction(ctx context.Context, actionID string, res types.ActionResult) error {
	blob, err := json.Marshal(res)
	if err != nil {
		return err
	}
	out, err := q.db.ExecContext(ctx, `
		UPDATE actions SET state='done', result=?, lease_token='', lease_expires_at_ns=0, updated_at=?
		WHERE id=? AND state='pending'`,
		string(blob), ts(time.Now()), actionID)
	if err != nil {
		return err
	}
	if n, _ := out.RowsAffected(); n > 0 {
		return nil
	}
	if _, err := q.GetAction(ctx, actionID); err != nil {
		return err
	}
	return store.ErrLeaseLost
}

func (q *Queries) GetAction(ctx context.Context, actionID string) (*types.QueuedAction, error) {
	row := q.db.QueryRowContext(ctx, actionSelect+` WHERE id=?`, actionID)
	return scanAction(row)
}

const actionSelect = `
	SELECT id, node, incident_id, type, params, timeout_ns, state, result,
	       lease_token, lease_expires_at_ns, attempts, executor_boot_id
	FROM actions`

func scanAction(r rowScanner) (*types.QueuedAction, error) {
	var qa types.QueuedAction
	var actionType, params, state, result string
	var timeoutNS, leaseExpiresAtNS int64
	err := r.Scan(&qa.Action.ID, &qa.Node, &qa.IncidentID, &actionType, &params, &timeoutNS, &state, &result,
		&qa.LeaseToken, &leaseExpiresAtNS, &qa.Attempts, &qa.ExecutorBootID)
	if err == sql.ErrNoRows {
		return nil, store.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	qa.Action.Type = types.ActionType(actionType)
	qa.Action.Timeout = time.Duration(timeoutNS)
	if err := json.Unmarshal([]byte(params), &qa.Action.Params); err != nil {
		return nil, fmt.Errorf("action %s: corrupt params: %w", qa.Action.ID, err)
	}
	qa.Done = state == "done"
	qa.Cancelled = state == "cancelled"
	if state == "leased" && leaseExpiresAtNS > 0 {
		qa.LeaseExpiresAt = time.Unix(0, leaseExpiresAtNS).UTC()
	} else {
		qa.LeaseToken = ""
	}
	if qa.Done && result != "" {
		qa.Result = &types.ActionResult{}
		if err := json.Unmarshal([]byte(result), qa.Result); err != nil {
			return nil, fmt.Errorf("action %s: corrupt result: %w", qa.Action.ID, err)
		}
	}
	return &qa, nil
}

func newLeaseToken() (string, error) {
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw[:]), nil
}

// --- events / workflow outbox ---

func (c *Core) WriteEvent(ctx context.Context, ev *types.AgentEvent) (bool, error) {
	return c.ArchiveAndEnqueueEvent(ctx, ev)
}

func (c *Core) ArchiveAndEnqueueEvent(ctx context.Context, ev *types.AgentEvent) (bool, error) {
	tx, err := c.SQL.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("begin event archive transaction: %w", err)
	}
	rollback := func(cause error) (bool, error) {
		if rbErr := tx.Rollback(); rbErr != nil && rbErr != sql.ErrTxDone {
			return false, fmt.Errorf("%w (rollback failed: %v)", cause, rbErr)
		}
		return false, cause
	}

	// RETURNING keeps this portable: PostgreSQL has no LastInsertId, and a
	// conflicting (duplicate) insert returns no row on both engines.
	var eventRowID int64
	err = c.wrap(tx).QueryRowContext(ctx, `
		INSERT INTO events (event_id, node, gpu_index, gpu_uuid, xid, raw, timestamp)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT DO NOTHING
		RETURNING id`,
		ev.EventID, ev.Node, ev.GPUIndex, ev.GPUUUID, ev.XID, ev.Raw, ts(ev.Timestamp)).Scan(&eventRowID)
	if errors.Is(err, sql.ErrNoRows) {
		if err := tx.Commit(); err != nil {
			return false, fmt.Errorf("commit duplicate event archive: %w", err)
		}
		return false, nil
	}
	if err != nil {
		return rollback(fmt.Errorf("archive agent event: %w", err))
	}
	now := ts(time.Now())
	if _, err := c.wrap(tx).ExecContext(ctx, `
		INSERT INTO event_outbox (event_row_id, created_at, updated_at)
		VALUES (?, ?, ?)`, eventRowID, now, now); err != nil {
		return rollback(fmt.Errorf("enqueue archived event: %w", err))
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("commit event archive and enqueue: %w", err)
	}
	return true, nil
}

func (c *Core) ClaimNextEvent(ctx context.Context, workerID string, leaseDuration time.Duration) (*store.ClaimedEvent, error) {
	if workerID == "" {
		return nil, fmt.Errorf("claim event: worker ID is required")
	}
	if leaseDuration <= 0 {
		return nil, fmt.Errorf("claim event: lease duration must be positive")
	}
	token, err := newLeaseToken()
	if err != nil {
		return nil, fmt.Errorf("claim event: generate lease token: %w", err)
	}
	now := time.Now()
	nowNS := now.UnixNano()
	expiresAtNS := now.Add(leaseDuration).UnixNano()
	var outboxID int64
	err = c.db.QueryRowContext(ctx, `
		UPDATE event_outbox
		SET state='leased', attempts=attempts+1, lease_owner=?, lease_token=?,
		    lease_expires_at_ns=?, updated_at=?
		WHERE id = (
			SELECT candidate.id
			FROM event_outbox AS candidate
			WHERE candidate.state='pending'
			   OR (candidate.state='leased' AND candidate.lease_expires_at_ns <= ?)
			ORDER BY candidate.created_at, candidate.id
			LIMIT 1
		)
		RETURNING id`,
		workerID, token, expiresAtNS, ts(now), nowNS).Scan(&outboxID)
	if err == sql.ErrNoRows {
		return nil, store.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("claim event: %w", err)
	}

	row := c.db.QueryRowContext(ctx, eventOutboxSelect+` WHERE o.id=?`, outboxID)
	claimed, err := scanClaimedEvent(row)
	if err != nil {
		return nil, err
	}
	return claimed, nil
}

func (c *Core) ProcessClaimedEvent(ctx context.Context, outboxID int64, leaseToken string, fn func(store.Tx) error) error {
	if fn == nil {
		return fmt.Errorf("process claimed event: callback is required")
	}
	tx, err := c.SQL.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin claimed-event transaction: %w", err)
	}
	rollback := func(cause error) error {
		if rbErr := tx.Rollback(); rbErr != nil && rbErr != sql.ErrTxDone {
			return fmt.Errorf("%w (rollback failed: %v)", cause, rbErr)
		}
		return cause
	}

	q := c.txQueries(tx)
	if err := q.requireCurrentEventLease(ctx, outboxID, leaseToken, time.Now()); err != nil {
		return rollback(err)
	}
	if err := fn(q); err != nil {
		return rollback(err)
	}
	// CompleteClaimedEvent repeats the predicate at the point of commit: the
	// callback may have run long enough for the lease to expire. Returning
	// ErrEventLeaseLost rolls every callback mutation back in that case.
	if err := q.CompleteClaimedEvent(ctx, outboxID, leaseToken); err != nil {
		return rollback(err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit claimed-event transaction: %w", err)
	}
	return nil
}

func (q *Queries) CompleteClaimedEvent(ctx context.Context, outboxID int64, leaseToken string) error {
	if outboxID <= 0 || leaseToken == "" {
		return store.ErrEventLeaseLost
	}
	now := time.Now()
	res, err := q.db.ExecContext(ctx, `
		UPDATE event_outbox
		SET state='done', lease_owner='', lease_token='', lease_expires_at_ns=0,
		    completed_at=?, updated_at=?
		WHERE id=? AND state='leased' AND lease_token=? AND lease_expires_at_ns > ?`,
		ts(now), ts(now), outboxID, leaseToken, now.UnixNano())
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n > 0 {
		return nil
	}
	var exists int
	err = q.db.QueryRowContext(ctx, `SELECT 1 FROM event_outbox WHERE id=?`, outboxID).Scan(&exists)
	if err == sql.ErrNoRows {
		return store.ErrNotFound
	}
	if err != nil {
		return err
	}
	return store.ErrEventLeaseLost
}

func (q *Queries) requireCurrentEventLease(ctx context.Context, outboxID int64, leaseToken string, now time.Time) error {
	if outboxID <= 0 || leaseToken == "" {
		return store.ErrEventLeaseLost
	}
	var id int64
	err := q.db.QueryRowContext(ctx, `
		SELECT id FROM event_outbox
		WHERE id=? AND state='leased' AND lease_token=? AND lease_expires_at_ns > ?`,
		outboxID, leaseToken, now.UnixNano()).Scan(&id)
	if err == nil {
		return nil
	}
	if err != sql.ErrNoRows {
		return err
	}
	var exists int
	err = q.db.QueryRowContext(ctx, `SELECT 1 FROM event_outbox WHERE id=?`, outboxID).Scan(&exists)
	if err == sql.ErrNoRows {
		return store.ErrNotFound
	}
	if err != nil {
		return err
	}
	return store.ErrEventLeaseLost
}

const eventOutboxSelect = `
	SELECT o.id, e.event_id, e.node, e.gpu_index, e.gpu_uuid, e.xid, e.raw, e.timestamp,
	       o.attempts, o.lease_token, o.lease_expires_at_ns
	FROM event_outbox AS o
	JOIN events AS e ON e.id=o.event_row_id`

func scanClaimedEvent(r rowScanner) (*store.ClaimedEvent, error) {
	var claimed store.ClaimedEvent
	var timestamp string
	var leaseExpiresAtNS int64
	err := r.Scan(&claimed.OutboxID, &claimed.Event.EventID, &claimed.Event.Node,
		&claimed.Event.GPUIndex, &claimed.Event.GPUUUID, &claimed.Event.XID,
		&claimed.Event.Raw, &timestamp, &claimed.Attempt, &claimed.LeaseToken,
		&leaseExpiresAtNS)
	if err == sql.ErrNoRows {
		return nil, store.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	claimed.Event.Timestamp = parseTS(timestamp)
	if claimed.LeaseToken == "" || leaseExpiresAtNS <= 0 {
		return nil, fmt.Errorf("event outbox %d: claimed row has no active lease", claimed.OutboxID)
	}
	claimed.LeaseExpiresAt = time.Unix(0, leaseExpiresAtNS).UTC()
	return &claimed, nil
}

// --- helpers ---

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

func ts(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

func parseTS(s string) time.Time {
	t, _ := time.Parse(time.RFC3339Nano, s)
	return t
}

func repeat(s string, n int) string {
	out := ""
	for i := 0; i < n; i++ {
		out += s
	}
	return out
}
