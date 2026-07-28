package pagerduty

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/kubeneuron/kubeneuron/internal/notify"
	"github.com/kubeneuron/kubeneuron/pkg/types"
)

func capture(t *testing.T) (*Notifier, *event, func()) {
	t.Helper()
	got := &event{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(got); err != nil {
			t.Errorf("decode: %v", err)
		}
		w.WriteHeader(http.StatusAccepted)
	}))
	n := New("rk-123")
	n.Endpoint = server.URL
	return n, got, server.Close
}

func TestLifecycleMapsToTriggerWithStableDedupKey(t *testing.T) {
	n, got, done := capture(t)
	defer done()
	err := n.Notify(context.Background(), notify.NotifyEvent{
		Kind: notify.EventNeedsHuman, Message: "quarantined",
		Incident: &types.Incident{
			ID: "inc-1", Class: types.ClassECCDBE, State: types.StateNeedsHuman,
			Target: types.Target{Node: "n1", GPUUUID: "GPU-abc"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.RoutingKey != "rk-123" || got.EventAction != "trigger" || got.DedupKey != "inc-1" {
		t.Fatalf("event = %+v", got)
	}
	if got.Payload == nil || got.Payload.Severity != "critical" || got.Payload.Source != "n1" || got.Payload.Component != "GPU-abc" {
		t.Fatalf("payload = %+v", got.Payload)
	}
}

func TestResolvedResolvesTheSameAlert(t *testing.T) {
	n, got, done := capture(t)
	defer done()
	err := n.Notify(context.Background(), notify.NotifyEvent{
		Kind:     notify.EventResolved,
		Incident: &types.Incident{ID: "inc-1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.EventAction != "resolve" || got.DedupKey != "inc-1" || got.Payload != nil {
		t.Fatalf("event = %+v", got)
	}
}

func TestApprovalRequestPagesCritical(t *testing.T) {
	n, got, done := capture(t)
	defer done()
	if err := n.RequestApproval(context.Background(), &types.Incident{
		ID: "inc-2", Target: types.Target{Node: "n2"},
	}, "reboot"); err != nil {
		t.Fatal(err)
	}
	if got.EventAction != "trigger" || got.Payload == nil || got.Payload.Severity != "critical" {
		t.Fatalf("event = %+v", got)
	}
}

func TestRejectionSurfacesAsError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"status":"invalid event"}`, http.StatusBadRequest)
	}))
	defer server.Close()
	n := New("rk-123")
	n.Endpoint = server.URL
	if err := n.Notify(context.Background(), notify.NotifyEvent{
		Kind: notify.EventOpened, Incident: &types.Incident{ID: "inc-1"},
	}); err == nil {
		t.Fatal("400 must surface as an error so the retry/dead-letter path engages")
	}
}
