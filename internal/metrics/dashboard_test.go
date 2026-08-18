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
	// And the label MATCHERS inside a selector: kubeneuron_incidents{state="X"}.
	// The doc for this test always said "groups by OR MATCHES ON", but only
	// the grouping half was ever parsed — so a panel selecting on a label the
	// metric does not carry matched nothing and rendered an empty graph, which
	// is the same silent failure as the grouping case and the more common way
	// this dashboard is written: it already uses matchers in five panels.
	matcherRe := regexp.MustCompile(`kubeneuron_[a-z0-9_]+\s*\{([^}]*)\}`)
	labelInMatcherRe := regexp.MustCompile(`([a-zA-Z_][a-zA-Z0-9_]*)\s*(?:=~|!~|!=|=)`)
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
			// A matcher is checked against ITS OWN metric, not the union: a
			// panel combining two metrics may group by either's labels, but
			// {state="X"} on one of them has to be that one's label.
			for _, m := range matcherRe.FindAllStringSubmatch(tg.Expr, -1) {
				name := dashboardMetricRe.FindString(m[0])
				base := strings.TrimSuffix(strings.TrimSuffix(name, "_bucket"), "_sum")
				base = strings.TrimSuffix(base, "_count")
				own := map[string]bool{}
				for _, key := range []string{name, base} {
					for l := range declared[key] {
						own[l] = true
					}
				}
				for _, lm := range labelInMatcherRe.FindAllStringSubmatch(m[1], -1) {
					l := lm[1]
					if infra[l] || own[l] {
						continue
					}
					bad = append(bad, p.Title+": matches on "+l+"=…, which "+name+" does not carry")
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

// notCharted lists metrics that deliberately have no panel, each with the
// reason. Everything else this package registers must appear somewhere in the
// dashboard.
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
}

// TestEveryMetricIsChartedOrExcused fails when this package registers a metric
// that no panel reads and no line above excuses.
func TestEveryMetricIsChartedOrExcused(t *testing.T) {
	declared := declaredMetrics(t)
	d := loadDashboard(t)

	charted := map[string]bool{}
	eachPanel(d.Panels, func(p panel) {
		for _, tg := range p.Targets {
			for _, name := range dashboardMetricRe.FindAllString(tg.Expr, -1) {
				charted[name] = true
				base := strings.TrimSuffix(strings.TrimSuffix(name, "_bucket"), "_sum")
				charted[strings.TrimSuffix(base, "_count")] = true
			}
		}
	})

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
