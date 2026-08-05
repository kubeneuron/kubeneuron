package detect

import (
	"testing"
	"time"

	"github.com/kubeneuron/kubeneuron/internal/config"
	"github.com/kubeneuron/kubeneuron/pkg/types"
)

func TestCatalogXIDOverrideWinsOverBuiltin(t *testing.T) {
	catalog, err := NewCatalog([]config.SignalOverride{{
		Name: "xid-79-observe", XIDCodes: []int{79},
		Class: "custom-fell-off-bus", Severity: types.SeverityWarning,
	}})
	if err != nil {
		t.Fatal(err)
	}

	sig, ok := catalog.SignalFromAgentEvent(types.AgentEvent{Node: "n1", XID: 79, Timestamp: time.Now()})
	if !ok {
		t.Fatal("overridden XID must map")
	}
	if sig.Class != "custom-fell-off-bus" || sig.Severity != types.SeverityWarning {
		t.Fatalf("signal = %+v, want override class/severity", sig)
	}

	// Non-overridden XIDs keep the built-in classification.
	sig, ok = catalog.SignalFromAgentEvent(types.AgentEvent{Node: "n1", XID: 48, Timestamp: time.Now()})
	if !ok || sig.Class != types.ClassECCDBE {
		t.Fatalf("built-in fallback = %+v, %v", sig, ok)
	}
}

func TestCatalogAddsUnknownXID(t *testing.T) {
	// XID 45 is not in the built-in table; an override makes it actionable.
	catalog, err := NewCatalog([]config.SignalOverride{{
		Name: "xid-45", XIDCodes: []int{45}, Class: types.ClassXIDApp, Severity: types.SeverityInfo,
	}})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := SignalFromAgentEvent(types.AgentEvent{XID: 45}); ok {
		t.Fatal("XID 45 must not be actionable without the override")
	}
	if _, ok := catalog.SignalFromAgentEvent(types.AgentEvent{Node: "n1", XID: 45, Timestamp: time.Now()}); !ok {
		t.Fatal("override must make XID 45 actionable")
	}
}

func TestCatalogAlertOverride(t *testing.T) {
	catalog, err := NewCatalog([]config.SignalOverride{{
		Name: "vendor-alert", AlertName: "VendorGpuFault",
		Class: types.ClassDriverHang, Severity: types.SeverityCritical,
	}})
	if err != nil {
		t.Fatal(err)
	}

	// Unknown to the built-in map, mapped by the override; override severity
	// is the default.
	sig, ok := catalog.SignalFromAlert(Alert{
		Status: "firing",
		Labels: map[string]string{"alertname": "VendorGpuFault", "node": "n1"},
	})
	if !ok || sig.Class != types.ClassDriverHang || sig.Severity != types.SeverityCritical {
		t.Fatalf("override alert = %+v, %v", sig, ok)
	}

	// The alert's own severity label still wins.
	sig, _ = catalog.SignalFromAlert(Alert{
		Status: "firing",
		Labels: map[string]string{"alertname": "VendorGpuFault", "node": "n1", "severity": "warning"},
	})
	if sig.Severity != types.SeverityWarning {
		t.Fatalf("severity label must win: %+v", sig)
	}

	// Resolved alerts never map.
	if _, ok := catalog.SignalFromAlert(Alert{
		Status: "resolved",
		Labels: map[string]string{"alertname": "VendorGpuFault"},
	}); ok {
		t.Fatal("resolved alert must not map")
	}

	// Built-in alerts still map when not overridden.
	sig, ok = catalog.SignalFromAlert(Alert{
		Status: "firing",
		Labels: map[string]string{"alertname": "GpuExporterDown", "node": "n1"},
	})
	if !ok || sig.Class != types.ClassDriverHang {
		t.Fatalf("built-in alert fallback = %+v, %v", sig, ok)
	}
}

func TestCatalogRejectsConflicts(t *testing.T) {
	if _, err := NewCatalog([]config.SignalOverride{
		{Name: "a", XIDCodes: []int{79}, Class: "x", Severity: types.SeverityInfo},
		{Name: "b", XIDCodes: []int{79}, Class: "y", Severity: types.SeverityInfo},
	}); err == nil {
		t.Fatal("duplicate XID overrides must be rejected")
	}
	if _, err := NewCatalog([]config.SignalOverride{
		{Name: "a", AlertName: "A", Class: "x", Severity: types.SeverityInfo},
		{Name: "b", AlertName: "A", Class: "y", Severity: types.SeverityInfo},
	}); err == nil {
		t.Fatal("duplicate alert overrides must be rejected")
	}
}

func TestNilCatalogFallsBackToBuiltins(t *testing.T) {
	var catalog *Catalog
	if _, ok := catalog.SignalFromAgentEvent(types.AgentEvent{Node: "n1", XID: 79, Timestamp: time.Now()}); !ok {
		t.Fatal("nil catalog must classify built-in XIDs")
	}
	if _, ok := catalog.SignalFromAlert(Alert{
		Status: "firing", Labels: map[string]string{"alertname": "GpuExporterDown", "node": "n1"},
	}); !ok {
		t.Fatal("nil catalog must map built-in alerts")
	}
}

// N2: a GPUSignalMapping override must reach the neutral fault encoding too.
// Before this, remapping XID 48 changed policy for the kmsg source while the
// nvidia-smi/DCGM fallback kept the built-in classification for the same
// physical condition — a user-visible policy split between detection sources.
func TestCatalogFaultOverrideWinsOverBuiltin(t *testing.T) {
	catalog, err := NewCatalog([]config.SignalOverride{{
		Name:   "quieter-dbe",
		Faults: []config.FaultOverride{{Vendor: "nvidia", Code: "ecc-dbe"}},
		Class:  "custom-ecc", Severity: types.SeverityWarning,
	}})
	if err != nil {
		t.Fatal(err)
	}

	ev := types.AgentEvent{
		Node: "n1", GPUUUID: "GPU-a", Timestamp: time.Now(),
		Fault: &types.FaultSignal{Vendor: "nvidia", Source: "nvidia-smi", Code: "ecc-dbe"},
	}
	sig, ok := catalog.SignalFromAgentEvent(ev)
	if !ok {
		t.Fatal("overridden fault must map")
	}
	if sig.Class != "custom-ecc" || sig.Severity != types.SeverityWarning {
		t.Fatalf("signal = %+v, want override class/severity", sig)
	}

	// Non-overridden fault codes keep the built-in classification.
	ev.Fault = &types.FaultSignal{Vendor: "nvidia", Source: "nvidia-smi", Code: "row-remap-failure"}
	if sig, ok := catalog.SignalFromAgentEvent(ev); !ok || sig.Class != types.ClassRowRemapFailure {
		t.Fatalf("built-in fallback = %+v, %v", sig, ok)
	}
	// The package-level (catalog-less) path stays built-in-only.
	ev.Fault = &types.FaultSignal{Vendor: "nvidia", Source: "nvidia-smi", Code: "ecc-dbe"}
	if sig, _ := SignalFromFault(ev); sig.Class != types.ClassECCDBE {
		t.Fatalf("built-in SignalFromFault = %+v, want ClassECCDBE", sig)
	}
}

// N2: an override can make an unknown vendor code actionable (the fault-table
// analogue of adding an unknown XID), and duplicate fault overrides are
// rejected at catalog build.
//
// The example code must be one the built-in table does NOT know: "amd/
// page-retirement" served here until the AMD source shipped and made it a
// built-in row, at which point this test would have asserted nothing.
func TestCatalogFaultOverrideAddsUnknownCodeAndRejectsDuplicates(t *testing.T) {
	const unknownCode = "hbm-stack-vendor-quirk"
	if _, builtin := ClassifyFault("amd", unknownCode); builtin {
		t.Fatalf("%q is now a built-in fault code; this test needs a code the table does not know", unknownCode)
	}
	catalog, err := NewCatalog([]config.SignalOverride{{
		Name:   "amd-hbm-quirk",
		Faults: []config.FaultOverride{{Vendor: "amd", Code: unknownCode}},
		Class:  types.ClassECCContained, Severity: types.SeverityWarning,
	}})
	if err != nil {
		t.Fatal(err)
	}
	ev := types.AgentEvent{
		Node: "n1", Timestamp: time.Now(),
		Fault: &types.FaultSignal{Vendor: "amd", Source: "amd-smi", Code: unknownCode},
	}
	if _, ok := SignalFromFault(ev); ok {
		t.Fatal("unknown vendor code must not be actionable without the override")
	}
	if sig, ok := catalog.SignalFromAgentEvent(ev); !ok || sig.Class != types.ClassECCContained {
		t.Fatalf("override must make the vendor code actionable: %+v, %v", sig, ok)
	}

	if _, err := NewCatalog([]config.SignalOverride{
		{Name: "a", Faults: []config.FaultOverride{{Vendor: "nvidia", Code: "ecc-dbe"}}, Class: "x", Severity: types.SeverityInfo},
		{Name: "b", Faults: []config.FaultOverride{{Vendor: "nvidia", Code: "ecc-dbe"}}, Class: "y", Severity: types.SeverityInfo},
	}); err == nil {
		t.Fatal("duplicate fault overrides must be rejected")
	}
}

// Pin of a DESIGN BOUNDARY, not a defect: the agent's cross-source dedup
// anchors on the built-in classification (detect.FaultClass) and is
// deliberately catalog-blind — GPUSignalMapping overrides are controller
// configuration and are never shipped to node agents. An override therefore
// changes controller-side classification (incident identity, playbook
// selection, severity) but NOT which raw events the agent collapses before
// they reach the controller. If this test breaks because FaultClass grew
// catalog awareness, the override config must be shipped to agents in the
// same change, or agent- and controller-side identities will drift apart.
func TestAgentDedupClassIsCatalogBlindByDesign(t *testing.T) {
	catalog, err := NewCatalog([]config.SignalOverride{{
		Name:   "remapped-dbe",
		Faults: []config.FaultOverride{{Vendor: "nvidia", Code: "ecc-dbe"}},
		Class:  "custom-ecc", Severity: types.SeverityWarning,
	}})
	if err != nil {
		t.Fatal(err)
	}
	ev := types.AgentEvent{
		Node: "n1", Timestamp: time.Now(),
		Fault: &types.FaultSignal{Vendor: "nvidia", Source: "nvidia-smi", Code: "ecc-dbe"},
	}

	// Controller-side: the override wins.
	if sig, ok := catalog.SignalFromAgentEvent(ev); !ok || sig.Class != "custom-ecc" {
		t.Fatalf("controller classification = %+v, %v; want the override class", sig, ok)
	}
	// Agent-side dedup identity: the built-in class, regardless of overrides.
	if class, ok := FaultClass(ev); !ok || class != types.ClassECCDBE {
		t.Fatalf("agent dedup class = %v, %v; want the built-in ClassECCDBE", class, ok)
	}
}
