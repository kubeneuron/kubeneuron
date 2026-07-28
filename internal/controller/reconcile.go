package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/kubeneuron/kubeneuron/internal/config"
	"github.com/kubeneuron/kubeneuron/internal/metrics"
	"github.com/kubeneuron/kubeneuron/internal/notify"
	"github.com/kubeneuron/kubeneuron/internal/platform"
	"github.com/kubeneuron/kubeneuron/internal/playbook"
	"github.com/kubeneuron/kubeneuron/internal/store"
	"github.com/kubeneuron/kubeneuron/pkg/types"
)

// Default timings; SetTimings overrides them from configs/policies.yaml.
const (
	defaultVerifyQuietWindow = 10 * time.Minute
	defaultApprovalTTL       = 12 * time.Hour
	defaultObserveWindow     = 24 * time.Hour
	defaultDrainGracePeriod  = 30 * time.Second
	defaultReconcileInterval = 10 * time.Second
)

// SetTimings configures the verification quiet window and the approval TTL.
// Zero values keep the current setting.
func (c *Controller) SetTimings(verifyQuiet, approvalTTL time.Duration) {
	if verifyQuiet > 0 {
		c.verifyQuiet = verifyQuiet
	}
	if approvalTTL > 0 {
		c.approvalTTL = approvalTTL
	}
}

// reconcile advances every non-terminal incident one step. Long-running
// steps execute in per-incident goroutines guarded by the inFlight set, so
// a 30-minute drain never blocks the other incidents.
func (c *Controller) reconcile(ctx context.Context) {
	defer func(start time.Time) {
		metrics.ReconcileSeconds.Observe(time.Since(start).Seconds())
	}(time.Now())
	incidents, err := c.store.ListIncidents(ctx, store.IncidentFilter{
		States: []types.IncidentState{
			types.StateOpen, types.StateObserving, types.StateEvaluating,
			types.StateAwaitingApproval, types.StateExecuting, types.StateVerifying,
		},
	})
	if err != nil {
		c.log.Error("reconcile: listing incidents failed", "err", err)
		return
	}
	for _, inc := range incidents {
		if c.isInFlight(inc.ID) {
			continue
		}
		if err := c.advance(ctx, inc); err != nil {
			c.log.Error("reconcile: advancing incident failed",
				"incident", inc.ID, "state", inc.State, "err", err)
		}
	}
}

func (c *Controller) advance(ctx context.Context, inc *types.Incident) error {
	switch inc.State {
	case types.StateOpen:
		return c.advanceOpen(ctx, inc)
	case types.StateObserving:
		return c.advanceObserving(ctx, inc)
	case types.StateEvaluating:
		return c.advanceEvaluating(ctx, inc)
	case types.StateAwaitingApproval:
		return c.advanceAwaitingApproval(ctx, inc)
	case types.StateExecuting:
		return c.recoverOrphanedExecution(ctx, inc)
	case types.StateVerifying:
		return c.advanceVerifying(ctx, inc)
	default:
		return nil
	}
}

// advanceOpen routes a fresh incident to OBSERVING (observe-first classes)
// or EVALUATING, quarantining flapping targets first.
func (c *Controller) advanceOpen(ctx context.Context, inc *types.Incident) error {
	if c.flap != nil && c.flap.RecordReopen(inc.Target, inc.Class) {
		return c.quarantine(ctx, inc, "flap threshold reached: automation stopped")
	}
	book, hasBook := c.engine.Playbook(inc.Playbook)
	if !hasBook || isObservePlaybook(book) {
		return c.transition(ctx, inc, types.StateObserving, "system", "observe", "", nil)
	}
	return c.transition(ctx, inc, types.StateEvaluating, "system", "evaluate", "", nil)
}

// advanceObserving escalates once the policy threshold is crossed and
// resolves incidents that stay quiet for the whole observation window.
func (c *Controller) advanceObserving(ctx context.Context, inc *types.Incident) error {
	threshold, window := c.observePolicy(inc.Class)
	if threshold > 0 && inc.SignalSeen >= threshold {
		return c.transition(ctx, inc, types.StateEvaluating, "system", "observe-threshold",
			fmt.Sprintf("%d signals >= threshold %d", inc.SignalSeen, threshold), nil)
	}
	if time.Since(inc.UpdatedAt) >= window {
		if err := c.transition(ctx, inc, types.StateResolved, "system", "observe-quiet",
			fmt.Sprintf("no recurrence within %s", window), nil); err != nil {
			return err
		}
		if c.flap != nil {
			c.flap.RecordResolved(inc.Target, inc.Class)
		}
		return c.notifier.Notify(ctx, notify.NotifyEvent{
			Kind: notify.EventResolved, Incident: inc,
			Message: fmt.Sprintf("observed incident resolved: quiet for %s", window),
		})
	}
	return nil
}

// advanceEvaluating picks the next step and either parks the incident for
// approval or reserves a safety slot and starts execution.
func (c *Controller) advanceEvaluating(ctx context.Context, inc *types.Incident) error {
	if paused, err := c.nodePaused(ctx, inc.Target.Node); err == nil && paused {
		return nil // per-node pause: hold position, retry next tick
	}
	if window, active := c.activeMaintenanceWindow(ctx, inc.Target.Node); active {
		c.log.Debug("maintenance window active, holding incident",
			"incident", inc.ID, "node", inc.Target.Node, "window", window)
		return nil
	}
	book, ok := c.engine.Playbook(inc.Playbook)
	if !ok {
		return c.transition(ctx, inc, types.StateObserving, "system", "observe",
			"no playbook bound; observing", nil)
	}
	// Respect the playbook-level cooldown for a fresh run on this target.
	if inc.StepIndex == 0 && inc.Attempt == 0 {
		if remaining := c.gate.CooldownRemaining(inc.Target, playbookCooldownAction(inc.Playbook)); remaining > 0 {
			c.log.Debug("playbook in cooldown", "incident", inc.ID, "playbook", inc.Playbook, "remaining", remaining)
			return nil
		}
	}
	step, done, err := c.engine.NextStep(inc)
	if err != nil {
		return c.quarantine(ctx, inc, err.Error())
	}
	if done {
		_ = book
		return c.transition(ctx, inc, types.StateVerifying, "system", "verify",
			"playbook steps complete; verifying quiet window", nil)
	}
	if step.NeedsApproval() {
		if err := c.transition(ctx, inc, types.StateAwaitingApproval, "system", step.Name,
			"step requires human approval", nil); err != nil {
			return err
		}
		return c.notifier.RequestApproval(ctx, inc, step.Name)
	}
	return c.startStep(ctx, inc, step, "system")
}

// advanceAwaitingApproval resumes on a recorded decision and expires stale
// requests. The TTL anchors to StateChangedAt (see approval.Manager).
func (c *Controller) advanceAwaitingApproval(ctx context.Context, inc *types.Incident) error {
	decision, err := c.store.LatestApproval(ctx, inc.ID)
	if err != nil && err != store.ErrNotFound {
		return err
	}
	if decision != nil && decision.At.After(inc.StateChangedAt) {
		step, done, err := c.engine.NextStep(inc)
		if err != nil || done {
			return c.quarantine(ctx, inc, "approved step no longer exists")
		}
		switch decision.Decision {
		case types.ApprovalApproved:
			if window, active := c.activeMaintenanceWindow(ctx, inc.Target.Node); active {
				c.log.Debug("maintenance window active, holding approved step",
					"incident", inc.ID, "node", inc.Target.Node, "window", window)
				return nil // execute once the window closes; approval stays recorded
			}
			return c.startStep(ctx, inc, step, decision.Actor)
		case types.ApprovalRejected:
			if err := c.transition(ctx, inc, types.StateNeedsHuman, decision.Actor, step.Name,
				"approval rejected", nil); err != nil {
				return err
			}
			return c.notifier.Notify(ctx, notify.NotifyEvent{
				Kind: notify.EventNeedsHuman, Incident: inc,
				Message: fmt.Sprintf("step %s rejected by %s", step.Name, decision.Actor),
			})
		}
	}
	if time.Since(inc.StateChangedAt) > c.approvalTTL {
		if err := c.transition(ctx, inc, types.StateExpired, "system", "approval-ttl",
			fmt.Sprintf("no decision within %s", c.approvalTTL), nil); err != nil {
			return err
		}
		return c.notifier.Notify(ctx, notify.NotifyEvent{
			Kind: notify.EventExpired, Incident: inc,
			Message: "approval request expired without a decision",
		})
	}
	return nil
}

// advanceVerifying resolves after a quiet window; a new signal during
// verification means the remediation did not hold and the ladder escalates.
// A non-dry-run incident additionally requires positive runtime evidence —
// a fresh agent heartbeat and a fresh, ready accelerator report that still
// contains the target device. Missing evidence never auto-resolves: it
// holds, and after the evidence deadline it fails closed to NEEDS_HUMAN.
func (c *Controller) advanceVerifying(ctx context.Context, inc *types.Incident) error {
	if inc.UpdatedAt.After(inc.StateChangedAt) {
		return c.escalate(ctx, inc, "verification failed: signal recurred during quiet window")
	}
	if time.Since(inc.StateChangedAt) < c.verifyQuiet {
		return nil
	}
	if !inc.DryRun {
		ok, reason := c.verifyRuntimeEvidence(ctx, inc)
		if !ok {
			if time.Since(inc.StateChangedAt) < c.verifyEvidenceDeadline() {
				c.log.Debug("verification evidence not ready, holding",
					"incident", inc.ID, "reason", reason)
				return nil
			}
			if err := c.transition(ctx, inc, types.StateNeedsHuman, "system", "verify-evidence",
				"verification evidence unavailable: "+reason, nil); err != nil {
				return err
			}
			return c.notifier.Notify(ctx, notify.NotifyEvent{
				Kind: notify.EventNeedsHuman, Incident: inc,
				Message: "cannot verify remediation: " + reason,
			})
		}
	}
	cooldown := time.Duration(0)
	if book, ok := c.engine.Playbook(inc.Playbook); ok {
		cooldown = book.Cooldown.Std()
	}
	// Record the playbook cooldown only: this path holds no gate slot (each
	// step released its own in startStep), so it must not call Done and
	// release a reservation a concurrent incident on the same target owns.
	c.gate.RecordCooldown(inc.Target, playbookCooldownAction(inc.Playbook), cooldown)
	if err := c.transition(ctx, inc, types.StateResolved, "system", "resolve",
		fmt.Sprintf("healthy: quiet for %s", c.verifyQuiet), nil); err != nil {
		return err
	}
	if c.flap != nil {
		c.flap.RecordResolved(inc.Target, inc.Class)
	}
	return c.notifier.Notify(ctx, notify.NotifyEvent{
		Kind: notify.EventResolved, Incident: inc,
		Message: fmt.Sprintf("incident resolved after %s quiet window", c.verifyQuiet),
	})
}

// verifyEvidenceMaxAge bounds how old runtime evidence may be to support a
// resolution, and the deadline multiplier bounds how long VERIFYING waits
// for evidence before failing closed to a human.
const verifyEvidenceMaxAge = 5 * time.Minute

func (c *Controller) verifyEvidenceDeadline() time.Duration {
	// Three quiet windows is enough for a healthy agent to report twice;
	// never below 10 minutes so short dev quiet windows cannot flap
	// incidents into NEEDS_HUMAN on scheduling jitter alone.
	if d := 3 * c.verifyQuiet; d > 10*time.Minute {
		return d
	}
	return 10 * time.Minute
}

// verifyRuntimeEvidence checks positive health evidence for a non-dry-run
// resolution: a fresh agent heartbeat, and — for GPU-class targets — a
// fresh, ready NVIDIA accelerator report bound to the current node that
// still lists the target device. It returns ok=false with a reason that is
// safe to surface to operators.
func (c *Controller) verifyRuntimeEvidence(ctx context.Context, inc *types.Incident) (bool, string) {
	node, err := c.store.GetNode(ctx, inc.Target.Node)
	if err != nil || node == nil {
		return false, "node is not registered"
	}
	if node.AgentLastSeen.IsZero() || time.Since(node.AgentLastSeen) > verifyEvidenceMaxAge {
		return false, "agent heartbeat is stale"
	}
	if !inc.Target.IsGPU() {
		// Node-scoped incidents: the durable heartbeat is the strongest
		// node-liveness evidence available today; deeper node diagnostics
		// arrive with the hardware-qualified runtime.
		return true, ""
	}
	reports, ok := c.store.(store.AcceleratorReportStore)
	if !ok {
		return false, "accelerator report store is not configured"
	}
	report, err := reports.GetAcceleratorReport(ctx, inc.Target.Node, types.AcceleratorVendorNVIDIA)
	if err != nil {
		return false, "no accelerator report for the node"
	}
	if time.Since(report.ObservedAt) > verifyEvidenceMaxAge {
		return false, "accelerator report is stale"
	}
	if report.Readiness != types.AcceleratorReadinessReady {
		return false, fmt.Sprintf("accelerator runtime is %s, not ready", report.Readiness)
	}
	for _, device := range report.Devices {
		if device.ID == inc.Target.GPUUUID &&
			device.Kind == types.AcceleratorDevicePhysical &&
			device.Family == types.AcceleratorFamilyGPU {
			return true, ""
		}
	}
	return false, fmt.Sprintf("GPU %s is missing from the current inventory", inc.Target.GPUUUID)
}

// recoverOrphanedExecution handles EXECUTING incidents with no running step
// goroutine — a controller restart mid-step. Action IDs are deterministic,
// so re-evaluating and re-running the step is idempotent by design.
func (c *Controller) recoverOrphanedExecution(ctx context.Context, inc *types.Incident) error {
	return c.transition(ctx, inc, types.StateEvaluating, "system", "recover",
		"controller restarted mid-step; re-evaluating (idempotent action IDs)", nil)
}

// startStep reserves a gate slot, moves the incident to EXECUTING, and runs
// the step in its own goroutine.
func (c *Controller) startStep(ctx context.Context, inc *types.Incident, step *playbook.Step, actor string) error {
	// Capability evidence is a precondition, not an execution failure. If a
	// reset cannot be admitted because its runtime profile/report is absent,
	// stale, or unsafe, leave the incident in EVALUATING. Escalating from a
	// withheld reset into a reboot would turn missing evidence into a more
	// destructive action, which is the opposite of fail-closed remediation.
	if err := c.allowAcceleratorStep(ctx, inc, step); err != nil {
		metrics.GateDenials.Inc()
		c.log.Info("accelerator capability gate denied step, will hold",
			"incident", inc.ID, "step", step.Name, "reason", err.Error())
		return nil
	}
	action := gateAction(step)
	if err := c.gate.Allow(inc.Target, action); err != nil {
		metrics.GateDenials.Inc()
		c.log.Info("safety gate denied step, will retry",
			"incident", inc.ID, "step", step.Name, "reason", err.Error())
		return nil // denial is not an error: hold position and retry
	}
	if err := c.transition(ctx, inc, types.StateExecuting, actor, step.Name,
		"executing "+step.Action, step.Params); err != nil {
		c.gate.Done(inc.Target, action, 0)
		return err
	}
	c.setInFlight(inc.ID, true)
	go func() {
		defer c.setInFlight(inc.ID, false)
		defer c.gate.Done(inc.Target, action, 0)
		c.runStep(ctx, inc, step)
	}()
	return nil
}

func (c *Controller) allowAcceleratorStep(ctx context.Context, inc *types.Incident, step *playbook.Step) error {
	// Dry-run executes no side effect and is intentionally usable without a
	// hardware profile, so operators can see the planned ladder before
	// qualifying a node/runtime combination.
	if inc.DryRun {
		return nil
	}
	kind, op, ok := strings.Cut(step.Action, ".")
	if !ok || kind != "agent" || op != string(types.ActionGPUReset) {
		return nil
	}
	return c.allowNVIDIAReset(ctx, inc.Target)
}

// runStep executes one step and applies the outcome: next step, verification,
// escalation, or quarantine. Runs outside the reconcile goroutine.
func (c *Controller) runStep(ctx context.Context, inc *types.Incident, step *playbook.Step) {
	result, err := c.executeStep(ctx, inc, step)

	output := ""
	if result != nil {
		output = result.Output
	}
	if err != nil {
		metrics.StepsExecuted.WithLabelValues("failed").Inc()
		c.log.Warn("step failed", "incident", inc.ID, "step", step.Name, "err", err)
		if auditErr := c.appendAudit(ctx, inc, "system", step.Name, "FAILED: "+err.Error()); auditErr != nil {
			c.log.Error("audit append failed", "incident", inc.ID, "err", auditErr)
		}
		if escErr := c.escalate(ctx, inc, fmt.Sprintf("step %s failed: %v", step.Name, err)); escErr != nil {
			c.log.Error("escalation failed", "incident", inc.ID, "err", escErr)
		}
		return
	}

	if inc.DryRun {
		metrics.StepsExecuted.WithLabelValues("dry_run").Inc()
	} else {
		metrics.StepsExecuted.WithLabelValues("ok").Inc()
	}
	inc.StepIndex++
	_, done, nextErr := c.engine.NextStep(inc)
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
	if notifyErr := c.notifier.Notify(ctx, notify.NotifyEvent{
		Kind: notify.EventActionTaken, Incident: inc,
		Message: fmt.Sprintf("step %s: %s", step.Name, firstNonBlank(output, "done")),
	}); notifyErr != nil {
		c.log.Warn("notification failed", "incident", inc.ID, "err", notifyErr)
	}
}

// executeStep dispatches a step to the platform, the actuator, verification,
// or notification. Dry-run incidents never touch anything: every step
// becomes an auditable no-op, including platform operations.
func (c *Controller) executeStep(ctx context.Context, inc *types.Incident, step *playbook.Step) (*types.ActionResult, error) {
	if timeout := step.Timeout.Std(); timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	if inc.DryRun {
		now := time.Now()
		return &types.ActionResult{
			ActionID:   actionID(inc),
			OK:         true,
			Output:     fmt.Sprintf("DRY-RUN: would execute %s on %s", step.Action, inc.Target.Node),
			StartedAt:  now,
			FinishedAt: now,
		}, nil
	}

	kind, op, _ := strings.Cut(step.Action, ".")
	switch kind {
	case "platform":
		return c.executePlatformStep(ctx, inc, op, step)
	case "agent":
		return c.executeAgentStep(ctx, inc, op, step)
	case "verify":
		node, _ := c.nodeFor(ctx, inc.Target.Node)
		if err := c.actuator.Healthy(ctx, node); err != nil {
			return nil, fmt.Errorf("%s: %w", step.Action, err)
		}
		return okResult(inc, step.Action+" passed"), nil
	case "notify":
		err := c.notifier.Notify(ctx, notify.NotifyEvent{
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
	reason := fmt.Sprintf("kubeneuron: %s (%s)", inc.Class, inc.ID)
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
			GracePeriod: defaultDrainGracePeriod,
			Timeout:     step.Timeout.Std(),
		})
		if err != nil {
			return nil, err
		}
		return okResult(inc, "drained "+node), nil
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
	// gpu_reset is the only current agent action that the NVIDIA adapter can
	// advertise as a physical-device capability. Do this before assembling or
	// dispatching the action: a report is merely evidence, and a missing,
	// stale, profile-mismatched, partitioned, or wrong-device report must never
	// reach the executor. Dry-run returns above, so normal planning remains
	// observable without fabricating a runtime capability.
	if op == string(types.ActionGPUReset) {
		if err := c.allowNVIDIAReset(ctx, inc.Target); err != nil {
			return nil, fmt.Errorf("agent.gpu_reset: accelerator capability gate: %w", err)
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
	if op == "reboot" && node.BootID != "" {
		// The executor's idempotency guard: a retry after the node already
		// rebooted (boot ID changed) succeeds without bouncing it again.
		params["boot_id"] = node.BootID
	}
	result, err := c.actuator.Execute(ctx, node, types.Action{
		ID:      actionID(inc),
		Type:    types.ActionType(op),
		Params:  params,
		Timeout: step.Timeout.Std(),
	})
	if err != nil {
		return nil, err
	}
	if !result.OK {
		return result, fmt.Errorf("agent.%s failed: %s", op, firstNonBlank(result.Error, "no detail"))
	}
	return result, nil
}

// allowNVIDIAReset combines the configured server profile with a fresh,
// ready, agent-owned observation. Neither input alone is authority: the
// profile selects the intended node fleet and digest, while the report proves
// the current physical device and unpartitioned topology. This is deliberately
// narrower than a future general action gate so unmodelled agent actions do
// not accidentally inherit a reset capability.
func (c *Controller) allowNVIDIAReset(ctx context.Context, target types.Target) error {
	if target.Node == "" || target.GPUUUID == "" {
		return fmt.Errorf("physical NVIDIA reset requires node and GPU UUID target")
	}
	// Node names and labels can change with a Kubernetes delete/recreate. Read
	// both from one current inventory object before considering a report, so a
	// previous node object cannot inherit a matching profile or capability.
	node, ok := c.acceleratorNode(ctx, target.Node)
	if !ok || node.UID == "" {
		return fmt.Errorf("node UID is unavailable for accelerator report binding")
	}
	if node.Labels == nil {
		return fmt.Errorf("node labels are unavailable for runtime profile selection")
	}
	c.acceleratorProfilesMu.RLock()
	profiles := append([]config.AcceleratorRuntimeProfile(nil), c.acceleratorProfiles...)
	c.acceleratorProfilesMu.RUnlock()
	profile, err := (config.Config{AcceleratorProfiles: profiles}).ResolveAcceleratorRuntimeProfile(node.Labels, types.AcceleratorVendorNVIDIA)
	if err != nil {
		return err
	}
	reports, ok := c.store.(store.AcceleratorReportStore)
	if !ok {
		return fmt.Errorf("accelerator report store is not configured")
	}
	report, err := reports.GetAcceleratorReport(ctx, target.Node, types.AcceleratorVendorNVIDIA)
	if err != nil {
		return fmt.Errorf("load NVIDIA accelerator report: %w", err)
	}
	if report.NodeUID != node.UID {
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

// escalate switches the incident to the failure-escalation playbook, or
// quarantines it when the ladder is exhausted.
func (c *Controller) escalate(ctx context.Context, inc *types.Incident, reason string) error {
	c.cancelPendingActions(ctx, inc)
	next, ok := c.engine.Escalation(inc.Playbook)
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
	return c.notifier.Notify(ctx, notify.NotifyEvent{
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
	return c.notifier.Notify(ctx, notify.NotifyEvent{
		Kind: notify.EventNeedsHuman, Incident: inc,
		Message: reason,
	})
}

// transition applies a validated state change and persists the incident and
// its audit entry in one store transaction (the statemachine contract).
func (c *Controller) transition(ctx context.Context, inc *types.Incident, to types.IncidentState, actor, action, result string, params map[string]string) error {
	from := inc.State
	if err := playbook.Transition(inc, to); err != nil {
		return err
	}
	now := time.Now()
	inc.UpdatedAt = now
	inc.StateChangedAt = now
	if to == types.StateResolved {
		inc.ResolvedAt = &now
	}
	return c.store.WithTx(ctx, func(tx store.Tx) error {
		if err := tx.UpdateIncident(ctx, inc); err != nil {
			return err
		}
		return tx.AppendAudit(ctx, &types.AuditEntry{
			IncidentID: inc.ID, Time: now,
			FromState: from, ToState: to,
			Actor: actor, Action: action,
			Params: params, Result: result, DryRun: inc.DryRun,
		})
	})
}

// appendAudit records an event that does not change state (e.g. a failed
// step before escalation decides the next state).
func (c *Controller) appendAudit(ctx context.Context, inc *types.Incident, actor, action, result string) error {
	return c.store.AppendAudit(ctx, &types.AuditEntry{
		IncidentID: inc.ID, Time: time.Now(),
		FromState: inc.State, ToState: inc.State,
		Actor: actor, Action: action, Result: result, DryRun: inc.DryRun,
	})
}

// --- helpers ---

func (c *Controller) isInFlight(id string) bool {
	c.inFlightMu.Lock()
	defer c.inFlightMu.Unlock()
	return c.inFlight[id]
}

func (c *Controller) setInFlight(id string, v bool) {
	c.inFlightMu.Lock()
	defer c.inFlightMu.Unlock()
	if v {
		c.inFlight[id] = true
	} else {
		delete(c.inFlight, id)
	}
}

// activeMaintenanceWindow reports whether any currently active window
// covers the node, returning the window name. When a selector window's node
// labels cannot be resolved, the check fails toward holding automation —
// during declared maintenance, not acting is the safe direction.
func (c *Controller) activeMaintenanceWindow(ctx context.Context, node string) (string, bool) {
	c.windowsMu.RLock()
	windows := c.windows
	c.windowsMu.RUnlock()
	if len(windows) == 0 {
		return "", false
	}
	now := time.Now()
	var labels map[string]string
	labelsResolved := false
	for _, w := range windows {
		if !w.ActiveAt(now) {
			continue
		}
		if len(w.MatchLabels) == 0 {
			return w.Name, true
		}
		if !labelsResolved {
			labels = c.nodeLabels(ctx, node)
			labelsResolved = true
		}
		if labels == nil {
			c.log.Warn("cannot resolve node labels; holding automation during selector window",
				"node", node, "window", w.Name)
			return w.Name, true
		}
		if w.MatchesLabels(labels) {
			return w.Name, true
		}
	}
	return "", false
}

// nodeLabels resolves node labels from inventory, falling back to the
// platform; nil means the labels could not be determined.
func (c *Controller) nodeLabels(ctx context.Context, node string) map[string]string {
	if n, err := c.store.GetNode(ctx, node); err == nil && len(n.Labels) > 0 {
		return n.Labels
	}
	if c.platform == nil {
		return nil
	}
	nodes, err := c.platform.ListNodes(ctx)
	if err != nil {
		return nil
	}
	for _, n := range nodes {
		if n.Name == node {
			if n.Labels == nil {
				return map[string]string{}
			}
			return n.Labels
		}
	}
	return nil
}

// acceleratorNode resolves the current inventory object used for a
// profile-gated accelerator action. A live platform result is authoritative
// when available: falling back to SQLite after a Kubernetes list failure
// would allow stale node-name reuse during an outage. Store fallback exists
// only for non-platform controller tests and integrations that explicitly
// persist an immutable identity.
func (c *Controller) acceleratorNode(ctx context.Context, node string) (*types.Node, bool) {
	if c.platform != nil {
		nodes, err := c.platform.ListNodes(ctx)
		if err != nil {
			return nil, false
		}
		for i := range nodes {
			if nodes[i].Name == node {
				return &nodes[i], true
			}
		}
		return nil, false
	}
	n, err := c.store.GetNode(ctx, node)
	if err != nil {
		return nil, false
	}
	return n, true
}

func (c *Controller) nodePaused(ctx context.Context, name string) (bool, error) {
	n, err := c.store.GetNode(ctx, name)
	if err != nil {
		return false, err
	}
	return n.Paused, nil
}

// nodeFor loads inventory for actuation, falling back to a name-only node
// (Kubernetes actuation needs nothing more).
func (c *Controller) nodeFor(ctx context.Context, name string) (types.Node, error) {
	n, err := c.store.GetNode(ctx, name)
	if err == store.ErrNotFound {
		return types.Node{Name: name}, nil
	}
	if err != nil {
		return types.Node{}, err
	}
	return *n, nil
}

// observePolicy reads the observation threshold and window for a class.
// Explicit policy params win; otherwise the detection catalog's declared
// threshold ("XID 13 is actionable after 3 occurrences in 1h") applies, so
// the documented catalog semantics are enforced rather than advisory.
// threshold 0 means "never auto-escalate".
func (c *Controller) observePolicy(class types.ProblemClass) (threshold int, window time.Duration) {
	window = defaultObserveWindow
	if catThreshold, catWindow, ok := c.catalog.ObservePolicy(class); ok {
		threshold, window = catThreshold, catWindow
	}
	pol, ok := c.engine.PolicyFor(class)
	if !ok {
		return threshold, window
	}
	if v, err := strconv.Atoi(pol.Params["threshold"]); err == nil && v > 0 {
		threshold = v
	}
	if d, err := time.ParseDuration(pol.Params["window"]); err == nil && d > 0 {
		window = d
	}
	return threshold, window
}

// isObservePlaybook reports whether a playbook only records and notifies.
func isObservePlaybook(book *playbook.Playbook) bool {
	return len(book.Steps) == 1 && strings.HasPrefix(book.Steps[0].Action, "notify.")
}

// gateAction maps a step to the action type the safety gate reasons about,
// so reboot-class steps hit the reboot concurrency cap.
func gateAction(step *playbook.Step) types.ActionType {
	if kind, op, ok := strings.Cut(step.Action, "."); ok && kind == "agent" {
		return types.ActionType(op)
	}
	return types.ActionType(step.Action)
}

// playbookCooldownAction is the pseudo-action under which a completed
// playbook's cooldown is recorded on the gate.
func playbookCooldownAction(name string) types.ActionType {
	return types.ActionType("playbook:" + name)
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

func firstNonBlank(ss ...string) string {
	for _, s := range ss {
		if strings.TrimSpace(s) != "" {
			return s
		}
	}
	return ""
}
