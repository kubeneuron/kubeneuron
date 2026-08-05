package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/kubeneuron/kubeneuron/internal/metrics"
	"github.com/kubeneuron/kubeneuron/internal/notify"
	"github.com/kubeneuron/kubeneuron/internal/playbook"
	"github.com/kubeneuron/kubeneuron/internal/store"
	"github.com/kubeneuron/kubeneuron/pkg/types"
)

// This file is the state walk: the reconcile tick, the per-state advance
// functions, the approval request/decision protocol, verification, and the
// transactional transition every state change goes through. Its seams live
// beside it: gate admission and confinement in admission.go, step execution
// in execution.go, node resolution in noderesolution.go.

// Default timings; SetTimings overrides them from configs/policies.yaml.
const (
	defaultVerifyQuietWindow = 10 * time.Minute
	defaultApprovalTTL       = 12 * time.Hour
	defaultObserveWindow     = 24 * time.Hour
	defaultDrainGracePeriod  = 30 * time.Second
	defaultReconcileInterval = 10 * time.Second
	// agentResultGrace is how much longer the controller waits than the agent's
	// own action budget, so a failing action's reason survives the deadline.
	agentResultGrace = 15 * time.Second
	// defaultStepTimeout bounds a step whose playbook declares no timeout. It is
	// generous — a drain or a reboot can legitimately take many minutes — because
	// its job is not pacing but leak prevention: without any bound, a step whose
	// agent never answers holds its goroutine, inFlight mark, and gate slot until
	// the controller restarts.
	defaultStepTimeout = 30 * time.Minute
	// maxEscalationAttempts caps how many times one incident may climb its
	// escalation ladder before it fails closed to NEEDS_HUMAN. inc.Attempt is
	// bumped once per escalation, so this bounds a persistent fault that keeps
	// re-failing each rung: past the cap a human decides, rather than the
	// destructive ladder repeating at reconcile speed. Cyclic ladders are
	// already rejected at compile/load; this is the runtime backstop for a long
	// (but acyclic) ladder and for an engine assembled without that validation.
	maxEscalationAttempts = 8
)

// SetTimings configures the verification quiet window and the approval TTL.
// Zero values keep the current setting.
func (c *Controller) SetTimings(verifyQuiet, approvalTTL time.Duration) {
	c.mutateRuntimeConfig(func(rc *RuntimeConfig) {
		if verifyQuiet > 0 {
			rc.VerifyQuiet = verifyQuiet
		}
		if approvalTTL > 0 {
			rc.ApprovalTTL = approvalTTL
		}
	})
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
	// Pin ONE configuration snapshot for this whole advance — and for the
	// step goroutine it may spawn, which captures the same ctx. Every read
	// below (engine, selectors, windows, timings, catalog, profiles) observes
	// the same generation; the next advance re-pins fresh, so a hot reload is
	// visible within one tick without ever mixing generations mid-decision.
	ctx = c.pinRuntimeConfig(ctx)
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
	book, hasBook := c.runtimeConfig(ctx).Engine.Playbook(inc.Playbook)
	if !hasBook || isObservePlaybook(book) {
		return c.transition(ctx, inc, types.StateObserving, "system", "observe", "", nil)
	}
	return c.transition(ctx, inc, types.StateEvaluating, "system", "evaluate", "", nil)
}

// advanceObserving escalates once the policy threshold is crossed and
// resolves incidents that stay quiet for the whole observation window.
func (c *Controller) advanceObserving(ctx context.Context, inc *types.Incident) error {
	// Late-bind, narrowly: an incident that opened before its policy existed
	// carries no playbook (binding is open-time), and without one it can only
	// quiet-resolve — a fault that raced a configuration rollout would get
	// neither remediation nor a page. Bind here IF a policy now matches:
	// only never-bound incidents, only in OBSERVING (no approval round, no
	// gate slot, no step state exists yet), from this pass's pinned
	// snapshot. Bound incidents are never re-bound — selection changes must
	// not move a mid-ladder incident between playbooks.
	if inc.Playbook == "" {
		if book, ok := c.runtimeConfig(ctx).Engine.Select(types.Signal{Class: inc.Class}); ok && book != nil {
			// Field-only rewrite: bump StateChangedAt — the WRITE-FENCE —
			// so a concurrent transition off a stale snapshot conflicts
			// instead of silently overwriting the bind.
			inc.StateChangedAt = time.Now()
			inc.Playbook = book.Name
			if err := c.store.UpdateIncident(ctx, inc); err != nil {
				return err // conflict: the next pass re-reads and retries
			}
			if err := c.appendAudit(ctx, inc, "system", "bind-playbook",
				fmt.Sprintf("policy for class %s appeared after open; bound playbook %q", inc.Class, book.Name)); err != nil {
				c.log.Error("bind-playbook audit append failed", "incident", inc.ID, "err", err)
			}
			c.log.Info("late-bound playbook to incident opened before its policy existed",
				"incident", inc.ID, "class", inc.Class, "playbook", book.Name)
		}
	}
	threshold, window := c.observePolicy(ctx, inc.Class)
	if threshold > 0 && inc.SignalSeen >= threshold {
		// Only escalate to EVALUATING when a playbook is actually bound to act on.
		// A threshold-crossed class with no bound playbook (a CRD-compiled policy
		// set that omits the class) would otherwise livelock: advanceEvaluating
		// sends an unbound incident straight back to OBSERVING, and every
		// transition rewrites UpdatedAt — the quiet-window anchor — so it can never
		// quiet-resolve and appends ~8,640 audit rows/day. With no playbook bound,
		// hold in OBSERVING and let the quiet window resolve it instead.
		if _, bound := c.runtimeConfig(ctx).Engine.Playbook(inc.Playbook); bound {
			return c.transition(ctx, inc, types.StateEvaluating, "system", "observe-threshold",
				fmt.Sprintf("%d signals >= threshold %d", inc.SignalSeen, threshold), nil)
		}
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
		c.deferPlaybook(ctx, inc, metrics.DeferNodePaused)
		return nil // per-node pause: hold position, retry next tick
	}
	if window, active := c.activeMaintenanceWindow(ctx, inc.Target.Node); active {
		c.deferPlaybook(ctx, inc, metrics.DeferMaintenanceWindow)
		c.log.Debug("maintenance window active, holding incident",
			"incident", inc.ID, "node", inc.Target.Node, "window", window)
		return nil
	}
	book, ok := c.runtimeConfig(ctx).Engine.Playbook(inc.Playbook)
	if !ok {
		return c.transition(ctx, inc, types.StateObserving, "system", "observe",
			"no playbook bound; observing", nil)
	}
	// Respect the playbook-level cooldown before the first step of any run on
	// this target — including a rung reached by escalation (Attempt > 0). Gating
	// this on Attempt == 0 let every escalated rung skip cooldown and drive its
	// first (often destructive) step at reconcile speed; keying only on
	// StepIndex == 0 keeps the loop rate bounded by the cooldown as well as by
	// the escalation cap.
	if inc.StepIndex == 0 {
		if remaining := c.gate.CooldownRemaining(inc.Target, playbookCooldownAction(inc.Playbook)); remaining > 0 {
			c.deferPlaybook(ctx, inc, metrics.DeferPlaybookCooldown)
			c.log.Debug("playbook in cooldown", "incident", inc.ID, "playbook", inc.Playbook, "remaining", remaining)
			return nil
		}
		// Refuse a reset the node can never perform before the first step
		// disrupts anything. Escalation is the right outcome here, not a hold:
		// this is a reported fact, not missing evidence, so the ladder should
		// move on to a rung that can actually work.
		if err := c.refuseInfeasibleReset(ctx, inc, book); err != nil {
			c.log.Info("reset playbook refused before any disruptive step",
				"incident", inc.ID, "playbook", inc.Playbook, "reason", err.Error())
			if !errors.Is(err, errResetTargetUnattributed) {
				// Only the holder refusal is protection: processes are using the
				// device, so nothing was disrupted for a reset that could not
				// have worked. An unattributed incident is a structural dead end
				// — the ladder escalates to a rung that DOES run — so counting it
				// as protection would claim a disruption that still happens.
				c.deferPlaybook(ctx, inc, metrics.DeferDeviceHolders)
			}
			if errors.Is(err, errResetTargetUnattributed) {
				// An unattributed incident has no device to reset and can never gain
				// one. From EVALUATING the ladder cannot legally re-enter EVALUATING to
				// swap in the reboot rung, so fail closed to a human with the reason —
				// the task-sanctioned outcome — instead of holding here forever. A
				// human can identify the affected GPU (or reboot the node) by hand.
				return c.quarantine(ctx, inc, err.Error())
			}
			return c.escalate(ctx, inc, err.Error())
		}
		// Same principle for the arming boundary: if any rung of this playbook
		// is agent-destructive and the node's agent has definitively declared
		// itself unarmed, escalate before the first cordon/drain — not after
		// the node is disrupted and the reboot rung dies at the executor.
		if !inc.DryRun && playbookNeedsArmedAgent(book) {
			for i := range book.Steps {
				reason, verdict := c.refuseUnarmedAgent(ctx, inc, &book.Steps[i])
				if verdict == armingHold {
					c.deferStep(inc, &book.Steps[i], metrics.DeferUnarmedAgent)
					c.log.Info("holding: destructive-execution arming is still propagating to the node's agent",
						"incident", inc.ID, "node", inc.Target.Node)
					return nil
				}
				if verdict == armingRefuse {
					c.deferStep(inc, &book.Steps[i], metrics.DeferUnarmedAgent)
					c.log.Info("playbook needs an armed agent the node does not have",
						"incident", inc.ID, "playbook", inc.Playbook, "reason", reason)
					return c.escalate(ctx, inc, reason)
				}
			}
		}
	}
	step, done, err := c.runtimeConfig(ctx).Engine.NextStep(inc)
	if err != nil {
		return c.quarantine(ctx, inc, err.Error())
	}
	if done {
		return c.transition(ctx, inc, types.StateVerifying, "system", "verify",
			"playbook steps complete; verifying quiet window", nil)
	}
	// A recycle whose instance provably cannot be recycled (an autoscaling-group
	// member) escalates now — before a human is asked to approve a step the
	// node group would fight and lose by timeout. Likewise an agent-destructive
	// step on a node whose agent definitively registered as unarmed: the human
	// must never be asked to approve a step the node will provably refuse.
	if reason, refused := c.refuseUnrecyclableNode(ctx, inc, step); refused {
		c.deferStep(inc, step, metrics.DeferRecycleNotViable)
		return c.escalate(ctx, inc, reason)
	}
	if reason, verdict := c.refuseUnarmedAgent(ctx, inc, step); verdict == armingHold {
		c.deferStep(inc, step, metrics.DeferUnarmedAgent)
		c.log.Info("holding before approval: destructive-execution arming is still propagating to the node's agent",
			"incident", inc.ID, "node", inc.Target.Node)
		return nil
	} else if verdict == armingRefuse {
		c.deferStep(inc, step, metrics.DeferUnarmedAgent)
		return c.escalate(ctx, inc, reason)
	}
	if step.NeedsApproval() {
		return c.parkForApproval(ctx, inc, step, "step requires human approval")
	}
	return c.startStep(ctx, inc, step, "system")
}

// parkForApproval parks (or re-parks) the incident for a human decision as
// ONE atomic approval round: the state change, the epoch bump, the audit
// entry, and the round's "requested" record — the durable statement of
// exactly what the human is being asked — commit in a single transaction.
// A decision then binds to this round by epoch (see DecideApproval), so a
// playbook hot-swap between the notification and the click cannot substitute
// the approved action, and a re-park orphans every stale decision by
// construction rather than by timestamp comparison.
func (c *Controller) parkForApproval(ctx context.Context, inc *types.Incident, step *playbook.Step, note string) error {
	from := inc.State
	if from != types.StateAwaitingApproval {
		if err := playbook.Transition(inc, types.StateAwaitingApproval); err != nil {
			return err
		}
	}
	now := time.Now()
	fence := inc.StateChangedAt // same write-fence as transition(); see there
	inc.UpdatedAt = now
	inc.StateChangedAt = now // the round's TTL anchor
	if err := c.store.WithTx(ctx, func(tx store.Tx) error {
		fresh, err := tx.GetIncident(ctx, inc.ID)
		if err != nil {
			return err
		}
		if fresh.State != from || !fresh.StateChangedAt.Equal(fence) {
			return store.ErrConflict
		}
		inc.SignalSeen = fresh.SignalSeen
		inc.Version = fresh.Version
		inc.ApprovalEpoch = fresh.ApprovalEpoch + 1
		if err := tx.UpdateIncident(ctx, inc); err != nil {
			return err
		}
		if err := tx.AppendAudit(ctx, &types.AuditEntry{
			IncidentID: inc.ID, Time: now,
			FromState: from, ToState: types.StateAwaitingApproval,
			Actor: "system", Action: step.Name, Result: note, DryRun: inc.DryRun,
		}); err != nil {
			return err
		}
		return tx.RecordApproval(ctx, &types.Approval{
			IncidentID:   inc.ID,
			StepName:     step.Name,
			Decision:     types.ApprovalRequested,
			Actor:        "system",
			Channel:      "system",
			At:           now,
			PlaybookName: inc.Playbook,
			StepAction:   step.Action,
			StepHash:     stepContentHash(inc.Playbook, step),
			ParkEpoch:    inc.ApprovalEpoch,
		})
	}); err != nil {
		return err
	}
	if from == types.StateAwaitingApproval {
		c.log.Warn("re-parking for a fresh approval", "incident", inc.ID, "step", step.Name, "reason", note)
	}
	return c.notifier.RequestApproval(ctx, inc, step.Name)
}

// advanceAwaitingApproval resumes on the current approval round's decision
// and expires undecided rounds. The round is identified by the incident's
// ApprovalEpoch: decisions of earlier epochs are invisible by construction,
// so no timestamp ordering against StateChangedAt is needed to ignore them.
func (c *Controller) advanceAwaitingApproval(ctx context.Context, inc *types.Incident) error {
	if inc.ApprovalEpoch == 0 {
		// Epoch 0 is the pre-upgrade population, not a round: every legacy
		// row — requests AND orphaned decisions from any number of old parks
		// — carries park_epoch 0, so consulting decisions here could pair a
		// stale approval with a request from a DIFFERENT park and execute a
		// step the human never approved. Migrate the park into a verifiable
		// round first; the human re-decides that one.
		step, done, err := c.runtimeConfig(ctx).Engine.NextStep(inc)
		if err != nil || done {
			return c.quarantine(ctx, inc, "parked step no longer exists")
		}
		return c.parkForApproval(ctx, inc, step,
			"re-parking: this park predates approval rounds; a fresh approval is required")
	}
	decision, err := c.store.LatestApprovalDecision(ctx, inc.ID, inc.ApprovalEpoch)
	if err != nil && err != store.ErrNotFound {
		return err
	}
	if decision != nil {
		step, done, err := c.runtimeConfig(ctx).Engine.NextStep(inc)
		if err != nil || done {
			return c.quarantine(ctx, inc, "approved step no longer exists")
		}
		switch decision.Decision {
		case types.ApprovalApproved:
			// The decision is honored only for the exact step this round asked
			// about. The round's request record ALWAYS exists and always carries
			// full identity — a round without one (a pre-epoch park upgraded
			// mid-flight) fails closed into a fresh round rather than being
			// waved through as "legacy".
			request, reqErr := c.store.GetApprovalRequest(ctx, inc.ID, inc.ApprovalEpoch)
			if reqErr != nil {
				return c.parkForApproval(ctx, inc, step,
					"re-parking: this park predates approval rounds and has no request record; a fresh approval is required")
			}
			if reason := requestMismatch(request, inc.Playbook, step); reason != "" {
				return c.parkForApproval(ctx, inc, step, "not executing approved step: "+reason)
			}
			if window, active := c.activeMaintenanceWindow(ctx, inc.Target.Node); active {
				c.deferStep(inc, step, metrics.DeferMaintenanceWindow)
				c.log.Debug("maintenance window active, holding approved step",
					"incident", inc.ID, "node", inc.Target.Node, "window", window)
				return nil // execute once the window closes; approval stays recorded
			}
			return c.startStep(ctx, inc, step, decision.Actor)
		case types.ApprovalRejected:
			// Audit and notify with the step the human actually REJECTED — the
			// decision inherits its round's request identity — not whatever
			// step is current now (a hot-swap may have changed it; the
			// rejection deliberately fails closed regardless of the swap).
			rejected := firstNonBlank(decision.StepName, step.Name)
			if err := c.transition(ctx, inc, types.StateNeedsHuman, decision.Actor, rejected,
				"approval rejected", nil); err != nil {
				return err
			}
			return c.notifier.Notify(ctx, notify.NotifyEvent{
				Kind: notify.EventNeedsHuman, Incident: inc,
				Message: fmt.Sprintf("step %s rejected by %s", rejected, decision.Actor),
			})
		}
	}
	if time.Since(inc.StateChangedAt) > c.runtimeConfig(ctx).ApprovalTTL {
		if err := c.transition(ctx, inc, types.StateExpired, "system", "approval-ttl",
			fmt.Sprintf("no decision within %s", c.runtimeConfig(ctx).ApprovalTTL), nil); err != nil {
			return err
		}
		return c.notifier.Notify(ctx, notify.NotifyEvent{
			Kind: notify.EventExpired, Incident: inc,
			Message: "approval request expired without a decision",
		})
	}
	return nil
}

// stepContentHash hashes the meaningful content of a step — its playbook, name,
// action, approval requirement, and params — so an edit at the same index is
// detected even when the step's name and action are unchanged.
func stepContentHash(playbookName string, step *playbook.Step) string {
	h := sha256.New()
	write := func(s string) {
		_, _ = h.Write([]byte(s))
		_, _ = h.Write([]byte{0})
	}
	write(playbookName)
	write(step.Name)
	write(step.Action)
	write(step.Approval)
	keys := make([]string, 0, len(step.Params))
	for k := range step.Params {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		write(k)
		write(step.Params[k])
	}
	return hex.EncodeToString(h.Sum(nil))
}

// requestMismatch reports why the current step no longer matches what this
// approval round ASKED the human, or "" when they match. Unlike the retired
// decision-identity ladder, there is no legacy waiver: a request record
// always exists for a round and always carries full identity, so any
// difference fails closed into a fresh round.
func requestMismatch(request *types.Approval, playbookName string, step *playbook.Step) string {
	if request.PlaybookName != playbookName {
		return fmt.Sprintf("approval was requested under playbook %q but the incident now runs %q", request.PlaybookName, playbookName)
	}
	if request.StepName != step.Name {
		return fmt.Sprintf("approval was requested for step %q but the current step is %q", request.StepName, step.Name)
	}
	if request.StepAction != step.Action {
		return fmt.Sprintf("approval was requested for action %q but the current step runs %q", request.StepAction, step.Action)
	}
	if request.StepHash != stepContentHash(playbookName, step) {
		return "the requested step's content changed at this index"
	}
	return ""
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
	if time.Since(inc.StateChangedAt) < c.runtimeConfig(ctx).VerifyQuiet {
		return nil
	}
	if !inc.DryRun {
		ok, reason := c.verifyRuntimeEvidence(ctx, inc)
		if !ok {
			if time.Since(inc.StateChangedAt) < c.verifyEvidenceDeadline(ctx) {
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
	if book, ok := c.runtimeConfig(ctx).Engine.Playbook(inc.Playbook); ok {
		cooldown = book.Cooldown.Std()
	}
	// Record the playbook cooldown only: this path holds no gate slot (each
	// step released its own in startStep), so it must not call Done and
	// release a reservation a concurrent incident on the same target owns.
	c.gate.RecordCooldown(inc.Target, playbookCooldownAction(inc.Playbook), cooldown)
	if err := c.transition(ctx, inc, types.StateResolved, "system", "resolve",
		fmt.Sprintf("healthy: quiet for %s", c.runtimeConfig(ctx).VerifyQuiet), nil); err != nil {
		return err
	}
	if c.flap != nil {
		c.flap.RecordResolved(inc.Target, inc.Class)
	}
	return c.notifier.Notify(ctx, notify.NotifyEvent{
		Kind: notify.EventResolved, Incident: inc,
		Message: fmt.Sprintf("incident resolved after %s quiet window", c.runtimeConfig(ctx).VerifyQuiet),
	})
}

// verifyEvidenceMaxAge bounds how old runtime evidence may be to support a
// resolution, and the deadline multiplier bounds how long VERIFYING waits
// for evidence before failing closed to a human.
const verifyEvidenceMaxAge = 5 * time.Minute

func (c *Controller) verifyEvidenceDeadline(ctx context.Context) time.Duration {
	// Three quiet windows is enough for a healthy agent to report twice;
	// never below 10 minutes so short dev quiet windows cannot flap
	// incidents into NEEDS_HUMAN on scheduling jitter alone.
	if d := 3 * c.runtimeConfig(ctx).VerifyQuiet; d > 10*time.Minute {
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

// transition applies a validated state change and persists the incident and
// its audit entry in one store transaction (the statemachine contract).
func (c *Controller) transition(ctx context.Context, inc *types.Incident, to types.IncidentState, actor, action, result string, params map[string]string) error {
	from := inc.State
	if err := playbook.Transition(inc, to); err != nil {
		return err
	}
	now := time.Now()
	// fence: the caller's snapshot of when the incident last changed shape.
	// StateChangedAt is the WRITE-FENCE for every non-state field rewrite:
	// ingest never touches it (only UpdatedAt/SignalSeen), while the janitor's
	// quiesce rewind — which moves StepIndex without changing State — bumps
	// it deliberately. Guarding on it below turns a rewind that landed
	// between this caller's ListIncidents snapshot and this transition into a
	// detected conflict instead of a silent un-rewind (the janitors run on
	// their own goroutine since round 8, so this interleaving is real).
	fence := inc.StateChangedAt
	inc.UpdatedAt = now
	inc.StateChangedAt = now
	if to == types.StateResolved {
		inc.ResolvedAt = &now
	}
	// A halting transition clears the durable slot bit in the SAME transaction
	// that halts the incident, and releases the in-memory gate slot only if
	// the FRESH row still held the bit — so two racing halting paths release
	// exactly once (the loser fails fresh.State != from and the version
	// guard), and a crash after commit loses nothing: the bit is durably 0
	// and the dead process's gate memory dies with it.
	release := false
	hadSlotBit := inc.RemediationSlotHeld
	if to.Halted() {
		release = hadSlotBit
		inc.RemediationSlotHeld = false
	}
	if err := c.store.WithTx(ctx, func(tx store.Tx) error {
		// Re-read the row inside the transaction and apply the change to the
		// fresh copy rather than persisting this goroutine's snapshot. A long
		// step runs in its own goroutine off a snapshot captured at
		// ListIncidents; while it ran, a concurrent ingest may have bumped
		// SignalSeen. Writing the stale snapshot back would erase that count
		// (and, under a READ COMMITTED backend, could regress state/StepIndex).
		// The reconcile path owns state and playbook progress; the ingest path
		// owns SignalSeen — so take SignalSeen and the current version from the
		// fresh row, and fail closed if the state OR the fence moved out from
		// under us.
		fresh, err := tx.GetIncident(ctx, inc.ID)
		if err != nil {
			return err
		}
		if fresh.State != from || !fresh.StateChangedAt.Equal(fence) {
			return store.ErrConflict
		}
		if to.Halted() {
			release = release && fresh.RemediationSlotHeld
		}
		inc.SignalSeen = fresh.SignalSeen
		inc.Version = fresh.Version
		if err := tx.UpdateIncident(ctx, inc); err != nil {
			return err
		}
		return tx.AppendAudit(ctx, &types.AuditEntry{
			IncidentID: inc.ID, Time: now,
			FromState: from, ToState: to,
			Actor: actor, Action: action,
			Params: params, Result: result, DryRun: inc.DryRun,
		})
	}); err != nil {
		if to.Halted() {
			// The halting write never committed; restore the caller's snapshot.
			inc.RemediationSlotHeld = hadSlotBit
		}
		return err
	}
	if from == types.StateExecuting {
		// The incident just left EXECUTING. If a per-step slot was seeded for it
		// on leadership acquisition, hand it back now: recovery either re-admits
		// it through the normal admission path (next reconcile) or closes it.
		// No-op for any incident that was not seeded.
		c.releaseRecoveredSlot(inc.ID)
	}
	if release && c.gate != nil {
		// Halted: the remediation is over, whatever route it took — resolved,
		// expired, or parked for a human. Hand back the target's slot.
		c.gate.ReleaseRemediation(inc.Target)
	}
	// Any committed transition ends the arming-in-flight hold epoch: if the
	// incident re-enters EVALUATING later, the propagation grace starts over
	// from its next first observation.
	c.clearArmingHold(inc.ID)
	// Scheduler feedback rides on the committed transition, never on the
	// attempt: a node must not be marked degraded by a transition that lost its
	// conflict check and never happened.
	c.syncDegradedTaint(ctx, inc, from, to)
	if to.Halted() {
		c.recordRecoveryOutcome(ctx, inc, to)
	}
	return nil
}

// recordRecoveryOutcome emits the outcome metrics once per incident, on the
// committed halting transition: how long it was open, whether it came back,
// and how much accelerator capacity was degraded meanwhile. Everything else
// the controller exports is process telemetry; this is what the fleet got
// back, which is the number a capacity owner actually budgets against.
//
// Emitted AFTER the commit so a conflicting transition cannot double-count,
// and only for halting states so an incident is measured exactly once.
func (c *Controller) recordRecoveryOutcome(ctx context.Context, inc *types.Incident, to types.IncidentState) {
	if inc.OpenedAt.IsZero() {
		return
	}
	duration := time.Since(inc.OpenedAt).Seconds()
	if duration < 0 {
		return
	}
	outcome := strings.ToLower(string(to))
	class := string(inc.Class)
	metrics.IncidentDuration.WithLabelValues(class, outcome).Observe(duration)
	if to == types.StateResolved {
		// An incident that never minted an approval round was handled end to
		// end without a human decision — the automation's actual yield.
		unattended := "false"
		if inc.ApprovalEpoch == 0 {
			unattended = "true"
		}
		metrics.IncidentsRecovered.WithLabelValues(class, unattended).Inc()
	}
	metrics.DegradedGPUSeconds.WithLabelValues(class, outcome).
		Add(duration * float64(c.affectedGPUs(ctx, inc)))
}

// affectedGPUs is how many accelerators the incident covered: one for a
// GPU-scoped incident, the node's whole inventory for a node-scoped one.
// Unknown inventory counts as one — undercounting is the honest failure
// direction for a capacity number.
func (c *Controller) affectedGPUs(ctx context.Context, inc *types.Incident) int {
	if inc.Target.IsGPU() {
		return 1
	}
	node, err := c.store.GetNode(ctx, inc.Target.Node)
	if err != nil || node == nil || len(node.GPUs) == 0 {
		return 1
	}
	return len(node.GPUs)
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

// observePolicy reads the observation threshold and window for a class.
// Explicit policy params win; otherwise the detection catalog's declared
// threshold ("XID 13 is actionable after 3 occurrences in 1h") applies, so
// the documented catalog semantics are enforced rather than advisory.
// threshold 0 means "never auto-escalate".
func (c *Controller) observePolicy(ctx context.Context, class types.ProblemClass) (threshold int, window time.Duration) {
	window = defaultObserveWindow
	if catThreshold, catWindow, ok := c.runtimeConfig(ctx).Catalog.ObservePolicy(class); ok {
		threshold, window = catThreshold, catWindow
	}
	pol, ok := c.runtimeConfig(ctx).Engine.PolicyFor(class)
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

func firstNonBlank(ss ...string) string {
	for _, s := range ss {
		if strings.TrimSpace(s) != "" {
			return s
		}
	}
	return ""
}
