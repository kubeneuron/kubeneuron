// SPDX-License-Identifier: Apache-2.0

package metrics

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// The default rule pack and the dashboards are plain PromQL, so they load in
// Prometheus as well as in vmalert and VictoriaMetrics. MetricsQL accepts
// every PromQL function and many of its own (bitmap_and, label_del, median,
// default, ...); a MetricsQL-only call still loads in the managed vmalert and
// fails only on a Prometheus backend, so nothing else would catch it.
// hack/check-rules.sh parses the same expressions with promtool; this test
// is the check that runs without docker.

// promqlFunctions is every non-experimental function and aggregation
// operator of PromQL (Prometheus 3), plus the keywords that may stand
// before an opening parenthesis.
var promqlFunctions = setOf(
	// Functions.
	"abs", "absent", "absent_over_time", "acos", "acosh", "asin", "asinh", "atan", "atanh",
	"avg_over_time", "ceil", "changes", "clamp", "clamp_max", "clamp_min", "cos", "cosh",
	"count_over_time", "day_of_month", "day_of_week", "day_of_year", "days_in_month", "deg",
	"delta", "deriv", "exp", "floor", "histogram_avg", "histogram_count", "histogram_fraction",
	"histogram_quantile", "histogram_stddev", "histogram_stdvar", "histogram_sum", "hour",
	"idelta", "increase", "irate", "label_join", "label_replace", "last_over_time", "ln",
	"log10", "log2", "max_over_time", "min_over_time", "minute", "month", "pi",
	"predict_linear", "present_over_time", "quantile_over_time", "rad", "rate", "resets",
	"round", "scalar", "sgn", "sin", "sinh", "sort", "sort_desc", "sqrt", "stddev_over_time",
	"stdvar_over_time", "sum_over_time", "tan", "tanh", "time", "timestamp", "vector", "year",
	// Aggregation operators.
	"avg", "bottomk", "count", "count_values", "group", "max", "min", "quantile", "stddev",
	"stdvar", "sum", "topk",
	// Keywords.
	"and", "atan2", "bool", "by", "group_left", "group_right", "ignoring", "on", "or",
	"unless", "without",
)

// metricsqlKeywords are MetricsQL modifiers that PromQL has no word for.
var metricsqlKeywords = regexp.MustCompile(`\b(default|if|ifnot|keep_metric_names|WITH)\b`)

var (
	quotedRe   = regexp.MustCompile(`"(?:[^"\\]|\\.)*"|'(?:[^'\\]|\\.)*'`)
	matchersRe = regexp.MustCompile(`\{[^{}]*\}`)
	// A call, or an aggregation with its by/without clause before the
	// parenthesis.
	callRe = regexp.MustCompile(`([A-Za-z_][A-Za-z0-9_:]*)\s*(?:(?:by|without)\s*\([^()]*\)\s*)?\(`)
)

func setOf(names ...string) map[string]bool {
	m := make(map[string]bool, len(names))
	for _, n := range names {
		m[n] = true
	}
	return m
}

// nonPromQL names what in expr is not PromQL: a call to a function PromQL
// does not have, or a MetricsQL modifier. Quoted strings and label
// matchers are blanked first, so a label value or name never counts.
func nonPromQL(expr string) []string {
	bare := matchersRe.ReplaceAllString(quotedRe.ReplaceAllString(expr, `""`), "{}")
	var out []string
	for _, m := range callRe.FindAllStringSubmatch(bare, -1) {
		if !promqlFunctions[m[1]] {
			out = append(out, m[1]+"()")
		}
	}
	out = append(out, metricsqlKeywords.FindAllString(bare, -1)...)
	return out
}

type ruleFile struct {
	Groups []struct {
		Name  string `yaml:"name"`
		Rules []struct {
			Record string `yaml:"record"`
			Alert  string `yaml:"alert"`
			Expr   string `yaml:"expr"`
		} `yaml:"rules"`
	} `yaml:"groups"`
}

// ruleExprs returns every rule expression of the shipped rule files and
// of the VMRule mirror, keyed by where it is.
func ruleExprs(t *testing.T) map[string]string {
	t.Helper()
	out := map[string]string{}
	add := func(src string, f ruleFile) {
		for _, g := range f.Groups {
			for i, r := range g.Rules {
				name := r.Record + r.Alert
				out[src+": "+g.Name+"["+strconv.Itoa(i)+"] "+name] = r.Expr
			}
		}
	}
	files, err := filepath.Glob(filepath.Join(repoRoot, "configs/vmalert/*.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, p := range files {
		if strings.HasSuffix(p, ".test.yaml") {
			continue
		}
		raw, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		var f ruleFile
		if err := yaml.Unmarshal(raw, &f); err != nil {
			t.Fatalf("%s: %v", p, err)
		}
		add(filepath.Base(p), f)
		n++
	}
	if n < 5 {
		t.Fatalf("found %d rule files in configs/vmalert, want at least 5", n)
	}
	const mirror = "deploy/kubernetes/dependencies/observability/rules.yaml"
	raw, err := os.ReadFile(filepath.Join(repoRoot, mirror))
	if err != nil {
		t.Fatal(err)
	}
	dec := yaml.NewDecoder(strings.NewReader(string(raw)))
	for {
		var doc struct {
			Spec ruleFile `yaml:"spec"`
		}
		if err := dec.Decode(&doc); err != nil {
			break
		}
		add(mirror, doc.Spec)
	}
	return out
}

// Every rule expression in the default pack and its VMRule mirror is plain
// PromQL.
func TestRulePackIsPlainPromQL(t *testing.T) {
	exprs := ruleExprs(t)
	if len(exprs) < 100 {
		t.Fatalf("found only %d rule expressions; the reader is broken", len(exprs))
	}
	keys := make([]string, 0, len(exprs))
	for k := range exprs {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if bad := nonPromQL(exprs[k]); len(bad) != 0 {
			t.Errorf("%s: not PromQL: %v in %q", k, bad, exprs[k])
		}
	}
}

// Every panel expression of the six dashboards is plain PromQL. The
// label_values() variable queries are Grafana's own and are not read here.
func TestDashboardsArePlainPromQL(t *testing.T) {
	n := 0
	for _, d := range loadAll(t) {
		eachTarget(d, func(p panel, tg target) {
			if tg.Expr == "" {
				return
			}
			n++
			if bad := nonPromQL(tg.Expr); len(bad) != 0 {
				t.Errorf("%s panel %q: not PromQL: %v in %q", d.Path, p.Title, bad, tg.Expr)
			}
		})
	}
	if n < 50 {
		t.Fatalf("found only %d panel expressions; the reader is broken", n)
	}
}

// The check names each MetricsQL-only construct the pack used to carry, and
// passes their PromQL rewrites.
func TestNonPromQLRejectsMetricsQL(t *testing.T) {
	for _, expr := range []string{
		`bitmap_and(DCGM_FI_DEV_CLOCK_THROTTLE_REASONS, 8) > bool 0`,
		`label_del(rate(kubeneuron_gpu_ecc_errors_total{bits="single"}[5m]), "bits")`,
		`median by (gpu_model) (kubeneuron_gpu_clock_ratio)`,
		`kubeneuron_gpu_power_watts default 0`,
		`sum(rate(kubeneuron_gpu_pcie_replays_total[5m])) keep_metric_names`,
	} {
		if len(nonPromQL(expr)) == 0 {
			t.Errorf("accepted MetricsQL %q", expr)
		}
	}
	for _, expr := range []string{
		`floor(DCGM_FI_DEV_CLOCK_THROTTLE_REASONS / 8) % 2`,
		`label_replace(rate(kubeneuron_gpu_ecc_errors_total{bits="single"}[5m]), "bits", "", "bits", ".*")`,
		`quantile by (gpu_model) (0.5, kubeneuron_gpu_clock_ratio)`,
		`sum by (node) (rate(x{label="median(", other=~"bitmap_and(.*"}[5m])) > on (node) group_left() (y or vector(0))`,
	} {
		if bad := nonPromQL(expr); len(bad) != 0 {
			t.Errorf("rejected PromQL %q: %v", expr, bad)
		}
	}
}
