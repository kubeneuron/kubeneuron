// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"

	sigyaml "sigs.k8s.io/yaml"
)

// celReservedWords are the CEL keywords that Kubernetes requires as
// __keyword__ when they name a schema property. Kubernetes 1.29 to 1.31
// refuse to compile a rule that reads such a property unescaped, so the CRD
// would not install there.
var celReservedWords = map[string]bool{
	"true": true, "false": true, "null": true, "in": true, "as": true,
	"break": true, "const": true, "continue": true, "else": true, "for": true,
	"function": true, "if": true, "import": true, "let": true, "loop": true,
	"package": true, "namespace": true, "return": true, "var": true,
	"void": true, "while": true,
}

var (
	celStringLiteral = regexp.MustCompile(`'(?:[^'\\]|\\.)*'|"(?:[^"\\]|\\.)*"`)
	celFieldAccess   = regexp.MustCompile(`\.([A-Za-z_][A-Za-z0-9_]*)`)
)

func TestGeneratedCRDRulesEscapeReservedPropertyNames(t *testing.T) {
	files, err := filepath.Glob("../../config/crd/bases/*.yaml")
	if err != nil || len(files) == 0 {
		t.Fatalf("no generated CRDs found: %v", err)
	}
	rules := 0
	for _, file := range files {
		data, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		var crd map[string]any
		if err := sigyaml.Unmarshal(data, &crd); err != nil {
			t.Fatalf("%s: %v", file, err)
		}
		walkCELRules(crd, func(rule string) {
			rules++
			code := celStringLiteral.ReplaceAllString(rule, "''")
			for _, m := range celFieldAccess.FindAllStringSubmatch(code, -1) {
				if celReservedWords[m[1]] {
					t.Errorf("%s: rule reads property %q unescaped; write __%s__: %s", filepath.Base(file), m[1], m[1], rule)
				}
			}
		})
	}
	if rules == 0 {
		t.Fatal("found no x-kubernetes-validations rules; the walk is broken")
	}
}

func walkCELRules(node any, visit func(string)) {
	switch v := node.(type) {
	case map[string]any:
		if list, ok := v["x-kubernetes-validations"].([]any); ok {
			for _, entry := range list {
				if m, ok := entry.(map[string]any); ok {
					if rule, ok := m["rule"].(string); ok {
						visit(rule)
					}
				}
			}
		}
		for _, child := range v {
			walkCELRules(child, visit)
		}
	case []any:
		for _, child := range v {
			walkCELRules(child, visit)
		}
	}
}
