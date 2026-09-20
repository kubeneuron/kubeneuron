package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

type recordedRequest struct {
	Method string
	Path   string
	Query  string
	Key    string
	Body   map[string]any
}

func runRuntimeQualifications(t *testing.T, server string, args ...string) (string, error) {
	t.Helper()
	root := &cobra.Command{Use: "kubeneuronctl", SilenceUsage: true, SilenceErrors: true}
	root.PersistentFlags().String("server", server, "")
	root.PersistentFlags().String("token", "test-token", "")
	root.PersistentFlags().String("token-file", "", "")
	root.AddCommand(cmdRuntimeQualifications())
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetArgs(args)
	err := root.Execute()
	return out.String(), err
}

const qualificationJSON = `{"version":"runtime-contract-qualification/v1","id":"rcq-1","resource_version":3,"state":"ReadyForApproval",` +
	`"vendor":"nvidia","cohort":[{"name":"gpu-a","uid":"uid-a"}],"requirements":{"min_samples":2,"min_duration":"30m0s"},` +
	`"expires_at":"2026-09-05T13:00:00Z","evaluated_at":"2026-09-05T13:00:00Z","effective_state":"Expired","expired":true,` +
	`"expiry_pending":true,"ready_for_approval":false,"summary":"expired at 2026-09-05T13:00:00Z while stored as ReadyForApproval; not ready for approval"}`

func runtimeQualificationsServer(t *testing.T, seen *[]recordedRequest) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		record := recordedRequest{Method: r.Method, Path: r.URL.Path, Query: r.URL.RawQuery, Key: r.Header.Get("Idempotency-Key")}
		if raw, _ := io.ReadAll(r.Body); len(raw) > 0 {
			if err := json.Unmarshal(raw, &record.Body); err != nil {
				t.Errorf("request body is not JSON: %v", err)
			}
		}
		*seen = append(*seen, record)
		if r.Header.Get("Authorization") != "Bearer test-token" {
			http.Error(w, "operator authentication failed", http.StatusUnauthorized)
			return
		}
		if r.Method == http.MethodPost && record.Key == "" {
			http.Error(w, "Idempotency-Key header is required", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/runtime-contract-qualifications":
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(strings.Replace(qualificationJSON, `"state":"ReadyForApproval"`, `"state":"Observing"`, 1)))
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/runtime-contract-qualifications":
			_, _ = w.Write([]byte(`{"items":[` + qualificationJSON + `],"next_cursor":"page-2"}`))
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/runtime-contract-qualifications/rcq-1":
			_, _ = w.Write([]byte(qualificationJSON))
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/runtime-contract-qualifications/rcq-1/observe":
			if version, _ := record.Body["resource_version"].(float64); version == 2 {
				http.Error(w, "operations: optimistic concurrency conflict", http.StatusConflict)
				return
			}
			_, _ = w.Write([]byte(qualificationJSON))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
}

func TestRuntimeQualificationsCommandIsRegisteredWithoutAuthoritySubcommands(t *testing.T) {
	root := newRootCommand()
	group, _, err := root.Find([]string{"runtime-qualifications"})
	if err != nil || group == nil || group.Name() != "runtime-qualifications" {
		t.Fatalf("runtime-qualifications is not registered on the root command: %v", err)
	}
	names := make([]string, 0, len(group.Commands()))
	for _, sub := range group.Commands() {
		names = append(names, sub.Name())
	}
	if strings.Join(names, ",") != "create,list,observe,show" {
		t.Fatalf("runtime-qualifications subcommands = %v, want exactly create, list, observe, show", names)
	}
	for _, forbidden := range []string{"approve", "promote", "delete", "apply", "enable", "authority", "revoke", "expire", "invalidate"} {
		if sub, _, err := root.Find([]string{"runtime-qualifications", forbidden}); err == nil && sub != nil && sub.Name() == forbidden {
			t.Errorf("runtime-qualifications %s exists; the surface must stay evidence-only", forbidden)
		}
	}
	create, _, _ := root.Find([]string{"runtime-qualifications", "create"})
	for _, flag := range []string{"nodes", "vendor", "min-samples", "min-duration", "expires-at", "tenant", "cluster", "actor"} {
		if create.Flags().Lookup(flag) == nil {
			t.Errorf("create lacks --%s", flag)
		}
	}
	observe, _, _ := root.Find([]string{"runtime-qualifications", "observe"})
	for _, flag := range []string{"resource-version", "actor"} {
		if observe.Flags().Lookup(flag) == nil {
			t.Errorf("observe lacks --%s", flag)
		}
	}
	list, _, _ := root.Find([]string{"runtime-qualifications", "list"})
	for _, flag := range []string{"state", "tenant", "cluster", "since", "until", "cursor", "limit", "include-expired"} {
		if list.Flags().Lookup(flag) == nil {
			t.Errorf("list lacks --%s", flag)
		}
	}
}

func TestRuntimeQualificationsCreateSendsExplicitRequestAndPrintsJSON(t *testing.T) {
	var seen []recordedRequest
	server := runtimeQualificationsServer(t, &seen)
	defer server.Close()

	out, err := runRuntimeQualifications(t, server.URL, "runtime-qualifications", "create",
		"--nodes", "gpu-b, gpu-a", "--nodes", "gpu-c", "--vendor", "nvidia", "--min-samples", "3", "--min-duration", "30m",
		"--expires-at", "2026-09-06T12:00:00+02:00", "--tenant", "team-a", "--cluster", "east", "--actor", "alice")
	if err != nil {
		t.Fatal(err)
	}
	if len(seen) != 1 || seen[0].Method != http.MethodPost || seen[0].Path != "/api/v1/runtime-contract-qualifications" || !strings.HasPrefix(seen[0].Key, "kubeneuronctl-") {
		t.Fatalf("requests = %+v, want one keyed POST", seen)
	}
	body := seen[0].Body
	nodes, _ := body["nodes"].([]any)
	if len(nodes) != 3 || nodes[0] != "gpu-b" || nodes[1] != "gpu-a" || nodes[2] != "gpu-c" {
		t.Fatalf("nodes = %v, want the explicit trimmed cohort", body["nodes"])
	}
	requirements, _ := body["requirements"].(map[string]any)
	for key, want := range map[string]any{"actor": "alice", "vendor": "nvidia", "tenant": "team-a", "cluster": "east", "expires_at": "2026-09-06T10:00:00Z"} {
		if body[key] != want {
			t.Errorf("body %s = %v, want %v", key, body[key], want)
		}
	}
	if requirements["min_samples"] != 3.0 || requirements["min_duration"] != "30m" {
		t.Fatalf("requirements = %v, want min_samples 3 and the human-readable duration", body["requirements"])
	}
	for _, key := range []string{"state", "cohort", "profile", "config_digest", "resource_version"} {
		if _, present := body[key]; present {
			t.Errorf("create body carries %q; bindings are frozen by the controller, never the client", key)
		}
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("create output is not JSON: %v\n%s", err, out)
	}
	if got["id"] != "rcq-1" || got["state"] != "Observing" || got["effective_state"] != "Expired" {
		t.Fatalf("create output = %v", got)
	}

	// Optional scope and actor are omitted rather than sent blank.
	seen = nil
	if _, err := runRuntimeQualifications(t, server.URL, "runtime-qualifications", "create", "--nodes", "gpu-a", "--vendor", "nvidia", "--min-samples", "1", "--min-duration", "1h", "--expires-at", "2026-09-06T12:00:00Z"); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"tenant", "cluster"} {
		if _, present := seen[0].Body[key]; present {
			t.Errorf("unset --%s was sent as %v", key, seen[0].Body[key])
		}
	}
	if actor, _ := seen[0].Body["actor"].(string); actor == "" {
		t.Fatalf("actor defaulted to %q, want the local user", actor)
	}
}

func TestRuntimeQualificationsCreateValidatesFlagsBeforeSending(t *testing.T) {
	var seen []recordedRequest
	server := runtimeQualificationsServer(t, &seen)
	defer server.Close()
	valid := []string{"runtime-qualifications", "create", "--nodes", "gpu-a", "--vendor", "nvidia", "--min-samples", "2", "--min-duration", "30m", "--expires-at", "2026-09-06T12:00:00Z"}
	cases := map[string]struct {
		args []string
		want string
	}{
		"no nodes":         {[]string{"runtime-qualifications", "create", "--vendor", "nvidia", "--min-samples", "2", "--min-duration", "30m", "--expires-at", "2026-09-06T12:00:00Z"}, "--nodes is required"},
		"blank nodes":      {[]string{"runtime-qualifications", "create", "--nodes", " , ", "--vendor", "nvidia", "--min-samples", "2", "--min-duration", "30m", "--expires-at", "2026-09-06T12:00:00Z"}, "--nodes is required"},
		"no vendor":        {[]string{"runtime-qualifications", "create", "--nodes", "gpu-a", "--min-samples", "2", "--min-duration", "30m", "--expires-at", "2026-09-06T12:00:00Z"}, "--vendor is required"},
		"zero samples":     {[]string{"runtime-qualifications", "create", "--nodes", "gpu-a", "--vendor", "nvidia", "--min-duration", "30m", "--expires-at", "2026-09-06T12:00:00Z"}, "--min-samples must be a positive integer"},
		"bad duration":     {[]string{"runtime-qualifications", "create", "--nodes", "gpu-a", "--vendor", "nvidia", "--min-samples", "2", "--min-duration", "thirty", "--expires-at", "2026-09-06T12:00:00Z"}, "--min-duration must be a positive Go duration"},
		"missing duration": {[]string{"runtime-qualifications", "create", "--nodes", "gpu-a", "--vendor", "nvidia", "--min-samples", "2", "--expires-at", "2026-09-06T12:00:00Z"}, "--min-duration must be a positive Go duration"},
		"bad expiry":       {[]string{"runtime-qualifications", "create", "--nodes", "gpu-a", "--vendor", "nvidia", "--min-samples", "2", "--min-duration", "30m", "--expires-at", "tomorrow"}, "--expires-at must be an RFC3339 timestamp"},
		"positional":       {append(append([]string(nil), valid...), "gpu-b"), "unknown command"},
	}
	for name, tc := range cases {
		_, err := runRuntimeQualifications(t, server.URL, tc.args...)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want %q", name, err, tc.want)
		}
	}
	if len(seen) != 0 {
		t.Fatalf("invalid flags still sent %+v", seen)
	}
	if _, err := runRuntimeQualifications(t, server.URL, valid...); err != nil || len(seen) != 1 {
		t.Fatalf("valid create = %v (%d requests)", err, len(seen))
	}
}

func TestRuntimeQualificationsListShowObservePrintJSON(t *testing.T) {
	var seen []recordedRequest
	server := runtimeQualificationsServer(t, &seen)
	defer server.Close()

	out, err := runRuntimeQualifications(t, server.URL, "runtime-qualifications", "list", "--state", "ReadyForApproval", "--tenant", "team-a", "--cluster", "east", "--limit", "5", "--cursor", "page-1", "--include-expired=false")
	if err != nil {
		t.Fatal(err)
	}
	if len(seen) != 1 || seen[0].Method != http.MethodGet || seen[0].Path != "/api/v1/runtime-contract-qualifications" {
		t.Fatalf("list requests = %+v", seen)
	}
	if query := seen[0].Query; !strings.Contains(query, "state=ReadyForApproval") || !strings.Contains(query, "tenant=team-a") || !strings.Contains(query, "cluster=east") || !strings.Contains(query, "limit=5") || !strings.Contains(query, "cursor=page-1") || !strings.Contains(query, "include_expired=false") {
		t.Fatalf("list query = %q", query)
	}
	var page map[string]any
	if err := json.Unmarshal([]byte(out), &page); err != nil {
		t.Fatalf("list output is not JSON: %v\n%s", err, out)
	}
	items, _ := page["items"].([]any)
	if len(items) != 1 || page["next_cursor"] != "page-2" || items[0].(map[string]any)["effective_state"] != "Expired" {
		t.Fatalf("list output = %v", page)
	}
	// The default list sends no include_expired override at all.
	seen = nil
	if _, err := runRuntimeQualifications(t, server.URL, "runtime-qualifications", "list"); err != nil {
		t.Fatal(err)
	}
	if seen[0].Query != "" {
		t.Fatalf("default list query = %q, want none", seen[0].Query)
	}

	seen = nil
	out, err = runRuntimeQualifications(t, server.URL, "runtime-qualifications", "show", "rcq-1")
	if err != nil {
		t.Fatal(err)
	}
	if seen[0].Method != http.MethodGet || seen[0].Path != "/api/v1/runtime-contract-qualifications/rcq-1" {
		t.Fatalf("show request = %+v", seen[0])
	}
	var shown map[string]any
	if err := json.Unmarshal([]byte(out), &shown); err != nil {
		t.Fatalf("show output is not JSON: %v\n%s", err, out)
	}
	// The effective expiry travels verbatim: a stored ReadyForApproval past
	// its window is printed with effective_state Expired and ready_for_approval
	// false, and the summary says so in words.
	for key, want := range map[string]any{"state": "ReadyForApproval", "effective_state": "Expired", "expired": true, "expiry_pending": true, "ready_for_approval": false} {
		if shown[key] != want {
			t.Errorf("show %s = %v, want %v", key, shown[key], want)
		}
	}
	if !strings.Contains(out, "not ready for approval") {
		t.Fatalf("show output does not state the effective expiry:\n%s", out)
	}

	seen = nil
	out, err = runRuntimeQualifications(t, server.URL, "runtime-qualifications", "observe", "rcq-1", "--resource-version", "3", "--actor", "bob")
	if err != nil {
		t.Fatal(err)
	}
	if seen[0].Method != http.MethodPost || seen[0].Path != "/api/v1/runtime-contract-qualifications/rcq-1/observe" || !strings.HasPrefix(seen[0].Key, "kubeneuronctl-") {
		t.Fatalf("observe request = %+v", seen[0])
	}
	if seen[0].Body["actor"] != "bob" || seen[0].Body["resource_version"] != 3.0 || len(seen[0].Body) != 2 {
		t.Fatalf("observe body = %v, want only actor and resource_version", seen[0].Body)
	}
	var observed map[string]any
	if err := json.Unmarshal([]byte(out), &observed); err != nil || observed["id"] != "rcq-1" {
		t.Fatalf("observe output = %v (%v)\n%s", observed, err, out)
	}
	// A conflict is surfaced with the controller's status and message.
	if _, err := runRuntimeQualifications(t, server.URL, "runtime-qualifications", "observe", "rcq-1", "--resource-version", "2"); err == nil || !strings.Contains(err.Error(), "409") || !strings.Contains(err.Error(), "optimistic concurrency conflict") {
		t.Fatalf("stale observe error = %v", err)
	}
	if _, err := runRuntimeQualifications(t, server.URL, "runtime-qualifications", "observe", "rcq-1", "--resource-version", "-1"); err == nil || !strings.Contains(err.Error(), "must not be negative") {
		t.Fatalf("negative version error = %v", err)
	}
	for _, args := range [][]string{{"runtime-qualifications", "show"}, {"runtime-qualifications", "observe"}, {"runtime-qualifications", "show", "a", "b"}} {
		if _, err := runRuntimeQualifications(t, server.URL, args...); err == nil {
			t.Errorf("%v accepted the wrong number of arguments", args)
		}
	}
}
