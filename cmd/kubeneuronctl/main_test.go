// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/kubeneuron/kubeneuron/internal/inventory"
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

	out, err = runCommand(t, server.URL, "approve", "ecc-dbe-n1-abc", "--actor", "alice")
	if err != nil {
		t.Fatal(err)
	}
	if approved.Actor != "alice" {
		t.Fatalf("approved actor = %q", approved.Actor)
	}
	if out != "approve: ecc-dbe-n1-abc\nApproval recorded. The controller will check the step again before it runs it.\n" {
		t.Fatalf("approve output = %q", out)
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

// Without a notification channel the round an approve binds to is read here;
// incidents impact is named only when the round has a preview to read.
func TestIncidentShowApprovalRound(t *testing.T) {
	const (
		withPreview = "approval:  round 3 (use --round 3 with approve, reject and incidents impact)\n"
		unavailable = "approval:  round 3 (use --round 3 with approve and reject; no impact preview for this round: WorkloadListingTimeout)\n"
		off         = "approval:  round 3 (use --round 3 with approve and reject; impact preview is off — set spec.safety.impactPreview.enabled to see what this step affects)\n"
	)
	for _, tc := range []struct {
		name    string
		state   types.IncidentState
		preview map[string]any
		want    string
	}{
		{"preview", types.StateAwaitingApproval, map[string]any{"impact_preview_digest": "sha256:abc"}, withPreview},
		{"unavailable preview", types.StateAwaitingApproval, map[string]any{"impact_preview_unavailable": "WorkloadListingTimeout"}, unavailable},
		{"previews off", types.StateAwaitingApproval, nil, off},
		{"not parked", types.StateExecuting, map[string]any{"impact_preview_digest": "sha256:abc"}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				body := map[string]any{"incident": types.Incident{ID: "parked", State: tc.state, ApprovalEpoch: 3}}
				body["approval_step"] = "drain"
				body["approval_deadline"] = "2026-11-23T19:13:00Z"
				for k, v := range tc.preview {
					body[k] = v
				}
				_ = json.NewEncoder(w).Encode(body)
			}))
			defer server.Close()
			out, err := runCommand(t, server.URL, "incidents", "show", "parked")
			if err != nil {
				t.Fatal(err)
			}
			got := ""
			for _, line := range strings.SplitAfter(out, "\n") {
				if strings.HasPrefix(line, "approval:") {
					got = line
				}
			}
			for _, phrase := range []string{"Step drain does not run", "2026-11-23 19:13:00 UTC", "kubeneuronctl approve parked --round 3", "kubeneuronctl reject parked --round 3"} {
				if strings.Contains(out, phrase) != (tc.state == types.StateAwaitingApproval) {
					t.Fatalf("unexpected presence of %q in %s", phrase, out)
				}
			}
			if got != tc.want {
				t.Fatalf("approval line %q, want %q:\n%s", got, tc.want, out)
			}
		})
	}
}

// The audit result carries the controller or provider hold reason.
func TestIncidentShowProviderHold(t *testing.T) {
	at := time.Date(2026, 11, 20, 15, 41, 0, 0, time.UTC)
	for _, tc := range []struct{ golden, action, reason string }{
		{"incident-hold-no-provider.golden", "provider-recovery-unavailable", "no cloud provider is configured, so this node cannot be replaced — set spec.cloud.provider (see docs/install.md)"},
		{"incident-hold-unmatched.golden", "provider-recovery-unavailable", "KubeNeuron could not match this node to a cloud instance, so it cannot be replaced — the incident has no record of which instance belonged to the node when it opened; check that the node has a provider ID and that the controller can read the Kubernetes node (see docs/cloud-aws.md), then resolve this incident with kubeneuronctl resolve inc-held --actor <who> so that the next fault opens a new one"},
		{"incident-hold-match-pending.golden", "provider-recovery-unavailable", "KubeNeuron could not match this node to a cloud instance yet, so it cannot be replaced — the cloud instance could not be identified when the incident opened; KubeNeuron checks again every minute"},
		{"incident-hold-node-recreated.golden", "provider-recovery-unavailable", "KubeNeuron could not match this node to a cloud instance, so it cannot be replaced — the node was re-created under the same name after the incident opened, so it is no longer the node the fault was seen on; resolve this incident with kubeneuronctl resolve inc-held --actor <who>"},
		{"incident-late-cloud-match.golden", "cloud-instance-matched", "cloud instance matched after the incident opened: the Kubernetes node is the same object (same UID) as when the incident opened; boot ID recorded at match time"},
		{"incident-hold-native-repair.golden", "provider-recovery-unavailable", "EKS node auto repair is enabled for node group gpu, so KubeNeuron does not replace its nodes. Leave repairs of this group to EKS, or turn EKS node auto repair off for this group: aws eks update-nodegroup-config --cluster-name training --nodegroup-name gpu --node-repair-config enabled=false. EKS then no longer repairs nodes in this group. KubeNeuron checks again every minute."},
		{"incident-hold-recent-change.golden", "provider-recovery-unavailable", "Node group gpu changed at 15:30 UTC (EKS update: NodeRepairEnabled). KubeNeuron waits 30 minutes after a change and checks again at 16:00 UTC."},
		{"incident-hold-read-denied.golden", "provider-recovery-unavailable", "KubeNeuron could not read node group gpu: eks:ListUpdates was denied. Allow eks:ListUpdates in the controller's IAM role. KubeNeuron retries every minute."},
	} {
		t.Run(tc.golden, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != "GET" || r.URL.Path != "/api/v1/incidents/inc-held" {
					http.NotFound(w, r)
					return
				}
				_ = json.NewEncoder(w).Encode(map[string]any{
					"incident": types.Incident{ID: "inc-held", State: types.StateEvaluating, Class: "xid-79", Target: types.Target{Node: "gpu-node-1"}, Playbook: "gpu-replace", OpenedAt: at.Add(-time.Minute)},
					"audit":    []types.AuditEntry{{Time: at, Actor: "system", Action: tc.action, Result: tc.reason}}})
			}))
			defer server.Close()
			got, err := runCommand(t, server.URL, "incidents", "show", "inc-held")
			if err != nil {
				t.Fatal(err)
			}
			want, err := os.ReadFile("testdata/" + tc.golden)
			if os.Getenv("UPDATE_GOLDEN") != "" {
				err = os.WriteFile("testdata/"+tc.golden, []byte(got), 0o644)
			} else if got != string(want) {
				t.Fatalf("incidents show:\n%s", got)
			}
			if err != nil {
				t.Fatal(err)
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
	if got, want := replacementLine([]*types.AuditEntry{resolve("resize-diff")}), "replaced:  node n9 (uid uid-n9), the only instance that joined the pool during the resize"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
	reopened := &types.AuditEntry{FromState: types.StateResolved, ToState: types.StateOpen, Action: "reopen"}
	if got := replacementLine([]*types.AuditEntry{resolve("pool"), reopened}); got != "" {
		t.Fatalf("a later transition must hide the line, got %q", got)
	}
}

// A remove-first replacement whose node the pool's members singled out.
func TestIncidentShowResizeDiffReplacement(t *testing.T) {
	at := time.Date(2026, 11, 20, 15, 41, 0, 0, time.UTC)
	const node = "gke-training-gpu-a-0003"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"incident": types.Incident{ID: "inc-resize", State: types.StateResolved, Class: "xid-79", Target: types.Target{Node: "gke-training-gpu-a-0001"}, Playbook: "gpu-replace", OpenedAt: at.Add(-time.Minute)},
			"audit": []types.AuditEntry{
				{Time: at, Actor: "system", Action: "remove-first-done", Result: "operation inc-resize-a0: done"},
				{Time: at.Add(time.Minute), FromState: types.StateVerifying, ToState: types.StateResolved, Actor: "system", Action: "resolve",
					Result: "Replaced by node " + node + ", the only instance that joined pool gpu during the resize. Verification used Kubernetes node readiness and advertised accelerator capacity.",
					Params: map[string]string{"infrastructure_evidence": "verified_replacement_node", "replacement_node": node, "replacement_node_uid": "uid-0003",
						"replacement_correlation": "resize-diff", "replacement_provider_id": "gce://ml-project/us-central1-a/" + node}},
			},
		})
	}))
	defer server.Close()
	got, err := runCommand(t, server.URL, "incidents", "show", "inc-resize")
	if err != nil {
		t.Fatal(err)
	}
	const file = "testdata/incident-resize-diff.golden"
	if os.Getenv("UPDATE_GOLDEN") != "" {
		if err := os.WriteFile(file, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(file)
	if err != nil || got != string(want) {
		t.Fatalf("incidents show (%v):\n%s", err, got)
	}
}

func TestIncidentShowCrossZoneSuccessor(t *testing.T) {
	at := time.Date(2026, 11, 20, 15, 41, 0, 0, time.UTC)
	const placement = "Successor worker-b is in zone zone-b; worker-a was in zone zone-a (policy allows any zone of the pool)."
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"incident": types.Incident{ID: "inc-zones", State: types.StateResolved, Class: "xid-79", Target: types.Target{Node: "worker-a"}, Playbook: "gpu-replace", OpenedAt: at.Add(-time.Minute)},
			"audit": []types.AuditEntry{
				{Time: at, Actor: "system", Action: "successor-ready", Result: "operation op: successor worker-b is Ready with its accelerators; two nodes run until worker-a is removed. " + placement},
				{Time: at.Add(time.Minute), Actor: "system", Action: "resolve", Result: "Replaced by node worker-b, named by the cloud operation. " + placement + " Verification used Kubernetes node readiness and advertised accelerator capacity."},
			},
		})
	}))
	defer server.Close()
	got, err := runCommand(t, server.URL, "incidents", "show", "inc-zones")
	if err != nil {
		t.Fatal(err)
	}
	const file = "testdata/incident-cross-zone.golden"
	if os.Getenv("UPDATE_GOLDEN") != "" {
		if err := os.WriteFile(file, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(file)
	if err != nil || got != string(want) {
		t.Fatalf("incidents show (%v):\n%s", err, got)
	}
}

func TestStatusContinuesAfterEmptyVersion(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/version":
			_, _ = io.WriteString(w, `{"version":""}`)
		case "/api/v1/incidents":
			_, _ = io.WriteString(w, "[]")
		case "/api/v1/pause":
			_, _ = io.WriteString(w, `{"paused":true}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	out, err := runCommand(t, server.URL, "status")
	if err != nil || !strings.HasPrefix(out, "controller: version not reported\nautomation paused: true\n") {
		t.Fatalf("status: %q, %v", out, err)
	}
}

func TestStatusInventoryGolden(t *testing.T) {
	for _, tc := range []struct {
		name        string
		gpus, nodes int
		mode        inventory.ActionMode
	}{
		{"disabled", 1, 2, inventory.ActionsDisabled}, {"enabled", 2, 1, inventory.ActionsEnabled}, {"legacy", 1, 1, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			now := time.Now()
			summary := inventory.Summary{ObservedAt: now, LastReportAt: now, ReportingNodes: tc.nodes, GPUs: tc.gpus, NodeLevelActions: tc.mode}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/api/v1/version":
					if tc.name == "legacy" {
						http.NotFound(w, r)
						return
					}
					_ = json.NewEncoder(w).Encode(map[string]string{"version": "v1.0.0-rc.3"})
				case "/api/v1/incidents":
					_ = json.NewEncoder(w).Encode([]types.Incident{})
				case "/api/v1/pause":
					_ = json.NewEncoder(w).Encode(map[string]bool{"paused": false})
				case "/api/v1/inventory-summary":
					_ = json.NewEncoder(w).Encode(summary.Data())
				default:
					http.NotFound(w, r)
				}
			}))
			defer server.Close()
			out, err := runCommand(t, server.URL, "status")
			if err != nil {
				t.Fatal(err)
			}
			checkGolden(t, "status-inventory-"+tc.name+".golden", []byte(out))
		})
	}
}

// A replacement whose pool size is held is a line of its own, from the
// incident record.
func TestStatusHeldResize(t *testing.T) {
	inc := types.Incident{ID: "inc-1", State: types.StateExecuting, Target: types.Target{Node: "n1", GPUIndex: -1},
		RemoveFirst: &types.RemoveFirstRecord{RemoveFirstPlan: cloud.RemoveFirstPlan{Pool: "gpu", Phase: cloud.RemoveFirstResizing, State: map[string]string{
			types.RemoveFirstHeldFor: "the operator (policy auto is in Approval mode)", types.RemoveFirstHeldCurrent: "3", types.RemoveFirstHeldPlanned: "4"}}}}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/incidents":
			_ = json.NewEncoder(w).Encode([]types.Incident{inc})
		case "/api/v1/pause":
			_ = json.NewEncoder(w).Encode(map[string]bool{"paused": false})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	out, err := runCommand(t, server.URL, "status")
	if err != nil {
		t.Fatal(err)
	}
	checkGolden(t, "status-held-resize.golden", []byte(out))
}

func TestShowMissingGPUIdentity(t *testing.T) {
	inc := types.Incident{ID: "inc-amd", OpenedAt: time.Date(2026, 11, 23, 0, 0, 0, 0, time.UTC), SignalSeen: 1, Vendor: "amd", Class: types.ClassDriverHang, State: types.StateNeedsHuman, Playbook: "gpu-reset", Target: types.Target{Node: "n1", GPUIndex: -1, PCIAddr: "0000:c3:00"}}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/incidents/inc-amd" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"incident": inc})
	}))
	defer server.Close()
	out, err := runCommand(t, server.URL, "incidents", "show", inc.ID)
	if err != nil {
		t.Fatal(err)
	}
	checkGolden(t, "incident-amd-no-uuid.golden", []byte(out))
}

func TestIncidentShowHandoverCause(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"incident": types.Incident{ID: "inc-cause", State: types.StateNeedsHuman}, "audit": []types.AuditEntry{{Action: "verify-evidence", Result: "operator guidance", Params: map[string]string{"cause": "agent heartbeat is stale", "private": "undisplayed value"}}}})
	}))
	defer server.Close()
	out, err := runCommand(t, server.URL, "incidents", "show", "inc-cause")
	if err != nil || !strings.Contains(out, "operator guidance\n  cause: agent heartbeat is stale\n") || strings.Contains(out, "undisplayed value") {
		t.Fatalf("show: %v\n%s", err, out)
	}
}

func TestIncidentShowHistoricalHandover(t *testing.T) {
	const old = "parked step no longer exists"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"incident": types.Incident{ID: "old", State: types.StateNeedsHuman}, "audit": []types.AuditEntry{{Action: "quarantine", Result: old}}})
	}))
	defer server.Close()
	out, err := runCommand(t, server.URL, "incidents", "show", "old")
	if err != nil || !strings.Contains(out, old) || strings.Contains(out, "cause:") {
		t.Fatalf("show: %v\n%s", err, out)
	}
}
