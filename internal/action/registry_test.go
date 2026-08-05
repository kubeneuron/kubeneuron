package action

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	kubeneuronv1alpha1 "github.com/kubeneuron/kubeneuron/api/v1alpha1"
	"github.com/kubeneuron/kubeneuron/pkg/types"
)

// crdPlaybookActionEnum parses the closed PlaybookAction enum out of the
// generated GPUPlaybook CRD, which is the API source of truth the registry must
// stay consistent with. Reading the generated schema (rather than a hand-copied
// list) makes this test fail the moment the CRD enum and the registry diverge.
func crdPlaybookActionEnum(t *testing.T) map[string]bool {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "config", "crd", "bases", "kubeneuron.io_gpuplaybooks.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	schema := string(data)
	// The action property's enum immediately follows the PlaybookAction
	// description and ends at its "type: string".
	start := strings.Index(schema, "PlaybookAction is deliberately closed")
	if start < 0 {
		t.Fatal("could not locate PlaybookAction enum in generated CRD")
	}
	block := schema[start:]
	if end := strings.Index(block, "type: string"); end >= 0 {
		block = block[:end]
	}
	values := map[string]bool{}
	re := regexp.MustCompile(`(?m)^\s+-\s+(\w+)\s*$`)
	for _, m := range re.FindAllStringSubmatch(block, -1) {
		values[m[1]] = true
	}
	if len(values) == 0 {
		t.Fatal("parsed no PlaybookAction enum values from the CRD")
	}
	return values
}

// TestRegistryMatchesCRDEnum proves the registry and the closed CRD enum are a
// bijection: every enum value has exactly one registry entry, and every
// enum-mapped registry entry corresponds to an enum value.
func TestRegistryMatchesCRDEnum(t *testing.T) {
	enum := crdPlaybookActionEnum(t)

	seen := map[kubeneuronv1alpha1.PlaybookAction]int{}
	for _, d := range definitions {
		if d.PlaybookAction == "" {
			continue
		}
		seen[d.PlaybookAction]++
		if !enum[string(d.PlaybookAction)] {
			t.Errorf("registry has action %q that is not in the CRD enum", d.PlaybookAction)
		}
	}
	for value := range enum {
		if seen[kubeneuronv1alpha1.PlaybookAction(value)] != 1 {
			t.Errorf("CRD enum value %q maps to %d registry entries, want exactly 1", value, seen[kubeneuronv1alpha1.PlaybookAction(value)])
		}
	}
}

// TestWireIsKindDotOp guarantees dispatch can rely on Wire == "<Kind>.<Op>",
// which is what lets gateAction/executeStep derive the executor split from the
// registry instead of re-parsing the string.
func TestWireIsKindDotOp(t *testing.T) {
	seenWire := map[string]bool{}
	for _, d := range definitions {
		if want := string(d.Kind) + "." + d.Op; d.Wire != want {
			t.Errorf("action %q: Wire = %q, want %q", d.Wire, d.Wire, want)
		}
		if seenWire[d.Wire] {
			t.Errorf("duplicate wire action %q", d.Wire)
		}
		seenWire[d.Wire] = true
		switch d.Kind {
		case KindPlatform, KindAgent, KindVerify, KindNotify:
		default:
			t.Errorf("action %q: unknown kind %q", d.Wire, d.Kind)
		}
	}
}

// TestCapabilityGateOnlyGPUReset locks in that the collapsed special-case still
// fires for exactly one action: agent.gpu_reset carries the NVIDIA reset gate
// and nothing else does.
func TestCapabilityGateOnlyGPUReset(t *testing.T) {
	for _, d := range definitions {
		gate := d.CapabilityGate
		if d.Wire == "agent.gpu_reset" {
			if gate != CapabilityNVIDIAReset {
				t.Errorf("agent.gpu_reset CapabilityGate = %q, want %q", gate, CapabilityNVIDIAReset)
			}
			if d.Vendor != types.AcceleratorVendorNVIDIA {
				t.Errorf("agent.gpu_reset Vendor = %q, want nvidia", d.Vendor)
			}
			continue
		}
		if gate != CapabilityNone {
			t.Errorf("action %q CapabilityGate = %q, want none (gate must not spread beyond gpu_reset)", d.Wire, gate)
		}
	}
}

// TestGateActionAndRebootClass pins the safety-gate class each action consumes
// and which classes count against the reboot cap, so the safety gate stays
// behavior-identical to the old hand-maintained gateAction + isRebootClass.
func TestGateActionAndRebootClass(t *testing.T) {
	wantGate := map[string]types.ActionType{
		"agent.reboot":           types.ActionReboot,
		"agent.gpu_reset":        types.ActionGPUReset,
		"agent.driver_reinstall": types.ActionDriverReinstall,
		"platform.recycle_node":  types.ActionType("platform.recycle_node"),
		"platform.cordon":        types.ActionType("platform.cordon"),
		"verify.gpu_health":      types.ActionType("verify.gpu_health"),
		"notify.observe":         types.ActionType("notify.observe"),
	}
	for wire, want := range wantGate {
		d, ok := ByWire(wire)
		if !ok {
			t.Fatalf("registry missing %q", wire)
		}
		if got := d.GateAction(); got != want {
			t.Errorf("%q GateAction = %q, want %q", wire, got, want)
		}
	}

	// Only reboot, driver_reinstall (and the actuator-only power_cycle) are
	// reboot-class. recycle_node restarts a whole VM but is deliberately not
	// counted against the reboot cap — preserved, not "fixed".
	reboot := map[string]bool{"agent.reboot": true, "agent.driver_reinstall": true}
	for _, d := range definitions {
		want := reboot[d.Wire]
		if got := IsRebootClass(d.GateAction()); got != want {
			t.Errorf("%q IsRebootClass = %v, want %v", d.Wire, got, want)
		}
	}
	if !IsRebootClass(types.ActionPowerCycle) {
		t.Error("power_cycle must remain reboot-class for direct actuator use")
	}
}

// TestDestructiveActionSet pins the exact set of actions the controller's
// destructive-scope confinement check consumes via Definition.Destructive.
// Exactly these platform ops (and only these) are Destructive, and every
// Destructive action is a controller-side platform action.
// quiesce_accelerator_stack joined the set in round 5: it stands down DCGM and
// the device plugin on the node, and an action that can switch off a node's
// monitoring must not escape the declared blast radius.
func TestDestructiveActionSet(t *testing.T) {
	want := map[string]bool{
		"platform.cordon":                    true,
		"platform.drain":                     true,
		"platform.evict_gpu_workload":        true,
		"platform.quiesce_accelerator_stack": true,
		"platform.recycle_node":              true,
		"platform.replace_node":              true,
	}
	for _, d := range definitions {
		if got := d.Destructive; got != want[d.Wire] {
			t.Errorf("%q Destructive = %v, want %v", d.Wire, got, want[d.Wire])
		}
		if d.Destructive && d.Kind != KindPlatform {
			t.Errorf("%q is Destructive but Kind = %q, want platform (confinement is a controller-side platform check)", d.Wire, d.Kind)
		}
	}
}

// TestForcesApproval pins the whole-node actions the compiler forces approval
// on.
func TestForcesApproval(t *testing.T) {
	forced := map[string]bool{
		"agent.reboot":          true,
		"platform.recycle_node": true,
		"platform.replace_node": true,
	}
	for _, d := range definitions {
		if got := d.ForcesApproval; got != forced[d.Wire] {
			t.Errorf("%q ForcesApproval = %v, want %v", d.Wire, got, forced[d.Wire])
		}
	}
}

// Round-7 item C pins the exact agent-destructive set: every action whose
// execution path reaches the agent executor's destructive boundary
// (allowDestructiveAction) — and only those — so admission can refuse a step
// that a definitively unarmed agent will provably reject. quiesce is in the
// set (its host half stands monitoring DOWN and needs an armed agent);
// restore is NOT: undoing is always allowed — its host half executes even on
// a disarmed node, because a shrunk blast radius must not strand a quiesced
// node with its monitoring down.
func TestAgentDestructiveActionSet(t *testing.T) {
	want := map[string]bool{
		"agent.gpu_reset":                    true,
		"agent.reboot":                       true,
		"agent.driver_reload":                true,
		"agent.driver_reinstall":             true,
		"agent.run_script":                   true,
		"platform.quiesce_accelerator_stack": true,
	}
	for _, d := range definitions {
		if got := d.AgentDestructive; got != want[d.Wire] {
			t.Errorf("%q AgentDestructive = %v, want %v", d.Wire, got, want[d.Wire])
		}
	}
}
