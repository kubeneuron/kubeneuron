package detect

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/kubeneuron/kubeneuron/pkg/types"
)

// M5 safety boundary (v0.7): shadow-only GPU alerts exist for humans and can
// never reach the controller. The boundary has three independent levels, and
// each test here pins one of them against the shipped files, parsed as YAML
// rather than matched as text:
//
//  1. Names: every shadow alert is ShadowGpu*, never Gpu*, so the controller
//     webhook route's alertname regex cannot match it.
//  2. Routing: every shadow rule carries kubeneuron_shadow="true", which the
//     Alertmanager config routes to humans first (tested in internal/depprofile).
//  3. Code: no shadow name is in alertClassMap, so SignalFromAlert drops it
//     even if it were mis-routed. alertClassMap itself is pinned to its
//     pre-M5 golden list, so M5 cannot have changed it.
//
// Deliberately breaking any one of these (a shadow name in alertClassMap, a
// Gpu-prefixed shadow alert, a shadow rule without the label) must fail a
// test here.

const (
	shadowRulesPath    = "configs/vmalert/gpu-shadow-rules.yaml"
	recordingRulesPath = "configs/vmalert/gpu-recording-rules.yaml"
	deployedRulesPath  = "deploy/kubernetes/dependencies/observability/rules.yaml"

	shadowGroupName   = "gpu-shadow"
	shadowVMRuleName  = "kubeneuron-gpu-shadow"
	legacyVMRuleName  = "kubeneuron-gpu"
	shadowAlertPrefix = "ShadowGpu"
	shadowLabel       = "kubeneuron_shadow"
)

// alertRuleGroup is the subset of a vmalert group the shadow tests compare; a
// VMRule spec.groups entry and a plain rules-file group both decode to it.
type alertRuleGroup struct {
	Name     string      `yaml:"name"`
	Interval string      `yaml:"interval"`
	Rules    []alertRule `yaml:"rules"`
}

type alertRule struct {
	Alert       string            `yaml:"alert"`
	Record      string            `yaml:"record"`
	Expr        string            `yaml:"expr"`
	For         string            `yaml:"for"`
	Labels      map[string]string `yaml:"labels"`
	Annotations map[string]string `yaml:"annotations"`
}

func repoPath(rel string) string {
	return filepath.Join("..", "..", filepath.FromSlash(rel))
}

// loadRuleGroups parses a plain vmalert rules file.
func loadRuleGroups(t *testing.T, rel string) []alertRuleGroup {
	t.Helper()
	src, err := os.ReadFile(repoPath(rel))
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Groups []alertRuleGroup `yaml:"groups"`
	}
	if err := yaml.Unmarshal(src, &doc); err != nil {
		t.Fatalf("%s: not a vmalert rules file: %v", rel, err)
	}
	if len(doc.Groups) == 0 {
		t.Fatalf("%s: no groups; the harness is broken", rel)
	}
	return doc.Groups
}

// loadDeployedVMRuleGroups returns the groups of the one VMRule with the
// given name in the deployed multi-document manifest.
func loadDeployedVMRuleGroups(t *testing.T, name string) []alertRuleGroup {
	t.Helper()
	f, err := os.Open(repoPath(deployedRulesPath))
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := f.Close(); err != nil {
			t.Errorf("close %s: %v", deployedRulesPath, err)
		}
	}()
	dec := yaml.NewDecoder(f)
	var found []alertRuleGroup
	docs, matches := 0, 0
	for {
		var doc struct {
			Kind     string `yaml:"kind"`
			Metadata struct {
				Name string `yaml:"name"`
			} `yaml:"metadata"`
			Spec struct {
				Groups []alertRuleGroup `yaml:"groups"`
			} `yaml:"spec"`
		}
		err := dec.Decode(&doc)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("%s: document %d does not parse: %v", deployedRulesPath, docs+1, err)
		}
		docs++
		if doc.Kind == "VMRule" && doc.Metadata.Name == name {
			matches++
			found = doc.Spec.Groups
		}
	}
	if matches != 1 {
		t.Fatalf("%s: want exactly one VMRule named %s, found %d in %d documents", deployedRulesPath, name, matches, docs)
	}
	if len(found) == 0 {
		t.Fatalf("%s: VMRule %s has no groups", deployedRulesPath, name)
	}
	return found
}

// shadowRules returns the rules of the single gpu-shadow group in the
// canonical shadow file, failing if the file has any other shape.
func shadowRules(t *testing.T) []alertRule {
	t.Helper()
	groups := loadRuleGroups(t, shadowRulesPath)
	if len(groups) != 1 || groups[0].Name != shadowGroupName {
		names := make([]string, 0, len(groups))
		for _, g := range groups {
			names = append(names, g.Name)
		}
		t.Fatalf("%s: want exactly one group named %s (shadow rules are never mixed into controller-facing groups), got %v", shadowRulesPath, shadowGroupName, names)
	}
	if len(groups[0].Rules) == 0 {
		t.Fatalf("%s: group %s has no rules; the harness is broken", shadowRulesPath, shadowGroupName)
	}
	return groups[0].Rules
}

// The deployed VMRule kubeneuron-gpu-shadow must carry the canonical
// gpu-shadow group unchanged: same group name, interval, rule order and, per
// rule, the same alert, expr, for, labels and annotations. Indentation and
// comments are free to differ.
func TestShadowRulesMirrorDeployedVMRule(t *testing.T) {
	canonical := loadRuleGroups(t, shadowRulesPath)
	deployed := loadDeployedVMRuleGroups(t, shadowVMRuleName)
	if len(canonical) != len(deployed) {
		t.Fatalf("group count: canonical %d, deployed VMRule %s %d", len(canonical), shadowVMRuleName, len(deployed))
	}
	for i := range canonical {
		c, d := canonical[i], deployed[i]
		if c.Name != d.Name || c.Interval != d.Interval {
			t.Errorf("group %d: canonical %s/%s, deployed %s/%s", i, c.Name, c.Interval, d.Name, d.Interval)
		}
		if len(c.Rules) != len(d.Rules) {
			t.Fatalf("group %s: canonical has %d rules, deployed has %d", c.Name, len(c.Rules), len(d.Rules))
		}
		for j := range c.Rules {
			if !reflect.DeepEqual(c.Rules[j], d.Rules[j]) {
				t.Errorf("group %s rule %d (%s) differs:\n canonical: %+v\n deployed:  %+v", c.Name, j, c.Rules[j].Alert, c.Rules[j], d.Rules[j])
			}
		}
	}
}

// Level 1 and level 3 of the boundary: every shadow alert is ShadowGpu* and
// has no entry in alertClassMap. Also: the group holds alerts only (no
// record rules hide in it), and no shadow name collides with a shipped
// controller-facing name.
func TestShadowAlertsAreNamedShadowGpuAndAbsentFromAlertClassMap(t *testing.T) {
	for _, r := range shadowRules(t) {
		if r.Record != "" {
			t.Errorf("%s: record rule %q in the shadow group; the group is alerts only", shadowRulesPath, r.Record)
			continue
		}
		if !strings.HasPrefix(r.Alert, shadowAlertPrefix) {
			t.Errorf("%s: alert %q must begin with %s", shadowRulesPath, r.Alert, shadowAlertPrefix)
		}
		if strings.HasPrefix(r.Alert, "Gpu") {
			t.Errorf("%s: alert %q begins with Gpu and would match the controller webhook route", shadowRulesPath, r.Alert)
		}
		if class, ok := alertClassMap[r.Alert]; ok {
			t.Errorf("shadow alert %q is in alertClassMap (class %s); shadow alerts must never map to a problem class", r.Alert, class)
		}
	}
}

// Level 3 of the boundary, exercised rather than inspected: a firing shadow
// alert, with the exact labels the rule attaches plus the GPU identity the
// controller would correlate on, produces no signal through either the
// package-level path or the (nil, built-in) catalog path.
func TestSignalFromAlertReturnsFalseForEveryShadowAlert(t *testing.T) {
	for _, r := range shadowRules(t) {
		labels := map[string]string{
			"alertname": r.Alert,
			"node":      "gpu-node-1",
			"gpu":       "0",
			"gpu_uuid":  "GPU-00000000-0000-0000-0000-000000000000",
			"UUID":      "GPU-00000000-0000-0000-0000-000000000000",
		}
		for k, v := range r.Labels {
			labels[k] = v
		}
		a := Alert{Status: "firing", Labels: labels, Annotations: r.Annotations, StartsAt: time.Now()}
		if sig, ok := SignalFromAlert(a); ok {
			t.Errorf("SignalFromAlert(%s) = %+v, true; a shadow alert must never become a signal", r.Alert, sig)
		}
		if sig, ok := (*Catalog)(nil).SignalFromAlert(a); ok {
			t.Errorf("(*Catalog)(nil).SignalFromAlert(%s) = %+v, true; a shadow alert must never become a signal", r.Alert, sig)
		}
	}
}

// Golden list: alertClassMap exactly as it stood before M5 (v0.6.1). M5 adds
// shadow rules, routing and tests, and nothing else; any entry added here,
// removed or re-classed is a change to what the controller acts on and is
// out of M5's scope by definition.
func TestAlertClassMapGoldenPreM5(t *testing.T) {
	golden := map[string]types.ProblemClass{
		"GpuDoubleBitEccVolatile":  types.ClassECCDBE,
		"GpuDoubleBitEccAggregate": types.ClassECCDBE,
		"GpuRowRemapPending":       types.ClassRowRemapOK,
		"GpuRowRemapFailure":       types.ClassRowRemapFailure,
		"GpuRemappedRowsHigh":      types.ClassRowRemapBudget,
		"GpuThermalThrottle":       types.ClassThermal,
		"GpuTempCritical":          types.ClassThermal,
		"GpuPowerBrake":            types.ClassPower,
		"GpuNvlinkCrcErrors":       types.ClassNVLink,
		"GpuPcieReplayHigh":        types.ClassPCIe,
		"GpuExporterDown":          types.ClassDriverHang,
		"KubeNeuronAgentDown":      types.ClassAgentDown,
	}
	if reflect.DeepEqual(alertClassMap, golden) {
		return
	}
	for name, class := range golden {
		got, ok := alertClassMap[name]
		switch {
		case !ok:
			t.Errorf("alertClassMap lost %q (golden class %s)", name, class)
		case got != class:
			t.Errorf("alertClassMap[%q] = %s, golden %s", name, got, class)
		}
	}
	for name, class := range alertClassMap {
		if _, ok := golden[name]; !ok {
			t.Errorf("alertClassMap gained %q (class %s); not in the pre-M5 golden list", name, class)
		}
	}
}

// synthesisRE matches the ways a MetricsQL expression can turn missing data
// into a value: absent()/absent_over_time(), the default operator and an
// `or vector(0)` style fallback. No shadow rule may use any of them; a GPU
// whose exporter does not report an input has no shadow alert for it.
var synthesisRE = regexp.MustCompile(`absent(_over_time)?\s*\(|\bdefault\b|vector\s*\(\s*0(\.0*)?\s*\)`)

// metricNameRE finds metric-name-shaped tokens in an expression; label names
// inside {...} selectors are the same shape, so the test keeps only the
// tokens that name a recorded or exporter series by prefix.
var metricNameRE = regexp.MustCompile(`[a-zA-Z_:][a-zA-Z0-9_:]*`)

// Every shadow rule carries the two labels the boundary and the routing
// rely on, explains itself as shadow-only in its annotations, reads only
// series the M3 recording rules produce (never a dcgm-exporter series) and
// synthesises nothing.
func TestShadowRulesAreLabelledExplainedAndReadOnlyRecordedSeries(t *testing.T) {
	recorded := map[string]bool{}
	for _, g := range loadRuleGroups(t, recordingRulesPath) {
		for _, r := range g.Rules {
			if r.Record != "" {
				recorded[r.Record] = true
			}
		}
	}
	if len(recorded) == 0 {
		t.Fatalf("%s: no record rules; the harness is broken", recordingRulesPath)
	}

	for _, r := range shadowRules(t) {
		if got := r.Labels[shadowLabel]; got != "true" {
			t.Errorf("%s: labels.%s = %q, want \"true\"", r.Alert, shadowLabel, got)
		}
		if got := r.Labels["severity"]; got != "info" {
			t.Errorf("%s: labels.severity = %q, want info", r.Alert, got)
		}
		if r.Annotations["summary"] == "" {
			t.Errorf("%s: no summary annotation", r.Alert)
		}
		desc := strings.ToLower(r.Annotations["description"])
		for _, phrase := range []string{"shadow-only", "human review", "no remediation"} {
			if !strings.Contains(desc, phrase) {
				t.Errorf("%s: description must state %q; got %q", r.Alert, phrase, r.Annotations["description"])
			}
		}
		if r.Expr == "" {
			t.Errorf("%s: empty expr", r.Alert)
			continue
		}
		if m := synthesisRE.FindString(r.Expr); m != "" {
			t.Errorf("%s: expr synthesises missing data with %q; missing data must stay missing: %s", r.Alert, m, r.Expr)
		}
		reads := 0
		for _, tok := range metricNameRE.FindAllString(r.Expr, -1) {
			switch {
			case strings.HasPrefix(tok, "DCGM_"):
				t.Errorf("%s: expr reads exporter series %s directly; shadow rules read only recorded canonical or derived series", r.Alert, tok)
			case strings.HasPrefix(tok, "kubeneuron_"):
				reads++
				if !recorded[tok] {
					t.Errorf("%s: expr reads %s, which no rule in %s records", r.Alert, tok, recordingRulesPath)
				}
			}
		}
		if reads == 0 {
			t.Errorf("%s: expr reads no kubeneuron_* series: %s", r.Alert, r.Expr)
		}
	}
}

// The controller-facing VMRule stays controller-facing and the shadow
// VMRule stays shadow: no shadow alert, name or label leaks into
// kubeneuron-gpu, every alert in its gpu-* groups is one the controller maps,
// and kubeneuron-gpu-shadow holds nothing but ShadowGpu* alerts carrying the
// shadow label. This is the deployed-side twin of the canonical-file checks.
func TestDeployedVMRulesKeepControllerFacingAndShadowRulesApart(t *testing.T) {
	for _, g := range loadDeployedVMRuleGroups(t, legacyVMRuleName) {
		if g.Name == shadowGroupName {
			t.Errorf("VMRule %s carries group %s; shadow rules belong in VMRule %s", legacyVMRuleName, g.Name, shadowVMRuleName)
		}
		for _, r := range g.Rules {
			if r.Alert == "" {
				continue
			}
			if strings.HasPrefix(r.Alert, shadowAlertPrefix) {
				t.Errorf("VMRule %s group %s carries shadow alert %s", legacyVMRuleName, g.Name, r.Alert)
			}
			if v, ok := r.Labels[shadowLabel]; ok {
				t.Errorf("VMRule %s alert %s carries %s=%q; controller-facing alerts never carry the shadow label", legacyVMRuleName, r.Alert, shadowLabel, v)
			}
			if strings.HasPrefix(g.Name, "gpu-") {
				if _, ok := alertClassMap[r.Alert]; !ok {
					t.Errorf("VMRule %s group %s alert %s is not in alertClassMap; the gpu-* groups are controller-facing only", legacyVMRuleName, g.Name, r.Alert)
				}
			}
		}
	}

	var names []string
	for _, g := range loadDeployedVMRuleGroups(t, shadowVMRuleName) {
		if g.Name != shadowGroupName {
			t.Errorf("VMRule %s carries group %s; want only %s", shadowVMRuleName, g.Name, shadowGroupName)
		}
		for _, r := range g.Rules {
			names = append(names, r.Alert)
			if !strings.HasPrefix(r.Alert, shadowAlertPrefix) || r.Record != "" {
				t.Errorf("VMRule %s carries non-shadow rule alert=%q record=%q", shadowVMRuleName, r.Alert, r.Record)
			}
			if r.Labels[shadowLabel] != "true" {
				t.Errorf("VMRule %s alert %s lacks %s=\"true\"", shadowVMRuleName, r.Alert, shadowLabel)
			}
		}
	}
	sort.Strings(names)
	for i := 1; i < len(names); i++ {
		if names[i] == names[i-1] {
			t.Errorf("VMRule %s defines alert %s twice", shadowVMRuleName, names[i])
		}
	}
}
