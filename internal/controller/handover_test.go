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

// Lower this ceiling whenever a legacy message gains structured instructions.
func TestLegacyHandoverCeiling(t *testing.T) {
	const ceiling = 0
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	var calls []string
	for _, path := range files {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			ident, ok := call.Fun.(*ast.Ident)
			if ok && ident.Name == "legacyHandover" {
				calls = append(calls, fset.Position(call.Pos()).String())
			}
			return true
		})
	}
	if len(calls) != ceiling {
		t.Fatalf("found %d legacy handovers, ceiling %d; migrate new calls or lower the ceiling after migration:\n%s", len(calls), ceiling, strings.Join(calls, "\n"))
	}
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
	allowed := 0
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
				if !ok || len(call.Args) != 2 {
					return true
				}
				target, ok := call.Args[1].(*ast.SelectorExpr)
				if !ok || target.Sel.Name != "StateNeedsHuman" {
					return true
				}
				method, ok := call.Fun.(*ast.SelectorExpr)
				if !ok || method.Sel.Name != "Transition" {
					return true
				}
				if path == "cloud_settlement.go" && fn.Name.Name == "ResolveSettledCloudOperation" && allowed == 0 {
					allowed++
				} else {
					t.Errorf("%s: direct transition to NEEDS_HUMAN outside the single settlement exception", fset.Position(call.Pos()))
				}
				return true
			})
		}
	}
	if allowed != 1 {
		t.Fatal("expected one internal NEEDS_HUMAN transition in ResolveSettledCloudOperation")
	}
}
