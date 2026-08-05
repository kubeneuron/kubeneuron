package safety

import (
	"testing"
	"time"

	"github.com/kubeneuron/kubeneuron/pkg/types"
)

// R4: on leader failover the new leader rebuilds gate occupancy from durable
// mid-remediation incidents, so the concurrency and reboot caps hold across
// the failover instead of transiently undercounting to zero. OccupyRemediation
// reserves the target slot exactly like Allow; OccupyStep reserves the
// reboot-class slot of a recovered EXECUTING step.
func TestOccupyHoldsConcurrencyAndRebootCaps(t *testing.T) {
	g := NewGate(Limits{MaxConcurrentRemediations: 1, MaxConcurrentReboots: 1})

	// A durable EXECUTING reboot on node-a, re-seeded on leadership acquisition.
	g.OccupyRemediation(target("node-a"))
	g.OccupyStep(target("node-a"), types.ActionReboot)

	if err := g.Allow(target("node-b"), types.ActionGPUReset); err == nil {
		t.Fatal("remediation cap must hold: the seeded node-a slot must be counted")
	}
	if err := g.Allow(target("node-b"), types.ActionReboot); err == nil {
		t.Fatal("reboot cap must hold: the seeded node-a reboot must be counted")
	}

	// The recovered step finishes (the incident leaves EXECUTING): the reboot
	// slot frees, the remediation slot stays held until terminalization.
	g.StepDone(target("node-a"), types.ActionReboot, 0)
	if err := g.Allow(target("node-b"), types.ActionReboot); err == nil {
		t.Fatal("remediation cap must still hold while node-a is mid-remediation")
	}
	if err := g.AllowHeld(target("node-a"), types.ActionReboot); err != nil {
		t.Fatalf("node-a's own next step must be admitted without the remediation cap: %v", err)
	}
	g.StepDone(target("node-a"), types.ActionReboot, 0)

	// The incident terminalizes: the remediation slot frees capacity.
	g.ReleaseRemediation(target("node-a"))
	if err := g.Allow(target("node-b"), types.ActionReboot); err != nil {
		t.Fatalf("after the remediation is released, node-b must be admitted: %v", err)
	}
}

// The remediation cap counts remediations, not steps: a target that finished a
// step but has not terminalized still occupies its slot, and its own later
// steps are admitted through AllowHeld while other targets stay denied.
func TestRemediationSlotSpansSteps(t *testing.T) {
	g := NewGate(Limits{MaxConcurrentRemediations: 1, MaxConcurrentReboots: 1})

	if err := g.Allow(target("node-a"), types.ActionGPUReset); err != nil {
		t.Fatalf("first step: %v", err)
	}
	g.StepDone(target("node-a"), types.ActionGPUReset, 0)

	// Between node-a's steps, another target must NOT slip in.
	if err := g.Allow(target("node-b"), types.ActionGPUReset); err == nil {
		t.Fatal("node-b admitted between node-a's steps: the cap capped steps, not remediations")
	}
	// node-a's own next step proceeds.
	if err := g.AllowHeld(target("node-a"), types.ActionCollectBundle); err != nil {
		t.Fatalf("held target's next step: %v", err)
	}
	g.StepDone(target("node-a"), types.ActionCollectBundle, 0)

	g.ReleaseRemediation(target("node-a"))
	if err := g.Allow(target("node-b"), types.ActionGPUReset); err != nil {
		t.Fatalf("after release: %v", err)
	}
}

// AllowHeld still applies the per-step checks: pause, cooldown, and the
// reboot cap deny a held target's next step exactly as they deny a first step.
func TestAllowHeldAppliesStepChecks(t *testing.T) {
	g := NewGate(Limits{MaxConcurrentRemediations: 2, MaxConcurrentReboots: 1})

	if err := g.Allow(target("node-a"), types.ActionGPUReset); err != nil {
		t.Fatalf("node-a first step: %v", err)
	}
	if err := g.Allow(target("node-b"), types.ActionReboot); err != nil {
		t.Fatalf("node-b reboot: %v", err)
	}
	// node-a's reboot step must respect the reboot cap held by node-b.
	if err := g.AllowHeld(target("node-a"), types.ActionReboot); err == nil {
		t.Fatal("reboot cap must apply to a held target's step")
	}
	g.Pause()
	if err := g.AllowHeld(target("node-a"), types.ActionCollectBundle); err == nil {
		t.Fatal("pause must apply to a held target's step")
	}
	g.Resume()
	g.RecordCooldown(target("node-a"), types.ActionCollectBundle, time.Minute)
	if err := g.AllowHeld(target("node-a"), types.ActionCollectBundle); err == nil {
		t.Fatal("cooldown must apply to a held target's step")
	}
}
