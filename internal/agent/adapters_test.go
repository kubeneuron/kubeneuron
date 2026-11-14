// SPDX-License-Identifier: LicenseRef-KubeNeuron

package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kubeneuron/kubeneuron/internal/agent/nvml"
	"github.com/kubeneuron/kubeneuron/pkg/accelerator"
	"github.com/kubeneuron/kubeneuron/pkg/types"
)

// fakeSecondVendorAdapter is a registered non-NVIDIA adapter with no NVIDIA
// preflight: the agent must treat it as observation-only.
type fakeSecondVendorAdapter struct {
	node    string
	now     func() time.Time
	options map[string]string
	stream  chan accelerator.Detection
}

func (f *fakeSecondVendorAdapter) target(id string) accelerator.Target {
	return accelerator.Target{NodeName: f.node, Vendor: accelerator.VendorAMD, DeviceID: id, Scope: accelerator.ScopePhysicalDevice}
}

func (f *fakeSecondVendorAdapter) Inventory(context.Context) (accelerator.Inventory, error) {
	return accelerator.Inventory{
		NodeName:       f.node,
		Vendor:         accelerator.VendorAMD,
		DriverVersion:  "6.10.5",
		RuntimeVersion: "rocm-6.4",
		ObservedAt:     f.now(),
		Devices: []accelerator.Device{
			{ID: "amd-gpu-0", Kind: accelerator.DevicePhysical, Family: accelerator.FamilyGPU, Model: "MI300X"},
			{ID: "amd-gpu-1", Kind: accelerator.DevicePhysical, Family: accelerator.FamilyGPU, Model: "MI300X"},
		},
	}, nil
}

func (f *fakeSecondVendorAdapter) Capabilities(context.Context) (accelerator.CapabilitySet, error) {
	return accelerator.CapabilitySet{
		{Action: accelerator.ActionCollectDiagnostics, Scopes: []accelerator.TargetScope{accelerator.ScopeNode}},
		{Action: accelerator.ActionResetDevice, Scopes: []accelerator.TargetScope{accelerator.ScopePhysicalDevice}},
	}, nil
}

func (f *fakeSecondVendorAdapter) CheckHealth(_ context.Context, target accelerator.Target) (accelerator.HealthReport, error) {
	status := accelerator.HealthHealthy
	if target.DeviceID == "amd-gpu-1" {
		status = accelerator.HealthDegraded
	}
	return accelerator.HealthReport{Target: target, Status: status, CheckedAt: f.now()}, nil
}

func (f *fakeSecondVendorAdapter) Detect(context.Context) ([]accelerator.Detection, error) {
	return []accelerator.Detection{
		{
			ID: "poll-1", Target: f.target("amd-gpu-0"), Class: accelerator.DetectionMemory,
			Severity: accelerator.SeverityCritical, ObservedAt: f.now(),
			Evidence: map[string]string{"code": "page-retirement", "retired_pages": "3"},
		},
		// Another node's target must never become an event on this node.
		{Target: accelerator.Target{NodeName: "other-node", Vendor: accelerator.VendorAMD, Scope: accelerator.ScopeNode}, Class: accelerator.DetectionThermal},
	}, nil
}

func (f *fakeSecondVendorAdapter) WatchDetections(context.Context) (<-chan accelerator.Detection, error) {
	return f.stream, nil
}

func (f *fakeSecondVendorAdapter) Vendor() accelerator.Vendor { return accelerator.VendorAMD }

var (
	registerFakeAdaptersOnce sync.Once
	fakeAdapterMu            sync.Mutex
	fakeAdapter              *fakeSecondVendorAdapter
)

// registerFakeAdapters registers the fake AMD adapter, an Intel adapter whose
// build always fails, and a Google adapter whose hardware is never present on
// the node. The registry is process-global, so this runs once.
func registerFakeAdapters() {
	registerFakeAdaptersOnce.Do(func() {
		accelerator.Register(accelerator.VendorAMD, func(_ context.Context, cfg accelerator.Config) (accelerator.Adapter, error) {
			fakeAdapterMu.Lock()
			defer fakeAdapterMu.Unlock()
			fakeAdapter = &fakeSecondVendorAdapter{
				node: cfg.NodeName, now: cfg.Now, options: cfg.Options,
				stream: make(chan accelerator.Detection, 1),
			}
			return fakeAdapter, nil
		})
		accelerator.Register(accelerator.VendorIntel, func(context.Context, accelerator.Config) (accelerator.Adapter, error) {
			return nil, errors.New("level zero runtime not found")
		})
		accelerator.Register(accelerator.VendorGoogle, func(context.Context, accelerator.Config) (accelerator.Adapter, error) {
			return nil, fmt.Errorf("no /dev/accel* devices: %w", accelerator.ErrNotPresent)
		})
	})
}

type capturedPosts struct {
	mu      sync.Mutex
	reports []types.AgentAcceleratorReport
	events  []types.AgentEvent
}

func (c *capturedPosts) handler(t *testing.T) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		c.mu.Lock()
		defer c.mu.Unlock()
		switch {
		case r.Method == http.MethodPost && r.URL.Path == types.AgentAcceleratorReportPath:
			var report types.AgentAcceleratorReport
			if err := json.Unmarshal(body, &report); err != nil {
				t.Errorf("decode report: %v", err)
			}
			c.reports = append(c.reports, report)
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/events":
			var event types.AgentEvent
			if err := json.Unmarshal(body, &event); err != nil {
				t.Errorf("decode event: %v", err)
			}
			c.events = append(c.events, event)
			w.WriteHeader(http.StatusAccepted)
		default:
			http.NotFound(w, r)
		}
	}
}

func (c *capturedPosts) report(vendor types.AcceleratorVendor) (types.AgentAcceleratorReport, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, report := range c.reports {
		if report.Vendor == vendor {
			return report, true
		}
	}
	return types.AgentAcceleratorReport{}, false
}

func (c *capturedPosts) eventCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.events)
}

func TestAgentBuildsAndDrivesASecondVendorAdapter(t *testing.T) {
	registerFakeAdapters()
	captured := &capturedPosts{}
	controller := httptest.NewServer(captured.handler(t))
	defer controller.Close()

	clock := &testClock{now: time.Date(2026, 11, 12, 9, 0, 0, 0, time.UTC)}
	a, err := New(Config{
		NodeName:               "gpu-node-1",
		ControllerURL:          controller.URL,
		AllowInsecureHTTP:      true,
		SpoolPath:              t.TempDir() + "/spool.jsonl",
		ActionJournalPath:      t.TempDir() + "/actions.jsonl",
		HealthListenAddress:    "127.0.0.1:0",
		RegistrationInterval:   10 * time.Second,
		RegistrationStaleAfter: 30 * time.Second,
		Adapters: map[accelerator.Vendor]AdapterConfig{
			accelerator.VendorAMD:   {Enabled: true, Options: map[string]string{"tool": "amd-smi"}},
			accelerator.VendorIntel: {Enabled: true},
			"acme":                  {Enabled: true},
		},
	}, &nvml.Fake{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("New() must not fail when one adapter fails to build: %v", err)
	}
	a.now = clock.Now

	// The agent built the registered second-vendor adapter with its options,
	// and recorded the failed and unregistered vendors without disabling it.
	byVendor := map[accelerator.Vendor]*adapterRuntime{}
	for _, rt := range a.adapters {
		byVendor[rt.vendor] = rt
	}
	if rt := byVendor[accelerator.VendorAMD]; rt == nil || rt.adapter == nil || rt.preflight {
		t.Fatalf("AMD adapter not built as observation-only: %+v", rt)
	}
	fakeAdapterMu.Lock()
	options := fakeAdapter.options
	fakeAdapterMu.Unlock()
	if options["tool"] != "amd-smi" {
		t.Fatalf("adapter options not passed to factory: %v", options)
	}
	for _, vendor := range []accelerator.Vendor{accelerator.VendorIntel, "acme"} {
		if rt := byVendor[vendor]; rt == nil || rt.adapter != nil || rt.buildErr == nil {
			t.Fatalf("%s must fail closed with a recorded build error: %+v", vendor, rt)
		}
	}
	if a.nvidia != nil {
		t.Fatal("no NVIDIA observation was configured")
	}

	// Inventory, capabilities, and health flow into one report per vendor.
	a.reportAdapters(context.Background())
	amd, ok := captured.report(types.AcceleratorVendorAMD)
	if !ok {
		t.Fatal("no AMD accelerator report posted")
	}
	if amd.Node != "gpu-node-1" || len(amd.Devices) != 2 || amd.Devices[0].ID != "amd-gpu-0" {
		t.Fatalf("AMD report does not carry the adapter inventory: %+v", amd)
	}
	if amd.Readiness != types.AcceleratorReadinessDegraded || amd.DriverVersion != "6.10.5" {
		t.Fatalf("observation-only adapter must report degraded with its versions: %+v", amd)
	}
	for _, capability := range amd.Capabilities {
		if capability.Action == types.AcceleratorActionResetDevice {
			t.Fatalf("physical reset must be withheld without a preflight: %+v", amd.Capabilities)
		}
	}
	if len(amd.Capabilities) != 1 || amd.Capabilities[0].Action != types.AcceleratorActionCollectDiagnostics {
		t.Fatalf("declared non-reset capabilities must be reported: %+v", amd.Capabilities)
	}
	if !containsReason(amd.ReadinessReasons, "amd device amd-gpu-1 health is degraded") {
		t.Fatalf("health result missing from reasons: %v", amd.ReadinessReasons)
	}
	intel, ok := captured.report(types.AcceleratorVendorIntel)
	if !ok || intel.Readiness != types.AcceleratorReadinessNotReady {
		t.Fatalf("failed Intel adapter must post a not-ready report: %+v (found %v)", intel, ok)
	}

	// A polled detection becomes a vendor-neutral fault event; the foreign
	// node's detection is dropped.
	a.pollAdapterDetections(context.Background())
	captured.mu.Lock()
	events := append([]types.AgentEvent(nil), captured.events...)
	captured.mu.Unlock()
	if len(events) != 1 {
		t.Fatalf("polled detections posted %d events, want 1: %+v", len(events), events)
	}
	event := events[0]
	if event.Node != "gpu-node-1" || event.XID != 0 || event.Fault == nil ||
		event.Fault.Vendor != "amd" || event.Fault.Code != "page-retirement" ||
		event.GPUUUID != "amd-gpu-0" || event.Fault.Attributes["retired_pages"] != "3" {
		t.Fatalf("detection not converted to a fault event: %+v (fault %+v)", event, event.Fault)
	}

	// A streamed detection reaches the Run loop through the merged channel.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream := a.watchAdapterDetections(ctx)
	if stream == nil {
		t.Fatal("adapter detection stream was not started")
	}
	fakeAdapterMu.Lock()
	fake := fakeAdapter
	fakeAdapterMu.Unlock()
	fake.stream <- accelerator.Detection{
		Target: fake.target("amd-gpu-1"), Class: accelerator.DetectionThermal,
		Severity: accelerator.SeverityWarning, ObservedAt: clock.Now(),
	}
	select {
	case got := <-stream:
		if got.source != "adapter-amd" || got.event.Fault == nil || got.event.Fault.Code != "thermal" {
			t.Fatalf("streamed detection = %+v", got)
		}
		a.handleDetection(ctx, got.event, got.source)
	case <-time.After(5 * time.Second):
		t.Fatal("streamed detection not delivered")
	}
	if got := captured.eventCount(); got != 2 {
		t.Fatalf("events after streamed detection = %d, want 2", got)
	}
}

func TestAgentSkipsAnAdapterWhoseHardwareIsNotPresent(t *testing.T) {
	registerFakeAdapters()
	captured := &capturedPosts{}
	controller := httptest.NewServer(captured.handler(t))
	defer controller.Close()

	var logs strings.Builder
	var logMu sync.Mutex
	a, err := New(Config{
		NodeName:               "gpu-node-1",
		ControllerURL:          controller.URL,
		AllowInsecureHTTP:      true,
		SpoolPath:              t.TempDir() + "/spool.jsonl",
		ActionJournalPath:      t.TempDir() + "/actions.jsonl",
		HealthListenAddress:    "127.0.0.1:0",
		RegistrationInterval:   10 * time.Second,
		RegistrationStaleAfter: 30 * time.Second,
		Adapters: map[accelerator.Vendor]AdapterConfig{
			accelerator.VendorAMD:    {Enabled: true},
			accelerator.VendorGoogle: {Enabled: true},
		},
	}, &nvml.Fake{}, slog.New(slog.NewTextHandler(lockedWriter{w: &logs, mu: &logMu}, nil)))
	if err != nil {
		t.Fatalf("New() = %v", err)
	}

	for _, rt := range a.adapters {
		if rt.vendor == accelerator.VendorGoogle {
			t.Fatalf("a vendor that is not present must not be kept as a runtime: %+v", rt)
		}
	}
	// Several ticks: the absent vendor never reports, never warns, and the
	// present vendor keeps running.
	for i := 0; i < 3; i++ {
		a.reportAdapters(context.Background())
		a.pollAdapterDetections(context.Background())
	}
	if _, ok := captured.report(types.AcceleratorVendorGoogle); ok {
		t.Fatal("a vendor that is not present must post no accelerator report")
	}
	if _, ok := captured.report(types.AcceleratorVendorAMD); !ok {
		t.Fatal("the present vendor stopped reporting")
	}
	// The repeated detection is deduplicated by the agent; one event shows
	// the present vendor's detections still flow.
	if got := captured.eventCount(); got != 1 {
		t.Fatalf("present vendor detection events = %d, want 1", got)
	}

	logMu.Lock()
	output := logs.String()
	logMu.Unlock()
	if n := strings.Count(output, "vendor=google"); n != 1 {
		t.Fatalf("absent vendor logged %d lines, want exactly one start-up line:\n%s", n, output)
	}
	if !strings.Contains(output, "level=INFO msg=\"accelerator adapter skipped: vendor not present on this node\" vendor=google") {
		t.Fatalf("absent vendor start-up line missing or not at info level:\n%s", output)
	}
}

// lockedWriter serializes log writes from the agent's goroutines.
type lockedWriter struct {
	w  io.Writer
	mu *sync.Mutex
}

func (l lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

func containsReason(reasons []string, want string) bool {
	for _, reason := range reasons {
		if reason == want {
			return true
		}
	}
	return false
}
