package operations

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// A qualification stored as ReadyForApproval keeps that stored state after its
// wall-clock expiry until an observation records Expired. The read projection
// must say Expired at once, from the clock alone, without writing anything.
func TestQualificationViewReportsEffectiveExpiryWithoutWriting(t *testing.T) {
	mgr, st, fixture := newQualificationManager(t)
	ctx := context.Background()
	request := qualificationRequest("view")
	request.Requirements = RuntimeContractQualificationRequirements{MinSamples: 1, MinDuration: time.Minute}
	request.ExpiresAt = fixture.now.Add(20 * time.Minute)
	created, _, err := mgr.CreateRuntimeContractQualification(ctx, "alice", request)
	if err != nil {
		t.Fatal(err)
	}
	observing := mgr.ViewRuntimeContractQualification(created)
	if observing.EffectiveState != QualificationObserving || observing.Expired || observing.ExpiryPending || observing.ReadyForApproval || !observing.EvaluatedAt.Equal(fixture.now) {
		t.Fatalf("observing view = %+v", observing)
	}
	if observing.Requirements != (RuntimeContractQualificationRequirementsView{MinSamples: 1, MinDuration: "1m0s"}) {
		t.Fatalf("requirements view = %+v, want the human-readable duration", observing.Requirements)
	}
	fixture.now = fixture.now.Add(time.Minute)
	first := observe(t, mgr, created.ID, "v1", created.ResourceVersion)
	fixture.now = fixture.now.Add(time.Minute)
	ready := observe(t, mgr, created.ID, "v2", first.ResourceVersion)
	if ready.State != QualificationReadyForApproval {
		t.Fatalf("ready = %#v", ready)
	}
	view := mgr.ViewRuntimeContractQualification(ready)
	if !view.ReadyForApproval || view.EffectiveState != QualificationReadyForApproval || view.Expired || view.ExpiryPending || !strings.HasPrefix(view.Summary, "ready for approval since ") {
		t.Fatalf("ready view = %+v", view)
	}

	// One nanosecond before expiry the evidence is still current; at expiry
	// the projection flips, exactly where Observe would record Expired.
	fixture.now = request.ExpiresAt.Add(-time.Nanosecond)
	if view := mgr.ViewRuntimeContractQualification(ready); !view.ReadyForApproval || view.Expired {
		t.Fatalf("view just before expiry = %+v, want still ready", view)
	}
	fixture.now = request.ExpiresAt
	stale := mgr.ViewRuntimeContractQualification(ready)
	if stale.State != QualificationReadyForApproval || stale.EffectiveState != QualificationExpired || !stale.Expired || !stale.ExpiryPending || stale.ReadyForApproval {
		t.Fatalf("view at expiry = %+v, want stored ReadyForApproval with effective Expired", stale)
	}
	if !strings.Contains(stale.Summary, "expired at "+request.ExpiresAt.Format(time.RFC3339)) || !strings.Contains(stale.Summary, "stored as ReadyForApproval") || !strings.Contains(stale.Summary, "not ready for approval") {
		t.Fatalf("summary = %q, want an unambiguous expiry statement", stale.Summary)
	}
	// Projection is pure: the wrapped record and the store are untouched.
	if ready.State != QualificationReadyForApproval || ready.ExpiredAt != nil {
		t.Fatalf("projection mutated the record: %#v", ready)
	}
	stored, err := mgr.GetRuntimeContractQualification(ctx, created.ID)
	if err != nil || stored.State != QualificationReadyForApproval || stored.ResourceVersion != ready.ResourceVersion {
		t.Fatalf("stored after projection = (%#v, %v), want unchanged", stored, err)
	}
	if actions := auditActions(t, st, created.ID); actions != "create=Observing,observe=full,observe=full,ready=ReadyForApproval" {
		t.Fatalf("audit after projection = %q, want no new events", actions)
	}

	// The JSON shape keeps the stored state and adds the effective fields, with
	// the wire requirements shadowing the nanosecond payload representation.
	blob, err := json.Marshal(stale)
	if err != nil {
		t.Fatal(err)
	}
	var wire map[string]any
	if err := json.Unmarshal(blob, &wire); err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]any{
		"state": "ReadyForApproval", "effective_state": "Expired", "expired": true, "expiry_pending": true, "ready_for_approval": false,
		"evaluated_at": request.ExpiresAt.Format(time.RFC3339), "id": created.ID,
	} {
		if wire[key] != want {
			t.Errorf("%s = %v, want %v", key, wire[key], want)
		}
	}
	requirements, _ := wire["requirements"].(map[string]any)
	if requirements["min_duration"] != "1m0s" || requirements["min_samples"] != 1.0 {
		t.Fatalf("wire requirements = %v, want the human-readable duration", wire["requirements"])
	}

	// Once an observation persists Expired the projection agrees and no
	// longer reports a pending expiry.
	expired := observe(t, mgr, created.ID, "v3", ready.ResourceVersion)
	final := mgr.ViewRuntimeContractQualification(expired)
	if final.State != QualificationExpired || final.EffectiveState != QualificationExpired || !final.Expired || final.ExpiryPending || final.ReadyForApproval {
		t.Fatalf("recorded expiry view = %+v", final)
	}
}

func TestQualificationViewTerminalStatesAndPage(t *testing.T) {
	now := time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)
	expiresAt := now.Add(time.Hour)
	invalidated := &RuntimeContractQualification{ID: "rcq-1", State: QualificationInvalidated, ExpiresAt: expiresAt, InvalidationReason: "node \"gpu-a\" uid changed", Requirements: RuntimeContractQualificationRequirements{MinSamples: 3, MinDuration: 90 * time.Minute}}
	observing := &RuntimeContractQualification{ID: "rcq-2", State: QualificationObserving, ExpiresAt: expiresAt, Requirements: RuntimeContractQualificationRequirements{MinSamples: 3, MinDuration: 90 * time.Minute}}
	mgr := New(Options{Now: func() time.Time { return expiresAt.Add(time.Second) }})
	views := mgr.ViewRuntimeContractQualifications([]*RuntimeContractQualification{invalidated, observing})
	if len(views) != 2 {
		t.Fatalf("views = %d", len(views))
	}
	// Invalidated is final: the closed window is not reported as expiry.
	if views[0].EffectiveState != QualificationInvalidated || views[0].Expired || views[0].ExpiryPending || views[0].ReadyForApproval || !strings.HasPrefix(views[0].Summary, "invalidated: node") {
		t.Fatalf("invalidated view = %+v", views[0])
	}
	// An Observing qualification past its window is effectively Expired.
	if views[1].EffectiveState != QualificationExpired || !views[1].Expired || !views[1].ExpiryPending || views[1].ReadyForApproval || views[1].Requirements.MinDuration != "1h30m0s" {
		t.Fatalf("observing-past-expiry view = %+v", views[1])
	}
	if !views[0].EvaluatedAt.Equal(views[1].EvaluatedAt) {
		t.Fatalf("page evaluated at %s and %s, want one shared instant", views[0].EvaluatedAt, views[1].EvaluatedAt)
	}
	if NewRuntimeContractQualificationView(nil, now) != nil {
		t.Fatal("nil qualification must project to nil")
	}
}
