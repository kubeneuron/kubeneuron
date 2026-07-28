package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/kubeneuron/kubeneuron/pkg/types"
)

func runCommand(t *testing.T, server string, args ...string) (string, error) {
	t.Helper()
	root := &cobra.Command{Use: "kubeneuronctl", SilenceUsage: true, SilenceErrors: true}
	root.PersistentFlags().String("server", server, "")
	root.PersistentFlags().String("token", "test-token", "")
	root.PersistentFlags().String("token-file", "", "")
	root.AddCommand(cmdStatus(), cmdNodes(), cmdIncidents(), cmdApprove(), cmdReject(), cmdResolve(), cmdRemediate(), cmdPause(), cmdResume())
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetArgs(args)
	err := root.Execute()
	return out.String(), err
}

func TestIncidentsListAndApprove(t *testing.T) {
	var approved struct {
		Actor string `json:"actor"`
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-token" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch {
		case r.Method == "GET" && r.URL.Path == "/api/v1/incidents":
			_ = json.NewEncoder(w).Encode([]*types.Incident{{
				ID: "ecc-dbe-n1-abc", State: types.StateAwaitingApproval,
				Class: types.ClassECCDBE, Target: types.Target{Node: "n1", GPUUUID: "GPU-1"},
				Playbook: "drain-and-reset", SignalSeen: 2,
			}})
		case r.Method == "POST" && r.URL.Path == "/api/v1/incidents/ecc-dbe-n1-abc/approve":
			_ = json.NewDecoder(r.Body).Decode(&approved)
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	out, err := runCommand(t, server.URL, "incidents", "--state", "awaiting_approval")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "ecc-dbe-n1-abc") || !strings.Contains(out, "drain-and-reset") {
		t.Fatalf("incidents output = %q", out)
	}

	if _, err := runCommand(t, server.URL, "approve", "ecc-dbe-n1-abc", "--actor", "alice"); err != nil {
		t.Fatal(err)
	}
	if approved.Actor != "alice" {
		t.Fatalf("approved actor = %q", approved.Actor)
	}
}

func TestUnauthorizedGivesActionableError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "operator authentication failed", http.StatusUnauthorized)
	}))
	defer server.Close()

	_, err := runCommand(t, server.URL, "nodes")
	if err == nil || !strings.Contains(err.Error(), "KUBENEURONCTL_TOKEN") {
		t.Fatalf("err = %v, want hint about token configuration", err)
	}
}
