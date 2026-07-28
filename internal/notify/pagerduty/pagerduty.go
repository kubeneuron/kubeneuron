// Package pagerduty delivers incident notifications through the PagerDuty
// Events API v2. Incidents map to PagerDuty alerts by a stable dedup key
// (the KubeNeuron incident ID): lifecycle updates re-trigger the same
// alert, needs-human raises severity to critical, and a resolved incident
// resolves the PagerDuty alert.
package pagerduty

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/kubeneuron/kubeneuron/internal/notify"
	"github.com/kubeneuron/kubeneuron/pkg/types"
)

const (
	// DefaultEndpoint is the public Events API v2 enqueue endpoint.
	DefaultEndpoint = "https://events.pagerduty.com/v2/enqueue"
	postTimeout     = 5 * time.Second
)

// Notifier posts Events API v2 events. RoutingKey identifies the PagerDuty
// service and is a credential: pass it via a file, never argv.
type Notifier struct {
	RoutingKey string
	// Endpoint may be overridden for tests or PagerDuty EU; empty uses
	// DefaultEndpoint.
	Endpoint string
	// HTTPClient may be overridden in tests; nil uses a 5s-timeout client.
	HTTPClient *http.Client
}

var _ notify.Notifier = (*Notifier)(nil)

// New builds a PagerDuty notifier.
func New(routingKey string) *Notifier {
	return &Notifier{RoutingKey: routingKey}
}

type event struct {
	RoutingKey  string   `json:"routing_key"`
	EventAction string   `json:"event_action"`
	DedupKey    string   `json:"dedup_key"`
	Payload     *payload `json:"payload,omitempty"`
}

type payload struct {
	Summary       string         `json:"summary"`
	Source        string         `json:"source"`
	Severity      string         `json:"severity"`
	Component     string         `json:"component,omitempty"`
	CustomDetails map[string]any `json:"custom_details,omitempty"`
}

var eventSeverity = map[notify.EventKind]string{
	notify.EventOpened:      "warning",
	notify.EventActionTaken: "info",
	notify.EventNeedsHuman:  "critical",
	notify.EventExpired:     "critical",
}

// Notify implements notify.Notifier.
func (n *Notifier) Notify(ctx context.Context, ev notify.NotifyEvent) error {
	if ev.Incident == nil {
		return fmt.Errorf("pagerduty: notify event %q carries no incident", ev.Kind)
	}
	if ev.Kind == notify.EventResolved {
		return n.post(ctx, event{
			RoutingKey: n.RoutingKey, EventAction: "resolve", DedupKey: ev.Incident.ID,
		})
	}
	severity := eventSeverity[ev.Kind]
	if severity == "" {
		severity = "info"
	}
	return n.post(ctx, event{
		RoutingKey: n.RoutingKey, EventAction: "trigger", DedupKey: ev.Incident.ID,
		Payload: n.eventPayload(ev.Incident, severity,
			fmt.Sprintf("KubeNeuron %s: %s on %s (%s)", ev.Kind, ev.Incident.Class, ev.Incident.Target.Node, ev.Message)),
	})
}

// RequestApproval implements notify.Notifier. An approval request pages:
// automation is deliberately stopped until a human decides.
func (n *Notifier) RequestApproval(ctx context.Context, inc *types.Incident, stepName string) error {
	return n.post(ctx, event{
		RoutingKey: n.RoutingKey, EventAction: "trigger", DedupKey: inc.ID,
		Payload: n.eventPayload(inc, "critical",
			fmt.Sprintf("KubeNeuron approval required: step %s on %s — kubeneuronctl approve %s", stepName, inc.Target.Node, inc.ID)),
	})
}

func (n *Notifier) eventPayload(inc *types.Incident, severity, summary string) *payload {
	if len(summary) > 1024 { // Events v2 summary limit
		summary = summary[:1024]
	}
	return &payload{
		Summary:  summary,
		Source:   inc.Target.Node,
		Severity: severity,
		Component: func() string {
			if inc.Target.GPUUUID != "" {
				return inc.Target.GPUUUID
			}
			return "gpu"
		}(),
		CustomDetails: map[string]any{
			"incident_id": inc.ID,
			"class":       string(inc.Class),
			"state":       string(inc.State),
			"playbook":    inc.Playbook,
			"dry_run":     inc.DryRun,
		},
	}
}

func (n *Notifier) post(ctx context.Context, ev event) error {
	body, err := json.Marshal(ev)
	if err != nil {
		return fmt.Errorf("pagerduty: encode event: %w", err)
	}
	endpoint := n.Endpoint
	if endpoint == "" {
		endpoint = DefaultEndpoint
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("pagerduty: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	client := n.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: postTimeout}
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("pagerduty: post: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
	if resp.StatusCode != http.StatusAccepted && (resp.StatusCode < 200 || resp.StatusCode > 299) {
		return fmt.Errorf("pagerduty: events API returned %s", resp.Status)
	}
	return nil
}
