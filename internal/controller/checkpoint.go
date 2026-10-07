package controller

// This file is the checkpoint coordination PRE-PHASE of the two disruption
// steps, platform.drain and platform.evict_gpu_workload
// (docs/checkpoint-coordination-design.md). It is a property of those steps,
// not a playbook action: a workload that opted in is told, given a bounded
// deadline, and then disrupted exactly as before. Nothing here can make the
// disruption later than the policy allows, and nothing here can fail the step.
//
// The rule every branch below enforces: a workload may influence HOW it dies,
// never WHETHER. Every error on this path is counted and then proceeds; the
// audit row describing it is attempted under the bounded coordination context
// and may be dropped (and logged) once the budget is spent.

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/kubeneuron/kubeneuron/internal/checkpoint"
	"github.com/kubeneuron/kubeneuron/internal/metrics"
	"github.com/kubeneuron/kubeneuron/internal/platform"
	"github.com/kubeneuron/kubeneuron/internal/playbook"
	"github.com/kubeneuron/kubeneuron/pkg/types"
)

// checkpointBudgetReserve is the part of a disruption step's own budget that
// coordination may never spend, so the disruption it precedes still has time
// to happen. It covers the drain's evictionGraceMargin (30s of slack between an
// eviction's grace period and the step deadline) plus the Kubernetes default
// pod termination grace period (30s), with the same again on top so a wait
// that ends at its deadline is not followed by a drain that starts already
// out of time. When the step budget minus this reserve is not positive, no
// request goes out at all and the step proceeds untouched.
//
// The reserve is kept by construction, not by arithmetic on the deadline
// alone: everything the pre-phase does with the platform or the store once
// the budget is known, from listing the node to the last audit row, runs
// under a coordination context that expires when the budget is spent
// (coordinateCheckpoint), so a slow listing, a hung patch, a stalled read or
// a slow audit append consumes the budget and never the reserve. Nothing in
// the pre-phase borrows time from the step's own context, not even the row
// that records that there was no budget to spend. The disruption then runs
// under that context, which still has at least the reserve left.
//
// It is a floor on what the disruption keeps, not a promise about the pod: a
// tenant-declared terminationGracePeriodSeconds longer than what is left is
// handled by the drain itself, which declines to clamp below the platform
// default rather than force-deleting. Coordination never shortens that.
const checkpointBudgetReserve = 90 * time.Second

// defaultCheckpointPollInterval paces the observation loop while a request is
// outstanding. Each pass is one live read per pending workload.
const defaultCheckpointPollInterval = 2 * time.Second

// checkpointAuditWorkloadLimit bounds how many workload identifiers one audit
// row names. The rest are counted, so a node running hundreds of pods cannot
// turn an audit row into a page.
const checkpointAuditWorkloadLimit = 5

// checkpointNextAction is the wire name a request stamps as the disruption
// that follows, so a workload reading its annotations knows what is coming.
func checkpointNextAction(op string) string { return "platform." + op }

// checkpointOpCoordinates reports whether a platform operation carries the
// coordination pre-phase. Only the two disruption steps do.
func checkpointOpCoordinates(op string) bool {
	return op == "drain" || op == "evict_gpu_workload"
}

// checkpointNowFunc is the clock the coordination pre-phase stamps and
// classifies with. Tests substitute a controllable one; production uses the
// wall clock.
func (c *Controller) checkpointNowFunc() func() time.Time {
	if c.checkpointNow != nil {
		return c.checkpointNow
	}
	return time.Now
}

func (c *Controller) checkpointPollInterval() time.Duration {
	if c.checkpointPoll > 0 {
		return c.checkpointPoll
	}
	return defaultCheckpointPollInterval
}

// checkpointBudget is the time coordination may spend before the disruption:
// the step's INTENDED budget minus the reserve above. The intended budget is
// the step timeout, further bounded by what is actually left on ctx minus the
// agent-result grace the executeStep context adds on top of that timeout, so
// the grace is never spent on waiting. Zero or negative means "do not ask".
func checkpointBudget(ctx context.Context, step *playbook.Step) time.Duration {
	budget := effectiveStepTimeout(step)
	if deadline, ok := ctx.Deadline(); ok {
		if remaining := time.Until(deadline) - agentResultGrace; remaining < budget {
			budget = remaining
		}
	}
	return budget - checkpointBudgetReserve
}

// checkpointCandidates lists the node's workloads and filters them down to the
// ones a request would go to: exactly the workloads the disruption that
// follows will touch, then whatever the pure policy accepts for this
// incident's class. For drain that is the platform's own drain eligibility
// under the step's force setting (Workload.DrainEligible), so a pod the drain
// will skip — finished, a mirror or DaemonSet pod, or an unmanaged pod on a
// non-forced drain — is neither asked nor waited for. For evict_gpu_workload
// it is the GPU holders, which that step evicts whatever their drain
// exclusion. The count of what was listed comes back too, for the audit row.
func (c *Controller) checkpointCandidates(ctx context.Context, policy checkpoint.Policy, inc *types.Incident, op string, force bool) (candidates []platform.Workload, listed int, err error) {
	workloads, err := c.platform.NodeWorkloads(ctx, inc.Target.Node)
	if err != nil {
		return nil, 0, err
	}
	for _, w := range workloads {
		switch op {
		case "evict_gpu_workload":
			if !w.UsesGPU {
				continue
			}
		case "drain":
			if !w.DrainEligible(force) {
				continue
			}
		}
		if policy.Eligibility(w.Checkpoint(), inc.Class) != "" {
			continue
		}
		candidates = append(candidates, w)
	}
	return candidates, len(workloads), nil
}

// checkpointDryRunProjection is the read-only half of the pre-phase for a
// simulated step: how many workloads a real run would have asked. It never
// patches, waits, or counts. A disabled policy and a class the policy skips
// cost no platform call at all; anything that goes wrong leaves the dry run
// successful and says the projection is unavailable.
func (c *Controller) checkpointDryRunProjection(ctx context.Context, inc *types.Incident, op string, step *playbook.Step) string {
	if !checkpointOpCoordinates(op) {
		return ""
	}
	policy := c.runtimeConfig(ctx).Checkpoint
	if !policy.Enabled {
		return ""
	}
	if policy.SkipsClass(inc.Class) {
		return fmt.Sprintf("; checkpoint coordination would be skipped (%s)", checkpoint.IneligibleClassSkipped)
	}
	if c.platform == nil {
		return "; checkpoint projection unavailable (no platform configured)"
	}
	if _, ok := c.platform.(platform.WorkloadCheckpointer); !ok {
		return "; checkpoint coordination would be skipped (platform does not support it)"
	}
	candidates, listed, err := c.checkpointCandidates(ctx, policy, inc, op, drainForce(step))
	if err != nil {
		c.log.Warn("checkpoint dry-run projection unavailable", "incident", inc.ID, "node", inc.Target.Node, "err", err)
		return "; checkpoint projection unavailable (workload listing failed)"
	}
	return fmt.Sprintf("; checkpoint coordination would request %d candidate workload(s) of %d listed",
		len(candidates), listed)
}

// checkpointTracked is one workload a request went out to, or one carrying a
// live request from another incident that this step elected to honor.
type checkpointTracked struct {
	workload platform.Workload
	// incidentID is the incident of the request IN FORCE on the object: this
	// step's own, or the other incident's when its live request was honored.
	// An acknowledgement is bound to it (checkpoint.Acknowledges): the
	// workload answers the request it read, so a stale acknowledgement of
	// an earlier incident, or a plain "complete", never settles this one. For
	// a foreign request the value was read off the object and is used for
	// that comparison only; it is never logged or audited.
	incidentID string
	// foreign is true when the request in force is another incident's. Every
	// field of such an entry other than workload is derived from annotation
	// values a tenant could have written, and the audit row for the phase
	// must then render none of them (checkpointRequestedAudit).
	foreign bool
	// requestedAt is the durable start of the window the wait metric measures
	// from: the RequestedAt of the request actually in force on the object
	// when it is present and trustworthy, otherwise the time this step issued
	// its own request. It is never after this step's request time and never
	// more than the ceiling before it (checkpoint.RequestStart); the elapsed
	// observation itself is bounded separately (checkpointElapsed), since a
	// settlement after the request time can still push a floored start past
	// the ceiling.
	requestedAt time.Time
	// deadline is absolute and already bounded: never later than the deadline
	// this step proposed for THIS workload, which is itself bounded by the
	// workload's own policy grant and the step budget.
	deadline time.Time
	outcome  checkpoint.Outcome
	settled  time.Time
	// done is set once outcome is final. It is kept apart from
	// Outcome.Settled because an unreachable answer AT the deadline is final
	// for this step while the pure outcome, on its own, is not.
	done bool
}

// inForce is the request the workload is held to, as checkpoint.Classify
// wants it: the incident an acknowledgement must name and the bounded deadline.
func (tr *checkpointTracked) inForce() checkpoint.Request {
	return checkpoint.Request{IncidentID: tr.incidentID, DeadlineAt: tr.deadline}
}

// checkpointSummary is what the pre-phase reports back to the step.
type checkpointSummary struct {
	counts map[checkpoint.Outcome]int
	// waited is true only when the step really slept before disrupting, which
	// is the one condition under which the deferral metric counts.
	waited bool
}

// coordinateCheckpoint runs the pre-phase for one disruption step and returns
// once every request has settled, its deadline passed, or the coordination
// budget is spent. It never returns an error: the step's own disruption
// follows whatever happened here.
//
// A disabled policy returns before touching the platform, the store, or a
// metric, so an installation that never asked for the feature runs exactly the
// code it ran before.
//
// Everything after the budget is known runs under a coordination context that
// expires when the budget does: the listing, every request, every audit row
// of the phase, every observation and every sleep between them. That, not the
// deadline arithmetic alone, is what keeps checkpointBudgetReserve for the
// disruption: a platform call that blocks cannot outlive the budget, and the
// step's own ctx, which the disruption then runs under, still has the reserve.
// No pre-phase work ever runs under the step's own ctx, the audit rows
// included: a row whose turn comes after the budget is spent is attempted
// with the spent context, so it fails fast and is logged rather than
// persisted. When there is no budget at all there is no coordination context
// either, and the one row that says so is attempted under an already
// cancelled child of the step's ctx for the same reason. The audit trail of
// the pre-phase is best effort in exactly those cases; the reserve is not.
func (c *Controller) coordinateCheckpoint(ctx context.Context, inc *types.Incident, step *playbook.Step, op string) checkpointSummary {
	summary := checkpointSummary{counts: map[checkpoint.Outcome]int{}}
	if !checkpointOpCoordinates(op) {
		return summary
	}
	policy := c.runtimeConfig(ctx).Checkpoint
	if !policy.Enabled {
		return summary
	}
	skip := func(ctx context.Context, why string) checkpointSummary {
		summary.counts[checkpoint.OutcomeSkipped]++
		metrics.CheckpointRequests.WithLabelValues(string(checkpoint.OutcomeSkipped)).Inc()
		c.auditCheckpoint(ctx, inc, step, "checkpoint: skipped ("+why+")")
		return summary
	}
	budget := checkpointBudget(ctx, step)
	if budget <= 0 {
		// There is no coordination window at all, so there is nothing the
		// row that says so may spend either. It is attempted under a child
		// of the step's context that is already cancelled: the append fails
		// fast and is logged if the store is not immediately writable, and
		// it can never borrow the step's remaining time, which belongs to the
		// disruption in full. This row is therefore best effort and may be
		// missing from the trail; the skipped count is always recorded.
		noBudgetCtx, cancel := context.WithCancel(ctx)
		cancel()
		return skip(noBudgetCtx, fmt.Sprintf("step budget %v leaves nothing to wait with after the %v reserve for the disruption itself",
			effectiveStepTimeout(step).Round(time.Second), checkpointBudgetReserve))
	}

	// From here on the pre-phase runs under its own deadline, the budget:
	// the skip rows below, the listing, the requests, the wait and the rows
	// that record them. The budget is spent on whatever happens first, and
	// nothing here borrows from the step's own context afterwards.
	//
	// The request time is taken HERE, with the context, and is the one start
	// every deadline of this step is measured from: the coordination window
	// is [requestedAt, requestedAt+budget] on the same clock the stamps use,
	// so a slow listing or a slow patch spends the window and never resets
	// it. A request stamped later in the phase promises the workload no more
	// than what is left of that window.
	now := c.checkpointNowFunc()
	requestedAt := now()
	budgetEnd := requestedAt.Add(budget)
	coordCtx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()

	if policy.SkipsClass(inc.Class) {
		return skip(coordCtx, string(checkpoint.IneligibleClassSkipped)+": "+string(inc.Class))
	}
	checkpointer, ok := c.platform.(platform.WorkloadCheckpointer)
	if !ok {
		return skip(coordCtx, "platform "+c.platform.Name()+" does not support checkpoint coordination")
	}

	candidates, listed, err := c.checkpointCandidates(coordCtx, policy, inc, op, drainForce(step))
	if err != nil {
		// Coordination could not even see the node. That is one unreachable
		// answer for the step, with an audit row attempted under the budget,
		// and the disruption proceeds within its own budget: a listing
		// failure must never hold or fail a remediation.
		c.log.Warn("checkpoint: workload listing failed; proceeding without coordination",
			"incident", inc.ID, "node", inc.Target.Node, "err", err)
		summary.counts[checkpoint.OutcomeUnreachable]++
		metrics.CheckpointRequests.WithLabelValues(string(checkpoint.OutcomeUnreachable)).Inc()
		c.auditCheckpoint(coordCtx, inc, step, "checkpoint: workload listing failed; proceeding without coordination")
		return summary
	}
	if len(candidates) == 0 {
		return skip(coordCtx, fmt.Sprintf("no eligible workloads on %s; %d listed", inc.Target.Node, listed))
	}

	// One request time for the whole step, the one taken with the context
	// above. The stamp carries it at full precision
	// (checkpoint.TimestampLayout), so the durable stamp and what this step
	// remembers about it are exactly equal after a round trip through the
	// object, sub-second windows included. Each workload then gets its OWN
	// deadline: its policy grant (its request, or the default, never more
	// than maxWait), bounded by the step budget, all from that one request
	// time, so no stamped deadline is later than the end of the coordination
	// window however long the listing or the earlier patches took. A workload
	// asking for 30s next to one granted 5m gets 30s; the step as a whole
	// waits until the latest of those deadlines. A candidate whose turn comes
	// after the window has closed is not asked at all: there is no time left
	// to promise, and the disruption is what follows.
	nextAction := checkpointNextAction(op)

	var tracked []*checkpointTracked
	var unrequested []string
	for _, w := range candidates {
		if coordCtx.Err() != nil {
			summary.counts[checkpoint.OutcomeUnreachable]++
			metrics.CheckpointRequests.WithLabelValues(string(checkpoint.OutcomeUnreachable)).Inc()
			unrequested = append(unrequested, checkpointWorkloadID(w)+"="+string(checkpoint.OutcomeUnreachable))
			continue
		}
		req := checkpoint.Request{
			IncidentID: inc.ID, RequestedAt: requestedAt,
			DeadlineAt: requestedAt.Add(policy.BoundedWait(w.Checkpoint(), budget)),
			Reason:     inc.Class, NextAction: nextAction,
		}
		tr, outcome := c.requestCheckpoint(coordCtx, checkpointer, policy, inc, w, req)
		switch outcome {
		case "":
			tracked = append(tracked, tr)
		default:
			summary.counts[outcome]++
			metrics.CheckpointRequests.WithLabelValues(string(outcome)).Inc()
			unrequested = append(unrequested, checkpointWorkloadID(w)+"="+string(outcome))
		}
	}
	if len(unrequested) > 0 {
		c.auditCheckpoint(coordCtx, inc, step, "checkpoint: no request in force for "+checkpointJoin(unrequested))
	}
	if len(tracked) == 0 {
		return summary
	}

	// The wait ends at the LATEST deadline in force, which may already be in the
	// past when every request resumed an expired window.
	latest := tracked[0].deadline
	var names []string
	anyForeign := false
	for _, tr := range tracked {
		if tr.deadline.After(latest) {
			latest = tr.deadline
		}
		anyForeign = anyForeign || tr.foreign
		names = append(names, checkpointWorkloadID(tr.workload))
	}
	c.auditCheckpoint(coordCtx, inc, step, checkpointRequestedAudit(len(tracked), names, anyForeign, latest, requestedAt, nextAction))

	// Observe until everything settles, the latest bounded deadline passes, or
	// the coordination budget is spent. The first look happens before any
	// sleep, so a request whose deadline is already behind us — a controller
	// restart resuming an expired window — settles immediately and never
	// starts a countdown of its own. Each sleep is the poll interval, cut to
	// what is left until the latest deadline and to what is left of the
	// window, and ends early when the coordination context does. A context
	// that has ended is never used for another read: the answer would only
	// be its own error, and each such read would be one more call after the
	// budget was spent.
	poll := c.checkpointPollInterval()
	for coordCtx.Err() == nil {
		pending := 0
		at := now()
		for _, tr := range tracked {
			if tr.done || coordCtx.Err() != nil {
				continue
			}
			obs := checkpointer.ObserveCheckpoint(coordCtx, policy, inc.Target.Node, tr.workload)
			outcome := checkpoint.Classify(at, tr.inForce(), tr.workload.Checkpoint(), obs)
			switch {
			case outcome.Settled():
			case outcome == checkpoint.OutcomeUnreachable && !at.Before(tr.deadline):
				// A read that failed says nothing about the workload, so before
				// the deadline it is looked at again on the next pass. AT the
				// deadline the failure is the workload's final answer: the same
				// bounded wait, never a longer one.
			default:
				pending++
				continue
			}
			tr.outcome, tr.settled, tr.done = outcome, at, true
		}
		if pending == 0 || !at.Before(latest) || coordCtx.Err() != nil {
			break
		}
		// waited is set only once a positive sleep begins: a sleep the budget
		// or the step then cuts short still paused the disruption and still
		// counts as a deferral. A sleep that would be zero or negative — the
		// logical window already reached on the coordinator's clock — is no
		// wait at all: nothing pauses, nothing is deferred, the loop ends.
		d := checkpointSleepFor(at, latest, budgetEnd, poll)
		if d <= 0 {
			break
		}
		summary.waited = true
		if !checkpointSleep(coordCtx, d) {
			break
		}
	}
	if summary.waited {
		c.deferStep(inc, step, metrics.DeferCheckpointWait)
	}

	// Whatever is still open here had its window end without an answer being
	// observed. When its own stamped deadline has passed, it expired: the
	// workload was given its whole window and did not answer. When the
	// deadline has not passed, the workload was not observed for a reason of
	// this step's, not its own: the coordination budget ran out first (the
	// wall clock, or a read that outlived it), or the step itself was
	// cancelled. Either way nothing more is known about the workload and it
	// is unreachable, never expired: the stamp still promises it time that
	// this step, not the workload, gave up.
	var lines []string
	settledAt := now()
	for _, tr := range tracked {
		if !tr.done {
			tr.outcome, tr.settled, tr.done = checkpoint.OutcomeUnreachable, settledAt, true
			if !settledAt.Before(tr.deadline) && ctx.Err() == nil {
				tr.outcome = checkpoint.OutcomeExpired
			}
		}
		summary.counts[tr.outcome]++
		metrics.CheckpointRequests.WithLabelValues(string(tr.outcome)).Inc()
		metrics.CheckpointWaitSeconds.Observe(checkpointElapsed(tr.requestedAt, tr.settled).Seconds())
		lines = append(lines, checkpointWorkloadID(tr.workload)+"="+string(tr.outcome))
	}
	// Under the coordination context on purpose: this row is a courtesy and
	// the reserve is not. Once the budget is spent the append fails fast and
	// is logged; the outcomes above are already counted.
	c.auditCheckpoint(coordCtx, inc, step, fmt.Sprintf("checkpoint: complete after %v: %s; proceeding with %s",
		settledAt.Sub(requestedAt).Round(time.Millisecond), checkpointJoin(lines), nextAction))
	return summary
}

// checkpointRequestedAudit renders the row that opens the wait. When every
// request in force is this step's own, the row carries the deadline and the
// wait, both of which this step computed. When ANY tracked request is another
// incident's, the latest deadline, and the wait derived from it, is a value
// read off a tenant-writable object: the row then says only that a live
// request is being honored, and renders none of the foreign incident's ID,
// reason, next action, request time, deadline or the wait derived from it.
// The deadline is still the one the loop waits to; it is only not written.
func checkpointRequestedAudit(count int, names []string, anyForeign bool, latest, requestedAt time.Time, nextAction string) string {
	if anyForeign {
		return fmt.Sprintf("checkpoint: coordinating %d workload(s) %s; honoring an already-live request before %s",
			count, checkpointJoin(names), nextAction)
	}
	return fmt.Sprintf("checkpoint: requested %d workload(s) %s; deadline %s (wait %v) before %s",
		count, checkpointJoin(names), latest.UTC().Format(checkpoint.TimestampLayout), latest.Sub(requestedAt).Round(time.Millisecond), nextAction)
}

// checkpointSleepFor is the next sleep of the observation loop as seen from
// at: the poll interval, cut to what is left until the latest deadline in
// force and to what is left of the coordination window. Zero or negative
// means the window is already spent on this clock and nothing is to be slept.
func checkpointSleepFor(at, latest, budgetEnd time.Time, poll time.Duration) time.Duration {
	return min(poll, latest.Sub(at), budgetEnd.Sub(at))
}

// checkpointSleep sleeps for d or until ctx is done, whichever is first, and
// reports whether the full sleep happened. A non-positive d does not sleep;
// the loop treats such a d as no wait at all and never reaches here with it.
func checkpointSleep(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

// requestCheckpoint stamps one request and returns the workload as tracked,
// with the deadline and durable request start in force, or the outcome that
// settles the workload without a request. The empty outcome means "tracked".
//
// A conflict is retried at most once; the adapter reads the object live on
// every call, so the retry IS the fresh read. A live request from another
// incident is never overwritten and never granted a new window: the deadline
// it already carries is honored, capped to the one this step proposed for
// this workload, so nothing on the object can extend this step's wait. Any
// other failure counts the workload unreachable and waits for nothing.
//
// The foreign request honored is the one the adapter parsed off the object on
// the read that refused this step's request, never the one in the listing
// snapshot w was taken from: between the listing and that read another
// incident may have stamped the object, and a wait bound to the earlier
// incident would read a retained acknowledgement of it as an answer to a
// question no longer in force. w.Annotations are not consulted on this path.
//
// Nothing read off the object is ever logged: the foreign incident's ID, its
// deadline, its reason and its next action are annotation values a tenant
// with patch rights could have written, and the log names only the workload by
// namespace/name and this step's own incident. The returned request is kept
// as values compared and waited to, never formatted.
func (c *Controller) requestCheckpoint(ctx context.Context, checkpointer platform.WorkloadCheckpointer, policy checkpoint.Policy, inc *types.Incident, w platform.Workload, req checkpoint.Request) (*checkpointTracked, checkpoint.Outcome) {
	var (
		inForce checkpoint.Request
		err     error
	)
	for attempt := 0; attempt < 2; attempt++ {
		inForce, err = checkpointer.RequestCheckpoint(ctx, policy, inc.Target.Node, w, req)
		if !errors.Is(err, platform.ErrCheckpointConflict) || ctx.Err() != nil {
			// A conflict is retried once, but not after the coordination
			// window closed: no call goes out on a spent context.
			break
		}
	}
	switch {
	case err == nil:
		// A resumed same-incident request comes back with the original
		// RequestedAt from the object, so the wait metric spans the whole
		// durable window rather than only this retry's part of it. The
		// acknowledgement is bound to this step's own incident: that is the
		// value the stamp names, whichever of the two windows is in force.
		return &checkpointTracked{
			workload:    w,
			incidentID:  inc.ID,
			requestedAt: checkpoint.RequestStart(inForce.RequestedAt, req.RequestedAt),
			deadline:    checkpointCapDeadline(inForce.DeadlineAt, req.DeadlineAt),
		}, ""
	case errors.Is(err, platform.ErrCheckpointWorkloadGone), errors.Is(err, platform.ErrCheckpointScope):
		// Gone, replaced, or moved: the instance the decision was made about is
		// no longer there to ask, which is the zero-RBAC acknowledgement.
		return nil, checkpoint.OutcomeExited
	case errors.Is(err, platform.ErrCheckpointForeignRequest):
		// inForce is the live foreign request the adapter read; the contract
		// promises it, and each condition is still checked here so an adapter
		// that returned nothing, this step's own incident, or an already
		// expired window binds nothing: there is then no live request of
		// record to honor, and the workload is unreachable, not waited for.
		if inForce.IncidentID != "" && inForce.IncidentID != inc.ID && inForce.DeadlineAt.After(req.RequestedAt) {
			// The workload was asked by the other incident and will answer
			// that request, so the acknowledgement is bound to the incident
			// on the object. The ID is compared, never written anywhere.
			tr := &checkpointTracked{
				workload:    w,
				incidentID:  inForce.IncidentID,
				foreign:     true,
				requestedAt: checkpoint.RequestStart(inForce.RequestedAt, req.RequestedAt),
				deadline:    checkpointCapDeadline(inForce.DeadlineAt, req.DeadlineAt),
			}
			c.log.Info("checkpoint: honoring another incident's live request",
				"incident", inc.ID, "workload", checkpointWorkloadID(w))
			return tr, ""
		}
		c.log.Warn("checkpoint: request refused", "incident", inc.ID, "workload", checkpointWorkloadID(w), "err", err)
		return nil, checkpoint.OutcomeUnreachable
	default:
		c.log.Warn("checkpoint: request failed", "incident", inc.ID, "workload", checkpointWorkloadID(w), "err", err)
		return nil, checkpoint.OutcomeUnreachable
	}
}

// checkpointCapDeadline bounds a deadline read back from an object to the one
// this step proposed. It repeats what checkpoint.ResolveDeadline already
// guarantees for a same-incident stamp, and extends the guarantee to a foreign
// one and to any adapter that returns something later than it was asked for.
func checkpointCapDeadline(deadline, proposed time.Time) time.Time {
	if deadline.IsZero() || deadline.After(proposed) {
		return proposed
	}
	return deadline
}

// checkpointElapsed is the duration the wait metric observes for one
// workload: from its durable request start until its outcome settled, bounded
// to [0, checkpoint.MaxWaitCeiling]. The start is no later than this step's
// request time and the clock is the same one on both ends, so a negative
// value is not expected; the ceiling is needed, not merely kept: a stale or
// tampered requested-at is floored one ceiling before the request time, and
// the wait this step then adds on top of it would otherwise observe past the
// ceiling and the histogram's top bucket. A valid same-incident resume never
// spans more than maxWait, which the ceiling bounds, so it is unaffected.
func checkpointElapsed(requestedAt, settled time.Time) time.Duration {
	return checkpoint.ObservedWait(requestedAt, settled)
}

// checkpointWorkloadID is the bounded identifier an audit row uses for a
// workload: namespace/name, never an annotation value.
func checkpointWorkloadID(w platform.Workload) string {
	return w.Namespace + "/" + w.Name
}

// checkpointJoin renders a bounded, sorted list of identifiers for an audit row.
func checkpointJoin(ids []string) string {
	sorted := append([]string(nil), ids...)
	sort.Strings(sorted)
	if len(sorted) > checkpointAuditWorkloadLimit {
		return strings.Join(sorted[:checkpointAuditWorkloadLimit], ", ") +
			fmt.Sprintf(" (+%d more)", len(sorted)-checkpointAuditWorkloadLimit)
	}
	return strings.Join(sorted, ", ")
}

// auditCheckpoint records one coordination fact under the system actor. The
// existing step start and outcome rows are untouched; these sit between them.
func (c *Controller) auditCheckpoint(ctx context.Context, inc *types.Incident, step *playbook.Step, result string) {
	if err := c.appendAudit(ctx, inc, "system", step.Name, result); err != nil {
		c.log.Error("checkpoint audit append failed", "incident", inc.ID, "step", step.Name, "err", err)
	}
}
