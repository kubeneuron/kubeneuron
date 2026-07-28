package accelerator

import (
	"context"
	"testing"
	"time"
)

func TestInventoryValidateAndTargetForDistinguishesPhysicalAndPartitions(t *testing.T) {
	inventory := Inventory{
		NodeName:       "gpu-node-1",
		Vendor:         VendorNVIDIA,
		DriverVersion:  "570.42",
		RuntimeVersion: "dcgm-4.1",
		ObservedAt:     time.Now(),
		Devices: []Device{
			{ID: "GPU-physical", Kind: DevicePhysical, Family: FamilyGPU, Model: "H100"},
			{ID: "MIG-partition-a", Kind: DevicePartition, Family: FamilyGPU, ParentID: "GPU-physical", PartitionProfile: "1g.10gb"},
		},
	}

	if err := inventory.Validate(); err != nil {
		t.Fatalf("valid inventory: %v", err)
	}

	physical, err := inventory.TargetFor("GPU-physical")
	if err != nil {
		t.Fatal(err)
	}
	if physical.Scope != ScopePhysicalDevice || physical.DeviceID != "GPU-physical" {
		t.Fatalf("physical target = %+v", physical)
	}

	partition, err := inventory.TargetFor("MIG-partition-a")
	if err != nil {
		t.Fatal(err)
	}
	if partition.Scope != ScopePartition || partition.DeviceID != "MIG-partition-a" {
		t.Fatalf("partition target = %+v", partition)
	}

	node, err := inventory.TargetFor("")
	if err != nil {
		t.Fatal(err)
	}
	if node.Scope != ScopeNode || node.DeviceID != "" {
		t.Fatalf("node target = %+v", node)
	}
}

func TestInventoryValidateRejectsUnsafeTopology(t *testing.T) {
	validPhysical := Device{ID: "physical-1", Kind: DevicePhysical, Family: FamilyGPU}
	for name, inventory := range map[string]Inventory{
		"missing node": {
			Vendor: VendorNVIDIA,
		},
		"duplicate device identity": {
			NodeName: "n1", Vendor: VendorNVIDIA,
			Devices: []Device{validPhysical, validPhysical},
		},
		"physical has parent": {
			NodeName: "n1", Vendor: VendorNVIDIA,
			Devices: []Device{{ID: "physical-1", Kind: DevicePhysical, Family: FamilyGPU, ParentID: "bad"}},
		},
		"partition lacks parent": {
			NodeName: "n1", Vendor: VendorNVIDIA,
			Devices: []Device{{ID: "part-1", Kind: DevicePartition, Family: FamilyGPU}},
		},
		"partition parent is a partition": {
			NodeName: "n1", Vendor: VendorNVIDIA,
			Devices: []Device{
				{ID: "physical-1", Kind: DevicePhysical, Family: FamilyGPU},
				{ID: "part-1", Kind: DevicePartition, Family: FamilyGPU, ParentID: "physical-1"},
				{ID: "part-2", Kind: DevicePartition, Family: FamilyGPU, ParentID: "part-1"},
			},
		},
		"partition family differs from parent": {
			NodeName: "n1", Vendor: VendorGoogle,
			Devices: []Device{
				{ID: "tpu-1", Kind: DevicePhysical, Family: FamilyTPU},
				{ID: "part-1", Kind: DevicePartition, Family: FamilyGPU, ParentID: "tpu-1"},
			},
		},
	} {
		t.Run(name, func(t *testing.T) {
			if err := inventory.Validate(); err == nil {
				t.Fatal("Validate() succeeded for invalid inventory")
			}
		})
	}
}

func TestCapabilitySetPreservesPartitionSafety(t *testing.T) {
	capabilities := CapabilitySet{
		{Action: ActionResetDevice, Scopes: []TargetScope{ScopePhysicalDevice}},
		{Action: ActionCollectDiagnostics, Scopes: []TargetScope{ScopeNode, ScopePhysicalDevice, ScopePartition}},
		{Action: ActionRebootNode, Scopes: []TargetScope{ScopeNode}},
	}
	if err := capabilities.Validate(); err != nil {
		t.Fatalf("valid capabilities: %v", err)
	}

	if !capabilities.Supports(ActionResetDevice, ScopePhysicalDevice) {
		t.Fatal("physical reset must be supported")
	}
	if capabilities.Supports(ActionResetDevice, ScopePartition) {
		t.Fatal("partition reset must not be inferred from physical reset")
	}
	if !capabilities.Supports(ActionCollectDiagnostics, ScopePartition) {
		t.Fatal("partition diagnostics must be supported")
	}
	if capabilities.Supports(ActionRebootNode, ScopePhysicalDevice) {
		t.Fatal("node reboot must not be exposed as a device action")
	}
}

func TestCapabilitySetValidateRejectsAmbiguousDeclarations(t *testing.T) {
	for name, capabilities := range map[string]CapabilitySet{
		"unknown action":   {{Action: Action("vendor-reset"), Scopes: []TargetScope{ScopePhysicalDevice}}},
		"empty scope list": {{Action: ActionResetDevice}},
		"invalid scope":    {{Action: ActionResetDevice, Scopes: []TargetScope{"rack"}}},
		"duplicated scope": {{Action: ActionResetDevice, Scopes: []TargetScope{ScopePhysicalDevice, ScopePhysicalDevice}}},
		"duplicated across declarations": {
			{Action: ActionResetDevice, Scopes: []TargetScope{ScopePhysicalDevice}},
			{Action: ActionResetDevice, Scopes: []TargetScope{ScopePhysicalDevice}},
		},
	} {
		t.Run(name, func(t *testing.T) {
			if err := capabilities.Validate(); err == nil {
				t.Fatal("Validate() succeeded for invalid capabilities")
			}
		})
	}
}

type testAdapter struct{}

var _ Adapter = testAdapter{}
var _ DetectionWatcher = testAdapter{}

func (testAdapter) Vendor() Vendor { return VendorNVIDIA }

func (testAdapter) Inventory(context.Context) (Inventory, error) {
	return Inventory{NodeName: "n1", Vendor: VendorNVIDIA}, nil
}

func (testAdapter) Capabilities(context.Context) (CapabilitySet, error) {
	return CapabilitySet{{Action: ActionVerifyHealth, Scopes: []TargetScope{ScopeNode}}}, nil
}

func (testAdapter) CheckHealth(_ context.Context, target Target) (HealthReport, error) {
	return HealthReport{Target: target, Status: HealthHealthy, CheckedAt: time.Now()}, nil
}

func (testAdapter) Detect(context.Context) ([]Detection, error) { return nil, nil }

func (testAdapter) WatchDetections(ctx context.Context) (<-chan Detection, error) {
	stream := make(chan Detection)
	go func() {
		<-ctx.Done()
		close(stream)
	}()
	return stream, nil
}

func TestAdapterContractsCanServePollingAndStreamingDetection(t *testing.T) {
	adapter := testAdapter{}
	if adapter.Vendor() != VendorNVIDIA {
		t.Fatalf("Vendor() = %q", adapter.Vendor())
	}
	if _, err := adapter.Detect(context.Background()); err != nil {
		t.Fatalf("Detect(): %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	stream, err := adapter.WatchDetections(ctx)
	if err != nil {
		cancel()
		t.Fatalf("WatchDetections(): %v", err)
	}
	cancel()
	if _, open := <-stream; open {
		t.Fatal("stream remained open after cancellation")
	}
}
