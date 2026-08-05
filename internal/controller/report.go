package controller

import (
	"context"
	"fmt"
	"math"
	"sort"
	"time"

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
	incidents, err := c.store.ListIncidents(ctx, store.IncidentFilter{ActiveSince: from})
	if err != nil {
		return nil, err
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
	return aggregateRecovery(incidents, gpusPerNode, from, to), nil
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
//   - RECOVERED means the incident reached RESOLVED. NEEDS_HUMAN and EXPIRED
//     do not count, and a NEEDS_HUMAN incident keeps accruing degraded time:
//     automation stopped, the capacity did not come back. This is stricter
//     than kubeneuron_incidents_recovered_total's sibling histogram, which
//     stops the clock at the park; the strict reading is the one that cannot
//     overclaim.
//   - UNATTENDED means the incident never minted an approval round
//     (ApprovalEpoch == 0), the same test recordRecoveryOutcome applies.
//   - MTTR uses the FULL open-to-resolved duration of incidents that resolved
//     inside the window. A clipped duration is not a recovery time.
func aggregateRecovery(incidents []*types.Incident, gpusPerNode map[string]int, from, to time.Time) *types.RecoveryReport {
	report := &types.RecoveryReport{From: from, To: to}
	type classAccum struct {
		row       types.RecoveryClassReport
		durations []float64
	}
	classes := map[types.ProblemClass]*classAccum{}
	var mttrSamples []float64

	for _, inc := range incidents {
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

		if inc.State == types.StateResolved {
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
	report.MTTR = summarizeLatency(mttrSamples)
	report.Classes = make([]types.RecoveryClassReport, 0, len(classes))
	for _, acc := range classes {
		acc.row.DegradedGPUHours = roundHours(acc.row.DegradedGPUHours)
		acc.row.RecoveredGPUHours = roundHours(acc.row.RecoveredGPUHours)
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
	if inc.Target.IsGPU() {
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
