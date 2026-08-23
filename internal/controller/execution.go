package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/kubeneuron/kubeneuron/internal/action"
	"github.com/kubeneuron/kubeneuron/internal/metrics"
	"github.com/kubeneuron/kubeneuron/internal/notify"
	"github.com/kubeneuron/kubeneuron/internal/platform"
	"github.com/kubeneuron/kubeneuron/internal/playbook"
	"github.com/kubeneuron/kubeneuron/pkg/types"
)

// This file is the execution seam: dispatching an admitted step to the
// platform, the agent, verification, or notification, and routing its outcome
// (advance, escalate, quarantine). Admission decisions are made before
// startStep in admission.go; the state walk that calls it lives in
// reconcile.go.

// startStep reserves a gate slot, moves the incident to EXECUTING, and runs
// the step in its own goroutine.
func (c *Controller) startStep(ctx context.Context, inc *types.Incident, step *playbook.Step, actor string) error {
	// A destructive controller-side platform step must stay inside the declared
	// blast radius. On an Enabled install the agent path is already confined to
	// spec.safety.destructiveExecution.nodeSelector; the controller path (cordon,
	// drain, evict, recycle/replace) was not, so a non-dry-run incident on a node
	// outside the selector could cordon and drain it and one approval from
	// terminating it. Fail closed to NEEDS_HUMAN with an audited reason instead.
	// Pinned HERE, before the confinement check, not later beside the audit.
	//
	// Confinement reads the execution MODE while the blast radius comes from
	// the pinned runtime config, and the reload writes those two in separate
	// statements — so on a DryRun -> Enabled reload there is a window where the
	// gate says Enabled and the selector is still the empty one DryRun
	// compiled, and an empty selector is read as "no confinement configured"
	// and allowed. Deciding the mode once, above both reads, closes the half of
	// that window this function owns: a step cannot be waved through as a
	// simulation and then executed for real by the same call.
	ctx, simulate := c.pinSimulate(ctx, inc)
	switch reason, res := c.destructiveStepConfinement(ctx, inc, step); res {
	case confinementOutOfScope:
		// Confirmed outside the blast radius: fail closed to a human. Never execute.
		c.deferStep(inc, step, metrics.DeferConfinement)
		return c.quarantine(ctx, inc, reason)
	case confinementUnresolved:
		c.deferStep(inc, step, metrics.DeferConfinement)
		// The node's labels could not be resolved right now (a transient
		// platform/apiserver blip). This is missing evidence, not a confirmed
		// scope violation, so hold in EVALUATING and retry — matching the
		// accelerator evidence gate below — rather than turning a passing blip
		// into a terminal quarantine. Fail closed only if resolution stays
		// impossible past the bounded deadline.
		if time.Since(inc.StateChangedAt) < c.verifyEvidenceDeadline(ctx) {
			c.log.Info("destructive-step confinement: node labels unavailable, holding",
				"incident", inc.ID, "node", inc.Target.Node, "step", step.Name, "reason", reason)
			return nil
		}
		return c.quarantine(ctx, inc, reason+"; still unresolvable past the confinement deadline")
	}
	// Capability evidence is a precondition, not an execution failure. If a
	// reset cannot be admitted because its runtime profile/report is absent,
	// stale, or unsafe, leave the incident in EVALUATING. Escalating from a
	// withheld reset into a reboot would turn missing evidence into a more
	// destructive action, which is the opposite of fail-closed remediation.
	if err := c.allowAcceleratorStep(ctx, inc, step); err != nil {
		if errors.Is(err, errResetTargetUnattributed) {
			// Structural and permanent, not missing evidence: an empty GPU UUID can
			// never be filled in by a later report, so a per-device reset here can
			// never succeed. Fail closed to a human with the reason rather than
			// re-deny this reset on every tick after the node was cordoned/drained.
			return c.quarantine(ctx, inc, err.Error())
		}
		// Everything else here is missing/stale/mismatched evidence: the step is
		// HELD, so the device keeps running whatever it is running. That is a
		// deferral, not a failure, and it is the most common reason a reset does
		// not happen.
		c.deferStep(inc, step, metrics.DeferAcceleratorEvidence)
		metrics.GateDenials.Inc()
		// But the hold has to END somewhere, and it did not.
		//
		// Both siblings in this function bound their equivalent wait — the
		// confinement hold above, and the VERIFYING evidence hold in
		// reconcile.go — because evidence that is merely late becomes evidence
		// that is never coming, and the two are indistinguishable from here.
		// This one held forever, and the shipped drain-and-reset ladder reaches
		// the reset rung AFTER cordon and drain. So a node whose evidence can
		// never arrive — no PCI reset on a virtualised instance, MIG enabled
		// after the incident opened, a relabelled profile, a dead agent — sat
		// cordoned and emptied of work indefinitely, in EVALUATING rather than
		// NEEDS_HUMAN, on no alert and in nobody's queue. A metric counter
		// climbed and nothing else said anything.
		//
		// Past the deadline this is not missing evidence any more, it is an
		// answer: this reset cannot be admitted here. Fail closed to a human,
		// who can then uncordon the node or fix the profile. Inside the
		// deadline the hold is unchanged — refusing to escalate to a bigger
		// hammer on absent evidence is right, and stays.
		if time.Since(inc.StateChangedAt) < c.verifyEvidenceDeadline(ctx) {
			c.log.Info("accelerator capability gate denied step, will hold",
				"incident", inc.ID, "step", step.Name, "reason", err.Error())
			return nil
		}
		return c.quarantine(ctx, inc,
			"accelerator reset evidence still unavailable past the evidence deadline: "+err.Error())
	}
	action := gateAction(step)
	// The first admitted step acquires the target's remediation slot, held from
	// here until the incident halts; later steps of the same remediation pass
	// only the per-step checks. Acquiring per step and releasing between steps
	// made MaxConcurrentRemediations a cap on concurrent steps: N incidents
	// could interleave, each holding a slot only while a step ran. Ownership is
	// the DURABLE bit on the incident row — persisted atomically with the
	// EXECUTING transition below — so a leader failover rebuilds occupancy from
	// truth instead of inferring it (the inference missed escalated incidents,
	// whose StepIndex resets to 0 mid-remediation).
	alreadyHeld := inc.RemediationSlotHeld
	var admit error
	if alreadyHeld {
		admit = c.gate.AllowHeld(inc.Target, action)
	} else {
		admit = c.gate.Allow(inc.Target, action)
	}
	if admit != nil {
		c.deferForGateDenial(inc, step, admit)
		metrics.GateDenials.Inc()
		c.log.Info("safety gate denied step, will retry",
			"incident", inc.ID, "step", step.Name, "reason", admit.Error())
		return nil // denial is not an error: hold position and retry
	}
	if !alreadyHeld {
		inc.RemediationSlotHeld = true // persisted by the transition below
	}
	// The audit records what this step WILL do — the decision pinned above,
	// not the flag stamped when the incident opened. A ladder that simulates
	// after a mid-flight switch to DryRun must not read later as remediation.
	if err := c.transition(ctx, inc, types.StateExecuting, actor, step.Name,
		auditStepResult(step.Action, simulate), step.Params); err != nil {
		c.gate.StepDone(inc.Target, action, 0)
		if !alreadyHeld {
			// The transition never committed, so the bit was never persisted;
			// undo the in-memory acquisition rather than waiting for a
			// halting transition this incident may never take from here.
			inc.RemediationSlotHeld = false
			c.gate.ReleaseRemediation(inc.Target)
		}
		return err
	}
	c.setInFlight(inc.ID, true)
	// The step goroutine pins the engine that ADMITTED the step: it may run
	// for many minutes, and re-reading the live engine after completion would
	// compute done/not-done against a generation that never admitted this
	// step. The next advance re-reads fresh.
	engine := c.runtimeConfig(ctx).Engine
	go func() {
		defer c.setInFlight(inc.ID, false)
		defer c.gate.StepDone(inc.Target, action, 0)
		c.runStep(ctx, engine, inc, step)
	}()
	return nil
}

// runStep executes one step and applies the outcome: next step, verification,
// escalation, or quarantine. Runs outside the reconcile goroutine, against
// the engine snapshot that admitted the step.
func (c *Controller) runStep(ctx context.Context, engine *playbook.Engine, inc *types.Incident, step *playbook.Step) {
	result, err := c.executeStep(ctx, inc, step)

	output := ""
	if result != nil {
		output = result.Output
	}
	if err != nil {
		metrics.StepsExecuted.WithLabelValues("failed").Inc()
		// An idle guard that refuses is the device saying "I am still working".
		// The rung it stands in front of does not run, which is protection, not
		// a malfunction — count it as such before the ladder moves on.
		// The control decision asks only "was this a guard?" — see
		// idleGuardStopped. The metric asks the narrower question.
		stopped := idleGuardStopped(step)
		c.recordIdleRefusal(inc, step, result)
		c.log.Warn("step failed", "incident", inc.ID, "step", step.Name, "err", err)
		if auditErr := c.appendAudit(ctx, inc, "system", step.Name, "FAILED: "+err.Error()); auditErr != nil {
			c.log.Error("audit append failed", "incident", inc.ID, "err", auditErr)
		}
		if stopped {
			// Do NOT escalate. Escalation switches to the failure playbook,
			// whose rungs are by construction bigger hammers than the one that
			// just got stopped — so a guard that refused BECAUSE live work is
			// on the device would be answered by reaching for something more
			// destructive. That inverts the guard into a trigger.
			//
			// Whether to end somebody's running job is a human's call, so the
			// incident is handed over with the holders named in the audit
			// trail. The node keeps whatever cordon the ladder already applied,
			// so nothing new lands on it while the decision is pending.
			reason := fmt.Sprintf("%s did not clear the device (%v); automation stops here "+
				"rather than escalating to a more destructive step", step.Name, err)
			if qErr := c.quarantine(ctx, inc, reason); qErr != nil {
				c.log.Error("quarantine after an idle refusal failed", "incident", inc.ID, "err", qErr)
			}
			return
		}
		if escErr := c.escalate(ctx, inc, fmt.Sprintf("step %s failed: %v", step.Name, err)); escErr != nil {
			c.log.Error("escalation failed", "incident", inc.ID, "err", escErr)
		}
		return
	}

	// The label must describe what this step DID — the decision pinned when it
	// was admitted, not the gate as it stands now. Re-reading here reported a
	// step that really ran as simulated whenever the operator flipped the mode
	// while the agent held the action.
	if c.simulating(ctx, inc) {
		metrics.StepsExecuted.WithLabelValues("dry_run").Inc()
	} else {
		metrics.StepsExecuted.WithLabelValues("ok").Inc()
	}
	inc.StepIndex++
	_, done, nextErr := engine.NextStep(inc)
	next := types.StateEvaluating
	note := "step complete; evaluating next step"
	if nextErr == nil && done {
		next = types.StateVerifying
		note = "playbook complete; verifying quiet window"
	}
	if err := c.transition(ctx, inc, next, "system", step.Name, firstNonBlank(output, note), step.Params); err != nil {
		c.log.Error("post-step transition failed", "incident", inc.ID, "err", err)
		return
	}
	if notifyErr := c.notify(ctx, notify.NotifyEvent{
		Kind: notify.EventActionTaken, Incident: inc,
		Message: fmt.Sprintf("step %s: %s", step.Name, firstNonBlank(output, "done")),
	}); notifyErr != nil {
		c.log.Warn("notification failed", "incident", inc.ID, "err", notifyErr)
	}
}

// pinnedSimulateKey carries ONE step's simulate-or-execute decision.
//
// The gate reloads in place, so reading it three times per step — once for the
// audit row, once to decide, once to label the metric — reads three different
// answers across a window that is the whole step, minutes for a reboot. Both
// disagreements were observable: a step dispatched for real while the operator
// flipped to DryRun mid-flight was counted as simulated, so the very counter
// used to confirm the stop had taken effect reported a real fleet change as a
// no-op; and the reverse flip made the audit say "simulating" for a step that
// then really rebooted a node, which the recovery report reads as
// observed-only. The audit trail is the only evidence a repair happened, and
// this made it lie about a destructive act.
//
// It mirrors how the engine and the runtime config are already pinned for the
// life of a step: decide once, at admission, and carry the decision.
type pinnedSimulateKey struct{}

// pinSimulate evaluates the decision once and returns it with a context that
// carries it to the step goroutine.
func (c *Controller) pinSimulate(ctx context.Context, inc *types.Incident) (context.Context, bool) {
	simulate := c.effectiveDryRun(inc)
	return context.WithValue(ctx, pinnedSimulateKey{}, simulate), simulate
}

// simulating reports this step's pinned decision, falling back to the live
// gate for callers outside a step (the janitor's own actuator calls).
func (c *Controller) simulating(ctx context.Context, inc *types.Incident) bool {
	if pinned, ok := ctx.Value(pinnedSimulateKey{}).(bool); ok {
		return pinned
	}
	return c.effectiveDryRun(inc)
}

// effectiveDryRun answers whether this step must be simulated, from the
// incident AND the gate that is live right now.
//
// inc.DryRun alone is not that answer. It is stamped once, in openIncidentTx,
// and never revisited — which was the only correct reading while configuration
// arrived by rolling the Deployment, because a mode change restarted the
// process and no incident outlived it. Configuration now reloads in place, so
// an operator who sets executionMode: DryRun to STOP damage changes what the
// gate says while every already-open incident keeps executing for real.
//
// That is bad on its own and worse in combination: the operator compiles the
// destructive node selector only for an Enabled install, so the same flip
// empties the selector, and an empty selector is read as "no confinement
// configured" and allowed. The documented way to stop a runaway remediation
// therefore removed the blast radius from every ladder already in flight — a
// step refused a second earlier for being outside spec.safety.
// destructiveExecution.nodeSelector became allowed on any node in the cluster.
//
// Reading the live gate here is monotonic toward safety. An incident opened in
// DryRun stays dry-run for life whatever the gate later says, which is the
// existing stamped-at-open guarantee and the direction worth keeping; an
// incident opened Enabled becomes a no-op the moment the operator asks for
// one, and resumes if they change their mind.
//
// Pause is already enforced, separately and earlier, by gate.Allow.
func (c *Controller) effectiveDryRun(inc *types.Incident) bool {
	if inc == nil || inc.DryRun {
		return true
	}
	if c.gate == nil {
		return true
	}
	return c.gate.DryRun()
}

// executeStep dispatches a step to the platform, the actuator, verification,
// or notification. Dry-run incidents never touch anything: every step
// becomes an auditable no-op, including platform operations.
func (c *Controller) executeStep(ctx context.Context, inc *types.Incident, step *playbook.Step) (*types.ActionResult, error) {
	timeout := step.Timeout.Std()
	if timeout <= 0 {
		// Every step gets an upper bound, declared or not. A step with no
		// timeout inherited the process context, and an agent that never
		// answered (agentrpc polls until its context dies) kept the step
		// goroutine, its inFlight mark, and its gate slot alive forever —
		// silently eating a MaxConcurrentRemediations slot until restart.
		timeout = defaultStepTimeout
	}
	// The agent gets the step's timeout; the controller waits slightly
	// longer. Given the same deadline the controller always wins the race
	// and reports "context deadline exceeded", throwing away the agent's
	// own diagnosis — on real hardware that turned "GPU 0 is still held by
	// nvidia-device-plugin(11621)" into a timeout with no cause named.
	var cancel context.CancelFunc
	ctx, cancel = context.WithTimeout(ctx, timeout+agentResultGrace)
	defer cancel()
	if c.simulating(ctx, inc) {
		now := time.Now()
		return &types.ActionResult{
			ActionID:   actionID(inc),
			OK:         true,
			Output:     fmt.Sprintf("DRY-RUN: would execute %s on %s", step.Action, inc.Target.Node),
			StartedAt:  now,
			FinishedAt: now,
		}, nil
	}

	def, ok := action.ByWire(step.Action)
	if !ok {
		return nil, fmt.Errorf("unknown step action %q", step.Action)
	}
	switch def.Kind {
	case action.KindPlatform:
		return c.executePlatformStep(ctx, inc, def.Op, step)
	case action.KindAgent:
		return c.executeAgentStep(ctx, inc, def.Op, step)
	case action.KindVerify:
		node, _ := c.nodeFor(ctx, inc.Target.Node)
		if err := c.actuator.Healthy(ctx, node); err != nil {
			return nil, fmt.Errorf("%s: %w", step.Action, err)
		}
		return okResult(inc, step.Action+" passed"), nil
	case action.KindNotify:
		err := c.notify(ctx, notify.NotifyEvent{
			Kind: notify.EventActionTaken, Incident: inc,
			Message: firstNonBlank(step.Params["note"], step.Action),
		})
		if err != nil {
			return nil, err
		}
		return okResult(inc, step.Action+" delivered"), nil
	default:
		return nil, fmt.Errorf("unknown step action %q", step.Action)
	}
}

func (c *Controller) executePlatformStep(ctx context.Context, inc *types.Incident, op string, step *playbook.Step) (*types.ActionResult, error) {
	if c.platform == nil {
		return nil, fmt.Errorf("platform.%s: no platform configured", op)
	}
	node := inc.Target.Node
	reason := cordonReason(inc)
	switch op {
	case "cordon":
		if err := c.platform.Cordon(ctx, node, reason); err != nil {
			return nil, err
		}
		return okResult(inc, "cordoned "+node), nil
	case "uncordon":
		if err := c.platform.Uncordon(ctx, node); err != nil {
			return nil, err
		}
		return okResult(inc, "uncordoned "+node), nil
	case "drain":
		err := c.platform.Drain(ctx, node, platform.DrainOptions{
			// The POD's own grace period, not ours.
			//
			// DeleteOptions.GracePeriodSeconds overrides the pod spec in both
			// directions, so passing a constant 30s SIGKILLed a job that
			// declared terminationGracePeriodSeconds: 600 precisely so it could
			// checkpoint on SIGTERM — the workload most likely to be running on
			// the GPU we are about to reset, and the one with the most to lose.
			//
			// The zero value means "do not override", and evictPod clamps it
			// to the step's own remaining budget so a tenant-declared period
			// cannot outlast the window the ladder has.
			GracePeriod: platform.DrainUsePodGracePeriod,
			Timeout:     step.Timeout.Std(),
			// force: a playbook author's answer to "what should happen when a
			// pod has no controller".
			//
			// Drain refuses such a node by default, because nothing would
			// reschedule that pod and kubectl refuses for the same reason. But
			// kubectl is typed by a human who can add --force; a remediation
			// ladder has nobody at that moment, so without a way to say it a
			// node carrying one transient `kubectl run` pod could never be
			// remediated at all — and that pod is often a debug shell somebody
			// left open on the very node that is failing.
			//
			// Off by default, and stated per step, so evicting somebody's
			// unmanaged work is a decision written down in a playbook rather
			// than a default nobody chose.
			// Validate rejects an unparseable value at load, so the error here
			// is only reachable for an absent key: that is the default, and the
			// default is off.
			Force: func() bool { v, _ := strconv.ParseBool(step.Params["force"]); return v }(),
		})
		if err != nil {
			return nil, err
		}
		return okResult(inc, "drained "+node), nil
	case "quiesce_accelerator_stack":
		return c.quiesceAcceleratorStack(ctx, inc, step)
	case "restore_accelerator_stack":
		return c.restoreAcceleratorStackStep(ctx, inc, step)
	case "recycle_node":
		return c.recycleNodeStep(ctx, inc, false)
	case "replace_node":
		return c.recycleNodeStep(ctx, inc, true)
	case "evict_gpu_workload":
		workloads, err := c.platform.NodeWorkloads(ctx, node)
		if err != nil {
			return nil, err
		}
		evicted := 0
		for _, w := range workloads {
			if !w.UsesGPU {
				continue
			}
			if err := c.platform.EvictWorkload(ctx, w); err != nil {
				return nil, fmt.Errorf("evicting %s/%s: %w", w.Namespace, w.Name, err)
			}
			evicted++
			// Counted per workload and only after the eviction was accepted, so
			// the series is what the fleet actually lost rather than what the
			// step intended. Dry-run never reaches here (executeStep returns
			// earlier), which is why no DryRun guard is needed.
			metrics.WorkloadsEvicted.WithLabelValues(string(inc.Class)).Inc()
		}
		if evicted == 0 {
			// "evicted 0" used to read as success and let the ladder proceed
			// to a reset believing the device was clear. Say plainly that
			// nothing matched: either the node genuinely runs no accelerator
			// pods, or their resource name is one the matcher does not know —
			// and the operator must be able to tell those apart from the
			// audit trail alone.
			c.log.Info("evict_gpu_workload matched no accelerator workloads",
				"incident", inc.ID, "node", node, "pods_considered", len(workloads))
			return okResult(inc, fmt.Sprintf(
				"no accelerator workloads to evict from %s (%d pods considered)", node, len(workloads))), nil
		}
		return okResult(inc, fmt.Sprintf("evicted %d GPU workloads from %s", evicted, node)), nil
	default:
		return nil, fmt.Errorf("unknown platform action %q", op)
	}
}

func (c *Controller) executeAgentStep(ctx context.Context, inc *types.Incident, op string, step *playbook.Step) (*types.ActionResult, error) {
	if c.actuator == nil {
		return nil, fmt.Errorf("agent.%s: no actuator configured", op)
	}
	// A capability-gated agent action (today only agent.gpu_reset, which the
	// NVIDIA adapter advertises as a physical-device capability) must clear its
	// runtime evidence gate before the action is assembled or dispatched: a
	// report is merely evidence, and a missing, stale, profile-mismatched,
	// partitioned, or wrong-device report must never reach the executor. The
	// gate is selected by the registry, not a hard-coded string. Dry-run returns
	// above, so normal planning stays observable without fabricating a
	// capability.
	if def, ok := action.ByWire(step.Action); ok && def.CapabilityGate == action.CapabilityNVIDIAReset {
		if err := c.allowNVIDIAReset(ctx, inc, inc.Target); err != nil {
			return nil, fmt.Errorf("%s: accelerator capability gate: %w", step.Action, err)
		}
	}
	node, err := c.nodeFor(ctx, inc.Target.Node)
	if err != nil {
		return nil, err
	}
	params := map[string]string{}
	for k, v := range step.Params {
		params[k] = v
	}
	if inc.Target.GPUUUID != "" {
		params["gpu_uuid"] = inc.Target.GPUUUID
		params["gpu_index"] = strconv.Itoa(inc.Target.GPUIndex)
	}
	if op == string(types.ActionQuiesceAcceleratorHost) || op == string(types.ActionRestoreAcceleratorHost) {
		// Host state is node-wide and has to survive a controller restart, so both
		// normal playbook restoration and janitor recovery use the same durable key.
		params["host_state_key"] = inc.Target.Node
	}
	if op == "reboot" && node.BootID != "" {
		// The executor's idempotency guard: a retry after the node already
		// rebooted (boot ID changed) succeeds without bouncing it again.
		params["boot_id"] = node.BootID
	}
	result, err := c.actuator.Execute(ctx, node, types.Action{
		ID:         actionID(inc),
		IncidentID: inc.ID,
		Type:       types.ActionType(op),
		Params:     params,
		Timeout:    step.Timeout.Std(),
	})
	if err != nil {
		return nil, err
	}
	if !result.OK {
		return result, fmt.Errorf("agent.%s failed: %s", op, firstNonBlank(result.Error, "no detail"))
	}
	return result, nil
}

// escalate switches the incident to the failure-escalation playbook, or
// quarantines it when the ladder is exhausted.
func (c *Controller) escalate(ctx context.Context, inc *types.Incident, reason string) error {
	c.cancelPendingActions(ctx, inc)
	if inc.Attempt >= maxEscalationAttempts {
		// The ladder has climbed as far as it is allowed to on its own. Fail
		// closed to a human instead of continuing to drive destructive rungs.
		return c.quarantine(ctx, inc,
			fmt.Sprintf("%s; escalation cap reached (%d attempts) — stopping automation and handing off to a human",
				reason, inc.Attempt))
	}
	next, ok := c.runtimeConfig(ctx).Engine.Escalation(inc.Playbook)
	if !ok {
		return c.quarantine(ctx, inc, reason+"; no escalation playbook")
	}
	metrics.EscalationsTotal.Inc()
	previous := inc.Playbook
	inc.Playbook = next.Name
	inc.StepIndex = 0
	inc.Attempt++
	if err := c.transition(ctx, inc, types.StateEvaluating, "system", "escalate",
		fmt.Sprintf("%s; escalating %s -> %s (attempt %d)", reason, previous, next.Name, inc.Attempt), nil); err != nil {
		return err
	}
	return c.notify(ctx, notify.NotifyEvent{
		Kind: notify.EventActionTaken, Incident: inc,
		Message: fmt.Sprintf("escalated to playbook %s: %s", next.Name, reason),
	})
}

// cancelPendingActions tombstones the incident's undelivered queue entries
// so a superseded ladder rung can never be executed later by a slow agent.
// Leased (possibly running) work is deliberately left to finish or expire.
func (c *Controller) cancelPendingActions(ctx context.Context, inc *types.Incident) {
	cancelled, err := c.store.CancelPendingActionsForIncident(ctx, inc.ID)
	if err != nil {
		c.log.Warn("cancelling pending actions failed", "incident", inc.ID, "err", err)
		return
	}
	if cancelled > 0 {
		c.log.Info("cancelled undelivered actions for superseded step",
			"incident", inc.ID, "count", cancelled)
	}
}

// quarantine fails the incident closed to NEEDS_HUMAN.
func (c *Controller) quarantine(ctx context.Context, inc *types.Incident, reason string) error {
	c.cancelPendingActions(ctx, inc)
	if err := c.transition(ctx, inc, types.StateNeedsHuman, "system", "quarantine", reason, nil); err != nil {
		return err
	}
	return c.notify(ctx, notify.NotifyEvent{
		Kind: notify.EventNeedsHuman, Incident: inc,
		Message: reason,
	})
}

// actionID is deterministic per (incident, step, attempt) so replays after a
// controller restart are idempotent (executors cache completed IDs).
func actionID(inc *types.Incident) string {
	h := sha256.Sum256(fmt.Appendf(nil, "%s|%d|%d", inc.ID, inc.StepIndex, inc.Attempt))
	return hex.EncodeToString(h[:8])
}

func okResult(inc *types.Incident, output string) *types.ActionResult {
	now := time.Now()
	return &types.ActionResult{
		ActionID:   actionID(inc),
		OK:         true,
		Output:     output,
		StartedAt:  now,
		FinishedAt: now,
	}
}
