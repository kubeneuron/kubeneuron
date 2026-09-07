package types

import (
	"encoding/json"
	"time"
)

// OperationalResourceKind names a durable v0.4.0 product resource.  These
// values are intentionally stable because they are used as storage partitions,
// audit filters, and API link targets.
type OperationalResourceKind string

const (
	ResourceDecisionSnapshot       OperationalResourceKind = "decision-snapshot"
	ResourceCandidateConfiguration OperationalResourceKind = "candidate-configuration"
	ResourcePolicyImpactPreview    OperationalResourceKind = "policy-impact-preview"
	ResourceHealthCheckRun         OperationalResourceKind = "health-check-run"
	ResourceRemediationSimulation  OperationalResourceKind = "remediation-simulation"
	// ResourceIncidentOperation is an idempotency/audit partition for v0.4.0
	// first-class incident operations. Incidents themselves remain in the
	// established workflow table; this avoids duplicating their state machine
	// merely to give acknowledge/resolve-style requests durable correlation.
	ResourceIncidentOperation OperationalResourceKind = "incident-operation"
	ResourceAutonomyPlan      OperationalResourceKind = "gpu-autonomy-plan"
	ResourceAutonomyRollout   OperationalResourceKind = "autonomy-rollout"
	// ResourceAutonomyEffect is the durable, idempotent hand-off record that
	// must exist before a hardware-qualified autonomy executor is called.
	ResourceAutonomyEffect OperationalResourceKind = "autonomy-effect"
)

// OperationalResource is the durable envelope around a versioned v0.4.0
// resource.  Payload is normalized JSON owned by the feature package; the
// envelope owns concurrency, scope, lifecycle, retention, and audit linkage.
// It deliberately does not store raw diagnostic blobs outside the payload so
// callers can redact and authorize those separately.
type OperationalResource struct {
	Kind         OperationalResourceKind `json:"kind"`
	ID           string                  `json:"id"`
	State        string                  `json:"state"`
	Tenant       string                  `json:"tenant,omitempty"`
	Cluster      string                  `json:"cluster,omitempty"`
	Actor        string                  `json:"actor,omitempty"`
	ConfigDigest string                  `json:"config_digest,omitempty"`
	Payload      json.RawMessage         `json:"payload"`
	CreatedAt    time.Time               `json:"created_at"`
	UpdatedAt    time.Time               `json:"updated_at"`
	ExpiresAt    *time.Time              `json:"expires_at,omitempty"`
	Version      int                     `json:"resource_version"`
}

// Clone returns a deep copy suitable for concurrent API consumers.
func (r *OperationalResource) Clone() *OperationalResource {
	if r == nil {
		return nil
	}
	out := *r
	out.Payload = append(json.RawMessage(nil), r.Payload...)
	if r.ExpiresAt != nil {
		t := *r.ExpiresAt
		out.ExpiresAt = &t
	}
	return &out
}

// OperationalResourceFilter is deliberately small and storage-neutral.  The
// API layer handles authorization before passing tenant/cluster filters down;
// the store provides stable resource-version order and bounded pagination.
type OperationalResourceFilter struct {
	Kind    OperationalResourceKind
	State   string
	Tenant  string
	Cluster string
	Limit   int
	// CreatedSince/CreatedUntil are inclusive time-window bounds for API
	// explorers and retention-safe exports. They apply before the opaque
	// (created_at,id) cursor, preserving a stable page order.
	CreatedSince   time.Time
	CreatedUntil   time.Time
	AfterCreatedAt time.Time
	AfterID        string
	IncludeExpired bool
}

// OperationalAuditEvent is append-only evidence of a product operation.  Hash
// and PrevHash form a per-resource tamper-evident chain; immutable audit
// exports include both rather than trusting a mutable summary field.
type OperationalAuditEvent struct {
	ID         int64                   `json:"id"`
	Kind       OperationalResourceKind `json:"kind"`
	ResourceID string                  `json:"resource_id"`
	// Tenant and Cluster are copied from the durable resource envelope at
	// append time. They make the global audit explorer scopeable without
	// rehydrating arbitrary payloads (and remain available after a summary is
	// eventually pruned).
	Tenant     string            `json:"tenant,omitempty"`
	Cluster    string            `json:"cluster,omitempty"`
	Time       time.Time         `json:"time"`
	Actor      string            `json:"actor"`
	Action     string            `json:"action"`
	RequestID  string            `json:"request_id,omitempty"`
	DecisionID string            `json:"decision_id,omitempty"`
	Params     map[string]string `json:"params,omitempty"`
	Result     string            `json:"result,omitempty"`
	PrevHash   string            `json:"prev_hash,omitempty"`
	Hash       string            `json:"hash"`
}

// OperationalAuditFilter supports the audit explorer without making callers
// scrape every resource's individual chain. All populated fields are ANDed;
// storage preserves the immutable global insertion order for cursor paging.
type OperationalAuditFilter struct {
	Kind       OperationalResourceKind
	ResourceID string
	Tenant     string
	Cluster    string
	Actor      string
	RequestID  string
	DecisionID string
	Action     string
	Since      time.Time
	Until      time.Time
	AfterID    int64
	Limit      int
}

// OperationalIdempotencyRecord binds a caller's idempotency key to exactly
// one immutable request digest and resource.  A key reused with a different
// request digest is a conflict, never a second side effect.
type OperationalIdempotencyRecord struct {
	Kind          OperationalResourceKind `json:"kind"`
	Actor         string                  `json:"actor"`
	Key           string                  `json:"key"`
	RequestDigest string                  `json:"request_digest"`
	ResourceID    string                  `json:"resource_id"`
	CreatedAt     time.Time               `json:"created_at"`
	ExpiresAt     time.Time               `json:"expires_at"`
}
