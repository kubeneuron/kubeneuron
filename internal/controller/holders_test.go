package controller

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/kubeneuron/kubeneuron/internal/notify"
	"github.com/kubeneuron/kubeneuron/internal/playbook"
	"github.com/kubeneuron/kubeneuron/internal/safety"
	storesqlite "github.com/kubeneuron/kubeneuron/internal/store/sqlite"
	"github.com/kubeneuron/kubeneuron/pkg/types"
)

func gpuNode(labels map[string]string) *types.Node {
	return &types.Node{Name: "gpu-1", UID: "uid-1", Labels: labels}
}

func holder(command string, pid int, device string) types.AgentDeviceHolder {
	return types.AgentDeviceHolder{PID: pid, Command: command, Device: device}
}

// The measured EKS case: the device plugin comes from the machine image, so the
// GPU Operator's per-component label is absent and no label flip will remove it.
// That must be visible before a playbook cordons and drains the node.
func TestUnmanagedDevicePluginIsAnObstruction(t *testing.T) {
	node := gpuNode(map[string]string{
		"nvidia.com/gpu.deploy.dcgm":          "true",
		"nvidia.com/gpu.deploy.dcgm-exporter": "true",
		// no nvidia.com/gpu.deploy.device-plugin
	})
	got := resetObstructions(node, []types.AgentDeviceHolder{
		holder("nv-hostengine", 11850, "/dev/nvidia0"),
		holder("dcgm-exporter", 12382, "/dev/nvidia0"),
		holder("nvidia-persiste", 8140, "/dev/nvidia0"),
		holder("nvidia-device-p", 11567, "/dev/nvidia0"),
	}, nil)

	if len(got) != 1 {
		t.Fatalf("obstructions = %v, want only the unmanaged device plugin", got)
	}
	if got[0].Holder.Command != "nvidia-device-p" {
		t.Fatalf("obstruction = %+v", got[0])
	}
	// The message has to be actionable, not just a complaint.
	for _, want := range []string{"not managed by the GPU Operator", "machine image"} {
		if !strings.Contains(got[0].Reason, want) {
			t.Fatalf("reason = %q, want it to mention %q", got[0].Reason, want)
		}
	}
}

func TestOperatorManagedHoldersAreNotObstructions(t *testing.T) {
	node := gpuNode(map[string]string{
		"nvidia.com/gpu.deploy.dcgm":          "true",
		"nvidia.com/gpu.deploy.dcgm-exporter": "true",
		"nvidia.com/gpu.deploy.device-plugin": "true",
	})
	got := resetObstructions(node, []types.AgentDeviceHolder{
		holder("nv-hostengine", 1, "/dev/nvidia0"),
		holder("dcgm-exporter", 2, "/dev/nvidia0"),
		holder("nvidia-device-p", 3, "/dev/nvidia0"),
		// Released by the agent on the node; no label needs to exist.
		holder("nvidia-persiste", 4, "/dev/nvidia0"),
	}, nil)
	if len(got) != 0 {
		t.Fatalf("obstructions = %v, want none — the quiesce can release all of these", got)
	}
}

// Some processes hold the GPU and must not be stopped. fabricmanager is the
// standing example: killing it to make room for a remediation would break the
// GPUs it manages.
func TestDeclaredForbiddenHolderRefusesTheReset(t *testing.T) {
	node := gpuNode(map[string]string{"nvidia.com/gpu.deploy.dcgm": "true"})
	got := resetObstructions(node,
		[]types.AgentDeviceHolder{holder("nv-fabricmanager", 900, "/dev/nvidia0")},
		[]string{"nv-fabricmanager"})
	if len(got) != 1 || !strings.Contains(got[0].Reason, "forbidResetWhenPresent") {
		t.Fatalf("obstructions = %v, want the declared refusal", got)
	}
}

func TestUnknownHolderIsAnObstructionWithRemediation(t *testing.T) {
	got := resetObstructions(gpuNode(nil),
		[]types.AgentDeviceHolder{holder("datadog-agent", 555, "/dev/nvidia0")}, nil)
	if len(got) != 1 {
		t.Fatalf("obstructions = %v", got)
	}
	// An operator must learn both options rather than only that it failed.
	for _, want := range []string{"no way to release", "forbidResetWhenPresent"} {
		if !strings.Contains(got[0].Reason, want) {
			t.Fatalf("reason = %q, want it to mention %q", got[0].Reason, want)
		}
	}
}

// A process holding three device nodes is one thing for an operator to fix.
func TestObstructionsAreReportedOncePerProcess(t *testing.T) {
	got := resetObstructions(gpuNode(nil), []types.AgentDeviceHolder{
		holder("datadog-agent", 555, "/dev/nvidia0"),
		holder("datadog-agent", 555, "/dev/nvidia1"),
		holder("datadog-agent", 555, "/dev/nvidiactl"),
	}, nil)
	if len(got) != 1 {
		t.Fatalf("obstructions = %v, want one entry per process", got)
	}
}

// An empty list is evidence the node looked and found nothing. A blank declared
// name must not match it and forbid every reset.
func TestNoHoldersMeansNoObstructions(t *testing.T) {
	if got := resetObstructions(gpuNode(nil), []types.AgentDeviceHolder{}, []string{"", "  "}); len(got) != 0 {
		t.Fatalf("obstructions = %v, want none", got)
	}
}

// C1 regression: an AMD-attributed incident bound to a reset-bearing playbook
// used to cordon the node, drain every workload off it, and then re-deny the
// reset on every reconcile tick forever — the AMD UUID can never appear in an
// NVIDIA accelerator report, but "absent from the report" reads as evidence
// that has not arrived, which HOLDS. Nothing ever reached a human. The vendor
// mismatch must be refused BEFORE the first disruptive step.
func TestAMDIncidentRefusesNVIDIAResetBeforeAnyDisruption(t *testing.T) {
	book := &playbook.Playbook{
		Name: "drain-and-reset", Target: "gpu",
		Steps: []playbook.Step{
			{Name: "Cordon", Action: "platform.cordon"},
			{Name: "Drain", Action: "platform.drain"},
			{Name: "Reset", Action: "agent.gpu_reset"},
		},
	}
	engine, err := playbook.NewEngine(map[string]*playbook.Playbook{"drain-and-reset": book},
		[]playbook.Policy{{Class: types.ClassNVLink, Playbook: "drain-and-reset"}})
	if err != nil {
		t.Fatal(err)
	}
	c, st, _ := miniController(t, engine)
	ctx := context.Background()
	if err := st.UpsertNode(ctx, &types.Node{Name: "n1"}); err != nil {
		t.Fatal(err)
	}
	inc := &types.Incident{
		ID: "inc-amd", Target: types.Target{Node: "n1", GPUUUID: "amd-0000-1111"},
		Class: types.ClassNVLink, State: types.StateEvaluating, Playbook: "drain-and-reset",
		Vendor:   types.AcceleratorVendorAMD,
		OpenedAt: time.Now(), UpdatedAt: time.Now(), StateChangedAt: time.Now(),
	}
	if err := st.CreateIncident(ctx, inc); err != nil {
		t.Fatal(err)
	}
	if err := c.advanceEvaluating(ctx, inc); err != nil {
		t.Fatal(err)
	}
	got, err := st.GetIncident(ctx, inc.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != types.StateNeedsHuman {
		t.Fatalf("state = %s, want NEEDS_HUMAN: a reset that can never run must reach a human, not hold", got.State)
	}
	if got.StepIndex != 0 {
		t.Fatalf("step index = %d, want 0 — nothing may execute, least of all cordon and drain", got.StepIndex)
	}
	trail, err := st.AuditTrail(ctx, inc.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range trail {
		if e.Action == "Cordon" || e.Action == "Drain" {
			t.Fatalf("audit shows %q ran before the refusal — the node was disrupted for a reset that cannot happen", e.Action)
		}
	}
	// An NVIDIA incident on the same playbook must be untouched by the guard.
	nv := &types.Incident{
		ID: "inc-nv", Target: types.Target{Node: "n1", GPUUUID: "GPU-abc"},
		Class: types.ClassNVLink, State: types.StateEvaluating, Playbook: "drain-and-reset",
		Vendor:   types.AcceleratorVendorNVIDIA,
		OpenedAt: time.Now(), UpdatedAt: time.Now(), StateChangedAt: time.Now(),
	}
	if err := st.CreateIncident(ctx, nv); err != nil {
		t.Fatal(err)
	}
	if err := c.advanceEvaluating(ctx, nv); err != nil {
		t.Fatal(err)
	}
	if got, _ := st.GetIncident(ctx, nv.ID); got.State == types.StateNeedsHuman {
		t.Fatal("an NVIDIA incident must not be refused by the vendor guard")
	}
}

// TestVendorlessIncidentRefusesAResetTheNodeCannotServe closes the door the
// incident-side check cannot see. An incident opened by a manual trigger or a
// metric alert never learns its vendor, so resetVendorMismatch has nothing to
// compare — yet the node itself reports which runtime it has. Without this,
// such an incident cordoned and drained the node and only then failed at the
// reset step on missing evidence: a hold with no deadline, re-denying every
// tick, node left drained, nobody paged.
func TestVendorlessIncidentRefusesAResetTheNodeCannotServe(t *testing.T) {
	st, err := storesqlite.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ctx := context.Background()

	book := &playbook.Playbook{Name: "reset-nv", Target: "gpu", Steps: []playbook.Step{
		{Name: "cordon", Action: "platform.cordon"},
		{Name: "reset", Action: "agent.gpu_reset"},
	}}
	engine, err := playbook.NewEngine(map[string]*playbook.Playbook{book.Name: book}, nil)
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	c := New(st, st, engine,
		safety.NewGate(safety.Limits{MaxConcurrentRemediations: 4, MaxConcurrentReboots: 1}),
		nil, nil, nil, &notify.Log{Logger: log}, log)

	if err := st.UpsertNode(ctx, &types.Node{
		Name: "node-amd", UID: "node-amd-uid", AgentLastSeen: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	// The machine says what it runs: AMD, and no NVIDIA.
	if err := st.UpsertAcceleratorReport(ctx, &types.AgentAcceleratorReport{
		Node: "node-amd", NodeUID: "node-amd-uid", Vendor: types.AcceleratorVendorAMD,
		Readiness: types.AcceleratorReadinessReady, ObservedAt: time.Now(),
		TopologySafety: types.AcceleratorTopologyVerifiedUnpartitioned,
		DriverVersion:  "6.8.5", RuntimeVersion: "amd-smi 24.6.2",
	}); err != nil {
		t.Fatal(err)
	}

	inc := &types.Incident{
		ID: "inc-vendorless", Target: types.Target{Node: "node-amd", GPUUUID: "GPU-amd-0"},
		Class: types.ClassECCDBE, State: types.StateEvaluating, Playbook: book.Name,
		// Vendor deliberately unset: a manual trigger names none.
		OpenedAt: time.Now(), UpdatedAt: time.Now(), StateChangedAt: time.Now(),
	}
	if err := st.CreateIncident(ctx, inc); err != nil {
		t.Fatal(err)
	}

	err = c.refuseInfeasibleReset(ctx, inc, book)
	if !errors.Is(err, errResetVendorMismatch) {
		t.Fatalf("refuseInfeasibleReset = %v, want errResetVendorMismatch: the node reports an AMD "+
			"runtime and no NVIDIA one, so this reset can never run and must be refused BEFORE the cordon", err)
	}
}

// A node that has simply not reported yet must be left alone: "no evidence
// yet" is not "evidence of a different runtime", and refusing on it would
// block every legitimate reset during an agent restart.
func TestSilentNodeDoesNotTriggerTheVendorAbsenceRefusal(t *testing.T) {
	st, err := storesqlite.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ctx := context.Background()

	book := &playbook.Playbook{Name: "reset-nv", Target: "gpu", Steps: []playbook.Step{
		{Name: "reset", Action: "agent.gpu_reset"},
	}}
	inc := &types.Incident{
		ID: "inc-silent", Target: types.Target{Node: "node-quiet", GPUUUID: "GPU-0"},
		Class: types.ClassECCDBE, State: types.StateEvaluating, Playbook: book.Name,
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	c := New(st, st, nil,
		safety.NewGate(safety.Limits{MaxConcurrentRemediations: 4, MaxConcurrentReboots: 1}),
		nil, nil, nil, &notify.Log{Logger: log}, log)

	if _, mismatched := c.resetVendorAbsentFromNode(ctx, inc, book); mismatched {
		t.Fatal("refused a reset on a node that has reported nothing at all; " +
			"an agent that has not spoken yet is not evidence of a different runtime")
	}
}

// TestDryRunRefusesAStructurallyImpossibleReset covers the number a pilot is
// told to read.
//
// The structural refusals used to be skipped for dry-run incidents, so the
// simulation modelled a strictly MORE capable system than the live one: on AMD
// silicon an ecc-dbe incident walked its whole ladder and landed in "would
// have recovered", while the same incident live is refused before the first
// disruptive step and parked for a human. The pilot checklist's whole premise
// is to sit in dry-run and read what this would have got you, and the answer
// was inflated worst on exactly the fleets where it should read "nothing".
func TestDryRunRefusesAStructurallyImpossibleReset(t *testing.T) {
	st, err := storesqlite.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	c := New(st, st, nil, safety.NewGate(safety.Limits{MaxConcurrentRemediations: 2}),
		nil, nil, nil, &notify.Log{Logger: log}, log)

	book := &playbook.Playbook{Name: "drain-and-reset", Steps: []playbook.Step{
		{Name: "reset", Action: "agent.gpu_reset"},
	}}

	// An AMD device, a ladder whose repair rung is scoped to NVIDIA.
	amd := &types.Incident{
		ID: "amd-1", Target: types.Target{Node: "n1", GPUUUID: "GPU-AMD-1"},
		Class: types.ClassECCDBE, Vendor: types.AcceleratorVendorAMD, DryRun: true,
	}
	if err := c.refuseInfeasibleReset(context.Background(), amd, book); err == nil {
		t.Fatal("a dry-run AMD incident simulated an NVIDIA-scoped reset; the report would " +
			"count it as capacity this fleet would have recovered, which it provably would not")
	}

	// And an unattributed incident, which can never gain a device.
	unattributed := &types.Incident{
		ID: "un-1", Target: types.Target{Node: "n1"},
		Class: types.ClassECCDBE, DryRun: true,
	}
	if err := c.refuseInfeasibleReset(context.Background(), unattributed, book); err == nil {
		t.Fatal("a dry-run incident with no GPU UUID simulated a per-device reset")
	}

	// A well-formed NVIDIA incident is untouched: the simulation still runs.
	ok := &types.Incident{
		ID: "nv-1", Target: types.Target{Node: "n1", GPUUUID: "GPU-NV-1"},
		Class: types.ClassECCDBE, Vendor: types.AcceleratorVendorNVIDIA, DryRun: true,
	}
	if err := c.refuseInfeasibleReset(context.Background(), ok, book); err != nil {
		t.Fatalf("a valid dry-run reset was refused: %v", err)
	}
}
