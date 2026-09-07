package controller

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/kubeneuron/kubeneuron/internal/config"
	"github.com/kubeneuron/kubeneuron/internal/notify"
	"github.com/kubeneuron/kubeneuron/internal/playbook"
	"github.com/kubeneuron/kubeneuron/internal/safety"
	storesqlite "github.com/kubeneuron/kubeneuron/internal/store/sqlite"
	"github.com/kubeneuron/kubeneuron/pkg/types"
)

func TestNVIDIAResetCapabilityGateRequiresProfileFreshReportAndExactDevice(t *testing.T) {
	st, err := storesqlite.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ctx := context.Background()
	if err := st.UpsertNode(ctx, &types.Node{Name: "node-a", UID: "node-uid-a", Labels: map[string]string{"accelerator": "nvidia-h100"}}); err != nil {
		t.Fatal(err)
	}
	c := New(st, nil, nil, nil, nil, nil, nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	target := types.Target{Node: "node-a", GPUUUID: "GPU-a"}
	if err := c.allowNVIDIAReset(ctx, &types.Incident{ID: "inc-gate"}, target); !errors.Is(err, config.ErrNoAcceleratorRuntimeProfile) {
		t.Fatalf("default-deny reset gate = %v, want ErrNoAcceleratorRuntimeProfile", err)
	}

	profile := testNVIDIAResetProfile()
	if err := c.SetAcceleratorRuntimeProfiles([]config.AcceleratorRuntimeProfile{profile}); err != nil {
		t.Fatalf("SetAcceleratorRuntimeProfiles() error = %v", err)
	}
	report := readyNVIDIAResetReport(time.Now().UTC(), profile.ProfileDigest)
	if err := st.UpsertAcceleratorReport(ctx, &report); err != nil {
		t.Fatalf("UpsertAcceleratorReport() error = %v", err)
	}
	if err := c.allowNVIDIAReset(ctx, &types.Incident{ID: "inc-gate"}, target); err != nil {
		t.Fatalf("fresh matching report reset gate = %v", err)
	}

	if err := c.allowNVIDIAReset(ctx, &types.Incident{ID: "inc-gate"}, types.Target{Node: "node-a", GPUUUID: "GPU-missing"}); err == nil || !strings.Contains(err.Error(), "does not contain targeted") {
		t.Fatalf("unknown device reset gate = %v, want physical inventory denial", err)
	}
}

// TestAcceleratorGateFiresOnlyForRegisteredCapability locks in the collapsed
// special-case: allowAcceleratorStep engages the NVIDIA reset gate for the one
// registry action that declares it (agent.gpu_reset) and is a no-op for every
// other agent action, so the gate is a registry fact, not a string match.
func TestAcceleratorGateFiresOnlyForRegisteredCapability(t *testing.T) {
	st, err := storesqlite.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ctx := context.Background()
	if err := st.UpsertNode(ctx, &types.Node{Name: "node-a", UID: "node-uid-a", Labels: map[string]string{"accelerator": "nvidia-h100"}}); err != nil {
		t.Fatal(err)
	}
	c := New(st, nil, nil, nil, nil, nil, nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	liveLimits := safety.Limits{MaxConcurrentRemediations: 2, MaxConcurrentReboots: 1, DryRun: false}
	if err := c.InstallRuntimeConfig(RuntimeConfig{SafetyLimits: &liveLimits}); err != nil {
		t.Fatal(err)
	}
	inc := &types.Incident{ID: "inc-gate", Target: types.Target{Node: "node-a", GPUUUID: "GPU-a"}, DryRun: false}

	// A non-reset agent action never touches the capability gate.
	for _, ungated := range []string{"agent.collect_bundle", "agent.run_diag", "platform.cordon", "agent.driver_reinstall"} {
		if err := c.allowAcceleratorStep(ctx, inc, &playbook.Step{Action: ungated}); err != nil {
			t.Errorf("allowAcceleratorStep(%q) = %v, want nil (ungated)", ungated, err)
		}
	}

	// agent.gpu_reset engages the gate, which default-denies without a profile.
	if err := c.allowAcceleratorStep(ctx, inc, &playbook.Step{Action: "agent.gpu_reset"}); !errors.Is(err, config.ErrNoAcceleratorRuntimeProfile) {
		t.Fatalf("allowAcceleratorStep(agent.gpu_reset) = %v, want the reset gate default-deny", err)
	}

	// Dry-run keeps the planned ladder observable without any capability.
	dry := &types.Incident{ID: "inc-dry", Target: types.Target{Node: "node-a", GPUUUID: "GPU-a"}, DryRun: true}
	if err := c.allowAcceleratorStep(ctx, dry, &playbook.Step{Action: "agent.gpu_reset"}); err != nil {
		t.Fatalf("allowAcceleratorStep(dry-run gpu_reset) = %v, want nil", err)
	}
}

func TestNVIDIAResetCapabilityGateRejectsStaleOrPartitionedEvidence(t *testing.T) {
	for name, mutate := range map[string]func(*types.AgentAcceleratorReport){
		"stale": func(report *types.AgentAcceleratorReport) {
			report.ObservedAt = time.Now().Add(-2 * time.Minute).UTC()
		},
		"partitioned": func(report *types.AgentAcceleratorReport) {
			report.TopologySafety = types.AcceleratorTopologyPartitioned
			report.Capabilities = []types.AgentAcceleratorCapability{{
				Action: types.AcceleratorActionVerifyHealth,
				Scopes: []types.AcceleratorTargetScope{types.AcceleratorScopeNode},
			}}
		},
		"different profile generation": func(report *types.AgentAcceleratorReport) {
			report.ProfileGeneration = 2
		},
		"different driver version": func(report *types.AgentAcceleratorReport) {
			report.DriverVersion = "550.54.15"
		},
		"different node UID": func(report *types.AgentAcceleratorReport) {
			report.NodeUID = "node-uid-recreated"
		},
	} {
		t.Run(name, func(t *testing.T) {
			st, err := storesqlite.Open(":memory:")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = st.Close() })
			ctx := context.Background()
			if err := st.UpsertNode(ctx, &types.Node{Name: "node-a", UID: "node-uid-a", Labels: map[string]string{"accelerator": "nvidia-h100"}}); err != nil {
				t.Fatal(err)
			}
			c := New(st, nil, nil, nil, nil, nil, nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
			profile := testNVIDIAResetProfile()
			profile.MaxReportAge = config.Duration(time.Minute)
			if err := c.SetAcceleratorRuntimeProfiles([]config.AcceleratorRuntimeProfile{profile}); err != nil {
				t.Fatal(err)
			}
			report := readyNVIDIAResetReport(time.Now().UTC(), profile.ProfileDigest)
			mutate(&report)
			if err := st.UpsertAcceleratorReport(ctx, &report); err != nil {
				t.Fatal(err)
			}
			if err := c.allowNVIDIAReset(ctx, &types.Incident{ID: "inc-gate"}, types.Target{Node: "node-a", GPUUUID: "GPU-a"}); err == nil {
				t.Fatal("stale or partitioned evidence passed the reset capability gate")
			}
		})
	}
}

func TestNVIDIAResetCapabilityGateRunsBeforeExecutor(t *testing.T) {
	st, err := storesqlite.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	actuator := &resetGateActuator{}
	c := New(st, nil, nil, nil, nil, nil, actuator, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	_, err = c.executeAgentStep(context.Background(), &types.Incident{
		ID: "incident-a", Target: types.Target{Node: "node-a", GPUUUID: "GPU-a"}, DryRun: false,
	}, string(types.ActionGPUReset), &playbook.Step{Action: "agent.gpu_reset"})
	if err == nil || !strings.Contains(err.Error(), "accelerator capability gate") {
		t.Fatalf("executeAgentStep error = %v, want capability-gate denial", err)
	}
	if actuator.calls != 0 {
		t.Fatalf("executor calls = %d, want 0 after gate denial", actuator.calls)
	}
}

func TestNVIDIAResetCapabilityPreconditionHoldsInsteadOfEscalating(t *testing.T) {
	st, err := storesqlite.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	actuator := &resetGateActuator{}
	c := New(st, nil, nil,
		safety.NewGate(safety.Limits{MaxConcurrentRemediations: 1, MaxConcurrentReboots: 1}),
		nil, nil, actuator, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	inc := &types.Incident{
		ID: "incident-a", State: types.StateEvaluating,
		Target: types.Target{Node: "node-a", GPUUUID: "GPU-a"}, DryRun: false,
		// INSIDE the evidence deadline. The hold this test is about is the
		// one that applies while evidence may still be on its way; the zero
		// value meant "long ago", which is now a different case with its own
		// test below.
		StateChangedAt: time.Now(),
	}
	if err := c.startStep(context.Background(), inc, &playbook.Step{Name: "reset", Action: "agent.gpu_reset"}, "system"); err != nil {
		t.Fatalf("startStep() error = %v", err)
	}
	if inc.State != types.StateEvaluating || c.isInFlight(inc.ID) {
		t.Fatalf("capability denial changed incident execution state: %+v", inc)
	}
	if actuator.calls != 0 {
		t.Fatalf("executor calls = %d, want 0 after held capability precondition", actuator.calls)
	}
}

func TestAcceleratorObservationProfileSelectsServerOwnedDigestOrFailsClosed(t *testing.T) {
	st, err := storesqlite.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ctx := context.Background()
	if err := st.UpsertNode(ctx, &types.Node{Name: "node-a", UID: "node-uid-a", Labels: map[string]string{"accelerator": "nvidia-h100"}}); err != nil {
		t.Fatal(err)
	}
	c := New(st, nil, nil, nil, nil, nil, nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))

	profile, err := c.AcceleratorObservationProfile(ctx, "node-a", types.AcceleratorVendorNVIDIA)
	if err != nil || profile != nil {
		t.Fatalf("default-deny profile = %+v, %v; want nil, nil", profile, err)
	}
	selected := testNVIDIAResetProfile()
	if err := c.SetAcceleratorRuntimeProfiles([]config.AcceleratorRuntimeProfile{selected}); err != nil {
		t.Fatal(err)
	}
	profile, err = c.AcceleratorObservationProfile(ctx, "node-a", types.AcceleratorVendorNVIDIA)
	if err != nil || profile == nil || profile.Vendor != types.AcceleratorVendorNVIDIA || profile.ProfileDigest != selected.ProfileDigest ||
		profile.ProfileUID != selected.ProfileUID || profile.ProfileGeneration != selected.ProfileGeneration {
		t.Fatalf("selected observation profile = %+v, %v", profile, err)
	}
	if profile, err := c.AcceleratorObservationProfile(ctx, "node-a", types.AcceleratorVendorAMD); err != nil || profile != nil {
		t.Fatalf("different vendor profile = %+v, %v; want nil, nil", profile, err)
	}

	overlap := selected
	overlap.Name = "nvidia-h100-overlap"
	if err := c.SetAcceleratorRuntimeProfiles([]config.AcceleratorRuntimeProfile{selected, overlap}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.AcceleratorObservationProfile(ctx, "node-a", types.AcceleratorVendorNVIDIA); !errors.Is(err, config.ErrAmbiguousAcceleratorRuntimeProfile) {
		t.Fatalf("overlapping observation profile error = %v, want ambiguity", err)
	}
}

func testNVIDIAResetProfile() config.AcceleratorRuntimeProfile {
	return config.AcceleratorRuntimeProfile{
		Name:              "nvidia-h100-reset-v1",
		NodeSelector:      map[string]string{"accelerator": "nvidia-h100"},
		Vendor:            types.AcceleratorVendorNVIDIA,
		ProfileDigest:     "sha256:" + strings.Repeat("a", 64),
		DriverVersion:     "570.42",
		RuntimeVersion:    "gpu-operator-25.3",
		ProfileUID:        "nvidia-h100-reset-uid",
		ProfileGeneration: 1,
		MaxReportAge:      config.Duration(5 * time.Minute),
		AllowedActions: []config.AcceleratorActionPolicy{{
			Action:                               types.AcceleratorActionResetDevice,
			Scopes:                               []types.AcceleratorTargetScope{types.AcceleratorScopePhysicalDevice},
			RequireVerifiedUnpartitionedTopology: true,
		}},
	}
}

func readyNVIDIAResetReport(observedAt time.Time, digest string) types.AgentAcceleratorReport {
	return types.AgentAcceleratorReport{
		Node:           "node-a",
		NodeUID:        "node-uid-a",
		Vendor:         types.AcceleratorVendorNVIDIA,
		ObservedAt:     observedAt,
		DriverVersion:  "570.42",
		RuntimeVersion: "gpu-operator-25.3",
		TopologySafety: types.AcceleratorTopologyVerifiedUnpartitioned,
		Devices: []types.AgentAcceleratorDevice{{
			ID: "GPU-a", Kind: types.AcceleratorDevicePhysical, Family: types.AcceleratorFamilyGPU,
		}},
		Capabilities: []types.AgentAcceleratorCapability{{
			Action: types.AcceleratorActionResetDevice,
			Scopes: []types.AcceleratorTargetScope{types.AcceleratorScopePhysicalDevice},
		}},
		Readiness:         types.AcceleratorReadinessReady,
		ProfileDigest:     digest,
		ProfileUID:        "nvidia-h100-reset-uid",
		ProfileGeneration: 1,
	}
}

type resetGateActuator struct{ calls int }

func (a *resetGateActuator) Name() string { return "reset-gate-test" }

func (a *resetGateActuator) Capabilities() []types.ActionType {
	return []types.ActionType{types.ActionGPUReset}
}

func (a *resetGateActuator) Healthy(context.Context, types.Node) error { return nil }

func (a *resetGateActuator) Execute(context.Context, types.Node, types.Action) (*types.ActionResult, error) {
	a.calls++
	return &types.ActionResult{OK: true}, nil
}

// Non-dry-run incidents must not resolve on a quiet window alone: fresh
// runtime evidence (heartbeat + ready report listing the device) is
// required, missing evidence holds, and the evidence deadline fails closed
// to NEEDS_HUMAN. Dry-run incidents keep the quiet-window-only contract.
func TestVerifyRequiresRuntimeEvidenceForRealIncidents(t *testing.T) {
	newFixture := func(t *testing.T) (*Controller, *storesqlite.Store, context.Context) {
		t.Helper()
		st, err := storesqlite.Open(":memory:")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = st.Close() })
		engine, err := playbook.NewEngine(map[string]*playbook.Playbook{}, nil)
		if err != nil {
			t.Fatal(err)
		}
		gate := safety.NewGate(safety.Limits{MaxConcurrentRemediations: 4, MaxConcurrentReboots: 1})
		log := slog.New(slog.NewTextHandler(io.Discard, nil))
		c := New(st, st, engine, gate, nil, nil, nil, &notify.Log{Logger: log}, log)
		c.SetTimings(time.Millisecond, time.Hour) // quiet window: 1ms
		return c, st, context.Background()
	}

	verifying := func(dryRun bool, stateChangedAgo time.Duration) *types.Incident {
		now := time.Now()
		changed := now.Add(-stateChangedAgo)
		return &types.Incident{
			ID:     "inc-verify-" + map[bool]string{true: "dry", false: "real"}[dryRun],
			Target: types.Target{Node: "node-a", GPUUUID: "GPU-a"},
			Class:  types.ClassECCDBE, State: types.StateVerifying, DryRun: dryRun,
			OpenedAt: changed, UpdatedAt: changed, StateChangedAt: changed,
		}
	}

	t.Run("dry-run resolves on quiet window alone", func(t *testing.T) {
		c, st, ctx := newFixture(t)
		inc := verifying(true, time.Minute)
		if err := st.CreateIncident(ctx, inc); err != nil {
			t.Fatal(err)
		}
		if err := c.advance(ctx, inc); err != nil {
			t.Fatal(err)
		}
		got, _ := st.GetIncident(ctx, inc.ID)
		if got.State != types.StateResolved {
			t.Fatalf("dry-run state = %s, want RESOLVED", got.State)
		}
	})

	t.Run("real incident without evidence holds inside the deadline", func(t *testing.T) {
		c, st, ctx := newFixture(t)
		inc := verifying(false, time.Minute) // past quiet window, before deadline
		if err := st.CreateIncident(ctx, inc); err != nil {
			t.Fatal(err)
		}
		if err := c.advance(ctx, inc); err != nil {
			t.Fatal(err)
		}
		got, _ := st.GetIncident(ctx, inc.ID)
		if got.State != types.StateVerifying {
			t.Fatalf("state = %s, want VERIFYING hold without evidence", got.State)
		}
	})

	t.Run("real incident without evidence fails closed after the deadline", func(t *testing.T) {
		c, st, ctx := newFixture(t)
		inc := verifying(false, 15*time.Minute) // past the 10m evidence floor
		if err := st.CreateIncident(ctx, inc); err != nil {
			t.Fatal(err)
		}
		if err := c.advance(ctx, inc); err != nil {
			t.Fatal(err)
		}
		got, _ := st.GetIncident(ctx, inc.ID)
		if got.State != types.StateNeedsHuman {
			t.Fatalf("state = %s, want NEEDS_HUMAN after evidence deadline", got.State)
		}
	})

	t.Run("real incident resolves with fresh heartbeat and ready report", func(t *testing.T) {
		c, st, ctx := newFixture(t)
		if err := st.UpsertNode(ctx, &types.Node{Name: "node-a", UID: "node-uid-a", AgentLastSeen: time.Now()}); err != nil {
			t.Fatal(err)
		}
		report := readyNVIDIAResetReport(time.Now().UTC(), "digest")
		if err := st.UpsertAcceleratorReport(ctx, &report); err != nil {
			t.Fatal(err)
		}
		inc := verifying(false, time.Minute)
		if err := st.CreateIncident(ctx, inc); err != nil {
			t.Fatal(err)
		}
		if err := c.advance(ctx, inc); err != nil {
			t.Fatal(err)
		}
		got, _ := st.GetIncident(ctx, inc.ID)
		if got.State != types.StateResolved {
			t.Fatalf("state = %s, want RESOLVED with evidence", got.State)
		}
	})

	t.Run("missing target GPU blocks resolution", func(t *testing.T) {
		c, st, ctx := newFixture(t)
		if err := st.UpsertNode(ctx, &types.Node{Name: "node-a", UID: "node-uid-a", AgentLastSeen: time.Now()}); err != nil {
			t.Fatal(err)
		}
		report := readyNVIDIAResetReport(time.Now().UTC(), "digest")
		report.Devices = []types.AgentAcceleratorDevice{{
			ID: "GPU-other", Kind: types.AcceleratorDevicePhysical, Family: types.AcceleratorFamilyGPU,
		}}
		if err := st.UpsertAcceleratorReport(ctx, &report); err != nil {
			t.Fatal(err)
		}
		inc := verifying(false, time.Minute)
		if err := st.CreateIncident(ctx, inc); err != nil {
			t.Fatal(err)
		}
		if err := c.advance(ctx, inc); err != nil {
			t.Fatal(err)
		}
		got, _ := st.GetIncident(ctx, inc.ID)
		if got.State != types.StateVerifying {
			t.Fatalf("state = %s, want VERIFYING hold when the GPU vanished", got.State)
		}
	})
}

// TestVendorWithoutARuntimeAdapterVerifiesOnTheHeartbeat is the AMD dead-end.
//
// verifyRuntimeEvidence used to read an NVIDIA accelerator report for every
// GPU-scoped incident regardless of the incident's vendor, and only the NVIDIA
// adapter exists. On a perfectly healthy AMD node no report is ever produced,
// so a device-scoped AMD incident held for the evidence deadline and then
// parked in NEEDS_HUMAN — after the cordon and drain had already run, with a
// reason naming no action anybody could take. Every such incident landed as
// outcome="needs_human", so an AMD fleet's recovery report read 0% recovered
// for a system that had recovered them.
func TestVendorWithoutARuntimeAdapterVerifiesOnTheHeartbeat(t *testing.T) {
	newFixture := func(t *testing.T) (*Controller, *storesqlite.Store, context.Context) {
		t.Helper()
		st, err := storesqlite.Open(":memory:")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = st.Close() })
		engine, err := playbook.NewEngine(map[string]*playbook.Playbook{}, nil)
		if err != nil {
			t.Fatal(err)
		}
		log := slog.New(slog.NewTextHandler(io.Discard, nil))
		c := New(st, st, engine,
			safety.NewGate(safety.Limits{MaxConcurrentRemediations: 4, MaxConcurrentReboots: 1}),
			nil, nil, nil, &notify.Log{Logger: log}, log)
		c.SetTimings(time.Millisecond, time.Hour)
		ctx := context.Background()
		// A healthy node: fresh heartbeat, registered inventory.
		if err := st.UpsertNode(ctx, &types.Node{
			Name: "node-amd", UID: "node-amd-uid", AgentLastSeen: time.Now(),
			GPUs: []types.GPUInfo{{Index: 0, UUID: "GPU-amd-0"}},
		}); err != nil {
			t.Fatal(err)
		}
		return c, st, ctx
	}

	incident := func(vendor types.AcceleratorVendor) *types.Incident {
		changed := time.Now().Add(-15 * time.Minute) // past the 10m evidence floor
		return &types.Incident{
			ID:     "inc-" + string(vendor),
			Target: types.Target{Node: "node-amd", GPUUUID: "GPU-amd-0"},
			Class:  types.ClassECCDBE, State: types.StateVerifying, Vendor: vendor,
			OpenedAt: changed, UpdatedAt: changed, StateChangedAt: changed,
		}
	}

	t.Run("amd resolves rather than dead-ending", func(t *testing.T) {
		c, st, ctx := newFixture(t)
		inc := incident(types.AcceleratorVendorAMD)
		if err := st.CreateIncident(ctx, inc); err != nil {
			t.Fatal(err)
		}
		if err := c.advance(ctx, inc); err != nil {
			t.Fatal(err)
		}
		got, _ := st.GetIncident(ctx, inc.ID)
		if got.State != types.StateResolved {
			t.Fatalf("state = %s, want RESOLVED: this build has no AMD runtime adapter, so the "+
				"report it was waiting for can never be produced by any agent", got.State)
		}
	})

	// The other half, and the one that must not regress: NVIDIA is attested by
	// this build, so a missing report there is a degraded agent, not an absent
	// runtime — and it must still fail closed.
	//
	// "Told to report" is what makes the absence a degraded agent: the node
	// carries the label a configured profile selects, so its agent was served
	// that profile and has still posted nothing. Without the profile the
	// absence means something else entirely — see
	// TestNVIDIAIncidentWithoutASelectingProfileVerifiesOnTheHeartbeat — and a
	// label-less node would make this case pass through the "cannot tell"
	// branch rather than the one it is about.
	t.Run("nvidia still fails closed without its report", func(t *testing.T) {
		c, st, ctx := newFixture(t)
		if err := st.UpsertNode(ctx, &types.Node{
			Name: "node-amd", UID: "node-amd-uid", AgentLastSeen: time.Now(),
			Labels: map[string]string{"accelerator": "nvidia-h100"},
			GPUs:   []types.GPUInfo{{Index: 0, UUID: "GPU-amd-0"}},
		}); err != nil {
			t.Fatal(err)
		}
		if err := c.SetAcceleratorRuntimeProfiles([]config.AcceleratorRuntimeProfile{testNVIDIAResetProfile()}); err != nil {
			t.Fatal(err)
		}
		inc := incident(types.AcceleratorVendorNVIDIA)
		if err := st.CreateIncident(ctx, inc); err != nil {
			t.Fatal(err)
		}
		if err := c.advance(ctx, inc); err != nil {
			t.Fatal(err)
		}
		got, _ := st.GetIncident(ctx, inc.ID)
		if got.State != types.StateNeedsHuman {
			t.Fatalf("state = %s, want NEEDS_HUMAN: a missing report from a vendor this build "+
				"CAN attest, on a node a profile selects, is a degraded agent, and resolving "+
				"on it would be the one direction verification must never fail", got.State)
		}
	})

	// An incident that never learned its vendor keeps the conservative path.
	t.Run("unknown vendor still fails closed", func(t *testing.T) {
		c, st, ctx := newFixture(t)
		inc := incident("")
		inc.ID = "inc-unknown"
		if err := st.CreateIncident(ctx, inc); err != nil {
			t.Fatal(err)
		}
		if err := c.advance(ctx, inc); err != nil {
			t.Fatal(err)
		}
		got, _ := st.GetIncident(ctx, inc.ID)
		if got.State != types.StateNeedsHuman {
			t.Fatalf("state = %s, want NEEDS_HUMAN for an unattributed incident", got.State)
		}
	})
}

// TestNVIDIAIncidentWithoutASelectingProfileVerifiesOnTheHeartbeat is the
// no-profile dead end, the seam between two deliberate behaviours.
//
// The operator-managed agent runs with --nvidia-controller-profile and holds
// observation — posts no accelerator report at all — whenever the controller
// answers that no AcceleratorRuntimeProfile selects its node. That is the
// default-deny for CAPABILITIES. verifyRuntimeEvidence read the same absence
// as a degraded agent and failed closed, which is right when the agent was
// told to report, and a dead end when it was told not to: the ordinary Enabled
// install ships no profile (the samples, the chart and install.sh create
// none), so on a real driver every device-scoped incident held for the
// evidence deadline and parked in NEEDS_HUMAN after the cordon and drain.
// Hardware run 12 found it: the first real-mode, device-scoped resolution ever
// attempted on a real driver, an observe-only ladder, waited ten minutes for a
// report the agent had been told not to send.
//
// The contract now: a node NO profile selects verifies on the heartbeat, at
// reduced depth, named in the audit; a node a profile DOES select still fails
// closed on a missing report; anything that cannot say which fails closed
// with the cause in the reason. Fresh negative evidence is never overridden.
func TestNVIDIAIncidentWithoutASelectingProfileVerifiesOnTheHeartbeat(t *testing.T) {
	const node = "node-a"
	labels := map[string]string{"accelerator": "nvidia-h100", "nvidia.com/gpu.present": "true"}

	newFixture := func(t *testing.T, heartbeat time.Time) (*Controller, *storesqlite.Store, context.Context) {
		t.Helper()
		st, err := storesqlite.Open(":memory:")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = st.Close() })
		engine, err := playbook.NewEngine(map[string]*playbook.Playbook{}, nil)
		if err != nil {
			t.Fatal(err)
		}
		log := slog.New(slog.NewTextHandler(io.Discard, nil))
		c := New(st, st, engine,
			safety.NewGate(safety.Limits{MaxConcurrentRemediations: 4, MaxConcurrentReboots: 1}),
			nil, nil, nil, &notify.Log{Logger: log}, log)
		c.SetTimings(time.Millisecond, time.Hour)
		ctx := context.Background()
		if err := st.UpsertNode(ctx, &types.Node{
			Name: node, UID: "node-uid-a", AgentLastSeen: heartbeat, Labels: labels,
			GPUs: []types.GPUInfo{{Index: 0, UUID: "GPU-a"}},
		}); err != nil {
			t.Fatal(err)
		}
		return c, st, ctx
	}

	// A real (non-dry-run) NVIDIA device-scoped incident past its quiet window.
	incident := func(id string, stateChangedAgo time.Duration) *types.Incident {
		changed := time.Now().Add(-stateChangedAgo)
		return &types.Incident{
			ID:     id,
			Target: types.Target{Node: node, GPUUUID: "GPU-a"},
			Class:  types.ClassECCSBERate, State: types.StateVerifying, Vendor: types.AcceleratorVendorNVIDIA,
			OpenedAt: changed, UpdatedAt: changed, StateChangedAt: changed,
		}
	}
	const insideDeadline = time.Minute
	const pastDeadline = 15 * time.Minute // past the 10m evidence floor

	advance := func(t *testing.T, c *Controller, st *storesqlite.Store, ctx context.Context, inc *types.Incident) *types.Incident {
		t.Helper()
		if err := st.CreateIncident(ctx, inc); err != nil {
			t.Fatal(err)
		}
		if err := c.advance(ctx, inc); err != nil {
			t.Fatal(err)
		}
		got, err := st.GetIncident(ctx, inc.ID)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}

	lastAudit := func(t *testing.T, st *storesqlite.Store, ctx context.Context, id string) *types.AuditEntry {
		t.Helper()
		trail, err := st.AuditTrail(ctx, id)
		if err != nil || len(trail) == 0 {
			t.Fatalf("audit trail for %s: %v (%d entries)", id, err, len(trail))
		}
		return trail[len(trail)-1]
	}

	t.Run("no profile selects the node: resolves on the heartbeat, inside the deadline, and the audit names the depth", func(t *testing.T) {
		c, st, ctx := newFixture(t, time.Now())
		got := advance(t, c, st, ctx, incident("inc-no-profile", insideDeadline))
		if got.State != types.StateResolved {
			t.Fatalf("state = %s, want RESOLVED: no profile selects the node, so its agent holds "+
				"observation and the report this hold waited for cannot be produced", got.State)
		}
		entry := lastAudit(t, st, ctx, got.ID)
		if entry.Action != "resolve" || !strings.Contains(entry.Result, "healthy: quiet for") ||
			!strings.Contains(entry.Result, "verified on the agent heartbeat") ||
			!strings.Contains(entry.Result, "no nvidia accelerator runtime profile selects the node") {
			t.Fatalf("resolve audit = %q %q, want the reduced depth and its cause named beside the resolution", entry.Action, entry.Result)
		}
	})

	t.Run("no profile and only a stale report: resolves on the heartbeat", func(t *testing.T) {
		// A report from before the profile was removed. The agent has been
		// holding since, so waiting for a fresher one is the same dead end.
		c, st, ctx := newFixture(t, time.Now())
		stale := readyNVIDIAResetReport(time.Now().Add(-time.Hour).UTC(), "digest")
		if err := st.UpsertAcceleratorReport(ctx, &stale); err != nil {
			t.Fatal(err)
		}
		got := advance(t, c, st, ctx, incident("inc-no-profile-stale", insideDeadline))
		if got.State != types.StateResolved {
			t.Fatalf("state = %s, want RESOLVED", got.State)
		}
	})

	t.Run("no profile but a fresh not-ready report: fresh negative evidence still fails closed", func(t *testing.T) {
		// A statically configured agent can report without a controller
		// profile. A current report saying the runtime is not ready is real
		// evidence, and the heartbeat must never outrank it.
		c, st, ctx := newFixture(t, time.Now())
		degraded := readyNVIDIAResetReport(time.Now().UTC(), "digest")
		degraded.Readiness = types.AcceleratorReadinessNotReady
		degraded.ReadinessReasons = []string{"nvidia-smi timed out"}
		if err := st.UpsertAcceleratorReport(ctx, &degraded); err != nil {
			t.Fatal(err)
		}
		got := advance(t, c, st, ctx, incident("inc-no-profile-notready", pastDeadline))
		if got.State != types.StateNeedsHuman {
			t.Fatalf("state = %s, want NEEDS_HUMAN on a fresh not-ready report", got.State)
		}
	})

	t.Run("no profile and a stale heartbeat: still fails closed", func(t *testing.T) {
		c, st, ctx := newFixture(t, time.Now().Add(-time.Hour))
		got := advance(t, c, st, ctx, incident("inc-no-profile-dead-agent", pastDeadline))
		if got.State != types.StateNeedsHuman {
			t.Fatalf("state = %s, want NEEDS_HUMAN: the heartbeat is the evidence this path rests on", got.State)
		}
		if entry := lastAudit(t, st, ctx, got.ID); !strings.Contains(entry.Result, "agent heartbeat is stale") {
			t.Fatalf("audit result = %q, want the stale heartbeat named", entry.Result)
		}
	})

	t.Run("a profile selects the node and no report arrived: holds, then fails closed naming the absence", func(t *testing.T) {
		c, st, ctx := newFixture(t, time.Now())
		if err := c.SetAcceleratorRuntimeProfiles([]config.AcceleratorRuntimeProfile{testNVIDIAResetProfile()}); err != nil {
			t.Fatal(err)
		}
		if held := advance(t, c, st, ctx, incident("inc-profile-hold", insideDeadline)); held.State != types.StateVerifying {
			t.Fatalf("state = %s, want VERIFYING hold inside the deadline: the agent was told to report", held.State)
		}
		// One open incident per (node, device, class): the past-deadline
		// sibling is a different class on the same device.
		aged := incident("inc-profile-needs-human", pastDeadline)
		aged.Class = types.ClassECCDBE
		got := advance(t, c, st, ctx, aged)
		if got.State != types.StateNeedsHuman {
			t.Fatalf("state = %s, want NEEDS_HUMAN past the deadline", got.State)
		}
		if entry := lastAudit(t, st, ctx, got.ID); !strings.Contains(entry.Result, "no nvidia accelerator report for the node") {
			t.Fatalf("audit result = %q, want the missing report named", entry.Result)
		}
	})

	t.Run("a profile selects the node and the report is stale: fails closed", func(t *testing.T) {
		c, st, ctx := newFixture(t, time.Now())
		if err := c.SetAcceleratorRuntimeProfiles([]config.AcceleratorRuntimeProfile{testNVIDIAResetProfile()}); err != nil {
			t.Fatal(err)
		}
		stale := readyNVIDIAResetReport(time.Now().Add(-time.Hour).UTC(), testNVIDIAResetProfile().ProfileDigest)
		if err := st.UpsertAcceleratorReport(ctx, &stale); err != nil {
			t.Fatal(err)
		}
		got := advance(t, c, st, ctx, incident("inc-profile-stale", pastDeadline))
		if got.State != types.StateNeedsHuman {
			t.Fatalf("state = %s, want NEEDS_HUMAN", got.State)
		}
		if entry := lastAudit(t, st, ctx, got.ID); !strings.Contains(entry.Result, "accelerator report is stale") {
			t.Fatalf("audit result = %q, want the stale report named", entry.Result)
		}
	})

	t.Run("a profile selects the node and a fresh ready report lists the device: resolves at full depth", func(t *testing.T) {
		c, st, ctx := newFixture(t, time.Now())
		if err := c.SetAcceleratorRuntimeProfiles([]config.AcceleratorRuntimeProfile{testNVIDIAResetProfile()}); err != nil {
			t.Fatal(err)
		}
		report := readyNVIDIAResetReport(time.Now().UTC(), testNVIDIAResetProfile().ProfileDigest)
		if err := st.UpsertAcceleratorReport(ctx, &report); err != nil {
			t.Fatal(err)
		}
		got := advance(t, c, st, ctx, incident("inc-profile-full", insideDeadline))
		if got.State != types.StateResolved {
			t.Fatalf("state = %s, want RESOLVED", got.State)
		}
		if entry := lastAudit(t, st, ctx, got.ID); strings.Contains(entry.Result, "verified on the agent heartbeat") {
			t.Fatalf("audit result = %q: a full-depth resolution must not be labelled reduced", entry.Result)
		}
	})

	t.Run("overlapping profiles: cannot tell what the agent was told, fails closed naming the ambiguity", func(t *testing.T) {
		c, st, ctx := newFixture(t, time.Now())
		second := testNVIDIAResetProfile()
		second.Name = "nvidia-h100-reset-v2"
		second.ProfileUID = "nvidia-h100-reset-uid-2"
		if err := c.SetAcceleratorRuntimeProfiles([]config.AcceleratorRuntimeProfile{testNVIDIAResetProfile(), second}); err != nil {
			t.Fatal(err)
		}
		got := advance(t, c, st, ctx, incident("inc-ambiguous", pastDeadline))
		if got.State != types.StateNeedsHuman {
			t.Fatalf("state = %s, want NEEDS_HUMAN", got.State)
		}
		if entry := lastAudit(t, st, ctx, got.ID); !strings.Contains(entry.Result, "multiple accelerator runtime profiles match") {
			t.Fatalf("audit result = %q, want the overlapping profiles named", entry.Result)
		}
	})

	t.Run("node labels unavailable: cannot tell, fails closed", func(t *testing.T) {
		c, st, ctx := newFixture(t, time.Now())
		if err := st.UpsertNode(ctx, &types.Node{Name: node, UID: "node-uid-a", AgentLastSeen: time.Now()}); err != nil {
			t.Fatal(err)
		}
		got := advance(t, c, st, ctx, incident("inc-no-labels", pastDeadline))
		if got.State != types.StateNeedsHuman {
			t.Fatalf("state = %s, want NEEDS_HUMAN when profile selection cannot be resolved", got.State)
		}
		if entry := lastAudit(t, st, ctx, got.ID); !strings.Contains(entry.Result, "cannot tell whether a runtime profile selects the node") {
			t.Fatalf("audit result = %q, want the unresolved selection named", entry.Result)
		}
	})

	t.Run("dry-run is untouched", func(t *testing.T) {
		c, st, ctx := newFixture(t, time.Now().Add(-time.Hour))
		inc := incident("inc-dry", insideDeadline)
		inc.DryRun = true
		if got := advance(t, c, st, ctx, inc); got.State != types.StateResolved {
			t.Fatalf("state = %s, want RESOLVED on the quiet window alone", got.State)
		}
	})
}

// TestAttestedVendorsMatchTheAdapters fails the build when an accelerator
// adapter package lands without being declared attested — otherwise the new
// vendor's incidents would silently take the reduced-verification path while a
// real adapter sat there able to attest them.
func TestAttestedVendorsMatchTheAdapters(t *testing.T) {
	entries, err := os.ReadDir("../accelerator")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		vendor := types.AcceleratorVendor(e.Name())
		if !vendor.Valid() {
			continue
		}
		if !vendorRuntimeAttested(vendor) {
			t.Errorf("internal/accelerator/%s exists but %s is not in attestedRuntimeVendors; "+
				"its incidents would verify on the heartbeat alone despite having a real adapter", e.Name(), vendor)
		}
	}
	for vendor := range attestedRuntimeVendors {
		if _, err := os.Stat("../accelerator/" + string(vendor)); err != nil {
			t.Errorf("attestedRuntimeVendors claims %s, but internal/accelerator/%s does not exist; "+
				"incidents would wait for a report nothing can produce", vendor, vendor)
		}
	}
}

// TestAcceleratorEvidenceHoldEndsAtTheDeadline is the other half, and the one
// that was missing: evidence that is merely late becomes evidence that is never
// coming, and from inside the controller the two are indistinguishable.
//
// The hold above was unbounded. The shipped drain-and-reset ladder reaches the
// reset rung AFTER cordon and drain, so a node whose evidence can never arrive
// — no PCI reset on a virtualised instance, MIG enabled after the incident
// opened, a relabelled profile, a dead agent — sat cordoned and emptied of
// every tenant workload indefinitely. It stayed in EVALUATING rather than
// NEEDS_HUMAN, so it was on no alert and in nobody's queue: a deferral counter
// climbed and nothing else in the system said a word.
//
// Both siblings of this hold already bound themselves this way. This is the
// third.
func TestAcceleratorEvidenceHoldEndsAtTheDeadline(t *testing.T) {
	st, err := storesqlite.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	actuator := &resetGateActuator{}
	c := New(st, nil, nil,
		safety.NewGate(safety.Limits{MaxConcurrentRemediations: 1, MaxConcurrentReboots: 1}),
		nil, nil, actuator, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))

	ctx := context.Background()
	inc := &types.Incident{
		ID: "incident-stranded", State: types.StateEvaluating,
		Target: types.Target{Node: "node-a", GPUUUID: "GPU-a"}, DryRun: false,
		OpenedAt: time.Now().Add(-24 * time.Hour), UpdatedAt: time.Now(),
		// Held here for a day: well past any evidence deadline.
		StateChangedAt: time.Now().Add(-24 * time.Hour),
	}
	if err := st.CreateIncident(ctx, inc); err != nil {
		t.Fatal(err)
	}

	if err := c.startStep(ctx, inc, &playbook.Step{Name: "reset", Action: "agent.gpu_reset"}, "system"); err != nil {
		t.Fatalf("startStep() error = %v", err)
	}

	if inc.State != types.StateNeedsHuman {
		t.Fatalf("after a day of unavailable reset evidence the incident is in %s, not NEEDS_HUMAN; "+
			"the node stays cordoned and drained with no tenant work on it, on no alert and in "+
			"nobody's queue, for as long as the evidence never arrives", inc.State)
	}
	if actuator.calls != 0 {
		t.Fatalf("executor calls = %d: the deadline must fail CLOSED to a human, never open into "+
			"a reset the gate refused", actuator.calls)
	}
}

// TestRevokedProfileStopsAPinnedReset covers what the quiesce pin is allowed to
// remember.
//
// Stopping the DCGM host engine erases the agent's attestation from every later
// report, so the report must be pinned — that is the pin's entire reason to
// exist. The profile and the node UID were pinned beside it, and neither is
// destroyed by a quiesce: both can be read live at admission. Keeping them
// turned a snapshot of EVIDENCE into a snapshot of the controller's own
// AUTHORITY.
//
// The scenario is an operator's: resets are landing and going wrong, so they
// edit spec.acceleratorProfiles to revoke reset-device — the documented way to
// withdraw permission — and then watch a reset execute anyway on a node that
// was already quiesced, with nothing in the audit trail saying why.
func TestRevokedProfileStopsAPinnedReset(t *testing.T) {
	st, err := storesqlite.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ctx := context.Background()
	if err := st.UpsertNode(ctx, &types.Node{
		Name: "node-a", UID: "node-uid-a", Labels: map[string]string{"accelerator": "nvidia-h100"},
	}); err != nil {
		t.Fatal(err)
	}
	c := New(st, nil, nil, nil, nil, nil, nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))

	profile := testNVIDIAResetProfile()
	if err := c.SetAcceleratorRuntimeProfiles([]config.AcceleratorRuntimeProfile{profile}); err != nil {
		t.Fatal(err)
	}
	report := readyNVIDIAResetReport(time.Now().UTC(), profile.ProfileDigest)
	if err := st.UpsertAcceleratorReport(ctx, &report); err != nil {
		t.Fatal(err)
	}

	inc := &types.Incident{ID: "inc-pinned", Target: types.Target{Node: "node-a", GPUUUID: "GPU-a"}}
	target := inc.Target

	// The quiesce step pins the evidence, exactly as it does in production.
	c.pinAcceleratorEvidence(inc.ID, pinnedAcceleratorEvidence{
		node: "node-a", report: report, pinnedAt: time.Now(),
	})

	// The operator revokes reset permission while the incident is in flight.
	if err := c.SetAcceleratorRuntimeProfiles(nil); err != nil {
		t.Fatal(err)
	}

	if err := c.allowNVIDIAReset(ctx, inc, target); err == nil {
		t.Fatal("a reset was admitted from pinned evidence after its profile was revoked; the " +
			"documented way to withdraw reset permission does nothing for exactly the " +
			"incidents that are already quiesced and about to reset")
	}
}

// TestPinnedResetStillChecksNodeIdentityLive: the node-UID check exists so a
// deleted-and-recreated node cannot inherit a matching profile or capability.
// With both operands taken from the same pin it was vacuous by construction —
// it compared a value to itself.
func TestPinnedResetStillChecksNodeIdentityLive(t *testing.T) {
	st, err := storesqlite.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ctx := context.Background()
	if err := st.UpsertNode(ctx, &types.Node{
		Name: "node-a", UID: "node-uid-a", Labels: map[string]string{"accelerator": "nvidia-h100"},
	}); err != nil {
		t.Fatal(err)
	}
	c := New(st, nil, nil, nil, nil, nil, nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	profile := testNVIDIAResetProfile()
	if err := c.SetAcceleratorRuntimeProfiles([]config.AcceleratorRuntimeProfile{profile}); err != nil {
		t.Fatal(err)
	}
	report := readyNVIDIAResetReport(time.Now().UTC(), profile.ProfileDigest)
	if err := st.UpsertAcceleratorReport(ctx, &report); err != nil {
		t.Fatal(err)
	}

	inc := &types.Incident{ID: "inc-recreated", Target: types.Target{Node: "node-a", GPUUUID: "GPU-a"}}
	c.pinAcceleratorEvidence(inc.ID, pinnedAcceleratorEvidence{
		node: "node-a", report: report, pinnedAt: time.Now(),
	})

	// The autoscaler replaces the machine: same name, new object.
	if err := st.UpsertNode(ctx, &types.Node{
		Name: "node-a", UID: "node-uid-REPLACED", Labels: map[string]string{"accelerator": "nvidia-h100"},
	}); err != nil {
		t.Fatal(err)
	}

	if err := c.allowNVIDIAReset(ctx, inc, inc.Target); err == nil {
		t.Fatal("a reset pinned against the previous node object was admitted on its " +
			"replacement; the identity check compared the pin to itself")
	}
}
