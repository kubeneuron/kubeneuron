// SPDX-License-Identifier: Apache-2.0

package slack

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/kubeneuron/kubeneuron/internal/notify"
	"github.com/kubeneuron/kubeneuron/pkg/types"
)

func capture(t *testing.T, status int) (*Notifier, *[]string) {
	t.Helper()
	var texts []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload struct {
			Text string `json:"text"`
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Errorf("bad payload: %v", err)
		}
		texts = append(texts, payload.Text)
		w.WriteHeader(status)
	}))
	t.Cleanup(server.Close)
	return New(server.URL), &texts
}

func incident() *types.Incident {
	return &types.Incident{
		ID: "ecc-dbe-n1-abcd", Class: types.ClassECCDBE, State: types.StateExecuting,
		Target: types.Target{Node: "n1", GPUUUID: "GPU-1"}, DryRun: true,
	}
}

func TestNotifyPostsFormattedMessage(t *testing.T) {
	n, texts := capture(t, http.StatusOK)
	err := n.Notify(context.Background(), notify.NotifyEvent{
		Kind: notify.EventOpened, Incident: incident(), Message: "incident opened",
	})
	if err != nil {
		t.Fatal(err)
	}
	got := (*texts)[0]
	for _, want := range []string{"ecc-dbe-n1-abcd", "n1", "ecc-dbe", "dry-run"} {
		if !strings.Contains(got, want) {
			t.Fatalf("message %q missing %q", got, want)
		}
	}
}

func TestRequestApprovalIncludesDecisionCommands(t *testing.T) {
	n, texts := capture(t, http.StatusOK)
	ac := notify.ApprovalContext{Deadline: time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC), ImpactDigest: "sha256:abc", ImpactSummary: "one workload"}
	if err := n.RequestApprovalWithContext(context.Background(), &types.Incident{ID: "inc-2", ApprovalEpoch: 3}, "reboot", ac); err != nil {
		t.Fatal(err)
	}
	body := (*texts)[0]
	for _, want := range []string{"kubeneuronctl approve inc-2 --round 3 --impact-digest sha256:abc", "kubeneuronctl reject inc-2 --round 3", "Step reboot does not run until someone approves it. Without a decision by 2026-11-24 12:00:00 UTC, the step will not run. Reject stops this repair; the incident then waits for the operator."} {
		if !strings.Contains(body, want) {
			t.Fatalf("missing %q in %q", want, body)
		}
	}
}

func TestWebhookErrorSurfaces(t *testing.T) {
	n, _ := capture(t, http.StatusForbidden)
	err := n.Notify(context.Background(), notify.NotifyEvent{
		Kind: notify.EventOpened, Incident: incident(),
	})
	if err == nil || !strings.Contains(err.Error(), "403") {
		t.Fatalf("err = %v, want webhook status error", err)
	}
}

// A node_kept notice has no incident behind it, so the message names the node
// and nothing renders an empty class, state or incident command.
func TestNodeKeptNoticeShowsOnlyTheNodeAndMessage(t *testing.T) {
	n, texts := capture(t, http.StatusOK)
	err := n.Notify(context.Background(), notify.NotifyEvent{
		Kind: notify.EventNodeKept, Node: "n1", IncidentID: "inc-old", Message: "Node n1 carries a KubeNeuron cordon",
	})
	if err != nil {
		t.Fatal(err)
	}
	want := ":no_entry: KubeNeuron: node `n1` needs an operator\nNode n1 carries a KubeNeuron cordon"
	if got := (*texts)[0]; got != want {
		t.Fatalf("text = %q, want %q", got, want)
	}
}
