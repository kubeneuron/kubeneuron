package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type topologyRequest struct {
	method, path, authorization string
}

func topologyServer(t *testing.T, seen *[]topologyRequest) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*seen = append(*seen, topologyRequest{method: r.Method, path: r.URL.EscapedPath(), authorization: r.Header.Get("Authorization")})
		if r.URL.Path != "/api/v1/nodes/gpu-a/topology" {
			http.Error(w, "resource not found", http.StatusNotFound)
			return
		}
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"version":"topology-classification/v1","node":"gpu-a","node_uid":"uid-a","topology":"time-sliced",` +
			`"device_actions":"refused","reasons":["TimeSliced"],"attribution":"device-wide",` +
			`"evidence":{"resource_source":"node","whole_devices":4,"mig_instances":0,"shared_replicas":0,"gpu_replicas_label":"4","report_physical_devices":1,"report_partitions":0,"report_fresh":true},` +
			`"dra":{"state":"not-observed","reason":"api-unavailable","resource_slices":0,"devices":0,"pods_inspected":false,"claim_pods":0,"direct_claims":0,"template_claims":0},` +
			`"summary":"topology=time-sliced device_actions=refused attribution=device-wide reasons=TimeSliced",` +
			`"observed_at":"2026-10-21T12:00:00Z","inputs_digest":"sha256:0123"}`))
	}))
}

// TestNodesTopologyCommandIsAReadOnlyGet pins the CLI surface: one GET on the
// node's topology route with the operator token, a table that names the
// topology, the verdict and the reasons, and --json for the full document.
func TestNodesTopologyCommandIsAReadOnlyGet(t *testing.T) {
	var seen []topologyRequest
	server := topologyServer(t, &seen)
	defer server.Close()

	out, err := runCommand(t, server.URL, "nodes", "topology", "gpu-a")
	if err != nil {
		t.Fatalf("nodes topology: %v", err)
	}
	if len(seen) != 1 || seen[0].method != http.MethodGet || seen[0].path != "/api/v1/nodes/gpu-a/topology" || seen[0].authorization != "Bearer test-token" {
		t.Fatalf("requests = %+v, want one authenticated GET on the topology route", seen)
	}
	for _, want := range []string{"TOPOLOGY", "time-sliced", "DEVICE ACTIONS", "refused", "REASONS", "TimeSliced", "ATTRIBUTION", "device-wide",
		"DRA", "not-observed (api-unavailable)", "INPUTS DIGEST", "sha256:0123", "VERSION", "topology-classification/v1"} {
		if !strings.Contains(out, want) {
			t.Errorf("table output lacks %q:\n%s", want, out)
		}
	}

	seen = nil
	out, err = runCommand(t, server.URL, "nodes", "topology", "gpu-a", "--json")
	if err != nil {
		t.Fatalf("nodes topology --json: %v", err)
	}
	var doc map[string]any
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("--json output is not JSON: %v\n%s", err, out)
	}
	if doc["version"] != "topology-classification/v1" || doc["topology"] != "time-sliced" || doc["device_actions"] != "refused" {
		t.Fatalf("--json document = %v", doc)
	}
	if len(seen) != 1 || seen[0].method != http.MethodGet {
		t.Fatalf("--json requests = %+v, want one GET", seen)
	}

	// The node argument is required and single; nothing is sent otherwise.
	seen = nil
	if _, err := runCommand(t, server.URL, "nodes", "topology"); err == nil {
		t.Fatal("topology without a node was accepted")
	}
	if _, err := runCommand(t, server.URL, "nodes", "topology", "gpu-a", "gpu-b"); err == nil {
		t.Fatal("topology with two nodes was accepted")
	}
	if len(seen) != 0 {
		t.Fatalf("argument errors sent requests: %+v", seen)
	}
	// An unknown node is an error, not an empty table.
	if _, err := runCommand(t, server.URL, "nodes", "topology", "ghost"); err == nil {
		t.Fatal("404 was swallowed")
	}
	// The node name is path-escaped, so it cannot change the route.
	seen = nil
	_, _ = runCommand(t, server.URL, "nodes", "topology", "../incidents")
	if len(seen) != 1 || seen[0].path != "/api/v1/nodes/..%2Fincidents/topology" {
		t.Fatalf("escaped path = %+v", seen)
	}
}

func TestNodesTopologyRequiresTheOperatorToken(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "operator authentication failed", http.StatusUnauthorized)
	}))
	defer server.Close()
	_, err := runCommand(t, server.URL, "nodes", "topology", "gpu-a")
	if err == nil || !strings.Contains(err.Error(), "KUBENEURONCTL_TOKEN") {
		t.Fatalf("err = %v, want the token hint", err)
	}
}

// TestNodesGroupHasOnlyTheReadOnlyTopologySubcommand: the nodes group grows
// exactly one subcommand, and it cannot approve, change or request anything.
func TestNodesGroupHasOnlyTheReadOnlyTopologySubcommand(t *testing.T) {
	root := newRootCommand()
	nodes, _, err := root.Find([]string{"nodes"})
	if err != nil || nodes == nil || nodes.Name() != "nodes" {
		t.Fatalf("nodes is not registered on the root command: %v", err)
	}
	subs := nodes.Commands()
	if len(subs) != 1 || subs[0].Name() != "topology" {
		names := make([]string, 0, len(subs))
		for _, sub := range subs {
			names = append(names, sub.Name())
		}
		t.Fatalf("nodes subcommands = %v, want only topology", names)
	}
	if subs[0].Flags().Lookup("json") == nil {
		t.Fatal("nodes topology lacks --json")
	}
	if len(subs[0].Commands()) != 0 {
		t.Fatalf("nodes topology has subcommands: %v", subs[0].Commands())
	}
	for _, flag := range []string{"approve", "reset", "apply", "force", "confirm"} {
		if subs[0].Flags().Lookup(flag) != nil {
			t.Errorf("nodes topology carries a mutating-looking flag --%s", flag)
		}
	}
}
