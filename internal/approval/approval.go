// Package approval manages the human-approval workflow for risky playbook
// steps: parking incidents in AWAITING_APPROVAL, recording decisions from
// any channel (Slack, CLI, Web UI), and expiring stale requests.
package approval

import (
	"context"
	"fmt"
	"time"

	"github.com/kubeneuron/kubeneuron/internal/store"
	"github.com/kubeneuron/kubeneuron/pkg/types"
)

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

// Decide records a decision for an incident's pending step. The caller
// (HTTP API) has already authenticated the actor.
func (m *Manager) Decide(ctx context.Context, incidentID, stepName, actor, channel string, decision types.ApprovalDecision) error {
	inc, err := m.store.GetIncident(ctx, incidentID)
	if err != nil {
		return err
	}
	if inc.State != types.StateAwaitingApproval {
		return fmt.Errorf("incident %s is in state %s, not awaiting approval", incidentID, inc.State)
	}
	return m.store.RecordApproval(ctx, &types.Approval{
		IncidentID: incidentID,
		StepName:   stepName,
		Decision:   decision,
		Actor:      actor,
		Channel:    channel,
		At:         time.Now(),
	})
}

// Expired reports whether an incident has been awaiting approval longer
// than the TTL and must move to EXPIRED (and be re-notified). The TTL
// anchors to StateChangedAt — the moment the incident entered
// AWAITING_APPROVAL — not UpdatedAt, which every attached duplicate signal
// bumps and which would let a signal storm postpone expiry indefinitely.
func (m *Manager) Expired(inc *types.Incident) bool {
	return inc.State == types.StateAwaitingApproval && time.Since(inc.StateChangedAt) > m.ttl
}
