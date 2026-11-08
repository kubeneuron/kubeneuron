package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const impactDoc = `{"version":"impact-preview/v1","incident":"inc-1","round":2,"computed_at":"2026-11-05T10:00:00Z","digest":"sha256:cur",` +
	`"node":{"name":"gpu-a","topology":{"topology":"whole-device","device_actions":"eligible"}},"pending_step":"reboot",` +
	`"downstream_disruptions":["platform.drain"],"workloads":{"total":5,"gpu_holders":2,"namespaces":3,"eligible_for_checkpoint":2,` +
	`"ineligible":{"not-opted-in":2,"drain-excluded":1},"groups":[],"items":[]},"devices":{"attribution":"per-pod"},` +
	`"limits":{"items_truncated":false,"max_items":200},"warnings":["DeviceActionsRefused"],"unavailable":null}`

type impactRequest struct {
	method, path string
	body         map[string]any
}

func impactServer(t *testing.T, seen *[]impactRequest, servePreview bool) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		req := impactRequest{method: r.Method, path: r.URL.RequestURI()}
		if r.Body != nil {
			raw, _ := io.ReadAll(r.Body)
			if len(raw) > 0 {
				_ = json.Unmarshal(raw, &req.body)
			}
		}
		*seen = append(*seen, req)
		switch {
		case strings.HasPrefix(r.URL.Path, "/api/v1/incidents/inc-1/impact-preview"):
			if !servePreview {
				http.Error(w, "not found", http.StatusNotFound)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(impactDoc))
		case r.URL.Path == "/api/v1/incidents/inc-1/approve", r.URL.Path == "/api/v1/incidents/inc-1/reject":
			w.WriteHeader(http.StatusNoContent)
		default:
			http.Error(w, "not found", http.StatusNotFound)
		}
	}))
}

func TestIncidentsImpactRendersTheSummaryAndJSON(t *testing.T) {
	var seen []impactRequest
	server := impactServer(t, &seen, true)
	defer server.Close()

	out, err := runCommand(t, server.URL, "incidents", "impact", "inc-1")
	if err != nil {
		t.Fatalf("incidents impact: %v", err)
	}
	if len(seen) != 1 || seen[0].method != http.MethodGet || seen[0].path != "/api/v1/incidents/inc-1/impact-preview" {
		t.Fatalf("requests = %+v, want one GET on the current round", seen)
	}
	for _, want := range []string{"DIGEST", "sha256:cur", "TOPOLOGY", "whole-device / device actions eligible", "WORKLOADS", "5 total, 2 GPU holders, 3 namespaces", "CHECKPOINT", "2 eligible", "WARNINGS", "DeviceActionsRefused", "DOWNSTREAM", "platform.drain"} {
		if !strings.Contains(out, want) {
			t.Errorf("table lacks %q:\n%s", want, out)
		}
	}
	seen = nil
	out, err = runCommand(t, server.URL, "incidents", "impact", "inc-1", "--round", "2", "--json")
	if err != nil {
		t.Fatal(err)
	}
	if seen[0].path != "/api/v1/incidents/inc-1/impact-preview?round=2" {
		t.Fatalf("path = %q, want the round forwarded", seen[0].path)
	}
	var doc map[string]any
	if err := json.Unmarshal([]byte(out), &doc); err != nil || doc["digest"] != "sha256:cur" {
		t.Fatalf("--json output is not the document: %v\n%s", err, out)
	}
}

// An approve pinned to a round never binds itself to whatever preview is
// current: without the digest the human read it prints the preview, names
// the exact bound command and stops, so a preview recomputed between the
// read and the click cannot be approved by a command that never saw it.
func TestApproveWithRoundNeverBindsToAPreviewItDidNotRead(t *testing.T) {
	var seen []impactRequest
	server := impactServer(t, &seen, true)
	defer server.Close()

	out, err := runCommand(t, server.URL, "approve", "inc-1", "--actor", "alice", "--round", "2")
	if err == nil {
		t.Fatalf("approve without the digest must stop:\n%s", out)
	}
	if len(seen) != 1 || seen[0].method != http.MethodGet {
		t.Fatalf("requests = %+v, want the preview read and NO approve", seen)
	}
	if !strings.Contains(out, "sha256:cur") || !strings.Contains(err.Error(), "--impact-digest sha256:cur") {
		t.Fatalf("output should show the preview and the error the bound command:\n%s\n%v", out, err)
	}

	// An explicit digest is sent as given, without a read.
	seen = nil
	if _, err := runCommand(t, server.URL, "approve", "inc-1", "--actor", "alice", "--round", "2", "--impact-digest", "sha256:mine"); err != nil {
		t.Fatal(err)
	}
	if len(seen) != 1 || seen[0].body["impact_digest"] != "sha256:mine" {
		t.Fatalf("requests = %+v, want one approve carrying the given digest", seen)
	}

	// Without --round nothing is fetched and no digest is sent.
	seen = nil
	if _, err := runCommand(t, server.URL, "approve", "inc-1", "--actor", "alice"); err != nil {
		t.Fatal(err)
	}
	if len(seen) != 1 {
		t.Fatalf("requests = %+v, want one unbound approve", seen)
	}
	if _, has := seen[0].body["impact_digest"]; has {
		t.Fatalf("approve body = %v, want no digest", seen[0].body)
	}

	// A reject never reads the preview.
	seen = nil
	if _, err := runCommand(t, server.URL, "reject", "inc-1", "--actor", "alice", "--round", "2"); err != nil {
		t.Fatal(err)
	}
	if len(seen) != 1 || seen[0].path != "/api/v1/incidents/inc-1/reject" {
		t.Fatalf("requests = %+v, want one reject", seen)
	}
}

func TestApproveAgainstAControllerWithoutPreviewsStaysUnbound(t *testing.T) {
	var seen []impactRequest
	server := impactServer(t, &seen, false)
	defer server.Close()
	if _, err := runCommand(t, server.URL, "approve", "inc-1", "--actor", "alice", "--round", "2"); err != nil {
		t.Fatalf("approve must survive a 404 on the preview route: %v", err)
	}
	if len(seen) != 2 || seen[1].body["park_epoch"] != float64(2) {
		t.Fatalf("requests = %+v", seen)
	}
	if _, has := seen[1].body["impact_digest"]; has {
		t.Fatalf("approve body = %v, want no digest", seen[1].body)
	}
}
