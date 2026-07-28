// Package webhook posts incident notifications as JSON to any HTTP
// endpoint — the integration point for chat systems, ticketing, or custom
// automation that KubeNeuron does not know about. The payload is versioned
// and additive; consumers must ignore unknown fields.
package webhook

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

const postTimeout = 5 * time.Second

// Payload is the versioned wire format of one notification.
type Payload struct {
	Version int    `json:"version"`
	Kind    string `json:"kind"`
	// Step is set on approval requests: the playbook step awaiting a human.
	Step     string          `json:"step,omitempty"`
	Message  string          `json:"message,omitempty"`
	Incident *types.Incident `json:"incident"`
}

// Notifier posts each notification to URL. Token, when set, is sent as an
// Authorization bearer credential — pass both via files, never argv.
type Notifier struct {
	URL   string
	Token string
	// HTTPClient may be overridden in tests; nil uses a 5s-timeout client.
	HTTPClient *http.Client
}

var _ notify.Notifier = (*Notifier)(nil)

// New builds a webhook notifier.
func New(url, token string) *Notifier {
	return &Notifier{URL: url, Token: token}
}

// Notify implements notify.Notifier.
func (n *Notifier) Notify(ctx context.Context, ev notify.NotifyEvent) error {
	if ev.Incident == nil {
		return fmt.Errorf("webhook: notify event %q carries no incident", ev.Kind)
	}
	return n.post(ctx, Payload{
		Version: 1, Kind: string(ev.Kind), Message: ev.Message, Incident: ev.Incident,
	})
}

// RequestApproval implements notify.Notifier.
func (n *Notifier) RequestApproval(ctx context.Context, inc *types.Incident, stepName string) error {
	return n.post(ctx, Payload{
		Version: 1, Kind: "approval_required", Step: stepName,
		Message:  "approve with: kubeneuronctl approve " + inc.ID,
		Incident: inc,
	})
}

func (n *Notifier) post(ctx context.Context, payload Payload) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("webhook: encode payload: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, n.URL, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("webhook: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if n.Token != "" {
		req.Header.Set("Authorization", "Bearer "+n.Token)
	}
	client := n.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: postTimeout}
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("webhook: post: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("webhook: endpoint returned %s", resp.Status)
	}
	return nil
}
