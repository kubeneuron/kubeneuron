// SPDX-License-Identifier: Apache-2.0

package operator

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/kubeneuron/kubeneuron/internal/action"
	"github.com/kubeneuron/kubeneuron/internal/playbook"
)

// packPrefix is how the CRD pack names the ladders the file set ships under
// the bare name.
const packPrefix = "kubeneuron-"

// keepsRunningWork lists the registry actions that are destructive for
// confinement but end no running work: cordon only stops new work from
// landing, and quiescing the accelerator stack degrades monitoring, not a
// workload. Every other Destructive or AgentDestructive action ends running
// work or takes the machine away, so a new one lands under the rule below
// unless it is listed here on purpose.
var keepsRunningWork = map[string]string{
	"platform.cordon":                    "stops new work from landing; running work stays",
	"platform.quiesce_accelerator_stack": "stands down monitoring and the device plugin, not workloads",
}

func endsRunningWork(wire string) bool {
	def, ok := action.ByWire(wire)
	if !ok || (!def.Destructive && !def.AgentDestructive) {
		return false
	}
	_, keeps := keepsRunningWork[wire]
	return !keeps
}

// checkLadderApprovals applies the one rule both shipped sets follow: a human
// decides at the first step that ends running work, and at every step whose
// action the registry marks ForcesApproval.
func ladderApprovalProblems(plane string, book *playbook.Playbook) []string {
	var problems []string
	firstSeen := false
	for _, step := range book.Steps {
		required := step.Approval == "required"
		if def, ok := action.ByWire(step.Action); ok && def.ForcesApproval && !required {
			problems = append(problems, fmt.Sprintf("%s playbook %q step %q: %s restarts or destroys the node and does not require approval",
				plane, book.Name, step.Name, step.Action))
		}
		if endsRunningWork(step.Action) && !firstSeen {
			firstSeen = true
			if !required {
				problems = append(problems, fmt.Sprintf("%s playbook %q step %q: %s is the first step that ends running work and does not require approval",
					plane, book.Name, step.Name, step.Action))
			}
		}
	}
	return problems
}

func approvalSteps(book *playbook.Playbook) []string {
	var out []string
	for _, step := range book.Steps {
		if step.Approval == "required" {
			out = append(out, step.Name)
		}
	}
	sort.Strings(out)
	return out
}

// TestShippedPlaybookSetsAgreeOnApproval pins configs/playbooks (the file
// plane) to config/policies (the CRD plane). They are the same ladders under
// the same names; an installation must not lose a human gate because it reads
// its playbooks off disk instead of from the cluster.
func TestShippedPlaybookSetsAgreeOnApproval(t *testing.T) {
	files, err := playbook.LoadDir(filepath.Join("..", "..", "configs", "playbooks"))
	if err != nil {
		t.Fatalf("loading the file playbooks: %v", err)
	}
	crds, _ := loadPolicyPack(t)
	pack := map[string]*playbook.Playbook{}
	for i := range crds {
		compiled, err := compilePlaybook(&crds[i])
		if err != nil {
			t.Fatalf("compiling pack playbook %q: %v", crds[i].Name, err)
		}
		pack[strings.TrimPrefix(crds[i].Name, packPrefix)] = compiled
	}
	for _, book := range files {
		for _, problem := range ladderApprovalProblems("file", book) {
			t.Error(problem)
		}
	}
	for _, book := range pack {
		for _, problem := range ladderApprovalProblems("CRD", book) {
			t.Error(problem)
		}
	}
	shared := 0
	for name, crd := range pack {
		file, ok := files[name]
		if !ok {
			continue
		}
		shared++
		if got, want := approvalSteps(file), approvalSteps(crd); strings.Join(got, ",") != strings.Join(want, ",") {
			t.Errorf("playbook %q: file set asks approval at %v, CRD pack at %v", name, got, want)
		}
	}
	if shared < 6 {
		t.Fatalf("only %d ladders are shared between the two sets; the name mapping is broken", shared)
	}
}

// The rule must catch the regression it exists for.
func TestLadderApprovalRuleCatchesUngatedSteps(t *testing.T) {
	cases := map[string][]playbook.Step{
		"drain without approval": {
			{Name: "cordon", Action: "platform.cordon"},
			{Name: "drain", Action: "platform.drain"},
		},
		"reboot approved by an earlier step only": {
			{Name: "drain", Action: "platform.drain", Approval: "required"},
			{Name: "reboot", Action: "agent.reboot"},
		},
		"replace without approval": {
			{Name: "replace", Action: "platform.replace_node"},
		},
	}
	for name, steps := range cases {
		if len(ladderApprovalProblems("test", &playbook.Playbook{Name: name, Steps: steps})) == 0 {
			t.Errorf("%s: the rule accepted it", name)
		}
	}
}
