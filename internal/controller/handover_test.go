// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"errors"
	"github.com/kubeneuron/kubeneuron/internal/cloud/removefirst"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kubeneuron/kubeneuron/pkg/types"
)

func TestTransitionRejectsUntypedHandover(t *testing.T) {
	inc := &types.Incident{State: types.StateExecuting}
	err := (&Controller{}).transition(context.Background(), inc, types.StateNeedsHuman, "system", "example", "reason", nil)
	if err == nil || inc.State != types.StateExecuting {
		t.Fatalf("expected rejection before state change, got state %v, error %v", inc.State, err)
	}
}

func testOperatorHandover(id string) handover {
	return handover{What: "The repair stopped.", Check: "Check the node. Then run", Command: "kubeneuronctl resolve " + id}
}

func TestRestoreHandoverMessages(t *testing.T) {
	const check = "Check the original machine and the size of the pool; if it is one node short, set it back to 3 nodes. Then run"
	const command = `kubeneuronctl resolve example --cloud-operation-settled --reason "<what you found>"`
	for _, tc := range []struct {
		name        string
		cause       error
		what, check string
	}{
		{"autoscaled", removefirst.ErrAutoscaled, "KubeNeuron did not set the size of pool train back because the pool is now autoscaled.", "Check the size of the pool; the autoscaler may already have replaced the node. Then run"},
		{"unequal", removefirst.ErrUnequalZones, "KubeNeuron did not set the size of pool train back because its zones now have different sizes, so it cannot tell which zone is one node short.", check},
		{"changed", removefirst.ErrChangedZones, "KubeNeuron did not set the size of pool train back because the zones of the pool changed after the replacement started.", check},
		{"unknown", nil, "KubeNeuron could not safely restore the size of pool train.", check},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := restoreHandover(errors.Join(tc.cause, errors.New("provider private diagnostic")), "train", "3 nodes", "example")
			want := tc.what + " " + tc.check + " " + command
			if got := h.message(); got != want {
				t.Fatalf("got %q, want %q", got, want)
			}
		})
	}
}

func TestHandoverValidation(t *testing.T) {
	const id = "incident-42"
	const plain = "kubeneuronctl resolve incident-42"
	for _, tc := range []struct {
		name, what, check, command string
		valid                      bool
	}{
		{"plain", "Node unavailable.", "Check the node, then run", plain, true},
		{"settlement", "Node unavailable.", "Check the node, then run", settlementCommand(id), true},
		{"missing cause", "", "Check the node", plain, false},
		{"missing check", "Node unavailable.", "  ", plain, false},
		{"missing command", "Node unavailable.", "Check the node", "", false},
		{"wrong incident", "Node unavailable.", "Check the node", plain + "0", false},
		{"period after id", "Node unavailable.", "Check the node", plain + ".", false},
		{"period after argument", "Node unavailable.", "Check the node", settlementCommand(id) + ".", false},
		{"text after command", "Node unavailable.", "Check the node", plain + " then wait", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := handover{What: tc.what, Check: tc.check, Command: tc.command}
			if err := h.validate(id); (err == nil) != tc.valid {
				t.Fatalf("valid=%v, error=%v", tc.valid, err)
			}
		})
	}
}

func TestDirectNeedsHumanTransition(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	settlement, handovers := 0, 0
	for _, path := range files {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				method, ok := call.Fun.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				var targetIndex int
				switch method.Sel.Name {
				case "Transition":
					targetIndex = 1
				case "transition", "transitionWithRepairStep":
					targetIndex = 2
				default:
					return true
				}
				if len(call.Args) <= targetIndex {
					return true
				}
				target, ok := call.Args[targetIndex].(*ast.SelectorExpr)
				if !ok || target.Sel.Name != "StateNeedsHuman" {
					return true
				}
				switch {
				case method.Sel.Name == "Transition" && path == "cloud_settlement.go" && fn.Name.Name == "ResolveSettledCloudOperation":
					settlement++
				case method.Sel.Name == "transitionWithRepairStep" && path == "handover.go" && fn.Name.Name == "handOver":
					handovers++
				default:
					t.Errorf("%s: transition to NEEDS_HUMAN bypasses handOver", fset.Position(call.Pos()))
				}
				return true
			})
		}
	}
	if settlement != 1 || handovers != 1 {
		t.Fatalf("expected one NEEDS_HUMAN transition each in settlement and handOver, got %d and %d", settlement, handovers)
	}
}
