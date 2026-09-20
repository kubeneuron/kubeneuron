package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

func runRuntimeContracts(t *testing.T, server string, args ...string) (string, error) {
	t.Helper()
	root := &cobra.Command{Use: "kubeneuronctl", SilenceUsage: true, SilenceErrors: true}
	root.PersistentFlags().String("server", server, "")
	root.PersistentFlags().String("token", "test-token", "")
	root.PersistentFlags().String("token-file", "", "")
	root.AddCommand(cmdRuntimeContracts())
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetArgs(args)
	err := root.Execute()
	return out.String(), err
}

func runtimeContractsServer(t *testing.T, seen *[]url.URL) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*seen = append(*seen, *r.URL)
		if r.Header.Get("Authorization") != "Bearer test-token" {
			http.Error(w, "operator authentication failed", http.StatusUnauthorized)
			return
		}
		vendor := r.URL.Query().Get("vendor")
		if vendor == "" {
			http.Error(w, "vendor is required", http.StatusBadRequest)
			return
		}
		if vendor != "nvidia" {
			http.Error(w, "vendor must be nvidia, amd, intel, or google", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/nodes/gpu-a/runtime-contract":
			_, _ = w.Write([]byte(`{"version":"runtime-contract-coverage/v1","config_digest":"sha256:live-config","evaluated_at":"2026-09-05T12:00:00Z","node_name":"gpu-a","node_uid":"node-uid","vendor":"nvidia","profile_name":"nvidia-a100","profile_uid":"profile-uid","profile_generation":1,"profile_digest":"sha256:aaaa","selection":"Exact","attestation":"FreshCompatible","verification_depth":"Full","summary":"full","agent_last_seen":"2026-09-05T11:59:00Z","report_observed_at":"2026-09-05T11:59:00Z"}`))
		case "/api/v1/runtime-contracts/coverage":
			if r.URL.Query().Get("cursor") == "page-2" {
				_, _ = w.Write([]byte(`{"items":[{"node_name":"gpu-c","node_uid":"","vendor":"nvidia","selection":"Uncovered","attestation":"NotApplicable","verification_depth":"Reduced","reasons":["ProfileNotFound"],"summary":"uncovered"}],"coverage_version":"runtime-contract-coverage/v1"}`))
				return
			}
			_, _ = w.Write([]byte(`{"items":[` +
				`{"node_name":"gpu-a","node_uid":"node-uid","vendor":"nvidia","profile_name":"nvidia-a100","selection":"Exact","attestation":"FreshCompatible","verification_depth":"Full","summary":"full"},` +
				`{"node_name":"gpu-b","node_uid":"node-uid","vendor":"nvidia","profile_name":"nvidia-a100","selection":"Exact","attestation":"Missing","verification_depth":"Reduced","reasons":["ReportMissing"],"summary":"missing"}` +
				`],"coverage_version":"runtime-contract-coverage/v1","next_cursor":"page-2"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
}

func TestRuntimeContractsCommandIsRegistered(t *testing.T) {
	root := newRootCommand()
	group, _, err := root.Find([]string{"runtime-contracts"})
	if err != nil || group == nil || group.Name() != "runtime-contracts" {
		t.Fatalf("runtime-contracts is not registered on the root command: %v", err)
	}
	coverage, _, err := root.Find([]string{"runtime-contracts", "coverage"})
	if err != nil || coverage == nil || coverage.Name() != "coverage" {
		t.Fatalf("runtime-contracts coverage is not registered: %v", err)
	}
	for _, flag := range []string{"vendor", "tenant", "cluster", "cursor", "limit"} {
		if coverage.Flags().Lookup(flag) == nil {
			t.Errorf("coverage command lacks --%s", flag)
		}
	}
	// The group is read-only: it must not grow a mutating subcommand.
	if subs := group.Commands(); len(subs) != 1 {
		names := make([]string, 0, len(subs))
		for _, sub := range subs {
			names = append(names, sub.Name())
		}
		t.Fatalf("runtime-contracts subcommands = %v, want only coverage", names)
	}
	// Two positional arguments are rejected before any request is built.
	if _, err := runRuntimeContracts(t, "http://127.0.0.1:0", "runtime-contracts", "coverage", "gpu-a", "gpu-b", "--vendor", "nvidia"); err == nil {
		t.Fatal("coverage accepted two node arguments")
	}
}

func TestRuntimeContractsCoverageNodePrintsJSON(t *testing.T) {
	var seen []url.URL
	server := runtimeContractsServer(t, &seen)
	defer server.Close()

	out, err := runRuntimeContracts(t, server.URL, "runtime-contracts", "coverage", "gpu-a", "--vendor", "nvidia")
	if err != nil {
		t.Fatal(err)
	}
	if len(seen) != 1 || seen[0].Path != "/api/v1/nodes/gpu-a/runtime-contract" || seen[0].Query().Get("vendor") != "nvidia" {
		t.Fatalf("requests = %v, want one node runtime-contract request with vendor=nvidia", seen)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("node output is not JSON: %v\n%s", err, out)
	}
	for key, want := range map[string]any{
		"version": "runtime-contract-coverage/v1", "node_name": "gpu-a", "node_uid": "node-uid", "vendor": "nvidia",
		"selection": "Exact", "attestation": "FreshCompatible", "verification_depth": "Full", "profile_generation": 1.0,
	} {
		if got[key] != want {
			t.Errorf("%s = %v, want %v", key, got[key], want)
		}
	}
}

func TestRuntimeContractsCoverageFleetPrintsTable(t *testing.T) {
	var seen []url.URL
	server := runtimeContractsServer(t, &seen)
	defer server.Close()

	out, err := runRuntimeContracts(t, server.URL, "runtime-contracts", "coverage", "--vendor", "nvidia", "--tenant", "team-a", "--cluster", "east", "--limit", "2")
	if err != nil {
		t.Fatal(err)
	}
	if len(seen) != 1 || seen[0].Path != "/api/v1/runtime-contracts/coverage" {
		t.Fatalf("requests = %v, want one fleet coverage request", seen)
	}
	query := seen[0].Query()
	if query.Get("vendor") != "nvidia" || query.Get("tenant") != "team-a" || query.Get("cluster") != "east" || query.Get("limit") != "2" {
		t.Fatalf("fleet query = %v", query)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) < 3 || !strings.HasPrefix(lines[0], "NODE") {
		t.Fatalf("fleet output = %q", out)
	}
	for _, want := range []string{"SELECTION", "ATTESTATION", "VERIFICATION", "PROFILE", "REASONS"} {
		if !strings.Contains(lines[0], want) {
			t.Errorf("header %q missing %q", lines[0], want)
		}
	}
	row := strings.Fields(lines[2])
	if strings.Join(row, " ") != "gpu-b Exact Missing Reduced nvidia-a100 ReportMissing" {
		t.Fatalf("gpu-b row = %v", row)
	}
	if !strings.Contains(out, "--cursor page-2") {
		t.Fatalf("fleet output does not tell the operator how to fetch the next page:\n%s", out)
	}

	seen = nil
	out, err = runRuntimeContracts(t, server.URL, "runtime-contracts", "coverage", "--vendor", "nvidia", "--cursor", "page-2")
	if err != nil {
		t.Fatal(err)
	}
	if seen[0].Query().Get("cursor") != "page-2" || !strings.Contains(out, "gpu-c") || strings.Contains(out, "--cursor") {
		t.Fatalf("second page = %q (query %v)", out, seen[0].Query())
	}
}

func TestRuntimeContractsCoverageVendorIsRequiredAndValidated(t *testing.T) {
	var seen []url.URL
	server := runtimeContractsServer(t, &seen)
	defer server.Close()

	if _, err := runRuntimeContracts(t, server.URL, "runtime-contracts", "coverage", "gpu-a"); err == nil || !strings.Contains(err.Error(), "--vendor is required") {
		t.Fatalf("missing vendor error = %v", err)
	}
	if len(seen) != 0 {
		t.Fatalf("missing vendor still sent %v", seen)
	}
	_, err := runRuntimeContracts(t, server.URL, "runtime-contracts", "coverage", "--vendor", "cuda")
	if err == nil || !strings.Contains(err.Error(), "400") || !strings.Contains(err.Error(), "vendor must be nvidia, amd, intel, or google") {
		t.Fatalf("invalid vendor error = %v, want the controller's validation message", err)
	}
}
