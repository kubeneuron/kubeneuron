package agent

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kubeneuron/kubeneuron/internal/agent/nvml"
	"github.com/kubeneuron/kubeneuron/internal/agent/spool"
	"github.com/kubeneuron/kubeneuron/pkg/types"
)

// eventCountingController accepts /api/v1/events and counts them.
func eventCountingController(t *testing.T, count *int64) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if serveRegistrationCapability(w, r) {
			return
		}
		if r.URL.Path == "/api/v1/events" && r.Method == http.MethodPost {
			atomic.AddInt64(count, 1)
			w.WriteHeader(http.StatusNoContent)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
}

func TestHandleDetectionDeduplicatesAcrossSources(t *testing.T) {
	var posted int64
	controller := eventCountingController(t, &posted)
	defer controller.Close()

	clock := &testClock{now: time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)}
	a := newTestAgent(t, controller.URL, clock, slog.New(slog.NewTextHandler(io.Discard, nil)))

	ctx := context.Background()
	base := types.AgentEvent{Node: "gpu-node-1", GPUIndex: 1, GPUUUID: "GPU-x", XID: 48, Raw: "kmsg"}

	// kmsg observes the fault first.
	a.handleDetection(ctx, base, "kmsg")
	// The DCGM poll observes the same underlying fault moments later.
	dcgm := base
	dcgm.Raw = "DCGM"
	a.handleDetection(ctx, dcgm, "gpuhealth")

	if got := atomic.LoadInt64(&posted); got != 1 {
		t.Fatalf("posted = %d, want 1 (same fault from two sources deduplicated)", got)
	}

	// A different XID on the same GPU is a distinct fault and is posted.
	other := base
	other.XID = 79
	a.handleDetection(ctx, other, "gpuhealth")
	if got := atomic.LoadInt64(&posted); got != 2 {
		t.Fatalf("posted = %d, want 2 (distinct XID)", got)
	}

	// After the dedup window elapses, a recurrence is a fresh signal again.
	clock.Advance(detectionDedupWindow + time.Second)
	a.handleDetection(ctx, base, "kmsg")
	if got := atomic.LoadInt64(&posted); got != 3 {
		t.Fatalf("posted = %d, want 3 (recurrence past dedup window)", got)
	}
}

// TestUnresolvedKmsgAndResolvedDCGMDedupToOneIncident is the regression test
// for the cross-source key mismatch (the XID 79 case). When the GPU falls off
// the bus it vanishes from nvidia-smi, so the fast kmsg path cannot resolve it
// (index -1, empty UUID) and reports a node-scoped observation, while the later
// DCGM poll resolves the same fault to a specific GPU. With a precise-only key
// the two miss each other and two incidents open for one fault; the node+XID
// fallback collapses them.
func TestUnresolvedKmsgAndResolvedDCGMDedupToOneIncident(t *testing.T) {
	var posted int64
	controller := eventCountingController(t, &posted)
	defer controller.Close()

	clock := &testClock{now: time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)}
	a := newTestAgent(t, controller.URL, clock, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx := context.Background()

	// kmsg sees XID 79 first but cannot resolve the vanished device.
	kmsgEv := types.AgentEvent{Node: "gpu-node-1", GPUIndex: -1, GPUUUID: "", XID: 79, Raw: "kmsg"}
	a.handleDetection(ctx, kmsgEv, "kmsg")
	// The DCGM poll observes the same fault moments later and DOES resolve it.
	dcgmEv := types.AgentEvent{Node: "gpu-node-1", GPUIndex: 3, GPUUUID: "GPU-abc", XID: 79, Raw: "DCGM"}
	a.handleDetection(ctx, dcgmEv, "gpuhealth")

	if got := atomic.LoadInt64(&posted); got != 1 {
		t.Fatalf("posted = %d, want 1 (one fault seen by two sources is one incident)", got)
	}

	// A different XID on the same node is a distinct fault and still posts.
	other := dcgmEv
	other.XID = 48
	a.handleDetection(ctx, other, "gpuhealth")
	if got := atomic.LoadInt64(&posted); got != 2 {
		t.Fatalf("posted = %d, want 2 (distinct XID on the node)", got)
	}
}

// TestDistinctGPUsSameXIDAreNotOverCollapsed guards the flip side of Fix 4: two
// attributed faults on different GPUs of one node sharing an XID must NOT be
// folded together by the node+XID fallback. The precise per-GPU key keeps them
// separate whenever both sides carry a UUID.
func TestDistinctGPUsSameXIDAreNotOverCollapsed(t *testing.T) {
	var posted int64
	controller := eventCountingController(t, &posted)
	defer controller.Close()

	clock := &testClock{now: time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)}
	a := newTestAgent(t, controller.URL, clock, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx := context.Background()

	gpu3 := types.AgentEvent{Node: "gpu-node-1", GPUIndex: 3, GPUUUID: "GPU-a", XID: 79}
	gpu4 := types.AgentEvent{Node: "gpu-node-1", GPUIndex: 4, GPUUUID: "GPU-b", XID: 79}
	a.handleDetection(ctx, gpu3, "gpuhealth")
	a.handleDetection(ctx, gpu4, "gpuhealth")

	if got := atomic.LoadInt64(&posted); got != 2 {
		t.Fatalf("posted = %d, want 2 (distinct GPUs must not collapse on node+XID)", got)
	}
}

// TestDistinctUnattributedGPUsByPCIAreNotCollapsed is the regression test for
// the fallback dedup key dropping the PCI address. When two distinct GPUs on one
// node fall off the bus with the same XID, both are unattributed (index -1,
// empty UUID) so neither carries a per-GPU key — but each kmsg line DOES carry
// its own PCI address. Folding the PCI address into the fallback key keeps the
// two faults distinct; the old node+XID-only key collapsed them into a single
// detection and lost one GPU's fault.
func TestDistinctUnattributedGPUsByPCIAreNotCollapsed(t *testing.T) {
	var posted int64
	controller := eventCountingController(t, &posted)
	defer controller.Close()

	clock := &testClock{now: time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)}
	a := newTestAgent(t, controller.URL, clock, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx := context.Background()

	gpuA := types.AgentEvent{Node: "gpu-node-1", GPUIndex: -1, GPUUUID: "", PCIAddr: "0000:3b:00.0", XID: 79}
	gpuB := types.AgentEvent{Node: "gpu-node-1", GPUIndex: -1, GPUUUID: "", PCIAddr: "0000:86:00.0", XID: 79}
	a.handleDetection(ctx, gpuA, "kmsg")
	a.handleDetection(ctx, gpuB, "kmsg")
	if got := atomic.LoadInt64(&posted); got != 2 {
		t.Fatalf("posted = %d, want 2 (distinct unattributed GPUs must not collapse on node+XID)", got)
	}

	// The same GPU recurring within the window is still one detection.
	a.handleDetection(ctx, gpuA, "kmsg")
	if got := atomic.LoadInt64(&posted); got != 2 {
		t.Fatalf("posted = %d, want 2 (a repeat of the same PCI address is a duplicate)", got)
	}

	// A later ATTRIBUTED observation of gpuA's fault (the DCGM poll resolves an
	// index/UUID but not the bus address) must still collapse via the coarse
	// node+XID cross-source anchor rather than opening a second incident.
	dcgmA := types.AgentEvent{Node: "gpu-node-1", GPUIndex: 3, GPUUUID: "GPU-a", XID: 79}
	a.handleDetection(ctx, dcgmA, "gpuhealth")
	if got := atomic.LoadInt64(&posted); got != 2 {
		t.Fatalf("posted = %d, want 2 (attributed observation of a known node fault is a duplicate)", got)
	}
}

// TestKmsgXIDAndNeutralFaultDedupToOneIncident is the R6 addition: after the
// nvidia-smi fallback stopped synthesizing XIDs, a kmsg XID 48 and an nvidia-smi
// neutral "ecc-dbe" for the same fault on the same GPU must still collapse to
// one incident. The dedup identity is the shared ProblemClass, not the raw XID,
// so the two encodings of one condition share one key.
func TestKmsgXIDAndNeutralFaultDedupToOneIncident(t *testing.T) {
	var posted int64
	controller := eventCountingController(t, &posted)
	defer controller.Close()

	clock := &testClock{now: time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)}
	a := newTestAgent(t, controller.URL, clock, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx := context.Background()

	// kmsg sees the genuine XID 48 (double-bit ECC).
	kmsgEv := types.AgentEvent{Node: "gpu-node-1", GPUIndex: 2, GPUUUID: "GPU-x", XID: 48, Raw: "kmsg"}
	a.handleDetection(ctx, kmsgEv, "kmsg")
	// The nvidia-smi poll observes the same fault as a neutral NVIDIA fault.
	smiEv := types.AgentEvent{Node: "gpu-node-1", GPUIndex: 2, GPUUUID: "GPU-x",
		Fault: &types.FaultSignal{Vendor: "nvidia", Source: "nvidia-smi", Code: "ecc-dbe"}, Raw: "nvidia-smi"}
	a.handleDetection(ctx, smiEv, "gpuhealth")

	if got := atomic.LoadInt64(&posted); got != 1 {
		t.Fatalf("posted = %d, want 1 (XID and neutral fault for one condition dedup)", got)
	}

	// A neutral row-remap-failure on the same GPU is a distinct fault (a
	// different ProblemClass) and must still post.
	remapEv := types.AgentEvent{Node: "gpu-node-1", GPUIndex: 2, GPUUUID: "GPU-x",
		Fault: &types.FaultSignal{Vendor: "nvidia", Source: "nvidia-smi", Code: "row-remap-failure"}}
	a.handleDetection(ctx, remapEv, "gpuhealth")
	if got := atomic.LoadInt64(&posted); got != 2 {
		t.Fatalf("posted = %d, want 2 (distinct neutral fault class)", got)
	}
}

// TestDistinctXIDsSharingAClassPostTwice is the regression test for
// ProblemClass-keyed dedup over-collapsing genuinely distinct same-source faults.
// XID 13 and XID 31 both map to ClassXIDApp; once the dedup key preferred the
// class, a second distinct app-XID within the window was suppressed, undercounting
// the threshold-gated escalation. The precise dedup key must be source-native, so
// two distinct XIDs on one GPU each post — while a kmsg XID and the nvidia-smi
// neutral fault for ONE condition still dedup across sources via the class anchor.
func TestDistinctXIDsSharingAClassPostTwice(t *testing.T) {
	var posted int64
	controller := eventCountingController(t, &posted)
	defer controller.Close()

	clock := &testClock{now: time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)}
	a := newTestAgent(t, controller.URL, clock, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx := context.Background()

	// Two distinct app-XIDs on one GPU, 30s apart, both ClassXIDApp. They are
	// genuinely distinct evidence and must each post.
	xid13 := types.AgentEvent{Node: "gpu-node-1", GPUIndex: 2, GPUUUID: "GPU-x", XID: 13, Raw: "kmsg"}
	a.handleDetection(ctx, xid13, "kmsg")
	clock.Advance(30 * time.Second)
	xid31 := types.AgentEvent{Node: "gpu-node-1", GPUIndex: 2, GPUUUID: "GPU-x", XID: 31, Raw: "kmsg"}
	a.handleDetection(ctx, xid31, "kmsg")

	if got := atomic.LoadInt64(&posted); got != 2 {
		t.Fatalf("posted = %d, want 2 (distinct XIDs 13 and 31 must not collapse on a shared class)", got)
	}

	// Cross-source dedup of ONE condition still holds: a kmsg XID 48 and an
	// nvidia-smi neutral "ecc-dbe" for the same GPU dedup to one incident.
	var posted2 int64
	controller2 := eventCountingController(t, &posted2)
	defer controller2.Close()
	b := newTestAgent(t, controller2.URL, clock, slog.New(slog.NewTextHandler(io.Discard, nil)))

	xid48 := types.AgentEvent{Node: "gpu-node-1", GPUIndex: 2, GPUUUID: "GPU-x", XID: 48, Raw: "kmsg"}
	b.handleDetection(ctx, xid48, "kmsg")
	smiECC := types.AgentEvent{Node: "gpu-node-1", GPUIndex: 2, GPUUUID: "GPU-x",
		Fault: &types.FaultSignal{Vendor: "nvidia", Source: "nvidia-smi", Code: "ecc-dbe"}, Raw: "nvidia-smi"}
	b.handleDetection(ctx, smiECC, "gpuhealth")
	if got := atomic.LoadInt64(&posted2); got != 1 {
		t.Fatalf("posted = %d, want 1 (kmsg XID 48 and nvidia-smi ecc-dbe are one condition)", got)
	}
}

// newTestAgentWithGPUs builds a test agent whose node inventory has the given
// GPUs, so the single-vs-multi-GPU dedup gate can be exercised.
func newTestAgentWithGPUs(t *testing.T, controllerURL string, clock *testClock, gpus []types.GPUInfo) *Agent {
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
	}, &nvml.Fake{GPUs: gpus}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	a.now = clock.Now
	return a
}

// TestAttributedFaultNotSuppressedByAnotherGPUsUnattributedXID is the regression
// for the coarse node+XID cross-source anchor suppressing a real fault on a
// multi-GPU node. GPU1's unattributed kmsg XID 79 must NOT suppress GPU0's later
// ATTRIBUTED XID 79 — node+XID cannot tell the two GPUs apart, so on a multi-GPU
// node the coarse anchor must not collapse them, or GPU0's fault never reaches
// the controller. The single-GPU unattributed->attributed collapse (which the
// coarse anchor legitimately serves) must still work.
func TestAttributedFaultNotSuppressedByAnotherGPUsUnattributedXID(t *testing.T) {
	t.Run("multi-GPU node does not over-suppress across devices", func(t *testing.T) {
		var posted int64
		controller := eventCountingController(t, &posted)
		defer controller.Close()

		clock := &testClock{now: time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)}
		a := newTestAgentWithGPUs(t, controller.URL, clock, []types.GPUInfo{
			{Index: 0, UUID: "GPU-0"},
			{Index: 1, UUID: "GPU-1"},
		})
		ctx := context.Background()

		// GPU1 falls off the bus: kmsg sees XID 79 but cannot resolve it, carrying
		// only GPU1's PCI address.
		gpu1Unattributed := types.AgentEvent{Node: "gpu-node-1", GPUIndex: -1, GPUUUID: "", PCIAddr: "0000:86:00.0", XID: 79, Raw: "kmsg"}
		a.handleDetection(ctx, gpu1Unattributed, "kmsg")
		// GPU0 then has its OWN XID 79, which the DCGM poll resolves to GPU0 (no
		// PCI address). This is a different physical device and must still post.
		gpu0Attributed := types.AgentEvent{Node: "gpu-node-1", GPUIndex: 0, GPUUUID: "GPU-0", XID: 79, Raw: "DCGM"}
		a.handleDetection(ctx, gpu0Attributed, "gpuhealth")

		if got := atomic.LoadInt64(&posted); got != 2 {
			t.Fatalf("posted = %d, want 2 (GPU0's attributed XID must not be suppressed by GPU1's unattributed XID)", got)
		}
	})

	t.Run("single-GPU node still collapses unattributed into attributed", func(t *testing.T) {
		var posted int64
		controller := eventCountingController(t, &posted)
		defer controller.Close()

		clock := &testClock{now: time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)}
		a := newTestAgentWithGPUs(t, controller.URL, clock, []types.GPUInfo{{Index: 0, UUID: "GPU-0"}})
		ctx := context.Background()

		// The one GPU falls off the bus (unattributed kmsg), then the DCGM poll
		// resolves the same fault. On a single-GPU node node+XID is unambiguous, so
		// these are one incident.
		unattributed := types.AgentEvent{Node: "gpu-node-1", GPUIndex: -1, GPUUUID: "", PCIAddr: "0000:3b:00.0", XID: 79, Raw: "kmsg"}
		a.handleDetection(ctx, unattributed, "kmsg")
		attributed := types.AgentEvent{Node: "gpu-node-1", GPUIndex: 0, GPUUUID: "GPU-0", XID: 79, Raw: "DCGM"}
		a.handleDetection(ctx, attributed, "gpuhealth")

		if got := atomic.LoadInt64(&posted); got != 1 {
			t.Fatalf("posted = %d, want 1 (single-GPU unattributed->attributed collapse must still work)", got)
		}
	})
}

func TestHandleDetectionAssignsEventID(t *testing.T) {
	var posted int64
	controller := eventCountingController(t, &posted)
	defer controller.Close()
	clock := &testClock{now: time.Now()}
	a := newTestAgent(t, controller.URL, clock, slog.New(slog.NewTextHandler(io.Discard, nil)))

	// An event arriving from the second source without an EventID must get one
	// so the controller can deduplicate at-least-once spool replays.
	a.handleDetection(context.Background(), types.AgentEvent{Node: "gpu-node-1", GPUIndex: 0, XID: 79}, "gpuhealth")
	if atomic.LoadInt64(&posted) != 1 {
		t.Fatalf("event without EventID was not posted")
	}
}

func TestFailedDeliveryIsNotDeduplicatedBeforeItReachesDurableStorage(t *testing.T) {
	clock := &testClock{now: time.Now()}
	a := newTestAgent(t, "http://127.0.0.1:1", clock, slog.New(slog.NewTextHandler(io.Discard, nil)))
	// Force both the network send and spool append to fail. The next copy of
	// the fault must be retried rather than being hidden by cross-source dedup.
	a.spool = &spool.Spool{}
	ev := types.AgentEvent{Node: "gpu-node-1", GPUIndex: 0, GPUUUID: "GPU-x", XID: 79}
	if a.handleDetection(context.Background(), ev, "kmsg") {
		t.Fatal("failed delivery must not report durable acceptance")
	}
	if a.duplicateDetection(ev, clock.Now(), false) {
		t.Fatal("failed delivery must not enter the detection dedup set")
	}
}

func TestFakeDriverGetsNoSecondSourceButKmsgCursorIsSet(t *testing.T) {
	clock := &testClock{now: time.Now()}
	a := newTestAgent(t, "http://controller.invalid", clock, slog.New(slog.NewTextHandler(io.Discard, nil)))
	// Mirrors --require-real-driver: the Fake driver must never fabricate a
	// second detection source.
	if a.health != nil {
		t.Fatal("fake driver must not get a DCGM/nvidia-smi second source")
	}
	// The kmsg tail-seek loss fix is always wired: a durable cursor beside the
	// spool.
	if a.watcher.CursorPath == "" || filepath.Base(a.watcher.CursorPath) != "kmsg-cursor.json" {
		t.Fatalf("kmsg cursor path = %q, want a kmsg-cursor.json beside the spool", a.watcher.CursorPath)
	}
}

func TestRealSMIDriverGetsSecondSource(t *testing.T) {
	clock := &testClock{now: time.Now()}
	a, err := New(Config{
		NodeName:               "gpu-node-1",
		ControllerURL:          "http://controller.invalid",
		AllowInsecureHTTP:      true,
		SpoolPath:              t.TempDir() + "/spool.jsonl",
		ActionJournalPath:      t.TempDir() + "/actions.jsonl",
		HealthListenAddress:    "127.0.0.1:0",
		RegistrationInterval:   10 * time.Second,
		RegistrationStaleAfter: 30 * time.Second,
		NVIDIAObservation:      NVIDIAObservationConfig{DCGMPath: "/nonexistent/dcgmi"},
	}, nvml.NewSMI("/nonexistent/nvidia-smi"), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	a.now = clock.Now
	if a.health == nil {
		t.Fatal("real nvidia-smi driver must get a DCGM/nvidia-smi second source")
	}
	if a.health.NodeName != "gpu-node-1" {
		t.Fatalf("second source node = %q, want gpu-node-1", a.health.NodeName)
	}
}
