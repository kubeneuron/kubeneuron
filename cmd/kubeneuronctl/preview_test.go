package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/spf13/cobra"
)

func runPreview(t *testing.T, server string, args ...string) (string, error) {
	t.Helper()
	root := &cobra.Command{Use: "kubeneuronctl", SilenceUsage: true, SilenceErrors: true}
	root.PersistentFlags().String("server", server, "")
	root.PersistentFlags().String("token", "test-token", "")
	root.PersistentFlags().String("token-file", "", "")
	root.AddCommand(cmdPreview())
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetArgs(args)
	err := root.Execute()
	return out.String(), err
}

// previewResponse is the shape the controller returns for a candidate whose
// single profile is identical to the live profile: the After decision fails
// closed and the impact demands fresh post-deploy attestation.
const previewResponse = `{"id":"preview-1","resource_version":1,"candidate_id":"cand-1","candidate_digest":"sha256:cand","inventory_snapshot_id":"inventory-1","evaluator_version":"v1",` +
	`"runtime_contract_impact_version":"candidate-runtime-contract-impact/v1","runtime_contract_profile_change":"ProfileSetReplaced","created_at":"2026-09-05T12:00:00Z",` +
	`"newly_eligible":null,"newly_blocked":[{"node":"gpu-a",` +
	`"before":{"evaluator_version":"v1","state":"Eligible","reason_codes":[],"human_summary":"ok","evidence_refs":[],"allowed_actions":["reset-device"],"limits":{},"config_digest":"sha256:live","evaluated_at":"2026-09-05T12:00:00Z","expires_at":"2026-09-05T12:04:00Z"},` +
	`"after":{"evaluator_version":"v1","state":"Unknown","reason_codes":["EvidenceStale"],"human_summary":"Unknown: EvidenceStale","evidence_refs":null,"required_actions":["refresh_evidence"],"limits":{},"config_digest":"sha256:cand","evaluated_at":"2026-09-05T12:00:00Z","expires_at":"2026-09-05T12:00:00Z"},` +
	`"changed":true,"runtime_contract_impact":{"version":"candidate-runtime-contract-impact/v1","assessment":"PreDeployStatic","node":"gpu-a","vendor":"nvidia","profile_change":"ProfileSetReplaced","static_selection":"Exact",` +
	`"candidate_profile_name":"nvidia-a100","candidate_profile_uid":"profile-uid","candidate_profile_generation":1,"candidate_profile_digest":"sha256:aaaa","post_deploy_attestation":"FreshRequired","captured_report_usable_as_candidate_attestation":false,` +
	`"after_decision_evidence":"CandidateProfileWithoutReport","reasons":["CandidateProfilesPresent","CapturedReportPredatesCandidate"],"summary":"pre-deploy static assessment"}}],"changed_observed_only":null,"unchanged":null}`

func TestPreviewCommandPrintsRuntimeContractImpactJSON(t *testing.T) {
	type seenRequest struct {
		method, path, key string
		body              map[string]any
	}
	var seen []seenRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-token" {
			http.Error(w, "operator authentication failed", http.StatusUnauthorized)
			return
		}
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		seen = append(seen, seenRequest{method: r.Method, path: r.URL.Path, key: r.Header.Get("Idempotency-Key"), body: body})
		if r.Method != http.MethodPost || r.URL.Path != "/api/v1/candidates/cand-1/preview" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(previewResponse))
	}))
	defer server.Close()

	out, err := runPreview(t, server.URL, "preview", "cand-1", "--actor", "alice")
	if err != nil {
		t.Fatal(err)
	}
	if len(seen) != 1 || seen[0].method != http.MethodPost || seen[0].path != "/api/v1/candidates/cand-1/preview" || seen[0].key == "" || seen[0].body["actor"] != "alice" {
		t.Fatalf("requests = %#v, want one idempotent POST preview for cand-1 by alice", seen)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("preview output is not JSON: %v\n%s", err, out)
	}
	if got["runtime_contract_impact_version"] != "candidate-runtime-contract-impact/v1" || got["runtime_contract_profile_change"] != "ProfileSetReplaced" {
		t.Fatalf("preview header = %v / %v", got["runtime_contract_impact_version"], got["runtime_contract_profile_change"])
	}
	blocked, _ := got["newly_blocked"].([]any)
	if len(blocked) != 1 {
		t.Fatalf("newly_blocked = %v, want one node", got["newly_blocked"])
	}
	delta, _ := blocked[0].(map[string]any)
	after, _ := delta["after"].(map[string]any)
	if after["state"] == "Eligible" {
		t.Fatalf("CLI presents an undeployed candidate profile as Eligible: %v", after)
	}
	impact, _ := delta["runtime_contract_impact"].(map[string]any)
	for key, want := range map[string]any{
		"version": "candidate-runtime-contract-impact/v1", "assessment": "PreDeployStatic", "node": "gpu-a", "vendor": "nvidia",
		"profile_change": "ProfileSetReplaced", "static_selection": "Exact", "candidate_profile_name": "nvidia-a100",
		"candidate_profile_uid": "profile-uid", "candidate_profile_generation": 1.0, "post_deploy_attestation": "FreshRequired",
		"captured_report_usable_as_candidate_attestation": false, "after_decision_evidence": "CandidateProfileWithoutReport",
	} {
		if impact[key] != want {
			t.Errorf("runtime_contract_impact.%s = %v, want %v", key, impact[key], want)
		}
	}
}

func TestPreviewCommandHasNoDeployOrApproveSubcommands(t *testing.T) {
	root := newRootCommand()
	preview, _, err := root.Find([]string{"preview"})
	if err != nil || preview == nil || preview.Name() != "preview" {
		t.Fatalf("preview is not registered on the root command: %v", err)
	}
	if subs := preview.Commands(); len(subs) != 0 {
		names := make([]string, 0, len(subs))
		for _, sub := range subs {
			names = append(names, sub.Name())
		}
		t.Fatalf("preview subcommands = %v, want none: a preview never deploys, applies, promotes, or approves", names)
	}
	for _, flag := range []string{"apply", "deploy", "promote", "approve", "enable"} {
		if preview.Flags().Lookup(flag) != nil {
			t.Errorf("preview command exposes --%s", flag)
		}
	}
}
