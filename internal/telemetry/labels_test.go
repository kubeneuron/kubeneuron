// SPDX-License-Identifier: Apache-2.0

package telemetry

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"

	"github.com/kubeneuron/kubeneuron/internal/metrics"
)

// Metric labels leave the process: over OTLP to the Collector and on to
// whatever store the user configures. None may carry a person's identity, a
// credential, an endpoint URL, or the names of the user's workloads
// (namespaces, pods, training jobs often embed people's or customers' names).
var (
	forbiddenLabels = map[string]bool{
		"namespace": true, "pod": true, "pod_name": true, "container": true,
		"job": true, "job_name": true, "workload": true, "owner_name": true,
	}
	forbiddenFragments = []string{
		"actor", "user", "email", "token", "secret", "password", "webhook", "url",
	}
)

func forbidden(label string) bool {
	l := strings.ToLower(label)
	if forbiddenLabels[l] {
		return true
	}
	for _, f := range forbiddenFragments {
		if strings.Contains(l, f) {
			return true
		}
	}
	return false
}

func TestForbiddenLabelRule(t *testing.T) {
	for _, l := range []string{"actor", "user_name", "webhook", "api_token", "endpoint_url", "Email", "namespace", "pod", "job"} {
		if !forbidden(l) {
			t.Errorf("%q should be forbidden", l)
		}
	}
	for _, l := range []string{"class", "outcome", "owner", "source", "adapter", "state", "reason", "digest"} {
		if forbidden(l) {
			t.Errorf("%q should be allowed", l)
		}
	}
}

// Every label name declared anywhere in our code: the []string label lists
// passed to New*Vec and NewDesc, and the keys of ConstLabels and
// prometheus.Labels literals. A vector with no observations yet is absent
// from Gather, so the source is the complete inventory.
func TestNoDeclaredMetricLabelCarriesPersonalData(t *testing.T) {
	root := filepath.Join("..", "..")
	fset := token.NewFileSet()
	declared := 0
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "node_modules", "vendor", "testdata", "bin":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return err
		}
		ast.Inspect(file, func(n ast.Node) bool {
			for _, label := range declaredLabels(n) {
				declared++
				if forbidden(label) {
					t.Errorf("%s: metric label %q may carry personal data, a credential or a workload name", fset.Position(n.Pos()), label)
				}
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if declared < 20 {
		t.Fatalf("found only %d declared labels; the scan no longer sees the metric definitions", declared)
	}
}

func declaredLabels(n ast.Node) []string {
	switch n := n.(type) {
	case *ast.CallExpr:
		name := ""
		switch fn := n.Fun.(type) {
		case *ast.SelectorExpr:
			name = fn.Sel.Name
		case *ast.Ident:
			name = fn.Name
		}
		if name != "NewDesc" && (!strings.HasPrefix(name, "New") || !strings.HasSuffix(name, "Vec")) {
			return nil
		}
		var out []string
		for _, arg := range n.Args {
			out = append(out, stringSlice(arg)...)
		}
		return out
	case *ast.KeyValueExpr:
		if key, ok := n.Key.(*ast.Ident); ok && key.Name == "ConstLabels" {
			return mapKeys(n.Value)
		}
	case *ast.CompositeLit:
		if sel, ok := n.Type.(*ast.SelectorExpr); ok && sel.Sel.Name == "Labels" {
			return mapKeys(n)
		}
	}
	return nil
}

func stringSlice(e ast.Expr) []string {
	lit, ok := e.(*ast.CompositeLit)
	if !ok {
		return nil
	}
	arr, ok := lit.Type.(*ast.ArrayType)
	if !ok {
		return nil
	}
	if id, ok := arr.Elt.(*ast.Ident); !ok || id.Name != "string" {
		return nil
	}
	var out []string
	for _, el := range lit.Elts {
		if s, ok := el.(*ast.BasicLit); ok && s.Kind == token.STRING {
			v, _ := strconv.Unquote(s.Value)
			out = append(out, v)
		}
	}
	return out
}

func mapKeys(e ast.Expr) []string {
	lit, ok := e.(*ast.CompositeLit)
	if !ok {
		return nil
	}
	var out []string
	for _, el := range lit.Elts {
		kv, ok := el.(*ast.KeyValueExpr)
		if !ok {
			continue
		}
		if s, ok := kv.Key.(*ast.BasicLit); ok && s.Kind == token.STRING {
			v, _ := strconv.Unquote(s.Value)
			out = append(out, v)
		}
	}
	return out
}

// The registries we export also hold collectors we did not write (Go
// runtime, process, controller-runtime). Check what they actually expose.
func TestNoGatheredMetricLabelCarriesPersonalData(t *testing.T) {
	_ = metrics.Handler() // the controller and agent registry
	for name, g := range map[string]prometheus.Gatherer{
		"default":            prometheus.DefaultGatherer,
		"controller-runtime": ctrlmetrics.Registry,
	} {
		mfs, err := g.Gather()
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		for _, mf := range mfs {
			for _, m := range mf.GetMetric() {
				for _, lp := range m.GetLabel() {
					if forbidden(lp.GetName()) {
						t.Errorf("%s registry: %s has label %q", name, mf.GetName(), lp.GetName())
					}
				}
			}
		}
	}
}
