package agent

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kubeneuron/kubeneuron/internal/agent/kmsg"
	"github.com/kubeneuron/kubeneuron/internal/agent/nvml"
	"github.com/kubeneuron/kubeneuron/internal/metrics"
	"github.com/kubeneuron/kubeneuron/pkg/types"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

// amdAgentConfig is the wiring under test: the AMD source declared enabled,
// pointed at a binary that either exists or does not.
func amdAgentConfig(t *testing.T, amdSMI, rocmSMI string) Config {
	t.Helper()
	return Config{
		NodeName:               "gpu-node-1",
		ControllerURL:          "http://controller.invalid",
		AllowInsecureHTTP:      true,
		SpoolPath:              t.TempDir() + "/spool.jsonl",
		ActionJournalPath:      t.TempDir() + "/actions.jsonl",
		HealthListenAddress:    "127.0.0.1:0",
		RegistrationInterval:   10 * time.Second,
		RegistrationStaleAfter: 30 * time.Second,
		AMDDetection: AMDDetectionConfig{
			Enabled: true, AMDSMIPath: amdSMI, ROCmSMIPath: rocmSMI, ThermalCriticalC: 105,
		},
	}
}

// TestAMDSourceRequiresRealTooling is the evidence gate: declaring the source in
// configuration is not enough, because a declaration cannot make a node have an
// AMD accelerator. Without the binaries the source stays unwired and the refusal
// is logged, rather than a permanently silent watcher masquerading as coverage.
func TestAMDSourceRequiresRealTooling(t *testing.T) {
	var logged strings.Builder
	cfg := amdAgentConfig(t, t.TempDir()+"/nope-amd-smi", t.TempDir()+"/nope-rocm-smi")
	a, err := New(cfg, &nvml.Fake{}, slog.New(slog.NewTextHandler(&logged, nil)))
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	if a.amdHealth != nil {
		t.Fatal("the AMD source must stay unwired when neither amd-smi nor rocm-smi exists")
	}
	if !strings.Contains(logged.String(), "AMD detection requested but disabled") {
		t.Fatalf("the refusal must be visible to an operator; log was: %s", logged.String())
	}
}

// TestAMDSourceWiredWhenToolingIsPresent uses /bin/sh as a stand-in for a real
// binary: the gate is "a tool actually resolves on this node", nothing more.
// It also pins that the source is independent of the NVIDIA driver — an AMD node
// runs no nvidia-smi, so gating on it would kill the source exactly where it is
// needed.
func TestAMDSourceWiredWhenToolingIsPresent(t *testing.T) {
	cfg := amdAgentConfig(t, "/bin/sh", "")
	a, err := New(cfg, &nvml.Fake{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	if a.amdHealth == nil {
		t.Fatal("a present AMD tool must enable the source even on the fake NVIDIA driver")
	}
	if a.amdHealth.NodeName != "gpu-node-1" {
		t.Fatalf("AMD source node = %q, want gpu-node-1", a.amdHealth.NodeName)
	}
	if a.amdHealth.ThermalCriticalC != 105 {
		t.Fatalf("thermal threshold = %v, want the configured 105", a.amdHealth.ThermalCriticalC)
	}
}

// TestAMDSourceOffByDefault keeps a fleet of NVIDIA nodes from spawning AMD
// subprocesses because a tool happens to be installed in the image.
func TestAMDSourceOffByDefault(t *testing.T) {
	clock := &testClock{now: time.Now()}
	a := newTestAgent(t, "http://controller.invalid", clock, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if a.amdHealth != nil {
		t.Fatal("the AMD source must be off unless explicitly enabled")
	}
}

// TestAMDKernelFaultIsNeverAttributedByTheNVIDIAInventory is the fail-closed
// attribution rule. The only inventory this agent has is NVML's; resolving an
// AMD PCI address through it could only produce a wrong device, and an AMD fault
// filed against an NVIDIA GPU would aim every downstream decision at the wrong
// hardware.
func TestAMDKernelFaultIsNeverAttributedByTheNVIDIAInventory(t *testing.T) {
	var posted int64
	controller := eventCountingController(t, &posted)
	defer controller.Close()

	clock := &testClock{now: time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)}
	var logged strings.Builder
	a := newTestAgent(t, controller.URL, clock, slog.New(slog.NewTextHandler(&logged, nil)))
	// The fake NVIDIA driver would happily answer for index 0 — that is exactly
	// the wrong answer this path must not take.
	a.driver = &nvml.Fake{GPUs: []types.GPUInfo{{Index: 0, UUID: "GPU-nvidia-0", Model: "test"}}}

	ev, ok := kmsg.ParseLine(`6,900,100;amdgpu 0000:c3:00.0: amdgpu: ring gfx_0.0.0 timeout, signaled seq=1, emitted seq=2`)
	if !ok {
		t.Fatal("fixture line must parse as an amdgpu fault")
	}
	if !a.handleKernelEvent(context.Background(), ev) {
		t.Fatal("the amdgpu fault must be delivered")
	}
	if atomic.LoadInt64(&posted) != 1 {
		t.Fatalf("posted = %d, want 1", posted)
	}
	if !strings.Contains(logged.String(), "no inventory for that vendor") {
		t.Fatalf("the attribution limit must be visible to an operator; log was: %s", logged.String())
	}
}

// TestDetectionSourceLabelsDistinguishTheVendorPaths is the observability
// requirement: an operator must be able to see WHICH source is reporting, or a
// dead AMD source is indistinguishable from a healthy AMD fleet.
func TestDetectionSourceLabelsDistinguishTheVendorPaths(t *testing.T) {
	var posted int64
	controller := eventCountingController(t, &posted)
	defer controller.Close()

	clock := &testClock{now: time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)}
	a := newTestAgent(t, controller.URL, clock, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx := context.Background()

	before := map[string]float64{}
	for _, source := range []string{"kmsg", "kmsg-amd", "amdhealth"} {
		before[source] = testutil.ToFloat64(metrics.AgentDetections.WithLabelValues(source))
	}

	kmsgXID, _ := kmsg.ParseLine(`6,901,100;NVRM: Xid (PCI:0000:3b:00): 48, pid=1, Ch 00000008`)
	a.handleKernelEvent(ctx, kmsgXID)
	amdKernel, _ := kmsg.ParseLine(`6,902,101;amdgpu 0000:c3:00.0: amdgpu: 2 uncorrectable hardware errors detected in umc block`)
	a.handleKernelEvent(ctx, amdKernel)
	a.handleDetection(ctx, types.AgentEvent{
		Node: "gpu-node-1", GPUIndex: 0, GPUUUID: "GPU-amd-0",
		Fault: &types.FaultSignal{Vendor: "amd", Source: "amd-smi", Code: "page-retirement"},
	}, "amdhealth")

	for _, source := range []string{"kmsg", "kmsg-amd", "amdhealth"} {
		got := testutil.ToFloat64(metrics.AgentDetections.WithLabelValues(source))
		if got-before[source] != 1 {
			t.Errorf("kubeneuron_agent_detections_total{source=%q} rose by %v, want 1", source, got-before[source])
		}
	}
}

// TestAMDKernelAndPollFaultForOneDeviceDeliverTheAttributedObservation closes
// the loop between the two new sources: the kernel line and the amd-smi poll
// observe ONE physical fault on one device, and that must end as ONE incident —
// but the collapse belongs to the controller, which can promote the incident
// onto the resolved device, not to this window, which can only discard.
//
// This test previously asserted that the agent suppressed the amd-smi
// observation. That is the defect: the kernel line names the device only by
// BDF, so the incident it opens has an empty GPU UUID, and an empty GPU UUID is
// read downstream as a permanent infeasibility. Dropping the one observation
// that carried the UUID meant the node was cordoned, drained of every tenant
// job, refused its reset and parked for a human — for a device amd-smi had
// named seconds earlier.
func TestAMDKernelAndPollFaultForOneDeviceDeliverTheAttributedObservation(t *testing.T) {
	var posted int64
	controller := eventCountingController(t, &posted)
	defer controller.Close()

	clock := &testClock{now: time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)}
	a := newTestAgent(t, controller.URL, clock, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx := context.Background()

	// The kernel reports it first, unattributed (this agent has no AMD
	// inventory) but addressed by PCI.
	kernel, _ := kmsg.ParseLine(`6,903,102;amdgpu 0000:c3:00.0: amdgpu: 2 uncorrectable hardware errors detected in umc block`)
	a.handleKernelEvent(ctx, kernel)
	// The amd-smi poll sees the same ECC counter move seconds later and CAN
	// attribute it, because amd-smi reports the UUID and the same BDF.
	precise := types.AgentEvent{
		Node: "gpu-node-1", GPUIndex: 0, GPUUUID: "amd-gpu-0", PCIAddr: "0000:c3:00.0",
		Fault: &types.FaultSignal{Vendor: "amd", Source: "amd-smi", Code: "ecc-uncorrectable"},
	}
	a.handleDetection(ctx, precise, "amdhealth")

	if got := atomic.LoadInt64(&posted); got != 2 {
		t.Fatalf("posted = %d, want 2: the amd-smi observation carrying the device UUID was suppressed as a "+
			"duplicate of the kernel line that could only name a BDF, so the incident stays unattributed and the "+
			"node is cordoned, drained and parked for a human although amd-smi named the exact device seconds later", got)
	}

	// The promotion is delivered ONCE. A repeat of the attributed observation
	// inside the window is a genuine duplicate and must still be suppressed, or
	// every amd-smi poll re-posts the same fault for as long as it persists.
	a.handleDetection(ctx, precise, "amdhealth")
	if got := atomic.LoadInt64(&posted); got != 2 {
		t.Fatalf("posted = %d, want 2: a repeat of the SAME attributed observation is a duplicate and must stay "+
			"suppressed; letting the promotion re-post on every poll turns a persistent fault into an event storm", got)
	}
}
