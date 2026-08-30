package controller

import (
	"testing"
	"time"

	"github.com/kubeneuron/kubeneuron/pkg/types"
)

// The report is the number a budget conversation quotes, so every definition
// it commits to gets a case here: window membership and clipping, what counts
// as recovered, what counts as unattended, GPU scope expansion, and the
// incidents that must NOT be silently closed (NEEDS_HUMAN).

func at(base time.Time, hours float64) time.Time {
	return base.Add(time.Duration(hours * float64(time.Hour)))
}

// allRemediated is the "a step ran for every one of these" evidence map. The
// cases below are about window arithmetic and GPU scope, not about coverage,
// so they assert against a fleet whose ladders all executed; the observed-only
// half has its own tests further down.
func allRemediated(incidents []*types.Incident) map[string]bool {
	out := make(map[string]bool, len(incidents))
	for _, inc := range incidents {
		if inc != nil {
			out[inc.ID] = true
		}
	}
	return out
}

func incident(id string, class types.ProblemClass, node, gpu string, state types.IncidentState, opened, changed time.Time) *types.Incident {
	inc := &types.Incident{
		ID:             id,
		Class:          class,
		Target:         types.Target{Node: node, GPUUUID: gpu},
		State:          state,
		OpenedAt:       opened,
		UpdatedAt:      changed,
		StateChangedAt: changed,
	}
	if state == types.StateResolved {
		resolved := changed
		inc.ResolvedAt = &resolved
	}
	return inc
}

func TestAggregateRecovery(t *testing.T) {
	// A fixed window makes every expectation arithmetic a reviewer can redo:
	// from = T-24h, to = T.
	to := time.Date(2026, 8, 5, 12, 0, 0, 0, time.UTC)
	from := to.Add(-24 * time.Hour)
	inventory := map[string]int{"node-8gpu": 8, "node-1gpu": 1}

	tests := []struct {
		name      string
		incidents []*types.Incident
		want      types.RecoveryReport
	}{
		{
			name: "gpu-scoped resolve, unattended: one GPU for two hours",
			incidents: []*types.Incident{
				incident("i1", "ecc-dbe", "node-8gpu", "GPU-1", types.StateResolved, at(to, -6), at(to, -4)),
			},
			want: types.RecoveryReport{
				Incidents: 1, Recovered: 1, RecoveredUnattended: 1,
				DegradedGPUHours: 2, RecoveredGPUHours: 2,
			},
		},
		{
			name: "node-scoped resolve charges the node's whole inventory",
			incidents: []*types.Incident{
				incident("i1", "fell-off-bus", "node-8gpu", "", types.StateResolved, at(to, -3), at(to, -2)),
			},
			want: types.RecoveryReport{
				Incidents: 1, Recovered: 1, RecoveredUnattended: 1,
				DegradedGPUHours: 8, RecoveredGPUHours: 8,
			},
		},
		{
			name: "node-scoped incident on an unknown node counts one GPU and says so",
			incidents: []*types.Incident{
				incident("i1", "fell-off-bus", "node-unregistered", "", types.StateResolved, at(to, -3), at(to, -2)),
			},
			want: types.RecoveryReport{
				Incidents: 1, Recovered: 1, RecoveredUnattended: 1,
				DegradedGPUHours: 1, RecoveredGPUHours: 1, AssumedSingleGPU: 1,
			},
		},
		{
			name: "an approval round makes the recovery attended",
			incidents: []*types.Incident{
				func() *types.Incident {
					inc := incident("i1", "ecc-dbe", "node-8gpu", "GPU-1", types.StateResolved, at(to, -2), at(to, -1))
					inc.ApprovalEpoch = 2
					return inc
				}(),
			},
			want: types.RecoveryReport{
				Incidents: 1, Recovered: 1, RecoveredUnattended: 0,
				DegradedGPUHours: 1, RecoveredGPUHours: 1,
			},
		},
		{
			name: "NEEDS_HUMAN is neither recovered nor finished: it keeps accruing to now",
			incidents: []*types.Incident{
				incident("i1", "thermal", "node-8gpu", "GPU-1", types.StateNeedsHuman, at(to, -5), at(to, -4)),
			},
			want: types.RecoveryReport{
				Incidents: 1, DegradedGPUHours: 5,
			},
		},
		{
			name: "EXPIRED stops the clock but returns nothing",
			incidents: []*types.Incident{
				incident("i1", "thermal", "node-8gpu", "GPU-1", types.StateExpired, at(to, -5), at(to, -4)),
			},
			want: types.RecoveryReport{
				Incidents: 1, DegradedGPUHours: 1,
			},
		},
		{
			name: "an incident that ended before the window is out of it",
			incidents: []*types.Incident{
				incident("i1", "ecc-dbe", "node-8gpu", "GPU-1", types.StateResolved, at(to, -40), at(to, -30)),
			},
			want: types.RecoveryReport{},
		},
		{
			name: "an incident straddling the window boundary is clipped to it",
			incidents: []*types.Incident{
				// Open 30h, of which 6h fall inside the 24h window.
				incident("i1", "ecc-dbe", "node-8gpu", "GPU-1", types.StateResolved, at(to, -30), at(to, -18)),
			},
			want: types.RecoveryReport{
				Incidents: 1, Recovered: 1, RecoveredUnattended: 1,
				DegradedGPUHours: 6, RecoveredGPUHours: 6,
				// The recovery completed in the window, so MTTR sees its FULL
				// 12h duration, not the clipped 6h.
				MTTR: types.RecoveryLatency{Samples: 1, MeanSeconds: 43200, P50Seconds: 43200, P90Seconds: 43200},
			},
		},
		{
			name: "a still-open incident is charged up to now",
			incidents: []*types.Incident{
				incident("i1", "ecc-dbe", "node-8gpu", "GPU-1", types.StateExecuting, at(to, -3), at(to, -3)),
			},
			want: types.RecoveryReport{
				Incidents: 1, DegradedGPUHours: 3,
			},
		},
		{
			name: "an incident with no open time cannot be measured and is skipped",
			incidents: []*types.Incident{
				{ID: "i1", Class: "ecc-dbe", State: types.StateResolved},
			},
			want: types.RecoveryReport{},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := aggregateRecovery(tc.incidents, inventory, allRemediated(tc.incidents), from, to)
			if got.Incidents != tc.want.Incidents {
				t.Errorf("incidents = %d, want %d", got.Incidents, tc.want.Incidents)
			}
			if got.Recovered != tc.want.Recovered {
				t.Errorf("recovered = %d, want %d", got.Recovered, tc.want.Recovered)
			}
			if got.RecoveredUnattended != tc.want.RecoveredUnattended {
				t.Errorf("recovered unattended = %d, want %d", got.RecoveredUnattended, tc.want.RecoveredUnattended)
			}
			if got.DegradedGPUHours != tc.want.DegradedGPUHours {
				t.Errorf("degraded GPU-hours = %v, want %v", got.DegradedGPUHours, tc.want.DegradedGPUHours)
			}
			if got.RecoveredGPUHours != tc.want.RecoveredGPUHours {
				t.Errorf("recovered GPU-hours = %v, want %v", got.RecoveredGPUHours, tc.want.RecoveredGPUHours)
			}
			if got.AssumedSingleGPU != tc.want.AssumedSingleGPU {
				t.Errorf("assumed-single-GPU incidents = %d, want %d", got.AssumedSingleGPU, tc.want.AssumedSingleGPU)
			}
			if tc.want.MTTR.Samples > 0 && got.MTTR != tc.want.MTTR {
				t.Errorf("MTTR = %+v, want %+v", got.MTTR, tc.want.MTTR)
			}
			if got.From != from || got.To != to {
				t.Errorf("window = %s..%s, want %s..%s", got.From, got.To, from, to)
			}
		})
	}
}

func TestAggregateRecoveryRanksClassesAndListsOpenIncidents(t *testing.T) {
	to := time.Date(2026, 8, 5, 12, 0, 0, 0, time.UTC)
	from := to.Add(-24 * time.Hour)
	inventory := map[string]int{"node-8gpu": 8}

	attended := incident("attended", "ecc-dbe", "node-8gpu", "GPU-2", types.StateResolved, at(to, -8), at(to, -6))
	attended.ApprovalEpoch = 1

	incidents := []*types.Incident{
		// 8 GPUs x 4h = 32 GPU-hours, recovered without a human.
		incident("bus", "fell-off-bus", "node-8gpu", "", types.StateResolved, at(to, -10), at(to, -6)),
		// 1 GPU x 2h = 2 GPU-hours, recovered after an approval.
		attended,
		// 1 GPU x 1h = 1 GPU-hour, still executing.
		incident("running", "ecc-dbe", "node-8gpu", "GPU-3", types.StateExecuting, at(to, -1), at(to, -1)),
		// 1 GPU x 12h = 12 GPU-hours, parked for a human since hour 11.
		incident("parked", "ecc-dbe", "node-8gpu", "GPU-4", types.StateNeedsHuman, at(to, -12), at(to, -11)),
	}
	report := aggregateRecovery(incidents, inventory, allRemediated(incidents), from, to)

	if report.DegradedGPUHours != 47 || report.RecoveredGPUHours != 34 {
		t.Fatalf("degraded/recovered = %v/%v GPU-hours, want 47/34", report.DegradedGPUHours, report.RecoveredGPUHours)
	}
	if report.Recovered != 2 || report.RecoveredUnattended != 1 {
		t.Fatalf("recovered/unattended = %d/%d, want 2/1", report.Recovered, report.RecoveredUnattended)
	}
	// MTTR over the two resolutions: 4h and 2h. Nearest-rank p50 of [7200,
	// 14400] is the lower sample; p90 the upper.
	want := types.RecoveryLatency{Samples: 2, MeanSeconds: 10800, P50Seconds: 7200, P90Seconds: 14400}
	if report.MTTR != want {
		t.Fatalf("MTTR = %+v, want %+v", report.MTTR, want)
	}

	if len(report.Classes) != 2 {
		t.Fatalf("classes = %d, want 2", len(report.Classes))
	}
	if report.Classes[0].Class != "fell-off-bus" || report.Classes[0].DegradedGPUHours != 32 {
		t.Fatalf("top class = %+v, want fell-off-bus at 32 GPU-hours", report.Classes[0])
	}
	ecc := report.Classes[1]
	if ecc.Incidents != 3 || ecc.DegradedGPUHours != 15 || ecc.Recovered != 1 || ecc.RecoveredUnattended != 0 {
		t.Fatalf("ecc-dbe row = %+v, want 3 incidents / 15 GPU-hours / 1 recovered / 0 unattended", ecc)
	}

	// Open means "capacity not returned", which includes the parked incident;
	// oldest first, because that is the one bleeding the most.
	if len(report.Open) != 2 || report.Open[0].ID != "parked" || report.Open[1].ID != "running" {
		t.Fatalf("open incidents = %+v, want [parked running]", report.Open)
	}
	if report.Open[0].DegradedGPUHours != 12 || report.Open[0].State != types.StateNeedsHuman {
		t.Fatalf("parked incident = %+v, want 12 GPU-hours in NEEDS_HUMAN", report.Open[0])
	}
}

func TestAggregateRecoveryClampsBackwardsAndFutureStamps(t *testing.T) {
	to := time.Date(2026, 8, 5, 12, 0, 0, 0, time.UTC)
	from := to.Add(-24 * time.Hour)

	// A resolved_at before opened_at (clock step, hand-edited row) must never
	// subtract capacity loss from the fleet total.
	backwards := incident("backwards", "ecc-dbe", "n1", "GPU-1", types.StateResolved, at(to, -2), at(to, -3))
	report := aggregateRecovery([]*types.Incident{backwards}, nil, allRemediated([]*types.Incident{backwards}), from, to)
	if report.Incidents != 0 || report.DegradedGPUHours != 0 {
		t.Fatalf("backwards incident produced %+v, want an empty report", report)
	}

	// An incident opened after the window end contributes nothing rather than
	// a negative overlap.
	future := incident("future", "ecc-dbe", "n1", "GPU-1", types.StateExecuting, at(to, 1), at(to, 1))
	report = aggregateRecovery([]*types.Incident{future}, nil, allRemediated([]*types.Incident{future}), from, to)
	if report.Incidents != 0 {
		t.Fatalf("future incident produced %+v, want an empty report", report)
	}
}

// TestDryRunFleetGetsSimulatedNumbers is the pilot's whole first month. The
// checklist tells operators to stay in dry-run until they have watched the
// system decide, and for that entire period every real number is zero by
// construction — so the question that decides whether to enable enforcement
// had a blank table for an answer.
func TestDryRunFleetGetsSimulatedNumbers(t *testing.T) {
	now := time.Now()
	from, to := now.Add(-24*time.Hour), now
	opened := now.Add(-3 * time.Hour)
	resolved := now.Add(-2 * time.Hour)

	inc := func(id string, dryRun bool, epoch int) *types.Incident {
		r := resolved
		return &types.Incident{
			ID: id, Target: types.Target{Node: "n1", GPUUUID: "GPU-" + id},
			Class: types.ClassECCDBE, State: types.StateResolved, DryRun: dryRun,
			ApprovalEpoch: epoch,
			OpenedAt:      opened, UpdatedAt: r, StateChangedAt: r, ResolvedAt: &r,
		}
	}
	incidents := []*types.Incident{
		inc("a", true, 0), // dry-run, unattended
		inc("b", true, 1), // dry-run, needed an approval
	}
	report := aggregateRecovery(incidents, map[string]int{"n1": 1}, allRemediated(incidents), from, to)

	// The headline stays honest: nothing was executed, so nothing recovered.
	if report.Incidents != 0 || report.RecoveredGPUHours != 0 {
		t.Fatalf("headline = %d incidents / %.2f recovered GPU-hours, want zero: "+
			"dry-run executed nothing", report.Incidents, report.RecoveredGPUHours)
	}
	if report.DryRunExcluded != 2 {
		t.Fatalf("DryRunExcluded = %d, want 2", report.DryRunExcluded)
	}

	sim := report.Simulated
	if sim == nil {
		t.Fatal("a dry-run fleet got no simulated numbers; the pilot's whole evaluation " +
			"period reports a blank table")
	}
	if sim.Incidents != 2 || sim.WouldRecover != 2 {
		t.Fatalf("simulated = %+v, want 2 incidents both reaching RESOLVED", sim)
	}
	if sim.WouldRecoverUnattended != 1 {
		t.Fatalf("WouldRecoverUnattended = %d, want 1 (the other minted an approval round)",
			sim.WouldRecoverUnattended)
	}
	// One hour degraded per incident, one GPU each.
	if sim.DegradedGPUHours < 1.9 || sim.DegradedGPUHours > 2.1 {
		t.Fatalf("simulated DegradedGPUHours = %.2f, want ~2", sim.DegradedGPUHours)
	}
}

// A fleet with no dry-run incidents must not grow a simulated section: an
// empty one would invite the reader to look for meaning in zeros.
func TestRealFleetHasNoSimulatedSection(t *testing.T) {
	now := time.Now()
	r := now.Add(-1 * time.Hour)
	report := aggregateRecovery([]*types.Incident{{
		ID: "real", Target: types.Target{Node: "n1", GPUUUID: "GPU-1"},
		Class: types.ClassECCDBE, State: types.StateResolved,
		OpenedAt: now.Add(-2 * time.Hour), UpdatedAt: r, StateChangedAt: r, ResolvedAt: &r,
	}}, map[string]int{"n1": 1}, map[string]bool{"real": true}, now.Add(-24*time.Hour), now)

	if report.Simulated != nil {
		t.Fatalf("a fleet with no dry-run incidents got a simulated section: %+v", report.Simulated)
	}
	if report.Recovered != 1 {
		t.Fatalf("Recovered = %d, want 1", report.Recovered)
	}
}

// TestAnIncidentNothingActedOnIsNotRecoveredCapacity is the defect this
// bucket exists for, in its exact shipped shape.
//
// A problem class with no GPURemediationPolicy bound to it opens an incident,
// observes, and quiet-resolves — no playbook, no step, no node touched. That
// incident reached RESOLVED, so it was counted as recovered; it never minted
// an approval round either, because nothing ever asked anybody, so it was also
// counted as recovered WITHOUT A HUMAN. An installation that had bound one
// class out of twenty therefore reported near-total unattended recovery, and
// docs/pilot-checklist.md tells the operator to take that number to whoever
// pays for the fleet.
func TestAnIncidentNothingActedOnIsNotRecoveredCapacity(t *testing.T) {
	to := time.Date(2026, 8, 5, 12, 0, 0, 0, time.UTC)
	from := to.Add(-24 * time.Hour)

	repaired := incident("repaired", "ecc-dbe", "n1", "GPU-1", types.StateResolved, at(to, -6), at(to, -4))
	unbound := incident("unbound", "thermal", "n1", "GPU-2", types.StateResolved, at(to, -6), at(to, -4))

	report := aggregateRecovery([]*types.Incident{repaired, unbound},
		map[string]int{"n1": 8},
		// Only the first has an EXECUTING row in its audit trail.
		map[string]bool{"repaired": true},
		from, to)

	if report.Recovered != 1 || report.RecoveredUnattended != 1 {
		t.Fatalf("recovered/unattended = %d/%d, want 1/1: an incident with no policy bound to "+
			"its class was counted as capacity this product returned",
			report.Recovered, report.RecoveredUnattended)
	}
	if report.ObservedOnly != 1 {
		t.Fatalf("ObservedOnly = %d, want 1", report.ObservedOnly)
	}
	// Both incidents degraded one GPU for two hours. The hours are real for
	// both; only one of them came back.
	if report.DegradedGPUHours != 4 || report.RecoveredGPUHours != 2 || report.ObservedOnlyGPUHours != 2 {
		t.Fatalf("degraded/recovered/observed-only = %v/%v/%v GPU-hours, want 4/2/2",
			report.DegradedGPUHours, report.RecoveredGPUHours, report.ObservedOnlyGPUHours)
	}
	// And it contributes no repair time: nothing was repaired.
	if report.MTTR.Samples != 1 {
		t.Fatalf("MTTR samples = %d, want 1: an observed-only close is not a recovery time",
			report.MTTR.Samples)
	}

	byClass := map[types.ProblemClass]types.RecoveryClassReport{}
	for _, row := range report.Classes {
		byClass[row.Class] = row
	}
	if row := byClass["thermal"]; row.Recovered != 0 || row.ObservedOnly != 1 {
		t.Fatalf("thermal row = %+v, want 0 recovered / 1 observed-only: the per-class column "+
			"is where an operator reads which classes have no ladder bound", row)
	}
}

// TestObserveOnlyLadderIsNotAWheelOfRecovery covers the subtler half. An
// observe-first class that crosses its threshold DOES execute — one
// notify.observe step — and then resolves. A step ran, so a naive "did
// anything execute" test would call it recovered capacity, which is a claim
// about a GPU made by a step whose entire effect was to write a line.
func TestObserveOnlyLadderIsNotAWheelOfRecovery(t *testing.T) {
	if remediationExecuted([]string{auditExecutingResult("notify.observe")}) {
		t.Fatal("a ladder whose only executed step was a notification counted as a repair")
	}
	if remediationExecuted([]string{auditExecutingResult("notify.ticket")}) {
		t.Fatal("opening a ticket is how a playbook asks a human to act; it is not the act")
	}
	// config/samples ships exactly this ladder — Observe then VerifyGPUHealth —
	// and it is the shape that slips past a naive "did any step run" test.
	// Reading health back is not repairing anything.
	if remediationExecuted([]string{
		auditExecutingResult("notify.observe"),
		auditExecutingResult("verify.gpu_health"),
	}) {
		t.Fatal("a ladder that observed and then checked health returned no capacity to service")
	}
	if !remediationExecuted([]string{
		auditExecutingResult("notify.observe"),
		auditExecutingResult("platform.cordon"),
	}) {
		t.Fatal("a ladder that notified and then cordoned did act on the fleet")
	}
	if remediationExecuted(nil) {
		t.Fatal("an incident with no EXECUTING audit row at all had no step to run")
	}
	// A row an older controller wrote carries no recognisable action. Calling
	// that observation would rewrite an existing installation's whole history
	// into "nothing was ever done", which is the louder lie of the two.
	if !remediationExecuted([]string{"reset complete"}) {
		t.Fatal("an unrecognised EXECUTING row must be read as 'a step ran, we cannot say which'")
	}
}

// The simulated section carries the same split. A dry-run pilot whose policy
// set covers two classes out of twenty must not read "would recover: all of
// them" — that projection is the argument for turning enforcement on.
func TestSimulatedRecoverySeparatesTheClassesWithNoLadder(t *testing.T) {
	now := time.Now()
	from, to := now.Add(-24*time.Hour), now
	opened, resolved := now.Add(-3*time.Hour), now.Add(-2*time.Hour)

	inc := func(id string, class types.ProblemClass) *types.Incident {
		r := resolved
		return &types.Incident{
			ID: id, Target: types.Target{Node: "n1", GPUUUID: "GPU-" + id},
			Class: class, State: types.StateResolved, DryRun: true,
			OpenedAt: opened, UpdatedAt: r, StateChangedAt: r, ResolvedAt: &r,
		}
	}
	report := aggregateRecovery(
		[]*types.Incident{inc("bound", types.ClassECCDBE), inc("unbound", types.ClassPower)},
		map[string]int{"n1": 1},
		map[string]bool{"bound": true},
		from, to)

	sim := report.Simulated
	if sim == nil {
		t.Fatal("a dry-run fleet got no simulated numbers")
	}
	if sim.WouldRecover != 1 || sim.ObservedOnly != 1 {
		t.Fatalf("simulated would-recover/observed-only = %d/%d, want 1/1: the class with no "+
			"policy bound was projected as a recovery enforcement would have delivered",
			sim.WouldRecover, sim.ObservedOnly)
	}
}

// TestSimulatedStepsAreNotRemediation covers the ladder that executed nothing
// because the operator switched a running installation to DryRun mid-flight.
//
// Execution follows the LIVE gate; the incident's own dry-run flag is stamped
// once, when it opens. So such an incident carries DryRun=false, simulates
// every step, resolves — and every accounting read keyed on that flag folds it
// into the REAL report. A fleet whose faults simply stopped recurring would be
// told it got its GPU-hours back from ladders that touched nothing, which is
// the number the observed-only bucket exists to protect.
func TestSimulatedStepsAreNotRemediation(t *testing.T) {
	cases := []struct {
		name    string
		results []string
		want    bool
		why     string
	}{
		{
			name:    "a real cordon is remediation",
			results: []string{auditStepResult("platform.cordon", false)},
			want:    true,
		},
		{
			name:    "the same cordon simulated is not",
			results: []string{auditStepResult("platform.cordon", true)},
			want:    false,
			why:     "the step entered EXECUTING and then did nothing at all",
		},
		{
			name: "a ladder that simulated every rung is not",
			results: []string{
				auditStepResult("platform.cordon", true),
				auditStepResult("platform.drain", true),
				auditStepResult("agent.reboot", true),
			},
			want: false,
			why:  "three rungs, no disruption, and the incident still reached RESOLVED",
		},
		{
			name: "one real rung among simulated ones counts",
			results: []string{
				auditStepResult("platform.cordon", true),
				auditStepResult("platform.drain", false),
			},
			want: true,
			why:  "the drain really moved workloads, whatever happened around it",
		},
		{
			name:    "notify-only is still not remediation",
			results: []string{auditStepResult("notify.observe", false)},
			want:    false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := remediationExecuted(tc.results); got != tc.want {
				t.Fatalf("remediationExecuted = %v, want %v — %s", got, tc.want, tc.why)
			}
		})
	}
}

// TestAPCIOnlyIncidentIsChargedOneDevice covers a capacity number that goes in
// front of whoever pays for the fleet.
//
// Both counters asked Target.IsGPU() — "do we know the UUID" — where the real
// question is "is this about one card or the whole machine". A card knocked off
// the bus has no UUID, so a PCI-only incident was charged the node's ENTIRE
// inventory. That was merely an overstatement while such faults collapsed into
// one incident per node. Once each device got its own incident, an 8-GPU node
// losing its PCIe switch produced eight incidents each charging eight GPUs:
// 64 GPU-seconds per second on a node that has eight.
//
// A node-scoped incident — no UUID and no address — must still charge the whole
// node, which is the case this rule exists for.
func TestAPCIOnlyIncidentIsChargedOneDevice(t *testing.T) {
	perNode := map[string]int{"n1": 8}

	pciOnly := &types.Incident{Target: types.Target{Node: "n1", PCIAddr: "0000:3b:00"}}
	if got, assumed := affectedGPUCount(pciOnly, perNode); got != 1 || assumed {
		t.Errorf("a PCI-only incident was charged %d GPUs (assumed=%v), want 1: it names one "+
			"card, and with one incident per device this multiplies the fleet's degraded-"+
			"capacity bill by the number of cards on the node", got, assumed)
	}

	attributed := &types.Incident{Target: types.Target{Node: "n1", GPUUUID: "GPU-a"}}
	if got, _ := affectedGPUCount(attributed, perNode); got != 1 {
		t.Errorf("an attributed incident was charged %d GPUs, want 1", got)
	}

	nodeScoped := &types.Incident{Target: types.Target{Node: "n1"}}
	if got, assumed := affectedGPUCount(nodeScoped, perNode); got != 8 || assumed {
		t.Errorf("a node-scoped incident was charged %d GPUs (assumed=%v), want the node's 8: "+
			"understating this hides how much capacity remediation brought back", got, assumed)
	}

	unknownInventory := &types.Incident{Target: types.Target{Node: "n-unknown"}}
	if got, assumed := affectedGPUCount(unknownInventory, perNode); got != 1 || !assumed {
		t.Errorf("unknown inventory charged %d (assumed=%v), want 1 and flagged", got, assumed)
	}
}
