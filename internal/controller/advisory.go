package controller

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/kubeneuron/kubeneuron/internal/action"
	"github.com/kubeneuron/kubeneuron/internal/checkpoint"
	"github.com/kubeneuron/kubeneuron/internal/metrics"
	"github.com/kubeneuron/kubeneuron/internal/platform"
	"github.com/kubeneuron/kubeneuron/pkg/types"
)

// The checkpoint advisory (docs/workload-safe-recovery-design.md, S2).
//
// When an incident parks for a human decision and a drain or GPU-workload
// eviction still lies downstream in its playbook, every eligible opted-in
// workload on the node is told so: a closed set of advisory keys, no
// deadline, no wait. The park is already committed when this runs, the round
// has its own budget, and nothing here can delay, block or change the
// decision the human is asked for. The Phase 1 request with its deadline
// still arrives at the disruption step exactly as before.
//
// Idempotency is on the object: one stamp per (Pod UID, incident, round), a
// re-park is a new round and a new stamp, and a restart that re-runs a round
// finds its own stamp and writes nothing. The round ends without the
// disruption when the incident leaves AWAITING_APPROVAL anywhere but into
// its step's execution; the notice is then withdrawn, best effort.

const (
	auditActionAdvisory         = "checkpoint-advisory"
	auditActionAdvisoryWithdraw = "checkpoint-advisory-withdraw"

	// advisoryAuditTimeout caps the audit append of a round that has already
	// spent its budget.
	advisoryAuditTimeout = 2 * time.Second
)

// advisoryCandidates is what a round would notify: the workloads the first
// downstream disruption would touch, filtered by the same eligibility the
// Phase 1 request uses, so the advisory can never reach a Pod the request
// would not. It returns the op the advisory names as the next action.
func (c *Controller) advisoryCandidates(ctx context.Context, policy checkpoint.Policy, inc *types.Incident) (candidates []platform.Workload, listed int, op string, err error) {
	op, force, found := c.downstreamDisruption(ctx, inc)
	if !found {
		return nil, 0, "", nil
	}
	candidates, listed, err = c.checkpointCandidates(ctx, policy, inc, op, force)
	return candidates, listed, op, err
}

// downstreamDisruption finds the first coordinating platform step at or after
// the incident's current step: the disruption an advisory warns about. A
// playbook with none left has nothing to announce.
func (c *Controller) downstreamDisruption(ctx context.Context, inc *types.Incident) (op string, force, found bool) {
	book, ok := c.runtimeConfig(ctx).Engine.Playbook(inc.Playbook)
	if !ok || inc.StepIndex < 0 {
		return "", false, false
	}
	for i := inc.StepIndex; i < len(book.Steps); i++ {
		step := &book.Steps[i]
		def, known := action.ByWire(step.Action)
		if !known || def.Kind != action.KindPlatform || !checkpointOpCoordinates(def.Op) {
			continue
		}
		return def.Op, drainForce(step), true
	}
	return "", false, false
}

// adviseCheckpoint runs one advisory round for the incident's current
// approval epoch. It is called after the park committed. Every outcome is
// counted; one audit row per round names up to five Pods.
func (c *Controller) adviseCheckpoint(ctx context.Context, inc *types.Incident) {
	policy := c.runtimeConfig(ctx).Checkpoint
	if !policy.Enabled || !policy.Advisory.Enabled {
		return
	}
	round := inc.ApprovalEpoch
	if round <= 0 {
		return
	}
	skip := func(ctx context.Context, why string) {
		metrics.CheckpointAdvisories.WithLabelValues(string(checkpoint.AdvisorySkipped)).Inc()
		c.auditAdvisory(ctx, inc, auditActionAdvisory, fmt.Sprintf("round %d: skipped (%s)", round, why))
	}
	budget := policy.Advisory.Budget
	if budget <= 0 {
		budget = checkpoint.DefaultAdvisoryBudget
	}
	roundCtx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()

	if policy.SkipsClass(inc.Class) {
		skip(roundCtx, string(checkpoint.IneligibleClassSkipped)+": "+string(inc.Class))
		return
	}
	checkpointer, ok := c.platform.(platform.WorkloadCheckpointer)
	if !ok {
		skip(roundCtx, "platform "+c.platform.Name()+" does not support checkpoint coordination")
		return
	}
	candidates, listed, op, err := c.advisoryCandidates(roundCtx, policy, inc)
	if err != nil {
		skip(roundCtx, "workload listing failed")
		c.log.Warn("checkpoint advisory: listing workloads failed", "incident", inc.ID, "node", inc.Target.Node, "err", err)
		return
	}
	if op == "" {
		skip(roundCtx, "no drain or GPU-workload eviction downstream of the parked step")
		return
	}
	if len(candidates) == 0 {
		skip(roundCtx, fmt.Sprintf("no eligible workload among %d listed on %s", listed, inc.Target.Node))
		return
	}
	if c.effectiveDryRun(ctx, inc) {
		// A dry run patches nothing and says what it would have done. The
		// live gate is read here, not the stamp the incident was opened with:
		// an installation switched to DryRun after this incident opened must
		// write nothing to a workload, exactly as its steps execute nothing
		// (effectiveDryRun). An incident opened in DryRun is dry for life.
		skip(roundCtx, fmt.Sprintf("dry-run: would have advised %d of %d workload(s) %s before platform.%s",
			len(candidates), listed, checkpointJoin(workloadIDs(candidates)), op))
		return
	}

	adv := checkpoint.Advisory{
		IncidentID: inc.ID, Round: round, AdvisedAt: c.checkpointNowFunc()(),
		Reason: string(inc.Class), NextAction: checkpointNextAction(op),
	}
	var stamped, unreachable []string
	for _, w := range candidates {
		id := checkpointWorkloadID(w)
		if err := checkpointer.AdviseCheckpoint(roundCtx, policy, inc.Target.Node, w, adv); err != nil {
			unreachable = append(unreachable, id)
			metrics.CheckpointAdvisories.WithLabelValues(string(checkpoint.AdvisoryUnreachable)).Inc()
			if !errors.Is(err, platform.ErrCheckpointWorkloadGone) {
				c.log.Warn("checkpoint advisory: stamp failed", "incident", inc.ID, "workload", id, "err", err)
			}
			continue
		}
		stamped = append(stamped, id)
		metrics.CheckpointAdvisories.WithLabelValues(string(checkpoint.AdvisoryStamped)).Inc()
	}
	result := fmt.Sprintf("round %d: advised %d of %d workload(s) %s before platform.%s",
		round, len(stamped), listed, checkpointJoin(stamped), op)
	if len(unreachable) > 0 {
		result += fmt.Sprintf("; %d unreachable %s", len(unreachable), checkpointJoin(unreachable))
	}
	c.auditAdvisory(roundCtx, inc, auditActionAdvisory, result)
}

// withdrawCheckpointAdvisories withdraws the notices of the given round on
// the incident's node after the round ended without its disruption. It is
// best effort end to end: a listing or a stamp that fails is logged and
// counted, and nothing that already happened to the incident depends on it.
// An incident opened in DryRun stamped nothing, so there is nothing to
// withdraw. The live DryRun gate is deliberately NOT consulted here: a notice
// this incident stamped while the installation was Enabled must not be left
// standing because the operator has since switched to DryRun — the
// withdrawal only ever touches a Pod carrying this incident's own stamp of
// this round, and says that the disruption it announced is not coming.
func (c *Controller) withdrawCheckpointAdvisories(ctx context.Context, inc *types.Incident, round int) {
	policy := c.runtimeConfig(ctx).Checkpoint
	if !policy.Enabled || !policy.Advisory.Enabled || round <= 0 || inc.DryRun {
		return
	}
	checkpointer, ok := c.platform.(platform.WorkloadCheckpointer)
	if !ok {
		return
	}
	budget := policy.Advisory.Budget
	if budget <= 0 {
		budget = checkpoint.DefaultAdvisoryBudget
	}
	roundCtx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()

	workloads, err := c.platform.NodeWorkloads(roundCtx, inc.Target.Node)
	if err != nil {
		metrics.CheckpointAdvisories.WithLabelValues(string(checkpoint.AdvisoryUnreachable)).Inc()
		c.log.Warn("checkpoint advisory: withdrawal listing failed", "incident", inc.ID, "node", inc.Target.Node, "err", err)
		return
	}
	at := c.checkpointNowFunc()()
	var withdrawn, unreachable []string
	for _, w := range workloads {
		existing, ok := checkpoint.ParseAdvisory(w.Annotations)
		if !ok || existing.IncidentID != inc.ID || existing.Round != round || checkpoint.IsAdvisoryWithdrawn(w.Annotations) {
			continue
		}
		if !policy.AllowsNamespace(w.Namespace) {
			continue // a stamp the policy could not have written; not ours to touch
		}
		id := checkpointWorkloadID(w)
		done, err := checkpointer.WithdrawAdvisory(roundCtx, policy, inc.Target.Node, w, inc.ID, round, at)
		switch {
		case err != nil:
			unreachable = append(unreachable, id)
			metrics.CheckpointAdvisories.WithLabelValues(string(checkpoint.AdvisoryUnreachable)).Inc()
			if !errors.Is(err, platform.ErrCheckpointWorkloadGone) {
				c.log.Warn("checkpoint advisory: withdrawal failed", "incident", inc.ID, "workload", id, "err", err)
			}
		case done:
			withdrawn = append(withdrawn, id)
			metrics.CheckpointAdvisories.WithLabelValues(string(checkpoint.AdvisoryWithdrawn)).Inc()
		}
	}
	if len(withdrawn) == 0 && len(unreachable) == 0 {
		return // nothing of this round was standing; no row for a non-event
	}
	result := fmt.Sprintf("round %d: withdrew %d advisory notice(s) %s", round, len(withdrawn), checkpointJoin(withdrawn))
	if len(unreachable) > 0 {
		result += fmt.Sprintf("; %d unreachable %s", len(unreachable), checkpointJoin(unreachable))
	}
	c.auditAdvisory(roundCtx, inc, auditActionAdvisoryWithdraw, result)
}

// auditAdvisory records one advisory fact under the system actor, with its
// own action name so the row is distinguishable from the step rows around
// it. A failed append is logged; the metrics have already been counted.
func (c *Controller) auditAdvisory(ctx context.Context, inc *types.Incident, action, result string) {
	// The row outlives the round's budget on purpose: a round that spent its
	// whole budget on an unreachable stamp is exactly the one whose row an
	// operator needs, and nothing is waiting on this append — the park is
	// committed and the human already notified. It gets its own short cap so
	// a stalled store cannot hold the reconcile loop either.
	auditCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), advisoryAuditTimeout)
	defer cancel()
	if err := c.appendAudit(auditCtx, inc, "system", action, result); err != nil {
		c.log.Error("checkpoint advisory audit append failed", "incident", inc.ID, "action", action, "err", err)
	}
}

// auditAdvisoryWithin records one advisory fact under the caller's context:
// the append is capped at advisoryAuditTimeout like auditAdvisory's, but it
// also ends when ctx does. It is the variant for rows written INSIDE a
// disruption step's coordination window (the group rows), where the window
// is the step's bound and nothing may outlive it — least of all an audit
// append per group after the budget is spent.
func (c *Controller) auditAdvisoryWithin(ctx context.Context, inc *types.Incident, action, result string) {
	auditCtx, cancel := context.WithTimeout(ctx, advisoryAuditTimeout)
	defer cancel()
	if err := c.appendAudit(auditCtx, inc, "system", action, result); err != nil {
		c.log.Error("checkpoint advisory audit append failed", "incident", inc.ID, "action", action, "err", err)
	}
}

func workloadIDs(workloads []platform.Workload) []string {
	ids := make([]string, 0, len(workloads))
	for _, w := range workloads {
		ids = append(ids, checkpointWorkloadID(w))
	}
	sort.Strings(ids)
	return ids
}
