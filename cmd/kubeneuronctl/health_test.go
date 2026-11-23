// SPDX-License-Identifier: Apache-2.0

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

func TestHealthCommand(t *testing.T) {
	var query string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != types.AcceleratorHealthPath || r.Header.Get("Authorization") != "Bearer test-token" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		query = r.URL.RawQuery
		_ = json.NewEncoder(w).Encode(types.AcceleratorHealthView{ProfileVersion: "v1", Truncated: true, Rows: []types.AcceleratorHealthRow{
			{Source: "dra", State: types.HealthRowNotReadThisTerm, Why: "not read since this controller became leader"},
			{Node: "gpu-1", Source: "kubernetes-node", Profile: "nvidia", Rule: "conditions", State: "fault", Reason: "XidCriticalError",
				ReadAt: time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)},
			{Node: "gpu-1", Source: "agent", Rule: "nvidia-kmsg", Class: types.ClassECCDBE, State: "unknown", Reason: "kernel-log-unavailable"},
			{Node: "gpu-1", Source: "agent", Rule: "nvidia-kmsg", Class: types.ClassXIDApp, State: "unknown", Reason: "kernel-log-unavailable"},
			{Node: "gpu-1", Source: "kubernetes-node", Profile: "nvidia", Rule: "conditions", State: "unknown", Why: "the node reports none of the conditions AcceleratedHardwareReady, GPUMissing, NVLinkDown"},
		}})
	}))
	defer server.Close()
	run := func(args ...string) string {
		t.Helper()
		root := &cobra.Command{Use: "kubeneuronctl", SilenceUsage: true, SilenceErrors: true}
		root.PersistentFlags().String("server", server.URL, "")
		root.PersistentFlags().String("token", "test-token", "")
		root.PersistentFlags().String("token-file", "", "")
		root.AddCommand(cmdHealth())
		var out bytes.Buffer
		root.SetOut(&out)
		root.SetArgs(args)
		if err := root.Execute(); err != nil {
			t.Fatal(err)
		}
		return out.String()
	}
	out := run("health", "--node", "gpu-1")
	if query != "node=gpu-1" {
		t.Fatalf("query %q", query)
	}
	// Rows that differ only by the class a fault opens must not read as duplicates.
	lines := strings.Split(out, "\n")
	if got := strings.Join(strings.Fields(lines[0])[3:6], " "); got != "RULE CLASS DEVICE" {
		t.Fatalf("header %q", lines[0])
	}
	for i, class := range []types.ProblemClass{types.ClassECCDBE, types.ClassXIDApp} {
		if fields := strings.Fields(lines[3+i]); fields[3] != "nvidia-kmsg" || fields[4] != string(class) {
			t.Fatalf("row %q, want class %s", lines[3+i], class)
		}
	}
	for _, want := range []string{"(all)", "not-read-this-term", "gpu-1", "XidCriticalError", "2026-11-18T12:00:00Z", "Truncated at 5 rows"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in\n%s", want, out)
		}
	}
	checkGolden(t, "health-conditions.golden", []byte(out))
	var view types.AcceleratorHealthView
	if err := json.Unmarshal([]byte(run("health", "--json")), &view); err != nil || len(view.Rows) != 5 || !strings.Contains(view.Rows[4].Why, "GPUMissing") || query != "" {
		t.Fatalf("json %+v %v %q", view, err, query)
	}
}
