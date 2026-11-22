// SPDX-License-Identifier: Apache-2.0

package agent

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/kubeneuron/kubeneuron/internal/controller"
	"github.com/kubeneuron/kubeneuron/internal/detect"
	"github.com/kubeneuron/kubeneuron/internal/httpapi"
	"github.com/kubeneuron/kubeneuron/internal/metrics"
	"github.com/kubeneuron/kubeneuron/internal/store/sqlite"
	"github.com/kubeneuron/kubeneuron/pkg/types"
)

// neuronTree is a synthetic driver tree: every file is written by the test.
type neuronTree struct {
	t    *testing.T
	root string
}

func newNeuronTree(t *testing.T) *neuronTree {
	return &neuronTree{t: t, root: filepath.Join(t.TempDir(), "neuron_device")}
}

func (n *neuronTree) set(device, file, value string) {
	n.t.Helper()
	path := filepath.Join(n.root, device, "stats", "hardware", file)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		n.t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(value+"\n"), 0o644); err != nil {
		n.t.Fatal(err)
	}
}

// neuronController records what the agent posts.
type neuronController struct {
	mu      sync.Mutex
	events  []types.AgentEvent
	reports []types.AgentAcceleratorReport
}

func (c *neuronController) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	c.mu.Lock()
	defer c.mu.Unlock()
	switch r.URL.Path {
	case "/api/v1/events":
		var ev types.AgentEvent
		_ = json.NewDecoder(r.Body).Decode(&ev)
		c.events = append(c.events, ev)
		w.WriteHeader(http.StatusAccepted)
	case types.AgentAcceleratorReportPath:
		var report types.AgentAcceleratorReport
		_ = json.NewDecoder(r.Body).Decode(&report)
		c.reports = append(c.reports, report)
		w.WriteHeader(http.StatusNoContent)
	default:
		w.WriteHeader(http.StatusNoContent)
	}
}

func (c *neuronController) faults() []types.AgentEvent {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []types.AgentEvent
	for _, ev := range c.events {
		if ev.Fault != nil && ev.Fault.Source == detect.NeuronSysfsSource {
			out = append(out, ev)
		}
	}
	return out
}

func neuronAgent(t *testing.T, url, root string) (*Agent, *testClock) {
	t.Helper()
	clock := &testClock{now: time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)}
	a := newTestAgent(t, url, clock, slog.New(slog.NewTextHandler(io.Discard, nil)))
	a.neuron = a.newNeuronSysfs(root, t.TempDir())
	return a, clock
}

// A component the driver lists is a fault only once two reads in a row list
// it: one read gives no event. It is sent once, and again only after a read
// no longer lists it.
func TestNeuronComponentNeedsTwoReads(t *testing.T) {
	tree := newNeuronTree(t)
	tree.set("neuron0", "health_status/unhealthy_components", "")
	tree.set("neuron1", "health_status/unhealthy_components", "hbm, not-a-component")
	ctl := &neuronController{}
	srv := httptest.NewServer(ctl)
	defer srv.Close()
	a, clock := neuronAgent(t, srv.URL, tree.root)
	ctx := context.Background()

	a.readNeuron(ctx)
	if got := ctl.faults(); len(got) != 0 {
		t.Fatalf("one read gave events %+v", got)
	}
	clock.Advance(time.Minute)
	a.readNeuron(ctx)
	got := ctl.faults()
	if len(got) != 2 {
		t.Fatalf("second read gave %d events, want one per component: %+v", len(got), got)
	}
	components := map[string]bool{}
	for _, ev := range got {
		if ev.Fault.Vendor != "aws" || ev.Fault.Code != "unhealthy-component" || ev.Fault.Attributes["device"] != "neuron1" ||
			ev.GPUIndex != -1 || ev.GPUUUID != "" {
			t.Fatalf("event %+v", ev)
		}
		components[ev.Fault.Attributes["component"]] = true
	}
	if !components["hbm"] || !components["unknown-component"] {
		t.Fatalf("components %v: a name not in the closed list must be unknown-component", components)
	}
	clock.Advance(time.Minute)
	a.readNeuron(ctx)
	if n := len(ctl.faults()); n != 2 {
		t.Fatalf("a component still listed was sent again: %d events", n)
	}

	tree.set("neuron1", "health_status/unhealthy_components", "")
	clock.Advance(time.Minute)
	a.readNeuron(ctx)
	tree.set("neuron1", "health_status/unhealthy_components", "hbm")
	clock.Advance(time.Minute)
	a.readNeuron(ctx)
	if n := len(ctl.faults()); n != 2 {
		t.Fatalf("a component back for one read gave an event: %d events", n)
	}
}

// A counter's rise is a fault of its device; a counter already set when the
// agent had no baseline is not. The devices go to the controller in a report
// of vendor aws, by their index.
func TestNeuronCounterRiseAndReport(t *testing.T) {
	tree := newNeuronTree(t)
	tree.set("neuron0", "mem_ecc_uncorrected", "0")
	tree.set("neuron3", "sram_ecc_uncorrected", "2")
	ctl := &neuronController{}
	srv := httptest.NewServer(ctl)
	defer srv.Close()
	a, clock := neuronAgent(t, srv.URL, tree.root)
	ctx := context.Background()

	a.readNeuron(ctx)
	if got := ctl.faults(); len(got) != 0 {
		t.Fatalf("counters read without a baseline gave events %+v", got)
	}
	tree.set("neuron0", "mem_ecc_uncorrected", "1")
	clock.Advance(time.Minute)
	a.readNeuron(ctx)
	got := ctl.faults()
	if len(got) != 1 || got[0].Fault.Code != "hbm-ecc-uncorrectable" || got[0].Fault.Attributes["device"] != "neuron0" {
		t.Fatalf("events %+v, want neuron0's hbm-ecc-uncorrectable", got)
	}

	ctl.mu.Lock()
	defer ctl.mu.Unlock()
	if len(ctl.reports) == 0 {
		t.Fatal("no accelerator report")
	}
	report := ctl.reports[len(ctl.reports)-1]
	if report.Vendor != types.AcceleratorVendorAWS || report.Node != "gpu-node-1" || report.Readiness == types.AcceleratorReadinessReady ||
		len(report.Capabilities) != 0 || len(report.Devices) != 2 {
		t.Fatalf("report %+v", report)
	}
	for i, id := range []string{"neuron0", "neuron3"} {
		if d := report.Devices[i]; d.ID != id || d.Family != types.AcceleratorFamilyNeuron || d.Kind != types.AcceleratorDevicePhysical {
			t.Fatalf("device %d %+v, want %s", i, d, id)
		}
	}
}

// A node without the driver's tree has no such source: nothing is sent.
func TestNeuronAbsentTreeSendsNothing(t *testing.T) {
	ctl := &neuronController{}
	srv := httptest.NewServer(ctl)
	defer srv.Close()
	a, _ := neuronAgent(t, srv.URL, filepath.Join(t.TempDir(), "none"))
	a.readNeuron(context.Background())
	ctl.mu.Lock()
	defer ctl.mu.Unlock()
	if len(ctl.events) != 0 || len(ctl.reports) != 0 {
		t.Fatalf("posts %+v %+v from a node without the tree", ctl.events, ctl.reports)
	}
}

type neuronAuth struct{}

func (neuronAuth) AuthenticateAgent(*http.Request) (httpapi.AgentPrincipal, error) {
	return httpapi.AgentPrincipal{NodeName: "gpu-node-1", NodeUID: "uid-gpu-node-1"}, nil
}

// Through the controller's agent routes: the report is stored with vendor
// aws under the node's current UID, which is where the pool view reads the
// node's vendor, and the fault is archived.
func TestNeuronReportAndFaultReachTheController(t *testing.T) {
	st, err := sqlite.Open(filepath.Join(t.TempDir(), "kn.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	c := controller.New(st, st, nil, nil, nil, nil, nil, nil, log)
	srv := httptest.NewServer(httpapi.New(c).AgentRoutes(neuronAuth{}))
	defer srv.Close()

	tree := newNeuronTree(t)
	tree.set("neuron2", "health_status/unhealthy_components", "link_efa")
	a, clock := neuronAgent(t, srv.URL, tree.root)
	ctx := context.Background()
	a.readNeuron(ctx)
	clock.Advance(time.Minute)
	a.readNeuron(ctx)

	report, err := st.GetAcceleratorReport(ctx, "gpu-node-1", types.AcceleratorVendorAWS)
	if err != nil {
		t.Fatal(err)
	}
	if report.NodeUID != "uid-gpu-node-1" || len(report.Devices) != 1 || report.Devices[0].ID != "neuron2" ||
		report.Readiness != types.AcceleratorReadinessObserved {
		t.Fatalf("stored report %+v", report)
	}
	a.reportSignalSources(ctx)
	if testing.Verbose() {
		out, _ := json.MarshalIndent(report, "", "  ")
		t.Logf("stored report:\n%s", out)
		view, _ := json.MarshalIndent(c.AcceleratorHealth(ctx, "gpu-node-1"), "", "  ")
		t.Logf("health view:\n%s", view)
	}
	events, err := st.RecentFaultEvents(ctx, "aws", clock.Now().Add(-time.Hour), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].Fault.Code != "unhealthy-component" || events[0].Fault.Attributes["component"] != "link_efa" ||
		events[0].Fault.Attributes["device"] != "neuron2" {
		t.Fatalf("archived %+v", events)
	}
}

// A tree that cannot be read is an unavailable source with its reason, and
// a report that is not ready, never a healthy node.
func TestNeuronUnreadableTreeIsUnavailable(t *testing.T) {
	root := filepath.Join(t.TempDir(), "neuron_device")
	if err := os.WriteFile(root, []byte("not a directory"), 0o644); err != nil {
		t.Fatal(err)
	}
	ctl := &neuronController{}
	srv := httptest.NewServer(ctl)
	defer srv.Close()
	a, _ := neuronAgent(t, srv.URL, root)
	a.readNeuron(context.Background())
	t.Cleanup(func() { a.neuron.setReason("") })

	var row *types.AgentSignalSource
	for _, r := range metrics.AgentSignalSources("gpu-node-1") {
		if r.Source == detect.NeuronSysfsSource {
			row = &r
		}
	}
	if row == nil || row.Available || row.Reason != "read-error" || row.Class != types.ClassAcceleratorFault {
		t.Fatalf("source row %+v, want unavailable with read-error", row)
	}
	ctl.mu.Lock()
	defer ctl.mu.Unlock()
	if len(ctl.reports) != 1 || ctl.reports[0].Readiness != types.AcceleratorReadinessNotReady || len(ctl.reports[0].Devices) != 0 {
		t.Fatalf("reports %+v", ctl.reports)
	}
}
