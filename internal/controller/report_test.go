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
			got := aggregateRecovery(tc.incidents, inventory, from, to)
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

	report := aggregateRecovery([]*types.Incident{
		// 8 GPUs x 4h = 32 GPU-hours, recovered without a human.
		incident("bus", "fell-off-bus", "node-8gpu", "", types.StateResolved, at(to, -10), at(to, -6)),
		// 1 GPU x 2h = 2 GPU-hours, recovered after an approval.
		attended,
		// 1 GPU x 1h = 1 GPU-hour, still executing.
		incident("running", "ecc-dbe", "node-8gpu", "GPU-3", types.StateExecuting, at(to, -1), at(to, -1)),
		// 1 GPU x 12h = 12 GPU-hours, parked for a human since hour 11.
		incident("parked", "ecc-dbe", "node-8gpu", "GPU-4", types.StateNeedsHuman, at(to, -12), at(to, -11)),
	}, inventory, from, to)

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
	report := aggregateRecovery([]*types.Incident{backwards}, nil, from, to)
	if report.Incidents != 0 || report.DegradedGPUHours != 0 {
		t.Fatalf("backwards incident produced %+v, want an empty report", report)
	}

	// An incident opened after the window end contributes nothing rather than
	// a negative overlap.
	future := incident("future", "ecc-dbe", "n1", "GPU-1", types.StateExecuting, at(to, 1), at(to, 1))
	report = aggregateRecovery([]*types.Incident{future}, nil, from, to)
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
	report := aggregateRecovery([]*types.Incident{
		inc("a", true, 0), // dry-run, unattended
		inc("b", true, 1), // dry-run, needed an approval
	}, map[string]int{"n1": 1}, from, to)

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
	}}, map[string]int{"n1": 1}, now.Add(-24*time.Hour), now)

	if report.Simulated != nil {
		t.Fatalf("a fleet with no dry-run incidents got a simulated section: %+v", report.Simulated)
	}
	if report.Recovered != 1 {
		t.Fatalf("Recovered = %d, want 1", report.Recovered)
	}
}
