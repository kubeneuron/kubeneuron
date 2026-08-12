package metrics

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// The shipped Grafana dashboard is the only part of this product whose
// correctness nothing compiles. A renamed metric or a dropped label does not
// break a build, does not fail a test, and does not error at query time — the
// panel simply renders an empty graph, which reads as "nothing is wrong" at
// exactly the moment somebody is looking for what is wrong.
//
// Removing the node label from kubeneuron_workloads_evicted_total is the shape
// of the accident these tests exist to catch: a defensible change to Go code
// that silently empties a panel nobody thought to open.

const dashboardPath = "../../deploy/grafana/kubeneuron-dashboard.json"

// metricNameRe finds our own metric names inside a PromQL expression.
var dashboardMetricRe = regexp.MustCompile(`kubeneuron_[a-z0-9_]+`)

// declaredMetricRe pulls a metric name out of this package's source. It spans
// both declaration shapes: the promauto helpers' `Name:` field and the custom
// collectors' prometheus.NewDesc, whose first argument is the name.
var declaredMetricRe = regexp.MustCompile(`(?:Name:\s*|NewDesc\(\s*)"(kubeneuron_[a-z0-9_]+)"`)

// labelSetRe pulls the label slice that follows a metric declaration, in
// either shape: `}, []string{"a","b"})` for promauto, `[]string{"a"}, nil)`
// for NewDesc.
var labelSetRe = regexp.MustCompile(`\[\]string\{([^}]*)\}`)

type dashboard struct {
	Panels []panel `json:"panels"`
}

type panel struct {
	Title   string   `json:"title"`
	Panels  []panel  `json:"panels"`
	Targets []target `json:"targets"`
}

type target struct {
	Expr string `json:"expr"`
}

func loadDashboard(t *testing.T) dashboard {
	t.Helper()
	raw, err := os.ReadFile(filepath.Clean(dashboardPath))
	if err != nil {
		t.Fatalf("the shipped dashboard must be readable: %v", err)
	}
	var d dashboard
	if err := json.Unmarshal(raw, &d); err != nil {
		t.Fatalf("the shipped dashboard is not valid JSON: %v", err)
	}
	return d
}

func eachPanel(panels []panel, fn func(panel)) {
	for _, p := range panels {
		fn(p)
		eachPanel(p.Panels, fn)
	}
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

// TestDashboardQueriesNameRealMetrics fails when a panel queries a metric this
// package does not register.
func TestDashboardQueriesNameRealMetrics(t *testing.T) {
	declared := declaredMetrics(t)
	d := loadDashboard(t)

	var missing []string
	eachPanel(d.Panels, func(p panel) {
		for _, tg := range p.Targets {
			for _, name := range dashboardMetricRe.FindAllString(tg.Expr, -1) {
				// Recording-rule style suffixes Prometheus appends to a
				// histogram are not separate declarations.
				base := strings.TrimSuffix(strings.TrimSuffix(name, "_bucket"), "_sum")
				base = strings.TrimSuffix(base, "_count")
				if _, ok := declared[name]; ok {
					continue
				}
				if _, ok := declared[base]; ok {
					continue
				}
				missing = append(missing, p.Title+": "+name)
			}
		}
	})
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Fatalf("dashboard panels query metrics this build does not register — each renders empty and reads as healthy:\n  %s",
			strings.Join(missing, "\n  "))
	}
}

// TestDashboardQueriesUseRealLabels fails when a panel groups by or matches on
// a label the metric does not carry. This is the failure that survives a
// rename: `sum by (node) (...)` on a metric with no node label collapses to a
// single unlabelled series rather than erroring.
func TestDashboardQueriesUseRealLabels(t *testing.T) {
	declared := declaredMetrics(t)
	d := loadDashboard(t)

	// by (a, b) / without (a, b) immediately around one of our metrics.
	groupRe := regexp.MustCompile(`(?:by|without)\s*\(([^)]*)\)`)
	// Labels Prometheus itself adds to every series, plus the ones scrape
	// configs attach; a panel may legitimately group on these.
	infra := map[string]bool{
		"instance": true, "job": true, "namespace": true, "pod": true,
		"container": true, "cluster": true, "le": true,
	}

	var bad []string
	eachPanel(d.Panels, func(p panel) {
		for _, tg := range p.Targets {
			names := dashboardMetricRe.FindAllString(tg.Expr, -1)
			if len(names) == 0 {
				continue
			}
			// Union of the labels every metric in this expression carries. A
			// panel combining two metrics may group by any of their labels.
			allowed := map[string]bool{}
			for _, n := range names {
				base := strings.TrimSuffix(strings.TrimSuffix(n, "_bucket"), "_sum")
				base = strings.TrimSuffix(base, "_count")
				for _, key := range []string{n, base} {
					for l := range declared[key] {
						allowed[l] = true
					}
				}
			}
			for _, g := range groupRe.FindAllStringSubmatch(tg.Expr, -1) {
				for _, l := range strings.Split(g[1], ",") {
					l = strings.TrimSpace(l)
					if l == "" || infra[l] || allowed[l] {
						continue
					}
					bad = append(bad, p.Title+": groups by "+l+", which none of "+strings.Join(names, ", ")+" carries")
				}
			}
		}
	})
	sort.Strings(bad)
	if len(bad) > 0 {
		t.Fatalf("dashboard panels group by labels their metrics do not have — the series collapses instead of erroring:\n  %s",
			strings.Join(bad, "\n  "))
	}
}
