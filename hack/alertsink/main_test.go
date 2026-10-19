package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestSinkRecordsAndReports(t *testing.T) {
	sink := NewSink(1 << 20)
	srv := httptest.NewServer(sink)
	defer srv.Close()

	// Nothing recorded yet, and the bookkeeping endpoints do not record.
	for _, p := range []string{"/healthz", "/requests"} {
		resp, err := http.Get(srv.URL + p)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s: status %d", p, resp.StatusCode)
		}
	}
	if n := len(sink.Records()); n != 0 {
		t.Fatalf("bookkeeping endpoints recorded %d requests", n)
	}

	body := `{"receiver":"human-default","status":"firing","alerts":[{"labels":{"alertname":"ShadowGpuMemoryBeyondRepair","kubeneuron_shadow":"true"}}]}`
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/api/v1/webhooks/alertmanager", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer fixture-token")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("webhook POST: status %d", resp.StatusCode)
	}
	// Non-JSON bodies are kept as text, never dropped.
	resp, err = http.Post(srv.URL+"/other", "text/plain", strings.NewReader("not json"))
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()

	resp, err = http.Get(srv.URL + "/requests")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var report struct {
		Count    int      `json:"count"`
		Requests []Record `json:"requests"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&report); err != nil {
		t.Fatal(err)
	}
	if report.Count != 2 || len(report.Requests) != 2 {
		t.Fatalf("want 2 recorded requests, got count=%d len=%d", report.Count, len(report.Requests))
	}
	first := report.Requests[0]
	if first.Method != http.MethodPost || first.Path != "/api/v1/webhooks/alertmanager" || first.Authorization != "Bearer fixture-token" {
		t.Fatalf("unexpected first record %+v", first)
	}
	var payload struct {
		Receiver string `json:"receiver"`
		Alerts   []struct {
			Labels map[string]string `json:"labels"`
		} `json:"alerts"`
	}
	if err := json.Unmarshal(first.Body, &payload); err != nil {
		t.Fatalf("body is not the JSON that was posted: %v", err)
	}
	if payload.Receiver != "human-default" || payload.Alerts[0].Labels["alertname"] != "ShadowGpuMemoryBeyondRepair" {
		t.Fatalf("payload not preserved: %+v", payload)
	}
	second := report.Requests[1]
	if string(second.Body) != "null" || second.BodyText != "not json" {
		t.Fatalf("non-JSON body not preserved as text: %+v", second)
	}
}

func TestPostJSONSendsJSONContentType(t *testing.T) {
	sink := NewSink(1 << 20)
	srv := httptest.NewServer(sink)
	defer srv.Close()

	status, out, err := PostJSON(srv.Client(), srv.URL+"/api/v2/alerts", []byte(`[{"labels":{"alertname":"GpuFixtureRoutingProbe"}}]`))
	if err != nil || status != http.StatusOK {
		t.Fatalf("PostJSON: status %d err %v", status, err)
	}
	if !strings.Contains(string(out), "recorded") {
		t.Fatalf("response body not returned: %q", out)
	}
	recs := sink.Records()
	if len(recs) != 1 || recs[0].ContentType != "application/json" || recs[0].Method != http.MethodPost {
		t.Fatalf("unexpected record %+v", recs)
	}
	if _, _, err := PostJSON(srv.Client(), srv.URL+"/x", []byte("not json")); err == nil {
		t.Fatal("PostJSON should refuse a non-JSON body")
	}
}
