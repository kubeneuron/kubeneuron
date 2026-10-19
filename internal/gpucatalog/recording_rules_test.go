package gpucatalog

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// The canonical recording rules (CanonicalRulePath) and their deployed mirror
// (the kubeneuron-gpu-recording VMRule in DeployedRulePath) are checked here
// as parsed YAML, never as text: a comment that names a series is not a
// source reference, and a rule is judged by its record, expr and labels.
// The catalog is the only source of truth, so every exporter-exposed DCGM
// field with a canonical mapping must be recorded with exactly its source
// series, factor and fixed labels, and nothing else may be recorded.

// ruleGroup is the subset of a vmalert group the mirror test compares; a
// VMRule spec.groups entry and a plain rules-file group both decode to it.
type ruleGroup struct {
	Name     string          `yaml:"name"`
	Interval string          `yaml:"interval"`
	Rules    []recordingRule `yaml:"rules"`
}

type recordingRule struct {
	Record string            `yaml:"record"`
	Alert  string            `yaml:"alert"`
	Expr   string            `yaml:"expr"`
	Labels map[string]string `yaml:"labels"`
	For    string            `yaml:"for"`
}

func loadCanonicalRules(t *testing.T) []ruleGroup {
	t.Helper()
	src, err := os.ReadFile(filepath.Join(repoRoot(t), CanonicalRulePath))
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Groups []ruleGroup `yaml:"groups"`
	}
	if err := yaml.Unmarshal(src, &doc); err != nil {
		t.Fatalf("%s: not a vmalert rules file: %v", CanonicalRulePath, err)
	}
	if len(doc.Groups) == 0 {
		t.Fatalf("%s: no groups; the harness is broken", CanonicalRulePath)
	}
	return doc.Groups
}

// canonicalGroups returns only the gpu-canonical-* (normalisation) groups;
// the gpu-derived-* groups are judged against Catalog.Derived instead.
func canonicalGroups(t *testing.T) []ruleGroup {
	t.Helper()
	var out []ruleGroup
	for _, g := range loadCanonicalRules(t) {
		if !strings.HasPrefix(g.Name, DerivedGroupPrefix) {
			out = append(out, g)
		}
	}
	if len(out) == 0 {
		t.Fatalf("%s: no gpu-canonical-* group; the harness is broken", CanonicalRulePath)
	}
	return out
}

// loadDeployedRecordingGroups returns the groups of the one VMRule in the
// deployed manifest that mirrors the canonical rules. The manifest is a
// multi-document YAML stream; the legacy alert VMRule in it is skipped by
// name, so its raw alerts are never mistaken for recording rules.
func loadDeployedRecordingGroups(t *testing.T) []ruleGroup {
	t.Helper()
	f, err := os.Open(filepath.Join(repoRoot(t), DeployedRulePath))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := f.Close(); err != nil {
			t.Errorf("%s: close: %v", DeployedRulePath, err)
		}
	})
	dec := yaml.NewDecoder(f)
	var found []ruleGroup
	docs, matches := 0, 0
	for {
		var doc struct {
			Kind     string `yaml:"kind"`
			Metadata struct {
				Name string `yaml:"name"`
			} `yaml:"metadata"`
			Spec struct {
				Groups []ruleGroup `yaml:"groups"`
			} `yaml:"spec"`
		}
		err := dec.Decode(&doc)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("%s: document %d does not parse: %v", DeployedRulePath, docs+1, err)
		}
		docs++
		if doc.Kind == "VMRule" && doc.Metadata.Name == "kubeneuron-gpu-recording" {
			matches++
			found = doc.Spec.Groups
		}
	}
	if matches != 1 {
		t.Fatalf("%s: want exactly one VMRule named kubeneuron-gpu-recording, found %d in %d documents", DeployedRulePath, matches, docs)
	}
	if len(found) == 0 {
		t.Fatalf("%s: VMRule kubeneuron-gpu-recording has no groups", DeployedRulePath)
	}
	return found
}

// The deployed VMRule must carry the canonical groups unchanged: same group
// names in the same order, same interval, same rules with the same record,
// expr and labels. Indentation and comments are free to differ.
func TestDeployedRecordingRulesMirrorCanonical(t *testing.T) {
	for _, msg := range mirrorMismatches(loadCanonicalRules(t), loadDeployedRecordingGroups(t)) {
		t.Error(msg)
	}
}

// mirrorMismatches is the pure core of the mirror check: every difference
// between the canonical groups and the deployed copy in group count, order,
// name, interval, rule count or any rule's record, expr, labels, alert or for.
func mirrorMismatches(canonical, deployed []ruleGroup) []string {
	var out []string
	if len(canonical) != len(deployed) {
		return []string{fmt.Sprintf("group count: %s has %d, %s VMRule has %d", CanonicalRulePath, len(canonical), DeployedRulePath, len(deployed))}
	}
	for i := range canonical {
		c, d := canonical[i], deployed[i]
		if c.Name != d.Name {
			out = append(out, fmt.Sprintf("group %d: canonical %q, deployed %q (order and names must match)", i, c.Name, d.Name))
			continue
		}
		if c.Interval != d.Interval {
			out = append(out, fmt.Sprintf("group %s: interval canonical %q, deployed %q", c.Name, c.Interval, d.Interval))
		}
		if len(c.Rules) != len(d.Rules) {
			out = append(out, fmt.Sprintf("group %s: %d canonical rules, %d deployed", c.Name, len(c.Rules), len(d.Rules)))
			continue
		}
		for j := range c.Rules {
			if !reflect.DeepEqual(c.Rules[j], d.Rules[j]) {
				out = append(out, fmt.Sprintf("group %s rule %d: canonical %+v, deployed %+v", c.Name, j, c.Rules[j], d.Rules[j]))
			}
		}
	}
	return out
}

// Every rule in the canonical file is a recording rule in a gpu-canonical-*
// or gpu-derived-* group with an evaluation interval: no alerts, no `for`,
// no stray group.
func TestCanonicalRulesAreRecordingOnly(t *testing.T) {
	seen := map[string]bool{}
	for _, g := range loadCanonicalRules(t) {
		if !strings.HasPrefix(g.Name, "gpu-canonical-") && !strings.HasPrefix(g.Name, DerivedGroupPrefix) {
			t.Errorf("group %q: groups are named gpu-canonical-* or %s*", g.Name, DerivedGroupPrefix)
		}
		if seen[g.Name] {
			t.Errorf("group %q appears twice", g.Name)
		}
		seen[g.Name] = true
		if g.Interval == "" {
			t.Errorf("group %s: no evaluation interval", g.Name)
		}
		if len(g.Rules) == 0 {
			t.Errorf("group %s: no rules", g.Name)
		}
		for _, r := range g.Rules {
			switch {
			case r.Alert != "":
				t.Errorf("group %s: alert %q in a recording-only file", g.Name, r.Alert)
			case r.Record == "":
				t.Errorf("group %s: rule without a record name: %+v", g.Name, r)
			case !strings.HasPrefix(r.Record, "kubeneuron_gpu_"):
				t.Errorf("group %s: record %q is not a kubeneuron_gpu_* series", g.Name, r.Record)
			}
			if r.For != "" {
				t.Errorf("group %s record %s: `for` belongs to alerts, not recording rules", g.Name, r.Record)
			}
			if strings.TrimSpace(r.Expr) == "" {
				t.Errorf("group %s record %s: empty expr", g.Name, r.Record)
			}
		}
	}
}

// scalarExprRE matches the only expression shape a scalar canonical rule
// may have: the bare source series, optionally times one numeric factor.
var scalarExprRE = regexp.MustCompile(`^([A-Za-z_][A-Za-z0-9_]*)(?: \* ([0-9][0-9.]*(?:e[+-]?[0-9]+)?))?$`)

// decodeExprRE matches the clock-event decode: one bit of the bitmask
// tested for being set.
var decodeExprRE = regexp.MustCompile(`^bitmap_and\(([A-Za-z_][A-Za-z0-9_]*), ([0-9]+)\) > bool 0$`)

// clockEventBits is the decode table for field 112 the catalog's help text
// documents (0x08, 0x20, 0x40, 0x80) and the recording rule must implement:
// one series per reason, value 1 while the bit is set.
var clockEventBits = map[string]int{
	"hw_slowdown":    0x08,
	"sw_thermal":     0x20,
	"hw_thermal":     0x40,
	"hw_power_brake": 0x80,
}

func labelsOf(ls []Label) map[string]string {
	if len(ls) == 0 {
		return nil
	}
	m := make(map[string]string, len(ls))
	for _, l := range ls {
		m[l.Name] = l.Value
	}
	return m
}

func ruleKey(r recordingRule) string {
	keys := make([]string, 0, len(r.Labels))
	for k := range r.Labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteString(r.Record)
	for _, k := range keys {
		fmt.Fprintf(&b, "{%s=%s}", k, r.Labels[k])
	}
	return b.String()
}

// Every exporter-exposed DCGM field with a canonical mapping is recorded
// exactly as the catalog states: from its source series (the name the
// pinned exporter publishes, legacy where DCGM renamed the field), times
// the catalog factor, with the catalog's fixed labels and nothing more.
// Field 112 is decoded into four reason series. Nothing else is recorded:
// no unprofiled field, no deferred field, no reserved name.
func TestCanonicalRulesImplementCatalog(t *testing.T) {
	c := Default()
	byKey := map[string]recordingRule{}
	for _, g := range canonicalGroups(t) {
		for _, r := range g.Rules {
			k := ruleKey(r)
			if _, dup := byKey[k]; dup {
				t.Errorf("group %s: %s is recorded twice; two rules would write the same series", g.Name, k)
			}
			byKey[k] = r
		}
	}
	matched := map[string]bool{}
	expected := 0
	for _, f := range c.Fields {
		if f.DCGMFieldID == 0 || !f.ExporterExposed || f.Canonical == nil {
			continue
		}
		if !ProfileEligible(f) {
			t.Errorf("field %d is exposed with a canonical mapping but not profile-eligible; the catalog test should have caught this", f.DCGMFieldID)
		}
		want := f.Canonical
		if len(want.ValueLabels) > 0 {
			// The decode case: one rule per reason bit.
			if f.DCGMFieldID != 112 || !reflect.DeepEqual(want.ValueLabels, []string{"reason"}) {
				t.Errorf("field %d: the only decoded mapping is field 112 by reason, got value labels %v", f.DCGMFieldID, want.ValueLabels)
				continue
			}
			for reason, bit := range clockEventBits {
				expected++
				k := ruleKey(recordingRule{Record: want.Series, Labels: map[string]string{"reason": reason}})
				r, ok := byKey[k]
				if !ok {
					t.Errorf("field %d: no rule records %s{reason=%q}", f.DCGMFieldID, want.Series, reason)
					continue
				}
				matched[k] = true
				m := decodeExprRE.FindStringSubmatch(r.Expr)
				if m == nil {
					t.Errorf("field %d reason %s: expr %q is not `bitmap_and(<series>, <bit>) > bool 0`", f.DCGMFieldID, reason, r.Expr)
					continue
				}
				if m[1] != f.Series {
					t.Errorf("field %d reason %s: decodes %s, the exporter publishes %s", f.DCGMFieldID, reason, m[1], f.Series)
				}
				if got, _ := strconv.Atoi(m[2]); got != bit {
					t.Errorf("field %d reason %s: tests bit %d, the catalog documents 0x%02x", f.DCGMFieldID, reason, got, bit)
				}
			}
			continue
		}
		expected++
		k := ruleKey(recordingRule{Record: want.Series, Labels: labelsOf(want.Labels)})
		r, ok := byKey[k]
		if !ok {
			t.Errorf("field %d (%s): no rule records %s with labels %v", f.DCGMFieldID, f.Series, want.Series, labelsOf(want.Labels))
			continue
		}
		matched[k] = true
		m := scalarExprRE.FindStringSubmatch(r.Expr)
		if m == nil {
			t.Errorf("field %d (%s): expr %q is not `<series>` or `<series> * <factor>`; a canonical rule is a plain transform of one series", f.DCGMFieldID, f.Series, r.Expr)
			continue
		}
		if m[1] != f.Series {
			t.Errorf("field %d: rule %s reads %s, but the exporter publishes the field as %s", f.DCGMFieldID, k, m[1], f.Series)
		}
		factor := 1.0
		if m[2] != "" {
			v, err := strconv.ParseFloat(m[2], 64)
			if err != nil {
				t.Errorf("field %d: factor %q in %q does not parse: %v", f.DCGMFieldID, m[2], r.Expr, err)
				continue
			}
			factor = v
		}
		if factor != want.Factor {
			t.Errorf("field %d (%s): rule factor %v, catalog factor %v (%s -> %s)", f.DCGMFieldID, f.Series, factor, want.Factor, f.Raw.Unit, want.Unit)
		}
		if want.Factor == 1 && m[2] != "" {
			t.Errorf("field %d: factor 1 must be written as the bare series, got %q", f.DCGMFieldID, r.Expr)
		}
	}
	if expected == 0 {
		t.Fatal("no exposed canonical field; the harness is broken")
	}
	for k, r := range byKey {
		if !matched[k] {
			t.Errorf("rule %s (expr %q) records a series no exposed catalog field maps to", k, r.Expr)
		}
	}
	if len(byKey) != expected {
		t.Errorf("rule count %d, catalog expects %d (one per exposed scalar mapping, four for field 112)", len(byKey), expected)
	}
}

// What the rules must not do: read a field the profile does not expose
// (190 P-state, 230 XID, 1523 recovery action) or does not catalog (NVLink
// byte rates 1011/1012), synthesise samples (`or vector(0)`, `default`,
// `absent(`), or aggregate across labels the label contract leaves optional
// (cluster, node_pool, driver), which would drop series on clusters that
// do not set them.
func TestCanonicalRulesRejectForbiddenShapes(t *testing.T) {
	c := Default()
	forbiddenSeries := map[string]string{}
	for _, id := range []int{190, 230, 1523} {
		f, ok := c.LookupDCGM(id)
		if !ok {
			t.Fatalf("field %d missing from the catalog", id)
		}
		if f.ExporterExposed {
			t.Fatalf("field %d is exposed; this test assumes the profile excludes it", id)
		}
		forbiddenSeries[f.Series] = fmt.Sprintf("field %d", id)
		for _, alias := range f.SeriesAliases {
			forbiddenSeries[alias] = fmt.Sprintf("field %d alias", id)
		}
	}
	for _, s := range []string{"DCGM_FI_PROF_NVLINK_TX_BYTES", "DCGM_FI_PROF_NVLINK_RX_BYTES"} {
		if _, ok := c.LookupSeries(s); ok {
			t.Fatalf("%s is cataloged; update this test's uncataloged list", s)
		}
		forbiddenSeries[s] = "uncataloged T2 field (1011/1012)"
	}
	reserved := map[string]bool{}
	for _, r := range c.ReservedCanonical {
		reserved[r.Series] = true
	}
	if len(reserved) == 0 {
		t.Fatal("no reserved canonical names; the harness is broken")
	}
	identRE := regexp.MustCompile(`[A-Za-z_][A-Za-z0-9_]*`)
	groupingRE := regexp.MustCompile(`\b(by|on|without|ignoring|group_left|group_right)\s*\(([^)]*)\)`)
	selectorRE := regexp.MustCompile(`\{([^}]*)\}`)
	optional := map[string]bool{"cluster": true, "node_pool": true, "driver": true}
	for _, g := range loadCanonicalRules(t) {
		for _, r := range g.Rules {
			where := fmt.Sprintf("group %s record %s (expr %q)", g.Name, r.Record, r.Expr)
			if reserved[r.Record] {
				t.Errorf("%s: records a reserved canonical name; reserved names stay unrecorded until their source is cataloged", where)
			}
			for _, id := range identRE.FindAllString(r.Expr, -1) {
				if why, bad := forbiddenSeries[id]; bad {
					t.Errorf("%s: reads %s (%s), which the shipped profile does not expose", where, id, why)
				}
			}
			for _, bad := range []string{"or vector(0)", "default", "absent("} {
				if strings.Contains(r.Expr, bad) {
					t.Errorf("%s: contains %q; an absent source must stay absent, never be synthesised", where, bad)
				}
			}
			for _, m := range groupingRE.FindAllStringSubmatch(r.Expr, -1) {
				for _, lbl := range strings.Split(m[2], ",") {
					if optional[strings.TrimSpace(lbl)] {
						t.Errorf("%s: groups by %s via %s(...); that label is optional in the label contract and the rule would drop series without it", where, strings.TrimSpace(lbl), m[1])
					}
				}
			}
			for _, m := range selectorRE.FindAllStringSubmatch(r.Expr, -1) {
				for _, matcher := range strings.Split(m[1], ",") {
					name := strings.TrimSpace(strings.SplitN(matcher, "=", 2)[0])
					name = strings.TrimRight(name, "!~")
					if optional[name] {
						t.Errorf("%s: selects on %s; that label is optional in the label contract", where, name)
					}
				}
			}
			for k := range r.Labels {
				if optional[k] {
					t.Errorf("%s: fixes label %s; identity and placement labels flow through from the source, they are never fixed", where, k)
				}
			}
		}
	}
}

// The fixed labels a rule adds must be catalog label names with values the
// catalog assigns to that series (a canonical mapping for gpu-canonical-*
// groups, a derived entry for gpu-derived-* groups), so the rules can never
// widen the label contract on their own.
func TestCanonicalRuleLabelsAreCatalogLabels(t *testing.T) {
	c := Default()
	allowed := map[string]bool{}
	for _, n := range CanonicalLabelNames {
		allowed[n] = true
	}
	for _, n := range DerivedLabelNames {
		allowed[n] = true
	}
	for _, g := range loadCanonicalRules(t) {
		for _, r := range g.Rules {
			for k, v := range r.Labels {
				if !allowed[k] {
					t.Errorf("group %s record %s: label %s is not a canonical or derived label name (%v, %v)", g.Name, r.Record, k, CanonicalLabelNames, DerivedLabelNames)
					continue
				}
				found := false
				for _, d := range c.LookupDerived(r.Record) {
					for _, l := range d.Labels {
						if l.Name == k && l.Value == v {
							found = true
						}
					}
				}
				for _, f := range c.LookupCanonical(r.Record) {
					for _, l := range f.Canonical.Labels {
						if l.Name == k && l.Value == v {
							found = true
						}
					}
					for _, vl := range f.Canonical.ValueLabels {
						if vl == k {
							_, found = clockEventBits[v]
						}
					}
				}
				if !found {
					t.Errorf("group %s record %s: label %s=%q is assigned to no catalog mapping of that series", g.Name, r.Record, k, v)
				}
			}
		}
	}
}

// loadCatalogYAML parses the generated catalog on disk (PathYAML) into the
// same yamlCatalog shape the renderer writes. The on-disk file, not
// Default(), is what an operator reads next to the rules, so the rules are
// also judged against it: a formula or factor that drifts in the YAML while
// the Go tables and the rules stay put must fail here.
func loadCatalogYAML(t *testing.T) yamlCatalog {
	t.Helper()
	src, err := os.ReadFile(filepath.Join(repoRoot(t), PathYAML))
	if err != nil {
		t.Fatal(err)
	}
	var c yamlCatalog
	if err := yaml.Unmarshal(src, &c); err != nil {
		t.Fatalf("%s: does not parse: %v", PathYAML, err)
	}
	if len(c.Fields) == 0 || len(c.Derived) == 0 {
		t.Fatalf("%s: no fields or no derived entries; the harness is broken", PathYAML)
	}
	return c
}

// canonicalRuleMismatches is the pure core of the YAML-vs-rules check for
// the gpu-canonical-* groups. It returns one message per defect: an exposed
// mapped field with no rule, a rule whose source series or factor is not
// the catalog's, a decode rule testing the wrong bit, a rule that records
// a series no exposed field maps to, or a duplicate rule. Empty means the
// rules implement exactly the catalog's canonical mappings.
func canonicalRuleMismatches(fields []yamlField, groups []ruleGroup) []string {
	var out []string
	byKey := map[string]recordingRule{}
	for _, g := range groups {
		for _, r := range g.Rules {
			k := ruleKey(r)
			if _, dup := byKey[k]; dup {
				out = append(out, fmt.Sprintf("group %s: %s is recorded twice", g.Name, k))
			}
			byKey[k] = r
		}
	}
	matched := map[string]bool{}
	for _, f := range fields {
		if f.DCGMFieldID == 0 || !f.ExporterExposed || f.Canonical == nil {
			continue
		}
		want := f.Canonical
		if len(want.ValueLabels) > 0 {
			if f.DCGMFieldID != 112 || !reflect.DeepEqual(want.ValueLabels, []string{"reason"}) {
				out = append(out, fmt.Sprintf("field %d: the only decoded mapping is field 112 by reason, got %v", f.DCGMFieldID, want.ValueLabels))
				continue
			}
			for reason, bit := range clockEventBits {
				k := ruleKey(recordingRule{Record: want.Series, Labels: map[string]string{"reason": reason}})
				r, ok := byKey[k]
				if !ok {
					out = append(out, fmt.Sprintf("field %d: no rule records %s", f.DCGMFieldID, k))
					continue
				}
				matched[k] = true
				m := decodeExprRE.FindStringSubmatch(r.Expr)
				if m == nil {
					out = append(out, fmt.Sprintf("field %d reason %s: expr %q is not a bitmap_and decode", f.DCGMFieldID, reason, r.Expr))
					continue
				}
				if m[1] != f.Series {
					out = append(out, fmt.Sprintf("field %d reason %s: decodes %s, catalog source is %s", f.DCGMFieldID, reason, m[1], f.Series))
				}
				if got, _ := strconv.Atoi(m[2]); got != bit {
					out = append(out, fmt.Sprintf("field %d reason %s: tests bit %d, want 0x%02x", f.DCGMFieldID, reason, got, bit))
				}
			}
			continue
		}
		k := ruleKey(recordingRule{Record: want.Series, Labels: labelsOf(want.Labels)})
		r, ok := byKey[k]
		if !ok {
			out = append(out, fmt.Sprintf("field %d (%s): no rule records %s", f.DCGMFieldID, f.Series, k))
			continue
		}
		matched[k] = true
		m := scalarExprRE.FindStringSubmatch(r.Expr)
		if m == nil {
			out = append(out, fmt.Sprintf("field %d (%s): expr %q is not `<series>` or `<series> * <factor>`", f.DCGMFieldID, f.Series, r.Expr))
			continue
		}
		if m[1] != f.Series {
			out = append(out, fmt.Sprintf("field %d: rule %s reads %s, catalog source is %s", f.DCGMFieldID, k, m[1], f.Series))
		}
		factor := 1.0
		if m[2] != "" {
			v, err := strconv.ParseFloat(m[2], 64)
			if err != nil {
				out = append(out, fmt.Sprintf("field %d: factor %q does not parse: %v", f.DCGMFieldID, m[2], err))
				continue
			}
			factor = v
		}
		if factor != want.Factor {
			out = append(out, fmt.Sprintf("field %d (%s): rule factor %v, catalog factor %v", f.DCGMFieldID, f.Series, factor, want.Factor))
		}
	}
	for k, r := range byKey {
		if !matched[k] {
			out = append(out, fmt.Sprintf("rule %s (expr %q) records a series no exposed catalog field maps to", k, r.Expr))
		}
	}
	sort.Strings(out)
	return out
}

// derivedRuleMismatches is the pure core of the YAML-vs-rules check for the
// gpu-derived-* groups. Every non-deferred derived entry must be recorded
// by exactly one rule, in the group the entry names, with the entry's
// fixed labels and an expr equal to the entry's formula; a deferred entry
// must have no rule; no derived rule may record anything else.
func derivedRuleMismatches(derived []DerivedSignal, groups []ruleGroup) []string {
	var out []string
	type placed struct {
		group string
		rule  recordingRule
	}
	byKey := map[string]placed{}
	for _, g := range groups {
		for _, r := range g.Rules {
			k := ruleKey(r)
			if _, dup := byKey[k]; dup {
				out = append(out, fmt.Sprintf("group %s: %s is recorded twice", g.Name, k))
			}
			byKey[k] = placed{g.Name, r}
		}
	}
	matched := map[string]bool{}
	for _, d := range derived {
		k := ruleKey(recordingRule{Record: d.Series, Labels: labelsOf(d.Labels)})
		p, ok := byKey[k]
		if d.Deferred != "" {
			if ok {
				out = append(out, fmt.Sprintf("%s is deferred in the catalog but group %s records it", k, p.group))
			}
			continue
		}
		if !ok {
			out = append(out, fmt.Sprintf("%s (group %s): no derived rule records it", k, d.Group))
			continue
		}
		matched[k] = true
		if p.group != d.Group {
			out = append(out, fmt.Sprintf("%s: catalog group %s, rule group %s", k, d.Group, p.group))
		}
		if strings.TrimSpace(p.rule.Expr) != strings.TrimSpace(d.Formula) {
			out = append(out, fmt.Sprintf("%s: rule expr %q, catalog formula %q", k, p.rule.Expr, d.Formula))
		}
	}
	for k, p := range byKey {
		if !matched[k] {
			out = append(out, fmt.Sprintf("group %s: rule %s records a series no catalog derived entry declares", p.group, k))
		}
	}
	sort.Strings(out)
	return out
}

// derivedGroups returns only the gpu-derived-* groups.
func derivedGroups(t *testing.T) []ruleGroup {
	t.Helper()
	var out []ruleGroup
	for _, g := range loadCanonicalRules(t) {
		if strings.HasPrefix(g.Name, DerivedGroupPrefix) {
			out = append(out, g)
		}
	}
	if len(out) == 0 {
		t.Fatalf("%s: no %s* group; the harness is broken", CanonicalRulePath, DerivedGroupPrefix)
	}
	return out
}

// The on-disk generated catalog (config/gpu-metrics/catalog.yaml), parsed
// with the renderer's own types, is implemented by the rule file: every
// exposed canonical mapping by exactly one gpu-canonical-* rule with the
// YAML's source, factor and fixed labels (four reason rules for field 112),
// every non-deferred derived entry by exactly one rule in its named
// gpu-derived-* group with the YAML's formula and labels, the deferred
// presence entry by none, and nothing else. Comments in either file play no
// part: only parsed record, expr and labels are compared.
func TestRecordingRulesImplementCatalogYAML(t *testing.T) {
	c := loadCatalogYAML(t)
	exposed, deferredDerived := 0, 0
	for _, f := range c.Fields {
		if f.DCGMFieldID != 0 && f.ExporterExposed && f.Canonical != nil {
			exposed++
		}
	}
	for _, d := range c.Derived {
		if d.Deferred != "" {
			deferredDerived++
		}
	}
	if exposed == 0 || deferredDerived == 0 {
		t.Fatalf("%s: %d exposed mapped fields, %d deferred derived entries; the harness is broken", PathYAML, exposed, deferredDerived)
	}
	for _, msg := range canonicalRuleMismatches(c.Fields, canonicalGroups(t)) {
		t.Errorf("canonical: %s", msg)
	}
	for _, msg := range derivedRuleMismatches(c.Derived, derivedGroups(t)) {
		t.Errorf("derived: %s", msg)
	}
}

// The pure helpers reject the drifts the YAML harness exists to catch: a
// scalar factor the rule does not apply, and a derived formula the rule
// does not state. Both start from a passing pair so a failure is the
// mutation's alone.
func TestRuleMismatchHelpersRejectDrift(t *testing.T) {
	field := func(factor float64) []yamlField {
		return []yamlField{{Field: Field{
			DCGMFieldID: 155, Series: "DCGM_FI_DEV_POWER_USAGE", ExporterExposed: true,
			Canonical: &Canonical{Series: "kubeneuron_gpu_power_watts", Factor: factor},
		}}}
	}
	canonRules := []ruleGroup{{Name: "gpu-canonical-power", Rules: []recordingRule{
		{Record: "kubeneuron_gpu_power_watts", Expr: "DCGM_FI_DEV_POWER_USAGE"},
	}}}
	if got := canonicalRuleMismatches(field(1), canonRules); len(got) != 0 {
		t.Fatalf("passing canonical pair reported %v", got)
	}
	got := canonicalRuleMismatches(field(0.001), canonRules)
	if len(got) != 1 || !strings.Contains(got[0], "rule factor 1, catalog factor 0.001") {
		t.Errorf("wrong scalar factor not rejected: %v", got)
	}

	derived := func(formula string) []DerivedSignal {
		return []DerivedSignal{{
			Series: "kubeneuron_gpu_power_ratio", Group: "gpu-derived-power", Formula: formula,
		}}
	}
	derivedRules := []ruleGroup{{Name: "gpu-derived-power", Rules: []recordingRule{
		{Record: "kubeneuron_gpu_power_ratio", Expr: "kubeneuron_gpu_power_watts / (kubeneuron_gpu_power_limit_watts > 0)"},
	}}}
	if got := derivedRuleMismatches(derived("kubeneuron_gpu_power_watts / (kubeneuron_gpu_power_limit_watts > 0)"), derivedRules); len(got) != 0 {
		t.Fatalf("passing derived pair reported %v", got)
	}
	got = derivedRuleMismatches(derived("kubeneuron_gpu_power_watts / kubeneuron_gpu_power_limit_watts"), derivedRules)
	if len(got) != 1 || !strings.Contains(got[0], "catalog formula") {
		t.Errorf("wrong derived formula not rejected: %v", got)
	}

	// A labeled component: the rule that carries the aggregate ECC label but
	// reads the volatile counter (or drops the component altogether) is a
	// drift the helper must name, since the label alone would look right.
	const hardFault = "kubeneuron_gpu_memory_hard_fault_active"
	eccRow := func(scope string) DerivedSignal {
		return DerivedSignal{
			Series: hardFault, Group: "gpu-derived-memory", Labels: []Label{L("component", "uncorrectable_ecc_"+scope)},
			Formula: `label_del(increase(kubeneuron_gpu_ecc_errors_total{bits="double",scope="` + scope + `"}[1h]), "bits", "scope") > bool 0`,
		}
	}
	eccRule := func(component, scope string) recordingRule {
		return recordingRule{
			Record: hardFault, Labels: map[string]string{"component": component},
			Expr: `label_del(increase(kubeneuron_gpu_ecc_errors_total{bits="double",scope="` + scope + `"}[1h]), "bits", "scope") > bool 0`,
		}
	}
	eccCatalog := []DerivedSignal{eccRow("volatile"), eccRow("aggregate")}
	okRules := []ruleGroup{{Name: "gpu-derived-memory", Rules: []recordingRule{eccRule("uncorrectable_ecc_volatile", "volatile"), eccRule("uncorrectable_ecc_aggregate", "aggregate")}}}
	if got := derivedRuleMismatches(eccCatalog, okRules); len(got) != 0 {
		t.Fatalf("passing ECC component pair reported %v", got)
	}
	wrongScope := []ruleGroup{{Name: "gpu-derived-memory", Rules: []recordingRule{eccRule("uncorrectable_ecc_volatile", "volatile"), eccRule("uncorrectable_ecc_aggregate", "volatile")}}}
	got = derivedRuleMismatches(eccCatalog, wrongScope)
	if len(got) != 1 || !strings.Contains(got[0], `{component=uncorrectable_ecc_aggregate}`) || !strings.Contains(got[0], "catalog formula") {
		t.Errorf("aggregate component reading the volatile counter not rejected: %v", got)
	}
	onlyVolatile := []ruleGroup{{Name: "gpu-derived-memory", Rules: []recordingRule{eccRule("uncorrectable_ecc_volatile", "volatile")}}}
	got = derivedRuleMismatches(eccCatalog, onlyVolatile)
	if len(got) != 1 || !strings.Contains(got[0], `{component=uncorrectable_ecc_aggregate}`) || !strings.Contains(got[0], "no derived rule records it") {
		t.Errorf("missing aggregate component not rejected: %v", got)
	}
}

// The mirror check rejects an in-memory divergence of the deployed VMRule
// from the canonical file in every field it compares: a changed expr or
// label, a dropped rule, a changed interval, a reordered or extra group.
// The deployed file on disk is not touched; the helper is exercised on
// parsed copies.
func TestMirrorHelperRejectsDivergence(t *testing.T) {
	canonical := loadCanonicalRules(t)
	cloneGroups := func(gs []ruleGroup) []ruleGroup {
		out := make([]ruleGroup, len(gs))
		for i, g := range gs {
			out[i] = g
			out[i].Rules = make([]recordingRule, len(g.Rules))
			for j, r := range g.Rules {
				out[i].Rules[j] = r
				if r.Labels != nil {
					out[i].Rules[j].Labels = map[string]string{}
					for k, v := range r.Labels {
						out[i].Rules[j].Labels[k] = v
					}
				}
			}
		}
		return out
	}
	if got := mirrorMismatches(canonical, cloneGroups(canonical)); len(got) != 0 {
		t.Fatalf("identical copy reported %v", got)
	}
	if got := mirrorMismatches(canonical, loadDeployedRecordingGroups(t)); len(got) != 0 {
		t.Fatalf("deployed VMRule diverges: %v", got)
	}
	cases := map[string]func(gs []ruleGroup) []ruleGroup{
		"expr": func(gs []ruleGroup) []ruleGroup {
			gs[len(gs)-1].Rules[0].Expr = "(" + gs[len(gs)-1].Rules[0].Expr + ") or vector(0)"
			return gs
		},
		"label": func(gs []ruleGroup) []ruleGroup {
			for i := range gs {
				for j := range gs[i].Rules {
					if gs[i].Rules[j].Labels != nil {
						gs[i].Rules[j].Labels["component"] = "other"
						return gs
					}
				}
			}
			t.Fatal("no labeled rule to mutate")
			return gs
		},
		"dropped rule": func(gs []ruleGroup) []ruleGroup { gs[0].Rules = gs[0].Rules[1:]; return gs },
		"interval":     func(gs []ruleGroup) []ruleGroup { gs[0].Interval = "1h"; return gs },
		"group order":  func(gs []ruleGroup) []ruleGroup { gs[0], gs[1] = gs[1], gs[0]; return gs },
		"extra group":  func(gs []ruleGroup) []ruleGroup { return append(gs, gs[0]) },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			if got := mirrorMismatches(canonical, mutate(cloneGroups(canonical))); len(got) == 0 {
				t.Fatalf("divergence %s not rejected", name)
			}
		})
	}
}

// exprIdentRE matches an identifier in a MetricsQL expression. Label keys
// (`by (node, gpu_uuid)`, `{domain=...}`) never carry the kubeneuron_
// prefix, so the prefix test below keeps only series references.
var exprIdentRE = regexp.MustCompile(`[A-Za-z_][A-Za-z0-9_]*`)

// kubeneuronRefs returns the distinct kubeneuron_* identifiers an expression
// reads, sorted, excluding anything inside a quoted label value.
func kubeneuronRefs(expr string) []string {
	var sb strings.Builder
	inString := false
	for _, r := range expr {
		if r == '"' {
			inString = !inString
			continue
		}
		if !inString {
			sb.WriteRune(r)
		}
	}
	seen := map[string]bool{}
	var out []string
	for _, id := range exprIdentRE.FindAllString(sb.String(), -1) {
		if strings.HasPrefix(id, "kubeneuron_") && !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	sort.Strings(out)
	return out
}

// No derived rule may read a kubeneuron_* series the catalog does not
// declare as its input: the identifiers parsed from the rule expr must be
// exactly the catalog entry's Inputs, and each input must be a canonical
// series, a T4 control-plane series or a derived series recorded by an
// earlier rule. Only parsed expr fields are read, never comments.
func TestDerivedRuleReferencesAreDeclaredInputs(t *testing.T) {
	c := Default()
	allowed := map[string]bool{}
	for _, s := range c.CanonicalSeries() {
		allowed[s] = true
	}
	for _, f := range c.Fields {
		if f.Tier == TierT4 {
			allowed[f.Series] = true
		}
	}
	byKey := map[string]DerivedSignal{}
	for _, d := range c.Derived {
		byKey[ruleKey(recordingRule{Record: d.Series, Labels: labelsOf(d.Labels)})] = d
	}
	recorded := map[string]bool{}
	checked := 0
	for _, g := range derivedGroups(t) {
		for _, r := range g.Rules {
			d, ok := byKey[ruleKey(r)]
			if !ok {
				t.Errorf("group %s: %s has no catalog entry", g.Name, ruleKey(r))
				continue
			}
			checked++
			refs := kubeneuronRefs(r.Expr)
			inputs := append([]string(nil), d.Inputs...)
			sort.Strings(inputs)
			if !reflect.DeepEqual(refs, inputs) {
				t.Errorf("%s: expr reads %v, catalog inputs %v", ruleKey(r), refs, inputs)
			}
			for _, in := range refs {
				if !allowed[in] && !recorded[in] {
					t.Errorf("%s: reads %s, which is neither canonical, T4 control-plane nor recorded by an earlier derived rule", ruleKey(r), in)
				}
			}
			recorded[r.Record] = true
		}
	}
	if checked < 10 {
		t.Fatalf("checked only %d derived rules; the harness is broken", checked)
	}
	// The parser itself: quoted label values are not references, and an
	// undeclared series would be reported.
	got := kubeneuronRefs(`max by (node, gpu_uuid) (kubeneuron_gpu_a{domain="kubeneuron_gpu_b"}) / kubeneuron_gpu_c`)
	if want := []string{"kubeneuron_gpu_a", "kubeneuron_gpu_c"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("kubeneuronRefs = %v, want %v", got, want)
	}
}
