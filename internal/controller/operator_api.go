package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/kubeneuron/kubeneuron/internal/approval"
	"github.com/kubeneuron/kubeneuron/internal/config"
	"github.com/kubeneuron/kubeneuron/internal/notify"
	"github.com/kubeneuron/kubeneuron/internal/playbook"
	"github.com/kubeneuron/kubeneuron/internal/store"
	"github.com/kubeneuron/kubeneuron/pkg/types"
)

// This file implements httpapi.OperatorBackend: the read and decision
// surface behind the authenticated operator API.

// ListIncidents returns incidents filtered by state names and node.
func (c *Controller) ListIncidents(ctx context.Context, states []string, node string, limit int) ([]*types.Incident, error) {
	f := store.IncidentFilter{Node: node, Limit: limit}
	for _, s := range states {
		f.States = append(f.States, types.IncidentState(s))
	}
	return c.store.ListIncidents(ctx, f)
}

// IncidentDetail returns one incident with its audit trail.
func (c *Controller) IncidentDetail(ctx context.Context, id string) (*types.Incident, []*types.AuditEntry, error) {
	inc, err := c.store.GetIncident(ctx, id)
	if err != nil {
		return nil, nil, err
	}
	trail, err := c.store.AuditTrail(ctx, id)
	if err != nil {
		return nil, nil, err
	}
	return inc, trail, nil
}

// DecideApproval records an authenticated human decision; the reconcile walk
// picks it up on its next pass. The decision is bound to the identity of the
// step that is current now — the one the human is approving — so a later
// hot-swap or rewind that changes the action at this index is caught at resume
// and the approval is not honored for an action the human never saw.
func (c *Controller) DecideApproval(ctx context.Context, id, actor, channel string, decision types.ApprovalDecision, expectedEpoch int, reason string) error {
	inc, err := c.store.GetIncident(ctx, id)
	if err != nil {
		return err
	}
	// The decision binds to the incident's CURRENT approval round: it inherits
	// the round's request record — the durable statement of what the round
	// asked — and the round's epoch, so a re-park (new epoch) orphans the
	// decision by construction and a swap AFTER the click cannot execute
	// under it. The notification-to-click half is covered by expectedEpoch
	// below, for every client that carries the round it displayed (the panel
	// and notification channels do; see RequestApproval).
	//
	// Epoch 0 is NOT a round: it is the pre-upgrade population, whose rows
	// (requests AND orphaned decisions from any number of old parks) all
	// carry park_epoch 0 — pairing them would let a stale approval execute a
	// step from a different park. Refuse; the walk re-parks epoch-0 incidents
	// into round 1, and the human decides that verifiable round.
	if inc.ApprovalEpoch == 0 {
		return fmt.Errorf("cannot record a decision for %s: its park predates approval rounds; the controller will re-park it — decide the fresh request", id)
	}
	// expectedEpoch closes the notification-to-click half of the hot-swap
	// window: when the client passes the round it DISPLAYED (>0) and a
	// re-park has since minted a newer round, the click must not be recorded
	// against content the human never saw. Zero means the client did not
	// carry a round (older CLI, raw curl) and keeps the bind-to-current
	// behavior, which the resume-time requestMismatch still guards.
	if expectedEpoch > 0 && expectedEpoch != inc.ApprovalEpoch {
		return fmt.Errorf("the approval request for %s changed since it was displayed (you decided round %d; round %d is current) — re-read the incident and decide again",
			id, expectedEpoch, inc.ApprovalEpoch)
	}
	request, err := c.store.GetApprovalRequest(ctx, id, inc.ApprovalEpoch)
	if err != nil {
		return fmt.Errorf("cannot record a decision for %s: its current approval round has no request record; wait for the controller to re-park it", id)
	}
	step := approval.StepIdentity{
		PlaybookName: request.PlaybookName,
		StepName:     request.StepName,
		StepAction:   request.StepAction,
		StepHash:     request.StepHash,
		ParkEpoch:    request.ParkEpoch,
	}
	if err := approval.New(c.store, c.runtimeConfig(ctx).ApprovalTTL).Decide(ctx, id, step, actor, channel, decision); err != nil {
		return err
	}
	// The decision moment itself belongs in the audit trail — with the
	// human's stated reason, which the API and CLI accept and promise to
	// record (review F2: it used to be silently dropped). The resume pass
	// later audits the resulting transition separately.
	if err := c.appendAudit(ctx, inc, actor,
		"approval-"+string(decision),
		firstNonBlank(reason, fmt.Sprintf("round %d %s via %s", inc.ApprovalEpoch, decision, channel))); err != nil {
		c.log.Error("decision audit append failed", "incident", id, "err", err)
	}
	return nil
}

// ResolveIncident manually resolves an incident (typically from
// NEEDS_HUMAN after out-of-band repair). The transition validator rejects
// states that must not be short-circuited, such as EXECUTING.
func (c *Controller) ResolveIncident(ctx context.Context, id, actor, reason string) error {
	inc, err := c.store.GetIncident(ctx, id)
	if err != nil {
		return err
	}
	if err := c.transition(ctx, inc, types.StateResolved, actor, "manual-resolve",
		firstNonBlank(reason, "manually resolved"), nil); err != nil {
		return err
	}
	return c.notify(ctx, notify.NotifyEvent{
		Kind: notify.EventResolved, Incident: inc,
		Message: fmt.Sprintf("manually resolved by %s: %s", actor, firstNonBlank(reason, "no reason given")),
	})
}

// ResolveIncidentRequest is the v0.4 idempotent/optimistic-concurrency form
// of manual resolution. The older ResolveIncident method remains for existing
// integrations, while the public API uses this form whenever the controller
// advertises it. A retry returns the resolved incident instead of attempting a
// second terminal transition or sending a second resolution notification.
func (c *Controller) ResolveIncidentRequest(ctx context.Context, id, actor, reason string, expectedVersion int, idempotencyKey string) (*types.Incident, bool, error) {
	if strings.TrimSpace(id) == "" || strings.TrimSpace(actor) == "" || strings.TrimSpace(idempotencyKey) == "" {
		return nil, false, fmt.Errorf("incident ID, actor, and idempotency key are required")
	}
	operational, ok := c.store.(store.OperationalStore)
	if !ok {
		return nil, false, fmt.Errorf("incident resolution requires the v0.4.0 operational store")
	}
	current, err := c.store.GetIncident(ctx, id)
	if err != nil {
		return nil, false, err
	}
	digest := incidentOperationDigest("resolve", id, actor, reason, expectedVersion)
	storageKey := incidentOperationKey("resolve", idempotencyKey)
	if existing, lookupErr := operational.GetOperationalIdempotency(ctx, types.ResourceIncidentOperation, actor, storageKey); lookupErr == nil {
		if existing.RequestDigest != digest || existing.ResourceID != id {
			return nil, false, store.ErrOperationalConflict
		}
		if current.State != types.StateResolved {
			// A record without the terminal result means the original request
			// lost a race or failed after reservation. Never masquerade that
			// partial write as a successful resolution; the caller must reread
			// and issue a fresh operation key if resolution is still appropriate.
			return nil, false, fmt.Errorf("%w: prior resolution request did not reach a terminal incident state", store.ErrOperationalConflict)
		}
		return current, true, nil
	} else if !errors.Is(lookupErr, store.ErrNotFound) {
		return nil, false, lookupErr
	}
	if expectedVersion > 0 && current.Version != expectedVersion {
		return nil, false, store.ErrConflict
	}
	// Validate the state transition before reserving the retry key. A typo or
	// stale terminal row must not poison a key an operator needs after rereading
	// the current incident.
	probe := *current
	if err := playbook.Transition(&probe, types.StateResolved); err != nil {
		return nil, false, err
	}
	record, created, err := operational.PutOperationalIdempotency(ctx, &types.OperationalIdempotencyRecord{
		Kind: types.ResourceIncidentOperation, Actor: actor, Key: storageKey,
		RequestDigest: digest, ResourceID: id,
	})
	if err != nil {
		return nil, false, err
	}
	if !created {
		if record.RequestDigest != digest || record.ResourceID != id {
			return nil, false, store.ErrOperationalConflict
		}
		incident, getErr := c.store.GetIncident(ctx, id)
		if getErr == nil && incident.State != types.StateResolved {
			return nil, false, fmt.Errorf("%w: prior resolution request did not reach a terminal incident state", store.ErrOperationalConflict)
		}
		return incident, true, getErr
	}
	if err := c.transition(ctx, current, types.StateResolved, actor, "manual-resolve", firstNonBlank(reason, "manually resolved"), nil); err != nil {
		return nil, false, err
	}
	if err := c.notify(ctx, notify.NotifyEvent{
		Kind: notify.EventResolved, Incident: current,
		Message: fmt.Sprintf("manually resolved by %s: %s", actor, firstNonBlank(reason, "no reason given")),
	}); err != nil {
		// The transactional state transition/audit is the operation's durable
		// result. Do not turn a notifier outage into a retry that appears
		// successful yet cannot re-deliver from this request key.
		c.log.Warn("manual resolution notification failed", "incident", id, "err", err)
	}
	if err := operational.AppendOperationalAudit(ctx, &types.OperationalAuditEvent{
		Kind: types.ResourceIncidentOperation, ResourceID: id, Time: current.UpdatedAt,
		Actor: actor, Action: "resolve", RequestID: idempotencyKey,
		Params: map[string]string{"reason": firstNonBlank(reason, "manually resolved")}, Result: "resolved",
	}); err != nil {
		c.log.Error("operational incident resolution audit append failed", "incident", id, "err", err)
	}
	return current, false, nil
}

// AcknowledgeIncident records that an identified operator has taken custody
// of an active incident without changing its remediation state.  It is a
// first-class, optimistic-versioned operation rather than a UI-only audit
// comment: callers can safely retry with the same idempotency key and an old
// browser tab cannot acknowledge a row that has already moved on.
func (c *Controller) AcknowledgeIncident(ctx context.Context, id, actor, reason string, expectedVersion int, idempotencyKey string) (*types.Incident, bool, error) {
	if strings.TrimSpace(id) == "" || strings.TrimSpace(actor) == "" || strings.TrimSpace(idempotencyKey) == "" {
		return nil, false, fmt.Errorf("incident ID, actor, and idempotency key are required")
	}
	operational, ok := c.store.(store.OperationalStore)
	if !ok {
		return nil, false, fmt.Errorf("incident acknowledgement requires the v0.4.0 operational store")
	}
	// Validate the caller's observed version before claiming the key. A bad
	// request must never leave an idempotency reservation that makes a later
	// corrected acknowledgement unretryable.
	current, err := c.store.GetIncident(ctx, id)
	if err != nil {
		return nil, false, err
	}
	digest := incidentOperationDigest("acknowledge", id, actor, reason, expectedVersion)
	storageKey := incidentOperationKey("acknowledge", idempotencyKey)
	if existing, lookupErr := operational.GetOperationalIdempotency(ctx, types.ResourceIncidentOperation, actor, storageKey); lookupErr == nil {
		if existing.RequestDigest != digest || existing.ResourceID != id {
			return nil, false, store.ErrOperationalConflict
		}
		return current, true, nil
	} else if !errors.Is(lookupErr, store.ErrNotFound) {
		return nil, false, lookupErr
	}
	if expectedVersion > 0 && current.Version != expectedVersion {
		return nil, false, store.ErrConflict
	}
	if current.State.Terminal() {
		return nil, false, fmt.Errorf("incident %q is already terminal", id)
	}
	record, created, err := operational.PutOperationalIdempotency(ctx, &types.OperationalIdempotencyRecord{
		Kind: types.ResourceIncidentOperation, Actor: actor, Key: storageKey,
		RequestDigest: digest, ResourceID: id,
	})
	if err != nil {
		return nil, false, err
	}
	if !created {
		if record.RequestDigest != digest || record.ResourceID != id {
			return nil, false, store.ErrOperationalConflict
		}
		incident, err := c.store.GetIncident(ctx, id)
		return incident, true, err
	}

	var acknowledged *types.Incident
	err = c.store.WithTx(ctx, func(tx store.Tx) error {
		incident, err := tx.GetIncident(ctx, id)
		if err != nil {
			return err
		}
		if expectedVersion > 0 && incident.Version != expectedVersion {
			return store.ErrConflict
		}
		if incident.State.Terminal() {
			return fmt.Errorf("incident %q is already terminal", id)
		}
		incident.UpdatedAt = time.Now().UTC()
		if err := tx.UpdateIncident(ctx, incident); err != nil {
			return err
		}
		if err := tx.AppendAudit(ctx, &types.AuditEntry{
			IncidentID: incident.ID, Time: incident.UpdatedAt, FromState: incident.State, ToState: incident.State,
			Actor: actor, Action: "acknowledge", Params: map[string]string{"reason": firstNonBlank(reason, "acknowledged")},
			Result: "acknowledged", DryRun: incident.DryRun,
		}); err != nil {
			return err
		}
		acknowledged = incident
		return nil
	})
	if err != nil {
		return nil, false, err
	}
	// The incident transition/audit above is the transactional system of
	// record. The operational chain adds global explorer correlation without
	// storing raw incident evidence a second time.
	if err := operational.AppendOperationalAudit(ctx, &types.OperationalAuditEvent{
		Kind: types.ResourceIncidentOperation, ResourceID: id, Time: acknowledged.UpdatedAt,
		Actor: actor, Action: "acknowledge", RequestID: idempotencyKey,
		Params: map[string]string{"reason": firstNonBlank(reason, "acknowledged")}, Result: "acknowledged",
	}); err != nil {
		// The incident mutation and its native audit record committed together
		// above, so reporting this secondary explorer-index failure as a failed
		// acknowledgement would make a client retry look like an idempotent
		// success while never repairing the outcome.  Keep the transactional
		// incident trail authoritative and surface the degraded global explorer
		// through logs/monitoring instead.
		c.log.Error("operational incident acknowledgement audit append failed", "incident", id, "err", err)
	}
	return acknowledged, false, nil
}

func incidentOperationDigest(action, id, actor, reason string, expectedVersion int) string {
	value := strings.Join([]string{action, id, actor, reason, fmt.Sprintf("%d", expectedVersion)}, "\x00")
	sum := sha256.Sum256([]byte(value))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// incidentOperationKey keeps a client retry token local to one endpoint. The
// same operator can safely use a UI-generated key for an acknowledgement and
// a later resolution without one lifecycle operation shadowing the other.
func incidentOperationKey(action, key string) string { return action + ":" + key }

// Nodes lists the persisted inventory.
func (c *Controller) Nodes(ctx context.Context) ([]*types.Node, error) {
	return c.store.ListNodes(ctx)
}

// Node returns one inventory record.
func (c *Controller) Node(ctx context.Context, name string) (*types.Node, error) {
	return c.store.GetNode(ctx, name)
}

// AcceleratorReports returns the latest retained report for each accelerator
// vendor on one known node. It is read-only operational evidence: exposing a
// ready report never changes execution mode or grants an action capability.
func (c *Controller) AcceleratorReports(ctx context.Context, node string) ([]*types.AgentAcceleratorReport, error) {
	if _, err := c.store.GetNode(ctx, node); err != nil {
		return nil, err
	}
	reports, ok := c.store.(store.AcceleratorReportStore)
	if !ok {
		return nil, fmt.Errorf("accelerator report store is not configured")
	}
	return reports.ListAcceleratorReports(ctx, node)
}

// AcceleratorObservationProfile returns only the immutable digest that the
// configured runtime profile selects for an authenticated node and vendor.
// It is an observation-binding API, not an action authorization API: a nil
// result is the normal default-deny outcome when no profile selects the node.
// Ambiguous selectors, invalid configuration, and unavailable node labels are
// errors so the HTTP layer can fail closed rather than choose an arbitrary
// profile.
func (c *Controller) AcceleratorObservationProfile(ctx context.Context, node string, vendor types.AcceleratorVendor) (*types.AgentAcceleratorObservationProfile, error) {
	if node == "" || !vendor.Valid() {
		return nil, fmt.Errorf("accelerator observation profile requires node and supported vendor")
	}
	labels := c.nodeLabels(ctx, node)
	if labels == nil {
		return nil, fmt.Errorf("node labels are unavailable for runtime profile selection")
	}
	profiles := c.runtimeConfig(ctx).AcceleratorProfiles
	profile, err := (config.Config{AcceleratorProfiles: profiles}).ResolveAcceleratorRuntimeProfile(labels, vendor)
	if errors.Is(err, config.ErrNoAcceleratorRuntimeProfile) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &types.AgentAcceleratorObservationProfile{
		Vendor:            profile.Vendor,
		ProfileDigest:     profile.ProfileDigest,
		ProfileUID:        profile.ProfileUID,
		ProfileGeneration: profile.ProfileGeneration,
		RuntimeVersion:    profile.RuntimeVersion,
	}, nil
}

// SetPaused flips the global automation pause (the big red button) only after
// its replacement state is durable.  The caller returns an error instead of
// acknowledging a process-local pause that a leader failover would erase.
func (c *Controller) SetPaused(ctx context.Context, paused bool, actor string) error {
	if c.gate == nil {
		return fmt.Errorf("global pause is unavailable: safety gate is not configured")
	}
	c.dispatchMu.Lock()
	defer c.dispatchMu.Unlock()
	if !paused {
		// A resumption is a new authorization boundary, not permission to run
		// arbitrary work that accumulated while paused.
		if err := c.cancelUndeliveredActionsForSafetyStop(ctx); err != nil {
			return fmt.Errorf("cancel queued actions before resuming automation: %w", err)
		}
		if err := c.gate.SetPaused(ctx, false, actor); err != nil {
			return err
		}
	} else {
		// Persist the red button before acknowledging it. Once that succeeds the
		// controller is fail-closed even if queue cancellation has a transient
		// store error and the caller receives a retryable response.
		if err := c.gate.SetPaused(ctx, true, actor); err != nil {
			return err
		}
		if err := c.cancelUndeliveredActionsForSafetyStop(ctx); err != nil {
			return fmt.Errorf("cancel queued actions after pausing automation: %w", err)
		}
	}
	c.log.Warn("automation pause changed", "paused", paused, "actor", actor)
	return nil
}

// Paused reports the global pause state.
func (c *Controller) Paused() bool { return c.gate.Paused() }
