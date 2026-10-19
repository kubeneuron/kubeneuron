package depprofile

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"flag"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/kubeneuron/kubeneuron/internal/gpucatalog"
)

// gpuOperatorChart lets a caller point the rendering test at a downloaded
// copy of the pinned GPU Operator chart:
//
//	go test ./internal/depprofile -args -gpu-operator-chart=/path/gpu-operator-v26.3.3.tgz
//
// Otherwise KUBENEURON_GPU_OPERATOR_CHART or the Helm repository cache
// ($HOME/.cache/helm/repository/gpu-operator-<version>.tgz) is tried. The
// archive is checked against the lock's SHA-256 before use, so a wrong or
// tampered chart fails rather than renders.
var gpuOperatorChart = flag.String("gpu-operator-chart", "", "path to the pinned gpu-operator chart archive for the helm template test")

const valuesPath = gpucatalog.ShippedProfilePath

// The values file is the reliability profile's only home. Its structure is
// what the chart reads: dcgmExporter.config.{name,create,data} creates the
// ConfigMap ClusterPolicy points at. The rows are checked against the
// catalog here as well as in internal/gpucatalog, from the consumer's side.
func TestValuesProfileStructure(t *testing.T) {
	root := repoRoot(t)
	raw := readFile(t, valuesPath)
	var values map[string]any
	if err := yaml.Unmarshal(raw, &values); err != nil {
		t.Fatal(err)
	}
	lock := loadLock(t)
	if v, _ := dig(values, "dcgmExporter", "enabled").(bool); !v {
		t.Error("dcgmExporter.enabled must be true")
	}
	if v := str(dig(values, "dcgmExporter", "version")); v != lock.Images.DCGMExporter.Tag {
		t.Errorf("dcgmExporter.version = %q, lock says %q", v, lock.Images.DCGMExporter.Tag)
	}
	if v, _ := dig(values, "dcgmExporter", "serviceMonitor", "enabled").(bool); v {
		t.Error("dcgmExporter.serviceMonitor.enabled must stay false: the VMAgent inline config is the scrape path")
	}
	if v, _ := dig(values, "gfd", "enabled").(bool); !v {
		t.Error("gfd.enabled must be true (GPU Feature Discovery is the documented, unverified candidate source of a future driver label)")
	}
	rows, err := gpucatalog.ShippedProfile(root)
	if err != nil {
		t.Fatal(err)
	}
	c := gpucatalog.Default()
	eligible := map[string]gpucatalog.Field{}
	for _, f := range c.ProfileFields() {
		eligible[f.Series] = f
	}
	seen := map[string]bool{}
	for _, r := range rows {
		f, ok := eligible[r.Series]
		if !ok {
			t.Errorf("values list %s, which is not an eligible catalog field", r.Series)
			continue
		}
		if seen[r.Series] {
			t.Errorf("values list %s twice", r.Series)
		}
		seen[r.Series] = true
		if r.Kind != f.Kind {
			t.Errorf("%s: kind %s, catalog %s", r.Series, r.Kind, f.Kind)
		}
		if len(r.Help) > 120 || strings.Contains(r.Help, ",") || strings.Contains(r.Help, "\"") {
			t.Errorf("%s: help must be short and free of commas and quotes for the exporter's CSV parser: %q", r.Series, r.Help)
		}
	}
	for s, f := range eligible {
		if !seen[s] {
			t.Errorf("eligible field %d (%s) is missing from the values", f.DCGMFieldID, s)
		}
	}
	// The data block says, in comments, where the Tesla T4 has no such
	// capability and which fields are deliberately absent.
	data, err := gpucatalog.ShippedProfileCSV(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"has no NVLink", "has no row remapping", "1011/1012", "T2"} {
		if !strings.Contains(data, want) {
			t.Errorf("profile data block must carry the hardware/absence comment %q", want)
		}
	}
	for _, want := range []string{"DCGM_FI_DEV_XID_ERRORS (230", "DCGM_FI_DEV_GPU_RECOVERY_ACTION (1523", "DCGM_FI_DEV_PSTATE", "has not been\n    # observed on any GPU by this project"} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("values must document the deliberate absence / unvalidated status: %q", want)
		}
	}
}

// Rendering the pinned chart with the shipped values produces the ConfigMap
// the exporter reads, with the full profile, and a ClusterPolicy that points
// at it. Requires helm and the pinned chart archive; otherwise skips with
// the reason, because the structural test above cannot see what the chart
// does with the values.
func TestHelmTemplateRendersProfile(t *testing.T) {
	root := repoRoot(t)
	lock := loadLock(t)
	if !lookPath("helm") {
		t.Skip("helm is not installed; chart rendering not exercised (values structure was validated)")
	}
	chart := locateChart(t, lock)
	if chart == "" {
		t.Skipf("pinned gpu-operator chart %s not available locally (set -gpu-operator-chart or KUBENEURON_GPU_OPERATOR_CHART, or `helm pull gpu-operator --repo https://helm.ngc.nvidia.com/nvidia --version %s`); rendering not exercised", lock.Artifacts.GPUOperatorChart.Version, lock.Artifacts.GPUOperatorChart.Version)
	}
	sum := sha256File(t, chart)
	if sum != lock.Artifacts.GPUOperatorChart.SHA256 {
		t.Fatalf("%s: sha256 %s does not match the lock (%s); refusing to render an unverified chart", chart, sum, lock.Artifacts.GPUOperatorChart.SHA256)
	}
	cmd := exec.Command("helm", "template", "gpu-operator", chart, "--namespace", "gpu-operator", "--values", filepath.Join(root, valuesPath))
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("helm template: %v\n%s", err, stderr.String())
	}
	docs := yamlDocs(t, stdout.Bytes(), "helm template")
	var csvText string
	var policies, configMaps int
	for _, d := range docs {
		switch str(d["kind"]) {
		case "ConfigMap":
			if str(dig(d, "metadata", "name")) != gpucatalog.ShippedProfileConfigMapName {
				continue
			}
			configMaps++
			if ns := str(dig(d, "metadata", "namespace")); ns != "gpu-operator" {
				t.Errorf("ConfigMap namespace = %q", ns)
			}
			csvText = str(dig(d, "data", "dcgm-metrics.csv"))
		case "ClusterPolicy":
			policies++
			if name := str(dig(d, "spec", "dcgmExporter", "config", "name")); name != gpucatalog.ShippedProfileConfigMapName {
				t.Errorf("ClusterPolicy dcgmExporter.config.name = %q, want %s", name, gpucatalog.ShippedProfileConfigMapName)
			}
			if v := str(dig(d, "spec", "dcgmExporter", "version")); v != lock.Images.DCGMExporter.Tag {
				t.Errorf("ClusterPolicy dcgmExporter.version = %q, lock %q", v, lock.Images.DCGMExporter.Tag)
			}
			if en, _ := dig(d, "spec", "dcgmExporter", "enabled").(bool); !en {
				t.Error("ClusterPolicy dcgmExporter.enabled is false")
			}
		}
	}
	if configMaps != 1 || policies != 1 {
		t.Fatalf("rendered %d %s ConfigMaps and %d ClusterPolicies, want one each", configMaps, gpucatalog.ShippedProfileConfigMapName, policies)
	}
	rows, err := gpucatalog.ParseProfileCSV(csvText)
	if err != nil {
		t.Fatalf("rendered dcgm-metrics.csv: %v", err)
	}
	rendered := map[string]gpucatalog.Kind{}
	for _, r := range rows {
		rendered[r.Series] = r.Kind
	}
	c := gpucatalog.Default()
	for _, f := range c.ProfileFields() {
		if k, ok := rendered[f.Series]; !ok {
			t.Errorf("rendered ConfigMap lacks %s", f.Series)
		} else if k != f.Kind {
			t.Errorf("rendered ConfigMap types %s as %s, catalog %s", f.Series, k, f.Kind)
		}
	}
	if len(rows) != len(c.ProfileFields()) {
		t.Errorf("rendered %d rows, catalog eligible %d", len(rows), len(c.ProfileFields()))
	}
	// The rendered CSV is byte-identical to the values data: the chart does
	// not rewrite it.
	want, err := gpucatalog.ShippedProfileCSV(root)
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(csvText) != strings.TrimSpace(want) {
		t.Error("rendered dcgm-metrics.csv differs from dcgmExporter.config.data")
	}
}

func locateChart(t *testing.T, lock versionsLock) string {
	t.Helper()
	name := "gpu-operator-" + lock.Artifacts.GPUOperatorChart.Version + ".tgz"
	candidates := []string{*gpuOperatorChart, os.Getenv("KUBENEURON_GPU_OPERATOR_CHART")}
	if home, err := os.UserHomeDir(); err == nil {
		candidates = append(candidates, filepath.Join(home, ".cache", "helm", "repository", name))
	}
	for _, c := range candidates {
		if c == "" {
			continue
		}
		if _, err := os.Stat(c); err == nil {
			return c
		}
	}
	return ""
}

func sha256File(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}
