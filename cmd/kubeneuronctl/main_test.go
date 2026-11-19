// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/kubeneuron/kubeneuron/pkg/cloud"
	"github.com/kubeneuron/kubeneuron/pkg/types"
)

func runCommand(t *testing.T, server string, args ...string) (string, error) {
	t.Helper()
	root := &cobra.Command{Use: "kubeneuronctl", SilenceUsage: true, SilenceErrors: true}
	root.PersistentFlags().String("server", server, "")
	root.PersistentFlags().String("token", "test-token", "")
	root.PersistentFlags().String("token-file", "", "")
	root.AddCommand(cmdPools(), cmdStatus(), cmdNodes(), cmdIncidents(), cmdApprove(), cmdReject(), cmdResolve(), cmdRemediate(), cmdPause(), cmdResume())
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

func TestSuccessorLinesShowTheOverlap(t *testing.T) {
	ready := time.Date(2026, 11, 17, 10, 0, 0, 0, time.UTC)
	ended := ready.Add(25 * time.Minute)
	s := &types.SuccessorRecord{Node: "gpu-7", NodeUID: "uid", ReadyAt: &ready, EndedAt: &ended, Outcome: types.SuccessorReplaced}
	s.Operation, s.Pool, s.Phase = "inc-a0", "train", cloud.SuccessorCreated
	got := strings.Join(successorLines(s, ready.Add(time.Hour)), "\n")
	for _, want := range []string{"gpu-7 in pool train", "replaced", "two nodes ran for 25m0s"} {
		if !strings.Contains(got, want) {
			t.Fatalf("successor lines %q lack %q", got, want)
		}
	}
	if successorLines(nil, ready) != nil {
		t.Fatal("no successor, no lines")
	}
}

func TestIncidentShowPolicyHoldAndRecovery(t *testing.T) {
	const reason = "node group spans several zones; use a single-zone node group, or set RemoveFirst in the remediation policy that applies to these nodes"
	for _, tc := range []struct {
		name, action, result, status string
	}{
		{"awaiting recovery", "notified", "", "unavailable"},
		{"pool corrected", "recovery-available-again", "pool eligible", "eligible"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != "GET" || r.URL.Path != "/api/v1/incidents/held" {
					http.NotFound(w, r)
					return
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"incident": types.Incident{ID: "held", State: types.StateEvaluating}, "audit": []types.AuditEntry{{Action: "recovery-unavailable", Result: reason}, {Action: tc.action, Result: tc.result}}})
			}))
			defer server.Close()
			output, err := runCommand(t, server.URL, "incidents", "show", "held")
			if err != nil {
				t.Fatal(err)
			}
			header, _, _ := strings.Cut(output, "audit trail:")
			wantReason := reason
			if tc.status == "eligible" {
				wantReason = tc.result
			}
			if !strings.Contains(header, "recovery:  "+tc.status+" [policy]") || !strings.Contains(header, "reason:    "+wantReason+"\n") {
				t.Fatalf("incident details: %s", header)
			}
		})
	}
}

func TestResolveCloudSettlementHint(t *testing.T) {
	for _, golden := range []string{"cloud-settlement.golden", "cloud-settlement-remove-first.golden"} {
		t.Run(golden, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != "POST" || r.URL.Path != "/api/v1/incidents/inc-owner/resolve" {
					t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
				}
				incident := types.Incident{ID: "inc-owner", State: types.StateResolved, Target: types.Target{Node: "worker"}, CloudIdentity: &cloud.NodeIdentity{ProviderID: "aws:///zone/i-old"}, Successor: &types.SuccessorRecord{Outcome: types.SuccessorHandedOver, Successor: cloud.Successor{Operation: "op-1", Pool: "training", RequestedAt: time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)}}}
				if strings.Contains(golden, "remove-first") {
					s := incident.Successor
					incident.Successor = nil
					incident.CloudIdentity.ProviderID = "gce://project/zone/node"
					incident.RemoveFirst = &types.RemoveFirstRecord{Outcome: types.RemoveFirstNeedsOperator, RemoveFirstPlan: cloud.RemoveFirstPlan{Operation: s.Operation, Pool: s.Pool, PlannedAt: s.RequestedAt}}
				}
				_ = json.NewEncoder(w).Encode(incident)
			}))
			defer server.Close()
			got, err := runCommand(t, server.URL, "resolve", "inc-owner")
			if err != nil {
				t.Fatal(err)
			}
			want, err := os.ReadFile("testdata/" + golden)
			if err != nil {
				t.Fatal(err)
			}
			if got != string(want) {
				t.Fatalf("resolve output:\n%s", got)
			}
		})
	}
}

// incidents show names the node that replaced the target, from the
// resolution's evidence, and how it was found.
func TestReplacementLineNamesTheNewNode(t *testing.T) {
	resolve := func(correlation string) *types.AuditEntry {
		return &types.AuditEntry{FromState: types.StateVerifying, ToState: types.StateResolved, Actor: "system", Action: "resolve",
			Params: map[string]string{"infrastructure_evidence": "verified_replacement_node", "replacement_node": "n9",
				"replacement_node_uid": "uid-n9", "replacement_correlation": correlation}}
	}
	if got, want := replacementLine([]*types.AuditEntry{resolve("successor")}), "replaced:  node n9 (uid uid-n9), named by the cloud operation"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
	if got, want := replacementLine([]*types.AuditEntry{resolve("pool")}), "replaced:  node n9 (uid uid-n9), the only node that joined its pool after the removal"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
	reopened := &types.AuditEntry{FromState: types.StateResolved, ToState: types.StateOpen, Action: "reopen"}
	if got := replacementLine([]*types.AuditEntry{resolve("pool"), reopened}); got != "" {
		t.Fatalf("a later transition must hide the line, got %q", got)
	}
}
