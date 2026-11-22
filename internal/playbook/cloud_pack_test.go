// SPDX-License-Identifier: Apache-2.0
package playbook

import (
	"path/filepath"
	"testing"
)

func TestCloudPackCannotEscalateToHostActions(t *testing.T) {
	books, err := LoadDir(filepath.Join("..", "..", "configs", "cloud-playbooks"))
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateEscalationGraph(books); err != nil {
		t.Fatal(err)
	}
	if len(books) != 3 {
		t.Fatalf("cloud ladders=%d", len(books))
	}
	for name, book := range books {
		for _, step := range book.Steps {
			switch step.Action {
			case "platform.cordon", "platform.checkpoint_warning":
			case "platform.drain", "platform.recycle_node", "platform.replace_node":
				if !step.NeedsApproval() {
					t.Fatalf("%s/%s interrupts workloads without an authorization boundary", name, step.Name)
				}
			default:
				t.Fatalf("%s contains action outside cloud pack: %s", name, step.Action)
			}
		}
	}
	if books["cloud-recycle"].OnFailure.EscalateTo != "cloud-replace" {
		t.Fatal("restart must escalate only to cloud replacement")
	}
}
