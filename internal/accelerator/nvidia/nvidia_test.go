package nvidia

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/kubeneuron/kubeneuron/internal/accelerator"
	"github.com/kubeneuron/kubeneuron/internal/agent/nvml"
	"github.com/kubeneuron/kubeneuron/pkg/types"
)

func TestInventoryTranslatesOnlyPhysicalNVIDIAGPUs(t *testing.T) {
	now := time.Date(2026, time.July, 24, 12, 0, 0, 0, time.UTC)
	adapter := newAdapter(t, Config{
		NodeName:          "gpu-node-1",
		DriverVersion:     "570.42",
		RuntimeVersion:    "dcgm-4.1",
		PartitionTopology: PartitionTopologyMIG,
		Now:               func() time.Time { return now },
	}, &nvml.Fake{GPUs: []types.GPUInfo{
		{Index: 3, UUID: "GPU-a", Model: "NVIDIA H100"},
		{Index: 7, UUID: "GPU-b", Model: "NVIDIA H100"},
	}})

	inventory, err := adapter.Inventory(context.Background())
	if err != nil {
		t.Fatalf("Inventory() error = %v", err)
	}
	if inventory.NodeName != "gpu-node-1" || inventory.Vendor != accelerator.VendorNVIDIA {
		t.Fatalf("inventory identity = %+v", inventory)
	}
	if inventory.DriverVersion != "570.42" || inventory.RuntimeVersion != "dcgm-4.1" || !inventory.ObservedAt.Equal(now) {
		t.Fatalf("inventory metadata = %+v", inventory)
	}
	if len(inventory.Devices) != 2 {
		t.Fatalf("devices = %+v, want two physical GPUs", inventory.Devices)
	}
	for _, device := range inventory.Devices {
		if device.Kind != accelerator.DevicePhysical || device.Family != accelerator.FamilyGPU || device.ParentID != "" {
			t.Fatalf("unsafe device topology = %+v", device)
		}
		if device.Attributes["nvidia.com/partition-topology"] != string(PartitionTopologyMIG) {
			t.Fatalf("device must disclose unsupported partition topology: %+v", device)
		}
	}
	if err := inventory.Validate(); err != nil {
		t.Fatalf("translated inventory does not validate: %v", err)
	}
}

func TestCapabilitiesFailClosedForUnknownOrMIGTopology(t *testing.T) {
	for name, topology := range map[string]PartitionTopology{
		"default unknown": "",
		"reported MIG":    PartitionTopologyMIG,
		"other partition": PartitionTopologyOther,
	} {
		t.Run(name, func(t *testing.T) {
			adapter := newAdapter(t, Config{NodeName: "n1", PartitionTopology: topology}, &nvml.Fake{})
			capabilities, err := adapter.Capabilities(context.Background())
			if err != nil {
				t.Fatalf("Capabilities() error = %v", err)
			}
			if capabilities.Supports(accelerator.ActionResetDevice, accelerator.ScopePhysicalDevice) {
				t.Fatalf("reset must be withheld for %q topology", topology)
			}
			if capabilities.Supports(accelerator.ActionResetDevice, accelerator.ScopePartition) {
				t.Fatal("partition reset must never be inferred")
			}
			if !capabilities.Supports(accelerator.ActionVerifyHealth, accelerator.ScopeNode) ||
				!capabilities.Supports(accelerator.ActionVerifyHealth, accelerator.ScopePhysicalDevice) {
				t.Fatalf("read-only health should remain available: %+v", capabilities)
			}
		})
	}
}

func TestCapabilitiesAllowOnlyVerifiedUnpartitionedPhysicalReset(t *testing.T) {
	adapter := newAdapter(t, Config{NodeName: "n1", PartitionTopology: PartitionTopologyNone}, &nvml.Fake{})
	capabilities, err := adapter.Capabilities(context.Background())
	if err != nil {
		t.Fatalf("Capabilities() error = %v", err)
	}
	if !capabilities.Supports(accelerator.ActionResetDevice, accelerator.ScopePhysicalDevice) {
		t.Fatalf("verified unpartitioned profile lacks physical reset: %+v", capabilities)
	}
	if capabilities.Supports(accelerator.ActionResetDevice, accelerator.ScopePartition) {
		t.Fatalf("partition reset was inferred: %+v", capabilities)
	}
	if capabilities.Supports(accelerator.ActionRebootNode, accelerator.ScopeNode) ||
		capabilities.Supports(accelerator.ActionCollectDiagnostics, accelerator.ScopeNode) {
		t.Fatalf("adapter claimed operations absent from GPUDriver: %+v", capabilities)
	}
}

func TestDetectionFromAgentEventNormalizesXIDAndRetainsEvidence(t *testing.T) {
	at := time.Date(2026, time.July, 24, 12, 30, 0, 0, time.UTC)
	detection, ok := DetectionFromAgentEvent(types.AgentEvent{
		EventID:   "event-79",
		Node:      "gpu-node-1",
		GPUIndex:  2,
		GPUUUID:   "GPU-physical",
		XID:       79,
		Raw:       "NVRM: Xid ...",
		Timestamp: at,
	})
	if !ok {
		t.Fatal("XID 79 must normalize")
	}
	if detection.ID != "event-79" || detection.Class != accelerator.DetectionDeviceUnavailable || detection.Severity != accelerator.SeverityCritical {
		t.Fatalf("detection classification = %+v", detection)
	}
	if detection.Target.NodeName != "gpu-node-1" || detection.Target.DeviceID != "GPU-physical" || detection.Target.Scope != accelerator.ScopePhysicalDevice {
		t.Fatalf("detection target = %+v", detection.Target)
	}
	if !detection.ObservedAt.Equal(at) || detection.Evidence["xid"] != "79" || detection.Evidence["legacy_problem_class"] != "fell-off-bus" {
		t.Fatalf("detection evidence = %+v", detection)
	}

	if _, ok := DetectionFromAgentEvent(types.AgentEvent{Node: "gpu-node-1", XID: 999, Timestamp: at}); ok {
		t.Fatal("unknown XID must not manufacture a normalized detection")
	}
}

func TestAdapterEventMappingRejectsOtherNodesAndLeavesMissingUUIDNodeScoped(t *testing.T) {
	at := time.Date(2026, time.July, 24, 13, 0, 0, 0, time.UTC)
	adapter := newAdapter(t, Config{NodeName: "local"}, &nvml.Fake{})
	if _, ok := adapter.DetectionFromAgentEvent(types.AgentEvent{Node: "other", XID: 48, Timestamp: at}); ok {
		t.Fatal("adapter accepted an event belonging to another node")
	}
	detection, ok := adapter.DetectionFromAgentEvent(types.AgentEvent{Node: "local", XID: 48, Timestamp: at})
	if !ok {
		t.Fatal("known local XID must map")
	}
	if detection.Target.Scope != accelerator.ScopeNode || detection.Target.DeviceID != "" {
		t.Fatalf("unresolved GPU must remain node-scoped, got %+v", detection.Target)
	}
}

func TestWatchDetectionsMapsOnlyLocalActionableXIDs(t *testing.T) {
	at := time.Date(2026, time.July, 24, 13, 30, 0, 0, time.UTC)
	events := make(chan types.AgentEvent, 3)
	events <- types.AgentEvent{EventID: "other", Node: "other-node", XID: 79, Timestamp: at}
	events <- types.AgentEvent{EventID: "unknown", Node: "local", XID: 999, Timestamp: at}
	events <- types.AgentEvent{EventID: "local", Node: "local", GPUUUID: "GPU-1", XID: 48, Timestamp: at}
	close(events)

	adapter := newAdapter(t, Config{NodeName: "local"}, eventFake{
		Fake:   &nvml.Fake{GPUs: []types.GPUInfo{{Index: 0, UUID: "GPU-1"}}},
		events: events,
	})
	stream, err := adapter.WatchDetections(context.Background())
	if err != nil {
		t.Fatalf("WatchDetections() error = %v", err)
	}
	detection, open := <-stream
	if !open {
		t.Fatal("stream closed before local actionable XID was mapped")
	}
	if detection.ID != "local" || detection.Class != accelerator.DetectionMemory {
		t.Fatalf("stream detection = %+v", detection)
	}
	if _, open := <-stream; open {
		t.Fatal("stream emitted a non-local or unknown XID")
	}
}

func TestCheckHealthUsesDriverAndNeverClaimsPartitionHealth(t *testing.T) {
	adapter := newAdapter(t, Config{NodeName: "n1"}, &nvml.Fake{GPUs: []types.GPUInfo{{Index: 0, UUID: "GPU-1"}}})

	report, err := adapter.CheckHealth(context.Background(), accelerator.Target{
		NodeName: "n1", Vendor: accelerator.VendorNVIDIA, DeviceID: "GPU-1", Scope: accelerator.ScopePhysicalDevice,
	})
	if err != nil {
		t.Fatalf("physical CheckHealth() error = %v", err)
	}
	if report.Status != accelerator.HealthHealthy || report.Evidence["physical_device_present"] != "true" {
		t.Fatalf("physical report = %+v", report)
	}

	_, err = adapter.CheckHealth(context.Background(), accelerator.Target{
		NodeName: "n1", Vendor: accelerator.VendorNVIDIA, DeviceID: "MIG-1", Scope: accelerator.ScopePartition,
	})
	if err == nil {
		t.Fatal("partition health must fail closed without MIG inventory")
	}
}

func TestPreflightReturnsEligibleSnapshotForVerifiedUnpartitionedNode(t *testing.T) {
	driver := &preflightDriver{gpus: []types.GPUInfo{{Index: 4, UUID: "GPU-4", Model: "NVIDIA H100"}}}
	adapter := newAdapter(t, Config{
		NodeName:          "n1",
		PartitionTopology: PartitionTopologyNone,
	}, driver)

	report := adapter.Preflight(context.Background())
	if report.Readiness != PreflightEligible || !report.Ready() {
		t.Fatalf("preflight readiness = %q, reasons = %v", report.Readiness, report.Reasons)
	}
	if driver.listCalls != 1 || driver.healthyCalls != 1 {
		t.Fatalf("preflight probes: ListGPUs=%d Healthy=%d, want one each", driver.listCalls, driver.healthyCalls)
	}
	if !report.Allows(accelerator.ActionResetDevice, accelerator.Target{
		NodeName: "n1", Vendor: accelerator.VendorNVIDIA, DeviceID: "GPU-4", Scope: accelerator.ScopePhysicalDevice,
	}) {
		t.Fatalf("eligible preflight withheld physical reset: %+v", report)
	}
	if report.Allows(accelerator.ActionResetDevice, accelerator.Target{
		NodeName: "n1", Vendor: accelerator.VendorNVIDIA, DeviceID: "MIG-4", Scope: accelerator.ScopePartition,
	}) {
		t.Fatalf("preflight allowed a partition reset: %+v", report)
	}
	if report.Allows(accelerator.ActionResetDevice, accelerator.Target{
		NodeName: "n1", Vendor: accelerator.VendorNVIDIA, DeviceID: "GPU-not-in-inventory", Scope: accelerator.ScopePhysicalDevice,
	}) {
		t.Fatalf("preflight allowed a physical GPU absent from its inventory: %+v", report)
	}
}

func TestPreflightUsesCurrentDriverMIGModeOverConfiguredTopology(t *testing.T) {
	for name, tc := range map[string]struct {
		topology string
		err      error
		want     PartitionTopology
		ready    PreflightReadiness
	}{
		"MIG denies configured none": {
			topology: "mig", want: PartitionTopologyMIG, ready: PreflightObservedOnly,
		},
		"probe failure is blocked": {
			err: errors.New("MIG query failed"), want: PartitionTopologyUnknown, ready: PreflightBlocked,
		},
	} {
		t.Run(name, func(t *testing.T) {
			driver := topologyPreflightDriver{
				preflightDriver: &preflightDriver{gpus: []types.GPUInfo{{Index: 0, UUID: "GPU-0"}}},
				topology:        tc.topology,
				err:             tc.err,
			}
			adapter := newAdapter(t, Config{NodeName: "n1", PartitionTopology: PartitionTopologyNone}, &driver)
			report := adapter.Preflight(context.Background())
			if report.Topology != tc.want || report.Readiness != tc.ready {
				t.Fatalf("preflight = topology %q readiness %q reasons=%v; want %q/%q", report.Topology, report.Readiness, report.Reasons, tc.want, tc.ready)
			}
			if report.Allows(accelerator.ActionResetDevice, accelerator.Target{
				NodeName: "n1", Vendor: accelerator.VendorNVIDIA, DeviceID: "GPU-0", Scope: accelerator.ScopePhysicalDevice,
			}) {
				t.Fatalf("current topology %q incorrectly allowed physical reset", tc.want)
			}
			for _, capability := range report.Capabilities {
				if capability.Action == accelerator.ActionResetDevice {
					t.Fatalf("current topology %q declared reset capability: %+v", tc.want, report.Capabilities)
				}
			}
		})
	}
}

func TestPreflightUsesCurrentDriverVersionOverConfiguredFallback(t *testing.T) {
	for name, tc := range map[string]struct {
		version string
		err     error
		ready   PreflightReadiness
	}{
		"current version": {version: "570.86.15", ready: PreflightEligible},
		"probe failure":   {err: errors.New("driver query failed"), ready: PreflightBlocked},
	} {
		t.Run(name, func(t *testing.T) {
			driver := versionPreflightDriver{
				preflightDriver: &preflightDriver{gpus: []types.GPUInfo{{Index: 0, UUID: "GPU-0"}}},
				version:         tc.version,
				err:             tc.err,
			}
			adapter := newAdapter(t, Config{
				NodeName: "n1", DriverVersion: "manual-unsafe-fallback", RuntimeVersion: "runtime-v1",
				PartitionTopology: PartitionTopologyNone,
			}, &driver)
			report := adapter.Preflight(context.Background())
			if report.Readiness != tc.ready {
				t.Fatalf("preflight readiness = %q, reasons=%v; want %q", report.Readiness, report.Reasons, tc.ready)
			}
			if tc.err == nil && report.Inventory.DriverVersion != tc.version {
				t.Fatalf("preflight driver version = %q, want current %q", report.Inventory.DriverVersion, tc.version)
			}
		})
	}
}

func TestPreflightUnknownOrMIGTopologyIsObservedOnlyAndCannotBeOverridden(t *testing.T) {
	for name, topology := range map[string]PartitionTopology{
		"unknown": PartitionTopologyUnknown,
		"MIG":     PartitionTopologyMIG,
	} {
		t.Run(name, func(t *testing.T) {
			adapter := newAdapter(t, Config{NodeName: "n1", PartitionTopology: topology}, &preflightDriver{
				gpus: []types.GPUInfo{{Index: 0, UUID: "GPU-0"}},
			})

			report := adapter.Preflight(context.Background())
			if report.Readiness != PreflightObservedOnly || report.Ready() {
				t.Fatalf("preflight readiness = %q, reasons = %v", report.Readiness, report.Reasons)
			}
			if !containsReason(report.Reasons, "not explicitly unpartitioned") {
				t.Fatalf("preflight did not explain reset exclusion: %v", report.Reasons)
			}

			physicalTarget := accelerator.Target{
				NodeName: "n1", Vendor: accelerator.VendorNVIDIA, DeviceID: "GPU-0", Scope: accelerator.ScopePhysicalDevice,
			}
			if report.Allows(accelerator.ActionResetDevice, physicalTarget) {
				t.Fatalf("%s topology allowed physical reset", topology)
			}

			// Status data can be copied or augmented by an external caller. That
			// must not change the adapter-owned preflight topology decision.
			report.Capabilities = append(report.Capabilities, accelerator.ActionCapability{
				Action: accelerator.ActionResetDevice,
				Scopes: []accelerator.TargetScope{accelerator.ScopePhysicalDevice},
			})
			if report.Allows(accelerator.ActionResetDevice, physicalTarget) {
				t.Fatalf("external capability made %s topology resettable", topology)
			}
			if !report.Allows(accelerator.ActionVerifyHealth, accelerator.Target{
				NodeName: "n1", Vendor: accelerator.VendorNVIDIA, Scope: accelerator.ScopeNode,
			}) {
				t.Fatal("observed-only preflight withheld read-only node health")
			}
		})
	}
}

func TestPreflightFailsClosedAndRetainsAllProbeReasons(t *testing.T) {
	driver := &preflightDriver{
		listErr:    errors.New("inventory unavailable"),
		healthyErr: errors.New("driver liveness timed out"),
	}
	adapter := newAdapter(t, Config{NodeName: "n1", PartitionTopology: PartitionTopologyNone}, driver)

	report := adapter.Preflight(context.Background())
	if report.Readiness != PreflightBlocked || report.Ready() {
		t.Fatalf("preflight readiness = %q, reasons = %v", report.Readiness, report.Reasons)
	}
	if driver.listCalls != 1 || driver.healthyCalls != 1 {
		t.Fatalf("preflight did not attempt all probes: ListGPUs=%d Healthy=%d", driver.listCalls, driver.healthyCalls)
	}
	if !containsReason(report.Reasons, "inventory probe failed") || !containsReason(report.Reasons, "node health is") {
		t.Fatalf("preflight lost failed-probe evidence: %v", report.Reasons)
	}
	if report.Allows(accelerator.ActionResetDevice, accelerator.Target{
		NodeName: "n1", Vendor: accelerator.VendorNVIDIA, DeviceID: "GPU-0", Scope: accelerator.ScopePhysicalDevice,
	}) {
		t.Fatalf("blocked preflight allowed reset: %+v", report)
	}
}

func TestPreflightBlocksEmptyInventory(t *testing.T) {
	adapter := newAdapter(t, Config{NodeName: "n1", PartitionTopology: PartitionTopologyNone}, &preflightDriver{})
	report := adapter.Preflight(context.Background())
	if report.Readiness != PreflightBlocked {
		t.Fatalf("preflight readiness = %q, want blocked", report.Readiness)
	}
	if !containsReason(report.Reasons, "contains no NVIDIA GPUs") {
		t.Fatalf("preflight did not explain empty inventory: %v", report.Reasons)
	}
}

func containsReason(reasons []string, want string) bool {
	for _, reason := range reasons {
		if strings.Contains(reason, want) {
			return true
		}
	}
	return false
}

func newAdapter(t *testing.T, cfg Config, driver nvml.GPUDriver) *Adapter {
	t.Helper()
	adapter, err := New(cfg, driver)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	return adapter
}

// eventFake keeps the standard nvml.Fake behaviour while making the optional
// driver event source controllable for this adapter-only test.
type eventFake struct {
	*nvml.Fake
	events <-chan types.AgentEvent
}

func (f eventFake) WatchEvents(context.Context) (<-chan types.AgentEvent, error) {
	return f.events, nil
}

// preflightDriver records the probes reached by Adapter.Preflight without
// exposing any host-side NVIDIA functionality to a unit test.
type preflightDriver struct {
	gpus       []types.GPUInfo
	listErr    error
	healthyErr error

	listCalls    int
	healthyCalls int
}

type topologyPreflightDriver struct {
	*preflightDriver
	topology string
	err      error
}

func (d *topologyPreflightDriver) PartitionTopology(context.Context) (string, error) {
	return d.topology, d.err
}

type versionPreflightDriver struct {
	*preflightDriver
	version string
	err     error
}

func (d *versionPreflightDriver) DriverVersion(context.Context) (string, error) {
	return d.version, d.err
}

func (d *preflightDriver) Init() error     { return nil }
func (d *preflightDriver) Shutdown() error { return nil }
func (d *preflightDriver) ListGPUs(context.Context) ([]types.GPUInfo, error) {
	d.listCalls++
	if d.listErr != nil {
		return nil, d.listErr
	}
	return d.gpus, nil
}
func (d *preflightDriver) GPUByPCIAddr(context.Context, string) (types.GPUInfo, error) {
	return types.GPUInfo{}, errors.New("not implemented in preflight test driver")
}
func (d *preflightDriver) ResetGPU(context.Context, int) error   { return nil }
func (d *preflightDriver) EnsureIdle(context.Context, int) error { return nil }
func (d *preflightDriver) WatchEvents(context.Context) (<-chan types.AgentEvent, error) {
	return nil, errors.New("not implemented in preflight test driver")
}
func (d *preflightDriver) Healthy(context.Context) error {
	d.healthyCalls++
	return d.healthyErr
}
