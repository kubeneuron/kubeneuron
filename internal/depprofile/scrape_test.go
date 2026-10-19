package depprofile

import (
	"bytes"
	"regexp"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// The scrape label contract (v0.7 M2) lives in the VMAgent resource's
// inline scrape config. These tests parse it as the relabel rules it is and
// evaluate them, instead of grepping for strings: the node_pool fallback is
// an ordering property of `replace` rules, and only running the rules over
// label sets proves the order and the non-empty guarantee.

// Node label keys the contract derives node_pool from, lowest precedence
// first. The last matching rule wins, so EKS beats GKE beats Karpenter
// beats the KubeNeuron fallback.
var nodePoolSources = []string{
	"kubeneuron.io/node-pool",
	"karpenter.sh/nodepool",
	"cloud.google.com/gke-nodepool",
	"eks.amazonaws.com/nodegroup",
}

type vmagentSpec struct {
	Image struct {
		Repository string `yaml:"repository"`
		Tag        string `yaml:"tag"`
		PullPolicy string `yaml:"pullPolicy"`
	} `yaml:"image"`
	SelectAllByDefault *bool             `yaml:"selectAllByDefault"`
	ScrapeInterval     string            `yaml:"scrapeInterval"`
	RemoteWrite        []map[string]any  `yaml:"remoteWrite"`
	InlineScrapeConfig string            `yaml:"inlineScrapeConfig"`
	ExternalLabels     map[string]string `yaml:"externalLabels"`
	IngestOnlyMode     *bool             `yaml:"ingestOnlyMode"`
	ServiceAccountName string            `yaml:"serviceAccountName"`
	Resources          map[string]any    `yaml:"resources"`
}

type vmagentDoc struct {
	APIVersion string `yaml:"apiVersion"`
	Kind       string `yaml:"kind"`
	Metadata   struct {
		Name      string            `yaml:"name"`
		Namespace string            `yaml:"namespace"`
		Labels    map[string]string `yaml:"labels"`
	} `yaml:"metadata"`
	Spec vmagentSpec `yaml:"spec"`
}

// scrapeJob is the subset of a Prometheus-style scrape_config the inline
// config uses. Decoding is strict (KnownFields), so a misspelled key such as
// attach_metadta fails here instead of being ignored by vmagent.
type scrapeJob struct {
	JobName              string         `yaml:"job_name"`
	KubernetesSDConfigs  []k8sSDConfig  `yaml:"kubernetes_sd_configs"`
	RelabelConfigs       []relabelRule  `yaml:"relabel_configs"`
	MetricRelabelConfigs []relabelRule  `yaml:"metric_relabel_configs"`
	Other                map[string]any `yaml:",inline"`
}

type k8sSDConfig struct {
	Role       string `yaml:"role"`
	Namespaces struct {
		Names []string `yaml:"names"`
	} `yaml:"namespaces"`
	AttachMetadata map[string]bool `yaml:"attach_metadata"`
}

type relabelRule struct {
	Action       string   `yaml:"action"`
	SourceLabels []string `yaml:"source_labels"`
	Separator    *string  `yaml:"separator"`
	Regex        *string  `yaml:"regex"`
	TargetLabel  string   `yaml:"target_label"`
	Replacement  *string  `yaml:"replacement"`
}

func (r relabelRule) regex() string {
	if r.Regex == nil {
		return "(.*)"
	}
	return *r.Regex
}

func (r relabelRule) replacement() string {
	if r.Replacement == nil {
		return "$1"
	}
	return *r.Replacement
}

func (r relabelRule) separator() string {
	if r.Separator == nil {
		return ";"
	}
	return *r.Separator
}

func (r relabelRule) action() string {
	if r.Action == "" {
		return "replace"
	}
	return r.Action
}

// apply runs one relabel rule over a label set the way Prometheus and
// vmagent do for the actions the contract uses: the regex is anchored, the
// source values are joined by the separator, `replace` sets the target to
// the expanded replacement on a match and leaves the set alone otherwise,
// `keep` drops the whole target when the regex does not match.
//
// Deliberately NOT modelled: the engines' habit of deleting a label whose
// replacement expands to the empty string. The contract must never rely on
// that cleanup, so here an empty expansion is stored as an empty value and
// the assertions flag it.
func apply(labels map[string]string, r relabelRule) (kept bool) {
	re := regexp.MustCompile("^(?:" + r.regex() + ")$")
	vals := make([]string, 0, len(r.SourceLabels))
	for _, s := range r.SourceLabels {
		vals = append(vals, labels[s])
	}
	joined := strings.Join(vals, r.separator())
	switch r.action() {
	case "keep":
		return re.MatchString(joined)
	case "replace":
		idx := re.FindStringSubmatchIndex(joined)
		if idx == nil {
			return true
		}
		out := re.ExpandString(nil, r.replacement(), joined, idx)
		labels[r.TargetLabel] = string(out)
		return true
	default:
		panic("relabel action not modelled: " + r.action())
	}
}

func applyAll(labels map[string]string, rules []relabelRule) (kept bool) {
	for _, r := range rules {
		if !apply(labels, r) {
			return false
		}
	}
	return true
}

func loadVMAgent(t *testing.T) (vmagentDoc, []scrapeJob) {
	t.Helper()
	raw := readFile(t, vmagentPath)
	var doc vmagentDoc
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)
	if err := dec.Decode(&doc); err != nil {
		t.Fatalf("%s: %v", vmagentPath, err)
	}
	if doc.Kind != "VMAgent" || doc.APIVersion != "operator.victoriametrics.com/v1beta1" || doc.Metadata.Name != "kubeneuron" || doc.Metadata.Namespace != "kubeneuron-monitoring" {
		t.Fatalf("%s: not the VMAgent this contract is written for: %+v", vmagentPath, doc.Metadata)
	}
	if strings.TrimSpace(doc.Spec.InlineScrapeConfig) == "" {
		t.Fatalf("%s: spec.inlineScrapeConfig is empty", vmagentPath)
	}
	var jobs []scrapeJob
	jdec := yaml.NewDecoder(strings.NewReader(doc.Spec.InlineScrapeConfig))
	jdec.KnownFields(true)
	if err := jdec.Decode(&jobs); err != nil {
		t.Fatalf("%s: inlineScrapeConfig: %v", vmagentPath, err)
	}
	if len(jobs) == 0 {
		t.Fatalf("%s: inlineScrapeConfig has no jobs", vmagentPath)
	}
	return doc, jobs
}

func dcgmJob(t *testing.T, jobs []scrapeJob) scrapeJob {
	t.Helper()
	for _, j := range jobs {
		if j.JobName == "dcgm-exporter" {
			return j
		}
	}
	t.Fatal("no dcgm-exporter job in the inline scrape config")
	return scrapeJob{}
}

func findRule(rules []relabelRule, target string) (relabelRule, bool) {
	for _, r := range rules {
		if r.TargetLabel == target && r.action() == "replace" {
			return r, true
		}
	}
	return relabelRule{}, false
}

// Every job's rules use only valid Prometheus label names, on both sides,
// and only the actions the evaluator models (so the evaluation below is
// faithful).
func TestScrapeConfigLabelNamesValid(t *testing.T) {
	_, jobs := loadVMAgent(t)
	for _, j := range jobs {
		if len(j.Other) > 0 {
			t.Errorf("job %s: unexpected keys %v (extend the test's model before using them)", j.JobName, j.Other)
		}
		for _, rules := range [][]relabelRule{j.RelabelConfigs, j.MetricRelabelConfigs} {
			for _, r := range rules {
				if a := r.action(); a != "replace" && a != "keep" {
					t.Errorf("job %s: action %q is not modelled by this test", j.JobName, a)
				}
				if r.TargetLabel != "" && !promLabelNameRE.MatchString(r.TargetLabel) {
					t.Errorf("job %s: target_label %q is not a valid Prometheus label name", j.JobName, r.TargetLabel)
				}
				for _, s := range r.SourceLabels {
					if !promLabelNameRE.MatchString(s) {
						t.Errorf("job %s: source label %q is not a valid Prometheus label name", j.JobName, s)
					}
				}
				if r.Regex != nil {
					if _, err := regexp.Compile("^(?:" + *r.Regex + ")$"); err != nil {
						t.Errorf("job %s: regex %q: %v", j.JobName, *r.Regex, err)
					}
				}
				if r.action() == "replace" && r.TargetLabel == "" {
					t.Errorf("job %s: replace rule without target_label: %+v", j.JobName, r)
				}
			}
		}
	}
}

// The dcgm-exporter job discovers the exporter Service's endpoints in the
// GPU Operator namespace with node metadata attached, and keeps node from
// the pod's node name.
func TestDCGMJobDiscovery(t *testing.T) {
	_, jobs := loadVMAgent(t)
	j := dcgmJob(t, jobs)
	if len(j.KubernetesSDConfigs) != 1 {
		t.Fatalf("dcgm-exporter job: want one kubernetes_sd_config, got %d", len(j.KubernetesSDConfigs))
	}
	sd := j.KubernetesSDConfigs[0]
	if sd.Role != "endpoints" {
		t.Errorf("role = %q, want endpoints (attach_metadata.node is valid for pod, endpoints and endpointslice)", sd.Role)
	}
	if len(sd.Namespaces.Names) != 1 || sd.Namespaces.Names[0] != "gpu-operator" {
		t.Errorf("namespaces = %v, want [gpu-operator]", sd.Namespaces.Names)
	}
	if !sd.AttachMetadata["node"] {
		t.Errorf("attach_metadata.node must be true so node labels reach relabeling; got %v", sd.AttachMetadata)
	}
	for k := range sd.AttachMetadata {
		if k != "node" {
			t.Errorf("attach_metadata.%s is not part of the contract", k)
		}
	}
	// Keep filters are unchanged from v0.6.1: the exporter Service and its
	// metrics port, nothing else.
	keeps := 0
	for _, r := range j.RelabelConfigs {
		if r.action() == "keep" {
			keeps++
			switch strings.Join(r.SourceLabels, ",") {
			case "__meta_kubernetes_service_name":
				if r.regex() != "nvidia-dcgm-exporter" {
					t.Errorf("service keep regex = %q", r.regex())
				}
			case "__meta_kubernetes_endpoint_port_name":
				if r.regex() != "gpu-metrics" {
					t.Errorf("port keep regex = %q", r.regex())
				}
			default:
				t.Errorf("unexpected keep rule on %v", r.SourceLabels)
			}
		}
	}
	if keeps != 2 {
		t.Errorf("want the two v0.6.1 keep rules, got %d", keeps)
	}
	node, ok := findRule(j.RelabelConfigs, "node")
	if !ok || len(node.SourceLabels) != 1 || node.SourceLabels[0] != "__meta_kubernetes_pod_node_name" || node.Regex != nil || node.Replacement != nil {
		t.Errorf("node must be copied from __meta_kubernetes_pod_node_name: %+v", node)
	}
}

// node_pool: one replace rule per documented node label, in the documented
// precedence order, each matching only a non-empty value and copying it
// verbatim.
func TestNodePoolRulesShape(t *testing.T) {
	_, jobs := loadVMAgent(t)
	j := dcgmJob(t, jobs)
	var rules []relabelRule
	for _, r := range j.RelabelConfigs {
		if r.TargetLabel == "node_pool" {
			rules = append(rules, r)
		}
	}
	if len(rules) != len(nodePoolSources) {
		t.Fatalf("want %d node_pool rules (one per source label), got %d", len(nodePoolSources), len(rules))
	}
	for i, r := range rules {
		want := "__meta_kubernetes_node_label_" + sanitizeLabelName(nodePoolSources[i])
		if len(r.SourceLabels) != 1 || r.SourceLabels[0] != want {
			t.Errorf("node_pool rule %d: source %v, want [%s] (%s)", i, r.SourceLabels, want, nodePoolSources[i])
		}
		if r.regex() != "(.+)" {
			t.Errorf("node_pool rule %d: regex %q must be (.+) so an absent or empty label never produces an empty node_pool", i, r.regex())
		}
		if r.replacement() != "$1" {
			t.Errorf("node_pool rule %d: replacement %q must copy the label value verbatim", i, r.replacement())
		}
		if r.action() != "replace" {
			t.Errorf("node_pool rule %d: action %q", i, r.action())
		}
	}
	for _, r := range j.MetricRelabelConfigs {
		if r.TargetLabel == "node_pool" {
			t.Errorf("node_pool is a target label (relabel_configs), not a metric relabel: %+v", r)
		}
	}
}

// node_pool semantics, by evaluation: the label exists exactly when a
// source label with a non-empty value exists, never as an empty string, and
// the precedence is kubeneuron.io < karpenter < GKE < EKS.
func TestNodePoolFallbackEvaluation(t *testing.T) {
	_, jobs := loadVMAgent(t)
	j := dcgmJob(t, jobs)
	meta := func(key string) string { return "__meta_kubernetes_node_label_" + sanitizeLabelName(key) }
	base := map[string]string{
		"__meta_kubernetes_service_name":       "nvidia-dcgm-exporter",
		"__meta_kubernetes_endpoint_port_name": "gpu-metrics",
		"__meta_kubernetes_pod_node_name":      "ip-10-0-0-1",
	}
	cases := []struct {
		name   string
		labels map[string]string
		want   string // "" means node_pool must be absent
	}{
		{"no pool label", nil, ""},
		{"EKS only", map[string]string{meta("eks.amazonaws.com/nodegroup"): "gpu-ng"}, "gpu-ng"},
		{"GKE only", map[string]string{meta("cloud.google.com/gke-nodepool"): "gpu-pool"}, "gpu-pool"},
		{"Karpenter only", map[string]string{meta("karpenter.sh/nodepool"): "kp-gpu"}, "kp-gpu"},
		{"KubeNeuron fallback only", map[string]string{meta("kubeneuron.io/node-pool"): "manual"}, "manual"},
		{"EKS wins over all", map[string]string{
			meta("kubeneuron.io/node-pool"): "manual", meta("karpenter.sh/nodepool"): "kp-gpu",
			meta("cloud.google.com/gke-nodepool"): "gpu-pool", meta("eks.amazonaws.com/nodegroup"): "gpu-ng",
		}, "gpu-ng"},
		{"GKE wins over Karpenter and fallback", map[string]string{
			meta("kubeneuron.io/node-pool"): "manual", meta("karpenter.sh/nodepool"): "kp-gpu", meta("cloud.google.com/gke-nodepool"): "gpu-pool",
		}, "gpu-pool"},
		{"Karpenter wins over fallback", map[string]string{meta("kubeneuron.io/node-pool"): "manual", meta("karpenter.sh/nodepool"): "kp-gpu"}, "kp-gpu"},
		{"empty EKS value is not a pool", map[string]string{meta("eks.amazonaws.com/nodegroup"): ""}, ""},
		{"empty higher-precedence label does not erase a lower one", map[string]string{meta("kubeneuron.io/node-pool"): "manual", meta("eks.amazonaws.com/nodegroup"): ""}, "manual"},
		{"unrelated node label is ignored", map[string]string{meta("topology.kubernetes.io/zone"): "eu-west-1a"}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			labels := map[string]string{}
			for k, v := range base {
				labels[k] = v
			}
			for k, v := range tc.labels {
				labels[k] = v
			}
			if !applyAll(labels, j.RelabelConfigs) {
				t.Fatal("target dropped by a keep rule")
			}
			got, present := labels["node_pool"]
			switch {
			case tc.want == "" && present:
				t.Errorf("node_pool must be absent, got %q", got)
			case tc.want != "" && got != tc.want:
				t.Errorf("node_pool = %q (present=%v), want %q", got, present, tc.want)
			}
			if labels["node"] != "ip-10-0-0-1" {
				t.Errorf("node = %q", labels["node"])
			}
		})
	}
	// The harness distinguishes: a `(.*)` rule WOULD store an empty
	// node_pool, which is exactly what the contract's `(.+)` prevents.
	loose := map[string]string{}
	empty := "(.*)"
	apply(loose, relabelRule{SourceLabels: []string{meta("eks.amazonaws.com/nodegroup")}, Regex: &empty, TargetLabel: "node_pool"})
	if v, present := loose["node_pool"]; !present || v != "" {
		t.Fatal("evaluator must store an empty expansion so the (.+) guarantee is actually tested")
	}
	// And the keep rules really drop a non-exporter target.
	other := map[string]string{"__meta_kubernetes_service_name": "something-else", "__meta_kubernetes_endpoint_port_name": "gpu-metrics"}
	if applyAll(other, j.RelabelConfigs) {
		t.Fatal("keep rule did not drop a foreign service; the evaluator is not faithful")
	}
}

// The required identity mappings are metric relabels (the exporter stamps
// UUID and modelName on each series, not on the target): gpu_uuid and
// gpu_model copy them, keeping the originals; mig_profile is optional and
// copies GPU_I_PROFILE only when present. Nothing drops or rewrites the
// exporter's own labels.
func TestIdentityLabelMappings(t *testing.T) {
	_, jobs := loadVMAgent(t)
	j := dcgmJob(t, jobs)
	want := map[string]string{"gpu_uuid": "UUID", "gpu_model": "modelName", "mig_profile": "GPU_I_PROFILE"}
	for target, source := range want {
		r, ok := findRule(j.MetricRelabelConfigs, target)
		if !ok {
			t.Errorf("metric_relabel_configs: no rule producing %s", target)
			continue
		}
		if len(r.SourceLabels) != 1 || r.SourceLabels[0] != source {
			t.Errorf("%s: source %v, want [%s]", target, r.SourceLabels, source)
		}
		if r.regex() != "(.+)" || r.replacement() != "$1" {
			t.Errorf("%s: regex %q replacement %q; must copy a non-empty value verbatim", target, r.regex(), r.replacement())
		}
	}
	for _, r := range j.MetricRelabelConfigs {
		if _, known := want[r.TargetLabel]; !known {
			t.Errorf("metric relabel produces %q, which the contract does not define", r.TargetLabel)
		}
		if r.action() != "replace" {
			t.Errorf("metric relabel %+v: only replace is allowed; nothing may drop exporter labels", r)
		}
	}
	// Evaluation: a Tesla T4 series without MIG keeps its exporter labels and
	// gains gpu_uuid and gpu_model only; a MIG series also gains mig_profile;
	// a series without UUID (not an exporter series) gains nothing.
	series := map[string]string{"gpu": "0", "UUID": "GPU-abc", "modelName": "Tesla T4", "pci_bus_id": "00000000:00:1E.0", "device": "nvidia0", "Hostname": "ip-10-0-0-1", "pod": "", "namespace": "", "container": ""}
	applyAll(series, j.MetricRelabelConfigs)
	if series["gpu_uuid"] != "GPU-abc" || series["gpu_model"] != "Tesla T4" || series["UUID"] != "GPU-abc" || series["modelName"] != "Tesla T4" {
		t.Errorf("identity mapping: %v", series)
	}
	if _, present := series["mig_profile"]; present {
		t.Errorf("mig_profile must be absent without MIG: %v", series)
	}
	if series["pod"] != "" || series["namespace"] != "" {
		t.Errorf("workload labels must pass through unchanged: %v", series)
	}
	mig := map[string]string{"UUID": "GPU-abc", "modelName": "A100", "GPU_I_PROFILE": "1g.5gb", "GPU_I_ID": "3"}
	applyAll(mig, j.MetricRelabelConfigs)
	if mig["mig_profile"] != "1g.5gb" || mig["GPU_I_PROFILE"] != "1g.5gb" {
		t.Errorf("MIG mapping: %v", mig)
	}
	bare := map[string]string{"foo": "bar"}
	applyAll(bare, j.MetricRelabelConfigs)
	if len(bare) != 1 {
		t.Errorf("a series without exporter labels must gain nothing: %v", bare)
	}
}

// No invented defaults: cluster is not set anywhere (externalLabels absent,
// no relabel produces it) and driver is not produced by any rule. Both are
// documented decisions; faking either would make rules and dashboards
// silently wrong.
func TestNoInventedClusterOrDriverLabel(t *testing.T) {
	doc, jobs := loadVMAgent(t)
	if doc.Spec.ExternalLabels != nil {
		t.Errorf("spec.externalLabels must be absent by default (administrator-provided): %v", doc.Spec.ExternalLabels)
	}
	for _, j := range jobs {
		for _, rules := range [][]relabelRule{j.RelabelConfigs, j.MetricRelabelConfigs} {
			for _, r := range rules {
				switch r.TargetLabel {
				case "cluster", "driver":
					t.Errorf("job %s: a relabel rule produces %q, which the contract leaves to the administrator (cluster) or intentionally absent (driver): %+v", j.JobName, r.TargetLabel, r)
				}
				if r.action() == "replace" && r.Replacement != nil && !strings.Contains(*r.Replacement, "$") {
					t.Errorf("job %s: rule writes the constant %q into %s; the contract only copies values that exist", j.JobName, *r.Replacement, r.TargetLabel)
				}
			}
		}
	}
	raw := string(readFile(t, vmagentPath))
	if !strings.Contains(raw, "externalLabels") || !strings.Contains(raw, "cluster") {
		t.Error("vmagent.yaml must explain, in a comment, how an administrator sets cluster through externalLabels")
	}
}

// The resource keeps the operator-generated ServiceAccount and RBAC (no
// serviceAccountName override, not ingest-only), so the ClusterRole the
// VictoriaMetrics operator creates for it, which includes nodes
// get/list/watch, is the one attach_metadata relies on; and the image is
// the locked vmagent version.
func TestVMAgentKeepsOperatorRBACAndPinnedImage(t *testing.T) {
	doc, _ := loadVMAgent(t)
	lock := loadLock(t)
	if doc.Spec.ServiceAccountName != "" {
		t.Errorf("spec.serviceAccountName = %q: a custom ServiceAccount would not carry the operator-generated ClusterRole that grants nodes access", doc.Spec.ServiceAccountName)
	}
	if doc.Spec.IngestOnlyMode != nil && *doc.Spec.IngestOnlyMode {
		t.Error("spec.ingestOnlyMode: true makes the operator generate no scrape RBAC at all")
	}
	if doc.Spec.SelectAllByDefault == nil || *doc.Spec.SelectAllByDefault {
		t.Error("spec.selectAllByDefault must stay false: the inline config is the whole scrape contract")
	}
	if doc.Spec.Image.Tag != lock.Images.VMAgent.Tag || doc.Spec.Image.Repository != "victoriametrics/vmagent" {
		t.Errorf("image %s:%s, lock says vmagent %s", doc.Spec.Image.Repository, doc.Spec.Image.Tag, lock.Images.VMAgent.Tag)
	}
}
