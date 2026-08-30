package controller

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/kubeneuron/kubeneuron/internal/action"
	"github.com/kubeneuron/kubeneuron/internal/store"
	"github.com/kubeneuron/kubeneuron/pkg/types"
)

// This file implements httpapi.RecoveryReportBackend: the capacity-owner view
// of a window — degraded GPU-hours, the share that came back, the share that
// came back without a human, cost by class, and what is still open.
//
// It is computed from the incident store rather than from the Prometheus
// series that carry the same three outcomes, for four reasons: the store is
// the ground truth those series are derived from (so the numbers are exact,
// not bucket-interpolated); a fresh install has a store on day one and may
// have no metrics backend at all; the report must survive a counter reset,
// which a range query over a restarted process cannot; and the GPU-hour
// arithmetic needs node inventory to expand a node-scoped incident, which
// only the controller can join. The Prometheus series remain the right tool
// for dashboards, where the question is a shape over time.
//
// The aggregation is deliberately a pure function of (incidents, inventory,
// window) so the definitions below are testable without a database, and so a
// reviewer can check the arithmetic against a table instead of a query plan.

// maxReportWindow bounds the requested window. Retention prunes terminal
// incidents long before this, so a longer window cannot return more truth —
// it can only claim a completeness the store does not have.
const maxReportWindow = 366 * 24 * time.Hour

// maxReportIncidents bounds how many incident rows one report will hold in
// memory. Sized for a large fleet's normal year — roughly 270 incidents a day
// across the whole installation — so reaching it means either an unusually
// wide window or a fault storm worth looking at directly.
const maxReportIncidents = 100_000

// RecoveryReport aggregates the incident store over the trailing window. The
// window is anchored to the controller's clock, which is the clock that
// stamped the incident rows: a caller passing a duration therefore gets a
// self-consistent answer even if its own clock has drifted, and the resolved
// [From, To] bounds are returned so the numbers can be reproduced.
func (c *Controller) RecoveryReport(ctx context.Context, window time.Duration) (*types.RecoveryReport, error) {
	if window <= 0 {
		return nil, fmt.Errorf("recovery report window must be positive")
	}
	if window > maxReportWindow {
		return nil, fmt.Errorf("recovery report window %s exceeds the %s retention horizon", window, maxReportWindow)
	}
	to := time.Now()
	from := to.Add(-window)
	// Bounded read, deliberately fail-loud. This query has no natural size: a
	// 500-node fleet over the 366-day horizon can hold far more incidents than
	// belong in one process's memory at once. The tempting fix — a plain LIMIT
	// — is the one thing that must not happen here, because a truncated set
	// still aggregates cleanly and hands back a capacity number that looks
	// authoritative and is simply wrong. Ask for one row more than the cap so
	// hitting it is detectable, and say so instead of answering.
	incidents, err := c.store.ListIncidents(ctx, store.IncidentFilter{
		ActiveSince: from,
		Limit:       maxReportIncidents + 1,
	})
	if err != nil {
		return nil, err
	}
	if len(incidents) > maxReportIncidents {
		return nil, fmt.Errorf(
			"the window %s covers more than %d incidents, which is more than this report will total in one pass; "+
				"narrow the window (a truncated total would be wrong rather than merely incomplete)",
			window, maxReportIncidents)
	}
	nodes, err := c.store.ListNodes(ctx)
	if err != nil {
		return nil, err
	}
	// One inventory read for the whole report: expanding a node-scoped
	// incident per incident would issue a query per row and still answer the
	// same question.
	gpusPerNode := make(map[string]int, len(nodes))
	for _, n := range nodes {
		gpusPerNode[n.Name] = len(n.GPUs)
	}
	// And one audit read for the whole report, asked only about the incidents
	// whose answer can change a number: RESOLVED is the only state that counts
	// toward recovery, so nothing else needs its trail read.
	resolved := make([]string, 0, len(incidents))
	for _, inc := range incidents {
		if inc != nil && inc.State == types.StateResolved {
			resolved = append(resolved, inc.ID)
		}
	}
	executed, err := c.store.ExecutedStepResults(ctx, resolved)
	if err != nil {
		// Fail loud. Without the audit there is no way to tell a repair from an
		// incident that merely aged out quietly, and the difference is the whole
		// meaning of the headline number — answering anyway would hand back a
		// figure that looks authoritative and may be entirely observation.
		return nil, fmt.Errorf("reading remediation evidence from the audit trail: %w", err)
	}
	remediated := make(map[string]bool, len(executed))
	for id, results := range executed {
		remediated[id] = remediationExecuted(results)
	}
	return aggregateRecovery(incidents, gpusPerNode, remediated, from, to), nil
}

// auditExecutingResult is the Result text written on every transition INTO
// EXECUTING, and remediationExecuted is the only reader of that format. They
// live together because they are one contract: the audit row's Action column
// carries the STEP NAME, which a playbook author chooses freely ("record",
// "notify", "reset"), so the wire action is recoverable from nowhere else.
//
// This is what lets the report tell a repair from an observation without a
// schema migration: the audit already carries the fact, it was simply never
// read back.
const (
	auditExecutingPrefix = "executing "
	// auditSimulatingPrefix marks a step the controller entered EXECUTING for
	// and then simulated, because the LIVE gate said dry-run.
	//
	// The distinction has to be durable and per-step. Execution follows the
	// live gate while the incident's own dry-run flag is stamped once, at open
	// — so an incident opened Enabled and switched to DryRun mid-ladder
	// executes nothing at all and still carries DryRun=false. Every accounting
	// read keyed on that flag then folds it into the REAL report: the fleet is
	// told it got its GPU-hours back from ladders that touched nothing. That is
	// precisely the number the observed-only bucket exists to protect, reached
	// one layer down.
	auditSimulatingPrefix = "simulating "
)

func auditExecutingResult(wireAction string) string { return auditExecutingPrefix + wireAction }

func auditStepResult(wireAction string, simulated bool) string {
	if simulated {
		return auditSimulatingPrefix + wireAction
	}
	return auditExecutingResult(wireAction)
}

// remediationExecuted reports whether an incident's EXECUTING audit rows prove
// that something was actually DONE to the fleet.
//
// The test is the action registry's Kind, which already divides the actions
// that change the fleet from the two that do not. A notify step records and
// tells somebody; a verify step reads health back. Neither is a repair, and a
// ladder made only of those — the shipped observe-first ones, and the
// two-step Observe+VerifyGPUHealth example in config/samples — reaches
// RESOLVED having changed nothing at all. Counting that as returned capacity
// is the same claim as counting a class with no playbook, made one layer down.
//
// Anything this build cannot classify is counted as remediation: a row written
// by an older controller, or one naming an action this binary no longer has.
// "A step ran, we cannot say which" honestly reads as a step having run, and
// guessing the other way would retroactively rewrite an existing
// installation's whole history into observation. The headline defect — a class
// with no playbook at all — produces no EXECUTING row whatsoever, so it is
// caught regardless.
// A step that entered EXECUTING and then FAILED still counts, and that is
// deliberate rather than an oversight. The entry row is written before the step
// runs, so a failure leaves it behind — but a failed step never resolves an
// incident on its own: escalate() either quarantines it (NEEDS_HUMAN, which is
// not RESOLVED and so never reaches this function) or climbs to the next rung,
// whose own execution writes its own entry row. The only way a failed step's
// row is the sole evidence is a ladder that escalated to an observe-only rung
// and then quiet-resolved — and there, remediation genuinely did run and
// genuinely did disrupt the node before the fault stopped recurring. Reporting
// that as recovered is a different claim from the one this function exists to
// refuse, which is an incident that changed nothing whatsoever.
//
// A step the controller SIMULATED is not remediation either, however the
// incident was stamped when it opened. Those rows carry their own prefix and
// are skipped here — see auditSimulatingPrefix.
func remediationExecuted(results []string) bool {
	for _, result := range results {
		if strings.HasPrefix(result, auditSimulatingPrefix) {
			continue
		}
		wire, formatted := strings.CutPrefix(result, auditExecutingPrefix)
		if !formatted {
			return true
		}
		def, known := action.ByWire(wire)
		if known && (def.Kind == action.KindNotify || def.Kind == action.KindVerify) {
			continue
		}
		return true
	}
	return false
}

// aggregateRecovery turns raw incidents into the window's capacity numbers.
//
// The definitions it commits to, each of which the CLI and docs must repeat
// verbatim so no reader has to guess what a number counts:
//
//   - An incident is IN the window when its degraded interval overlaps
//     [from, to]. Membership by "opened in the window" would drop the very
//     incidents a capacity owner cares most about — the long ones.
//   - Degraded GPU-hours are clipped to the window, so the number can never
//     exceed the GPU-time the window actually contained.
//   - RECOVERED means the incident reached RESOLVED **and** its audit trail
//     records a remediation step actually executing. NEEDS_HUMAN and EXPIRED
//     do not count, and a NEEDS_HUMAN incident keeps accruing degraded time:
//     automation stopped, the capacity did not come back. This is stricter
//     than kubeneuron_incidents_recovered_total's sibling histogram, which
//     stops the clock at the park; the strict reading is the one that cannot
//     overclaim.
//   - OBSERVED ONLY is the rest of RESOLVED: an incident that closed without
//     anything being done to the fleet. A problem class with no policy bound
//     to it opens an incident, observes, and quiet-resolves — and used to be
//     counted as recovered capacity, so an incomplete policy set inflated the
//     one number this report exists to be trusted on. It is reported as its
//     own bucket rather than dropped, because those incidents did degrade the
//     fleet and the hours are real; only the recovery is absent.
//   - UNATTENDED means the incident never minted an approval round
//     (ApprovalEpoch == 0), the same test recordRecoveryOutcome applies. It is
//     a subset of RECOVERED, so an observed-only close can no longer arrive in
//     it by never having asked anybody.
//   - MTTR uses the FULL open-to-resolved duration of incidents that RECOVERED
//     inside the window. A clipped duration is not a recovery time, and
//     neither is the age of an incident nothing repaired.
//
// remediated answers "did a remediation step run" per incident ID; an ID
// absent from it did not.
func aggregateRecovery(incidents []*types.Incident, gpusPerNode map[string]int, remediated map[string]bool, from, to time.Time) *types.RecoveryReport {
	report := aggregateRecoverySubset(incidents, gpusPerNode, remediated, from, to, false)
	// The simulated view answers the question a pilot actually has. The
	// checklist tells them to stay in dry-run until they have watched the
	// system decide, and for that whole period the real report is empty by
	// construction — so at the moment somebody asks "what would this have got
	// us", the answer was a blank table. Same arithmetic, same incidents,
	// labelled as a simulation and never folded into the headline.
	if report.DryRunExcluded > 0 {
		sim := aggregateRecoverySubset(incidents, gpusPerNode, remediated, from, to, true)
		report.Simulated = &types.SimulatedRecovery{
			Incidents:              sim.Incidents,
			WouldRecover:           sim.Recovered,
			WouldRecoverUnattended: sim.RecoveredUnattended,
			ObservedOnly:           sim.ObservedOnly,
			DegradedGPUHours:       sim.DegradedGPUHours,
			WouldRecoverGPUHours:   sim.RecoveredGPUHours,
			MTTR:                   sim.MTTR,
		}
	}
	return report
}

// aggregateRecoverySubset is the arithmetic, over either the real incidents or
// the dry-run ones.
func aggregateRecoverySubset(incidents []*types.Incident, gpusPerNode map[string]int, remediated map[string]bool, from, to time.Time, wantDryRun bool) *types.RecoveryReport {
	report := &types.RecoveryReport{
		From: from, To: to,
		// Both slices are initialised so an empty report marshals as [] and
		// not null: a client iterating the array should not have to special-
		// case the quiet fleet, which is the normal one.
		Classes: make([]types.RecoveryClassReport, 0),
		Open:    make([]types.RecoveryOpenIncident, 0),
	}
	type classAccum struct {
		row       types.RecoveryClassReport
		durations []float64
	}
	classes := map[types.ProblemClass]*classAccum{}
	var mttrSamples []float64

	for _, inc := range incidents {
		if inc != nil && inc.DryRun != wantDryRun {
			// Dry-run executes nothing, so nothing was recovered. Dry-run is
			// also the SHIPPED DEFAULT, and its incidents run the whole ladder
			// on synthetic successes and reach RESOLVED with no approval —
			// which would have made a fresh install report GPU-hours "returned
			// to service" and 100% unattended recovery for a system that had
			// not touched a node. The one number this report exists to be
			// trusted on cannot be the one that lies out of the box.
			//
			// So they are aggregated SEPARATELY rather than dropped: the same
			// arithmetic over the same incidents, labelled as the simulation
			// it is. See RecoveryReport.Simulated.
			if !wantDryRun {
				report.DryRunExcluded++
			}
			continue
		}
		if inc == nil || inc.OpenedAt.IsZero() {
			// Without an open time there is no interval to charge. Skipping
			// undercounts; guessing one would invent capacity loss.
			continue
		}
		start, end := degradedInterval(inc, to)
		overlapStart, overlapEnd := laterOf(start, from), earlierOf(end, to)
		if !overlapEnd.After(overlapStart) {
			continue
		}
		gpus, assumed := affectedGPUCount(inc, gpusPerNode)
		if assumed {
			report.AssumedSingleGPU++
		}
		hours := overlapEnd.Sub(overlapStart).Hours() * float64(gpus)

		acc := classes[inc.Class]
		if acc == nil {
			acc = &classAccum{row: types.RecoveryClassReport{Class: inc.Class}}
			classes[inc.Class] = acc
		}
		report.Incidents++
		report.DegradedGPUHours += hours
		acc.row.Incidents++
		acc.row.DegradedGPUHours += hours

		switch {
		case inc.State != types.StateResolved:
			// Not closed: neither recovered nor observed-only yet.
		case !remediated[inc.ID]:
			// RESOLVED, but nothing was ever executed against the fleet. The
			// degraded hours above are real and stay in the total; the recovery
			// is not, so it is counted here and nowhere else.
			report.ObservedOnly++
			report.ObservedOnlyGPUHours += hours
			acc.row.ObservedOnly++
			acc.row.ObservedOnlyGPUHours += hours
		default:
			report.Recovered++
			report.RecoveredGPUHours += hours
			acc.row.Recovered++
			acc.row.RecoveredGPUHours += hours
			if inc.ApprovalEpoch == 0 {
				report.RecoveredUnattended++
				acc.row.RecoveredUnattended++
			}
			// Only a recovery that COMPLETED inside the window contributes a
			// recovery time; one that completed later would report a duration
			// the window cannot account for.
			if !end.Before(from) && !end.After(to) {
				seconds := end.Sub(start).Seconds()
				mttrSamples = append(mttrSamples, seconds)
				acc.durations = append(acc.durations, seconds)
			}
		}
		if !inc.State.Terminal() {
			report.Open = append(report.Open, types.RecoveryOpenIncident{
				ID:               inc.ID,
				Class:            inc.Class,
				Node:             inc.Target.Node,
				GPUUUID:          inc.Target.GPUUUID,
				State:            inc.State,
				OpenedAt:         inc.OpenedAt,
				DegradedGPUHours: roundHours(hours),
			})
		}
	}

	report.DegradedGPUHours = roundHours(report.DegradedGPUHours)
	report.RecoveredGPUHours = roundHours(report.RecoveredGPUHours)
	report.ObservedOnlyGPUHours = roundHours(report.ObservedOnlyGPUHours)
	report.MTTR = summarizeLatency(mttrSamples)
	report.Classes = make([]types.RecoveryClassReport, 0, len(classes))
	for _, acc := range classes {
		acc.row.DegradedGPUHours = roundHours(acc.row.DegradedGPUHours)
		acc.row.RecoveredGPUHours = roundHours(acc.row.RecoveredGPUHours)
		acc.row.ObservedOnlyGPUHours = roundHours(acc.row.ObservedOnlyGPUHours)
		acc.row.MTTR = summarizeLatency(acc.durations)
		report.Classes = append(report.Classes, acc.row)
	}
	// Cost ranking first, then class name: a stable order keeps two runs over
	// the same data byte-identical, which is what makes the report diffable.
	sort.Slice(report.Classes, func(i, j int) bool {
		if report.Classes[i].DegradedGPUHours != report.Classes[j].DegradedGPUHours {
			return report.Classes[i].DegradedGPUHours > report.Classes[j].DegradedGPUHours
		}
		return report.Classes[i].Class < report.Classes[j].Class
	})
	sort.Slice(report.Open, func(i, j int) bool {
		if !report.Open[i].OpenedAt.Equal(report.Open[j].OpenedAt) {
			return report.Open[i].OpenedAt.Before(report.Open[j].OpenedAt)
		}
		return report.Open[i].ID < report.Open[j].ID
	})
	return report
}

// degradedInterval is the span an incident charges capacity for. Only a
// lifecycle-terminal state stops the clock: an incident parked in NEEDS_HUMAN
// is still degrading the fleet until somebody resolves it, and reporting it
// as finished at the park would credit KubeNeuron for capacity it never
// returned. EXPIRED stops at its transition because the row is closed and
// nothing further is known — an honest end, not a claim of recovery.
func degradedInterval(inc *types.Incident, now time.Time) (start, end time.Time) {
	start, end = inc.OpenedAt, now
	switch {
	case inc.State == types.StateResolved && inc.ResolvedAt != nil:
		end = *inc.ResolvedAt
	case inc.State.Terminal():
		// Pre-0002 rows carry no state_changed_at; updated_at is the closest
		// durable stamp of when the row stopped moving.
		end = firstSetTime(inc.StateChangedAt, inc.UpdatedAt, now)
	}
	if end.Before(start) {
		// Backwards clocks and hand-edited rows exist. Charging a negative
		// interval would silently subtract capacity loss from the fleet total.
		end = start
	}
	return start, end
}

// affectedGPUCount is how many accelerators an incident covers — the same
// rule recordRecoveryOutcome applies, so the report and the Prometheus
// counter cannot disagree about what a GPU-hour is: one for a GPU-scoped
// incident, the node's registered inventory for a node-scoped one. Unknown
// inventory counts as one and is reported as an assumption, because
// undercounting is the honest failure direction for a capacity number.
func affectedGPUCount(inc *types.Incident, gpusPerNode map[string]int) (gpus int, assumed bool) {
	// The same rule as affectedGPUs, asked of the same predicate, because these
	// two numbers are supposed to agree and drifted apart once already.
	if inc.Target.IsDeviceScoped() {
		return 1, false
	}
	if n := gpusPerNode[inc.Target.Node]; n > 0 {
		return n, false
	}
	return 1, true
}

// summarizeLatency computes the distribution from the raw durations. With the
// store as the source there is no reason to interpolate a histogram bucket
// when the exact percentile is available; percentiles are nearest-rank, so
// every reported value is a duration some incident actually took.
func summarizeLatency(seconds []float64) types.RecoveryLatency {
	if len(seconds) == 0 {
		return types.RecoveryLatency{}
	}
	sorted := append([]float64(nil), seconds...)
	sort.Float64s(sorted)
	sum := 0.0
	for _, s := range sorted {
		sum += s
	}
	return types.RecoveryLatency{
		Samples:     len(sorted),
		MeanSeconds: roundSeconds(sum / float64(len(sorted))),
		P50Seconds:  roundSeconds(nearestRank(sorted, 0.5)),
		P90Seconds:  roundSeconds(nearestRank(sorted, 0.9)),
	}
}

func nearestRank(sorted []float64, quantile float64) float64 {
	idx := int(math.Ceil(quantile*float64(len(sorted)))) - 1
	if idx < 0 {
		idx = 0
	}
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	return sorted[idx]
}

// roundHours and roundSeconds quantize the report so two runs over unchanged
// data produce byte-identical JSON: float accumulation order otherwise leaks
// into the last digits and makes a diff look like a change. 1e-4 GPU-hours is
// a third of a GPU-second — below any capacity decision.
func roundHours(v float64) float64   { return math.Round(v*1e4) / 1e4 }
func roundSeconds(v float64) float64 { return math.Round(v*10) / 10 }

func firstSetTime(candidates ...time.Time) time.Time {
	for _, t := range candidates {
		if !t.IsZero() {
			return t
		}
	}
	return time.Time{}
}

func laterOf(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

func earlierOf(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}
