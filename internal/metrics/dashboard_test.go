package metrics

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/kubeneuron/kubeneuron/internal/gpucatalog"
)

// The shipped Grafana dashboards are the only part of this product whose
// correctness nothing compiles. A renamed metric or a dropped label does not
// break a build, does not fail a test, and does not error at query time — the
// panel simply renders an empty graph, which reads as "nothing is wrong" at
// exactly the moment somebody is looking for what is wrong.
//
// Removing the node label from kubeneuron_workloads_evicted_total is the shape
// of the accident these tests exist to catch: a defensible change to Go code
// that silently empties a panel nobody thought to open.
//
// Two kinds of board are covered. The legacy kubeneuron-dashboard.json is
// hand-maintained and reads this package's series and the exporter's DCGM
// series directly. The five v0.7 boards are generated from
// internal/gpucatalog.DashboardSpecs and read canonical kubeneuron_gpu_*
// series, derived series, T4 control-plane series and vmalert's ALERTS.

const (
	repoRoot            = "../.."
	legacyDashboardPath = "deploy/grafana/kubeneuron-dashboard.json"
	gpuRulesPath        = "configs/vmalert/gpu-rules.yaml"
)

// metricNameRe finds our own metric names inside a PromQL expression.
var dashboardMetricRe = regexp.MustCompile(`kubeneuron_[a-z0-9_]+`)

// dcgmNameRe finds exporter series names inside a PromQL expression.
var dcgmNameRe = regexp.MustCompile(`DCGM_FI_[A-Z0-9_]+`)

// declaredMetricRe pulls a metric name out of this package's source. It spans
// both declaration shapes: the promauto helpers' `Name:` field and the custom
// collectors' prometheus.NewDesc, whose first argument is the name.
var declaredMetricRe = regexp.MustCompile(`(?:Name:\s*|NewDesc\(\s*)"(kubeneuron_[a-z0-9_]+)"`)

// labelSetRe pulls the label slice that follows a metric declaration, in
// either shape: `}, []string{"a","b"})` for promauto, `[]string{"a"}, nil)`
// for NewDesc.
var labelSetRe = regexp.MustCompile(`\[\]string\{([^}]*)\}`)

// promLabelNameRe is the Prometheus label-name grammar. Kubernetes-style
// names such as kubeneuron.io/shadow are not label names and cannot appear
// in a selector.
var promLabelNameRe = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)

type dashboard struct {
	Path       string
	UID        string     `json:"uid"`
	Panels     []panel    `json:"panels"`
	Templating templating `json:"templating"`
}

type templating struct {
	List []variable `json:"list"`
}

type variable struct {
	Name       string `json:"name"`
	Type       string `json:"type"`
	IncludeAll bool   `json:"includeAll"`
	Multi      bool   `json:"multi"`
	AllValue   string `json:"allValue"`
	Definition string `json:"definition"`
	Query      struct {
		Query string `json:"query"`
	} `json:"query"`
	Current struct {
		Text []string `json:"text"`
	} `json:"current"`
}

type panel struct {
	ID      int      `json:"id"`
	Title   string   `json:"title"`
	Panels  []panel  `json:"panels"`
	Targets []target `json:"targets"`
	Links   []link   `json:"links"`
	GridPos struct {
		H, W, X, Y int
	} `json:"gridPos"`
}

type target struct {
	Expr string `json:"expr"`
}

type link struct {
	Title string `json:"title"`
	URL   string `json:"url"`
}

func loadDashboard(t *testing.T, rel string) dashboard {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(repoRoot, filepath.Clean(rel)))
	if err != nil {
		t.Fatalf("the shipped dashboard must be readable: %v", err)
	}
	var d dashboard
	if err := json.Unmarshal(raw, &d); err != nil {
		t.Fatalf("%s is not valid JSON: %v", rel, err)
	}
	d.Path = rel
	return d
}

// loadLegacy returns the hand-maintained overview board.
func loadLegacy(t *testing.T) dashboard { return loadDashboard(t, legacyDashboardPath) }

// loadGenerated returns the five generated boards, in generator order.
func loadGenerated(t *testing.T) []dashboard {
	t.Helper()
	paths := gpucatalog.DashboardPaths()
	if len(paths) != 5 {
		t.Fatalf("expected 5 generated dashboards, generator lists %d: %v", len(paths), paths)
	}
	var out []dashboard
	for _, p := range paths {
		out = append(out, loadDashboard(t, p))
	}
	return out
}

// loadAll returns the legacy board followed by the generated ones.
func loadAll(t *testing.T) []dashboard {
	return append([]dashboard{loadLegacy(t)}, loadGenerated(t)...)
}

func eachPanel(panels []panel, fn func(panel)) {
	for _, p := range panels {
		fn(p)
		eachPanel(p.Panels, fn)
	}
}

func eachTarget(d dashboard, fn func(panel, target)) {
	eachPanel(d.Panels, func(p panel) {
		for _, tg := range p.Targets {
			fn(p, tg)
		}
	})
}

// declaredMetrics reads this package's own source for the metric names and
// label sets it registers. Reading the source rather than the registry is
// deliberate: a CounterVec has no series until something increments it, so a
// gathered registry cannot answer "which labels does this metric have".
func declaredMetrics(t *testing.T) map[string]map[string]bool {
	t.Helper()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	var text string
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		// A sentinel between files so a lookahead window cannot run off the
		// end of one declaration into another file's label slice.
		text += string(src) + "\n//---END---\n"
	}
	out := map[string]map[string]bool{}
	for _, m := range declaredMetricRe.FindAllStringSubmatchIndex(text, -1) {
		name := text[m[2]:m[3]]
		labels := map[string]bool{}
		// Look ahead a bounded distance for the label slice that closes this
		// declaration; a metric with no labels has none before the next one.
		window := text[m[1]:min(len(text), m[1]+400)]
		if next := declaredMetricRe.FindStringIndex(window); next != nil {
			window = window[:next[0]]
		}
		if ls := labelSetRe.FindStringSubmatch(window); ls != nil {
			for _, l := range strings.Split(ls[1], ",") {
				l = strings.Trim(strings.TrimSpace(l), `"`)
				if l != "" {
					labels[l] = true
				}
			}
		}
		out[name] = labels
	}
	if len(out) < 10 {
		t.Fatalf("only found %d declared metrics; the declaration shape changed and this test stopped checking anything", len(out))
	}
	return out
}

// catalogSeries returns every kubeneuron_* series name the GPU catalog
// records: canonical kubeneuron_gpu_* series, recorded derived series and
// the T4 control-plane series it lists. A derived series marked deferred
// (kubeneuron_gpu_present) is never recorded and is deliberately absent, so
// a panel reading it would be No data forever.
func catalogSeries(t *testing.T) map[string]bool {
	t.Helper()
	c := gpucatalog.Default()
	out := map[string]bool{}
	for _, s := range c.CanonicalSeries() {
		out[s] = true
	}
	for _, d := range c.Derived {
		if d.Deferred == "" {
			out[d.Series] = true
		}
	}
	for _, f := range c.Fields {
		if f.Tier == gpucatalog.TierT4 {
			out[f.Series] = true
		}
	}
	if len(out) < 20 {
		t.Fatalf("catalog lists only %d series; the catalog shape changed and this test stopped checking anything", len(out))
	}
	return out
}

// profileSeries returns the DCGM series names the shipped exporter profile
// (v0.7 M2) configures dcgm-exporter to emit, spelled as the pinned exporter
// publishes them.
func profileSeries(t *testing.T) map[string]bool {
	t.Helper()
	rows, err := gpucatalog.ShippedProfile(repoRoot)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]bool{}
	for _, r := range rows {
		out[r.Series] = true
	}
	if len(out) < 10 {
		t.Fatalf("shipped profile lists only %d fields", len(out))
	}
	return out
}

// shippedAlertNames returns every `alert:` name in the GPU alert pack.
func shippedAlertNames(t *testing.T) map[string]bool {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(repoRoot, gpuRulesPath))
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]bool{}
	for _, m := range regexp.MustCompile(`(?m)^\s*-\s*alert:\s*(\S+)`).FindAllStringSubmatch(string(raw), -1) {
		out[m[1]] = true
	}
	if len(out) < 5 {
		t.Fatalf("found only %d alerts in %s", len(out), gpuRulesPath)
	}
	return out
}

// metricNames returns the kubeneuron_* series names an expression reads. A
// kubeneuron_* token followed by a matcher operator is a label name inside
// a selector (ALERTS{kubeneuron_shadow="true"}), not a series.
func metricNames(expr string) []string {
	var out []string
	for _, m := range dashboardMetricRe.FindAllStringIndex(expr, -1) {
		rest := strings.TrimLeft(expr[m[1]:], " ")
		if strings.HasPrefix(rest, "=") || strings.HasPrefix(rest, "!") {
			continue
		}
		out = append(out, expr[m[0]:m[1]])
	}
	return out
}

func trimHistogramSuffix(name string) string {
	base := strings.TrimSuffix(strings.TrimSuffix(name, "_bucket"), "_sum")
	return strings.TrimSuffix(base, "_count")
}

// TestDashboardQueriesNameRealMetrics fails when a panel on any board queries
// a kubeneuron_* series that neither this package registers nor the GPU
// catalog records, or a DCGM_FI_* series the shipped exporter profile does
// not emit.
func TestDashboardQueriesNameRealMetrics(t *testing.T) {
	declared := declaredMetrics(t)
	cataloged := catalogSeries(t)
	profiled := profileSeries(t)

	// The probes must be able to fail: a deferred derived series and a
	// cataloged-but-unprofiled DCGM field are both unknown.
	if cataloged["kubeneuron_gpu_present"] {
		t.Fatal("kubeneuron_gpu_present is deferred and must not count as a readable series")
	}
	if profiled["DCGM_FI_DEV_XID_ERRORS"] {
		t.Fatal("DCGM_FI_DEV_XID_ERRORS is not in the shipped profile; the profile probe is wrong")
	}

	var missing []string
	for _, d := range loadAll(t) {
		eachTarget(d, func(p panel, tg target) {
			for _, name := range metricNames(tg.Expr) {
				// Recording-rule style suffixes Prometheus appends to a
				// histogram are not separate declarations.
				base := trimHistogramSuffix(name)
				if declared[name] != nil || declared[base] != nil || cataloged[name] || cataloged[base] {
					continue
				}
				missing = append(missing, d.Path+": "+p.Title+": "+name)
			}
			for _, name := range dcgmNameRe.FindAllString(tg.Expr, -1) {
				if !profiled[name] {
					missing = append(missing, d.Path+": "+p.Title+": "+name+" (not in the shipped exporter profile)")
				}
			}
		})
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Fatalf("dashboard panels query series nothing produces — each renders empty and reads as healthy:\n  %s",
			strings.Join(missing, "\n  "))
	}
}

// TestDashboardQueriesUseRealLabels fails when a panel groups by or matches on
// a label the metric does not carry. This is the failure that survives a
// rename: `sum by (node) (...)` on a metric with no node label collapses to a
// single unlabelled series rather than erroring.
//
// Only series this package declares are checked: their label sets are read
// from source. Catalog series (kubeneuron_gpu_*) carry the exporter's
// identity and placement labels plus the catalog's fixed labels, which the
// generator tests in internal/gpucatalog pin.
func TestDashboardQueriesUseRealLabels(t *testing.T) {
	declared := declaredMetrics(t)

	// by (a, b) / without (a, b) immediately around one of our metrics.
	groupRe := regexp.MustCompile(`(?:by|without)\s*\(([^)]*)\)`)
	// And the label MATCHERS inside a selector: kubeneuron_incidents{state="X"}.
	// The doc for this test always said "groups by OR MATCHES ON", but only
	// the grouping half was ever parsed — so a panel selecting on a label the
	// metric does not carry matched nothing and rendered an empty graph, which
	// is the same silent failure as the grouping case and the more common way
	// this dashboard is written: it already uses matchers in five panels.
	matcherRe := regexp.MustCompile(`kubeneuron_[a-z0-9_]+\s*\{([^}]*)\}`)
	labelInMatcherRe := regexp.MustCompile(`([a-zA-Z_][a-zA-Z0-9_]*)\s*(?:=~|!~|!=|=)`)
	// Labels Prometheus itself adds to every series, plus the ones scrape
	// configs attach; a panel may legitimately group on these. The template
	// variables of the generated boards are placement labels the scrape
	// attaches (cluster, node_pool) or that only attributed series carry.
	infra := map[string]bool{
		"instance": true, "job": true, "namespace": true, "pod": true,
		"container": true, "cluster": true, "le": true,
	}
	for _, v := range gpucatalog.DashboardVariables {
		infra[v] = true
	}

	var bad []string
	for _, d := range loadAll(t) {
		eachTarget(d, func(p panel, tg target) {
			names := metricNames(tg.Expr)
			if len(names) == 0 {
				return
			}
			// Union of the labels every metric in this expression carries. A
			// panel combining two metrics may group by any of their labels.
			// A catalog series is not declared here and is not checked.
			allowed := map[string]bool{}
			checkable := false
			for _, n := range names {
				for _, key := range []string{n, trimHistogramSuffix(n)} {
					if declared[key] != nil {
						checkable = true
					}
					for l := range declared[key] {
						allowed[l] = true
					}
				}
			}
			if !checkable {
				return
			}
			for _, g := range groupRe.FindAllStringSubmatch(tg.Expr, -1) {
				for _, l := range strings.Split(g[1], ",") {
					l = strings.TrimSpace(l)
					if l == "" || infra[l] || allowed[l] {
						continue
					}
					bad = append(bad, d.Path+": "+p.Title+": groups by "+l+", which none of "+strings.Join(names, ", ")+" carries")
				}
			}
			// A matcher is checked against ITS OWN metric, not the union: a
			// panel combining two metrics may group by either's labels, but
			// {state="X"} on one of them has to be that one's label.
			for _, m := range matcherRe.FindAllStringSubmatch(tg.Expr, -1) {
				name := dashboardMetricRe.FindString(m[0])
				own := map[string]bool{}
				ownDeclared := false
				for _, key := range []string{name, trimHistogramSuffix(name)} {
					if declared[key] != nil {
						ownDeclared = true
					}
					for l := range declared[key] {
						own[l] = true
					}
				}
				if !ownDeclared {
					continue
				}
				for _, lm := range labelInMatcherRe.FindAllStringSubmatch(m[1], -1) {
					l := lm[1]
					if infra[l] || own[l] {
						continue
					}
					bad = append(bad, d.Path+": "+p.Title+": matches on "+l+"=…, which "+name+" does not carry")
				}
			}
		})
	}
	sort.Strings(bad)
	if len(bad) > 0 {
		t.Fatalf("dashboard panels group by labels their metrics do not have — the series collapses instead of erroring:\n  %s",
			strings.Join(bad, "\n  "))
	}
}

// notCharted lists metrics that deliberately have no panel, each with the
// reason. Everything else this package registers must appear somewhere in
// a shipped dashboard.
//
// The two tests above check dashboard -> code: a panel may not name a metric
// or a label that does not exist. Nothing checked code -> dashboard, and that
// direction was the one actually failing. Twelve of twenty-nine registered
// metrics had no panel at all, including the whole protection family —
// workloads_evicted_total, destructive_steps_deferred_total, degraded_gpus —
// which docs/reference-metrics.md presents as the evidence for what
// remediation cost, what it deliberately did not cost, and how much capacity
// is currently lost. An operator following the documentation to the shipped
// dashboard found none of it.
//
// A metric belongs here when it is genuinely for alerting or for answering a
// question at the console, not for watching. Adding a line is fine; adding one
// without a reason is not.
var notCharted = map[string]string{
	"kubeneuron_runtime_config_info":                 "an info gauge: identity of the loaded config, read by /api/v1/runtime-config and by alerts, not watched over time",
	"kubeneuron_auth_failures_total":                 "alert-only (KubeNeuronAuthFailureBurst); a chart invites watching a number that should page",
	"kubeneuron_stack_restore_failures_total":        "alert-only (KubeNeuronStackRestoreFailing)",
	"kubeneuron_agent_events_rejected_total":         "alert-only (KubeNeuronAgentEventsRejected)",
	"kubeneuron_agent_registration_acks_total":       "alert-only (KubeNeuronAgentNeverAcked)",
	"kubeneuron_agent_detections_total":              "per-agent counter; the fleet view is the signal-rate panel, which is what an operator actually asks",
	"kubeneuron_agent_detections_deduplicated_total": "the dedup ratio is a debugging question, answered ad hoc",
	"kubeneuron_events_duplicate_total":              "same: a dedup counter, not a fleet health signal",
	"kubeneuron_decision_evaluations_total":          "the native readiness and audit surfaces show per-decision detail; alerting/recording rules consume this bounded distribution rather than a dashboard counter",
	"kubeneuron_decision_evaluation_seconds":         "diagnostic performance metric; use it in a preview/readiness SLO rather than a permanent panel",
	"kubeneuron_decision_stale_evidence_total":       "alert-only: a growing rate means a source is unhealthy, while the console identifies affected nodes",
	"kubeneuron_decision_compatibility_holds_total":  "release-compatibility detector; it should trend to zero before the legacy reset gate can be retired",
}

// TestEveryMetricIsChartedOrExcused fails when this package registers a metric
// that no panel on any board reads and no line above excuses.
func TestEveryMetricIsChartedOrExcused(t *testing.T) {
	declared := declaredMetrics(t)

	charted := map[string]bool{}
	for _, d := range loadAll(t) {
		eachTarget(d, func(_ panel, tg target) {
			for _, name := range metricNames(tg.Expr) {
				charted[name] = true
				charted[trimHistogramSuffix(name)] = true
			}
		})
	}

	var uncharted []string
	for name := range declared {
		if charted[name] {
			continue
		}
		if _, excused := notCharted[name]; excused {
			continue
		}
		uncharted = append(uncharted, name)
	}
	sort.Strings(uncharted)
	if len(uncharted) > 0 {
		t.Fatalf("metrics this build registers appear in no dashboard panel and are not listed in notCharted:\n  %s\n"+
			"add a panel, or add a line to notCharted saying why an operator does not need one",
			strings.Join(uncharted, "\n  "))
	}

	// And the excuse list must not outlive the metric it excuses, or it
	// quietly becomes permission for a panel nobody will ever add back.
	var stale []string
	for name := range notCharted {
		if _, ok := declared[name]; !ok {
			stale = append(stale, name)
		}
	}
	sort.Strings(stale)
	if len(stale) > 0 {
		t.Fatalf("notCharted excuses metrics this build no longer registers:\n  %s", strings.Join(stale, "\n  "))
	}
}

// --- v0.7 generated boards -------------------------------------------------

// TestGeneratedDashboardsAreTheGeneratorOutput fails when a checked-in board
// differs from what internal/gpucatalog renders from its one spec, so a
// hand edit to the JSON cannot survive. It also pins the UIDs the legacy
// board and the docs link to.
func TestGeneratedDashboardsAreTheGeneratorOutput(t *testing.T) {
	rendered, err := gpucatalog.Render(gpucatalog.Default())
	if err != nil {
		t.Fatal(err)
	}
	wantUID := map[string]string{
		gpucatalog.PathDashboardFleet:       gpucatalog.UIDDashboardFleet,
		gpucatalog.PathDashboardHealth:      gpucatalog.UIDDashboardHealth,
		gpucatalog.PathDashboardFailSlow:    gpucatalog.UIDDashboardFailSlow,
		gpucatalog.PathDashboardRemediation: gpucatalog.UIDDashboardRemediation,
		gpucatalog.PathDashboardPassport:    gpucatalog.UIDDashboardPassport,
	}
	seen := map[string]bool{}
	for _, d := range loadGenerated(t) {
		got, err := os.ReadFile(filepath.Join(repoRoot, d.Path))
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != string(rendered[d.Path]) {
			t.Errorf("%s is not the generator output; run `make generate-gpucatalog`", d.Path)
		}
		if d.UID != wantUID[d.Path] {
			t.Errorf("%s: uid %q, want %q", d.Path, d.UID, wantUID[d.Path])
		}
		if seen[d.UID] {
			t.Errorf("uid %q is used by more than one board", d.UID)
		}
		seen[d.UID] = true
		if len(d.Panels) == 0 {
			t.Errorf("%s has no panels", d.Path)
		}
	}
	if legacy := loadLegacy(t); seen[legacy.UID] {
		t.Errorf("legacy board uid %q collides with a generated board", legacy.UID)
	}
}

// TestGeneratedDashboardsCarryEveryVariable fails when a generated board
// lacks one of the seven shared template variables, or carries one that is
// not All/multi with allValue ".*" (the S3 label contract: a board on a
// cluster that never sets the label still renders), or references a $var
// no variable defines.
func TestGeneratedDashboardsCarryEveryVariable(t *testing.T) {
	varRefRe := regexp.MustCompile(`\$([a-zA-Z_][a-zA-Z0-9_]*)`)
	want := gpucatalog.DashboardVariables
	if len(want) != 7 {
		t.Fatalf("the shared variable list has %d entries, the plan fixes 7: %v", len(want), want)
	}
	for _, d := range loadGenerated(t) {
		var got []string
		for _, v := range d.Templating.List {
			got = append(got, v.Name)
			switch {
			case !v.IncludeAll || !v.Multi:
				t.Errorf("%s: variable %s must be includeAll and multi", d.Path, v.Name)
			case v.AllValue != ".*":
				t.Errorf("%s: variable %s allValue %q, want \".*\" so All matches an absent label", d.Path, v.Name, v.AllValue)
			case len(v.Current.Text) != 1 || v.Current.Text[0] != "All":
				t.Errorf("%s: variable %s must default to All, got %v", d.Path, v.Name, v.Current.Text)
			}
		}
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Errorf("%s: variables %v, want %v", d.Path, got, want)
		}
		defined := map[string]bool{}
		for _, v := range want {
			defined[v] = true
		}
		eachTarget(d, func(p panel, tg target) {
			for _, m := range varRefRe.FindAllStringSubmatch(tg.Expr, -1) {
				if m[1] == "__interval" || m[1] == "__rate_interval" || m[1] == "__range" {
					continue
				}
				if !defined[m[1]] {
					t.Errorf("%s: %s: expr references $%s, which no variable defines", d.Path, p.Title, m[1])
				}
			}
		})
	}
}

// TestGeneratedDashboardVariablesReadRealSeries parses every template
// variable query on every generated board and fails unless it is
// label_values(<series>, <variable>) over a series that exists on every
// installation: a canonical kubeneuron_gpu_* series whose source field the
// shipped exporter profile emits, or a recorded (non-deferred) derived
// series. A variable over a deferred series (kubeneuron_gpu_present is the
// one that exists) has an empty dropdown, so every board degrades to All
// and the selector never narrows — the operator sees the fleet when they
// asked for one GPU and nothing tells them why. A variable over a T4
// control-plane series is wrong for the opposite reason: those series carry
// no device or workload label, so the dropdown is empty too.
func TestGeneratedDashboardVariablesReadRealSeries(t *testing.T) {
	labelValuesRe := regexp.MustCompile(`^label_values\(\s*([a-zA-Z_:][a-zA-Z0-9_:]*)\s*,\s*([a-zA-Z_][a-zA-Z0-9_]*)\s*\)$`)
	c := gpucatalog.Default()
	cataloged := catalogSeries(t)
	profiled := profileSeries(t)

	// The probe must be able to fail: the deferred series is not readable.
	if cataloged["kubeneuron_gpu_present"] {
		t.Fatal("kubeneuron_gpu_present is deferred and must not count as a readable series")
	}
	control := map[string]bool{}
	for _, f := range c.Fields {
		if f.Tier == gpucatalog.TierT4 {
			control[f.Series] = true
		}
	}
	// readable reports whether a series has a sample on every installation
	// that runs the shipped profile and recording rules.
	readable := func(series string) (ok bool, why string) {
		if !cataloged[series] {
			return false, "not a canonical, recorded derived or T4 series in the catalog"
		}
		if control[series] {
			return false, "a T4 control-plane series, which carries no device or workload label"
		}
		if rows := c.LookupDerived(series); len(rows) > 0 {
			for _, r := range rows {
				if r.Deferred != "" {
					return false, "a deferred derived series that no rule records"
				}
			}
			return true, ""
		}
		fields := c.LookupCanonical(series)
		if len(fields) == 0 {
			return false, "not fed by any cataloged field"
		}
		for _, f := range fields {
			if f.ExporterExposed && profiled[f.Series] {
				return true, ""
			}
		}
		return false, "none of its source fields is in the shipped exporter profile"
	}
	if ok, _ := readable("kubeneuron_gpu_present"); ok {
		t.Fatal("the readability probe accepts the deferred kubeneuron_gpu_present")
	}
	if ok, _ := readable("kubeneuron_incidents"); ok {
		t.Fatal("the readability probe accepts a T4 control-plane series as a variable source")
	}

	for _, d := range loadGenerated(t) {
		for _, v := range d.Templating.List {
			if v.Type != "query" {
				t.Errorf("%s: variable %s has type %q, want \"query\"", d.Path, v.Name, v.Type)
				continue
			}
			if v.Definition != v.Query.Query {
				t.Errorf("%s: variable %s: definition %q differs from query %q; Grafana shows the definition and runs the query", d.Path, v.Name, v.Definition, v.Query.Query)
			}
			m := labelValuesRe.FindStringSubmatch(v.Query.Query)
			if m == nil {
				t.Errorf("%s: variable %s: query %q is not label_values(<series>, <label>)", d.Path, v.Name, v.Query.Query)
				continue
			}
			series, label := m[1], m[2]
			if label != v.Name {
				t.Errorf("%s: variable %s reads label %s; the selector substitutes $%s into %s=~, so the dropdown would offer values of the wrong label", d.Path, v.Name, label, v.Name, v.Name)
			}
			if ok, why := readable(series); !ok {
				t.Errorf("%s: variable %s reads %s, %s; the dropdown is empty and every board silently shows the whole fleet", d.Path, v.Name, series, why)
			}
		}
	}
}

// TestGeneratedDashboardsGroupCatalogSeriesByCarriedLabels fails when a
// panel on a generated board groups a catalog kubeneuron_gpu_* series by, or
// matches it on, a label the catalog says the series does not carry. A
// canonical series carries the exporter's identity and placement labels
// (the template variables) plus the fixed labels its catalog rows add; a
// derived series carries the variables, the fixed labels its rule adds and
// the input labels it is declared to carry. Anything else collapses the
// grouping or empties the panel without an error. The T4 half of this check
// is TestDashboardQueriesUseRealLabels, which reads the label sets from this
// package's source.
func TestGeneratedDashboardsGroupCatalogSeriesByCarriedLabels(t *testing.T) {
	c := gpucatalog.Default()
	nameSelRe := regexp.MustCompile(`(kubeneuron_gpu_[a-z0-9_]+)\s*\{([^}]*)\}`)
	groupRe := regexp.MustCompile(`(?:by|without)\s*\(([^)]*)\)`)
	labelInMatcherRe := regexp.MustCompile(`([a-zA-Z_][a-zA-Z0-9_]*)\s*(?:=~|!~|!=|=)\s*"([^"]*)"`)

	// carried returns the labels a catalog series carries beyond the
	// template variables, and whether the catalog knows the series at all.
	carried := func(series string) (map[string]bool, bool) {
		out := map[string]bool{}
		for _, v := range gpucatalog.DashboardVariables {
			out[v] = true
		}
		known := false
		for _, f := range c.LookupCanonical(series) {
			if f.Tier == gpucatalog.TierT4 {
				// A control-plane counter: its labels come from this
				// package's declarations, not from a canonical mapping.
				continue
			}
			known = true
			for _, l := range f.Canonical.Labels {
				out[l.Name] = true
			}
			for _, l := range f.Canonical.ValueLabels {
				out[l] = true
			}
		}
		for _, r := range c.LookupDerived(series) {
			known = true
			for _, l := range r.Labels {
				out[l.Name] = true
			}
			for _, l := range r.Carried {
				out[l] = true
			}
		}
		return out, known
	}
	// The probe must be able to fail: a label nothing carries is rejected,
	// and the one the hard-fault rule adds is accepted.
	if labels, known := carried("kubeneuron_gpu_memory_hard_fault_active"); !known || !labels["component"] || labels["reason"] {
		t.Fatalf("the carried-label probe is wrong for the hard-fault series: known=%v labels=%v", known, labels)
	}

	checked := 0
	for _, d := range loadGenerated(t) {
		eachTarget(d, func(p panel, tg target) {
			for _, m := range nameSelRe.FindAllStringSubmatch(tg.Expr, -1) {
				name, sel := m[1], m[2]
				labels, known := carried(name)
				if !known {
					// T4 series named kubeneuron_gpu_* are covered by the
					// declared-labels test; the catalog does not describe them.
					continue
				}
				checked++
				for _, lm := range labelInMatcherRe.FindAllStringSubmatch(sel, -1) {
					l, val := lm[1], lm[2]
					if labels[l] {
						continue
					}
					t.Errorf("%s: %s: matches %s on %s=%q, a label the catalog says it does not carry; the panel is empty, not wrong", d.Path, p.Title, name, l, val)
				}
			}
			// Grouping is checked once per expression against the union of
			// the catalog series it reads.
			union := map[string]bool{}
			any := false
			for _, n := range metricNames(tg.Expr) {
				if labels, known := carried(n); known {
					any = true
					for l := range labels {
						union[l] = true
					}
				}
			}
			if !any {
				return
			}
			for _, g := range groupRe.FindAllStringSubmatch(tg.Expr, -1) {
				for _, l := range strings.Split(g[1], ",") {
					l = strings.TrimSpace(l)
					if l == "" || union[l] {
						continue
					}
					t.Errorf("%s: %s: groups by %s, which no catalog series in the expression carries; the series collapse into one line", d.Path, p.Title, l)
				}
			}
		})
	}
	if checked < 20 {
		t.Fatalf("checked only %d catalog selectors; the boards or the probe changed shape", checked)
	}
}

// TestGeneratedDashboardsNeverSynthesiseZero fails when a generated board
// turns an absent series into a number. A device that reports nothing must
// show No data, not 0, so `or vector(0)`, `default` and `absent()` are
// forbidden in every expression.
func TestGeneratedDashboardsNeverSynthesiseZero(t *testing.T) {
	forbidden := regexp.MustCompile(`(?i)or\s+(on\s*\([^)]*\)\s*)?vector\(|\bdefault\b|absent\(`)
	// The probe must recognise the legacy board's shape.
	if !forbidden.MatchString(`count(up{job="x"} == 0) OR on() vector(0)`) {
		t.Fatal("the forbidden-expression probe does not match the known `OR on() vector(0)` shape")
	}
	for _, d := range loadGenerated(t) {
		eachTarget(d, func(p panel, tg target) {
			if forbidden.MatchString(tg.Expr) {
				t.Errorf("%s: %s: expr synthesises a value for an absent series: %s", d.Path, p.Title, tg.Expr)
			}
		})
	}
}

// TestGeneratedDashboardsReadAlertsOnlyByShadowLabelOrShippedName fails when
// an ALERTS selector on any board neither carries kubeneuron_shadow="true"
// nor names an alert from gpu-rules.yaml. Any other read of ALERTS would
// show whatever other rules the operator's vmalert runs as if it were ours.
// The shadow-only boards additionally filter ALERTS by the device scope
// only: the shadow rules deduplicate to (node, gpu_uuid, gpu_model), so a
// cluster, node_pool, namespace or pod matcher would hide every alert the
// moment a variable is narrowed.
func TestGeneratedDashboardsReadAlertsOnlyByShadowLabelOrShippedName(t *testing.T) {
	alertSelRe := regexp.MustCompile(`ALERTS\s*\{([^}]*)\}`)
	alertNameRe := regexp.MustCompile(`alertname\s*=\s*"([^"]+)"`)
	alertStateRe := regexp.MustCompile(`alertstate\s*(?:=|=~|!=|!~)\s*"([^"]*)"`)
	groupRe := regexp.MustCompile(`(?:by|without)\s*\(([^)]*)\)`)
	varRefRe := regexp.MustCompile(`\$([a-zA-Z_][a-zA-Z0-9_]*)`)
	names := shippedAlertNames(t)
	device := map[string]bool{}
	for _, v := range gpucatalog.DeviceScopeVariables {
		device[v] = true
	}
	reads := 0
	for _, d := range loadAll(t) {
		eachTarget(d, func(p panel, tg target) {
			if strings.Contains(tg.Expr, "ALERTS") && !alertSelRe.MatchString(tg.Expr) {
				t.Errorf("%s: %s: bare ALERTS read: %s", d.Path, p.Title, tg.Expr)
			}
			for _, m := range alertSelRe.FindAllStringSubmatch(tg.Expr, -1) {
				reads++
				sel := m[1]
				shadow := strings.Contains(sel, gpucatalog.ShadowAlertLabel)
				named := false
				if nm := alertNameRe.FindStringSubmatch(sel); nm != nil {
					named = names[nm[1]]
					if !named {
						t.Errorf("%s: %s: ALERTS names %s, which %s does not ship", d.Path, p.Title, nm[1], gpuRulesPath)
					}
				}
				if !shadow && !named {
					t.Errorf("%s: %s: ALERTS selector carries neither %s nor a shipped alertname: {%s}", d.Path, p.Title, gpucatalog.ShadowAlertLabel, sel)
				}
				if shadow {
					for _, v := range varRefRe.FindAllStringSubmatch(sel, -1) {
						if !device[v[1]] {
							t.Errorf("%s: %s: shadow ALERTS selector filters by $%s, a label the shadow rules drop: {%s}", d.Path, p.Title, v[1], sel)
						}
					}
				}
				// alertstate is the one label vmalert sets on ALERTS; a
				// misspelled value matches nothing and the panel is empty.
				for _, st := range alertStateRe.FindAllStringSubmatch(sel, -1) {
					if st[1] != "firing" && st[1] != "pending" {
						t.Errorf("%s: %s: ALERTS selector matches alertstate=%q; vmalert emits only firing and pending", d.Path, p.Title, st[1])
					}
				}
			}
			// An ALERTS expression may group only by what the shadow series
			// carries: alertname and alertstate from vmalert, the device
			// labels from the rule.
			if alertSelRe.MatchString(tg.Expr) {
				for _, g := range groupRe.FindAllStringSubmatch(tg.Expr, -1) {
					for _, l := range strings.Split(g[1], ",") {
						l = strings.TrimSpace(l)
						if l == "" || l == "alertname" || l == "alertstate" || device[l] {
							continue
						}
						t.Errorf("%s: %s: groups ALERTS by %s, a label the shadow rules do not carry", d.Path, p.Title, l)
					}
				}
			}
		})
	}
	if reads < 3 {
		t.Fatalf("found only %d ALERTS reads across the boards; the shadow panels are missing or the probe no longer sees them", reads)
	}
}

// TestGeneratedDashboardsUsePrometheusLabelNames fails when a label name in
// any selector or grouping of a generated board is not a Prometheus label
// name, and when any board mentions a Kubernetes-style spelling of the
// shadow label. The M5 lint covers the rule files; this is the dashboard
// half.
func TestGeneratedDashboardsUsePrometheusLabelNames(t *testing.T) {
	selRe := regexp.MustCompile(`\{([^}]*)\}`)
	matcherRe := regexp.MustCompile(`\s*([^,=!~]+?)\s*(?:=~|!~|!=|=)\s*"`)
	groupRe := regexp.MustCompile(`(?:by|without|on)\s*\(([^)]*)\)`)
	for _, d := range loadAll(t) {
		raw, err := os.ReadFile(filepath.Join(repoRoot, d.Path))
		if err != nil {
			t.Fatal(err)
		}
		for _, bad := range []string{"kubeneuron.io/shadow", "kubeneuron_io_shadow"} {
			if strings.Contains(string(raw), bad) {
				t.Errorf("%s contains %q, which is not a Prometheus label name", d.Path, bad)
			}
		}
	}
	for _, d := range loadGenerated(t) {
		eachTarget(d, func(p panel, tg target) {
			for _, s := range selRe.FindAllStringSubmatch(tg.Expr, -1) {
				for _, m := range matcherRe.FindAllStringSubmatch(s[1], -1) {
					if !promLabelNameRe.MatchString(m[1]) {
						t.Errorf("%s: %s: label name %q in selector {%s} is not a Prometheus label name", d.Path, p.Title, m[1], s[1])
					}
				}
			}
			for _, g := range groupRe.FindAllStringSubmatch(tg.Expr, -1) {
				for _, l := range strings.Split(g[1], ",") {
					l = strings.TrimSpace(l)
					if l != "" && !promLabelNameRe.MatchString(l) {
						t.Errorf("%s: %s: grouping label %q is not a Prometheus label name", d.Path, p.Title, l)
					}
				}
			}
		})
	}
}

// TestGeneratedDashboardsScopeSelectors pins the scope contract of the
// generator: every canonical or derived kubeneuron_gpu_* read is filtered by
// all seven variables, every T4 control-plane read by cluster only (the
// controller's counters carry no device or workload label, so any other
// matcher would empty the panel as soon as a variable is narrowed), and the
// hard-fault indicator is grouped by its catalog label, component.
func TestGeneratedDashboardsScopeSelectors(t *testing.T) {
	declared := declaredMetrics(t)
	nameSelRe := regexp.MustCompile(`(kubeneuron_[a-z0-9_]+)\s*\{([^}]*)\}`)
	varRefRe := regexp.MustCompile(`\$([a-zA-Z_][a-zA-Z0-9_]*)`)
	gpuScope := strings.Join(gpucatalog.GPUScopeVariables, ",")
	controlScope := strings.Join(gpucatalog.ControlScopeVariables, ",")
	seenHardFault := false
	for _, d := range loadGenerated(t) {
		eachTarget(d, func(p panel, tg target) {
			for _, m := range nameSelRe.FindAllStringSubmatch(tg.Expr, -1) {
				name, sel := m[1], m[2]
				var vars []string
				for _, v := range varRefRe.FindAllStringSubmatch(sel, -1) {
					vars = append(vars, v[1])
				}
				got := strings.Join(vars, ",")
				// A T4 series (declared by this package) or a derived series
				// computed from one carries the control scope; everything
				// else in the catalog is a device series.
				control := declared[name] != nil || declared[trimHistogramSuffix(name)] != nil || name == "kubeneuron_gpu_recovered_capacity_hours_total"
				switch {
				case control && got != controlScope:
					t.Errorf("%s: %s: control-plane series %s filtered by [%s], want [%s]", d.Path, p.Title, name, got, controlScope)
				case !control && got != gpuScope:
					t.Errorf("%s: %s: GPU series %s filtered by [%s], want [%s]", d.Path, p.Title, name, got, gpuScope)
				}
				if name == "kubeneuron_gpu_memory_hard_fault_active" {
					seenHardFault = true
					if !strings.Contains(tg.Expr, "component") || strings.Contains(tg.Expr, "reason") {
						t.Errorf("%s: %s: hard-fault panel must group by component (its catalog label), not reason: %s", d.Path, p.Title, tg.Expr)
					}
				}
				if name == "kubeneuron_incidents_opened_total" && strings.Contains(sel, "class=~") && !strings.Contains(sel, `class=~"xid-.*"`) {
					t.Errorf("%s: %s: XID panel must select incident classes xid-* (the agent's class names are xid-<kind>): {%s}", d.Path, p.Title, sel)
				}
			}
			if strings.Contains(tg.Expr, "kubeneuron_gpu_present") {
				t.Errorf("%s: %s: reads kubeneuron_gpu_present, which is deferred and never recorded", d.Path, p.Title)
			}
		})
	}
	if !seenHardFault {
		t.Fatal("no panel reads kubeneuron_gpu_memory_hard_fault_active; the health board lost its memory panel")
	}
}

// TestGeneratedDashboardsLayout fails when a generated board has duplicate
// panel ids or a panel that leaves the 24-column grid: Grafana imports both
// silently and draws the panel somewhere else.
func TestGeneratedDashboardsLayout(t *testing.T) {
	for _, d := range loadGenerated(t) {
		ids := map[int]bool{}
		eachPanel(d.Panels, func(p panel) {
			if ids[p.ID] {
				t.Errorf("%s: panel id %d used twice", d.Path, p.ID)
			}
			ids[p.ID] = true
			if p.GridPos.W <= 0 || p.GridPos.H <= 0 || p.GridPos.X < 0 || p.GridPos.X+p.GridPos.W > 24 {
				t.Errorf("%s: %s: gridPos %+v leaves the 24-column grid", d.Path, p.Title, p.GridPos)
			}
			if len(p.Targets) == 0 {
				t.Errorf("%s: %s has no targets", d.Path, p.Title)
			}
		})
	}
}

// TestLegacyPanelsLinkToRemediationBoard fails when a panel on the legacy
// overview board reads a series the generated remediation board also reads
// but carries no link to it. The plan keeps the legacy board and its panels;
// the duplicates point at the board that supersedes them instead of being
// deleted.
func TestLegacyPanelsLinkToRemediationBoard(t *testing.T) {
	var remediation *dashboard
	for _, d := range loadGenerated(t) {
		if d.Path == gpucatalog.PathDashboardRemediation {
			dd := d
			remediation = &dd
		}
	}
	if remediation == nil {
		t.Fatal("generator lists no remediation board")
	}
	wantURL := "/d/" + gpucatalog.UIDDashboardRemediation
	if remediation.UID != gpucatalog.UIDDashboardRemediation {
		t.Fatalf("remediation board uid %q, want %q", remediation.UID, gpucatalog.UIDDashboardRemediation)
	}
	covered := map[string]bool{}
	eachTarget(*remediation, func(_ panel, tg target) {
		for _, n := range metricNames(tg.Expr) {
			covered[trimHistogramSuffix(n)] = true
		}
	})
	// The derived capacity series is recorded from the legacy board's
	// degraded GPU-seconds counter, so that panel is a duplicate too.
	if covered["kubeneuron_gpu_recovered_capacity_hours_total"] {
		covered["kubeneuron_degraded_gpu_seconds_total"] = true
	}

	linked := 0
	eachTarget(loadLegacy(t), func(p panel, tg target) {
		dup := false
		for _, n := range metricNames(tg.Expr) {
			if covered[trimHistogramSuffix(n)] {
				dup = true
			}
		}
		if !dup {
			return
		}
		found := false
		for _, l := range p.Links {
			if l.URL == wantURL {
				found = true
				if l.Title == "" {
					t.Errorf("legacy panel %q: link to %s has no title", p.Title, wantURL)
				}
			}
		}
		if !found {
			t.Errorf("legacy panel %q reads a series the remediation board covers but has no link to %s", p.Title, wantURL)
			return
		}
		linked++
	})
	if linked < 8 {
		t.Fatalf("only %d legacy panels link to the remediation board; the duplicate set shrank or the probe stopped seeing links", linked)
	}
}
