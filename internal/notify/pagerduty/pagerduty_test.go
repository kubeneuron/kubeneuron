// SPDX-License-Identifier: Apache-2.0

package pagerduty

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

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
	ac := notify.ApprovalContext{Deadline: time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC), ImpactDigest: "sha256:abc", ImpactSummary: "one workload"}
	if err := n.RequestApprovalWithContext(context.Background(), &types.Incident{ID: "inc-2", ApprovalEpoch: 3}, "reboot", ac); err != nil {
		t.Fatal(err)
	}
	if got.EventAction != "trigger" || got.Payload == nil || got.Payload.Severity != "critical" {
		t.Fatalf("event = %+v", got)
	}
	body := fmt.Sprint(got.Payload.CustomDetails)
	for _, want := range []string{"kubeneuronctl approve inc-2 --round 3 --impact-digest sha256:abc", "kubeneuronctl reject inc-2 --round 3", "Step reboot does not run until someone approves it. Without a decision by 2026-11-24 12:00:00 UTC, the step will not run. Reject stops this repair; the incident then waits for the operator."} {
		if !strings.Contains(body, want) {
			t.Fatalf("missing %q in %q", want, body)
		}
	}
}

func TestApprovalRequestWithoutPreviewCarriesBothCommands(t *testing.T) {
	n, got, done := capture(t)
	defer done()
	if err := n.RequestApprovalWithContext(context.Background(), &types.Incident{ID: "inc-2", ApprovalEpoch: 3}, "reboot", notify.ApprovalContext{}); err != nil {
		t.Fatal(err)
	}
	d := got.Payload.CustomDetails
	if d["approve"] != "kubeneuronctl approve inc-2 --round 3" || d["reject"] != "kubeneuronctl reject inc-2 --round 3" {
		t.Fatalf("approve = %q, reject = %q", d["approve"], d["reject"])
	}
	if _, ok := d["impact_digest"]; ok {
		t.Fatalf("impact_digest without a preview: %v", d)
	}
}

// The summary is cut at the Events v2 limit; the commands in custom details
// are not, however long the incident ID.
func TestApprovalCommandsSurviveTheSummaryLimit(t *testing.T) {
	n, got, done := capture(t)
	defer done()
	id := "inc-" + strings.Repeat("x", 1100)
	ac := notify.ApprovalContext{ImpactDigest: "sha256:abc", ImpactSummary: "one workload"}
	if err := n.RequestApprovalWithContext(context.Background(), &types.Incident{ID: id, ApprovalEpoch: 3, Target: types.Target{Node: "nöde-1"}}, "reboot", ac); err != nil {
		t.Fatal(err)
	}
	if s := got.Payload.Summary; len(s) > 1024 || !utf8.ValidString(s) {
		t.Fatalf("summary is %d bytes, valid UTF-8 %v", len(s), utf8.ValidString(s))
	}
	d := got.Payload.CustomDetails
	if d["approve"] != "kubeneuronctl approve "+id+" --round 3 --impact-digest sha256:abc" || d["reject"] != "kubeneuronctl reject "+id+" --round 3" {
		t.Fatalf("commands cut: approve %d bytes, reject %d bytes", len(fmt.Sprint(d["approve"])), len(fmt.Sprint(d["reject"])))
	}
}

// A cut that would land inside a multi-byte character backs off to its start.
func TestSummaryIsCutOnACharacterBoundary(t *testing.T) {
	n := New("rk")
	summary := strings.Repeat("a", 1023) + "—tail"
	p := n.eventPayload(&types.Incident{}, "info", summary)
	if p.Summary != strings.Repeat("a", 1023) {
		t.Fatalf("summary ends %q", p.Summary[len(p.Summary)-4:])
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

// A node_kept notice pages with the node as its source and no incident fields
// it does not have.
func TestNodeKeptNoticePagesWithoutAnIncident(t *testing.T) {
	n, got, done := capture(t)
	defer done()
	err := n.Notify(context.Background(), notify.NotifyEvent{
		Kind: notify.EventNodeKept, Node: "n1", IncidentID: "inc-old", Message: "Node n1 carries a KubeNeuron cordon",
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.EventAction != "trigger" || got.DedupKey != "node-kept/n1/inc-old" || got.Payload == nil {
		t.Fatalf("event = %+v", got)
	}
	p := got.Payload
	if p.Summary != "KubeNeuron: node n1 needs an operator: Node n1 carries a KubeNeuron cordon" ||
		p.Source != "n1" || p.Severity != "critical" || p.Component != "" {
		t.Fatalf("payload = %+v", p)
	}
	if len(p.CustomDetails) != 1 || p.CustomDetails["incident_id"] != "inc-old" {
		t.Fatalf("custom details = %v, want only the incident ID", p.CustomDetails)
	}
}
