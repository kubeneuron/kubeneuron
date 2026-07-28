// Package notify delivers human-facing notifications: incident lifecycle
// updates and approval requests. Implementations: slack (Block Kit buttons),
// log (default/dev), generic webhook (Phase 2).
package notify

import (
	"context"
	"log/slog"

	"github.com/kubeneuron/kubeneuron/pkg/types"
)

// EventKind is what happened to the incident.
type EventKind string

const (
	EventOpened      EventKind = "opened"
	EventActionTaken EventKind = "action_taken"
	EventResolved    EventKind = "resolved"
	EventNeedsHuman  EventKind = "needs_human"
	EventExpired     EventKind = "expired"
)

// NotifyEvent is a notification about an incident.
type NotifyEvent struct {
	Kind     EventKind
	Incident *types.Incident
	// Message is a human-readable one-liner (action, result, denial reason).
	Message string
}

// Notifier delivers notifications and approval requests.
type Notifier interface {
	// Notify sends an informational update.
	Notify(ctx context.Context, ev NotifyEvent) error
	// RequestApproval asks a human to approve or reject a pending step. The
	// decision comes back through the approval API, not through this call.
	RequestApproval(ctx context.Context, inc *types.Incident, stepName string) error
}

// Log is the default Notifier: structured log lines only. Useful for dev
// and as a always-on companion to Slack.
type Log struct {
	Logger *slog.Logger
}

var _ Notifier = (*Log)(nil)

// Notify implements Notifier.
func (l *Log) Notify(ctx context.Context, ev NotifyEvent) error {
	l.Logger.Info("incident notification",
		"kind", ev.Kind,
		"incident", ev.Incident.ID,
		"node", ev.Incident.Target.Node,
		"class", ev.Incident.Class,
		"state", ev.Incident.State,
		"message", ev.Message,
	)
	return nil
}

// RequestApproval implements Notifier.
func (l *Log) RequestApproval(ctx context.Context, inc *types.Incident, stepName string) error {
	l.Logger.Warn("approval required",
		"incident", inc.ID,
		"node", inc.Target.Node,
		"class", inc.Class,
		"step", stepName,
		"hint", "approve with: kubeneuronctl approve "+inc.ID,
	)
	return nil
}

// Multi fans out to several notifiers, returning the first error.
type Multi []Notifier

var _ Notifier = (Multi)(nil)

// Notify implements Notifier.
func (m Multi) Notify(ctx context.Context, ev NotifyEvent) error {
	var firstErr error
	for _, n := range m {
		if err := n.Notify(ctx, ev); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// RequestApproval implements Notifier.
func (m Multi) RequestApproval(ctx context.Context, inc *types.Incident, stepName string) error {
	var firstErr error
	for _, n := range m {
		if err := n.RequestApproval(ctx, inc, stepName); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}
