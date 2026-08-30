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

	"github.com/kubeneuron/kubeneuron/internal/metrics"
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
	// SkipLocked is the dialect's row-locking clause appended to the
	// event-outbox candidate select: " FOR UPDATE SKIP LOCKED" on PostgreSQL,
	// empty on SQLite (whose single writer connection already serializes
	// claimers). It lets concurrent outbox workers lock and claim distinct rows
	// in parallel instead of all contending on the single oldest row. It is
	// deliberately NOT used for the action queue, whose "at most one unexpired
	// lease per node" invariant requires every claimer to converge on the same
	// row rather than fan out to sibling actions of the same node.
	SkipLocked string
}

// MaxActionAttempts caps how many leases one action may be issued before it is
// dead-lettered. A poison action — repeatedly claimed, never completed (a
// crash loop, or an incident a human already took over) — must stop re-entering
// the claimable pool on every lease expiry instead of retrying unboundedly.
const MaxActionAttempts = 8

// MaxEventAttempts caps how many leases one outbox event may be issued before
// it is dead-lettered, mirroring MaxActionAttempts for the action queue. A
// deterministically-failing ("poison") event must stop re-leasing forever and
// aborting each drain batch; past the budget it leaves the claimable pool.
const MaxEventAttempts = 8

// terminalIncidentStates is the set of incident states past which no queued
// action may be handed out: the incident is resolved, expired, or parked for a
// human (quarantine fails closed to NEEDS_HUMAN). CancelPendingActionsForIncident
// runs once at terminalization and spares an action under an unexpired lease; if
// that action's agent then crashes, the lease expires and — without this guard —
// the action would re-enter the claimable pool and be handed to a restarted
// agent for an incident a human already took over. The join is on incident_id,
// so an action with no matching incident row (unstamped) stays claimable.
//
// This SQL literal must stay in lockstep with types.IncidentState.Halted —
// the single definition of "automation has ended" — and a test pins the two
// together so a new state cannot silently join only one of them.
const terminalIncidentStates = `('RESOLVED','EXPIRED','NEEDS_HUMAN')`

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
			//
			// BOTH terminal states, for the same reason the actions prune takes
			// all three of its own — and this queue got the dead-letter
			// mechanism in the same round as that one and not the prune.
			//
			// The cost here is worse than accumulation. The events delete below
			// spares any row still referenced by the outbox, so every dead
			// outbox row pins its raw event past the configured retention
			// FOREVER: kernel fault text that was promised to age out does not.
			if stats.Outbox, err = q.execCount(ctx,
				`DELETE FROM event_outbox WHERE state IN `+terminalEventStates+` AND updated_at < ?`, cutoff); err != nil {
				return err
			}
			if stats.Events, err = q.execCount(ctx,
				`DELETE FROM events WHERE timestamp < ?
				 AND id NOT IN (SELECT event_row_id FROM event_outbox)`, cutoff); err != nil {
				return err
			}
			// Terminal actions are prunable: 'done' completed, 'dead' exhausted its
			// attempt budget, 'cancelled' was tombstoned when its incident
			// terminalized. Leaving 'dead'/'cancelled' rows unpruned let them
			// accumulate forever.
			if stats.Actions, err = q.execCount(ctx,
				`DELETE FROM actions WHERE state IN `+terminalActionStates+` AND updated_at < ?`, cutoff); err != nil {
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
			// Delete any actions still stamped with an incident being pruned, in
			// the SAME transaction, BEFORE the incident row disappears. The
			// terminal-incident claim guard in ClaimNextAction (NOT EXISTS ...
			// incidents ... terminal) only keeps a spared expired-lease action out
			// of the pool WHILE its incident row exists; once Prune removes the
			// incident the join is vacuously satisfied and a stale gpu_reset/reboot
			// becomes claimable again. Actions with no matching pruned incident
			// (incident_id='' or a janitor stamp) are not in the terminalOld set and
			// stay untouched.
			var prunedActions int64
			if prunedActions, err = q.execCount(ctx,
				`DELETE FROM actions WHERE incident_id IN (`+terminalOld+`)`, cutoff); err != nil {
				return err
			}
			stats.Actions += prunedActions
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
		INSERT INTO incidents (id, node, gpu_uuid, gpu_index, pci_addr, class, state, playbook,
		                       step_index, attempt, dry_run, signals_seen, remediation_slot_held, approval_epoch,
		                       vendor, opened_at, updated_at, state_changed_at, version)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		inc.ID, inc.Target.Node, inc.Target.GPUUUID, inc.Target.GPUIndex,
		types.NormalizePCIAddress(inc.Target.PCIAddr),
		string(inc.Class), string(inc.State), inc.Playbook,
		inc.StepIndex, inc.Attempt, b2i(inc.DryRun), inc.SignalSeen, b2i(inc.RemediationSlotHeld), inc.ApprovalEpoch,
		string(inc.Vendor), ts(inc.OpenedAt), ts(inc.UpdatedAt), ts(stateChanged), inc.Version)
	return err
}

// UpdateIncident persists inc under an optimistic-concurrency guard: it matches
// the row by id AND version and bumps the stored version, so a writer holding a
// stale snapshot cannot silently overwrite a row a concurrent writer advanced.
// A 0-row update is disambiguated: ErrNotFound when the row is gone, ErrConflict
// when it exists with a newer version. On success inc.Version is advanced to the
// value just persisted so the caller can keep writing without re-reading.
func (q *Queries) UpdateIncident(ctx context.Context, inc *types.Incident) error {
	var resolved any
	if inc.ResolvedAt != nil {
		resolved = ts(*inc.ResolvedAt)
	}
	res, err := q.db.ExecContext(ctx, `
		UPDATE incidents SET state=?, playbook=?, step_index=?, attempt=?, dry_run=?,
		                     signals_seen=?, remediation_slot_held=?, approval_epoch=?, vendor=?, updated_at=?, state_changed_at=?, resolved_at=?,
		                     version=version+1
		WHERE id=? AND version=?`,
		string(inc.State), inc.Playbook, inc.StepIndex, inc.Attempt, b2i(inc.DryRun),
		inc.SignalSeen, b2i(inc.RemediationSlotHeld), inc.ApprovalEpoch, string(inc.Vendor),
		ts(inc.UpdatedAt), ts(inc.StateChangedAt), resolved, inc.ID, inc.Version)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		// No row matched (id, version). Either the incident is gone or a
		// concurrent writer moved its version on; distinguish the two so the
		// caller can retry a conflict but surface a genuine disappearance.
		var current int
		switch err := q.db.QueryRowContext(ctx,
			`SELECT version FROM incidents WHERE id=?`, inc.ID).Scan(&current); {
		case err == sql.ErrNoRows:
			return store.ErrNotFound
		case err != nil:
			return err
		default:
			return store.ErrConflict
		}
	}
	inc.Version++
	return nil
}

func (q *Queries) GetIncident(ctx context.Context, id string) (*types.Incident, error) {
	row := q.db.QueryRowContext(ctx, incidentSelect+` WHERE id=?`, id)
	return scanIncident(row)
}

// openIncidentTail orders and clips every open-incident lookup. More than one
// row can match the coarsest of them (a node-scoped signal against several
// unattributed device incidents), and QueryRow would then pick an arbitrary
// one — so which incident a signal joined could differ between two identical
// calls. Oldest-first makes it the incident that has been waiting longest.
const openIncidentTail = ` AND state NOT IN ('RESOLVED','EXPIRED') ORDER BY opened_at LIMIT 1`

// GetOpenIncident finds the non-terminal incident a signal belongs to, or
// ErrNotFound.
//
// The lookup is layered because a device has two possible identities and they
// do not always arrive together. A signal that carries a GPU UUID is matched
// on the UUID exactly as it always was; only if that finds nothing does the
// PCI address get a say. So the attributed path — the common one — is
// unchanged, and the PCI address can never redirect a signal that already
// names its device.
//
// The second layer is the one that was missing. A kernel fault that knocks a
// GPU off the bus names it only by PCI address, so its incident is opened
// unattributed; when the vendor tool resolves that same address to a real UUID
// seconds later, this returns that unattributed incident as a PROMOTION
// candidate (see PromoteIncidentTarget) instead of ErrNotFound. Without it the
// precise signal either opened a second incident or was thrown away, and the
// original incident stayed unattributed until the ladder had cordoned and
// drained the node and then refused its reset for want of a UUID.
//
// The candidate is restricted to rows with an EMPTY gpu_uuid. A row that
// already carries a different UUID for this PCI address is a device that was
// replaced, not the same device seen more precisely, and attaching a new GPU's
// fault to the old GPU's incident would point every later decision — reset
// target, holder check, evidence gate — at a device that is no longer there.
func (q *Queries) GetOpenIncident(ctx context.Context, target types.Target, class types.ProblemClass) (*types.Incident, error) {
	pci := types.NormalizePCIAddress(target.PCIAddr)
	if target.GPUUUID != "" {
		inc, err := scanIncident(q.db.QueryRowContext(ctx, incidentSelect+`
			WHERE node=? AND gpu_uuid=? AND class=?`+openIncidentTail,
			target.Node, target.GPUUUID, string(class)))
		if err != store.ErrNotFound || pci == "" {
			return inc, err
		}
		// No incident for this device's UUID, but this signal names a bus
		// address: an incident opened for that address before anything could
		// resolve it is this same physical device, and it is promotable.
		return scanIncident(q.db.QueryRowContext(ctx, incidentSelect+`
			WHERE node=? AND gpu_uuid='' AND pci_addr=? AND class=?`+openIncidentTail,
			target.Node, pci, string(class)))
	}
	if pci != "" {
		// An unattributed signal that names a bus address is matched on that
		// address, whether or not the incident has since been promoted off it.
		// Matching it on gpu_uuid='' instead is what merged two distinct
		// unattributed GPUs of one node into a single incident, and what would
		// now open a SECOND incident for a device whose incident had just been
		// promoted.
		inc, err := scanIncident(q.db.QueryRowContext(ctx, incidentSelect+`
			WHERE node=? AND pci_addr=? AND class=?`+openIncidentTail,
			target.Node, pci, string(class)))
		if err != store.ErrNotFound {
			return inc, err
		}
		// Nothing carries this address. Fall back to an incident that names no
		// device at all, which is what every row opened before this column
		// existed looks like: during an upgrade the recurrence of a fault whose
		// incident is already open must join it rather than open a duplicate
		// that cordons the same node twice. This is the pre-existing rule, and
		// it is reached only after the exact address has been ruled out, so it
		// can never win over a device-addressed incident.
		return scanIncident(q.db.QueryRowContext(ctx, incidentSelect+`
			WHERE node=? AND gpu_uuid='' AND pci_addr='' AND class=?`+openIncidentTail,
			target.Node, string(class)))
	}
	// A signal that names neither a UUID nor a bus address is node-scoped
	// (an alert, a manual trigger), and it must find a node-scoped incident —
	// one that names no device either.
	//
	// Constraining gpu_uuid alone was not that. Every PCI-only incident also
	// has an empty gpu_uuid, so a node alert joined the oldest same-class
	// DEVICE incident instead of opening its own: the node's fault then
	// advanced a ladder aimed at one card and could reset it, while the
	// node-scoped problem never got an incident at all. Rows written before
	// the column have pci_addr '', so the upgrade case still matches here.
	return scanIncident(q.db.QueryRowContext(ctx, incidentSelect+`
		WHERE node=? AND gpu_uuid='' AND pci_addr='' AND class=?`+openIncidentTail,
		target.Node, string(class)))
}

// PromoteIncidentTarget replaces an unattributed incident's device identity
// with the precise one a later signal carried, in the caller's transaction.
//
// This is the write the identity defect needed. An incident opened from a
// kernel fault can only be addressed by PCI address, and an empty GPU UUID is
// treated downstream as PERMANENTLY unfixable — the reset preflight refuses it
// outright, which is the right reading of a UUID that can never arrive, and
// the wrong outcome entirely when the UUID did arrive two seconds later. By
// then the playbook has usually cordoned and drained the node, so the cost of
// dropping the precise signal is a drained machine parked for a human.
//
// It is deliberately NOT part of UpdateIncident. Everything else about a
// target is immutable for the life of an incident, and keeping it that way
// means the only statement in this store that can move an incident from one
// device to another is this one, with the guards below attached to it.
//
// The guards are the whole point, so they are in the WHERE clause rather than
// in a preceding read:
//
//   - version=? is the same optimistic fence UpdateIncident uses, so a caller
//     holding a stale snapshot cannot overwrite a row somebody else advanced.
//   - an empty gpu_uuid makes promotion strictly once. If two precise signals for
//     one device are ingested concurrently, exactly one UPDATE matches; the
//     loser sees ErrConflict and retries, finds the now-attributed incident by
//     UUID on the ordinary path, and simply attaches. Neither can overwrite
//     the other's identity, and no signal is lost.
//   - pci_addr=? pins the promotion to the address the caller matched on, so a
//     concurrent promotion of a DIFFERENT device cannot be silently accepted.
//   - the terminal-state exclusion keeps a resolved or expired incident from
//     being re-identified after automation has finished with it.
//
// The unique index on (node, gpu_uuid, class) for attributed rows is the last
// guard, and it is the database's rather than this function's: if an open
// incident already exists for this UUID and class, the UPDATE fails with a
// constraint error and the transaction aborts. That is loud and retryable,
// which is the correct outcome — two open incidents for one device would let
// two ladders drive the same GPU.
func (q *Queries) PromoteIncidentTarget(ctx context.Context, inc *types.Incident, to types.Target) error {
	if to.GPUUUID == "" {
		return fmt.Errorf("promoting incident %s needs a GPU UUID: an empty one is the state being promoted out of", inc.ID)
	}
	if to.Node != inc.Target.Node {
		return fmt.Errorf("promoting incident %s cannot move it from node %q to node %q", inc.ID, inc.Target.Node, to.Node)
	}
	matchedPCI := types.NormalizePCIAddress(inc.Target.PCIAddr)
	if matchedPCI == "" {
		return fmt.Errorf("promoting incident %s needs the PCI address it was opened with: without it nothing proves the precise signal names the same device", inc.ID)
	}
	now := time.Now()
	res, err := q.db.ExecContext(ctx, `
		UPDATE incidents SET gpu_uuid=?, gpu_index=?, updated_at=?, version=version+1
		WHERE id=? AND version=? AND gpu_uuid='' AND pci_addr=?
		  AND state NOT IN ('RESOLVED','EXPIRED')`,
		to.GPUUUID, to.GPUIndex, ts(now), inc.ID, inc.Version, matchedPCI)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		var current int
		switch err := q.db.QueryRowContext(ctx,
			`SELECT version FROM incidents WHERE id=?`, inc.ID).Scan(&current); {
		case err == sql.ErrNoRows:
			return store.ErrNotFound
		case err != nil:
			return err
		default:
			// The row is still there but no longer promotable: another writer
			// advanced its version, filled in the UUID, or terminalized it.
			// All three are retry-and-re-read, never overwrite.
			return store.ErrConflict
		}
	}
	inc.Target.GPUUUID = to.GPUUUID
	inc.Target.GPUIndex = to.GPUIndex
	inc.UpdatedAt = now
	inc.Version++
	return nil
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
	if !f.ActiveSince.IsZero() {
		// A terminal incident ends at resolved_at (RESOLVED) or at the
		// transition that terminated it (EXPIRED, which sets no resolved_at);
		// everything else is still running and always qualifies.
		//
		// The cutoff is padded by a second because timestamps are stored as
		// RFC3339Nano text, whose trailing-zero trimming makes lexical order
		// disagree with chronological order within a single second
		// ("…:00Z" sorts after "…:00.5Z"). Keeping one extra second of rows
		// cannot produce a wrong answer: the caller clips to the exact window
		// in Go. Losing a row silently would.
		// NULLIF covers pre-0002 rows whose state_changed_at is the empty
		// string: an unknown end time falls back to updated_at rather than
		// sorting before every cutoff and dropping the row.
		qs += ` AND (state NOT IN ('RESOLVED','EXPIRED')
			OR COALESCE(NULLIF(resolved_at, ''), NULLIF(state_changed_at, ''), updated_at) >= ?)`
		args = append(args, ts(f.ActiveSince.Add(-time.Second)))
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
	SELECT id, node, gpu_uuid, gpu_index, pci_addr, class, state, playbook,
	       step_index, attempt, dry_run, signals_seen, remediation_slot_held, approval_epoch,
	       vendor, opened_at, updated_at, state_changed_at, resolved_at, version
	FROM incidents`

type rowScanner interface{ Scan(dest ...any) error }

func scanIncident(r rowScanner) (*types.Incident, error) {
	var inc types.Incident
	var class, state, vendor, opened, updated, stateChanged string
	var dryRun, slotHeld int
	var resolved sql.NullString
	err := r.Scan(&inc.ID, &inc.Target.Node, &inc.Target.GPUUUID, &inc.Target.GPUIndex,
		&inc.Target.PCIAddr, &class, &state, &inc.Playbook, &inc.StepIndex, &inc.Attempt, &dryRun,
		&inc.SignalSeen, &slotHeld, &inc.ApprovalEpoch, &vendor, &opened, &updated, &stateChanged, &resolved, &inc.Version)
	if err == sql.ErrNoRows {
		return nil, store.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	inc.Class = types.ProblemClass(class)
	inc.State = types.IncidentState(state)
	inc.Vendor = types.AcceleratorVendor(vendor)
	inc.DryRun = dryRun != 0
	inc.RemediationSlotHeld = slotHeld != 0
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

// executedStepResultsChunk bounds how many incident IDs go into one IN clause.
// SQLite's compiled-in host-parameter limit is 999 on builds that predate
// 3.32, and a report over a busy year can ask about far more incidents than
// that; a query that fails on a large fleet and works on a small one is the
// kind of limit nobody finds until the report matters.
const executedStepResultsChunk = 500

// ExecutedStepResults implements store.Store.
func (q *Queries) ExecutedStepResults(ctx context.Context, incidentIDs []string) (map[string][]string, error) {
	out := make(map[string][]string, len(incidentIDs))
	for start := 0; start < len(incidentIDs); start += executedStepResultsChunk {
		end := min(start+executedStepResultsChunk, len(incidentIDs))
		chunk := incidentIDs[start:end]
		args := make([]any, 0, len(chunk))
		for _, id := range chunk {
			args = append(args, id)
		}
		// from_state <> to_state isolates a genuine transition INTO EXECUTING.
		// A step that fails appends a non-transition row (from EXECUTING to
		// EXECUTING) carrying "FAILED: …", and counting that as a second entry
		// would say a step ran twice.
		rows, err := q.db.QueryContext(ctx, `
			SELECT DISTINCT incident_id, result FROM audit_log
			WHERE to_state='EXECUTING' AND from_state <> 'EXECUTING'
			  AND incident_id IN (?`+repeat(",?", len(chunk)-1)+`)`, args...)
		if err != nil {
			return nil, err
		}
		err = func() error {
			defer func() { _ = rows.Close() }()
			for rows.Next() {
				var id, result string
				if err := rows.Scan(&id, &result); err != nil {
					return err
				}
				out[id] = append(out[id], result)
			}
			return rows.Err()
		}()
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

// --- approvals ---

func (q *Queries) RecordApproval(ctx context.Context, a *types.Approval) error {
	_, err := q.db.ExecContext(ctx, `
		INSERT INTO approvals (incident_id, step_name, decision, actor, channel, at, playbook_name, step_action, step_hash, park_epoch)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		a.IncidentID, a.StepName, string(a.Decision), a.Actor, a.Channel, ts(a.At),
		a.PlaybookName, a.StepAction, a.StepHash, a.ParkEpoch)
	return err
}

func (q *Queries) LatestApproval(ctx context.Context, incidentID string) (*types.Approval, error) {
	row := q.db.QueryRowContext(ctx, `
		SELECT incident_id, step_name, decision, actor, channel, at, playbook_name, step_action, step_hash, park_epoch
		FROM approvals WHERE incident_id=? ORDER BY id DESC LIMIT 1`, incidentID)
	var a types.Approval
	var decision, at string
	err := row.Scan(&a.IncidentID, &a.StepName, &decision, &a.Actor, &a.Channel, &at,
		&a.PlaybookName, &a.StepAction, &a.StepHash, &a.ParkEpoch)
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

// approvalRoundSelect is shared by the two per-epoch approval reads.
const approvalRoundSelect = `
	SELECT incident_id, step_name, decision, actor, channel, at, playbook_name, step_action, step_hash, park_epoch
	FROM approvals WHERE incident_id=? AND park_epoch=?`

// GetApprovalRequest returns the "requested" record of one approval round —
// the durable statement of exactly what the human was asked to approve when
// the incident parked at this epoch. ErrNotFound means the round has no
// request record (a pre-epoch park); a decision must not be honored for it.
func (q *Queries) GetApprovalRequest(ctx context.Context, incidentID string, epoch int) (*types.Approval, error) {
	row := q.db.QueryRowContext(ctx,
		approvalRoundSelect+` AND decision=? ORDER BY id DESC LIMIT 1`,
		incidentID, epoch, string(types.ApprovalRequested))
	return scanApproval(row)
}

// LatestApprovalDecision returns the newest human decision belonging to one
// approval round, or ErrNotFound while the round is undecided. A decision
// recorded for an earlier epoch is invisible here by construction — that is
// the whole point: a re-park mints a new epoch and orphans stale decisions.
func (q *Queries) LatestApprovalDecision(ctx context.Context, incidentID string, epoch int) (*types.Approval, error) {
	row := q.db.QueryRowContext(ctx,
		approvalRoundSelect+` AND decision<>? ORDER BY id DESC LIMIT 1`,
		incidentID, epoch, string(types.ApprovalRequested))
	return scanApproval(row)
}

func scanApproval(row *sql.Row) (*types.Approval, error) {
	var a types.Approval
	var decision, at string
	err := row.Scan(&a.IncidentID, &a.StepName, &decision, &a.Actor, &a.Channel, &at,
		&a.PlaybookName, &a.StepAction, &a.StepHash, &a.ParkEpoch)
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
	// agent_arming is written through UNCONDITIONALLY, like gpus/boot_id:
	// registration is the agent's authoritative self-snapshot, and preserving
	// a stale 'armed' across an agent downgrade or pod replacement would be a
	// stale-authority bug. '' (unknown) overwrites too, by design.
	_, err := q.db.ExecContext(ctx, `
		INSERT INTO nodes (name, node_uid, platform, gpus, boot_id, agent_last_seen, agent_arming)
		VALUES (?, ?, 'agent', ?, ?, ?, ?)
		ON CONFLICT(name) DO UPDATE SET
			node_uid=CASE WHEN excluded.node_uid<>'' THEN excluded.node_uid ELSE nodes.node_uid END,
			gpus=excluded.gpus, boot_id=excluded.boot_id,
			agent_last_seen=excluded.agent_last_seen,
			agent_arming=excluded.agent_arming`,
		n.Name, n.UID, string(gpus), n.BootID, lastSeen, string(n.AgentArming))
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
	SELECT name, node_uid, platform, labels, ssh_addr, bmc_addr, gpus, boot_id, paused, agent_last_seen, agent_arming
	FROM nodes`

func scanNode(r rowScanner) (*types.Node, error) {
	var n types.Node
	var labels, gpus, arming string
	var paused int
	var lastSeen sql.NullString
	err := r.Scan(&n.Name, &n.UID, &n.Platform, &labels, &n.SSHAddr, &n.BMCAddr, &gpus, &n.BootID, &paused, &lastSeen, &arming)
	if err == sql.ErrNoRows {
		return nil, store.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	n.Paused = paused != 0
	n.AgentArming = types.AgentArming(arming)
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
		&current.RuntimeVersion, &current.TopologySafety, &current.CapabilitiesJSON, &current.HoldersJSON)
	switch {
	case err == sql.ErrNoRows:
		if _, err := c.wrap(tx).ExecContext(ctx, `
			INSERT INTO accelerator_reports (
				node, vendor, node_uid, observed_at_ns, profile_digest, profile_uid, profile_generation, readiness, reasons_json,
				devices_json, driver_version, runtime_version, topology_safety, capabilities_json, holders_json
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			report.Node, string(report.Vendor), payload.NodeUID, payload.ObservedAtNS, payload.ProfileDigest, payload.ProfileUID, payload.ProfileGeneration,
			payload.Readiness, payload.ReasonsJSON, payload.DevicesJSON,
			payload.DriverVersion, payload.RuntimeVersion, payload.TopologySafety,
			payload.CapabilitiesJSON, payload.HoldersJSON); err != nil {
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
				capabilities_json=?, holders_json=?
			WHERE node=? AND vendor=? AND observed_at_ns < ?`,
			payload.NodeUID, payload.ObservedAtNS, payload.ProfileDigest, payload.ProfileUID, payload.ProfileGeneration, payload.Readiness,
			payload.ReasonsJSON, payload.DevicesJSON, payload.DriverVersion,
			payload.RuntimeVersion, payload.TopologySafety, payload.CapabilitiesJSON, payload.HoldersJSON,
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
	       devices_json, driver_version, runtime_version, topology_safety, capabilities_json, holders_json
	FROM accelerator_reports`

const acceleratorReportPayloadSelect = `
	SELECT observed_at_ns, node_uid, profile_digest, profile_uid, profile_generation, readiness, reasons_json, devices_json,
	       driver_version, runtime_version, topology_safety, capabilities_json, holders_json
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
	// HoldersJSON is the marshalled DeviceHolders slice. It preserves the
	// nil-vs-empty distinction the reset gate depends on: json 'null' means the
	// agent did not look, '[]' means it looked and found nothing holding a
	// device. Legacy rows default to 'null' so an old report stays "not
	// observed" rather than being misread as "observed, none present".
	HoldersJSON string
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
	// json.Marshal renders a nil slice as 'null' and an empty non-nil slice as
	// '[]', so the nil-vs-empty holders distinction survives the round trip.
	holders, err := json.Marshal(report.DeviceHolders)
	if err != nil {
		return acceleratorReportPayload{}, fmt.Errorf("marshal device holders: %w", err)
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
		HoldersJSON:       string(holders),
	}, nil
}

func scanAcceleratorReport(r rowScanner) (*types.AgentAcceleratorReport, error) {
	var report types.AgentAcceleratorReport
	var vendor string
	var observedAtNS int64
	var reasons, devices, capabilities, holders string
	var readiness, topologySafety string
	err := r.Scan(&report.Node, &vendor, &report.NodeUID, &observedAtNS, &report.ProfileDigest, &report.ProfileUID, &report.ProfileGeneration,
		&readiness, &reasons, &devices, &report.DriverVersion,
		&report.RuntimeVersion, &topologySafety, &capabilities, &holders)
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
	// report.DeviceHolders starts nil; a stored 'null' leaves it nil ("agent
	// did not look"), '[]' makes it non-nil empty ("looked, none present").
	// Keeping the two apart is load-bearing for the controller's reset gate.
	if err := json.Unmarshal([]byte(holders), &report.DeviceHolders); err != nil {
		return nil, fmt.Errorf("accelerator report %s/%s: corrupt device holders: %w", report.Node, report.Vendor, err)
	}
	if err := report.Validate(); err != nil {
		return nil, fmt.Errorf("accelerator report %s/%s: corrupt persisted report: %w", report.Node, report.Vendor, err)
	}
	return &report, nil
}

// --- action queue ---

func (q *Queries) EnqueueAction(ctx context.Context, node string, a types.Action) error {
	params, err := json.Marshal(a.Params)
	if err != nil {
		return err
	}
	now := ts(time.Now())
	_, err = q.db.ExecContext(ctx, `
		INSERT INTO actions (id, node, incident_id, type, params, timeout_ns, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO NOTHING`,
		a.ID, node, a.IncidentID, string(a.Type), string(params), int64(a.Timeout), now, now)
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
	// The lease outlives the action's own deadline by the reporting grace.
	//
	// Sized exactly at the deadline, it made a timing-out action's result
	// unreportable BY CONSTRUCTION: the executor cancels the work at T and
	// POSTs the reason a moment later, and CompleteClaimedAction requires
	// lease_expires_at_ns > now. So the only result worth having — why it timed
	// out, which processes still held the device — was the one guaranteed to be
	// refused, and the controller saw a generic timeout with no cause. The row
	// also read as reclaimable, so the action could be dispatched again: for
	// the shipped 12h WaitIdle rung, another twelve hours of a GPU held out of
	// service before anything changes.
	//
	// The grace is types.AgentResultGrace, the same value the controller waits
	// beyond the agent's budget, because a controller listening for an answer
	// the store will reject is worse than one that does not wait at all.
	resultGraceNS := int64(types.AgentResultGrace)

	// Dead-letter poison actions before claiming. An action that has exhausted
	// its attempt budget and is provably not executing — pending, or leased with
	// an expired lease — must leave the claimable pool for good rather than be
	// re-leased on every expiry forever. A genuinely in-flight action (unexpired
	// lease) is left untouched so the boot-ID/lease completion contract holds
	// for work that may still be running.
	if res, err := c.db.ExecContext(ctx, `
		UPDATE actions
		SET state='dead', lease_token='', lease_expires_at_ns=0, updated_at=?
		WHERE node=? AND attempts>=?
		  AND (state='pending' OR (state='leased' AND lease_expires_at_ns <= ?))`,
		ts(now), node, MaxActionAttempts, nowNS); err != nil {
		return nil, fmt.Errorf("claim action: dead-letter exhausted actions: %w", err)
	} else if n, _ := res.RowsAffected(); n > 0 {
		// Counted, because this is the moment work stops being retried.
		metrics.DeadLettered.WithLabelValues("actions").Add(float64(n))
	}

	// The claim is atomic under concurrency without FOR UPDATE SKIP LOCKED.
	// Every concurrent claimer's inner SELECT deterministically resolves to the
	// same oldest candidate, so they serialize on that one row's write lock; the
	// OUTER claimable-state predicate is then re-checked against the row each
	// waiter finally sees. Once the winner has leased it the row is no longer
	// pending-or-expired, so every loser updates zero rows and reports no work.
	// This both prevents the double-lease (two claimers overwriting one row's
	// token and double-counting attempts) and preserves "at most one unexpired
	// lease per node": SKIP LOCKED is intentionally absent here because it would
	// let a loser lease a different action for the same node and break that
	// invariant.
	row := c.db.QueryRowContext(ctx, `
		UPDATE actions
		SET state='leased', lease_token=?,
		    lease_expires_at_ns=? + (CASE WHEN timeout_ns > ? THEN timeout_ns ELSE ? END) + ?,
		    attempts=attempts+1, executor_boot_id=?,
		    updated_at=?
		WHERE id = (
			SELECT candidate.id
			FROM actions AS candidate
			WHERE candidate.node=?
			  AND (candidate.state='pending'
			       OR (candidate.state='leased' AND candidate.lease_expires_at_ns <= ?))
			  AND candidate.attempts < ?
			  AND NOT EXISTS (
				SELECT 1 FROM actions AS held
				WHERE held.node=candidate.node
				  AND held.state='leased'
				  AND held.lease_expires_at_ns > ?
			  )
			  AND NOT EXISTS (
				SELECT 1 FROM incidents AS inc
				WHERE inc.id=candidate.incident_id
				  AND inc.state IN `+terminalIncidentStates+`
			  )
			ORDER BY candidate.created_at, candidate.id
			LIMIT 1
		)
		  AND (state='pending' OR (state='leased' AND lease_expires_at_ns <= ?))
		RETURNING id, node, incident_id, type, params, timeout_ns, state, result,
		          lease_token, lease_expires_at_ns, attempts, executor_boot_id`,
		token, nowNS, minimumLeaseNS, minimumLeaseNS, resultGraceNS, bootID, ts(now),
		node, nowNS, MaxActionAttempts, nowNS, nowNS)
	return scanAction(row)
}

// CancelPendingActionsForIncident tombstones the incident's undelivered
// actions when the incident terminalizes. A 'pending' action was never
// delivered, and a 'leased' action whose lease has expired is provably not
// executing (the lease is the node's promise to be running it), so both can be
// safely revoked. An action under an unexpired lease is left alone: it may
// still be executing on the node, and pretending it was revoked would lie
// about a possible side effect. This closes the hole where a leased-then-
// orphaned action of a quarantined or resolved incident re-entered the
// claimable pool on every lease expiry — a crashed agent being handed a stale
// gpu_reset for an incident a human already took over. Returns the count
// cancelled.
func (q *Queries) CancelPendingActionsForIncident(ctx context.Context, incidentID string) (int64, error) {
	if incidentID == "" {
		return 0, fmt.Errorf("cancel actions: incident ID is required")
	}
	out, err := q.db.ExecContext(ctx, `
		UPDATE actions SET state='cancelled', lease_token='', lease_expires_at_ns=0, updated_at=?
		WHERE incident_id=?
		  AND (state='pending'
		       OR (state='leased' AND lease_expires_at_ns <= ?))`,
		ts(time.Now()), incidentID, time.Now().UnixNano())
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

// storedActionResult is how a result is written to the actions table.
//
// It exists for one field. types.ActionResult.Refusal is `json:"-"` on
// purpose: the refusal code travels in AgentActionRefusalHeader because the
// result ROUTE strict-decodes, and an unknown body field would 400 every
// result posted by a newer agent to an older controller. That reasoning is
// about the WIRE and is right.
//
// The store is not the wire. Marshalling types.ActionResult straight into the
// result column applied the wire's rule to a private blob, so the code the
// header carefully preserved was dropped on the way to disk — and the
// controller never sees the HTTP response anyway: agentrpc.Execute polls the
// store. So Refusal was set by the agent, accepted by the API, persisted as
// nothing, and read back empty. idleGuardRefused could never be true and
// kubeneuron_destructive_steps_deferred_total{reason="not_idle"} — the series
// docs/reference-metrics.md offers as evidence that workloads are protected —
// was structurally pinned at zero.
//
// Embedding keeps the on-disk shape identical apart from one added key, so
// blobs written by earlier builds decode unchanged with an empty refusal.
type storedActionResult struct {
	types.ActionResult
	Refusal string `json:"refusal,omitempty"`
}

func marshalActionResult(res types.ActionResult) ([]byte, error) {
	return json.Marshal(storedActionResult{ActionResult: res, Refusal: res.Refusal})
}

func unmarshalActionResult(blob string, out *types.ActionResult) error {
	var stored storedActionResult
	if err := json.Unmarshal([]byte(blob), &stored); err != nil {
		return err
	}
	*out = stored.ActionResult
	out.Refusal = stored.Refusal
	return nil
}

func (q *Queries) CompleteClaimedAction(ctx context.Context, actionID, leaseToken, bootID string, res types.ActionResult) error {
	if leaseToken == "" {
		return store.ErrLeaseLost
	}
	blob, err := marshalActionResult(res)
	if err != nil {
		return err
	}
	now := time.Now()
	// The boot guard binds the result to the node boot that claimed the
	// action: a result posted after an unnoticed reboot is not evidence
	// that the side effect completed on the boot that started it. An action
	// claimed with a boot ID (executor_boot_id != '') must be completed with a
	// matching one; an absent/empty completion boot ID no longer bypasses the
	// guard (it fails closed with ErrExecutorBootMismatch below). Actions
	// claimed without a boot ID (executor_boot_id='') carry no boot to check.
	out, err := q.db.ExecContext(ctx, `
		UPDATE actions
		SET state='done', result=?, lease_token='', lease_expires_at_ns=0, updated_at=?
		WHERE id=? AND state='leased' AND lease_token=? AND lease_expires_at_ns > ?
		  AND (executor_boot_id='' OR executor_boot_id=?)`,
		string(blob), ts(now), actionID, leaseToken, now.UnixNano(), bootID)
	if err != nil {
		return err
	}
	if n, _ := out.RowsAffected(); n > 0 {
		return nil
	}
	// Preserve ErrNotFound for an unknown ID; distinguish a boot mismatch
	// from a wrong/expired lease so operators see the reboot. A boot mismatch
	// is only diagnosable while the lease is otherwise current — token still
	// matching AND unexpired — but the completing boot differs from the one
	// that claimed it. An expired lease is ErrLeaseLost regardless of boot: the
	// action was reclaimable, so reporting a boot mismatch would misdiagnose it.
	current, err := q.GetAction(ctx, actionID)
	if err != nil {
		return err
	}
	if current.LeaseToken == leaseToken && current.LeaseExpiresAt.After(now) &&
		current.ExecutorBootID != "" && current.ExecutorBootID != bootID {
		return store.ErrExecutorBootMismatch
	}
	return store.ErrLeaseLost
}

func (q *Queries) CompleteAction(ctx context.Context, actionID string, res types.ActionResult) error {
	blob, err := marshalActionResult(res)
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

// terminalActionStates is the SQL half of types.QueuedAction.Terminal(): the
// three states from which an action can never make further progress. Written
// once so the prune, the discard and the Go predicate cannot disagree about
// how many there are — which they have, repeatedly, one omission per round.
const terminalActionStates = `('done','dead','cancelled')`

// terminalEventStates is the same idea for the event outbox, which
// terminalises two ways: 'done' delivered, 'dead' exhausted MaxEventAttempts.
// Written beside its sibling so the next person adding a queue sees that this
// is how the question is asked here.
const terminalEventStates = `('done','dead')`

// DiscardCompletedAction removes a finished queue row so a caller that
// derives a DETERMINISTIC action ID can start a fresh attempt.
//
// EnqueueAction is idempotent on the ID (ON CONFLICT DO NOTHING) and completed
// rows live for the retention window — 90 days by default. That combination is
// right for a retry inside one attempt and wrong across attempts: the second
// attempt silently re-attaches to the first one's stored result, returning it
// with no agent involved at all. A failed first attempt therefore wedges every
// later one, and a successful one makes a genuinely new attempt report success
// it never performed.
//
// All THREE terminal states, matching the prune query above: 'done' completed,
// 'dead' exhausted its attempt budget, 'cancelled' was tombstoned. Omitting
// 'dead' left the worst case open — an agent that crash-loops or is restarted
// by its own ladder's reboot rung fails eight claims, the row dead-letters, and
// from then on the discard matches nothing, the enqueue conflicts away, and
// agentrpc polls a row it can never claim until the caller's deadline. The
// janitor's restore budget is SHARED across every quiesced node, so that one
// node consumes it on every tick and starves the rest, and the condition
// self-heals only when retention drops the row — ninety days by default.
//
// Only a terminal row is removed, so this can never race a live claim: an
// action still pending or leased is left exactly where it is.
func (q *Queries) DiscardCompletedAction(ctx context.Context, actionID string) error {
	_, err := q.db.ExecContext(ctx,
		`DELETE FROM actions WHERE id=? AND state IN `+terminalActionStates, actionID)
	return err
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
	// The third terminal state, set here so QueuedAction.Terminal() can be the
	// only place anyone has to know there are three.
	qa.Dead = state == "dead"
	if state == "leased" && leaseExpiresAtNS > 0 {
		qa.LeaseExpiresAt = time.Unix(0, leaseExpiresAtNS).UTC()
	} else {
		qa.LeaseToken = ""
	}
	if qa.Done && result != "" {
		qa.Result = &types.ActionResult{}
		if err := unmarshalActionResult(result, qa.Result); err != nil {
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

	// The neutral fault envelope and PCI address must survive the archive round
	// trip: the controller classifies the event AFTER reading it back from the
	// outbox, so a fallback event carrying XID=0 + Fault{nvidia, ecc-dbe} whose
	// Fault was dropped here would classify as non-actionable and open no
	// incident. A nil Fault marshals to the empty string so legacy rows (which
	// only ever carried an XID) and genuine no-fault events both scan back to a
	// nil Fault rather than a fabricated one.
	faultJSON := ""
	if ev.Fault != nil {
		blob, err := json.Marshal(ev.Fault)
		if err != nil {
			return rollback(fmt.Errorf("marshal event fault: %w", err))
		}
		faultJSON = string(blob)
	}

	// RETURNING keeps this portable: PostgreSQL has no LastInsertId, and a
	// conflicting (duplicate) insert returns no row on both engines.
	var eventRowID int64
	err = c.wrap(tx).QueryRowContext(ctx, `
		INSERT INTO events (event_id, node, gpu_index, gpu_uuid, xid, raw, timestamp, fault_json, pci_addr)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT DO NOTHING
		RETURNING id`,
		ev.EventID, ev.Node, ev.GPUIndex, ev.GPUUUID, ev.XID, ev.Raw, ts(ev.Timestamp),
		faultJSON, ev.PCIAddr).Scan(&eventRowID)
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

	// Dead-letter poison events before claiming, mirroring the action queue. An
	// event that has exhausted its attempt budget and is provably not being
	// processed — pending, or leased with an expired lease — leaves the
	// claimable pool for good rather than re-leasing on every expiry and
	// aborting each drain batch forever. An event under an unexpired lease is
	// left untouched so a worker still holding a valid claim can complete it.
	if res, err := c.db.ExecContext(ctx, `
		UPDATE event_outbox
		SET state='dead', lease_owner='', lease_token='', lease_expires_at_ns=0, updated_at=?
		WHERE attempts>=?
		  AND (state='pending' OR (state='leased' AND lease_expires_at_ns <= ?))`,
		ts(now), MaxEventAttempts, nowNS); err != nil {
		return nil, fmt.Errorf("claim event: dead-letter exhausted events: %w", err)
	} else if n, _ := res.RowsAffected(); n > 0 {
		metrics.DeadLettered.WithLabelValues("events").Add(float64(n))
	}

	var outboxID int64
	// The outbox is a parallel work queue with no per-worker exclusivity, so
	// FOR UPDATE SKIP LOCKED (PostgreSQL) lets each concurrent worker lock and
	// claim a distinct row instead of two claimers picking the same one under
	// READ COMMITTED. The OUTER state predicate re-checks the row at UPDATE time
	// so that, even where the dialect has no skip-locked clause (SQLite), a
	// stale candidate that was leased out from under this claimer updates zero
	// rows rather than being re-leased. SQLite serializes writers on one
	// connection, so its empty clause is already safe.
	err = c.db.QueryRowContext(ctx, `
		UPDATE event_outbox
		SET state='leased', attempts=attempts+1, lease_owner=?, lease_token=?,
		    lease_expires_at_ns=?, updated_at=?
		WHERE id = (
			SELECT candidate.id
			FROM event_outbox AS candidate
			WHERE candidate.attempts < ?
			  AND (candidate.state='pending'
			       OR (candidate.state='leased' AND candidate.lease_expires_at_ns <= ?))
			ORDER BY candidate.created_at, candidate.id
			LIMIT 1`+c.SkipLocked+`
		)
		  AND (state='pending' OR (state='leased' AND lease_expires_at_ns <= ?))
		RETURNING id`,
		workerID, token, expiresAtNS, ts(now), MaxEventAttempts, nowNS, nowNS).Scan(&outboxID)
	if err == sql.ErrNoRows {
		return nil, store.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("claim event: %w", err)
	}

	// Re-read filtered on the lease token this claim just issued: if the row was
	// leased out from under us (a stall past the lease before this read), the
	// token no longer matches and scanClaimedEvent reports no work rather than
	// this worker adopting another worker's claim.
	row := c.db.QueryRowContext(ctx, eventOutboxSelect+` WHERE o.id=? AND o.lease_token=?`, outboxID, token)
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
	       e.fault_json, e.pci_addr,
	       o.attempts, o.lease_token, o.lease_expires_at_ns
	FROM event_outbox AS o
	JOIN events AS e ON e.id=o.event_row_id`

func scanClaimedEvent(r rowScanner) (*store.ClaimedEvent, error) {
	var claimed store.ClaimedEvent
	var timestamp, faultJSON string
	var leaseExpiresAtNS int64
	err := r.Scan(&claimed.OutboxID, &claimed.Event.EventID, &claimed.Event.Node,
		&claimed.Event.GPUIndex, &claimed.Event.GPUUUID, &claimed.Event.XID,
		&claimed.Event.Raw, &timestamp, &faultJSON, &claimed.Event.PCIAddr,
		&claimed.Attempt, &claimed.LeaseToken, &leaseExpiresAtNS)
	if err == sql.ErrNoRows {
		return nil, store.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	// An empty fault_json ("no fault", including every legacy row) leaves Fault
	// nil, keeping the XID-vs-Fault authority distinction the classifier depends
	// on; a stored envelope is rehydrated so the fallback detection source
	// survives the durable round trip.
	if faultJSON != "" {
		var fault types.FaultSignal
		if err := json.Unmarshal([]byte(faultJSON), &fault); err != nil {
			return nil, fmt.Errorf("event outbox %d: corrupt fault envelope: %w", claimed.OutboxID, err)
		}
		claimed.Event.Fault = &fault
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
