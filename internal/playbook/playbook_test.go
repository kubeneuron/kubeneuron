package playbook

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

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
	policy, ok := engine.PolicyFor(types.ClassECCDBE, "")
	if !ok || policy.Playbook != "gpu-recover" {
		t.Fatalf("PolicyFor = %+v, %v", policy, ok)
	}
	if _, ok := engine.PolicyFor(types.ClassXIDApp, ""); ok {
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

// TestForceParamRules pins the three rules params.force lives by. It exists at
// all because the parameter was added mid-round and each rule closes a way the
// first version was quietly wrong.
func TestForceParamRules(t *testing.T) {
	book := func(action, force, approval string) *Playbook {
		return &Playbook{
			Name: "p", Target: "node",
			Steps: []Step{{
				Name: "s", Action: action, Approval: approval,
				Params: map[string]string{"force": force},
			}},
		}
	}

	// 1. Unparseable values are rejected at load. They used to read as false,
	// which is the safe direction but a silent one: the author of `force: yes`
	// found out when the ladder escalated past a refused drain at 3am.
	if err := book("platform.drain", "yes", "required").Validate(); err == nil {
		t.Fatal("force: yes was accepted; it reads as false at execution time and the author " +
			"is never told")
	}

	// 2. force on an action that does not read it is rejected, rather than
	// validated and then silently dropped.
	if err := book("platform.cordon", "true", "required").Validate(); err == nil {
		t.Fatal("force was accepted on platform.cordon, which ignores it; a load-time boolean " +
			"check implies the key does something there")
	}

	// 3. A forced drain requires approval. It destroys pods nothing will
	// recreate, and no other gate in the system can see the difference between
	// this and an ordinary drain.
	if err := book("platform.drain", "true", "none").Validate(); err == nil {
		t.Fatal("a forced drain was accepted without approval; it ends a tenant's work outright " +
			"while every blast-radius gate still sees an ordinary Drain")
	}

	// And the well-formed spelling loads.
	if err := book("platform.drain", "true", "required").Validate(); err != nil {
		t.Fatalf("a correctly declared forced drain was rejected: %v", err)
	}
	// As does an ordinary drain with no force at all.
	if err := (&Playbook{Name: "p", Target: "node",
		Steps: []Step{{Name: "s", Action: "platform.drain"}}}).Validate(); err != nil {
		t.Fatalf("an ordinary drain was rejected: %v", err)
	}
}

// TestNegativeDurationsAreRejected: a negative cooldown is not a slow cooldown,
// it is none at all — every "has enough time passed" comparison is already
// true, so the playbook that just failed on this GPU re-runs on the next tick
// and keeps re-running. A negative step timeout is the same shape: the deadline
// is already past when it is computed, so the step is cancelled before doing
// anything and the ladder escalates to a more destructive rung on a playbook
// that never ran.
func TestNegativeDurationsAreRejected(t *testing.T) {
	step := Step{Name: "s", Action: "platform.drain"}

	p := &Playbook{Name: "p", Target: "node", Cooldown: Duration(-time.Hour), Steps: []Step{step}}
	if err := p.Validate(); err == nil {
		t.Fatal("a negative cooldown was accepted; it suppresses nothing, so a reset loop can " +
			"cycle a card on every tick")
	}

	step.Timeout = Duration(-time.Minute)
	p = &Playbook{Name: "p", Target: "node", Steps: []Step{step}}
	if err := p.Validate(); err == nil {
		t.Fatal("a negative step timeout was accepted; the step is cancelled before it acts and " +
			"the ladder escalates past it")
	}
}

// TestAVendorScopedPolicyDoesNotClaimAnotherVendorsSignal is the first half of
// making multi-vendor remediation expressible at all.
//
// A problem class is not vendor-specific — an uncorrectable ECC error happens on
// NVIDIA, AMD and Intel alike — but the ladder that answers it is. Selection
// matched on class alone, so an operator adding AMD nodes could not give them
// their own ladder: their faults selected the NVIDIA one, whose reset is then
// refused at the capability gate. Refused AFTER the cordon and the drain, which
// is the expensive part — the tenant's work is already gone by the time
// anything notices the reset could never run.
func TestAVendorScopedPolicyDoesNotClaimAnotherVendorsSignal(t *testing.T) {
	book := func(name string) *Playbook {
		return &Playbook{Name: name, Target: "gpu", Steps: []Step{{Name: "s", Action: "platform.cordon"}}}
	}
	e, err := NewEngine(
		map[string]*Playbook{"nvidia-ladder": book("nvidia-ladder"), "amd-ladder": book("amd-ladder")},
		[]Policy{
			{Class: "ecc-dbe", Vendor: types.AcceleratorVendorNVIDIA, Playbook: "nvidia-ladder"},
			{Class: "ecc-dbe", Vendor: types.AcceleratorVendorAMD, Playbook: "amd-ladder"},
		},
	)
	if err != nil {
		t.Fatal(err)
	}

	sig := func(vendor string) types.Signal {
		s := types.Signal{Class: "ecc-dbe", Target: types.Target{Node: "n1", GPUUUID: "GPU-a"}}
		if vendor != "" {
			s.Evidence = map[string]string{"vendor": vendor}
		}
		return s
	}

	got, ok := e.Select(sig("nvidia"))
	if !ok || got.Name != "nvidia-ladder" {
		t.Fatalf("an NVIDIA fault selected %v (ok=%v), want nvidia-ladder", got, ok)
	}
	got, ok = e.Select(sig("amd"))
	if !ok || got.Name != "amd-ladder" {
		t.Fatalf("an AMD fault selected %v (ok=%v), want amd-ladder: the AMD card would get the "+
			"NVIDIA ladder, refused at the capability gate only after the node has been "+
			"cordoned and drained", got, ok)
	}

	// A signal naming NO vendor matches neither scoped policy. These ladders
	// reset and reboot hardware; acting on an unconfirmed guess is the wrong
	// direction to fail in.
	if got, ok := e.Select(sig("")); ok {
		t.Fatalf("a signal naming no vendor selected %q; a vendor's ladder ran against hardware "+
			"nobody confirmed was that vendor's", got.Name)
	}
}

// A generic policy is a fallback, not a way for file order to disable a
// vendor-specific safety ladder. This was especially easy to hit with CR
// priorities: a broad policy at priority 1 shadowed an AMD policy at priority
// 2, and the node reached cordon/drain before its NVIDIA-only action failed.
func TestVendorSpecificPolicyBeatsAnEarlierGenericFallback(t *testing.T) {
	book := func(name string) *Playbook {
		return &Playbook{Name: name, Target: "gpu", Steps: []Step{{Name: "s", Action: "platform.cordon"}}}
	}
	e, err := NewEngine(
		map[string]*Playbook{"generic": book("generic"), "amd": book("amd")},
		[]Policy{
			{Class: "ecc-dbe", Playbook: "generic"},
			{Class: "ecc-dbe", Vendor: types.AcceleratorVendorAMD, Playbook: "amd"},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := e.SelectFor("ecc-dbe", types.AcceleratorVendorAMD); !ok || got.Name != "amd" {
		t.Fatalf("AMD selected %v (ok=%v), want the vendor-specific policy", got, ok)
	}
	if got, ok := e.SelectFor("ecc-dbe", types.AcceleratorVendorNVIDIA); !ok || got.Name != "generic" {
		t.Fatalf("NVIDIA selected %v (ok=%v), want the generic fallback", got, ok)
	}
}

// TestAnUnscopedPolicyStillMatchesEverything: every policy written before the
// vendor field must behave exactly as it did, including for signals that name
// no vendor at all. Otherwise this change silently stops existing fleets from
// remediating.
func TestAnUnscopedPolicyStillMatchesEverything(t *testing.T) {
	e, err := NewEngine(
		map[string]*Playbook{"any-ladder": {Name: "any-ladder", Target: "gpu",
			Steps: []Step{{Name: "s", Action: "platform.cordon"}}}},
		[]Policy{{Class: "ecc-dbe", Playbook: "any-ladder"}},
	)
	if err != nil {
		t.Fatal(err)
	}
	for _, vendor := range []string{"nvidia", "amd", ""} {
		sig := types.Signal{Class: "ecc-dbe", Target: types.Target{Node: "n1"}}
		if vendor != "" {
			sig.Evidence = map[string]string{"vendor": vendor}
		}
		if got, ok := e.Select(sig); !ok || got.Name != "any-ladder" {
			t.Fatalf("vendor %q no longer selects an unscoped policy", vendor)
		}
	}
}

// TestEveryPolicyLookupHonoursTheVendor covers the seam a vendor-aware Select
// left behind: two other paths ask the same question and asked it without the
// vendor.
//
// The late bind (an incident whose playbook an engine reload unbound) built a
// signal from the class alone, so it could never bind to a vendor-scoped
// policy — the incident quietly resolved with no ladder. And the observation
// threshold lookup took the first policy of that class whatever its vendor, so
// an AMD incident escalated on NVIDIA's timing: a decision nobody made, about
// hardware nobody looked at.
func TestEveryPolicyLookupHonoursTheVendor(t *testing.T) {
	book := func(name string) *Playbook {
		return &Playbook{Name: name, Target: "gpu", Steps: []Step{{Name: "s", Action: "platform.cordon"}}}
	}
	e, err := NewEngine(
		map[string]*Playbook{"nvidia-ladder": book("nvidia-ladder"), "amd-ladder": book("amd-ladder")},
		[]Policy{
			{Class: "ecc-dbe", Vendor: types.AcceleratorVendorNVIDIA, Playbook: "nvidia-ladder",
				Params: map[string]string{"threshold": "3"}},
			{Class: "ecc-dbe", Vendor: types.AcceleratorVendorAMD, Playbook: "amd-ladder",
				Params: map[string]string{"threshold": "7"}},
		},
	)
	if err != nil {
		t.Fatal(err)
	}

	// The late bind reaches the right ladder for each vendor.
	got, ok := e.SelectFor("ecc-dbe", types.AcceleratorVendorAMD)
	if !ok || got.Name != "amd-ladder" {
		t.Fatalf("late bind for AMD selected %v (ok=%v), want amd-ladder: an incident that raced "+
			"a policy rollout would resolve with no ladder at all", got, ok)
	}

	// And the observation threshold is read from that vendor's own policy.
	pol, ok := e.PolicyFor("ecc-dbe", types.AcceleratorVendorAMD)
	if !ok || pol.Params["threshold"] != "7" {
		t.Fatalf("AMD read threshold %q from policy %q, want 7 from the AMD policy: escalating "+
			"on another vendor's timing is a decision nobody made",
			pol.Params["threshold"], pol.Playbook)
	}

	// A vendor nobody scoped for still finds nothing rather than the first one.
	if got, ok := e.SelectFor("ecc-dbe", types.AcceleratorVendorIntel); ok {
		t.Fatalf("an Intel incident bound %q", got.Name)
	}
}
