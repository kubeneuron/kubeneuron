package detect

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"
	"time"

	"github.com/kubeneuron/kubeneuron/internal/config"
	"github.com/kubeneuron/kubeneuron/pkg/types"
)

func TestClassifyXIDCriticalCodes(t *testing.T) {
	critical := map[int]types.ProblemClass{
		48: types.ClassECCDBE,
		64: types.ClassRowRemapFailure,
		74: types.ClassNVLink,
		79: types.ClassFellOffBus,
		95: types.ClassECCDBE,
	}
	for code, wantClass := range critical {
		info, ok := ClassifyXID(code)
		if !ok {
			t.Fatalf("XID %d must be classified", code)
		}
		if info.Class != wantClass {
			t.Errorf("XID %d class = %s, want %s", code, info.Class, wantClass)
		}
		if info.Severity != types.SeverityCritical {
			t.Errorf("XID %d severity = %s, want critical", code, info.Severity)
		}
		if info.Threshold != 0 {
			t.Errorf("XID %d must act on first occurrence, has threshold %d", code, info.Threshold)
		}
	}
}

func TestClassifyXIDAppLevelHasThreshold(t *testing.T) {
	for _, code := range []int{13, 31} {
		info, ok := ClassifyXID(code)
		if !ok {
			t.Fatalf("XID %d must be classified", code)
		}
		if info.Class != types.ClassXIDApp {
			t.Errorf("XID %d class = %s, want %s", code, info.Class, types.ClassXIDApp)
		}
		if info.Threshold == 0 {
			t.Errorf("XID %d is app-level and must have an occurrence threshold", code)
		}
	}
}

func TestClassifyXIDUnknown(t *testing.T) {
	if _, ok := ClassifyXID(9999); ok {
		t.Error("unknown XID must not be actionable")
	}
}

func TestSignalFromAgentEvent(t *testing.T) {
	ev := types.AgentEvent{
		Node: "node07", GPUIndex: 3, GPUUUID: "GPU-abc",
		XID: 79, Raw: "NVRM: Xid ...", Timestamp: time.Now(),
	}
	sig, ok := SignalFromAgentEvent(ev)
	if !ok {
		t.Fatal("XID 79 must produce a signal")
	}
	if sig.Class != types.ClassFellOffBus || sig.Target.Node != "node07" || sig.Target.GPUUUID != "GPU-abc" {
		t.Errorf("unexpected signal: %+v", sig)
	}
	if sig.Evidence["xid"] != "79" {
		t.Errorf("evidence xid = %q, want 79", sig.Evidence["xid"])
	}
}

// A spoofed or malformed severity label at the webhook must not become an
// authoritative severity that could drive escalation: it is ignored in favor of
// the validated default. A known severity still wins.
func TestSignalFromAlertIgnoresUnknownSeverity(t *testing.T) {
	base := Alert{
		Status: "firing",
		Labels: map[string]string{"alertname": "GpuRowRemapFailure", "node": "n1"},
	}

	base.Labels["severity"] = "emergency" // not a known severity
	sig, ok := SignalFromAlert(base)
	if !ok {
		t.Fatal("a known firing alert must still map even with a bad severity label")
	}
	if sig.Severity != types.SeverityWarning {
		t.Fatalf("unknown severity must fall back to the validated default; got %q", sig.Severity)
	}

	base.Labels["severity"] = "critical" // a known severity is honored
	sig, _ = SignalFromAlert(base)
	if sig.Severity != types.SeverityCritical {
		t.Fatalf("a known severity must be honored; got %q", sig.Severity)
	}
}

func TestSignalFromAlert(t *testing.T) {
	a := Alert{
		Status: "firing",
		Labels: map[string]string{
			"alertname": "GpuRowRemapFailure",
			"severity":  "critical",
			"node":      "node03",
			"UUID":      "GPU-def",
			"gpu":       "5",
		},
		StartsAt: time.Now(),
	}
	sig, ok := SignalFromAlert(a)
	if !ok {
		t.Fatal("known firing alert must produce a signal")
	}
	if sig.Class != types.ClassRowRemapFailure {
		t.Errorf("class = %s, want %s", sig.Class, types.ClassRowRemapFailure)
	}
	if sig.Target.Node != "node03" || sig.Target.GPUUUID != "GPU-def" || sig.Target.GPUIndex != 5 {
		t.Errorf("unexpected target: %+v", sig.Target)
	}

	a.Status = "resolved"
	if _, ok := SignalFromAlert(a); ok {
		t.Error("resolved alerts must not produce signals")
	}

	a.Status = "firing"
	a.Labels["alertname"] = "SomethingUnknown"
	if _, ok := SignalFromAlert(a); ok {
		t.Error("unknown alert names must not produce signals")
	}
}

// TestAlertMapMatchesShippedRules cross-checks alertClassMap against the
// alert names defined in configs/vmalert/gpu-rules.yaml so code and rules
// cannot drift apart silently.
func TestAlertMapMatchesShippedRules(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "configs", "vmalert", "gpu-rules.yaml"))
	if err != nil {
		t.Fatalf("reading shipped rules: %v", err)
	}
	re := regexp.MustCompile(`(?m)^\s*- alert:\s*(\S+)`)
	shipped := map[string]bool{}
	for _, m := range re.FindAllStringSubmatch(string(data), -1) {
		shipped[m[1]] = true
	}
	if len(shipped) == 0 {
		t.Fatal("no alerts found in shipped rules — parsing broken?")
	}
	for name := range shipped {
		if _, ok := alertClassMap[name]; !ok {
			t.Errorf("shipped alert %q has no class mapping in alertClassMap", name)
		}
	}
	for _, name := range KnownAlertNames() {
		if !shipped[name] {
			t.Errorf("alertClassMap entry %q has no shipped vmalert rule", name)
		}
	}
}

func TestXID92HasDedicatedClass(t *testing.T) {
	info, ok := ClassifyXID(92)
	if !ok {
		t.Fatal("XID 92 must be classified")
	}
	if info.Class == types.ClassECCDBE {
		t.Fatal("XID 92 (corrected SBE rate) must not share the critical DBE class")
	}
	if info.Class != types.ClassECCSBERate {
		t.Fatalf("XID 92 class = %s, want %s", info.Class, types.ClassECCSBERate)
	}
}

func TestSignalFromAlertNormalizesInstanceToHost(t *testing.T) {
	for _, tc := range []struct {
		labels map[string]string
		want   string
	}{
		// node label wins untouched.
		{map[string]string{"alertname": "GpuExporterDown", "node": "gpu-node-1", "instance": "10.0.0.5:9400"}, "gpu-node-1"},
		// Hostname (dcgm-exporter) preferred over instance.
		{map[string]string{"alertname": "GpuExporterDown", "Hostname": "gpu-node-2", "instance": "10.0.0.5:9400"}, "gpu-node-2"},
		// instance fallback loses its port.
		{map[string]string{"alertname": "GpuExporterDown", "instance": "gpu-node-3:9400"}, "gpu-node-3"},
		// bare-host instance passes through.
		{map[string]string{"alertname": "GpuExporterDown", "instance": "gpu-node-4"}, "gpu-node-4"},
	} {
		sig, ok := SignalFromAlert(Alert{Status: "firing", Labels: tc.labels})
		if !ok {
			t.Fatalf("alert %v must map to a signal", tc.labels)
		}
		if sig.Target.Node != tc.want {
			t.Errorf("labels %v: node = %q, want %q", tc.labels, sig.Target.Node, tc.want)
		}
	}
}

// The catalog's occurrence thresholds are enforced through ObservePolicy,
// not merely documented: XID 13 (3/1h) must surface, an override must win,
// and classes without thresholds must report ok=false.
func TestCatalogObservePolicy(t *testing.T) {
	// XIDs 13/31 (3/1h) and 43 (10/24h) share ClassXIDApp; the largest
	// threshold wins as the fail-closed direction.
	threshold, window, ok := (*Catalog)(nil).ObservePolicy(types.ClassXIDApp)
	if !ok || threshold != 10 || window != 24*time.Hour {
		t.Fatalf("built-in xid-app policy = %d/%s ok=%v, want 10/24h true", threshold, window, ok)
	}
	if _, _, ok := (*Catalog)(nil).ObservePolicy(types.ClassECCDBE); ok {
		t.Fatal("classes without a catalog threshold must report ok=false")
	}

	over, err := NewCatalog([]config.SignalOverride{{
		Name: "xid13-override", XIDCodes: []int{13},
		Class: types.ProblemClass("custom-13"), Severity: types.SeverityWarning,
	}})
	if err != nil {
		t.Fatal(err)
	}
	// The override replaces XID 13 entirely (no threshold declared); the
	// class threshold must now come only from the remaining built-ins.
	if threshold, _, ok := over.ObservePolicy(types.ClassXIDApp); !ok || threshold != 10 {
		t.Fatalf("post-override xid-app threshold = %d ok=%v, want 10 from XIDs 31/43", threshold, ok)
	}
}

// TestDeployedSelfRulesContainCanonicalAlerts pins the deployed VMRule to the
// canonical SELF-health rules the same way the signal rules are pinned: an
// operator running their own Prometheus loads configs/vmalert/self-rules.yaml
// directly, and the two must not fork.
func TestDeployedSelfRulesContainCanonicalAlerts(t *testing.T) {
	canonical, err := os.ReadFile(filepath.Join("..", "..", "configs", "vmalert", "self-rules.yaml"))
	if err != nil {
		t.Fatalf("reading canonical self rules: %v", err)
	}
	deployed, err := os.ReadFile(filepath.Join("..", "..", "deploy", "kubernetes", "dependencies", "observability", "rules.yaml"))
	if err != nil {
		t.Fatalf("reading deployed rules: %v", err)
	}
	re := regexp.MustCompile(`(?m)^\s*- alert:\s*(\S+)`)
	deployedNames := map[string]bool{}
	for _, m := range re.FindAllStringSubmatch(string(deployed), -1) {
		deployedNames[m[1]] = true
	}
	count := 0
	for _, m := range re.FindAllStringSubmatch(string(canonical), -1) {
		count++
		if !deployedNames[m[1]] {
			t.Errorf("canonical self alert %q is missing from the deployed VMRule", m[1])
		}
	}
	if count == 0 {
		t.Fatal("no alerts found in the canonical self rules — parsing broken?")
	}
}

// TestDeployedRulesContainCanonicalAlerts pins the deployed VMRule
// (deploy/kubernetes/dependencies/observability/rules.yaml) to the canonical
// vmalert rules file: every canonical signal alert must ship in the deployed
// profile, so the two files cannot fork silently again.
func TestDeployedRulesContainCanonicalAlerts(t *testing.T) {
	canonical, err := os.ReadFile(filepath.Join("..", "..", "configs", "vmalert", "gpu-rules.yaml"))
	if err != nil {
		t.Fatalf("reading canonical rules: %v", err)
	}
	deployed, err := os.ReadFile(filepath.Join("..", "..", "deploy", "kubernetes", "dependencies", "observability", "rules.yaml"))
	if err != nil {
		t.Fatalf("reading deployed rules: %v", err)
	}
	re := regexp.MustCompile(`(?m)^\s*- alert:\s*(\S+)`)
	deployedNames := map[string]bool{}
	for _, m := range re.FindAllStringSubmatch(string(deployed), -1) {
		deployedNames[m[1]] = true
	}
	canonicalCount := 0
	for _, m := range re.FindAllStringSubmatch(string(canonical), -1) {
		canonicalCount++
		if !deployedNames[m[1]] {
			t.Errorf("canonical alert %q is missing from the deployed VMRule", m[1])
		}
	}
	if canonicalCount == 0 {
		t.Fatal("no alerts found in canonical rules — parsing broken?")
	}
}
