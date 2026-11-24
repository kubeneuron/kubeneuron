// SPDX-License-Identifier: Apache-2.0

package kindgroups

import (
	"os"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestGroupCoverage(t *testing.T) {
	makefile, err := os.ReadFile("../../Makefile")
	if err != nil {
		t.Fatal(err)
	}
	var want []string
	for line := range strings.Lines(string(makefile)) {
		if value, ok := strings.CutPrefix(line, "INTEGRATION_LEAF_SUITES :="); ok {
			want = strings.Fields(value)
		}
	}
	if len(want) == 0 {
		t.Fatal("missing INTEGRATION_LEAF_SUITES")
	}
	workflow, err := os.ReadFile("../../.github/workflows/ci.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var ci struct {
		Jobs map[string]struct {
			Strategy struct {
				Matrix struct {
					Suite   []string
					Include []struct {
						Suite, Target string
						NodeImage     string `yaml:"node_image"`
					}
				}
			}
		}
	}
	if err := yaml.Unmarshal(workflow, &ci); err != nil {
		t.Fatal(err)
	}
	var installer []string
	for _, row := range ci.Jobs["kind-other"].Strategy.Matrix.Include {
		// A row on another Kubernetes version repeats a suite; it is not
		// part of the coverage on the default version.
		if row.NodeImage != "" {
			continue
		}
		if row.Target == "" || row.Target == "test-integration-kind" {
			installer = append(installer, row.Suite)
		}
	}
	slices.Sort(want)
	matches := func(groups []string) bool {
		got := append(slices.Clone(groups), installer...)
		slices.Sort(got)
		return slices.Equal(got, want)
	}
	for _, arch := range []string{"amd64", "arm64"} {
		groups := ci.Jobs["kind-"+arch+"-groups"].Strategy.Matrix.Suite
		if !matches(groups) {
			t.Errorf("%s groups %v with installer %v do not cover %v exactly once", arch, groups, installer, want)
		}
	}
	t.Run("missing TLS", func(t *testing.T) {
		if matches([]string{"functional"}) {
			t.Fatal("incomplete matrix accepted")
		}
	})
}
