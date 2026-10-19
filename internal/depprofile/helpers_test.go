package depprofile

import (
	"bytes"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

const (
	dependenciesDir   = "deploy/kubernetes/dependencies"
	observabilityDir  = dependenciesDir + "/observability"
	vmagentPath       = observabilityDir + "/vmagent.yaml"
	kustomizationPath = observabilityDir + "/kustomization.yaml"
	versionsLockPath  = dependenciesDir + "/versions.lock.yaml"
	dependenciesDoc   = dependenciesDir + "/README.md"
	operationsDoc     = "docs/operations.md"
)

// promLabelNameRE is the Prometheus label-name grammar; every label the
// contract produces, and every meta label it reads, must match it.
var promLabelNameRE = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)

func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("go.mod not found")
		}
		dir = parent
	}
}

func readFile(t *testing.T, rel string) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(repoRoot(t), rel))
	if err != nil {
		t.Fatalf("%s: %v", rel, err)
	}
	return raw
}

// yamlDocs splits a multi-document YAML stream and decodes each document
// into a generic map, skipping empty documents.
func yamlDocs(t *testing.T, raw []byte, where string) []map[string]any {
	t.Helper()
	var out []map[string]any
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	for {
		var doc map[string]any
		err := dec.Decode(&doc)
		if err != nil {
			if strings.Contains(err.Error(), "EOF") {
				break
			}
			t.Fatalf("%s: %v", where, err)
		}
		if doc != nil {
			out = append(out, doc)
		}
	}
	return out
}

// dig walks nested maps by key and returns the value, or nil.
func dig(m map[string]any, path ...string) any {
	var cur any = m
	for _, p := range path {
		mm, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		cur, ok = mm[p]
		if !ok {
			return nil
		}
	}
	return cur
}

func str(v any) string {
	s, _ := v.(string)
	return s
}

// versionsLock is the subset of versions.lock.yaml the tests pin against.
type versionsLock struct {
	Artifacts struct {
		GPUOperatorChart struct {
			Version string `yaml:"version"`
			SHA256  string `yaml:"sha256"`
		} `yaml:"nvidiaGpuOperatorChart"`
		VMOperatorManifest struct {
			Version string `yaml:"version"`
		} `yaml:"victoriaMetricsOperatorManifest"`
	} `yaml:"artifacts"`
	Images struct {
		VMOperator struct {
			Tag string `yaml:"tag"`
		} `yaml:"victoriaMetricsOperator"`
		VMAgent struct {
			Tag string `yaml:"tag"`
		} `yaml:"victoriaMetricsAgent"`
		DCGMExporter struct {
			Tag string `yaml:"tag"`
		} `yaml:"nvidiaDcgmExporter"`
	} `yaml:"images"`
}

func loadLock(t *testing.T) versionsLock {
	t.Helper()
	var lock versionsLock
	if err := yaml.Unmarshal(readFile(t, versionsLockPath), &lock); err != nil {
		t.Fatalf("%s: %v", versionsLockPath, err)
	}
	if lock.Artifacts.GPUOperatorChart.Version == "" || lock.Images.VMAgent.Tag == "" || lock.Images.VMOperator.Tag == "" {
		t.Fatalf("%s: lock is missing the pins these tests need: %+v", versionsLockPath, lock)
	}
	return lock
}

// fencedBlocks returns the bodies of every fenced code block in a Markdown
// document, in order, with the info string (language) alongside.
func fencedBlocks(md string) []fencedBlock {
	var out []fencedBlock
	lines := strings.Split(md, "\n")
	for i := 0; i < len(lines); i++ {
		trimmed := strings.TrimSpace(lines[i])
		if !strings.HasPrefix(trimmed, "```") {
			continue
		}
		info := strings.TrimSpace(strings.TrimPrefix(trimmed, "```"))
		indent := lines[i][:len(lines[i])-len(strings.TrimLeft(lines[i], " "))]
		var body []string
		j := i + 1
		for ; j < len(lines); j++ {
			if strings.TrimSpace(lines[j]) == "```" {
				break
			}
			body = append(body, strings.TrimPrefix(lines[j], indent))
		}
		out = append(out, fencedBlock{Info: info, Body: strings.Join(body, "\n") + "\n", Line: i + 1})
		i = j
	}
	return out
}

type fencedBlock struct {
	Info string
	Body string
	Line int
}

// squash folds every run of whitespace into one space so a phrase can be
// asserted across Markdown line wraps.
func squash(text string) string {
	return strings.Join(strings.Fields(text), " ")
}

// section returns the body of the Markdown section whose heading line
// starts with the given prefix, up to the next heading of the same level.
func section(t *testing.T, md, headingPrefix string) string {
	t.Helper()
	lines := strings.Split(md, "\n")
	start := -1
	for i, l := range lines {
		if strings.HasPrefix(l, headingPrefix) {
			start = i
			break
		}
	}
	if start < 0 {
		t.Fatalf("no section starting with %q", headingPrefix)
	}
	level := strings.SplitN(headingPrefix, " ", 2)[0] + " "
	end := len(lines)
	for i := start + 1; i < len(lines); i++ {
		if strings.HasPrefix(lines[i], level) {
			end = i
			break
		}
	}
	return strings.Join(lines[start:end], "\n")
}

// sanitizeLabelName is how Kubernetes service discovery spells a label key
// as a __meta_* suffix: every character outside [a-zA-Z0-9_] becomes '_'.
func sanitizeLabelName(key string) string {
	var b strings.Builder
	for _, r := range key {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	return b.String()
}
