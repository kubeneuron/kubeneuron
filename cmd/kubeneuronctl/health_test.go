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
	for _, want := range []string{"(all)", "not-read-this-term", "gpu-1", "XidCriticalError", "2026-11-18T12:00:00Z", "Truncated at 2 rows"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in\n%s", want, out)
		}
	}
	var view types.AcceleratorHealthView
	if err := json.Unmarshal([]byte(run("health", "--json")), &view); err != nil || len(view.Rows) != 2 || query != "" {
		t.Fatalf("json %+v %v %q", view, err, query)
	}
}
