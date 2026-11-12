// SPDX-License-Identifier: LicenseRef-KubeNeuron

package detect

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// M7 documentation invariant for the v0.8 workload-safe recovery alerts:
// every self-health rule that reads a v0.8 counter (impact previews,
// checkpoint advisories) has exactly one runbook entry in the section
// reserved for them, every entry there documents such a rule, each opens by
// saying it does not trigger remediation, the section says what KubeNeuron
// does not measure, and no entry hands the reader a state-changing command
// or a write into a Kueue or Ray object — the two things the release plan
// forbids any public text to suggest.

const (
	selfRulesPath = "configs/vmalert/self-rules.yaml"
	// workloadSafeOpening is the pinned first sentence of every entry.
	workloadSafeOpening = "This rule does not trigger remediation and goes to humans."
	// workloadSafeNoClaim is the pinned sentence the section must carry.
	workloadSafeNoClaim = "it does not\nmeasure saved or lost training progress"
)

// workloadSafeMetricRE matches the v0.8 counters a rule may read.
var workloadSafeMetricRE = regexp.MustCompile(`kubeneuron_(impact_preview|checkpoint_advisor)[a-z_]*`)

// foreignWriteRE matches a kubectl write into a Kueue or Ray object.
var foreignWriteRE = regexp.MustCompile(`(?i)kubectl\b[^\n]{0,80}\b(patch|edit|delete|apply|annotate|label|scale)\b[^\n]{0,80}\b(workloads?|localqueues?|clusterqueues?|rayclusters?|rayjobs?)\b`)

func workloadSafeRules(t *testing.T) []alertRule {
	t.Helper()
	var out []alertRule
	for _, g := range loadRuleGroups(t, selfRulesPath) {
		for _, r := range g.Rules {
			if r.Alert != "" && workloadSafeMetricRE.MatchString(r.Expr) {
				out = append(out, r)
			}
		}
	}
	return out
}

func workloadSafeRunbookSection(t *testing.T) string {
	t.Helper()
	src, err := os.ReadFile(repoPath(runbooksPath))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(string(src), "\n")
	start := -1
	for i, l := range lines {
		if strings.HasPrefix(l, "## ") && strings.Contains(l, "Workload-safe recovery alerts") {
			start = i
			break
		}
	}
	if start < 0 {
		t.Fatalf("%s: no `## Workload-safe recovery alerts ...` section", runbooksPath)
	}
	end := len(lines)
	for i := start + 1; i < len(lines); i++ {
		if strings.HasPrefix(lines[i], "## ") {
			end = i
			break
		}
	}
	return strings.Join(lines[start:end], "\n")
}

func workloadSafeEntries(t *testing.T) (preamble string, entries map[string]string) {
	t.Helper()
	section := workloadSafeRunbookSection(t)
	entries = map[string]string{}
	var name string
	var body []string
	var pre []string
	flush := func() {
		if name != "" {
			entries[name] = strings.TrimSpace(strings.Join(body, "\n"))
		}
	}
	for _, l := range strings.Split(section, "\n") {
		if strings.HasPrefix(l, "### ") {
			flush()
			name = strings.TrimSpace(strings.TrimPrefix(l, "### "))
			body = nil
			continue
		}
		if name == "" {
			pre = append(pre, l)
			continue
		}
		body = append(body, l)
	}
	flush()
	return strings.Join(pre, "\n"), entries
}

func TestWorkloadSafeAlertsShipAndHaveRunbookEntries(t *testing.T) {
	rules := workloadSafeRules(t)
	if len(rules) < 2 {
		t.Fatalf("%s: expected at least the two v0.8 self-health rules reading impact preview and advisory counters, found %d", selfRulesPath, len(rules))
	}
	_, entries := workloadSafeEntries(t)
	shipped := map[string]bool{}
	for _, r := range rules {
		shipped[r.Alert] = true
		if _, ok := entries[r.Alert]; !ok {
			t.Errorf("%s: no runbook entry `### %s`", runbooksPath, r.Alert)
		}
	}
	for name := range entries {
		if !shipped[name] {
			t.Errorf("%s: entry %q is in the workload-safe section but no %s rule reading a v0.8 counter carries that name", runbooksPath, name, selfRulesPath)
		}
	}
}

func TestWorkloadSafeRunbookEntriesOpenHonestlyAndStayReadOnly(t *testing.T) {
	preamble, entries := workloadSafeEntries(t)
	if !strings.Contains(preamble, workloadSafeNoClaim) {
		t.Errorf("%s: the workload-safe section must say %q", runbooksPath, strings.ReplaceAll(workloadSafeNoClaim, "\n", " "))
	}
	for name, body := range entries {
		if !strings.HasPrefix(body, workloadSafeOpening) {
			t.Errorf("%s: entry %s must open with %q; got %q", runbooksPath, name, workloadSafeOpening, firstLine(body))
		}
		if m := destructiveCommandRE.FindString(body); m != "" {
			t.Errorf("%s: entry %s tells the reader to run a state-changing command: %q", runbooksPath, name, m)
		}
		if m := foreignWriteRE.FindString(body); m != "" {
			t.Errorf("%s: entry %s suggests a write into a Kueue or Ray object: %q", runbooksPath, name, m)
		}
		for _, bad := range []string{"checkpoint saved", "progress saved", "GPU-hours saved"} {
			if strings.Contains(strings.ToLower(body), bad) {
				t.Errorf("%s: entry %s claims %q, which the protocol cannot know", runbooksPath, name, bad)
			}
		}
	}
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
