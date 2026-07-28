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
