package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/kubeneuron/kubeneuron/pkg/types"
)

func runReport(t *testing.T, server string, args ...string) (string, error) {
	t.Helper()
	root := &cobra.Command{Use: "kubeneuronctl", SilenceUsage: true, SilenceErrors: true}
	root.PersistentFlags().String("server", server, "")
	root.PersistentFlags().String("token", "test-token", "")
	root.PersistentFlags().String("token-file", "", "")
	root.AddCommand(cmdReport())
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetArgs(args)
	err := root.Execute()
	return out.String(), err
}

func reportServer(t *testing.T, seen *string) *httptest.Server {
	t.Helper()
	to := time.Date(2026, 8, 5, 12, 0, 0, 0, time.UTC)
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/report/recovery" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		*seen = r.URL.Query().Get("window")
		_ = json.NewEncoder(w).Encode(types.RecoveryReport{
			From: to.Add(-720 * time.Hour), To: to,
			DegradedGPUHours: 47, RecoveredGPUHours: 34,
			Incidents: 4, Recovered: 2, RecoveredUnattended: 1,
			MTTR: types.RecoveryLatency{Samples: 2, MeanSeconds: 10800, P50Seconds: 7200, P90Seconds: 14400},
			Classes: []types.RecoveryClassReport{{
				Class: "fell-off-bus", Incidents: 1, DegradedGPUHours: 32, RecoveredGPUHours: 32,
				Recovered: 1, RecoveredUnattended: 1,
				MTTR: types.RecoveryLatency{Samples: 1, MeanSeconds: 14400, P50Seconds: 14400, P90Seconds: 14400},
			}},
			Open: []types.RecoveryOpenIncident{{
				ID: "ecc-dbe-n1-abc", Class: "ecc-dbe", Node: "node-8gpu", GPUUUID: "GPU-4",
				State: types.StateNeedsHuman, OpenedAt: to.Add(-12 * time.Hour), DegradedGPUHours: 12,
			}},
			AssumedSingleGPU: 1,
		})
	}))
}

func TestReportTextOutputStatesWhatItCounts(t *testing.T) {
	var window string
	server := reportServer(t, &window)
	defer server.Close()

	out, err := runReport(t, server.URL, "report", "--since", "30d")
	if err != nil {
		t.Fatal(err)
	}
	// The CLI spells days; the API takes a Go duration.
	if window != "720h0m0s" {
		t.Fatalf("requested window = %q, want 720h0m0s", window)
	}
	for _, want := range []string{
		"degraded GPU-hours", "47.0",
		"recovered GPU-hours", "34.0", "72.3%", // 34/47
		"incidents recovered", "2 of 4", "50.0%",
		"without a human", "1 of 2",
		"p50 2h0m0s", "p90 4h0m0s",
		"fell-off-bus", "ecc-dbe-n1-abc", "NEEDS_HUMAN",
		// The legend is load-bearing: a reader must never have to guess what
		// "recovered" counted — and, since an incident can now reach RESOLVED
		// without anything having been done to the fleet, what it did NOT
		// count is just as load-bearing.
		"recovered     = the incident reached RESOLVED *and* a remediation step executed",
		"nothing done  = reached RESOLVED with no remediation step ever executed",
		"unattended    = recovered without ever asking for an approval",
		"closed, nothing done",
		// The inventory undercount is disclosed rather than hidden.
		"were charged 1 GPU each",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("report output missing %q\n%s", want, out)
		}
	}
}

func TestReportJSONShape(t *testing.T) {
	var window string
	server := reportServer(t, &window)
	defer server.Close()

	out, err := runReport(t, server.URL, "report", "--json")
	if err != nil {
		t.Fatal(err)
	}
	if window != "168h0m0s" {
		t.Fatalf("default window = %q, want 168h0m0s (7d)", window)
	}

	// Decode into a map, not the typed struct: the JSON keys are the contract
	// a report pipeline parses, and a renamed field must fail here.
	var got map[string]any
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("report --json is not valid JSON: %v\n%s", err, out)
	}
	for key, want := range map[string]any{
		"degraded_gpu_hours":   47.0,
		"recovered_gpu_hours":  34.0,
		"incidents":            4.0,
		"recovered":            2.0,
		"recovered_unattended": 1.0,
		"assumed_single_gpu":   1.0,
	} {
		if got[key] != want {
			t.Errorf("%s = %v, want %v", key, got[key], want)
		}
	}
	for _, key := range []string{"from", "to", "mttr", "classes", "open_incidents"} {
		if _, ok := got[key]; !ok {
			t.Errorf("missing key %q in %v", key, got)
		}
	}
	mttr, _ := got["mttr"].(map[string]any)
	if mttr["p50_seconds"] != 7200.0 || mttr["p90_seconds"] != 14400.0 || mttr["samples"] != 2.0 {
		t.Errorf("mttr = %v", mttr)
	}
	classes, _ := got["classes"].([]any)
	if len(classes) != 1 {
		t.Fatalf("classes = %v", classes)
	}
	class, _ := classes[0].(map[string]any)
	if class["class"] != "fell-off-bus" || class["degraded_gpu_hours"] != 32.0 {
		t.Errorf("class row = %v", class)
	}
	open, _ := got["open_incidents"].([]any)
	if len(open) != 1 {
		t.Fatalf("open_incidents = %v", open)
	}
	openIncident, _ := open[0].(map[string]any)
	if openIncident["id"] != "ecc-dbe-n1-abc" || openIncident["state"] != "NEEDS_HUMAN" {
		t.Errorf("open incident = %v", openIncident)
	}
}

func TestParseWindow(t *testing.T) {
	tests := []struct {
		in      string
		want    time.Duration
		wantErr bool
	}{
		{in: "7d", want: 7 * 24 * time.Hour},
		{in: "30d", want: 30 * 24 * time.Hour},
		{in: "4w", want: 28 * 24 * time.Hour},
		{in: "24h", want: 24 * time.Hour},
		{in: "90m", want: 90 * time.Minute},
		{in: " 1d ", want: 24 * time.Hour},
		{in: "0d", wantErr: true},
		{in: "-3d", wantErr: true},
		{in: "0h", wantErr: true},
		{in: "", wantErr: true},
		{in: "last tuesday", wantErr: true},
	}
	for _, tc := range tests {
		got, err := parseWindow(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Errorf("parseWindow(%q) = %s, want an error", tc.in, got)
			}
			continue
		}
		if err != nil || got != tc.want {
			t.Errorf("parseWindow(%q) = %s, %v; want %s", tc.in, got, err, tc.want)
		}
	}
}
