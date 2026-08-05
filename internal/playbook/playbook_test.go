package playbook

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/kubeneuron/kubeneuron/internal/config"
	"github.com/kubeneuron/kubeneuron/pkg/types"
)

// TestLoadShippedPlaybooks validates every playbook shipped in
// configs/playbooks, including escalation references.
func TestLoadShippedPlaybooks(t *testing.T) {
	books, err := LoadDir(filepath.Join("..", "..", "configs", "playbooks"))
	if err != nil {
		t.Fatalf("loading shipped playbooks: %v", err)
	}
	for _, want := range []string{
		"observe-suspect", "workload-restart", "gpu-reset", "reset-when-idle",
		"drain-and-reset", "fell-off-bus", "reboot", "driver-reinstall", "rma",
	} {
		if _, ok := books[want]; !ok {
			t.Errorf("expected shipped playbook %q", want)
		}
	}

	// Risky rungs must require approval on their dangerous steps.
	for book, step := range map[string]string{
		"reboot": "reboot", "fell-off-bus": "reboot",
		"driver-reinstall": "reinstall", "rma": "open-ticket",
	} {
		b, ok := books[book]
		if !ok {
			continue
		}
		found := false
		for _, s := range b.Steps {
			if s.Name == step {
				found = true
				if !s.NeedsApproval() {
					t.Errorf("playbook %q step %q must require approval", book, step)
				}
			}
		}
		if !found {
			t.Errorf("playbook %q is missing step %q", book, step)
		}
	}
}

// TestShippedPoliciesResolve loads configs/policies.yaml and verifies every
// policy references an existing playbook.
func TestShippedPoliciesResolve(t *testing.T) {
	cfg, err := config.Load(filepath.Join("..", "..", "configs", "policies.yaml"))
	if err != nil {
		t.Fatalf("loading shipped config: %v", err)
	}
	books, err := LoadDir(filepath.Join("..", "..", "configs", "playbooks"))
	if err != nil {
		t.Fatalf("loading shipped playbooks: %v", err)
	}
	policies := make([]Policy, 0, len(cfg.Policies))
	for _, p := range cfg.Policies {
		policies = append(policies, Policy{Class: p.Match.Class, Playbook: p.Playbook, Params: p.Params})
	}
	if _, err := NewEngine(books, policies); err != nil {
		t.Fatalf("shipped policies do not resolve: %v", err)
	}
	if !cfg.Safety.DryRun {
		t.Error("shipped config must default to dry_run: true")
	}
}

func TestPlaybookValidationRejectsUnknownAction(t *testing.T) {
	p := &Playbook{
		Name:   "unknown-action",
		Target: "node",
		Steps:  []Step{{Name: "surprise", Action: "agent.shell"}},
	}
	if err := p.Validate(); err == nil || !strings.Contains(err.Error(), "unsupported action") {
		t.Fatalf("Validate() error = %v, want unsupported action", err)
	}
}

func TestStateMachineTransitions(t *testing.T) {
	legal := [][2]types.IncidentState{
		{types.StateOpen, types.StateEvaluating},
		{types.StateOpen, types.StateObserving},
		{types.StateObserving, types.StateEvaluating},
		{types.StateEvaluating, types.StateExecuting},
		{types.StateEvaluating, types.StateAwaitingApproval},
		{types.StateAwaitingApproval, types.StateExecuting},
		{types.StateAwaitingApproval, types.StateExpired},
		{types.StateExecuting, types.StateVerifying},
		{types.StateVerifying, types.StateResolved},
		{types.StateVerifying, types.StateEvaluating},
		{types.StateNeedsHuman, types.StateResolved},
	}
	for _, tr := range legal {
		if !CanTransition(tr[0], tr[1]) {
			t.Errorf("%s -> %s must be legal", tr[0], tr[1])
		}
	}

	illegal := [][2]types.IncidentState{
		{types.StateResolved, types.StateOpen},
		{types.StateExpired, types.StateExecuting},
		{types.StateOpen, types.StateExecuting},        // must evaluate first
		{types.StateObserving, types.StateExecuting},   // must evaluate first
		{types.StateExecuting, types.StateResolved},    // must verify first
		{types.StateOpen, types.StateAwaitingApproval}, // approval only from evaluating
	}
	for _, tr := range illegal {
		if CanTransition(tr[0], tr[1]) {
			t.Errorf("%s -> %s must be illegal", tr[0], tr[1])
		}
	}

	inc := &types.Incident{ID: "t-1", State: types.StateOpen}
	if err := Transition(inc, types.StateEvaluating); err != nil {
		t.Fatalf("legal transition failed: %v", err)
	}
	if err := Transition(inc, types.StateResolved); err != nil {
		t.Fatalf("evaluating -> resolved must be legal: %v", err)
	}
	if err := Transition(inc, types.StateOpen); err == nil {
		t.Fatal("terminal states must reject transitions")
	}
}

func engineFixture(t *testing.T) *Engine {
	t.Helper()
	books := map[string]*Playbook{
		"gpu-recover": {
			Name:      "gpu-recover",
			Steps:     []Step{{Name: "cordon", Action: "cordon"}, {Name: "reset", Action: "gpu_reset"}},
			OnFailure: OnFailure{EscalateTo: "node-reboot"},
		},
		"node-reboot": {
			Name:  "node-reboot",
			Steps: []Step{{Name: "reboot", Action: "reboot"}},
		},
	}
	engine, err := NewEngine(books, []Policy{
		{Class: types.ClassECCDBE, Playbook: "gpu-recover"},
		{Class: types.ClassECCDBE, Playbook: "node-reboot"}, // shadowed: first match wins
		{Class: types.ClassNVLink, Playbook: "node-reboot"},
	})
	if err != nil {
		t.Fatal(err)
	}
	return engine
}

func TestNewEngineRejectsDanglingPolicy(t *testing.T) {
	_, err := NewEngine(map[string]*Playbook{}, []Policy{{Class: types.ClassECCDBE, Playbook: "missing"}})
	if err == nil {
		t.Fatal("a policy referencing an unknown playbook must fail engine construction")
	}
}

func TestSelectAndPolicyForUseFirstMatch(t *testing.T) {
	engine := engineFixture(t)
	book, ok := engine.Select(types.Signal{Class: types.ClassECCDBE})
	if !ok || book.Name != "gpu-recover" {
		t.Fatalf("Select = %v, %v; want first-match gpu-recover", book, ok)
	}
	if _, ok := engine.Select(types.Signal{Class: types.ClassXIDApp}); ok {
		t.Fatal("unbound class must select nothing (observe-only)")
	}
	policy, ok := engine.PolicyFor(types.ClassECCDBE)
	if !ok || policy.Playbook != "gpu-recover" {
		t.Fatalf("PolicyFor = %+v, %v", policy, ok)
	}
	if _, ok := engine.PolicyFor(types.ClassXIDApp); ok {
		t.Fatal("unbound class must have no policy")
	}
}

func TestNextStepWalksAndTerminates(t *testing.T) {
	engine := engineFixture(t)
	inc := &types.Incident{ID: "inc-1", Playbook: "gpu-recover"}

	step, done, err := engine.NextStep(inc)
	if err != nil || done || step.Name != "cordon" {
		t.Fatalf("step 0 = %v %v %v", step, done, err)
	}
	inc.StepIndex = 1
	step, done, err = engine.NextStep(inc)
	if err != nil || done || step.Name != "reset" {
		t.Fatalf("step 1 = %v %v %v", step, done, err)
	}
	inc.StepIndex = 2
	if _, done, err = engine.NextStep(inc); err != nil || !done {
		t.Fatalf("exhausted playbook = done %v, err %v; want done", done, err)
	}
	if _, _, err := engine.NextStep(&types.Incident{ID: "inc-2", Playbook: "missing"}); err == nil {
		t.Fatal("unknown playbook must be an error, not a silent completion")
	}
}

func TestEscalationFollowsFailurePolicy(t *testing.T) {
	engine := engineFixture(t)
	next, ok := engine.Escalation("gpu-recover")
	if !ok || next.Name != "node-reboot" {
		t.Fatalf("Escalation = %v, %v", next, ok)
	}
	if _, ok := engine.Escalation("node-reboot"); ok {
		t.Fatal("playbook without escalateTo must not escalate")
	}
	if _, ok := engine.Escalation("missing"); ok {
		t.Fatal("unknown playbook must not escalate")
	}
	if book, ok := engine.Playbook("node-reboot"); !ok || book.Name != "node-reboot" {
		t.Fatalf("Playbook lookup = %v, %v", book, ok)
	}
}

// --- Fix 6: escalation cycles and self-references are rejected ---

func TestValidateEscalationGraphRejectsCyclesAndSelfReferences(t *testing.T) {
	step := []Step{{Name: "s", Action: "agent.reboot"}}
	book := func(name, escalateTo string) *Playbook {
		return &Playbook{Name: name, Target: "gpu", Steps: step, OnFailure: OnFailure{EscalateTo: escalateTo}}
	}

	// A self-reference is the smallest cycle.
	self := map[string]*Playbook{"a": book("a", "a")}
	if err := ValidateEscalationGraph(self); err == nil || !strings.Contains(err.Error(), "itself") {
		t.Fatalf("self-reference must be rejected, got %v", err)
	}

	// A two-hop cycle A->B->A compiles clean on a per-playbook existence check.
	cycle := map[string]*Playbook{"a": book("a", "b"), "b": book("b", "a")}
	if err := ValidateEscalationGraph(cycle); err == nil || !strings.Contains(err.Error(), "cycle") {
		t.Fatalf("A->B->A cycle must be rejected, got %v", err)
	}

	// An unknown target still fails (existence is part of the same check).
	missing := map[string]*Playbook{"a": book("a", "ghost")}
	if err := ValidateEscalationGraph(missing); err == nil || !strings.Contains(err.Error(), "unknown") {
		t.Fatalf("unknown escalation target must be rejected, got %v", err)
	}

	// A finite acyclic ladder A->B->C is accepted.
	acyclic := map[string]*Playbook{"a": book("a", "b"), "b": book("b", "c"), "c": book("c", "")}
	if err := ValidateEscalationGraph(acyclic); err != nil {
		t.Fatalf("acyclic ladder must be accepted, got %v", err)
	}
}
