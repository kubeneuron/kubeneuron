// SPDX-License-Identifier: Apache-2.0

package webhook

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

func TestNotifyPostsVersionedJSONWithBearer(t *testing.T) {
	var got Payload
	var auth, contentType string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		contentType = r.Header.Get("Content-Type")
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Errorf("decode: %v", err)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	n := New(server.URL, "hook-token")
	err := n.Notify(context.Background(), notify.NotifyEvent{
		Kind: notify.EventNeedsHuman, Message: "flap threshold",
		Incident: &types.Incident{ID: "inc-1", Target: types.Target{Node: "n1"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if auth != "Bearer hook-token" || contentType != "application/json" {
		t.Fatalf("headers = %q %q", auth, contentType)
	}
	if got.Version != 1 || got.Kind != "needs_human" || got.Incident == nil || got.Incident.ID != "inc-1" {
		t.Fatalf("payload = %+v", got)
	}
}

func TestApprovalRequestCarriesStep(t *testing.T) {
	var got Payload
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
	}))
	defer server.Close()

	n := New(server.URL, "")
	ac := notify.ApprovalContext{Deadline: time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC), ImpactDigest: "sha256:abc", ImpactSummary: "one workload"}
	if err := n.RequestApprovalWithContext(context.Background(), &types.Incident{ID: "inc-2", ApprovalEpoch: 3}, "reboot", ac); err != nil {
		t.Fatal(err)
	}
	if got.Kind != "approval_required" || got.Step != "reboot" {
		t.Fatalf("payload = %+v", got)
	}
	body := got.Message
	for _, want := range []string{"kubeneuronctl approve inc-2 --round 3 --impact-digest sha256:abc", "kubeneuronctl reject inc-2 --round 3", "Step reboot does not run until someone approves it. Without a decision by 2026-11-24 12:00:00 UTC, the step will not run. Reject stops this repair; the incident then waits for the operator."} {
		if !strings.Contains(body, want) {
			t.Fatalf("missing %q in %q", want, body)
		}
	}
}

func TestNon2xxIsAnError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "nope", http.StatusBadGateway)
	}))
	defer server.Close()
	n := New(server.URL, "")
	if err := n.Notify(context.Background(), notify.NotifyEvent{
		Kind: notify.EventOpened, Incident: &types.Incident{ID: "inc-1"},
	}); err == nil {
		t.Fatal("502 must surface as an error so the retry/dead-letter path engages")
	}
}
