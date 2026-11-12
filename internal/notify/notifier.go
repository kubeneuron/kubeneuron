// SPDX-License-Identifier: LicenseRef-KubeNeuron

// Package notify delivers human-facing notifications: incident lifecycle
// updates and approval requests. Implementations: slack (Block Kit buttons),
// log (default/dev), generic webhook (Phase 2).
package notify

import (
	"context"
	"fmt"
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

// ApprovalContext is what a notifier may carry beside an approval request
// (v0.8 development): the short impact summary and the digest of the preview
// it came from, so the human's command can bind to what they were shown.
// Zero values mean "no preview".
type ApprovalContext struct {
	ImpactSummary string
	ImpactDigest  string
}

// ContextualNotifier is the optional extension a Notifier implements to
// receive the approval context. RequestApproval stays the required method;
// a notifier without this one gets the request without the summary.
type ContextualNotifier interface {
	Notifier
	RequestApprovalWithContext(ctx context.Context, inc *types.Incident, stepName string, ac ApprovalContext) error
}

// RequestApprovalWithContext delivers an approval request with its context
// through n when n supports it, and the plain request otherwise.
func RequestApprovalWithContext(ctx context.Context, n Notifier, inc *types.Incident, stepName string, ac ApprovalContext) error {
	if n == nil {
		return nil
	}
	if cn, ok := n.(ContextualNotifier); ok {
		return cn.RequestApprovalWithContext(ctx, inc, stepName, ac)
	}
	return n.RequestApproval(ctx, inc, stepName)
}

// Log is the default Notifier: structured log lines only. Useful for dev
// and as a always-on companion to Slack.
type Log struct {
	Logger *slog.Logger
}

var _ Notifier = (*Log)(nil)

// Notify implements Notifier.
//
// The nil guard matters MORE here than in its siblings, not less. Log is the
// only notifier that is not wrapped in Async — it runs on the reconcile and
// ingest goroutines — so a nil incident here is a panic in the control loop
// rather than an error returned to a queue. slack, webhook and pagerduty all
// check; this one dereferenced.
func (l *Log) Notify(ctx context.Context, ev NotifyEvent) error {
	if ev.Incident == nil {
		return fmt.Errorf("log: notify event %q carries no incident", ev.Kind)
	}
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
	if inc == nil {
		return fmt.Errorf("log: approval request for step %q carries no incident", stepName)
	}
	l.Logger.Warn("approval required",
		"incident", inc.ID,
		"node", inc.Target.Node,
		"class", inc.Class,
		"step", stepName,
		"round", inc.ApprovalEpoch,
		"hint", fmt.Sprintf("approve with: kubeneuronctl approve %s --round %d", inc.ID, inc.ApprovalEpoch),
	)
	return nil
}

// RequestApprovalWithContext implements ContextualNotifier: the summary is
// logged as its own field.
func (l *Log) RequestApprovalWithContext(ctx context.Context, inc *types.Incident, stepName string, ac ApprovalContext) error {
	if inc == nil {
		return fmt.Errorf("log: approval request for step %q carries no incident", stepName)
	}
	if ac.ImpactSummary == "" {
		return l.RequestApproval(ctx, inc, stepName)
	}
	l.Logger.Warn("approval required",
		"incident", inc.ID,
		"node", inc.Target.Node,
		"class", inc.Class,
		"step", stepName,
		"round", inc.ApprovalEpoch,
		"impact", ac.ImpactSummary,
		"impact_digest", ac.ImpactDigest,
		"hint", ApproveHint(inc, ac),
	)
	return nil
}

// ApproveHint is the command a notification suggests: the approve pinned to
// the round it displayed and, when the notification carried a preview, to
// that preview's digest — so the suggested command approves exactly what
// was shown, and a preview recomputed in between is refused (409), not
// silently approved.
func ApproveHint(inc *types.Incident, ac ApprovalContext) string {
	hint := fmt.Sprintf("approve with: kubeneuronctl approve %s --round %d", inc.ID, inc.ApprovalEpoch)
	if ac.ImpactDigest != "" {
		hint += " --impact-digest " + ac.ImpactDigest
	}
	return hint
}

// Multi fans out to several notifiers, returning the first error.
type Multi []Notifier

var _ Notifier = (Multi)(nil)
var _ ContextualNotifier = (Multi)(nil)

// RequestApprovalWithContext implements ContextualNotifier: each member gets
// the context when it can take it.
func (m Multi) RequestApprovalWithContext(ctx context.Context, inc *types.Incident, stepName string, ac ApprovalContext) error {
	var firstErr error
	for _, n := range m {
		if err := RequestApprovalWithContext(ctx, n, inc, stepName, ac); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

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
