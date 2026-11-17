// SPDX-License-Identifier: Apache-2.0

package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/kubeneuron/kubeneuron/pkg/types"
)

const vmalertPath = "/api/v1/webhooks/vmalert/api/v2/alerts"

func vmalertBody(t *testing.T, alerts ...map[string]any) string {
	t.Helper()
	data, err := json.Marshal(alerts)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func vmalertAlert(name, node string, endsAt time.Time, extra map[string]string) map[string]any {
	labels := map[string]string{"alertname": name, "node": node, "UUID": "GPU-1", "gpu": "0"}
	for k, v := range extra {
		labels[k] = v
	}
	return map[string]any{
		"labels":       labels,
		"annotations":  map[string]string{"summary": "s"},
		"startsAt":     time.Now().Add(-time.Minute).Format(time.RFC3339),
		"endsAt":       endsAt.Format(time.RFC3339),
		"generatorURL": "http://vmalert/x",
	}
}

func TestVMAlertRouteAuthenticates(t *testing.T) {
	backend := &registrationBackend{}
	s := New(backend)
	s.SetWebhookToken("hook")
	handler := s.Routes()
	body := vmalertBody(t, vmalertAlert("GpuTempCritical", "n1", time.Now().Add(time.Hour), nil))
	for _, token := range []string{"", "wrong"} {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, operatorRequest("POST", vmalertPath, token, body))
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("token %q: status %d, want 401", token, rec.Code)
		}
	}
	if len(backend.signals) != 0 {
		t.Fatalf("an unauthenticated post drove %d signals", len(backend.signals))
	}

	tokenless := New(backend)
	rec := httptest.NewRecorder()
	tokenless.Routes().ServeHTTP(rec, operatorRequest("POST", vmalertPath, "", body))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("tokenless controller: status %d, want 401 (fail closed)", rec.Code)
	}
}

func TestVMAlertRouteMapsLikeTheWebhook(t *testing.T) {
	backend := &registrationBackend{}
	s := New(backend)
	s.SetWebhookToken("hook")
	handler := s.Routes()
	body := vmalertBody(t,
		vmalertAlert("GpuTempCritical", "n1", time.Now().Add(time.Hour), map[string]string{
			"severity": "critical", "pod": "trainer-0", "namespace": "team-a", "container": "main", "pod_uid": "u-1",
		}),
		vmalertAlert("GpuPowerBrake", "n1", time.Now().Add(-time.Minute), nil), // resolved
		vmalertAlert("SomethingElse", "n1", time.Now().Add(time.Hour), nil),    // unknown
		vmalertAlert("GpuExporterDown", "n2", time.Time{}, nil),                // no endsAt: firing
	)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, operatorRequest("POST", vmalertPath, "hook", body))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d %s", rec.Code, rec.Body.String())
	}
	if len(backend.signals) != 2 {
		t.Fatalf("signals = %+v, want the firing thermal and exporter-down alerts", backend.signals)
	}
	thermal := backend.signals[0]
	if thermal.Class != types.ClassThermal || thermal.Severity != types.SeverityCritical ||
		thermal.Target.Node != "n1" || thermal.Target.GPUUUID != "GPU-1" || thermal.Source != types.SourceAlertmanager {
		t.Errorf("thermal signal = %+v", thermal)
	}
	// What the incident and its audit trail are built from carries no
	// workload identity, whatever labels the alert had.
	encoded, _ := json.Marshal(thermal)
	for _, value := range []string{"trainer-0", "team-a", "u-1", `"pod"`, `"namespace"`} {
		if strings.Contains(string(encoded), value) {
			t.Errorf("signal carries %s: %s", value, encoded)
		}
	}
	if backend.signals[1].Class != types.ClassExporterDown || backend.signals[1].Target.Node != "n2" {
		t.Errorf("exporter-down signal = %+v", backend.signals[1])
	}
}

func TestVMAlertRouteRejectsAWebhookBody(t *testing.T) {
	s := New(&registrationBackend{})
	s.SetWebhookToken("hook")
	rec := httptest.NewRecorder()
	s.Routes().ServeHTTP(rec, operatorRequest("POST", vmalertPath, "hook", `{"alerts":[]}`))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status %d, want 400 for a non-list body", rec.Code)
	}
}

// A body as vmalert v1.147.0 posted it to -notifier.url, captured from the
// managed rules against a store fed over OTLP; only endsAt is moved to the
// future so the alert is still firing.
func TestVMAlertRouteTakesAVMAlertBody(t *testing.T) {
	body := `[{"startsAt": "2026-11-17T23:25:00Z", "generatorURL": "http://eed20ea4d85d:8880/vmalert/alert?group_id=2175941831892498416&alert_id=4728057754478353751", "endsAt": "ENDS", "labels": {"UUID": "GPU-aaa", "alertgroup": "gpu-hardware-alerts", "alertname": "GpuTempCritical", "gpu": "0", "node": "n1", "severity": "critical"}, "annotations": {"description": "Sustained temperature above 88C. Investigate cooling.", "summary": "Critical GPU temperature on n1 GPU 0 (95C)"}}]`
	body = strings.Replace(body, "ENDS", time.Now().Add(3*time.Minute).UTC().Format(time.RFC3339Nano), 1)
	backend := &registrationBackend{}
	s := New(backend)
	s.SetWebhookToken("hook")
	rec := httptest.NewRecorder()
	s.Routes().ServeHTTP(rec, operatorRequest("POST", vmalertPath, "hook", body))
	if rec.Code != http.StatusOK || len(backend.signals) != 1 {
		t.Fatalf("status %d, signals %+v", rec.Code, backend.signals)
	}
	if sig := backend.signals[0]; sig.Class != types.ClassThermal || sig.Target.Node != "n1" || sig.Target.GPUUUID != "GPU-aaa" {
		t.Errorf("signal = %+v", sig)
	}
}

type classOperator struct {
	*fakeOperator
	class types.ProblemClass
}

func (o *classOperator) IncidentDetail(_ context.Context, id string) (*types.Incident, []*types.AuditEntry, error) {
	return &types.Incident{ID: id, Class: o.class}, nil, nil
}

// The incident detail says in one sentence why an observe-only class runs
// no step; the CLI and the panel print that field.
func TestIncidentDetailSaysObserveOnly(t *testing.T) {
	for class, want := range map[types.ProblemClass]string{
		types.ClassAgentDown:    "no data from the node",
		types.ClassExporterDown: "no GPU metrics from the node",
		types.ClassThermal:      "",
	} {
		handler := operatorServer(&classOperator{fakeOperator: &fakeOperator{}, class: class}, "secret")
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, operatorRequest("GET", "/api/v1/incidents/i1", "secret", ""))
		var detail struct {
			ObserveOnly string `json:"observe_only"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &detail); err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(detail.ObserveOnly, want) || (want == "") != (detail.ObserveOnly == "") {
			t.Errorf("%s: observe_only %q, want it to start with %q", class, detail.ObserveOnly, want)
		}
	}
}
