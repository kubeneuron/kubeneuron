// Package e2e drives the full dry-run remediation loop in-process: real
// HTTP ingestion (webhook and authenticated agent routes), the real shipped
// policies and playbooks, the real reconcile walk, the operator REST API,
// and a SQLite file store — with every side effect a logged no-op.
//
// Kubernetes-level behavior (CRDs, operator, mTLS identity) is covered by
// hack/kind-integration.sh; this suite owns the remediation semantics.
package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kubeneuron/kubeneuron/internal/config"
	"github.com/kubeneuron/kubeneuron/internal/controller"
	"github.com/kubeneuron/kubeneuron/internal/httpapi"
	"github.com/kubeneuron/kubeneuron/internal/metrics"
	"github.com/kubeneuron/kubeneuron/internal/notify"
	"github.com/kubeneuron/kubeneuron/internal/playbook"
	"github.com/kubeneuron/kubeneuron/internal/safety"
	"github.com/kubeneuron/kubeneuron/internal/store"
	"github.com/kubeneuron/kubeneuron/internal/store/sqlite"
	"github.com/kubeneuron/kubeneuron/pkg/types"
	"github.com/kubeneuron/kubeneuron/web"
)

const operatorToken = "e2e-operator-token"

type harness struct {
	t          *testing.T
	public     *httptest.Server // webhook + operator API
	agent      *httptest.Server // authenticated agent routes
	controller *controller.Controller
	cancel     context.CancelFunc
}

type staticAuthenticator struct{ node string }

func (s staticAuthenticator) AuthenticateAgent(*http.Request) (httpapi.AgentPrincipal, error) {
	return httpapi.AgentPrincipal{NodeName: s.node}, nil
}

// newHarness wires the full controller from the shipped configuration and
// starts its Run loop with fast test timings.
func newHarness(t *testing.T) *harness {
	t.Helper()
	st, err := sqlite.Open(filepath.Join(t.TempDir(), "e2e.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })

	cfg, err := config.Load("../../configs/policies.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Safety.DryRun {
		t.Fatal("shipped config must be dry-run for this suite")
	}
	books, err := playbook.LoadDir("../../configs/playbooks")
	if err != nil {
		t.Fatal(err)
	}
	var policies []playbook.Policy
	for _, p := range cfg.Policies {
		policies = append(policies, playbook.Policy{Class: p.Match.Class, Playbook: p.Playbook, Params: p.Params})
	}
	engine, err := playbook.NewEngine(books, policies)
	if err != nil {
		t.Fatal(err)
	}
	gate := safety.NewGate(safety.Limits{
		MaxConcurrentRemediations: cfg.Safety.MaxConcurrentRemediations,
		MaxConcurrentReboots:      cfg.Safety.MaxConcurrentReboots,
		DryRun:                    cfg.Safety.DryRun,
	})
	flap := safety.NewFlapDetector(cfg.Safety.Flap.Count, cfg.Safety.Flap.Window.Std())
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	ctrl := controller.New(st, st, engine, gate, flap, nil, nil, &notify.Log{Logger: log}, log)
	ctrl.SetTimings(80*time.Millisecond, time.Hour)
	ctrl.SetReconcileInterval(15 * time.Millisecond)

	api := httpapi.New(ctrl)
	api.EnableOperatorAPI(ctrl, operatorToken)
	// The dry-run harness posts to the webhook without a token; the webhook now
	// fails closed by default, so opt into the development-only insecure mode.
	api.AllowInsecureWebhook()
	api.SetBackupStore(st, t.TempDir())
	metricsStore.Store(st)
	metricsOnce.Do(func() {
		metrics.RegisterIncidentStates(func() map[types.IncidentState]int {
			current := metricsStore.Load()
			if current == nil {
				return nil
			}
			counts, err := current.CountIncidentsByState(context.Background())
			if err != nil {
				return nil
			}
			return counts
		})
	})
	api.SetMetricsHandler(metrics.Handler())
	if dist, err := web.Dist(); err == nil {
		api.SetUI(http.FS(dist))
	}

	ctx, cancel := context.WithCancel(context.Background())
	go func() { _ = ctrl.Run(ctx) }()

	h := &harness{
		t:          t,
		public:     httptest.NewServer(api.Routes()),
		agent:      httptest.NewServer(api.AgentRoutes(staticAuthenticator{node: "gpu-node-1"})),
		controller: ctrl,
		cancel:     cancel,
	}
	t.Cleanup(func() {
		cancel()
		h.public.Close()
		h.agent.Close()
	})
	return h
}

func (h *harness) postJSON(server, path string, v any, wantStatus int) {
	h.t.Helper()
	payload, err := json.Marshal(v)
	if err != nil {
		h.t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodPost, server+path, bytes.NewReader(payload))
	if err != nil {
		h.t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if strings.HasPrefix(server, h.public.URL) && path != "/api/v1/webhooks/alertmanager" {
		req.Header.Set("Authorization", "Bearer "+operatorToken)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != wantStatus {
		body, _ := io.ReadAll(resp.Body)
		h.t.Fatalf("POST %s: status %d, want %d: %s", path, resp.StatusCode, wantStatus, body)
	}
}

func (h *harness) getJSON(path string, out any) {
	h.t.Helper()
	req, err := http.NewRequest(http.MethodGet, h.public.URL+path, nil)
	if err != nil {
		h.t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+operatorToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		h.t.Fatalf("GET %s: status %d: %s", path, resp.StatusCode, body)
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		h.t.Fatalf("GET %s: %v", path, err)
	}
}

// waitForIncident polls the operator API until an incident for the node
// reaches the wanted state.
func (h *harness) waitForIncident(node string, want types.IncidentState) *types.Incident {
	h.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	var last *types.Incident
	for time.Now().Before(deadline) {
		var incidents []*types.Incident
		h.getJSON("/api/v1/incidents?node="+node, &incidents)
		for _, inc := range incidents {
			last = inc
			if inc.State == want {
				return inc
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	if last != nil {
		h.t.Fatalf("incident on %s never reached %s; last state %s (playbook %s, step %d)",
			node, want, last.State, last.Playbook, last.StepIndex)
	}
	h.t.Fatalf("no incident ever appeared for %s", node)
	return nil
}

func agentEvent(eventID string, xid int) types.AgentEvent {
	return types.AgentEvent{
		EventID: eventID, Node: "gpu-node-1", GPUIndex: 0, GPUUUID: "GPU-e2e-1",
		XID: xid, Raw: fmt.Sprintf("NVRM: Xid (PCI:0000:3b:00): %d, e2e", xid),
		Timestamp: time.Now(),
	}
}

// TestDryRunLadderWithApproval walks XID 79 end to end over the wire:
// agent event -> incident -> drain ladder -> approval park -> REST approve
// -> remaining steps -> verification -> resolve, all as dry-run no-ops with
// a complete audit trail.
func TestDryRunLadderWithApproval(t *testing.T) {
	h := newHarness(t)

	h.postJSON(h.agent.URL, "/api/v1/events", agentEvent("e2e-79", 79), http.StatusAccepted)

	inc := h.waitForIncident("gpu-node-1", types.StateAwaitingApproval)
	if inc.Playbook != "fell-off-bus" || !inc.DryRun {
		t.Fatalf("incident = playbook %s dryRun %v, want fell-off-bus dry-run", inc.Playbook, inc.DryRun)
	}

	h.postJSON(h.public.URL, "/api/v1/incidents/"+inc.ID+"/approve",
		map[string]string{"actor": "e2e-operator"}, http.StatusNoContent)

	inc = h.waitForIncident("gpu-node-1", types.StateResolved)
	if inc.ResolvedAt == nil {
		t.Fatal("resolved incident must carry ResolvedAt")
	}

	var detail struct {
		Incident *types.Incident     `json:"incident"`
		Audit    []*types.AuditEntry `json:"audit"`
	}
	h.getJSON("/api/v1/incidents/"+inc.ID, &detail)

	var dryRunSteps, approvals int
	sawResolve := false
	for _, e := range detail.Audit {
		if strings.Contains(e.Result, "DRY-RUN") {
			dryRunSteps++
			if !e.DryRun {
				t.Fatalf("dry-run step audited without dry_run flag: %+v", e)
			}
		}
		if e.Actor == "token:e2e-operator" {
			approvals++
		}
		if e.ToState == types.StateResolved {
			sawResolve = true
		}
	}
	if dryRunSteps < 3 {
		t.Fatalf("dry-run step audits = %d, want the ladder recorded; trail: %+v", dryRunSteps, detail.Audit)
	}
	if approvals == 0 {
		t.Fatal("the approving actor must appear in the audit trail")
	}
	if !sawResolve {
		t.Fatal("audit trail must record the resolution")
	}
}

// TestEventDedupAcrossTheWire replays the same event (same capture ID) and
// asserts exactly one incident with one counted signal.
func TestEventDedupAcrossTheWire(t *testing.T) {
	h := newHarness(t)

	for i := 0; i < 3; i++ {
		h.postJSON(h.agent.URL, "/api/v1/events", agentEvent("dup-48", 48), http.StatusAccepted)
	}
	inc := h.waitForIncident("gpu-node-1", types.StateVerifying)
	if inc.SignalSeen != 1 {
		t.Fatalf("SignalSeen = %d, want 1 (replays deduplicated by event ID)", inc.SignalSeen)
	}

	var incidents []*types.Incident
	h.getJSON("/api/v1/incidents", &incidents)
	if len(incidents) != 1 {
		t.Fatalf("incidents = %d, want 1", len(incidents))
	}
}

// TestPauseBlocksAutomationOverTheWire pauses via REST, injects a critical
// event, and asserts the incident holds in EVALUATING until resume.
func TestPauseBlocksAutomationOverTheWire(t *testing.T) {
	h := newHarness(t)

	h.postJSON(h.public.URL, "/api/v1/pause", map[string]string{"actor": "e2e"}, http.StatusNoContent)
	h.postJSON(h.agent.URL, "/api/v1/events", agentEvent("pause-48", 48), http.StatusAccepted)

	inc := h.waitForIncident("gpu-node-1", types.StateEvaluating)
	time.Sleep(100 * time.Millisecond) // several reconcile ticks under pause
	var current []*types.Incident
	h.getJSON("/api/v1/incidents?node=gpu-node-1", &current)
	if current[0].State != types.StateEvaluating || current[0].StepIndex != 0 {
		t.Fatalf("paused incident advanced: %s step %d", current[0].State, current[0].StepIndex)
	}

	req, _ := http.NewRequest(http.MethodDelete, h.public.URL+"/api/v1/pause?actor=e2e", nil)
	req.Header.Set("Authorization", "Bearer "+operatorToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("resume status = %d", resp.StatusCode)
	}
	_ = inc
	h.waitForIncident("gpu-node-1", types.StateResolved)
}

// TestWebhookPathOpensIncident drives the slow path: an Alertmanager
// payload lands as an incident for the labeled node.
func TestWebhookPathOpensIncident(t *testing.T) {
	h := newHarness(t)

	// The webhook cross-checks the alert's node against known inventory, so an
	// alert can never drive remediation on a node the controller has never seen.
	// Register the node first, exactly as a live agent would on startup.
	h.postJSON(h.agent.URL, types.AgentRegistrationPath, map[string]any{"name": "gpu-node-1"}, http.StatusNoContent)

	payload := map[string]any{
		"version": "4", "status": "firing",
		"alerts": []map[string]any{{
			"status": "firing",
			"labels": map[string]string{
				"alertname": "GpuRowRemapFailure",
				"instance":  "gpu-node-1:9400",
				"UUID":      "GPU-e2e-1",
				"severity":  "critical",
			},
			"startsAt": time.Now().Format(time.RFC3339),
		}},
	}
	h.postJSON(h.public.URL, "/api/v1/webhooks/alertmanager", payload, http.StatusAccepted)

	// row-remap-failure maps to the rma playbook, whose ticket step is a
	// dry-run notify; the incident must reach at least EVALUATING with the
	// port stripped from the node identity.
	inc := h.waitForIncident("gpu-node-1", types.StateAwaitingApproval)
	if inc.Playbook != "rma" {
		t.Fatalf("playbook = %s, want rma", inc.Playbook)
	}
	if inc.Target.Node != "gpu-node-1" {
		t.Fatalf("node = %q, want port-stripped gpu-node-1", inc.Target.Node)
	}
}

// The state collector registers once per test binary; the indirection lets
// every harness point it at its own store.
var (
	metricsOnce  sync.Once
	metricsStore atomic.Pointer[sqlite.Store]
)

// TestMetricsExposition drives an incident and asserts the Prometheus
// surface reflects it.
func TestMetricsExposition(t *testing.T) {
	h := newHarness(t)

	h.postJSON(h.agent.URL, "/api/v1/events", agentEvent("metrics-48", 48), http.StatusAccepted)
	h.waitForIncident("gpu-node-1", types.StateVerifying)

	resp, err := http.Get(h.public.URL + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	text := string(body)
	// Counters are process-global and accumulate across the suite; assert
	// presence with labels. The state gauge reads this harness's store, so
	// its value is exact.
	for _, want := range []string{
		`kubeneuron_signals_total{class="ecc-dbe",source="agent-event"}`,
		`kubeneuron_incidents_opened_total{class="ecc-dbe"}`,
		`kubeneuron_incidents{state="VERIFYING"} 1`,
		`kubeneuron_steps_total{outcome="dry_run"}`,
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("metrics missing %q; body:\n%s", want, text)
		}
	}
}

// TestEmbeddedPanelServed asserts the control panel ships in the binary and
// that its API stays token-protected.
func TestEmbeddedPanelServed(t *testing.T) {
	h := newHarness(t)

	resp, err := http.Get(h.public.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), "KubeNeuron") {
		t.Fatalf("panel index = %d, body %.80s", resp.StatusCode, body)
	}

	// The static page is public; the data it needs is not.
	resp, err = http.Get(h.public.URL + "/api/v1/incidents")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated API from panel context = %d, want 401", resp.StatusCode)
	}
}

// The backup endpoint streams a consistent snapshot that a fresh store can
// open — the restore path, not just the download, is what matters. It also
// stays behind the operator token.
func TestBackupEndpointStreamsRestorableSnapshot(t *testing.T) {
	h := newHarness(t)

	// Put real data in the store first.
	h.postJSON(h.agent.URL, "/api/v1/events", agentEvent("e2e-backup-1", 79), http.StatusAccepted)
	h.waitForIncident("gpu-node-1", types.StateAwaitingApproval)

	// Unauthenticated download must be rejected.
	resp, err := http.Get(h.public.URL + "/api/v1/backup")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated backup: status %d, want 401", resp.StatusCode)
	}

	req, err := http.NewRequest(http.MethodGet, h.public.URL+"/api/v1/backup", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+operatorToken)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("backup: status %d", resp.StatusCode)
	}
	backupPath := filepath.Join(t.TempDir(), "restored.db")
	out, err := os.Create(backupPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(out, resp.Body); err != nil {
		t.Fatal(err)
	}
	if err := out.Close(); err != nil {
		t.Fatal(err)
	}

	restored, err := sqlite.Open(backupPath)
	if err != nil {
		t.Fatalf("restored snapshot must open cleanly: %v", err)
	}
	defer func() { _ = restored.Close() }()
	incidents, err := restored.ListIncidents(context.Background(), store.IncidentFilter{})
	if err != nil {
		t.Fatalf("listing incidents from restored snapshot: %v", err)
	}
	if len(incidents) == 0 {
		t.Fatal("restored snapshot contains no incidents")
	}
}
