package detect

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// M7 documentation invariant: every shipped shadow alert has a runbook
// entry, and every entry opens by saying the one thing an on-call reader
// must not get wrong: the rule does not trigger remediation and goes to
// humans. The section is also read-only by construction, so no entry may
// hand the reader a state-changing command.

const runbooksPath = "docs/runbooks.md"

// shadowRunbookOpening is the exact first sentence of every shadow entry.
// The wording is deliberate and pinned: a paraphrase elsewhere in the entry
// is fine, a different opening is not.
const shadowRunbookOpening = "This rule does not trigger remediation and goes to humans."

// Commands that change cluster or device state. A runbook for an
// observation-only alert must not tell a reader to run any of them; the
// controller-facing Gpu* entries above the shadow section may.
var destructiveCommandRE = regexp.MustCompile(
	`kubectl\b[^\n` + "`" + `]{0,60}\b(drain|cordon|uncordon|delete|rollout restart|patch|edit|apply|scale|exec)\b` +
		`|nvidia-smi\s+(-r\b|--gpu-reset)` +
		`|\b(systemctl|shutdown|reboot)\b` +
		`|kubeneuronctl remediate\b` +
		`|helm (upgrade|uninstall|install)\b`)

func shadowRunbookSection(t *testing.T) (string, int) {
	t.Helper()
	src, err := os.ReadFile(repoPath(runbooksPath))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(string(src), "\n")
	start := -1
	for i, l := range lines {
		if strings.HasPrefix(l, "## ") && strings.Contains(l, "Shadow-only GPU alerts") {
			start = i
			break
		}
	}
	if start < 0 {
		t.Fatalf("%s: no `## ... Shadow-only GPU alerts ...` section", runbooksPath)
	}
	end := len(lines)
	for i := start + 1; i < len(lines); i++ {
		if strings.HasPrefix(lines[i], "## ") {
			end = i
			break
		}
	}
	return strings.Join(lines[start:end], "\n"), start + 1
}

// entries splits the shadow section into (heading, body) pairs.
func shadowRunbookEntries(t *testing.T) map[string]string {
	t.Helper()
	section, _ := shadowRunbookSection(t)
	entries := map[string]string{}
	var name string
	var body []string
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
		body = append(body, l)
	}
	flush()
	return entries
}

// Every alert in the canonical shadow file has exactly one runbook entry,
// and the runbook has no entry for an alert that does not ship.
func TestEveryShadowAlertHasARunbookEntryAndViceVersa(t *testing.T) {
	entries := shadowRunbookEntries(t)
	if len(entries) == 0 {
		t.Fatalf("%s: shadow section has no `### ShadowGpu*` entries; the harness is broken", runbooksPath)
	}
	shipped := map[string]bool{}
	for _, r := range shadowRules(t) {
		shipped[r.Alert] = true
		if _, ok := entries[r.Alert]; !ok {
			t.Errorf("%s: no runbook entry `### %s`", runbooksPath, r.Alert)
		}
	}
	for name := range entries {
		if !strings.HasPrefix(name, shadowAlertPrefix) {
			t.Errorf("%s: entry %q in the shadow section is not a %s* alert", runbooksPath, name, shadowAlertPrefix)
		}
		if !shipped[name] {
			t.Errorf("%s: runbook entry %q documents an alert that %s does not ship", runbooksPath, name, shadowRulesPath)
		}
	}
}

// Every entry opens with the pinned sentence, as the first sentence of the
// first paragraph, not merely somewhere in the body.
func TestEveryShadowRunbookEntryOpensWithTheNoRemediationSentence(t *testing.T) {
	for name, body := range shadowRunbookEntries(t) {
		if !strings.HasPrefix(body, shadowRunbookOpening) {
			first := body
			if i := strings.IndexByte(first, '\n'); i >= 0 {
				first = first[:i]
			}
			t.Errorf("%s: entry %s must open with %q; opens with %q", runbooksPath, name, shadowRunbookOpening, first)
		}
	}
}

// The shadow section tells a reader where to look, never what to change.
func TestShadowRunbookSectionHasNoDestructiveCommands(t *testing.T) {
	section, firstLine := shadowRunbookSection(t)
	for i, l := range strings.Split(section, "\n") {
		if m := destructiveCommandRE.FindString(l); m != "" {
			t.Errorf("%s:%d: shadow runbook names a state-changing command %q: %s", runbooksPath, firstLine+i, m, strings.TrimSpace(l))
		}
	}
}

// The invariant must be falsifiable: a body with the sentence buried in its
// second paragraph, or missing, is rejected by the same check the shipped
// entries pass.
func TestShadowRunbookOpeningCheckRejectsBuriedSentence(t *testing.T) {
	for _, body := range []string{
		"Some context first. " + shadowRunbookOpening,
		"Some context first.\n\n" + shadowRunbookOpening,
		"The rule never triggers remediation.",
		"",
	} {
		if strings.HasPrefix(body, shadowRunbookOpening) {
			t.Errorf("opening check accepted %q", body)
		}
	}
	if !strings.HasPrefix(shadowRunbookOpening+" More.", shadowRunbookOpening) {
		t.Error("opening check rejects a conforming body")
	}
	if destructiveCommandRE.FindString("run `kubectl drain node-1` now") == "" ||
		destructiveCommandRE.FindString("nvidia-smi --gpu-reset -i 0") == "" ||
		destructiveCommandRE.FindString("kubectl -n gpu-operator rollout restart ds/x") == "" {
		t.Error("destructive-command check misses a command it exists to catch")
	}
	if destructiveCommandRE.FindString("kubectl -n gpu-operator logs ds/nvidia-dcgm-exporter --tail=200") != "" ||
		destructiveCommandRE.FindString("kubeneuronctl incidents --node n1") != "" {
		t.Error("destructive-command check flags a read-only command")
	}
}
