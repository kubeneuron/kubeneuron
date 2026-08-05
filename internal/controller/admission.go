package controller

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/kubeneuron/kubeneuron/internal/action"
	"github.com/kubeneuron/kubeneuron/internal/cloud"
	"github.com/kubeneuron/kubeneuron/internal/config"
	"github.com/kubeneuron/kubeneuron/internal/platform"
	"github.com/kubeneuron/kubeneuron/internal/playbook"
	"github.com/kubeneuron/kubeneuron/internal/store"
	"github.com/kubeneuron/kubeneuron/pkg/types"
)

// This file is the admission seam: everything consulted BEFORE a step may
// run. Gate slot lifecycle (remediation slots held to terminalization, the
// leadership rebuild), destructive blast-radius confinement, and the
// accelerator capability/evidence gates. Nothing here executes anything.

// recoveredSlot records a gate reservation seeded from a durable EXECUTING
// incident so it can be released, once, when that incident is re-driven.
type recoveredSlot struct {
	target types.Target
	action types.ActionType
}

// RebuildGateOccupancy re-seeds the safety gate's concurrency occupancy from
// the durable EXECUTING incidents before the first reconcile tick, so the
// MaxConcurrentRemediations/MaxConcurrentReboots caps hold across a leader
// failover. When a leader dies, agents may still be executing leased
// destructive actions the previous leader admitted; a new leader that started
// with empty slots would admit fresh remediations past the cap until
// recoverOrphanedExecution re-admitted each incident one reconcile at a time.
// Each seeded slot is released by releaseRecoveredSlot the first time its
// incident leaves EXECUTING (recovery re-admits it through the normal Allow
// path, or closes it), so the occupancy is a hand-off, not a leak.
func (c *Controller) RebuildGateOccupancy(ctx context.Context) error {
	if c.gate == nil {
		return nil
	}
	// Ownership is the durable bit on the incident row, not an inference from
	// state/StepIndex: escalate() resets StepIndex to 0 mid-remediation, so
	// the old inference silently dropped an escalated incident's slot across
	// failover. Every non-halted state is scanned — a slot-holding incident
	// can legally sit in OPEN/OBSERVING after an engine hot-swap unbound its
	// playbook, and it keeps its slot there until it halts.
	incidents, err := c.store.ListIncidents(ctx, store.IncidentFilter{
		States: []types.IncidentState{
			types.StateOpen, types.StateObserving,
			types.StateEvaluating, types.StateAwaitingApproval,
			types.StateExecuting, types.StateVerifying,
		},
	})
	if err != nil {
		return err
	}
	engine := c.currentEngine()
	for _, inc := range incidents {
		if !inc.RemediationSlotHeld {
			continue
		}
		c.gate.OccupyRemediation(inc.Target)
		if inc.State == types.StateExecuting {
			// The previous leader also held this incident's per-step (reboot
			// class) slot; reserve it too until recovery moves the incident out
			// of EXECUTING. This stays a rebuilt in-memory hand-off: its action
			// class is derived from the engine at rebuild time (persisting it
			// would go stale across hot-swaps) and its lifetime is one recovery
			// pass.
			var action types.ActionType
			if engine != nil {
				if step, done, stepErr := engine.NextStep(inc); stepErr == nil && !done && step != nil {
					action = gateAction(step)
				}
			}
			c.gate.OccupyStep(inc.Target, action)
			c.recoveredMu.Lock()
			c.recoveredSlots[inc.ID] = recoveredSlot{target: inc.Target, action: action}
			c.recoveredMu.Unlock()
		}
		c.log.Info("re-seeded safety gate slot from a durable mid-remediation incident",
			"incident", inc.ID, "state", inc.State, "node", inc.Target.Node, "gpu", inc.Target.GPUUUID)
	}
	return nil
}

// releaseRecoveredSlot hands back the per-step slot seeded by
// RebuildGateOccupancy. It is idempotent: only the first call for an incident
// releases the reservation, so the later normal EXECUTING exits are no-ops.
// The incident's remediation slot is not touched here — it lives until the
// incident terminalizes (releaseHeldSlot).
func (c *Controller) releaseRecoveredSlot(incidentID string) {
	c.recoveredMu.Lock()
	slot, ok := c.recoveredSlots[incidentID]
	if ok {
		delete(c.recoveredSlots, incidentID)
	}
	c.recoveredMu.Unlock()
	if ok && c.gate != nil {
		c.gate.StepDone(slot.target, slot.action, 0)
	}
}

// errResetTargetUnattributed marks a reset step whose target carries no GPU
// UUID. It is a reported fact (a permanent infeasibility), not missing evidence
// that could still arrive, so it must escalate rather than hold: an empty UUID
// can never be filled in by a later report.
var errResetTargetUnattributed = errors.New("physical NVIDIA reset requires a GPU UUID target, but this incident is unattributed (empty GPU UUID)")

// confinementResult classifies a destructive step against the declared
// destructiveExecution blast radius.
type confinementResult int

const (
	// confinementAllowed: proceed. Dry-run, no selector configured, a
	// non-destructive step, or a node proven inside the selector.
	confinementAllowed confinementResult = iota
	// confinementOutOfScope: the node's labels resolved and do NOT match the
	// selector. A confirmed scope violation — fail closed to a human, never
	// execute.
	confinementOutOfScope
	// confinementUnresolved: the node's labels could not be resolved right now
	// (a transient apiserver/ListNodes blip, or the node is momentarily absent
	// from inventory). This is missing evidence, not a confirmed violation, so
	// hold and retry — the step never executes while scope is unknown.
	confinementUnresolved
)

// destructiveStepConfinement classifies whether a destructive controller-side
// platform step may run against its target node given the declared
// destructiveExecution blast radius. It never returns "allowed" for a node it
// cannot prove is in scope: an unresolvable label lookup is reported as
// unresolved (hold-and-retry) and a resolved non-match as out-of-scope
// (quarantine). The destructive classification comes from the action registry,
// not a hand-maintained map. Dry-run incidents and installs with no selector
// are never in scope for the check (nothing executes for them).
func (c *Controller) destructiveStepConfinement(ctx context.Context, inc *types.Incident, step *playbook.Step) (string, confinementResult) {
	if inc.DryRun {
		return "", confinementAllowed
	}
	def, ok := action.ByWire(step.Action)
	if !ok || !def.Destructive {
		return "", confinementAllowed
	}
	selector := c.destructiveNodeSelector(ctx)
	if len(selector) == 0 {
		return "", confinementAllowed // no confinement configured
	}
	labels, err := c.nodeLabelsForConfinement(ctx, inc.Target.Node)
	if err != nil {
		return fmt.Sprintf("cannot confirm destructive step %s on node %s is within the declared spec.safety.destructiveExecution.nodeSelector: %v",
			step.Action, inc.Target.Node, err), confinementUnresolved
	}
	if !labelsMatchSelector(selector, labels) {
		return fmt.Sprintf("refusing destructive step %s on node %s: it is outside the declared spec.safety.destructiveExecution.nodeSelector (blast radius), which the controller path must honor just as the agent path does",
			step.Action, inc.Target.Node), confinementOutOfScope
	}
	return "", confinementAllowed
}

// labelsMatchSelector reports whether every selector key/value is present on
// the node labels (Kubernetes matchLabels semantics).
func labelsMatchSelector(selector, labels map[string]string) bool {
	for k, v := range selector {
		if labels[k] != v {
			return false
		}
	}
	return true
}

func (c *Controller) allowAcceleratorStep(ctx context.Context, inc *types.Incident, step *playbook.Step) error {
	// Dry-run executes no side effect and is intentionally usable without a
	// hardware profile, so operators can see the planned ladder before
	// qualifying a node/runtime combination.
	if inc.DryRun {
		return nil
	}
	// The capability gate is a registry fact now, not a hard-coded
	// agent.gpu_reset string match: any future gated action is a registry entry
	// with CapabilityGate set, not another special case here.
	def, ok := action.ByWire(step.Action)
	if !ok || def.CapabilityGate != action.CapabilityNVIDIAReset {
		return nil
	}
	return c.allowNVIDIAReset(ctx, inc, inc.Target)
}

// allowNVIDIAReset combines the configured server profile with a fresh,
// ready, agent-owned observation. Neither input alone is authority: the
// profile selects the intended node fleet and digest, while the report proves
// the current physical device and unpartitioned topology. This is deliberately
// narrower than a future general action gate so unmodelled agent actions do
// not accidentally inherit a reset capability.
// allowNVIDIAReset admits a physical reset only on current, node-bound,
// profile-matched runtime evidence.
//
// Evidence pinned by a quiesce step is accepted in place of the live report:
// stopping the DCGM host engine is what makes the reset possible at all, and it
// also erases the attestation from every later report. The pin is the same
// evidence, validated by this same gate a moment earlier, and it is still
// subject to verifyEvidenceMaxAge. Every other check is re-run against it.
func (c *Controller) allowNVIDIAReset(ctx context.Context, inc *types.Incident, target types.Target) error {
	if pin, ok := c.takePinnedAcceleratorEvidence(inc.ID, time.Now()); ok {
		if pin.node != target.Node {
			return fmt.Errorf("pinned accelerator evidence belongs to node %q, not %q", pin.node, target.Node)
		}
		return checkNVIDIAResetEvidence(pin.profile, &pin.report, pin.nodeUID, target)
	}
	_, _, _, err := c.acceleratorEvidenceForReset(ctx, target)
	return err
}

// acceleratorEvidenceForReset resolves and validates the live evidence for a
// physical reset, returning it so a caller can pin it.
func (c *Controller) acceleratorEvidenceForReset(ctx context.Context, target types.Target) (*types.AgentAcceleratorReport, string, *config.AcceleratorRuntimeProfile, error) {
	if target.Node == "" {
		return nil, "", nil, fmt.Errorf("physical NVIDIA reset requires a node target")
	}
	if target.GPUUUID == "" {
		// A missing GPU UUID is not missing evidence that could still arrive: the
		// incident is structurally unattributed (kmsg PCI->GPU resolution failed on
		// a wedged nvidia-smi). Return a distinct, permanent error so the caller
		// escalates to a rung that needs no device target instead of holding in
		// EVALUATING and re-denying the reset on every tick.
		return nil, "", nil, errResetTargetUnattributed
	}
	// Node names and labels can change with a Kubernetes delete/recreate. Read
	// both from one current inventory object before considering a report, so a
	// previous node object cannot inherit a matching profile or capability.
	node, ok := c.acceleratorNode(ctx, target.Node)
	if !ok || node.UID == "" {
		return nil, "", nil, fmt.Errorf("node UID is unavailable for accelerator report binding")
	}
	if node.Labels == nil {
		return nil, "", nil, fmt.Errorf("node labels are unavailable for runtime profile selection")
	}
	profiles := c.runtimeConfig(ctx).AcceleratorProfiles
	profile, err := (config.Config{AcceleratorProfiles: profiles}).ResolveAcceleratorRuntimeProfile(node.Labels, types.AcceleratorVendorNVIDIA)
	if err != nil {
		return nil, "", nil, err
	}
	reports, ok := c.store.(store.AcceleratorReportStore)
	if !ok {
		return nil, "", nil, fmt.Errorf("accelerator report store is not configured")
	}
	report, err := reports.GetAcceleratorReport(ctx, target.Node, types.AcceleratorVendorNVIDIA)
	if err != nil {
		return nil, "", nil, fmt.Errorf("load NVIDIA accelerator report: %w", err)
	}
	if err := checkNVIDIAResetEvidence(profile, report, node.UID, target); err != nil {
		return nil, "", nil, err
	}
	return report, node.UID, profile, nil
}

// checkNVIDIAResetEvidence is the whole evidence test, applied identically to a
// live report and to one pinned by a quiesce step.
func checkNVIDIAResetEvidence(profile *config.AcceleratorRuntimeProfile, report *types.AgentAcceleratorReport, nodeUID string, target types.Target) error {
	if report.NodeUID != nodeUID {
		return fmt.Errorf("accelerator report node UID does not match current node")
	}
	if err := profile.CheckAction(time.Now(), *report,
		types.AcceleratorActionResetDevice, types.AcceleratorScopePhysicalDevice); err != nil {
		return err
	}
	for _, device := range report.Devices {
		if device.ID == target.GPUUUID && device.Kind == types.AcceleratorDevicePhysical && device.Family == types.AcceleratorFamilyGPU {
			return nil
		}
	}
	return fmt.Errorf("report does not contain targeted physical NVIDIA GPU %q", target.GPUUUID)
}

// gateAction maps a step to the action type the safety gate reasons about,
// so reboot-class steps hit the reboot concurrency cap. The class is a
// registry fact; a step whose action is not in the registry gates on its raw
// wire string (it cannot be reboot-class — reboot classes are registry
// entries — so the raw string is a safe, conservative identity).
func gateAction(step *playbook.Step) types.ActionType {
	if def, ok := action.ByWire(step.Action); ok {
		return def.GateAction()
	}
	return types.ActionType(step.Action)
}

// playbookCooldownAction is the pseudo-action under which a completed
// playbook's cooldown is recorded on the gate.
func playbookCooldownAction(name string) types.ActionType {
	return types.ActionType("playbook:" + name)
}

// refuseUnrecyclableNode reports whether the incident's next step is a
// recycle_node whose target instance provably cannot be recycled — the
// per-instance verdict behind the provider-scoped capability. Capabilities
// gate the playbook at compile time, but an autoscaling-group member fails
// its group's health check the moment it stops and is terminated mid-recycle,
// so before this check the gap surfaced as a step timeout after a human had
// already approved the step. A definitive verdict escalates the ladder (the
// next rung is typically ReplaceNode, which is exactly what the node group
// wants); a transient lookup failure changes nothing — RecycleNode re-checks
// before issuing the stop, so the worst case is the old crisp step failure,
// never a blind stop.
func (c *Controller) refuseUnrecyclableNode(ctx context.Context, inc *types.Incident, step *playbook.Step) (string, bool) {
	// Registry fact, not a wire-string match: any action that reinitializes the
	// instance in place needs the same per-instance viability verdict.
	def, ok := action.ByWire(step.Action)
	if inc.DryRun || !ok || def.CloudPrimitive != action.CloudPrimitiveReinitializeInPlace {
		return "", false
	}
	recycler, ok := c.platform.(platform.NodeRecycler)
	if !ok || !recycler.CloudRecyclingConfigured() {
		return "", false // recycleNodeStep owns the no-provider failure path
	}
	err := recycler.CheckRecycleNode(ctx, inc.Target.Node)
	if err == nil {
		return "", false
	}
	if errors.Is(err, cloud.ErrRecycleNotViable) {
		return "refusing recycle_node before requesting approval: " + err.Error(), true
	}
	c.log.Debug("recycle viability could not be established; proceeding (RecycleNode re-checks before stopping)",
		"incident", inc.ID, "node", inc.Target.Node, "err", err)
	return "", false
}

// armingAdmission is refuseUnarmedAgent's verdict on one step.
type armingAdmission int

const (
	armingProceed armingAdmission = iota
	// armingHold: the node is inside the served blast radius but its agent's
	// fresh registration still says unarmed — adoption is in flight. The walk
	// holds (no transition, re-checked next pass) instead of asking a human
	// to approve a step the node cannot execute yet.
	armingHold
	armingRefuse
)

// armingPropagationGrace bounds the arming-in-flight hold: four agent
// registration ticks at the agent's default 30s interval, comfortably inside
// verifyEvidenceMaxAge so the declaration stays "fresh" for the whole window.
// An agent that still freshly registers unarmed after this long, on a node
// the selector says to arm, is not propagating — it CANNOT arm.
const armingPropagationGrace = 2 * time.Minute

// refuseUnarmedAgent judges an agent-destructive step against the node's
// arming declaration. It refuses when the agent has DEFINITIVELY declared
// itself unarmed: the registration explicitly said "unarmed" AND is fresh
// (AgentLastSeen within verifyEvidenceMaxAge — the same bound the verifier
// already trusts heartbeats for, and ten registration ticks). Everything else
// — unknown (an old agent or a v1 registration), no node row, a stale
// declaration (the pod may have been replaced by an armed one after a
// selector change) — is transient and proceeds: the agent executor
// re-refuses at dispatch, so the worst case is a crisp step failure, never a
// silent bypass. This check is advisory-early — a human must not be asked to
// approve a step the node will provably refuse — the executor's own boundary
// remains the enforcement.
func (c *Controller) refuseUnarmedAgent(ctx context.Context, inc *types.Incident, step *playbook.Step) (string, armingAdmission) {
	if inc.DryRun {
		return "", armingProceed
	}
	def, ok := action.ByWire(step.Action)
	if !ok || !def.AgentDestructive {
		return "", armingProceed
	}
	node, err := c.store.GetNode(ctx, inc.Target.Node)
	if err != nil || node == nil {
		return "", armingProceed // transient: no inventory row right now
	}
	if node.AgentArming != types.AgentArmingUnarmed {
		return "", armingProceed // armed, or unknown (old agent) — proceed
	}
	if node.AgentLastSeen.IsZero() || time.Since(node.AgentLastSeen) > verifyEvidenceMaxAge {
		return "", armingProceed // stale declaration: not definitive
	}
	// With controller-SERVED arming, a freshly-booted agent on an in-scope
	// node reports unarmed until it adopts the served answer — normally
	// within one registration tick. Hold, bounded by the grace, rather than
	// park: a human must not be asked to approve a step the node cannot
	// execute yet. Past the grace the declaration is a verdict, not
	// propagation — the agent refuses served arming (non-real GPU driver,
	// static unarmed pin) and can NEVER execute this step — so escalate now
	// instead of looping approve→executor-refusal→escalate. The hold clock
	// is StateChangedAt: it is not bumped while holding, so the grace
	// accumulates across passes and resets only on a real transition.
	if c.servedArming(ctx, inc.Target.Node) == types.AgentArmingArmed {
		if time.Since(inc.StateChangedAt) <= armingPropagationGrace {
			return "", armingHold
		}
		return fmt.Sprintf("refusing %s before requesting approval: node %s is inside spec.safety.destructiveExecution.nodeSelector but its agent has kept registering unarmed for over %s — it cannot adopt served arming (non-real GPU driver or a static pin); fix the agent or route the ladder to a platform-side rung",
			step.Action, inc.Target.Node, armingPropagationGrace), armingRefuse
	}
	return fmt.Sprintf("refusing %s before requesting approval: node %s's agent registered as unarmed and the node is outside spec.safety.destructiveExecution.nodeSelector; label it into the blast radius or route the ladder to a platform-side rung",
		step.Action, inc.Target.Node), armingRefuse
}

// playbookNeedsArmedAgent reports whether ANY step of the playbook is
// agent-destructive — the playbook-scope companion of refuseUnarmedAgent,
// consulted before the first disruptive step so a ladder does not cordon and
// drain a node only to die at its reboot rung.
func playbookNeedsArmedAgent(book *playbook.Playbook) bool {
	for _, step := range book.Steps {
		if def, ok := action.ByWire(step.Action); ok && def.AgentDestructive {
			return true
		}
	}
	return false
}
