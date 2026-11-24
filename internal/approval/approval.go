// SPDX-License-Identifier: Apache-2.0

// Package approval manages the human-approval workflow for risky playbook
// steps: parking incidents in AWAITING_APPROVAL, recording decisions from
// any channel (Slack, CLI, Web UI), and expiring stale requests.
package approval

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/kubeneuron/kubeneuron/internal/store"
	"github.com/kubeneuron/kubeneuron/pkg/types"
)

// ChangedRequest marks a decision bound to an outdated round or impact preview.
type ChangedRequest struct{ error }

func RequestChanged(err error) error   { return ChangedRequest{err} }
func (e ChangedRequest) Unwrap() error { return e.error }

func ExpiredRequest(id string) error {
	//nolint:staticcheck // This operator-facing message starts a complete sentence.
	return fmt.Errorf("The approval request for %s has already expired, so this decision was not recorded. See the incident's current state with kubeneuronctl incidents show %s", id, id)
}

type AlreadyDecided struct {
	IncidentID string
	Round      int
	Prior      *types.Approval
}

func (e AlreadyDecided) Error() string {
	return fmt.Sprintf("approval round %d of %s is already decided (%s by %s); the first decision is final", e.Round, e.IncidentID, e.Prior.Decision, e.Prior.Actor)
}

// Manager records approval decisions and expires stale requests. The
// controller consults it before resuming an AWAITING_APPROVAL incident.
type Manager struct {
	store store.Store
	ttl   time.Duration
}

// New builds a Manager; ttl bounds how long a request may stay pending.
func New(s store.Store, ttl time.Duration) *Manager {
	return &Manager{store: s, ttl: ttl}
}

// StepIdentity binds a decision to the specific step it was made for — the
// identity of the approval round's request — so a later hot-swap or rewind
// that changes the action at the incident's current step index is detected at
// resume and the decision is not honored for an action the human never saw.
// ParkEpoch scopes the decision to one approval round: a re-park mints a new
// epoch and orphans the decision by construction.
type StepIdentity struct {
	PlaybookName string
	StepName     string
	StepAction   string
	StepHash     string
	ParkEpoch    int
	// ImpactDigest is the impact preview the human was shown, when the
	// client carried one and the controller accepted it.
	ImpactDigest string
}

// Decide records a decision for an incident's pending step. The caller
// (HTTP API) has already authenticated the actor and resolved the identity of
// the step being approved.
//
// A round takes one decision: once it is decided, a second approve or reject
// is refused rather than recorded, so a reject can never be overridden by a
// later approve. A request older than the TTL is refused too — it is expired
// even if the reconcile walk has not moved it to EXPIRED yet.
func (m *Manager) Decide(ctx context.Context, incidentID string, step StepIdentity, actor, channel string, decision types.ApprovalDecision) error {
	inc, err := m.store.GetIncident(ctx, incidentID)
	if err != nil {
		return err
	}
	if inc.State != types.StateAwaitingApproval {
		return fmt.Errorf("incident %s is in state %s, not awaiting approval", incidentID, inc.State)
	}
	now := time.Now()
	if m.expiredAt(inc, now) {
		return ExpiredRequest(incidentID)
	}
	prior, err := m.store.FirstApprovalDecision(ctx, incidentID, step.ParkEpoch)
	switch {
	case err == nil:
		return AlreadyDecided{incidentID, step.ParkEpoch, prior}
	case !errors.Is(err, store.ErrNotFound):
		return err
	}
	return m.store.RecordApproval(ctx, &types.Approval{
		IncidentID:   incidentID,
		StepName:     step.StepName,
		Decision:     decision,
		Actor:        actor,
		Channel:      channel,
		At:           now,
		PlaybookName: step.PlaybookName,
		StepAction:   step.StepAction,
		StepHash:     step.StepHash,
		ParkEpoch:    step.ParkEpoch,
		ImpactDigest: step.ImpactDigest,
	})
}

// Expired reports whether an incident has been awaiting approval longer
// than the TTL and must move to EXPIRED (and be re-notified). The TTL
// anchors to StateChangedAt — the moment the incident entered
// AWAITING_APPROVAL — not UpdatedAt, which every attached duplicate signal
// bumps and which would let a signal storm postpone expiry indefinitely.
func (m *Manager) Expired(inc *types.Incident) bool {
	return m.expiredAt(inc, time.Now())
}

func (m *Manager) expiredAt(inc *types.Incident, now time.Time) bool {
	return inc.State == types.StateAwaitingApproval && m.ttl > 0 && now.Sub(inc.StateChangedAt) > m.ttl
}

// DecidedInTime reports whether a decision was made while its request was
// still live: at or before StateChangedAt + ttl. A decision recorded after
// that (a click racing the expiry) must not be honored.
func DecidedInTime(inc *types.Incident, decision *types.Approval, ttl time.Duration) bool {
	return ttl <= 0 || !decision.At.After(inc.StateChangedAt.Add(ttl))
}
