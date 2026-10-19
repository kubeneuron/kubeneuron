package depprofile

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/kubeneuron/kubeneuron/internal/detect"
)

// M5 safety boundary (v0.7), deployment side: shadow-only GPU alerts
// (configs/vmalert/gpu-shadow-rules.yaml, label kubeneuron_shadow="true",
// names ShadowGpu*) are routed to humans before the controller webhook route
// is consulted, the existing Gpu*/KubeNeuronAgentDown routing to the
// controller is unchanged, and every label name the rules and the
// Alertmanager matchers use is a valid Prometheus label name. The routing is
// checked by walking the shipped route tree with Alertmanager's own
// semantics (ordered children, first match wins unless continue, anchored
// regex matchers), not by grepping the file.

const (
	vmalertmanagerPath = observabilityDir + "/vmalertmanager.yaml"
	rulesPath          = observabilityDir + "/rules.yaml"
	vmalertRulesDir    = "configs/vmalert"
	shadowRulesPath    = vmalertRulesDir + "/gpu-shadow-rules.yaml"

	shadowLabel        = "kubeneuron_shadow"
	controllerReceiver = "kubeneuron"
	humanReceiver      = "human-default"
)

// amRoute is the subset of an Alertmanager route the tests evaluate.
type amRoute struct {
	Receiver string    `yaml:"receiver"`
	Continue bool      `yaml:"continue"`
	Matchers []string  `yaml:"matchers"`
	GroupBy  []string  `yaml:"group_by"`
	Routes   []amRoute `yaml:"routes"`
}

type amConfig struct {
	Route        amRoute `yaml:"route"`
	InhibitRules []struct {
		SourceMatchers []string `yaml:"source_matchers"`
		TargetMatchers []string `yaml:"target_matchers"`
		Equal          []string `yaml:"equal"`
	} `yaml:"inhibit_rules"`
	Receivers []struct {
		Name string `yaml:"name"`
	} `yaml:"receivers"`
}

// loadAlertmanagerConfig parses the raw Alertmanager config embedded in the
// shipped VMAlertmanager resource.
func loadAlertmanagerConfig(t *testing.T) amConfig {
	t.Helper()
	docs := yamlDocs(t, readFile(t, vmalertmanagerPath), vmalertmanagerPath)
	var raw string
	for _, doc := range docs {
		if str(doc["kind"]) == "VMAlertmanager" {
			raw = str(dig(doc, "spec", "configRawYaml"))
		}
	}
	if raw == "" {
		t.Fatalf("%s: no VMAlertmanager with spec.configRawYaml", vmalertmanagerPath)
	}
	var cfg amConfig
	if err := yaml.Unmarshal([]byte(raw), &cfg); err != nil {
		t.Fatalf("%s: configRawYaml does not parse: %v", vmalertmanagerPath, err)
	}
	if len(cfg.Route.Routes) == 0 {
		t.Fatalf("%s: route has no child routes; the harness is broken", vmalertmanagerPath)
	}
	return cfg
}

// matcherRE splits an Alertmanager matcher string `name op "value"` without
// validating the name, so a label-name lint can judge the name itself.
var matcherRE = regexp.MustCompile(`^\s*(\S+?)\s*(=~|!~|!=|=)\s*"((?:[^"\\]|\\.)*)"\s*$`)

type amMatcher struct {
	Name, Op, Value string
}

func parseMatcher(t *testing.T, where, m string) amMatcher {
	t.Helper()
	sub := matcherRE.FindStringSubmatch(m)
	if sub == nil {
		t.Fatalf("%s: matcher %q is not `name op \"value\"`", where, m)
	}
	return amMatcher{Name: sub[1], Op: sub[2], Value: sub[3]}
}

// matches applies one matcher with Alertmanager semantics: an absent label
// is the empty string and regex matchers are fully anchored.
func (m amMatcher) matches(t *testing.T, labels map[string]string) bool {
	t.Helper()
	v := labels[m.Name]
	switch m.Op {
	case "=":
		return v == m.Value
	case "!=":
		return v != m.Value
	}
	re, err := regexp.Compile("^(?:" + m.Value + ")$")
	if err != nil {
		t.Fatalf("matcher %s%s%q: bad regex: %v", m.Name, m.Op, m.Value, err)
	}
	if m.Op == "=~" {
		return re.MatchString(v)
	}
	return !re.MatchString(v)
}

// resolve walks the route tree the way Alertmanager does: children are
// tried in order, the first whose matchers all hold is taken (descending
// into its own children), and matching continues past it only when it sets
// continue. The result is the receivers reached, in order; the root receiver
// is the fallback when no child matches.
func resolve(t *testing.T, r amRoute, labels map[string]string) []string {
	t.Helper()
	var out []string
	for _, child := range r.Routes {
		ok := true
		for _, m := range child.Matchers {
			if !parseMatcher(t, "route", m).matches(t, labels) {
				ok = false
				break
			}
		}
		if !ok {
			continue
		}
		out = append(out, resolve(t, child, labels)...)
		if !child.Continue {
			return out
		}
	}
	if len(out) == 0 {
		out = append(out, r.Receiver)
	}
	return out
}

// shadowAlertNames reads the alert names from the canonical shadow rules
// file, so the routing test covers every shipped shadow alert, not a sample.
func shadowAlertNames(t *testing.T) []string {
	t.Helper()
	var doc struct {
		Groups []struct {
			Rules []struct {
				Alert  string            `yaml:"alert"`
				Labels map[string]string `yaml:"labels"`
			} `yaml:"rules"`
		} `yaml:"groups"`
	}
	if err := yaml.Unmarshal(readFile(t, shadowRulesPath), &doc); err != nil {
		t.Fatalf("%s: %v", shadowRulesPath, err)
	}
	var names []string
	for _, g := range doc.Groups {
		for _, r := range g.Rules {
			if r.Alert != "" {
				names = append(names, r.Alert)
			}
		}
	}
	if len(names) == 0 {
		t.Fatalf("%s: no alerts; the harness is broken", shadowRulesPath)
	}
	return names
}

// The shadow route is an explicit, non-continuing route to human-default
// placed before the controller webhook route, and the controller route still
// matches exactly what it matched before M5.
func TestShadowRouteIsOrderedBeforeControllerRoute(t *testing.T) {
	cfg := loadAlertmanagerConfig(t)
	shadowIdx, controllerIdx := -1, -1
	for i, child := range cfg.Route.Routes {
		for _, raw := range child.Matchers {
			m := parseMatcher(t, "route", raw)
			if m.Name == shadowLabel && m.Op == "=" && m.Value == "true" && shadowIdx < 0 {
				shadowIdx = i
				if child.Receiver != humanReceiver {
					t.Errorf("shadow route receiver = %q, want %s", child.Receiver, humanReceiver)
				}
				if child.Continue {
					t.Error("shadow route sets continue: true; it must settle the alert so no later route can reach the webhook")
				}
				if len(child.Matchers) != 1 {
					t.Errorf("shadow route has %d matchers %v; want the single %s matcher so nothing narrows it", len(child.Matchers), child.Matchers, shadowLabel)
				}
			}
			if child.Receiver == controllerReceiver && controllerIdx < 0 {
				controllerIdx = i
				if m.Name != "alertname" || m.Op != "=~" || m.Value != "Gpu.*|KubeNeuronAgentDown" {
					t.Errorf("controller route matcher = %q; the pre-M5 routing semantics must stay unchanged", raw)
				}
			}
		}
	}
	if shadowIdx < 0 {
		t.Fatalf("%s: no route matching %s = \"true\"", vmalertmanagerPath, shadowLabel)
	}
	if controllerIdx < 0 {
		t.Fatalf("%s: no route to receiver %s", vmalertmanagerPath, controllerReceiver)
	}
	if shadowIdx > controllerIdx {
		t.Errorf("shadow route is child %d, controller route is child %d; the shadow route must come first", shadowIdx, controllerIdx)
	}
	if cfg.Route.Receiver != humanReceiver {
		t.Errorf("root receiver = %q, want %s (unmatched alerts go to humans)", cfg.Route.Receiver, humanReceiver)
	}
	for _, rcv := range []string{controllerReceiver, humanReceiver} {
		found := false
		for _, r := range cfg.Receivers {
			found = found || r.Name == rcv
		}
		if !found {
			t.Errorf("receiver %s is routed to but not defined", rcv)
		}
	}
}

// Routing, evaluated: every shipped shadow alert, carrying the labels its
// rule attaches, reaches human-default and only human-default; every alert
// the controller maps still reaches kubeneuron; and a shadow-labelled alert
// reaches human-default even if its name were Gpu-prefixed, which proves the
// shadow route is not shadowed by the generic GPU regex.
func TestShadowAlertsRouteToHumansAndControllerAlertsStillRouteToController(t *testing.T) {
	cfg := loadAlertmanagerConfig(t)
	gpu := map[string]string{"node": "gpu-node-1", "gpu": "0", "UUID": "GPU-1", "gpu_uuid": "GPU-1"}
	withBase := func(extra map[string]string) map[string]string {
		out := map[string]string{}
		for k, v := range gpu {
			out[k] = v
		}
		for k, v := range extra {
			out[k] = v
		}
		return out
	}

	for _, name := range shadowAlertNames(t) {
		got := resolve(t, cfg.Route, withBase(map[string]string{"alertname": name, shadowLabel: "true", "severity": "info"}))
		if len(got) != 1 || got[0] != humanReceiver {
			t.Errorf("shadow alert %s routes to %v, want [%s]", name, got, humanReceiver)
		}
	}

	controllerNames := detect.KnownAlertNames()
	if len(controllerNames) == 0 {
		t.Fatal("detect.KnownAlertNames() is empty; the harness is broken")
	}
	for _, name := range controllerNames {
		severity := "warning"
		if name == "GpuDoubleBitEccVolatile" {
			severity = "critical"
		}
		got := resolve(t, cfg.Route, withBase(map[string]string{"alertname": name, "severity": severity}))
		if len(got) != 1 || got[0] != controllerReceiver {
			t.Errorf("controller alert %s routes to %v, want [%s]; the pre-M5 routing must stay unchanged", name, got, controllerReceiver)
		}
	}

	// The representative existing GPU alert, spelled out so the assertion
	// survives a change to KnownAlertNames.
	if got := resolve(t, cfg.Route, withBase(map[string]string{"alertname": "GpuDoubleBitEccVolatile", "severity": "critical"})); len(got) != 1 || got[0] != controllerReceiver {
		t.Errorf("GpuDoubleBitEccVolatile routes to %v, want [%s]", got, controllerReceiver)
	}
	// A shadow label wins over a Gpu-prefixed name: ordering, not the name,
	// keeps the webhook out of reach.
	if got := resolve(t, cfg.Route, withBase(map[string]string{"alertname": "GpuWouldMatchTheWebhookRegex", shadowLabel: "true"})); len(got) != 1 || got[0] != humanReceiver {
		t.Errorf("shadow-labelled Gpu* alert routes to %v, want [%s]; the shadow route is shadowed by the GPU regex", got, humanReceiver)
	}
	// Self-monitoring still goes to humans, and an unknown alert falls back
	// to the human root receiver, never to the webhook.
	for _, name := range []string{"KubeNeuronControllerDown", "SomethingNobodyRoutes"} {
		if got := resolve(t, cfg.Route, map[string]string{"alertname": name}); len(got) != 1 || got[0] != humanReceiver {
			t.Errorf("%s routes to %v, want [%s]", name, got, humanReceiver)
		}
	}
}

// Every label name in a `labels:` block of every shipped rules file (the
// canonical configs/vmalert files and every VMRule in the deployed
// manifest), every matcher label name in the Alertmanager routes and
// inhibit rules, and every group_by/equal label is a valid Prometheus label
// name. A Kubernetes-style spelling such as a key with dots or a slash is
// invalid in rule labels, matchers and ALERTS{} selectors; vmalert and
// amtool would reject it later, this test rejects it with the file and
// label named.
func TestRuleAndMatcherLabelNamesFollowPrometheusGrammar(t *testing.T) {
	checkLabels := func(where string, labels map[string]any) {
		for name := range labels {
			if !promLabelNameRE.MatchString(name) {
				t.Errorf("%s: label name %q is not a Prometheus label name (%s)", where, name, promLabelNameRE)
			}
		}
	}
	checkGroups := func(where string, groups []any) {
		rules := 0
		for _, g := range groups {
			group, _ := g.(map[string]any)
			list, _ := group["rules"].([]any)
			for _, r := range list {
				rule, _ := r.(map[string]any)
				rules++
				id := fmt.Sprintf("%s group %s rule %s%s", where, str(group["name"]), str(rule["alert"]), str(rule["record"]))
				if labels, ok := rule["labels"].(map[string]any); ok {
					checkLabels(id, labels)
				}
			}
		}
		if rules == 0 {
			t.Errorf("%s: no rules found; the harness is broken", where)
		}
	}

	entries, err := os.ReadDir(filepath.Join(repoRoot(t), vmalertRulesDir))
	if err != nil {
		t.Fatal(err)
	}
	files := 0
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".yaml") {
			continue
		}
		files++
		rel := vmalertRulesDir + "/" + e.Name()
		for _, doc := range yamlDocs(t, readFile(t, rel), rel) {
			groups, _ := doc["groups"].([]any)
			checkGroups(rel, groups)
		}
	}
	if files < 4 {
		t.Fatalf("%s: found %d rules files, want the gpu, gpu-recording, gpu-shadow and self files at least", vmalertRulesDir, files)
	}

	vmrules := 0
	for _, doc := range yamlDocs(t, readFile(t, rulesPath), rulesPath) {
		if str(doc["kind"]) != "VMRule" {
			continue
		}
		vmrules++
		groups, _ := dig(doc, "spec", "groups").([]any)
		checkGroups(rulesPath+" VMRule "+str(dig(doc, "metadata", "name")), groups)
	}
	if vmrules < 3 {
		t.Fatalf("%s: found %d VMRules, want at least kubeneuron-gpu, kubeneuron-gpu-recording and kubeneuron-gpu-shadow", rulesPath, vmrules)
	}

	cfg := loadAlertmanagerConfig(t)
	checkName := func(where, name string) {
		if !promLabelNameRE.MatchString(name) {
			t.Errorf("%s: label name %q is not a Prometheus label name (%s)", where, name, promLabelNameRE)
		}
	}
	var walk func(where string, r amRoute)
	walk = func(where string, r amRoute) {
		for _, m := range r.Matchers {
			checkName(where+" matcher "+m, parseMatcher(t, where, m).Name)
		}
		for _, g := range r.GroupBy {
			checkName(where+" group_by", g)
		}
		for i, child := range r.Routes {
			walk(fmt.Sprintf("%s.routes[%d]", where, i), child)
		}
	}
	walk("route", cfg.Route)
	for i, rule := range cfg.InhibitRules {
		where := fmt.Sprintf("inhibit_rules[%d]", i)
		for _, m := range append(append([]string{}, rule.SourceMatchers...), rule.TargetMatchers...) {
			checkName(where+" matcher "+m, parseMatcher(t, where, m).Name)
		}
		for _, e := range rule.Equal {
			checkName(where+" equal", e)
		}
	}
}

// The shadow label has exactly one spelling, kubeneuron_shadow. The two
// spellings a Kubernetes habit would produce are banned from every file
// under configs/, deploy/ and docs/, so no rule, matcher, dashboard or
// document can teach a reader the invalid form.
func TestForbiddenShadowLabelSpellingsAreAbsent(t *testing.T) {
	root := repoRoot(t)
	forbidden := []string{"kubeneuron.io/shadow", "kubeneuron_io_shadow"}
	scanned := 0
	for _, dir := range []string{"configs", "deploy", "docs"} {
		err := filepath.WalkDir(filepath.Join(root, dir), func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				return nil
			}
			raw, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			scanned++
			for i, line := range strings.Split(string(raw), "\n") {
				for _, f := range forbidden {
					if strings.Contains(line, f) {
						rel, _ := filepath.Rel(root, path)
						t.Errorf("%s:%d: forbidden spelling %q; the shadow label is %s", rel, i+1, f, shadowLabel)
					}
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if scanned == 0 {
		t.Fatal("scanned no files; the harness is broken")
	}
}
