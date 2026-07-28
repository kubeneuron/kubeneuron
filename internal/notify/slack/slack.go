// Package slack notifies a Slack channel through an incoming webhook.
// Messages are plain Block Kit sections; interactive Approve/Reject buttons
// (which need a Slack app, a signing secret, and the interaction receiver)
// remain future work — approval requests link to the kubeneuronctl command
// instead.
package slack

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

// Notifier posts to Slack via an incoming webhook URL. The URL embeds the
// destination channel and is a credential: pass it via a file, never argv.
type Notifier struct {
	WebhookURL string
	// HTTPClient may be overridden in tests; nil uses a 5s-timeout client.
	HTTPClient *http.Client
}

var _ notify.Notifier = (*Notifier)(nil)

// New builds a Slack notifier.
func New(webhookURL string) *Notifier {
	return &Notifier{WebhookURL: webhookURL}
}

var eventEmoji = map[notify.EventKind]string{
	notify.EventOpened:      ":rotating_light:",
	notify.EventActionTaken: ":gear:",
	notify.EventResolved:    ":white_check_mark:",
	notify.EventNeedsHuman:  ":no_entry:",
	notify.EventExpired:     ":hourglass:",
}

// Notify implements notify.Notifier.
func (n *Notifier) Notify(ctx context.Context, ev notify.NotifyEvent) error {
	if ev.Incident == nil {
		return fmt.Errorf("slack: notify event %q carries no incident", ev.Kind)
	}
	emoji := eventEmoji[ev.Kind]
	header := fmt.Sprintf("%s KubeNeuron %s: `%s`", emoji, ev.Kind, ev.Incident.ID)
	body := fmt.Sprintf("*node:* %s   *class:* %s   *state:* %s\n%s",
		ev.Incident.Target.Node, ev.Incident.Class, ev.Incident.State, ev.Message)
	if ev.Incident.DryRun {
		body += "\n_dry-run: no real action was taken_"
	}
	return n.post(ctx, header+"\n"+body)
}

// RequestApproval implements notify.Notifier.
func (n *Notifier) RequestApproval(ctx context.Context, inc *types.Incident, stepName string) error {
	if inc == nil {
		return fmt.Errorf("slack: approval request for step %q carries no incident", stepName)
	}
	text := fmt.Sprintf(
		":raised_hand: KubeNeuron approval required: `%s`\n*node:* %s   *class:* %s   *step:* %s\nDecide with:\n```kubeneuronctl approve %s\nkubeneuronctl reject %s```",
		inc.ID, inc.Target.Node, inc.Class, stepName, inc.ID, inc.ID)
	return n.post(ctx, text)
}

func (n *Notifier) post(ctx context.Context, text string) error {
	payload, err := json.Marshal(map[string]string{"text": text})
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, postTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, n.WebhookURL, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	client := n.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: postTimeout}
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("slack: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 300 {
		detail, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("slack: webhook returned %s: %s", resp.Status, detail)
	}
	return nil
}
