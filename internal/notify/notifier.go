// SPDX-License-Identifier: Apache-2.0

// Package notify delivers human-facing notifications: incident lifecycle
// updates and approval requests. Implementations: slack (Block Kit buttons),
// log (default/dev), generic webhook (Phase 2).
package notify

import (
	"context"
	"fmt"
	"log/slog"
	"time"

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
	// EventNodeKept says a node keeps a KubeNeuron cordon or taint for an
	// incident this controller's store has no record of. It carries no
	// Incident, only Node and IncidentID: there is no incident to show, and a
	// link to one would lead nowhere.
	EventNodeKept EventKind = "node_kept"
)

// NotifyEvent is a notification about an incident.
type NotifyEvent struct {
	Kind     EventKind
	Incident *types.Incident
	// Node and IncidentID identify an EventNodeKept notice, which has no
	// Incident.
	Node       string
	IncidentID string
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

// ApprovalContext is what a notifier may carry beside an approval request:
// the short impact summary and the digest of the preview
// it came from, so the human's command can bind to what they were shown.
// Zero values mean "no preview".
type ApprovalContext struct {
	Deadline      time.Time
	ImpactSummary string
	ImpactDigest  string
	// Delivered, when set, is called with a channel's name once that channel
	// has accepted the request: by Async after a successful attempt, and for
	// a notifier that delivers synchronously when it returns no error. It may
	// be called from another goroutine, once per channel, or never. The log
	// is not a channel and never calls it.
	Delivered func(channel string)
	// Skip names channels that already accepted this round's request; Multi
	// does not send it to them again.
	Skip map[string]bool
	// Sent, when set, is called with a channel's name when the request is
	// handed to that channel: by Async when it queues it (not when the round
	// is already in its queue), and before a synchronous notifier is called.
	Sent func(channel string)
}

// deliveryReporter is implemented by a notifier that reports an approval
// request's delivery itself, later, through ApprovalContext.Delivered.
type deliveryReporter interface{ reportsDelivery() }

// NamedChannel is implemented by a notifier that is one named channel.
type NamedChannel interface{ ChannelName() string }

// unnamedChannel names a channel notifier that does not name itself.
const unnamedChannel = "notifier"

func channelName(n Notifier) string {
	if named, ok := n.(NamedChannel); ok && named.ChannelName() != "" {
		return named.ChannelName()
	}
	return unnamedChannel
}

// Channels lists the channels an approval request through n is sent to: none
// for the log alone, the name of each member but the log for Multi.
func Channels(n Notifier) []string {
	switch n := n.(type) {
	case nil, *Log:
		return nil
	case Multi:
		var out []string
		for _, member := range n {
			out = append(out, Channels(member)...)
		}
		return out
	default:
		return []string{channelName(n)}
	}
}

// ContextualNotifier is the optional extension a Notifier implements to
// receive the approval context. RequestApproval stays the required method;
// a notifier without this one gets the request without the summary.
type ContextualNotifier interface {
	Notifier
	RequestApprovalWithContext(ctx context.Context, inc *types.Incident, stepName string, ac ApprovalContext) error
}

// RequestApprovalWithContext delivers an approval request with its context
// through n when n supports it, and the plain request otherwise. A notifier
// that delivers synchronously has delivered when it returns no error, and
// ac.Delivered is called then; one that reports delivery itself calls it.
func RequestApprovalWithContext(ctx context.Context, n Notifier, inc *types.Incident, stepName string, ac ApprovalContext) error {
	if n == nil {
		return nil
	}
	_, later := n.(deliveryReporter)
	_, isLog := n.(*Log)
	if !later && !isLog && ac.Sent != nil {
		ac.Sent(channelName(n))
	}
	var err error
	if cn, ok := n.(ContextualNotifier); ok {
		err = cn.RequestApprovalWithContext(ctx, inc, stepName, ac)
	} else {
		err = n.RequestApproval(ctx, inc, stepName)
	}
	if err == nil && !later && !isLog && ac.Delivered != nil {
		ac.Delivered(channelName(n))
	}
	return err
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
	if ev.Kind == EventNodeKept {
		l.Logger.Warn("node notification",
			"kind", ev.Kind, "node", ev.Node, "incident", ev.IncidentID, "message", ev.Message)
		return nil
	}
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
	return l.RequestApprovalWithContext(ctx, inc, stepName, ApprovalContext{})
}

// RequestApprovalWithContext implements ContextualNotifier: the summary is
// logged as its own field.
func (l *Log) RequestApprovalWithContext(ctx context.Context, inc *types.Incident, stepName string, ac ApprovalContext) error {
	if inc == nil {
		return fmt.Errorf("log: approval request for step %q carries no incident", stepName)
	}
	approve, reject := ApprovalCommands(inc.ID, inc.ApprovalEpoch, ac.ImpactDigest)
	l.Logger.Warn("approval required",
		"incident", inc.ID,
		"node", inc.Target.Node,
		"class", inc.Class,
		"step", stepName,
		"round", inc.ApprovalEpoch,
		"impact", ac.ImpactSummary,
		"impact_digest", ac.ImpactDigest,
		"hint", approve, "reject", reject, "guidance", ac.Guidance(stepName),
	)
	return nil
}

// ApprovalCommands pins both decisions to the displayed round.
func ApprovalCommands(id string, round int, digest string) (approve, reject string) {
	approve = fmt.Sprintf("kubeneuronctl approve %s --round %d", id, round)
	if digest != "" {
		approve += " --impact-digest " + digest
	}
	return approve, fmt.Sprintf("kubeneuronctl reject %s --round %d", id, round)
}

func (ac ApprovalContext) Guidance(step string) string {
	msg := "Step " + step + " does not run until someone approves it."
	if !ac.Deadline.IsZero() {
		msg += " Without a decision by " + ac.Deadline.UTC().Format("2006-01-02 15:04:05 UTC") + ", the step will not run."
	}
	return msg + " Reject stops this repair; the incident then waits for the operator."
}

func ApproveHint(inc *types.Incident, ac ApprovalContext) string {
	approve, _ := ApprovalCommands(inc.ID, inc.ApprovalEpoch, ac.ImpactDigest)
	return "approve with: " + approve
}

// Multi fans out to several notifiers, returning the first error.
type Multi []Notifier

var _ Notifier = (Multi)(nil)
var _ ContextualNotifier = (Multi)(nil)

// RequestApprovalWithContext implements ContextualNotifier: each member gets
// the context when it can take it.
//
// Members named in ac.Skip are left out: they accepted this round already.
func (m Multi) RequestApprovalWithContext(ctx context.Context, inc *types.Incident, stepName string, ac ApprovalContext) error {
	var firstErr error
	for _, n := range m {
		if _, isLog := n.(*Log); !isLog && ac.Skip[channelName(n)] {
			continue
		}
		if err := RequestApprovalWithContext(ctx, n, inc, stepName, ac); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// reportsDelivery marks Multi as passing delivery reports on from its members.
func (m Multi) reportsDelivery() {}

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
