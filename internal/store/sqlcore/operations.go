package sqlcore

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/kubeneuron/kubeneuron/internal/store"
	"github.com/kubeneuron/kubeneuron/pkg/types"
)

const operationalResourceSelect = `
	SELECT kind, id, state, tenant, cluster, actor, config_digest, payload,
	       created_at, updated_at, expires_at, version
	FROM operational_resources`

// CreateOperationalResource stores a new immutable-ID product resource.  The
// content may subsequently move through a lifecycle, but every update is
// optimistic-versioned so an approval, cancellation, or rollback cannot erase
// a concurrent transition it did not observe.
func (q *Queries) CreateOperationalResource(ctx context.Context, resource *types.OperationalResource) error {
	if err := validateOperationalResource(resource); err != nil {
		return err
	}
	if resource.State == "" {
		resource.State = "pending"
	}
	now := time.Now().UTC()
	if resource.CreatedAt.IsZero() {
		resource.CreatedAt = now
	}
	if resource.UpdatedAt.IsZero() {
		resource.UpdatedAt = resource.CreatedAt
	}
	if resource.Version == 0 {
		resource.Version = 1
	}
	var expires any
	if resource.ExpiresAt != nil {
		expires = ts(*resource.ExpiresAt)
	}
	_, err := q.db.ExecContext(ctx, `
		INSERT INTO operational_resources
			(kind, id, state, tenant, cluster, actor, config_digest, payload, created_at, updated_at, expires_at, version)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		string(resource.Kind), resource.ID, resource.State, resource.Tenant, resource.Cluster,
		resource.Actor, resource.ConfigDigest, string(resource.Payload), ts(resource.CreatedAt),
		ts(resource.UpdatedAt), expires, resource.Version)
	if err != nil {
		if uniqueConstraint(err) {
			return store.ErrOperationalConflict
		}
		return err
	}
	return nil
}

// GetOperationalResource returns a deep decoded resource by immutable kind and
// ID.  Expired resources remain readable for audit/replay; API authorization
// decides whether their payload may be rendered.
func (q *Queries) GetOperationalResource(ctx context.Context, kind types.OperationalResourceKind, id string) (*types.OperationalResource, error) {
	if kind == "" || strings.TrimSpace(id) == "" {
		return nil, store.ErrNotFound
	}
	row := q.db.QueryRowContext(ctx, operationalResourceSelect+` WHERE kind=? AND id=?`, string(kind), id)
	return scanOperationalResource(row)
}

// UpdateOperationalResource conditionally writes a lifecycle transition.  The
// caller supplies the version it displayed; a mismatch is never repaired by
// last-write-wins because that would permit an old approval to overwrite a
// pause or expiry.
func (q *Queries) UpdateOperationalResource(ctx context.Context, resource *types.OperationalResource, expectedVersion int) error {
	if err := validateOperationalResource(resource); err != nil {
		return err
	}
	if expectedVersion <= 0 {
		return fmt.Errorf("operational resource %s/%s: expected version is required", resource.Kind, resource.ID)
	}
	if resource.State == "" {
		return fmt.Errorf("operational resource %s/%s: state is required", resource.Kind, resource.ID)
	}
	resource.UpdatedAt = time.Now().UTC()
	var expires any
	if resource.ExpiresAt != nil {
		expires = ts(*resource.ExpiresAt)
	}
	res, err := q.db.ExecContext(ctx, `
		UPDATE operational_resources
		SET state=?, tenant=?, cluster=?, actor=?, config_digest=?, payload=?, updated_at=?, expires_at=?, version=version+1
		WHERE kind=? AND id=? AND version=?`,
		resource.State, resource.Tenant, resource.Cluster, resource.Actor, resource.ConfigDigest,
		string(resource.Payload), ts(resource.UpdatedAt), expires, string(resource.Kind), resource.ID, expectedVersion)
	if err != nil {
		return err
	}
	if affected, _ := res.RowsAffected(); affected > 0 {
		resource.Version = expectedVersion + 1
		return nil
	}
	var actual int
	err = q.db.QueryRowContext(ctx, `SELECT version FROM operational_resources WHERE kind=? AND id=?`, string(resource.Kind), resource.ID).Scan(&actual)
	if errors.Is(err, sql.ErrNoRows) {
		return store.ErrNotFound
	}
	if err != nil {
		return err
	}
	return store.ErrOperationalConflict
}

// ListOperationalResources returns a bounded, stable-order page.  The cursor
// is the (created_at,id) tuple, which remains valid when a resource's state or
// payload changes after the page was rendered.
func (q *Queries) ListOperationalResources(ctx context.Context, filter types.OperationalResourceFilter) ([]*types.OperationalResource, error) {
	if filter.Kind == "" {
		return nil, fmt.Errorf("operational resource kind is required")
	}
	limit := filter.Limit
	if limit <= 0 {
		limit = 100
	}
	if limit > 500 {
		return nil, fmt.Errorf("operational resource limit exceeds 500")
	}
	clauses := []string{"kind=?"}
	args := []any{string(filter.Kind)}
	if filter.State != "" {
		clauses = append(clauses, "state=?")
		args = append(args, filter.State)
	}
	if filter.Tenant != "" {
		clauses = append(clauses, "tenant=?")
		args = append(args, filter.Tenant)
	}
	if filter.Cluster != "" {
		clauses = append(clauses, "cluster=?")
		args = append(args, filter.Cluster)
	}
	if !filter.CreatedSince.IsZero() {
		clauses = append(clauses, "created_at >= ?")
		args = append(args, ts(filter.CreatedSince))
	}
	if !filter.CreatedUntil.IsZero() {
		clauses = append(clauses, "created_at <= ?")
		args = append(args, ts(filter.CreatedUntil))
	}
	if !filter.IncludeExpired {
		clauses = append(clauses, "(expires_at IS NULL OR expires_at > ?)")
		args = append(args, ts(time.Now()))
	}
	if !filter.AfterCreatedAt.IsZero() {
		clauses = append(clauses, "(created_at > ? OR (created_at = ? AND id > ?))")
		args = append(args, ts(filter.AfterCreatedAt), ts(filter.AfterCreatedAt), filter.AfterID)
	}
	args = append(args, limit)
	rows, err := q.db.QueryContext(ctx, operationalResourceSelect+` WHERE `+strings.Join(clauses, " AND ")+` ORDER BY created_at, id LIMIT ?`, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := make([]*types.OperationalResource, 0)
	for rows.Next() {
		resource, err := scanOperationalResource(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, resource)
	}
	return out, rows.Err()
}

// PutOperationalIdempotency atomically claims a caller-supplied key.  On a
// replay it returns the original record and created=false; the service checks
// the request digest before returning the original resource.
func (q *Queries) PutOperationalIdempotency(ctx context.Context, record *types.OperationalIdempotencyRecord) (*types.OperationalIdempotencyRecord, bool, error) {
	if record == nil || record.Kind == "" || strings.TrimSpace(record.Actor) == "" || strings.TrimSpace(record.Key) == "" || strings.TrimSpace(record.RequestDigest) == "" || strings.TrimSpace(record.ResourceID) == "" {
		return nil, false, fmt.Errorf("operational idempotency record requires kind, actor, key, request digest, and resource ID")
	}
	now := time.Now().UTC()
	if record.CreatedAt.IsZero() {
		record.CreatedAt = now
	}
	if record.ExpiresAt.IsZero() {
		record.ExpiresAt = now.Add(24 * time.Hour)
	}
	row := q.db.QueryRowContext(ctx, `
		INSERT INTO operational_idempotency
			(kind, actor, idempotency_key, request_digest, resource_id, created_at, expires_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(kind, actor, idempotency_key) DO NOTHING
		RETURNING kind, actor, idempotency_key, request_digest, resource_id, created_at, expires_at`,
		string(record.Kind), record.Actor, record.Key, record.RequestDigest, record.ResourceID,
		ts(record.CreatedAt), ts(record.ExpiresAt))
	inserted, err := scanOperationalIdempotency(row)
	if err == nil {
		return inserted, true, nil
	}
	if !errors.Is(err, store.ErrNotFound) {
		return nil, false, err
	}
	existing, err := q.GetOperationalIdempotency(ctx, record.Kind, record.Actor, record.Key)
	if err != nil {
		return nil, false, err
	}
	return existing, false, nil
}

func (q *Queries) GetOperationalIdempotency(ctx context.Context, kind types.OperationalResourceKind, actor, key string) (*types.OperationalIdempotencyRecord, error) {
	row := q.db.QueryRowContext(ctx, `
		SELECT kind, actor, idempotency_key, request_digest, resource_id, created_at, expires_at
		FROM operational_idempotency WHERE kind=? AND actor=? AND idempotency_key=?`, string(kind), actor, key)
	return scanOperationalIdempotency(row)
}

// AppendOperationalAudit appends one hash-chained audit record. Callers use
// Core.AppendOperationalAudit, which scopes this query to a transaction. The
// conditional operational_audit_heads update below is the concurrency fence:
// it makes two PostgreSQL writers retry against a new previous hash instead
// of both inserting children of the same event.
func (q *Queries) AppendOperationalAudit(ctx context.Context, event *types.OperationalAuditEvent) error {
	if event == nil || event.Kind == "" || strings.TrimSpace(event.ResourceID) == "" || strings.TrimSpace(event.Actor) == "" || strings.TrimSpace(event.Action) == "" {
		return fmt.Errorf("operational audit event requires kind, resource ID, actor, and action")
	}
	if event.Time.IsZero() {
		event.Time = time.Now().UTC()
	}
	// The generic audit explorer must be able to enforce a tenant/cluster
	// filter without decoding arbitrary resource payloads. Resolve missing
	// scope from the durable envelope while both the head advance and INSERT
	// are still in this transaction. Incident-operation events predate the
	// v0.4 envelope and deliberately remain unscoped unless their caller
	// supplies an explicit scope.
	if event.Tenant == "" || event.Cluster == "" {
		var tenant, cluster string
		scopeErr := q.db.QueryRowContext(ctx,
			`SELECT tenant, cluster FROM operational_resources WHERE kind=? AND id=?`,
			string(event.Kind), event.ResourceID).Scan(&tenant, &cluster)
		switch {
		case scopeErr == nil:
			if event.Tenant == "" {
				event.Tenant = tenant
			}
			if event.Cluster == "" {
				event.Cluster = cluster
			}
		case errors.Is(scopeErr, sql.ErrNoRows):
			// No envelope is expected for the legacy incident table.
		default:
			return scopeErr
		}
	}
	providedPrevious, providedHash := event.PrevHash, event.Hash
	if _, err := q.db.ExecContext(ctx, `
		INSERT INTO operational_audit_heads (kind, resource_id, last_hash)
		VALUES (?, ?, '') ON CONFLICT(kind, resource_id) DO NOTHING`, string(event.Kind), event.ResourceID); err != nil {
		return err
	}
	var previous string
	advanced := false
	for attempts := 0; attempts < 8; attempts++ {
		if err := q.db.QueryRowContext(ctx, `
			SELECT last_hash FROM operational_audit_heads WHERE kind=? AND resource_id=?`, string(event.Kind), event.ResourceID).Scan(&previous); err != nil {
			return err
		}
		if providedPrevious != "" && providedPrevious != previous {
			return store.ErrOperationalConflict
		}
		event.PrevHash = previous
		calculatedHash := operationalAuditHash(*event)
		if providedHash != "" && providedHash != calculatedHash {
			return store.ErrOperationalConflict
		}
		event.Hash = calculatedHash
		update, err := q.db.ExecContext(ctx, `
			UPDATE operational_audit_heads SET last_hash=?
			WHERE kind=? AND resource_id=? AND last_hash=?`, event.Hash, string(event.Kind), event.ResourceID, previous)
		if err != nil {
			return err
		}
		if affected, _ := update.RowsAffected(); affected == 1 {
			advanced = true
			break
		}
	}
	if !advanced {
		return store.ErrOperationalConflict
	}
	params, err := json.Marshal(event.Params)
	if err != nil {
		return fmt.Errorf("marshal operational audit params: %w", err)
	}
	if err := q.db.QueryRowContext(ctx, `
		INSERT INTO operational_audit
			(kind, resource_id, tenant, cluster, time, actor, action, request_id, decision_id, params, result, prev_hash, hash)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		RETURNING id`,
		string(event.Kind), event.ResourceID, event.Tenant, event.Cluster, ts(event.Time), event.Actor, event.Action,
		event.RequestID, event.DecisionID, string(params), event.Result, event.PrevHash, event.Hash).Scan(&event.ID); err != nil {
		return err
	}
	return nil
}

func (q *Queries) ListOperationalAudit(ctx context.Context, kind types.OperationalResourceKind, resourceID string, limit int) ([]*types.OperationalAuditEvent, error) {
	if kind == "" || strings.TrimSpace(resourceID) == "" {
		return nil, fmt.Errorf("operational audit kind and resource ID are required")
	}
	if limit <= 0 {
		limit = 200
	}
	if limit > 1000 {
		return nil, fmt.Errorf("operational audit limit exceeds 1000")
	}
	rows, err := q.db.QueryContext(ctx, `
		SELECT id, kind, resource_id, tenant, cluster, time, actor, action, request_id, decision_id, params, result, prev_hash, hash
		FROM operational_audit WHERE kind=? AND resource_id=? ORDER BY id LIMIT ?`, string(kind), resourceID, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := make([]*types.OperationalAuditEvent, 0)
	for rows.Next() {
		event, err := scanOperationalAudit(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, event)
	}
	return out, rows.Err()
}

// ListOperationalAuditEvents is the bounded, cursor-paged audit explorer
// query. Individual resource chains stay available through
// ListOperationalAudit; this broader query intentionally does not attempt to
// merge or rewrite those hashes, it merely filters their globally immutable
// insertion order.
func (q *Queries) ListOperationalAuditEvents(ctx context.Context, filter types.OperationalAuditFilter) ([]*types.OperationalAuditEvent, error) {
	limit := filter.Limit
	if limit <= 0 {
		limit = 200
	}
	if limit > 1000 {
		return nil, fmt.Errorf("operational audit limit exceeds 1000")
	}
	clauses := []string{"id > ?"}
	args := []any{filter.AfterID}
	if filter.Kind != "" {
		clauses = append(clauses, "kind=?")
		args = append(args, string(filter.Kind))
	}
	if strings.TrimSpace(filter.ResourceID) != "" {
		clauses = append(clauses, "resource_id=?")
		args = append(args, filter.ResourceID)
	}
	if strings.TrimSpace(filter.Tenant) != "" {
		clauses = append(clauses, "tenant=?")
		args = append(args, filter.Tenant)
	}
	if strings.TrimSpace(filter.Cluster) != "" {
		clauses = append(clauses, "cluster=?")
		args = append(args, filter.Cluster)
	}
	if strings.TrimSpace(filter.Actor) != "" {
		clauses = append(clauses, "actor=?")
		args = append(args, filter.Actor)
	}
	if strings.TrimSpace(filter.RequestID) != "" {
		clauses = append(clauses, "request_id=?")
		args = append(args, filter.RequestID)
	}
	if strings.TrimSpace(filter.DecisionID) != "" {
		clauses = append(clauses, "decision_id=?")
		args = append(args, filter.DecisionID)
	}
	if strings.TrimSpace(filter.Action) != "" {
		clauses = append(clauses, "action=?")
		args = append(args, filter.Action)
	}
	if !filter.Since.IsZero() {
		clauses = append(clauses, "time >= ?")
		args = append(args, ts(filter.Since))
	}
	if !filter.Until.IsZero() {
		clauses = append(clauses, "time <= ?")
		args = append(args, ts(filter.Until))
	}
	args = append(args, limit)
	rows, err := q.db.QueryContext(ctx, `
		SELECT id, kind, resource_id, tenant, cluster, time, actor, action, request_id, decision_id, params, result, prev_hash, hash
		FROM operational_audit WHERE `+strings.Join(clauses, " AND ")+` ORDER BY id LIMIT ?`, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := make([]*types.OperationalAuditEvent, 0)
	for rows.Next() {
		event, err := scanOperationalAudit(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, event)
	}
	return out, rows.Err()
}

// HasActiveActionLease reports whether node has an action under a live lease:
// state 'leased' with lease_expires_at_ns strictly later than the current UTC
// time. It uses the same predicate the claim path uses for "held" rows, so its
// answer agrees with what a concurrent claimer would see. Pending, expired,
// and terminal rows never count; an unknown or empty node simply reports false.
func (q *Queries) HasActiveActionLease(ctx context.Context, node string) (bool, error) {
	nowNS := time.Now().UTC().UnixNano()
	var active bool
	err := q.db.QueryRowContext(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM actions
			WHERE node=? AND state='leased' AND lease_expires_at_ns > ?
		)`, node, nowNS).Scan(&active)
	if err != nil {
		return false, err
	}
	return active, nil
}

func validateOperationalResource(resource *types.OperationalResource) error {
	if resource == nil || resource.Kind == "" || strings.TrimSpace(resource.ID) == "" {
		return fmt.Errorf("operational resource requires kind and ID")
	}
	if len(resource.Payload) == 0 || !json.Valid(resource.Payload) {
		return fmt.Errorf("operational resource %s/%s has invalid normalized JSON payload", resource.Kind, resource.ID)
	}
	return nil
}

func scanOperationalResource(row rowScanner) (*types.OperationalResource, error) {
	resource := &types.OperationalResource{}
	var kind, payload, created, updated string
	var expires sql.NullString
	err := row.Scan(&kind, &resource.ID, &resource.State, &resource.Tenant, &resource.Cluster,
		&resource.Actor, &resource.ConfigDigest, &payload, &created, &updated, &expires, &resource.Version)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, store.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	resource.Kind = types.OperationalResourceKind(kind)
	resource.Payload = append(json.RawMessage(nil), payload...)
	resource.CreatedAt = parseTS(created)
	resource.UpdatedAt = parseTS(updated)
	if expires.Valid {
		t := parseTS(expires.String)
		resource.ExpiresAt = &t
	}
	return resource, nil
}

func scanOperationalIdempotency(row rowScanner) (*types.OperationalIdempotencyRecord, error) {
	record := &types.OperationalIdempotencyRecord{}
	var kind, created, expires string
	err := row.Scan(&kind, &record.Actor, &record.Key, &record.RequestDigest, &record.ResourceID, &created, &expires)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, store.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	record.Kind = types.OperationalResourceKind(kind)
	record.CreatedAt = parseTS(created)
	record.ExpiresAt = parseTS(expires)
	return record, nil
}

func scanOperationalAudit(row rowScanner) (*types.OperationalAuditEvent, error) {
	event := &types.OperationalAuditEvent{}
	var kind, at, params string
	err := row.Scan(&event.ID, &kind, &event.ResourceID, &event.Tenant, &event.Cluster, &at, &event.Actor, &event.Action,
		&event.RequestID, &event.DecisionID, &params, &event.Result, &event.PrevHash, &event.Hash)
	if err != nil {
		return nil, err
	}
	event.Kind = types.OperationalResourceKind(kind)
	event.Time = parseTS(at)
	if err := json.Unmarshal([]byte(params), &event.Params); err != nil {
		return nil, fmt.Errorf("operational audit %d: corrupt params: %w", event.ID, err)
	}
	return event, nil
}

func operationalAuditHash(event types.OperationalAuditEvent) string {
	// encoding/json sorts map keys, so this is deterministic across process
	// restarts. The ID is purposely excluded because it is database-assigned.
	//
	// Tenant/Cluster intentionally are not part of this pre-existing chain
	// payload. v0.4.0 scope columns were added after audit rows already existed;
	// hashing them would invalidate every retained historic hash (and therefore
	// every later PrevHash) during an additive migration. Their immutable
	// database values are still used for access filtering, while this canonical
	// payload stays stable across the schema evolution.
	payload := struct {
		Kind       types.OperationalResourceKind `json:"kind"`
		ResourceID string                        `json:"resource_id"`
		Time       string                        `json:"time"`
		Actor      string                        `json:"actor"`
		Action     string                        `json:"action"`
		RequestID  string                        `json:"request_id"`
		DecisionID string                        `json:"decision_id"`
		Params     map[string]string             `json:"params"`
		Result     string                        `json:"result"`
		PrevHash   string                        `json:"prev_hash"`
	}{
		Kind: event.Kind, ResourceID: event.ResourceID, Time: ts(event.Time),
		Actor: event.Actor, Action: event.Action, RequestID: event.RequestID,
		DecisionID: event.DecisionID, Params: event.Params, Result: event.Result,
		PrevHash: event.PrevHash,
	}
	blob, _ := json.Marshal(payload)
	digest := sha256.Sum256(blob)
	return "sha256:" + hex.EncodeToString(digest[:])
}

func uniqueConstraint(err error) bool {
	if err == nil {
		return false
	}
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "unique constraint") || strings.Contains(message, "duplicate key")
}
