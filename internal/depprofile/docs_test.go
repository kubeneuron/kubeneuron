package depprofile

import (
	"encoding/json"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/kubeneuron/kubeneuron/internal/gpucatalog"
)

// The public documents that describe the profile and the label contract
// are checked against the files they describe: the rollback snippet must be
// the v0.6.1 list exactly, the cluster example must be a valid patch of the
// documented shape, the node_pool precedence must be stated in the order the
// rules implement, and the honesty statements must be present verbatim.

// The README's rollback block is the nine-series v0.6.1 profile, in order,
// parsed exactly as the exporter would parse the resulting ConfigMap.
func TestReadmeRollbackSnippetIsLegacyProfile(t *testing.T) {
	readme := string(readFile(t, dependenciesDoc))
	var found bool
	for _, b := range fencedBlocks(readme) {
		if !strings.Contains(b.Body, "data: |-") {
			continue
		}
		var snippet struct {
			Data string `yaml:"data"`
		}
		if err := yaml.Unmarshal([]byte(b.Body), &snippet); err != nil {
			t.Fatalf("%s:%d: rollback snippet is not YAML: %v", dependenciesDoc, b.Line, err)
		}
		rows, err := gpucatalog.ParseProfileCSV(snippet.Data)
		if err != nil {
			t.Fatalf("%s:%d: rollback snippet does not parse as an exporter CSV: %v", dependenciesDoc, b.Line, err)
		}
		var series []string
		for _, r := range rows {
			series = append(series, r.Series)
		}
		if !reflect.DeepEqual(series, gpucatalog.LegacyProfileSeries) {
			t.Errorf("%s:%d: rollback snippet lists %v, want the v0.6.1 profile %v", dependenciesDoc, b.Line, series, gpucatalog.LegacyProfileSeries)
		}
		found = true
	}
	if !found {
		t.Fatalf("%s: no fenced rollback snippet containing `data: |-`", dependenciesDoc)
	}
}

var patchRE = regexp.MustCompile(`kubectl patch vmagent kubeneuron[^']*-p '(\{.*\})'`)

// Every documented `kubectl patch vmagent` example sets exactly
// spec.externalLabels.cluster to a non-empty name: a real command, not
// pseudo-code, and nothing but the cluster label.
func TestDocsClusterPatchIsWellFormed(t *testing.T) {
	for _, doc := range []string{dependenciesDoc, operationsDoc} {
		text := string(readFile(t, doc))
		matches := patchRE.FindAllStringSubmatch(strings.ReplaceAll(text, "\\\n", " "), -1)
		if len(matches) == 0 {
			t.Errorf("%s: no `kubectl patch vmagent kubeneuron ... -p '{...}'` example for cluster", doc)
			continue
		}
		for _, m := range matches {
			var body struct {
				Spec struct {
					ExternalLabels map[string]string `json:"externalLabels"`
				} `json:"spec"`
			}
			if err := json.Unmarshal([]byte(m[1]), &body); err != nil {
				t.Errorf("%s: patch body %q is not JSON: %v", doc, m[1], err)
				continue
			}
			var generic map[string]any
			_ = json.Unmarshal([]byte(m[1]), &generic)
			spec, _ := generic["spec"].(map[string]any)
			if len(generic) != 1 || len(spec) != 1 {
				t.Errorf("%s: patch %q must touch spec.externalLabels only", doc, m[1])
			}
			if len(body.Spec.ExternalLabels) != 1 || body.Spec.ExternalLabels["cluster"] == "" {
				t.Errorf("%s: patch %q must set exactly externalLabels.cluster", doc, m[1])
			}
			if v := body.Spec.ExternalLabels["cluster"]; strings.ContainsAny(v, "<>") {
				t.Errorf("%s: cluster example %q should be a concrete name, not a placeholder", doc, v)
			}
		}
		if !strings.Contains(text, "spec.externalLabels") {
			t.Errorf("%s must name the spec.externalLabels field", doc)
		}
	}
}

// The precedence prose lists the four node labels in the order the relabel
// rules apply them (lowest first), and says what happens without any.
func TestDocsNodePoolPrecedenceMatchesRules(t *testing.T) {
	_, jobs := loadVMAgent(t)
	j := dcgmJob(t, jobs)
	var ruleOrder []string
	for _, r := range j.RelabelConfigs {
		if r.TargetLabel == "node_pool" {
			ruleOrder = append(ruleOrder, r.SourceLabels[0])
		}
	}
	for _, doc := range []string{dependenciesDoc, operationsDoc} {
		text := squash(string(readFile(t, doc)))
		last := -1
		for i, key := range nodePoolSources {
			idx := strings.Index(text, "`"+key+"`")
			if idx < 0 {
				t.Errorf("%s: does not name node label %s", doc, key)
				continue
			}
			if idx < last {
				t.Errorf("%s: node label %s is listed out of precedence order (rules apply %v)", doc, key, nodePoolSources)
			}
			last = idx
			want := "__meta_kubernetes_node_label_" + sanitizeLabelName(key)
			if ruleOrder[i] != want {
				t.Errorf("rule %d reads %s, docs promise %s", i, ruleOrder[i], key)
			}
		}
		for _, want := range []string{"never an empty string", "No shipped rule or dashboard may require", "intentionally absent", "observability only"} {
			if !strings.Contains(text, want) {
				t.Errorf("%s must state %q", doc, want)
			}
		}
	}
}

// The honesty and scope statements the release plan requires are present
// in both documents: M2 adds observability only, starts no remediation, and
// the added fields are unvalidated on hardware.
func TestDocsScopeStatements(t *testing.T) {
	readme := string(readFile(t, dependenciesDoc))
	// Only the v0.7 section of the operations guide is this milestone's; the
	// rest of that document is checked by hack/verify-docs.sh.
	ops := section(t, string(readFile(t, operationsDoc)), "## GPU reliability telemetry: exporter profile and scrape labels")
	for doc, tc := range map[string]struct {
		text  string
		wants []string
	}{
		dependenciesDoc: {readme, []string{
			"it starts no remediation",
			"has not been observed on any GPU by this project",
			"read by the recording rules only",
			"route to humans and open no incident",
			"`driver` is **not** produced",
			"nvidia.com/cuda.driver-version.full",
			"DCGM_FI_DRIVER_VERSION",
			"git show v0.6.1:deploy/kubernetes/dependencies/gpu-operator/values.yaml",
			"kubectl rollout restart daemonset/nvidia-dcgm-exporter",
		}},
		operationsDoc: {ops, []string{
			"nothing here starts a remediation",
			"has not been observed on any GPU by this project",
			"`driver` is **intentionally absent**",
			"kubectl rollout restart daemonset/nvidia-dcgm-exporter",
			"reference-gpu-metrics.md",
		}},
	} {
		text := squash(tc.text)
		for _, want := range tc.wants {
			if !strings.Contains(text, want) {
				t.Errorf("%s must contain %q", doc, want)
			}
		}
		// A lone "T4" is forbidden in public prose: it is either "Tier T4"
		// (the control plane) or "Tesla T4" (the GPU).
		for i, line := range strings.Split(tc.text, "\n") {
			for _, m := range regexp.MustCompile(`\bT4\b`).FindAllStringIndex(line, -1) {
				before := line[:m[0]]
				if !strings.HasSuffix(before, "Tesla ") && !strings.HasSuffix(before, "Tier ") && !strings.HasSuffix(before, "tier ") {
					t.Errorf("%s:%d: lone \"T4\" (write Tesla T4 or Tier T4): %s", doc, i+1, strings.TrimSpace(line))
				}
			}
		}
	}
}
