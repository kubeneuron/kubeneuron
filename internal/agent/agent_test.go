package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kubeneuron/kubeneuron/internal/accelerator"
	"github.com/kubeneuron/kubeneuron/internal/accelerator/nvidia"
	"github.com/kubeneuron/kubeneuron/internal/agent/actionjournal"
	"github.com/kubeneuron/kubeneuron/internal/agent/nvml"
	"github.com/kubeneuron/kubeneuron/pkg/types"
)

type testClock struct {
	mu  sync.Mutex
	now time.Time
}

type countingDriver struct {
	nvml.GPUDriver
	idleChecks atomic.Int32
}

type staticNVIDIAPreflight struct {
	report nvidia.PreflightReport
}

func (p staticNVIDIAPreflight) Preflight(context.Context) nvidia.PreflightReport {
	return p.report
}

type staticRuntimeVersionProber struct {
	version string
	gpus    int
	err     error
}

func (p staticRuntimeVersionProber) Version(context.Context) (string, error) { return p.version, p.err }
func (p staticRuntimeVersionProber) GPUCount(context.Context) (int, error)   { return p.gpus, p.err }

func (d *countingDriver) EnsureIdle(ctx context.Context, index int) error {
	d.idleChecks.Add(1)
	return d.GPUDriver.EnsureIdle(ctx, index)
}

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *testClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func newTestAgent(t *testing.T, controllerURL string, clock *testClock, log *slog.Logger) *Agent {
	t.Helper()
	a, err := New(Config{
		NodeName:               "gpu-node-1",
		ControllerURL:          controllerURL,
		AllowInsecureHTTP:      true,
		SpoolPath:              t.TempDir() + "/spool.jsonl",
		ActionJournalPath:      t.TempDir() + "/actions.jsonl",
		HealthListenAddress:    "127.0.0.1:0",
		RegistrationInterval:   10 * time.Second,
		RegistrationStaleAfter: 30 * time.Second,
	}, &nvml.Fake{GPUs: []types.GPUInfo{{Index: 0, UUID: "GPU-test", Model: "test"}}}, log)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	a.now = clock.Now
	return a
}

func probe(t *testing.T, handler http.Handler, path string) *httptest.ResponseRecorder {
	t.Helper()
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))
	return recorder
}

func serveRegistrationCapability(w http.ResponseWriter, r *http.Request) bool {
	if r.Method != http.MethodGet || r.URL.Path != types.AgentRegistrationPath {
		return false
	}
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, types.AgentRegistrationProtocol+"\n")
	return true
}

func TestHealthNeverReadyWithoutControllerAcknowledgment(t *testing.T) {
	clock := &testClock{now: time.Date(2026, 7, 13, 1, 2, 3, 0, time.UTC)}
	a := newTestAgent(t, "http://controller.invalid", clock, slog.New(slog.NewTextHandler(io.Discard, nil)))
	handler := a.healthHandler()

	if got := probe(t, handler, "/livez").Code; got != http.StatusOK {
		t.Fatalf("GET /livez status = %d, want %d", got, http.StatusOK)
	}
	ready := probe(t, handler, "/readyz")
	if ready.Code != http.StatusServiceUnavailable {
		t.Fatalf("GET /readyz status = %d, want %d", ready.Code, http.StatusServiceUnavailable)
	}
	if !strings.Contains(ready.Body.String(), "controller registration acknowledgment") {
		t.Fatalf("GET /readyz body = %q, want controller acknowledgment scope", ready.Body.String())
	}
	if strings.Contains(ready.Body.String(), "last_ack_unix_nano=") {
		t.Fatalf("GET /readyz body = %q, must not expose an absent acknowledgment timestamp", ready.Body.String())
	}
	if strings.Contains(ready.Body.String(), "ack_sequence=") {
		t.Fatalf("GET /readyz body = %q, must not expose an absent acknowledgment sequence", ready.Body.String())
	}
}

func TestRegistrationAcknowledgmentMakesReadyAndPayloadIsAgentOwned(t *testing.T) {
	var payload map[string]json.RawMessage
	controller := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if serveRegistrationCapability(w, r) {
			return
		}
		if r.Method != http.MethodPost || r.URL.Path != types.AgentRegistrationPath {
			http.NotFound(w, r)
			return
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Errorf("decode registration: %v", err)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer controller.Close()

	clock := &testClock{now: time.Date(2026, 7, 13, 1, 2, 3, 0, time.UTC)}
	var logs bytes.Buffer
	a := newTestAgent(t, controller.URL, clock, slog.New(slog.NewJSONHandler(&logs, nil)))
	if err := a.register(context.Background()); err != nil {
		t.Fatalf("register() error = %v", err)
	}
	ready := probe(t, a.healthHandler(), "/readyz")
	if ready.Code != http.StatusOK {
		t.Fatalf("GET /readyz status = %d, want %d", ready.Code, http.StatusOK)
	}
	wantReadyBody := fmt.Sprintf(
		"controller registration acknowledged\nlast_ack_unix_nano=%d\nack_sequence=1\n",
		clock.Now().UnixNano(),
	)
	if got := ready.Body.String(); got != wantReadyBody {
		t.Fatalf("GET /readyz body = %q, want %q", got, wantReadyBody)
	}
	for key := range payload {
		switch key {
		case "name", "gpus", "boot_id":
		default:
			t.Errorf("registration payload contains server-owned field %q", key)
		}
	}
	for _, forbidden := range []string{"agent_last_seen", "platform", "labels", "paused", "ssh_addr", "bmc_addr"} {
		if _, ok := payload[forbidden]; ok {
			t.Errorf("registration payload contains forbidden field %q", forbidden)
		}
	}

	clock.Advance(a.cfg.RegistrationInterval)
	if err := a.register(context.Background()); err != nil {
		t.Fatalf("heartbeat register() error = %v", err)
	}
	heartbeatReady := probe(t, a.healthHandler(), "/readyz")
	wantHeartbeatBody := fmt.Sprintf(
		"controller registration acknowledged\nlast_ack_unix_nano=%d\nack_sequence=2\n",
		clock.Now().UnixNano(),
	)
	if got := heartbeatReady.Body.String(); got != wantHeartbeatBody {
		t.Fatalf("heartbeat GET /readyz body = %q, want %q", got, wantHeartbeatBody)
	}
	if got := strings.Count(logs.String(), `"msg":"controller registration acknowledged"`); got != 1 {
		t.Fatalf("initial acknowledgment log count = %d, want 1; logs: %s", got, logs.String())
	}
}

func TestRegistrationAcknowledgmentBecomesStale(t *testing.T) {
	controller := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if serveRegistrationCapability(w, r) {
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer controller.Close()

	clock := &testClock{now: time.Date(2026, 7, 13, 1, 2, 3, 0, time.UTC)}
	a := newTestAgent(t, controller.URL, clock, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := a.register(context.Background()); err != nil {
		t.Fatalf("register() error = %v", err)
	}
	clock.Advance(a.cfg.RegistrationStaleAfter)
	ready := probe(t, a.healthHandler(), "/readyz")
	if ready.Code != http.StatusServiceUnavailable {
		t.Fatalf("stale GET /readyz status = %d, want %d", ready.Code, http.StatusServiceUnavailable)
	}
	if strings.Contains(ready.Body.String(), "last_ack_unix_nano=") {
		t.Fatalf("stale GET /readyz body = %q, must not expose a stale acknowledgment timestamp", ready.Body.String())
	}
	if strings.Contains(ready.Body.String(), "ack_sequence=") {
		t.Fatalf("stale GET /readyz body = %q, must not expose a stale acknowledgment sequence", ready.Body.String())
	}
}

func TestRegistrationFailureTransitionAndRecovery(t *testing.T) {
	var status atomic.Int32
	status.Store(http.StatusNoContent)
	controller := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if serveRegistrationCapability(w, r) {
			return
		}
		w.WriteHeader(int(status.Load()))
	}))
	defer controller.Close()

	clock := &testClock{now: time.Date(2026, 7, 13, 1, 2, 3, 0, time.UTC)}
	var logs bytes.Buffer
	a := newTestAgent(t, controller.URL, clock, slog.New(slog.NewJSONHandler(&logs, nil)))
	if err := a.register(context.Background()); err != nil {
		t.Fatalf("initial register() error = %v", err)
	}

	status.Store(http.StatusServiceUnavailable)
	clock.Advance(a.cfg.RegistrationInterval)
	if err := a.register(context.Background()); err == nil {
		t.Fatal("register() error = nil, want controller failure")
	}
	if got := probe(t, a.healthHandler(), "/readyz").Code; got != http.StatusOK {
		t.Fatalf("current acknowledgment after one failure status = %d, want %d", got, http.StatusOK)
	}

	clock.Advance(a.cfg.RegistrationStaleAfter - a.cfg.RegistrationInterval)
	if err := a.register(context.Background()); err == nil {
		t.Fatal("stale register() error = nil, want controller failure")
	}
	if got := probe(t, a.healthHandler(), "/readyz").Code; got != http.StatusServiceUnavailable {
		t.Fatalf("stale acknowledgment status = %d, want %d", got, http.StatusServiceUnavailable)
	}
	clock.Advance(a.cfg.RegistrationInterval)
	_ = a.register(context.Background())
	if got := strings.Count(logs.String(), `"msg":"controller registration acknowledgment lost"`); got != 1 {
		t.Fatalf("lost transition log count = %d, want 1; logs: %s", got, logs.String())
	}

	status.Store(http.StatusNoContent)
	if err := a.register(context.Background()); err != nil {
		t.Fatalf("recovery register() error = %v", err)
	}
	if got := probe(t, a.healthHandler(), "/readyz").Code; got != http.StatusOK {
		t.Fatalf("recovered acknowledgment status = %d, want %d", got, http.StatusOK)
	}
	if got := strings.Count(logs.String(), `"msg":"controller registration acknowledgment recovered"`); got != 1 {
		t.Fatalf("recovery transition log count = %d, want 1; logs: %s", got, logs.String())
	}
	clock.Advance(a.cfg.RegistrationInterval)
	if err := a.register(context.Background()); err != nil {
		t.Fatalf("post-recovery heartbeat error = %v", err)
	}
	if got := strings.Count(logs.String(), `"msg":"controller registration acknowledgment recovered"`); got != 1 {
		t.Fatalf("recovery log count after heartbeat = %d, want 1; logs: %s", got, logs.String())
	}
}

func TestRegistrationRequiresDurableNoContentAcknowledgment(t *testing.T) {
	controller := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if serveRegistrationCapability(w, r) {
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer controller.Close()

	clock := &testClock{now: time.Date(2026, 7, 13, 1, 2, 3, 0, time.UTC)}
	a := newTestAgent(t, controller.URL, clock, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := a.register(context.Background()); err == nil {
		t.Fatal("register() error = nil for non-204 response")
	}
	if got := probe(t, a.healthHandler(), "/readyz").Code; got != http.StatusServiceUnavailable {
		t.Fatalf("GET /readyz status = %d, want %d", got, http.StatusServiceUnavailable)
	}
}

func TestRegistrationCapabilityPreventsLegacyControllerPost(t *testing.T) {
	var posts atomic.Int32
	legacyController := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			posts.Add(1)
			w.WriteHeader(http.StatusAccepted)
			return
		}
		// A legacy controller has no GET capability route.
		w.WriteHeader(http.StatusMethodNotAllowed)
	}))
	defer legacyController.Close()

	clock := &testClock{now: time.Date(2026, 7, 13, 1, 2, 3, 0, time.UTC)}
	a := newTestAgent(t, legacyController.URL, clock, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := a.register(context.Background()); err == nil {
		t.Fatal("register() error = nil for legacy controller without capability")
	}
	if got := posts.Load(); got != 0 {
		t.Fatalf("registration POST count = %d, want 0 before capability", got)
	}
	if got := probe(t, a.healthHandler(), "/readyz").Code; got != http.StatusServiceUnavailable {
		t.Fatalf("GET /readyz status = %d, want %d", got, http.StatusServiceUnavailable)
	}
}

func TestRegistrationCapabilityDoesNotFollowRedirect(t *testing.T) {
	var targetRequests atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		targetRequests.Add(1)
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, types.AgentRegistrationProtocol+"\n")
	}))
	defer target.Close()

	redirectingController := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer redirectingController.Close()

	clock := &testClock{now: time.Date(2026, 7, 13, 1, 2, 3, 0, time.UTC)}
	a := newTestAgent(t, redirectingController.URL, clock, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := a.register(context.Background()); err == nil {
		t.Fatal("register() error = nil for redirected capability")
	}
	if got := targetRequests.Load(); got != 0 {
		t.Fatalf("redirect target request count = %d, want 0", got)
	}
}

func TestProjectedTokenFileIsRereadForEveryRequest(t *testing.T) {
	var mu sync.Mutex
	var authorizations []string
	controller := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		authorizations = append(authorizations, r.Header.Get("Authorization"))
		mu.Unlock()
		if serveRegistrationCapability(w, r) {
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer controller.Close()

	tokenFile := t.TempDir() + "/token"
	if err := os.WriteFile(tokenFile, []byte("first-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	clock := &testClock{now: time.Date(2026, 7, 13, 1, 2, 3, 0, time.UTC)}
	a, err := New(Config{
		NodeName:               "gpu-node-1",
		ControllerURL:          controller.URL,
		TokenFile:              tokenFile,
		AllowInsecureHTTP:      true,
		SpoolPath:              t.TempDir() + "/spool.jsonl",
		ActionJournalPath:      t.TempDir() + "/actions.jsonl",
		HealthListenAddress:    "127.0.0.1:0",
		RegistrationInterval:   10 * time.Second,
		RegistrationStaleAfter: 30 * time.Second,
	}, &nvml.Fake{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	a.now = clock.Now
	if err := a.register(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tokenFile, []byte("second-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := a.register(context.Background()); err != nil {
		t.Fatal(err)
	}

	mu.Lock()
	defer mu.Unlock()
	want := []string{"Bearer first-token", "Bearer first-token", "Bearer second-token", "Bearer second-token"}
	if strings.Join(authorizations, "\n") != strings.Join(want, "\n") {
		t.Fatalf("Authorization sequence = %q, want %q", authorizations, want)
	}
}

func TestConfigValidationAndDefaults(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	driver := &nvml.Fake{}

	a, err := New(Config{
		ControllerURL:     "http://controller.invalid",
		AllowInsecureHTTP: true,
		SpoolPath:         t.TempDir() + "/defaults.jsonl",
		ActionJournalPath: t.TempDir() + "/actions.jsonl",
	}, driver, log)
	if err != nil {
		t.Fatalf("New(defaults) error = %v", err)
	}
	if a.cfg.HealthListenAddress != defaultHealthListenAddress ||
		a.cfg.RegistrationInterval != defaultRegistrationInterval ||
		a.cfg.RegistrationStaleAfter != defaultRegistrationStaleAfter {
		t.Fatalf("defaults = (%q, %s, %s), want (%q, %s, %s)",
			a.cfg.HealthListenAddress,
			a.cfg.RegistrationInterval,
			a.cfg.RegistrationStaleAfter,
			defaultHealthListenAddress,
			defaultRegistrationInterval,
			defaultRegistrationStaleAfter,
		)
	}

	tests := []struct {
		name string
		cfg  Config
		want string
	}{
		{
			name: "negative interval",
			cfg:  Config{RegistrationInterval: -time.Second, RegistrationStaleAfter: time.Second},
			want: "registration interval must be positive",
		},
		{
			name: "negative stale-after",
			cfg:  Config{RegistrationInterval: time.Second, RegistrationStaleAfter: -time.Second},
			want: "registration stale-after must be at least",
		},
		{
			name: "stale-after shorter than interval",
			cfg:  Config{RegistrationInterval: 2 * time.Second, RegistrationStaleAfter: time.Second},
			want: "registration stale-after must be at least",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.cfg.SpoolPath = t.TempDir() + "/spool.jsonl"
			tt.cfg.ActionJournalPath = t.TempDir() + "/actions.jsonl"
			tt.cfg.ControllerURL = "http://controller.invalid"
			tt.cfg.AllowInsecureHTTP = true
			_, err := New(tt.cfg, driver, log)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("New() error = %v, want containing %q", err, tt.want)
			}
		})
	}

	if _, err := New(Config{
		ControllerURL:          "http://controller.invalid",
		AllowInsecureHTTP:      true,
		SpoolPath:              t.TempDir() + "/equal.jsonl",
		ActionJournalPath:      t.TempDir() + "/actions.jsonl",
		RegistrationInterval:   time.Second,
		RegistrationStaleAfter: time.Second,
	}, driver, log); err != nil {
		t.Fatalf("New(stale-after equal to interval) error = %v", err)
	}

	if _, err := New(Config{
		ControllerURL: "http://controller.invalid",
		SpoolPath:     t.TempDir() + "/insecure.jsonl",
	}, driver, log); err == nil || !strings.Contains(err.Error(), "explicit insecure development mode") {
		t.Fatalf("New(implicit HTTP) error = %v", err)
	}
	if _, err := New(Config{
		ControllerURL: "https://controller.invalid",
		Token:         "token",
		SpoolPath:     t.TempDir() + "/missing-tls.jsonl",
	}, driver, log); err == nil || !strings.Contains(err.Error(), "requires CA, client certificate, and client key") {
		t.Fatalf("New(incomplete HTTPS) error = %v", err)
	}
}

func TestNewRejectsSecondProcessForSameActionJournal(t *testing.T) {
	journalPath := t.TempDir() + "/actions.jsonl"
	cfg := Config{
		NodeName:          "gpu-node-1",
		ControllerURL:     "http://controller.invalid",
		AllowInsecureHTTP: true,
		SpoolPath:         t.TempDir() + "/spool.jsonl",
		ActionJournalPath: journalPath,
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	first, err := New(cfg, &nvml.Fake{}, log)
	if err != nil {
		t.Fatalf("first New() error = %v", err)
	}
	defer func() { _ = releaseActionJournalLock(first.journalLock) }()

	if _, err := New(cfg, &nvml.Fake{}, log); err == nil || !strings.Contains(err.Error(), "locking action journal") {
		t.Fatalf("second New() error = %v, want action journal lock failure", err)
	}
}

func TestRunServesAndShutsDownHealthEndpointsWithoutKmsg(t *testing.T) {
	controller := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if serveRegistrationCapability(w, r) {
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer controller.Close()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	clock := &testClock{now: time.Date(2026, 7, 13, 1, 2, 3, 0, time.UTC)}
	a := newTestAgent(t, controller.URL, clock, slog.New(slog.NewTextHandler(io.Discard, nil)))
	a.cfg.RegistrationInterval = 5 * time.Millisecond
	a.cfg.RegistrationStaleAfter = 15 * time.Millisecond
	a.watcher.Path = t.TempDir() + "/no-kmsg"
	a.listen = func(_, _ string) (net.Listener, error) { return listener, nil }

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx) }()

	client := &http.Client{Timeout: time.Second}
	readyURL := "http://" + listener.Addr().String() + "/readyz"
	deadline := time.Now().Add(2 * time.Second)
	for {
		response, requestErr := client.Get(readyURL)
		if requestErr == nil {
			_ = response.Body.Close()
			if response.StatusCode == http.StatusOK {
				break
			}
		}
		if time.Now().After(deadline) {
			cancel()
			t.Fatal("health server did not become ready")
		}
		time.Sleep(time.Millisecond)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run() error = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run() did not stop after cancellation")
	}
}

func TestRunQueuedActionExecutesAndPostsResult(t *testing.T) {
	var mu sync.Mutex
	served := false
	var posted []types.ActionResult
	controller := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == types.AgentActionLeasePath:
			mu.Lock()
			defer mu.Unlock()
			if served {
				w.WriteHeader(http.StatusNoContent)
				return
			}
			served = true
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set(types.AgentActionLeaseHeader, "lease-test")
			w.Header().Set(types.AgentActionLeaseExpiresHeader, time.Now().Add(time.Minute).UTC().Format(time.RFC3339Nano))
			_ = json.NewEncoder(w).Encode(types.Action{
				ID: "act-1", Type: types.ActionIdleCheck,
				Params: map[string]string{"gpu_index": "0"},
			})
		case r.Method == http.MethodPost && r.URL.Path == types.AgentActionLeasePath+"/act-1/result":
			if got := r.Header.Get(types.AgentActionLeaseHeader); got != "lease-test" {
				t.Errorf("action-result lease = %q, want lease-test", got)
			}
			var res types.ActionResult
			if err := json.NewDecoder(r.Body).Decode(&res); err != nil {
				t.Errorf("bad result payload: %v", err)
			}
			mu.Lock()
			posted = append(posted, res)
			mu.Unlock()
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer controller.Close()

	clock := &testClock{now: time.Date(2026, 7, 14, 1, 2, 3, 0, time.UTC)}
	a := newTestAgent(t, controller.URL, clock, slog.New(slog.NewTextHandler(io.Discard, nil)))

	ctx := context.Background()
	if !a.runQueuedAction(ctx) {
		t.Fatal("first poll must process the queued action")
	}
	if a.runQueuedAction(ctx) {
		t.Fatal("empty queue must report no work")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(posted) != 1 {
		t.Fatalf("results posted = %d, want 1", len(posted))
	}
	if !posted[0].OK || posted[0].ActionID != "act-1" {
		t.Fatalf("result = %+v, want OK for act-1 (fake driver idle check)", posted[0])
	}
}

func TestRunQueuedActionPostsFailureResults(t *testing.T) {
	var mu sync.Mutex
	served := false
	var posted []types.ActionResult
	controller := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == types.AgentActionLeasePath:
			mu.Lock()
			defer mu.Unlock()
			if served {
				w.WriteHeader(http.StatusNoContent)
				return
			}
			served = true
			w.Header().Set(types.AgentActionLeaseHeader, "lease-test")
			w.Header().Set(types.AgentActionLeaseExpiresHeader, time.Now().Add(time.Minute).UTC().Format(time.RFC3339Nano))
			_ = json.NewEncoder(w).Encode(types.Action{ID: "act-2", Type: types.ActionReboot})
		case r.Method == http.MethodPost && r.URL.Path == types.AgentActionLeasePath+"/act-2/result":
			if got := r.Header.Get(types.AgentActionLeaseHeader); got != "lease-test" {
				t.Errorf("action-result lease = %q, want lease-test", got)
			}
			var res types.ActionResult
			_ = json.NewDecoder(r.Body).Decode(&res)
			mu.Lock()
			posted = append(posted, res)
			mu.Unlock()
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	defer controller.Close()

	clock := &testClock{now: time.Date(2026, 7, 14, 1, 2, 3, 0, time.UTC)}
	a := newTestAgent(t, controller.URL, clock, slog.New(slog.NewTextHandler(io.Discard, nil)))

	if !a.runQueuedAction(context.Background()) {
		t.Fatal("failed action must still be processed and reported")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(posted) != 1 || posted[0].OK || posted[0].Error == "" {
		t.Fatalf("posted = %+v, want one not-OK result with error detail", posted)
	}
}

func TestRunQueuedActionRetriesDurableResultWithoutReexecution(t *testing.T) {
	const actionID = "act-journal-known"
	var posts atomic.Int32
	var polls atomic.Int32
	controller := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == types.AgentActionLeasePath:
			polls.Add(1)
			w.Header().Set(types.AgentActionLeaseHeader, "lease-original")
			w.Header().Set(types.AgentActionLeaseExpiresHeader, time.Now().Add(time.Minute).UTC().Format(time.RFC3339Nano))
			_ = json.NewEncoder(w).Encode(types.Action{
				ID: actionID, Type: types.ActionIdleCheck,
				Params: map[string]string{"gpu_index": "0"},
			})
		case r.Method == http.MethodPost && r.URL.Path == types.AgentActionLeasePath+"/"+actionID+"/result":
			if got := r.Header.Get(types.AgentActionLeaseHeader); got != "lease-original" {
				t.Errorf("result lease = %q, want original lease", got)
			}
			var result types.ActionResult
			if err := json.NewDecoder(r.Body).Decode(&result); err != nil {
				t.Errorf("decode result: %v", err)
			}
			if result.ActionID != actionID || !result.OK {
				t.Errorf("result = %+v, want successful durable result for %s", result, actionID)
			}
			if posts.Add(1) == 1 {
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	}))
	defer controller.Close()

	driver := &countingDriver{GPUDriver: &nvml.Fake{GPUs: []types.GPUInfo{{Index: 0, UUID: "GPU-test"}}}}
	actionJournalPath := t.TempDir() + "/actions.jsonl"
	newAgent := func(driver nvml.GPUDriver) *Agent {
		t.Helper()
		a, err := New(Config{
			NodeName:          "gpu-node-1",
			ControllerURL:     controller.URL,
			AllowInsecureHTTP: true,
			SpoolPath:         t.TempDir() + "/spool.jsonl",
			ActionJournalPath: actionJournalPath,
		}, driver, slog.New(slog.NewTextHandler(io.Discard, nil)))
		if err != nil {
			t.Fatalf("New() error = %v", err)
		}
		return a
	}
	first := newAgent(driver)

	if first.runQueuedAction(context.Background()) {
		t.Fatal("failed result post must leave the action for a later retry")
	}
	entry, ok := first.journal.Get(actionID)
	if !ok || entry.State != actionjournal.StateOutcomeKnown || entry.Result == nil {
		t.Fatalf("journal after failed post = %#v, found %t; want durable known result", entry, ok)
	}
	if entry.LeaseToken != "lease-original" || entry.LeaseExpiresAt.Before(time.Now()) {
		t.Fatalf("journal after failed post = %#v, want original live claim", entry)
	}
	if got := driver.idleChecks.Load(); got != 1 {
		t.Fatalf("idle checks after first delivery = %d, want 1", got)
	}
	if err := releaseActionJournalLock(first.journalLock); err != nil {
		t.Fatalf("release first journal lock: %v", err)
	}

	recovered := newAgent(driver)
	recovered.recoverQueuedActions(context.Background())
	if got := polls.Load(); got != 1 {
		t.Fatalf("controller polls = %d, want only the original claim", got)
	}
	if got := posts.Load(); got != 2 {
		t.Fatalf("result POSTs = %d, want failed original plus recovered retry", got)
	}
	if got := driver.idleChecks.Load(); got != 1 {
		t.Fatalf("idle checks after result retry = %d, want 1 (must not re-execute)", got)
	}
	entry, ok = recovered.journal.Get(actionID)
	if !ok || entry.State != actionjournal.StateReported {
		t.Fatalf("journal after controller acknowledgement = %#v, found %t; want reported", entry, ok)
	}
}

func TestRunQueuedActionReportsUnknownRecoveryWithoutReexecution(t *testing.T) {
	const actionID = "act-journal-unknown"
	var posted types.ActionResult
	var polls atomic.Int32
	controller := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == types.AgentActionLeasePath:
			polls.Add(1)
			http.Error(w, "recovery must use persisted lease before polling", http.StatusInternalServerError)
		case r.Method == http.MethodPost && r.URL.Path == types.AgentActionLeasePath+"/"+actionID+"/result":
			if got := r.Header.Get(types.AgentActionLeaseHeader); got != "lease-original" {
				t.Errorf("result lease = %q, want original lease", got)
			}
			if err := json.NewDecoder(r.Body).Decode(&posted); err != nil {
				t.Errorf("decode result: %v", err)
			}
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	}))
	defer controller.Close()

	actionJournalPath := t.TempDir() + "/actions.jsonl"
	newAgent := func(driver nvml.GPUDriver) *Agent {
		t.Helper()
		a, err := New(Config{
			NodeName:          "gpu-node-1",
			ControllerURL:     controller.URL,
			AllowInsecureHTTP: true,
			SpoolPath:         t.TempDir() + "/spool.jsonl",
			ActionJournalPath: actionJournalPath,
		}, driver, slog.New(slog.NewTextHandler(io.Discard, nil)))
		if err != nil {
			t.Fatalf("New() error = %v", err)
		}
		return a
	}

	first := newAgent(&nvml.Fake{GPUs: []types.GPUInfo{{Index: 0, UUID: "GPU-test"}}})
	action := types.Action{ID: actionID, Type: types.ActionIdleCheck, Params: map[string]string{"gpu_index": "0"}}
	if _, err := first.journal.RecordReceived(action); err != nil {
		t.Fatalf("RecordReceived() error = %v", err)
	}
	if _, err := first.journal.SetClaim(actionID, "lease-original", time.Now().Add(time.Minute)); err != nil {
		t.Fatalf("SetClaim() error = %v", err)
	}
	if _, err := first.journal.MarkRunning(actionID); err != nil {
		t.Fatalf("MarkRunning() error = %v", err)
	}
	if err := releaseActionJournalLock(first.journalLock); err != nil {
		t.Fatalf("release first journal lock: %v", err)
	}

	driver := &countingDriver{GPUDriver: &nvml.Fake{GPUs: []types.GPUInfo{{Index: 0, UUID: "GPU-test"}}}}
	recovered := newAgent(driver) // Open converts the durable running marker to unknown.
	recovered.recoverQueuedActions(context.Background())
	if got := polls.Load(); got != 0 {
		t.Fatalf("controller polls during recovery = %d, want 0", got)
	}
	if got := driver.idleChecks.Load(); got != 0 {
		t.Fatalf("idle checks after unknown recovery = %d, want 0 (must not re-execute)", got)
	}
	if posted.ActionID != actionID || posted.OK || !strings.Contains(posted.Error, "outcome is unknown") {
		t.Fatalf("posted recovery result = %+v, want non-OK unknown-outcome result", posted)
	}
	entry, ok := recovered.journal.Get(actionID)
	if !ok || entry.State != actionjournal.StateReported || entry.Result != nil {
		t.Fatalf("journal after unknown acknowledgement = %#v, found %t; want reported unknown", entry, ok)
	}
}

func TestRecoveryDoesNotExecuteReceivedActionAfterLeaseExpiry(t *testing.T) {
	const actionID = "act-expired-lease"
	actionJournalPath := t.TempDir() + "/actions.jsonl"
	newAgent := func(driver nvml.GPUDriver) *Agent {
		t.Helper()
		a, err := New(Config{
			NodeName:          "gpu-node-1",
			ControllerURL:     "http://controller.invalid",
			AllowInsecureHTTP: true,
			SpoolPath:         t.TempDir() + "/spool.jsonl",
			ActionJournalPath: actionJournalPath,
		}, driver, slog.New(slog.NewTextHandler(io.Discard, nil)))
		if err != nil {
			t.Fatalf("New() error = %v", err)
		}
		return a
	}

	first := newAgent(&nvml.Fake{GPUs: []types.GPUInfo{{Index: 0, UUID: "GPU-test"}}})
	action := types.Action{ID: actionID, Type: types.ActionIdleCheck, Params: map[string]string{"gpu_index": "0"}}
	if _, err := first.journal.RecordReceived(action); err != nil {
		t.Fatalf("RecordReceived() error = %v", err)
	}
	if _, err := first.journal.SetClaim(actionID, "expired-lease", time.Now().Add(-time.Second)); err != nil {
		t.Fatalf("SetClaim() error = %v", err)
	}
	if err := releaseActionJournalLock(first.journalLock); err != nil {
		t.Fatalf("release first journal lock: %v", err)
	}

	driver := &countingDriver{GPUDriver: &nvml.Fake{GPUs: []types.GPUInfo{{Index: 0, UUID: "GPU-test"}}}}
	recovered := newAgent(driver)
	recovered.recoverQueuedActions(context.Background())
	if got := driver.idleChecks.Load(); got != 0 {
		t.Fatalf("idle checks after expired lease recovery = %d, want 0", got)
	}
	entry, ok := recovered.journal.Get(actionID)
	if !ok || entry.State != actionjournal.StateReceived || entry.LeaseToken != "expired-lease" {
		t.Fatalf("expired recoverable entry = %#v, found %t; want untouched received action", entry, ok)
	}
}

func TestNVIDIAObservationNeverTreatsFakeDriverAsRuntimeEvidence(t *testing.T) {
	var logs bytes.Buffer
	a, err := New(Config{
		NodeName:          "gpu-node-1",
		ControllerURL:     "http://controller.invalid",
		AllowInsecureHTTP: true,
		SpoolPath:         t.TempDir() + "/spool.jsonl",
		ActionJournalPath: t.TempDir() + "/actions.jsonl",
		NVIDIAObservation: NVIDIAObservationConfig{
			Enabled:           true,
			DriverVersion:     "550.54.15",
			RuntimeVersion:    "gpu-operator-v24.9.2",
			PartitionTopology: nvidia.PartitionTopologyNone,
		},
	}, &nvml.Fake{GPUs: []types.GPUInfo{{Index: 0, UUID: "GPU-fake"}}}, slog.New(slog.NewJSONHandler(&logs, nil)))
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	if a.nvidia != nil {
		t.Fatal("fake GPU driver must never create an NVIDIA observation reporter")
	}
	if !strings.Contains(logs.String(), "no real nvidia-smi runtime evidence") {
		t.Fatalf("logs = %s, want explicit fake-driver refusal", logs.String())
	}
}

func TestNVIDIAControllerProfileRejectsStaticDigestFallbackConfiguration(t *testing.T) {
	_, err := New(Config{
		NodeName:          "gpu-node-1",
		ControllerURL:     "http://controller.invalid",
		AllowInsecureHTTP: true,
		NVIDIAObservation: NVIDIAObservationConfig{
			Enabled:              true,
			ProfileDigest:        "sha256:local",
			UseControllerProfile: true,
		},
	}, &nvml.Fake{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err == nil || !strings.Contains(err.Error(), "mutually exclusive") {
		t.Fatalf("New() error = %v, want controller/static digest rejection", err)
	}
}

func TestNVIDIAAcceleratorReportRequiresRuntimeAttestation(t *testing.T) {
	clock := &testClock{now: time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)}
	a := newTestAgent(t, "http://controller.invalid", clock, slog.New(slog.NewTextHandler(io.Discard, nil)))
	snapshot := eligibleNVIDIAPreflight(clock.Now())
	a.nvidia = &nvidiaObservation{
		preflight:      staticNVIDIAPreflight{report: snapshot},
		driverVersion:  "550.54.15",
		runtimeVersion: "gpu-operator-v24.9.2",
		profileDigest:  "sha256:profile",
	}

	report, err := a.nvidiaAcceleratorReport(context.Background())
	if err != nil {
		t.Fatalf("nvidiaAcceleratorReport() error = %v", err)
	}
	if report.Readiness != types.AcceleratorReadinessDegraded {
		t.Fatalf("unattested report readiness = %q, want degraded; report=%+v", report.Readiness, report)
	}
	if !strings.Contains(strings.Join(report.ReadinessReasons, " | "), "not attested") {
		t.Fatalf("unattested report reasons = %v, want runtime-attestation denial", report.ReadinessReasons)
	}
	if report.TopologySafety != types.AcceleratorTopologyVerifiedUnpartitioned {
		t.Fatalf("report topology = %q, want verified-unpartitioned", report.TopologySafety)
	}
	if report.ProfileDigest != "sha256:profile" || len(report.Devices) != 1 {
		t.Fatalf("report lost profile or inventory: %+v", report)
	}
	if len(report.Capabilities) != 2 || report.Capabilities[1].Action != types.AcceleratorActionResetDevice {
		t.Fatalf("report capabilities = %+v, want mapped NVIDIA capabilities", report.Capabilities)
	}

	// A bounded local DCGM observation must agree with the controller/profile
	// value before an otherwise matching NVIDIA preflight can be ready.
	a.nvidia.runtimeProber = staticRuntimeVersionProber{version: "gpu-operator-v24.9.2", gpus: 1}
	report, err = a.nvidiaAcceleratorReport(context.Background())
	if err != nil {
		t.Fatalf("attested nvidiaAcceleratorReport() error = %v", err)
	}
	if report.Readiness != types.AcceleratorReadinessReady {
		t.Fatalf("attested report readiness = %q, want ready; report=%+v", report.Readiness, report)
	}
	a.nvidia.runtimeProber = staticRuntimeVersionProber{version: "gpu-operator-v24.9.2", gpus: 2}
	report, err = a.nvidiaAcceleratorReport(context.Background())
	if err != nil {
		t.Fatalf("mismatched discovery nvidiaAcceleratorReport() error = %v", err)
	}
	if report.Readiness != types.AcceleratorReadinessDegraded ||
		!strings.Contains(strings.Join(report.ReadinessReasons, " | "), "DCGM discovery found 2 GPUs") {
		t.Fatalf("mismatched discovery report = %+v, want degraded inventory mismatch", report)
	}
	a.nvidia.runtimeProber = staticRuntimeVersionProber{version: "dcgm-9.9", gpus: 1}
	report, err = a.nvidiaAcceleratorReport(context.Background())
	if err != nil {
		t.Fatalf("mismatched attestation nvidiaAcceleratorReport() error = %v", err)
	}
	if report.Readiness != types.AcceleratorReadinessDegraded ||
		!strings.Contains(strings.Join(report.ReadinessReasons, " | "), "does not match the controller profile") {
		t.Fatalf("mismatched attested report = %+v, want degraded profile mismatch", report)
	}

	// A healthy probe cannot fill in missing immutable profile metadata. It
	// remains a report, but is deliberately degraded rather than ready.
	a.nvidia.runtimeProber = nil
	a.nvidia.runtimeVersion = ""
	report, err = a.nvidiaAcceleratorReport(context.Background())
	if err != nil {
		t.Fatalf("missing-version nvidiaAcceleratorReport() error = %v", err)
	}
	if report.Readiness != types.AcceleratorReadinessDegraded {
		t.Fatalf("missing runtime version readiness = %q, want degraded", report.Readiness)
	}
	if !strings.Contains(strings.Join(report.ReadinessReasons, " | "), "missing driver_version or runtime_version") {
		t.Fatalf("missing-version reasons = %v, want profile metadata reason", report.ReadinessReasons)
	}
}

func TestNVIDIAAcceleratorReportUsesObservedDriverVersionForRealRuntime(t *testing.T) {
	clock := &testClock{now: time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)}
	a := newTestAgent(t, "http://controller.invalid", clock, slog.New(slog.NewTextHandler(io.Discard, nil)))
	snapshot := eligibleNVIDIAPreflight(clock.Now())
	snapshot.Inventory.DriverVersion = "570.86.15"
	a.nvidia = &nvidiaObservation{
		preflight:                staticNVIDIAPreflight{report: snapshot},
		driverVersion:            "manual-fallback-must-not-win",
		runtimeVersion:           "gpu-operator-v24.9.2",
		useObservedDriverVersion: true,
	}
	report, err := a.nvidiaAcceleratorReport(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if report.DriverVersion != "570.86.15" {
		t.Fatalf("reported driver version = %q, want observed version", report.DriverVersion)
	}

	snapshot.Inventory.DriverVersion = ""
	a.nvidia.preflight = staticNVIDIAPreflight{report: snapshot}
	report, err = a.nvidiaAcceleratorReport(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if report.Readiness != types.AcceleratorReadinessDegraded || report.DriverVersion != "" {
		t.Fatalf("missing observed driver = readiness %q version %q, want degraded with no fallback", report.Readiness, report.DriverVersion)
	}
}

func TestNVIDIAAcceleratorReportUnknownTopologyDoesNotDeclareReset(t *testing.T) {
	clock := &testClock{now: time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)}
	a := newTestAgent(t, "http://controller.invalid", clock, slog.New(slog.NewTextHandler(io.Discard, nil)))
	snapshot := eligibleNVIDIAPreflight(clock.Now())
	snapshot.Readiness = nvidia.PreflightObservedOnly
	snapshot.Topology = nvidia.PartitionTopologyUnknown
	snapshot.Capabilities = accelerator.CapabilitySet{{
		Action: accelerator.ActionVerifyHealth,
		Scopes: []accelerator.TargetScope{accelerator.ScopeNode, accelerator.ScopePhysicalDevice},
	}}
	snapshot.Reasons = []string{"physical-device reset unavailable: partition topology \"unknown\" is not explicitly unpartitioned"}
	a.nvidia = &nvidiaObservation{
		preflight:      staticNVIDIAPreflight{report: snapshot},
		driverVersion:  "550.54.15",
		runtimeVersion: "gpu-operator-v24.9.2",
	}

	report, err := a.nvidiaAcceleratorReport(context.Background())
	if err != nil {
		t.Fatalf("nvidiaAcceleratorReport() error = %v", err)
	}
	if report.Readiness != types.AcceleratorReadinessDegraded || report.TopologySafety != types.AcceleratorTopologyUnknown {
		t.Fatalf("unknown-topology report = %+v, want degraded unknown observation", report)
	}
	for _, capability := range report.Capabilities {
		if capability.Action == types.AcceleratorActionResetDevice {
			t.Fatalf("unknown topology reported reset capability: %+v", report.Capabilities)
		}
	}
}

func TestNVIDIAReportPostFailureRetriesWithoutChangingReadiness(t *testing.T) {
	var reportPosts atomic.Int32
	controller := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == types.AgentAcceleratorReportPath {
			reportPosts.Add(1)
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		http.NotFound(w, r)
	}))
	defer controller.Close()

	clock := &testClock{now: time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)}
	a := newTestAgent(t, controller.URL, clock, slog.New(slog.NewTextHandler(io.Discard, nil)))
	a.nvidia = &nvidiaObservation{
		preflight:      staticNVIDIAPreflight{report: eligibleNVIDIAPreflight(clock.Now())},
		driverVersion:  "550.54.15",
		runtimeVersion: "gpu-operator-v24.9.2",
	}
	a.recordRegistrationAcknowledgment()
	if got := probe(t, a.healthHandler(), "/readyz").Code; got != http.StatusOK {
		t.Fatalf("ready before report failure = %d, want %d", got, http.StatusOK)
	}

	a.reportNVIDIA(context.Background())
	a.reportNVIDIA(context.Background())
	if got := reportPosts.Load(); got != 2 {
		t.Fatalf("NVIDIA report post attempts = %d, want retry on each call", got)
	}
	if got := probe(t, a.healthHandler(), "/readyz").Code; got != http.StatusOK {
		t.Fatalf("report failure changed readiness to %d, want %d", got, http.StatusOK)
	}
}

func TestNVIDIAControllerProfileBindingDoesNotFallBackToStaticDigest(t *testing.T) {
	var profileLookups atomic.Int32
	var reportPosts atomic.Int32
	var publishProfile atomic.Bool
	var posted types.AgentAcceleratorReport
	controller := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == types.AgentAcceleratorProfilePath:
			profileLookups.Add(1)
			if r.URL.Query().Get("vendor") != string(types.AcceleratorVendorNVIDIA) {
				http.Error(w, "wrong vendor", http.StatusBadRequest)
				return
			}
			if !publishProfile.Load() {
				w.WriteHeader(http.StatusNoContent)
				return
			}
			_ = json.NewEncoder(w).Encode(types.AgentAcceleratorObservationProfile{
				Vendor:            types.AcceleratorVendorNVIDIA,
				ProfileDigest:     "sha256:" + strings.Repeat("b", 64),
				ProfileUID:        "controller-profile-uid",
				ProfileGeneration: 3,
				RuntimeVersion:    "gpu-operator-v24.9.2",
			})
		case r.Method == http.MethodPost && r.URL.Path == types.AgentAcceleratorReportPath:
			if err := json.NewDecoder(r.Body).Decode(&posted); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			reportPosts.Add(1)
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	}))
	defer controller.Close()

	clock := &testClock{now: time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)}
	a := newTestAgent(t, controller.URL, clock, slog.New(slog.NewTextHandler(io.Discard, nil)))
	a.nvidia = &nvidiaObservation{
		preflight:            staticNVIDIAPreflight{report: eligibleNVIDIAPreflight(clock.Now())},
		driverVersion:        "550.54.15",
		runtimeVersion:       "gpu-operator-v24.9.2",
		profileDigest:        "sha256:unsafe-static-fallback",
		useControllerProfile: true,
	}

	// No selected profile must not produce a report carrying the local digest.
	a.reportNVIDIA(context.Background())
	if got := reportPosts.Load(); got != 0 {
		t.Fatalf("report posts without selected profile = %d, want 0", got)
	}
	publishProfile.Store(true)
	a.reportNVIDIA(context.Background())
	if got := profileLookups.Load(); got != 2 {
		t.Fatalf("profile lookups = %d, want 2 heartbeat attempts", got)
	}
	if got := reportPosts.Load(); got != 1 {
		t.Fatalf("report posts with selected profile = %d, want 1", got)
	}
	if posted.ProfileDigest != "sha256:"+strings.Repeat("b", 64) {
		t.Fatalf("posted profile digest = %q, want controller-selected digest", posted.ProfileDigest)
	}
	if posted.ProfileUID != "controller-profile-uid" || posted.ProfileGeneration != 3 {
		t.Fatalf("posted profile identity = %q/%d, want controller-profile-uid/3", posted.ProfileUID, posted.ProfileGeneration)
	}
}

func eligibleNVIDIAPreflight(observedAt time.Time) nvidia.PreflightReport {
	return nvidia.PreflightReport{
		Readiness: nvidia.PreflightEligible,
		Topology:  nvidia.PartitionTopologyNone,
		Inventory: accelerator.Inventory{
			NodeName:       "gpu-node-1",
			Vendor:         accelerator.VendorNVIDIA,
			DriverVersion:  "550.54.15",
			RuntimeVersion: "gpu-operator-v24.9.2",
			ObservedAt:     observedAt,
			Devices: []accelerator.Device{{
				ID:     "GPU-0001",
				Kind:   accelerator.DevicePhysical,
				Family: accelerator.FamilyGPU,
				Model:  "NVIDIA H100",
				Attributes: map[string]string{
					"nvidia.com/gpu-index": "0",
				},
			}},
		},
		Capabilities: accelerator.CapabilitySet{
			{
				Action: accelerator.ActionVerifyHealth,
				Scopes: []accelerator.TargetScope{accelerator.ScopeNode, accelerator.ScopePhysicalDevice},
			},
			{
				Action: accelerator.ActionResetDevice,
				Scopes: []accelerator.TargetScope{accelerator.ScopePhysicalDevice},
			},
		},
	}
}

// Destructive actions can never be armed against the fake driver: the fake
// reports success for ResetGPU, which would turn missing tooling into a
// silent lie about hardware state.
func TestNewRefusesDestructiveActionsWithFakeDriver(t *testing.T) {
	cfg := Config{
		NodeName:                 "gpu-node-1",
		ControllerURL:            "http://controller.invalid",
		AllowInsecureHTTP:        true,
		SpoolPath:                t.TempDir() + "/spool.jsonl",
		ActionJournalPath:        t.TempDir() + "/actions.jsonl",
		EnableDestructiveActions: true,
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	if _, err := New(cfg, &nvml.Fake{}, log); err == nil ||
		!strings.Contains(err.Error(), "destructive actions require the real nvidia-smi driver") {
		t.Fatalf("New() error = %v, want fake-driver refusal", err)
	}

	// The same config with the real SMI driver type constructs successfully
	// (the binary path is never executed at construction time).
	a, err := New(cfg, nvml.NewSMI("/nonexistent/nvidia-smi"), log)
	if err != nil {
		t.Fatalf("New() with real driver type error = %v", err)
	}
	defer func() { _ = releaseActionJournalLock(a.journalLock) }()
}
