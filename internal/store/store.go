// Package store persists controller state: incidents, the append-only audit
// log, approvals, and node inventory. The default implementation is SQLite
// (internal/store/sqlite); a Postgres implementation can slot in behind the
// same interface for multi-writer HA setups.
package store

import (
	"context"
	"errors"
	"time"

	"github.com/kubeneuron/kubeneuron/pkg/types"
)

var (
	// ErrNotFound is returned when a requested record does not exist.
	ErrNotFound = errors.New("store: not found")
	// ErrLeaseLost is returned when an action result is submitted with a
	// lease that is no longer current. Callers must not treat this as a
	// successful completion: the action may have been reclaimed and executed
	// by another agent after its lease expired.
	ErrLeaseLost = errors.New("store: action lease lost")
	// ErrExecutorBootMismatch is returned when an action result arrives from
	// a different node boot than the one that claimed the action: the node
	// rebooted mid-execution and the outcome must be treated as unknown.
	ErrExecutorBootMismatch = errors.New("store: action result from a different executor boot")
	// ErrEventLeaseLost is returned when an outbox event is acknowledged with
	// a lease that is no longer current. The worker must not treat this as a
	// successful completion: another worker may have reclaimed the event after
	// its lease expired.
	ErrEventLeaseLost = errors.New("store: event lease lost")
	// ErrStaleAcceleratorReport is returned when a report would replace a
	// newer report for the same node and accelerator vendor, or when a
	// different report claims the same observation timestamp. Callers must
	// obtain a fresh preflight observation rather than treating stale runtime
	// capability data as current.
	ErrStaleAcceleratorReport = errors.New("store: stale accelerator report")
)

// IncidentFilter narrows ListIncidents.
type IncidentFilter struct {
	States []types.IncidentState
	Node   string
	Limit  int
}

// Tx groups mutations that must commit atomically. The state machine
// contract (internal/playbook/statemachine.go) requires an incident change
// and its audit entry to persist in the same transaction.
type Tx interface {
	// GetOpenIncident reads the current non-terminal incident for a target and
	// problem class through the same transaction as its subsequent mutation.
	// It is deliberately the only incident read exposed on Tx today: event
	// workflow consumers use it to decide whether to open or update an
	// incident, then persist the decision and its audit entry atomically.
	GetOpenIncident(ctx context.Context, target types.Target, class types.ProblemClass) (*types.Incident, error)
	CreateIncident(ctx context.Context, inc *types.Incident) error
	UpdateIncident(ctx context.Context, inc *types.Incident) error
	AppendAudit(ctx context.Context, e *types.AuditEntry) error
	RecordApproval(ctx context.Context, a *types.Approval) error
}

// Store is the persistence interface used by the controller.
type Store interface {
	// WithTx runs fn inside one transaction: either every mutation made
	// through the Tx commits, or none do. fn must not retain the Tx.
	WithTx(ctx context.Context, fn func(Tx) error) error

	// Incidents
	CreateIncident(ctx context.Context, inc *types.Incident) error
	UpdateIncident(ctx context.Context, inc *types.Incident) error
	GetIncident(ctx context.Context, id string) (*types.Incident, error)
	// GetOpenIncident returns the non-terminal incident for (target, class),
	// or ErrNotFound.
	GetOpenIncident(ctx context.Context, target types.Target, class types.ProblemClass) (*types.Incident, error)
	ListIncidents(ctx context.Context, f IncidentFilter) ([]*types.Incident, error)
	// CountIncidentsByState returns incident counts grouped by state.
	CountIncidentsByState(ctx context.Context) (map[types.IncidentState]int, error)

	// Audit log (append-only)
	AppendAudit(ctx context.Context, e *types.AuditEntry) error
	AuditTrail(ctx context.Context, incidentID string) ([]*types.AuditEntry, error)

	// Approvals
	RecordApproval(ctx context.Context, a *types.Approval) error
	// LatestApproval returns the most recent decision for an incident, or
	// ErrNotFound when none has been recorded.
	LatestApproval(ctx context.Context, incidentID string) (*types.Approval, error)

	// Action queue (controller -> agent dispatch). EnqueueAction is
	// idempotent on Action.ID. ClaimNextAction atomically leases the oldest
	// available action for a node. At most one unexpired lease can exist for a
	// node, and an expired lease is eligible to be reclaimed. The returned
	// QueuedAction carries the opaque lease token that must be supplied to
	// CompleteClaimedAction. The requested duration is a minimum; the store
	// extends the lease to cover the action's declared timeout. A non-positive
	// lease duration is invalid.
	EnqueueAction(ctx context.Context, node, incidentID string, a types.Action) error
	ClaimNextAction(ctx context.Context, node, bootID string, leaseDuration time.Duration) (*types.QueuedAction, error)
	// CompleteClaimedAction conditionally completes an action only when the
	// supplied lease token still owns an unexpired lease. It returns
	// ErrLeaseLost for a stale, expired, or incorrect token.
	CompleteClaimedAction(ctx context.Context, actionID, leaseToken, bootID string, res types.ActionResult) error
	// CancelPendingActionsForIncident tombstones the incident's undelivered
	// ('pending') actions; leased work may already be executing and stays.
	CancelPendingActionsForIncident(ctx context.Context, incidentID string) (int64, error)

	// NextPendingAction and CompleteAction are legacy unleased helpers. New
	// dispatch paths must use ClaimNextAction and CompleteClaimedAction so a
	// stale agent result cannot complete a reclaimed action.
	NextPendingAction(ctx context.Context, node string) (*types.QueuedAction, error)
	CompleteAction(ctx context.Context, actionID string, res types.ActionResult) error
	GetAction(ctx context.Context, actionID string) (*types.QueuedAction, error)

	// Node inventory / state
	UpsertNode(ctx context.Context, n *types.Node) error
	// UpsertAgentRegistration persists only agent-owned inventory fields. On an
	// existing node it must preserve controller-managed inventory and state.
	UpsertAgentRegistration(ctx context.Context, n *types.Node) error
	GetNode(ctx context.Context, name string) (*types.Node, error)
	ListNodes(ctx context.Context) ([]*types.Node, error)
	// ApplyNodeConfigPauses makes the given set the complete list of paused
	// nodes: listed nodes become paused (inventory rows are created as
	// needed), every other node is unpaused, atomically. Node configs are
	// the single source of per-node pause state.
	ApplyNodeConfigPauses(ctx context.Context, pausedNodes []string) error

	Close() error
}

// EventSink receives raw events for long-term archival/analytics. The
// default sink is the primary store itself; a ClickHouse sink can be added
// for fleet-scale analytics (design.md §ClickHouse) — the controller fans
// out to all configured sinks.
type EventSink interface {
	// WriteEvent archives ev. It reports false when an event with the same
	// non-empty EventID is already stored — an at-least-once replay
	// duplicate the caller must not process again.
	WriteEvent(ctx context.Context, ev *types.AgentEvent) (bool, error)
}

// ClaimedEvent is a raw agent event leased to a workflow worker. OutboxID is
// an opaque durable queue identifier; Event.EventID remains the agent's
// capture-time identifier used to deduplicate at-least-once delivery.
//
// A worker may process an event at least once: if it dies before completing
// the lease, another worker can reclaim it after LeaseExpiresAt. Workflow
// consumers must therefore use Event.EventID (when present) as their own
// idempotency key.
type ClaimedEvent struct {
	OutboxID       int64
	Event          types.AgentEvent
	Attempt        int
	LeaseToken     string
	LeaseExpiresAt time.Time
}

// EventOutbox is the durable hand-off from event archival to asynchronous
// workflow processing. ArchiveAndEnqueueEvent commits the raw archive row and
// its pending outbox row in one transaction; a successful replay of an
// already archived non-empty EventID returns fresh=false and never enqueues a
// second workflow item.
//
// ClaimNextEvent leases one pending (or expired) item to workerID. Completion
// is conditional on the still-current, unexpired lease token, preventing a
// stale worker from acknowledging work reclaimed by another worker.
// Implementations may support multiple concurrent workers.
type EventOutbox interface {
	EventSink

	ArchiveAndEnqueueEvent(ctx context.Context, ev *types.AgentEvent) (fresh bool, err error)
	ClaimNextEvent(ctx context.Context, workerID string, leaseDuration time.Duration) (*ClaimedEvent, error)
	// ProcessClaimedEvent runs fn and marks the claimed workflow item done in
	// one transaction, but only while leaseToken is still the current,
	// unexpired claim. A stale token runs no callback. If fn returns an error
	// (or the claim expires before commit), every mutation made through Tx and
	// the completion marker roll back together. The event can then be retried
	// after its lease expires.
	ProcessClaimedEvent(ctx context.Context, outboxID int64, leaseToken string, fn func(Tx) error) error

	// CompleteClaimedEvent is retained for consumers that have no state change
	// to commit with the acknowledgement. New workflow consumers should use
	// ProcessClaimedEvent so incident/audit changes cannot be committed before
	// their durable outbox acknowledgement.
	CompleteClaimedEvent(ctx context.Context, outboxID int64, leaseToken string) error
}

// AcceleratorReportStore retains the latest agent-owned accelerator runtime
// report for every (node, vendor) pair. It is deliberately a narrow optional
// extension rather than part of Store while controller wiring is introduced:
// existing consumers that only need incidents, actions, and inventory do not
// gain an accidental runtime-profile dependency.
//
// UpsertAcceleratorReport is idempotent for an identical replay. It returns
// ErrStaleAcceleratorReport when report.ObservedAt is older than the retained
// report, or when a different payload claims the same timestamp. That
// fail-closed ordering prevents out-of-order agent delivery from restoring an
// obsolete readiness or capability declaration.
type AcceleratorReportStore interface {
	UpsertAcceleratorReport(ctx context.Context, report *types.AgentAcceleratorReport) error
	GetAcceleratorReport(ctx context.Context, node string, vendor types.AcceleratorVendor) (*types.AgentAcceleratorReport, error)
	// ListAcceleratorReports returns reports for node, ordered by node then
	// vendor. An empty node lists the current report for every node/vendor.
	ListAcceleratorReports(ctx context.Context, node string) ([]*types.AgentAcceleratorReport, error)
}
