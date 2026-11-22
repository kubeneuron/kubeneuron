// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kubeneuron/kubeneuron/internal/httpapi"
	"github.com/kubeneuron/kubeneuron/pkg/types"
)

type sourceReportAuth struct{}

func (sourceReportAuth) AuthenticateAgent(*http.Request) (httpapi.AgentPrincipal, error) {
	return httpapi.AgentPrincipal{NodeName: "node-a", NodeUID: "uid-a"}, nil
}

func TestAgentSourceReportToHealthView(t *testing.T) {
	c, _, _, _ := healthFixture(t)
	handler := httpapi.New(c).AgentRoutes(sourceReportAuth{})
	post := func(body string, want int) {
		t.Helper()
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, httptest.NewRequest("POST", types.AgentSignalSourcesPath, strings.NewReader(body)))
		if w.Code != want {
			t.Fatalf("status %d, want %d: %s", w.Code, want, w.Body.String())
		}
	}
	body := `[{"node":"node-a","source":"amd-kmsg","class":"pcie","available":false,"reason":"kernel-log-unavailable","read_at":"2026-11-18T00:00:00Z"}]`
	post(body, 204)
	post(strings.Replace(body, "node-a", "node-b", 1), 403)
	post(strings.Replace(body, "kernel-log-unavailable", "arbitrary text", 1), 400)
	neuron := `[{"node":"node-a","source":"aws-neuron-sysfs","class":"accelerator-fault","available":false,"reason":"permission-denied","read_at":"2026-11-18T00:00:00Z"}]`
	post(strings.Replace(neuron, "permission-denied", "kernel-log-unavailable", 1), 400)
	post(strings.Replace(neuron, "accelerator-fault", "pcie", 1), 400)
	post(neuron, 204)
	kernel := `[{"node":"node-a","source":"amd-kmsg","class":"pcie","available":true,"via":"journal","read_at":"2026-11-18T00:00:00Z"}]`
	post(strings.Replace(kernel, `"journal"`, `"dmesg"`, 1), 400)
	post(strings.Replace(kernel, `"available":true`, `"available":false,"reason":"kernel-log-unavailable"`, 1), 400)
	post(strings.Replace(neuron, `"available":false,"reason":"permission-denied"`, `"available":true,"via":"kmsg"`, 1), 400)
	post(kernel, 204)
	post(body, 204)
	row := strings.Trim(body, "[]")
	post("["+strings.Repeat(row+",", 64)+row+"]", 400)
	for _, node := range []string{"", "node-a"} {
		rows := healthRowsOf(c.AcceleratorHealth(t.Context(), node), agentHealthSource)
		if len(rows) != 1 || rows[0].Reason != "kernel-log-unavailable" || rows[0].State != "unknown" {
			t.Fatalf("rows: %+v", rows)
		}
	}
	c.health.reset()
	if len(c.AgentSignalSources("")) != 0 {
		t.Fatal("report survived leader reset")
	}
}
