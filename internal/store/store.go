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
	// ErrConflict is returned by UpdateIncident when the row still exists but
	// its version no longer matches the caller's snapshot: another writer
	// advanced it in between. It is the optimistic-concurrency signal that the
	// caller must re-read and retry rather than overwrite; it is deliberately
	// distinct from ErrNotFound, which means the row is gone for good.
	ErrConflict = errors.New("store: incident update conflict")
	// ErrLeaseLost is returned when an action result is submitted with a
	// lease that is no longer current. Callers must not treat this as a
	// successful completion: the action may have been reclaimed and executed
	// by another agent after its lease expired.
	ErrLeaseLost = errors.New("store: action lease lost")
	// ErrExecutorBootMismatch is returned when an action result arrives from
	// a different node boot than the one that claimed the action: the node
	// rebooted mid-execution and the outcome must be treated as unknown.
	ErrExecutorBootMismatch = errors.New("store: action result from a different executor boot")
	// ErrActionForeignNode is returned when a node submits a result for an
	// action that belongs to a different node. It is an authorization failure,
	// not a transient one: the caller must never retry, and the API surfaces it
	// as a 403 rather than a 5xx.
	ErrActionForeignNode = errors.New("store: action belongs to another node")
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
	// ActiveSince keeps incidents whose lifetime reaches into [ActiveSince,
	// now): everything not lifecycle-terminal is kept regardless of when it
	// opened, and a terminal incident is kept when it ended at or after the
	// cutoff. It is the window filter behind the recovery report — an
	// incident opened last month and resolved yesterday still cost capacity
	// yesterday, so "opened after the cutoff" would be the wrong question.
	//
	// It is a PREFILTER: implementations may keep a little more than asked
	// (see the sqlcore note on text timestamp ordering). Callers that need an
	// exact boundary must compare the returned timestamps themselves.
	ActiveSince time.Time
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
	// GetIncident reads an incident by ID through the transaction, so a caller
	// can re-read the current row (and its optimistic version) inside the same
	// transaction as the mutation it is about to apply.
	GetIncident(ctx context.Context, id string) (*types.Incident, error)
	CreateIncident(ctx context.Context, inc *types.Incident) error
	// UpdateIncident persists inc guarded by its optimistic version. It returns
	// ErrConflict when the stored row has a newer version than inc.Version, and
	// ErrNotFound when the row is absent. On success it bumps inc.Version to the
	// value it just persisted.
	UpdateIncident(ctx context.Context, inc *types.Incident) error
	// PromoteIncidentTarget gives an incident that could only be addressed by
	// PCI address the GPU UUID a later, more precise signal carried. It is the
	// ONLY statement in this store that changes an incident's device identity,
	// and it is fenced on the incident's version, on the UUID still being
	// empty, and on the PCI address it was matched by, so a promotion happens
	// exactly once even when two precise signals are ingested concurrently.
	// It returns ErrConflict when the row is no longer promotable (already
	// promoted, advanced, or terminal) and ErrNotFound when it is gone; both
	// mean re-read and retry, never overwrite.
	//
	// Without it an incident opened by a kernel fault stayed unattributed for
	// life, and the reset preflight — correctly treating an empty UUID as
	// permanently unfixable — parked a node that had already been cordoned and
	// drained for a human, although the exact device had been identified
	// seconds after the fault.
	PromoteIncidentTarget(ctx context.Context, inc *types.Incident, to types.Target) error
	AppendAudit(ctx context.Context, e *types.AuditEntry) error
	RecordApproval(ctx context.Context, a *types.Approval) error
}

// Store is the persistence interface used by the controller.
type Store interface {
	// WithTx runs fn inside one transaction: either every mutation made
	// through the Tx commits, or none do. fn must not retain the Tx.
	WithTx(ctx context.Context, fn func(Tx) error) error

	// Incidents. UpdateIncident is guarded by the incident's optimistic
	// version: it returns ErrConflict when a concurrent writer has advanced the
	// row past inc.Version, and bumps inc.Version on success.
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
	// ExecutedStepResults returns, for each requested incident that has any,
	// the Result text of every audit row recording a transition INTO
	// EXECUTING. It is the durable answer to "did a playbook step actually
	// run for this incident", which nothing on the incident row can give:
	// StepIndex is reset by every escalation and RemediationSlotHeld is
	// cleared when the incident halts.
	//
	// Bulk rather than per incident on purpose. The recovery report asks this
	// of every resolved incident in its window, and AuditTrail would issue one
	// query per row of a report that already caps itself at 100,000 rows.
	// Incidents with no such row are absent from the map rather than present
	// with an empty slice, so "nothing ran" and "not asked about" stay
	// distinguishable at the call site.
	ExecutedStepResults(ctx context.Context, incidentIDs []string) (map[string][]string, error)

	// Approvals
	RecordApproval(ctx context.Context, a *types.Approval) error
	// LatestApproval returns the most recent approvals ROW of any kind for an
	// incident (requests and decisions alike), or ErrNotFound.
	//
	// Deprecated for protocol decisions: it ignores approval rounds, which is
	// exactly the bug class the round-scoped queries below retired. Use
	// GetApprovalRequest/LatestApprovalDecision with the incident's
	// ApprovalEpoch; this remains only as a raw inspection read (tests, UI).
	LatestApproval(ctx context.Context, incidentID string) (*types.Approval, error)
	// GetApprovalRequest returns the "requested" record of one approval round
	// (park epoch) — what the human was asked. ErrNotFound: no record; a
	// decision must not be honored for such a round.
	GetApprovalRequest(ctx context.Context, incidentID string, epoch int) (*types.Approval, error)
	// LatestApprovalDecision returns the newest human decision of one round,
	// or ErrNotFound while the round is undecided. Decisions of earlier
	// epochs are invisible by construction.
	LatestApprovalDecision(ctx context.Context, incidentID string, epoch int) (*types.Approval, error)

	// Action queue (controller -> agent dispatch). EnqueueAction is
	// idempotent on Action.ID; the owning incident is Action.IncidentID
	// (empty for unowned work such as janitor restores).
	// ClaimNextAction atomically leases the oldest
	// available action for a node. At most one unexpired lease can exist for a
	// node, and an expired lease is eligible to be reclaimed. The returned
	// QueuedAction carries the opaque lease token that must be supplied to
	// CompleteClaimedAction. The requested duration is a minimum; the store
	// extends the lease to cover the action's declared timeout. A non-positive
	// lease duration is invalid.
	EnqueueAction(ctx context.Context, node string, a types.Action) error
	ClaimNextAction(ctx context.Context, node, bootID string, leaseDuration time.Duration) (*types.QueuedAction, error)
	// CompleteClaimedAction conditionally completes an action only when the
	// supplied lease token still owns an unexpired lease. It returns
	// ErrLeaseLost for a stale, expired, or incorrect token.
	CompleteClaimedAction(ctx context.Context, actionID, leaseToken, bootID string, res types.ActionResult) error
	// CancelPendingActionsForIncident tombstones the incident's undelivered
	// ('pending') actions; leased work may already be executing and stays.
	CancelPendingActionsForIncident(ctx context.Context, incidentID string) (int64, error)
	// CancelPendingActionsForSafetyStop tombstones undelivered destructive
	// actions across the fleet when automation is paused or switched to
	// DryRun. restore_accelerator_host is deliberately spared: it is an undo
	// operation which puts monitoring back after a prior quiesce.
	//
	// As with incident cancellation, pending work and expired leases are safe
	// to revoke; an unexpired lease may already be executing and is left alone.
	CancelPendingActionsForSafetyStop(ctx context.Context) (int64, error)

	// NextPendingAction and CompleteAction are legacy unleased helpers. New
	// dispatch paths must use ClaimNextAction and CompleteClaimedAction so a
	// stale agent result cannot complete a reclaimed action.
	NextPendingAction(ctx context.Context, node string) (*types.QueuedAction, error)
	CompleteAction(ctx context.Context, actionID string, res types.ActionResult) error
	GetAction(ctx context.Context, actionID string) (*types.QueuedAction, error)
	// DiscardCompletedAction drops a TERMINAL row — completed, dead-lettered
	// or cancelled — so a caller with a deterministic action ID can begin a new
	// attempt instead of re-reading the previous one's stored result or
	// conflicting with a row no agent can ever claim. Pending and leased rows
	// are untouched.
	DiscardCompletedAction(ctx context.Context, actionID string) error

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
