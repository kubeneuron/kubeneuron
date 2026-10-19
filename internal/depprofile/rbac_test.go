package depprofile

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// attach_metadata.node makes vmagent list and watch nodes. This profile does
// not ship its own RBAC for that: the VictoriaMetrics operator generates a
// ClusterRole for every VMAgent it reconciles in cluster-wide mode, and that
// role already carries nodes get/list/watch. The fact was verified in the
// pinned operator's source, not assumed, and the README records the version
// it was verified against. These tests keep the three places (lock, README,
// kustomization) from drifting apart: bumping the operator pin without
// re-verifying the RBAC claim fails here.

var rbacVerifiedRE = regexp.MustCompile(`verified in the VictoriaMetrics operator (v[0-9]+\.[0-9]+\.[0-9]+) source`)

func TestOperatorRBACClaimMatchesLock(t *testing.T) {
	lock := loadLock(t)
	if lock.Images.VMOperator.Tag != lock.Artifacts.VMOperatorManifest.Version {
		t.Fatalf("lock: operator image %s and manifest %s disagree", lock.Images.VMOperator.Tag, lock.Artifacts.VMOperatorManifest.Version)
	}
	readme := squash(string(readFile(t, dependenciesDoc)))
	m := rbacVerifiedRE.FindStringSubmatch(readme)
	if m == nil {
		t.Fatalf("%s must state the operator version the vmagent nodes RBAC was verified in (pattern %s)", dependenciesDoc, rbacVerifiedRE)
	}
	if m[1] != lock.Images.VMOperator.Tag {
		t.Errorf("%s says the nodes RBAC was verified in operator %s but the lock pins %s; re-verify the generated ClusterRole and update the statement", dependenciesDoc, m[1], lock.Images.VMOperator.Tag)
	}
	for _, want := range []string{
		"monitoring:kubeneuron-monitoring:vmagent-kubeneuron",
		"`get`, `list`, `watch`",
		"`WATCH_NAMESPACE`",
		"No write verb",
	} {
		if !strings.Contains(readme, want) {
			t.Errorf("%s must document %q for the nodes RBAC", dependenciesDoc, want)
		}
	}
}

// The observability kustomization renders exactly the shipped resources,
// and none of them is an RBAC object: the only RBAC the contract relies on
// is operator-generated and documented. Should a ClusterRole ever be added
// here, it may grant nothing beyond read-only nodes access.
func TestObservabilityKustomizationResources(t *testing.T) {
	root := repoRoot(t)
	var kust struct {
		Namespace string   `yaml:"namespace"`
		Resources []string `yaml:"resources"`
	}
	if err := yaml.Unmarshal(readFile(t, kustomizationPath), &kust); err != nil {
		t.Fatal(err)
	}
	if kust.Namespace != "kubeneuron-monitoring" {
		t.Errorf("namespace = %q", kust.Namespace)
	}
	kinds := map[string]int{}
	for _, res := range kust.Resources {
		path := filepath.Join(root, observabilityDir, res)
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Errorf("kustomization lists %s: %v", res, err)
			continue
		}
		for _, doc := range yamlDocs(t, raw, res) {
			kind := str(doc["kind"])
			if kind == "" || str(dig(doc, "metadata", "name")) == "" {
				t.Errorf("%s: document without kind/metadata.name", res)
			}
			kinds[kind]++
			checkRBACObject(t, res, doc)
		}
	}
	for _, want := range []string{"Namespace", "VMAgent", "VMSingle", "VMAlert", "VMAlertmanager", "VMRule"} {
		if kinds[want] == 0 {
			t.Errorf("kustomization renders no %s", want)
		}
	}
	if kinds["VMAgent"] != 1 {
		t.Errorf("want exactly one VMAgent, got %d", kinds["VMAgent"])
	}
	for _, rbac := range []string{"ClusterRole", "ClusterRoleBinding", "Role", "RoleBinding"} {
		if kinds[rbac] != 0 {
			t.Errorf("observability ships a %s; the nodes access is operator-generated (see README). If this is deliberate, update the README statement and keep it read-only", rbac)
		}
	}
}

// checkRBACObject enforces the ceiling any RBAC object in this directory
// must stay under: core nodes only, verbs get/list/watch only.
func checkRBACObject(t *testing.T, where string, doc map[string]any) {
	t.Helper()
	kind := str(doc["kind"])
	if kind != "ClusterRole" && kind != "Role" {
		return
	}
	rules, _ := doc["rules"].([]any)
	for _, r := range rules {
		rule, _ := r.(map[string]any)
		for _, v := range toStrings(rule["verbs"]) {
			switch v {
			case "get", "list", "watch":
			default:
				t.Errorf("%s: %s grants verb %q; only get/list/watch are allowed", where, kind, v)
			}
		}
		for _, res := range toStrings(rule["resources"]) {
			if res != "nodes" {
				t.Errorf("%s: %s grants resource %q; only nodes is allowed", where, kind, res)
			}
		}
		for _, g := range toStrings(rule["apiGroups"]) {
			if g != "" {
				t.Errorf("%s: %s grants apiGroup %q; only the core group is allowed", where, kind, g)
			}
		}
	}
}

func toStrings(v any) []string {
	list, _ := v.([]any)
	out := make([]string, 0, len(list))
	for _, x := range list {
		out = append(out, str(x))
	}
	return out
}

// kustomize build must still succeed with the contract in place. Uses
// `kustomize build` or `kubectl kustomize`, whichever is installed; skips
// (loudly) when neither is, since the structural test above already
// validates every resource file.
func TestObservabilityKustomizeBuild(t *testing.T) {
	root := repoRoot(t)
	dir := filepath.Join(root, observabilityDir)
	var cmd *exec.Cmd
	switch {
	case lookPath("kustomize"):
		cmd = exec.Command("kustomize", "build", dir)
	case lookPath("kubectl"):
		cmd = exec.Command("kubectl", "kustomize", dir)
	default:
		t.Skip("neither kustomize nor kubectl is installed; kustomize build not exercised here (structural checks still ran)")
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("%v: %v\n%s", cmd.Args, err, stderr.String())
	}
	docs := yamlDocs(t, stdout.Bytes(), "kustomize build")
	var agents int
	for _, d := range docs {
		if str(d["kind"]) != "VMAgent" {
			continue
		}
		agents++
		inline := str(dig(d, "spec", "inlineScrapeConfig"))
		if !strings.Contains(inline, "attach_metadata") || !strings.Contains(inline, "target_label: node_pool") || !strings.Contains(inline, "target_label: gpu_uuid") {
			t.Error("rendered VMAgent lost the scrape label contract")
		}
		if dig(d, "spec", "externalLabels") != nil {
			t.Error("rendered VMAgent carries externalLabels; cluster must stay administrator-provided")
		}
		if str(dig(d, "metadata", "namespace")) != "kubeneuron-monitoring" {
			t.Errorf("rendered VMAgent namespace = %q", str(dig(d, "metadata", "namespace")))
		}
	}
	if agents != 1 {
		t.Errorf("rendered %d VMAgent objects, want 1", agents)
	}
}

func lookPath(name string) bool {
	_, err := exec.LookPath(name)
	return err == nil
}
