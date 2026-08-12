package types

import "time"

// RecoveryReport answers the question a capacity owner asks about a window:
// how much accelerator capacity was degraded, how much of it came back, and
// how much came back without waking anybody. It is the wire shape of
// GET /api/v1/report/recovery and of `kubeneuronctl report --json`.
//
// Every number here is derived from the incident store, not from Prometheus:
// the store holds the ground truth (opened_at, resolved_at, class, target,
// approval epoch), so the report is exact, needs no metrics backend, and is
// reproducible from a database snapshot. The equivalent Prometheus series
// (kubeneuron_degraded_gpu_seconds_total and friends) answer the same
// question for dashboards, where a range query is natural.
//
// Two conventions must be stated wherever these numbers are shown, because a
// reader who guesses will guess wrong:
//   - "recovered" means the incident reached RESOLVED. Nothing else counts —
//     an incident parked in NEEDS_HUMAN or aged out to EXPIRED did not return
//     capacity to service as far as KubeNeuron can prove.
//   - GPU-hours are degraded-GPU-hours, not lost-GPU-hours: a degraded GPU may
//     still have been serving traffic.
type RecoveryReport struct {
	// From and To bound the window the numbers cover, as resolved by the
	// controller's clock (the same clock that stamped the incident rows).
	From time.Time `json:"from"`
	To   time.Time `json:"to"`
	// DegradedGPUHours is the GPU-time spent under an incident inside the
	// window: every incident's degraded interval is clipped to the window, so
	// a long incident contributes only its in-window part and the number
	// cannot exceed the window's real elapsed GPU-time.
	DegradedGPUHours float64 `json:"degraded_gpu_hours"`
	// RecoveredGPUHours is the DegradedGPUHours share belonging to incidents
	// that reached RESOLVED — the capacity remediation demonstrably returned.
	RecoveredGPUHours float64 `json:"recovered_gpu_hours"`
	// Incidents counts incidents whose degraded interval overlaps the window,
	// including ones opened before it and ones still open now.
	Incidents int `json:"incidents"`
	// Recovered counts those that reached RESOLVED; RecoveredUnattended is the
	// subset that never minted an approval round, i.e. no human decided.
	Recovered           int `json:"recovered"`
	RecoveredUnattended int `json:"recovered_unattended"`
	// MTTR is measured over incidents that RESOLVED inside the window, using
	// the full open-to-resolved duration (not the clipped one — a clipped
	// duration is not a recovery time).
	MTTR RecoveryLatency `json:"mttr"`
	// Classes are the problem classes seen in the window, ordered by degraded
	// GPU-hours descending: the cost ranking a capacity owner reads first.
	Classes []RecoveryClassReport `json:"classes"`
	// Open lists incidents that are still not lifecycle-terminal at To,
	// oldest first. NEEDS_HUMAN belongs here: automation has stopped, but the
	// capacity has not been returned.
	Open []RecoveryOpenIncident `json:"open_incidents"`
	// AssumedSingleGPU counts node-scoped incidents whose node had no
	// registered GPU inventory and were therefore charged one GPU. It exists
	// so the undercount is visible instead of silently flattering the report.
	AssumedSingleGPU int `json:"assumed_single_gpu"`
	// DryRunExcluded counts incidents left out because their installation was
	// in dry-run and therefore executed nothing. Reported rather than hidden:
	// on a fresh install — dry-run is the default — an empty report with a
	// large count here means "the system is watching but not acting", which
	// is a different and much better answer than a fabricated one.
	DryRunExcluded int `json:"dry_run_excluded"`
	// Simulated is what the excluded dry-run incidents WOULD have produced,
	// present only when there were any. It is deliberately a separate object
	// rather than a set of fields alongside the real ones: nothing here may be
	// added to a number above, and a reader skimming the headline must not be
	// able to mistake one for the other.
	//
	// It exists because dry-run is the shipped default and the pilot checklist
	// tells operators to stay in it until they have watched the system decide.
	// For that entire period every number above is zero by construction, so
	// the question that decides whether to enable enforcement — "what would
	// this have got us?" — had a blank table for an answer.
	Simulated *SimulatedRecovery `json:"simulated,omitempty"`
}

// SimulatedRecovery is the dry-run half of the window: the same arithmetic
// over incidents that ran the whole ladder against synthetic successes.
//
// Every field is named in the conditional on purpose. A dry-run incident
// reached RESOLVED without touching a node, so "recovered" would be a claim
// about capacity that was never returned; "would recover" is a claim about
// what the policy decided, which is exactly what a pilot is evaluating and is
// all this can honestly support.
type SimulatedRecovery struct {
	Incidents int `json:"incidents"`
	// WouldRecover counts dry-run incidents that reached RESOLVED, i.e. the
	// ladder ran to completion without needing a human.
	WouldRecover int `json:"would_recover"`
	// WouldRecoverUnattended is the subset that never minted an approval
	// round — the automation's projected yield if enforcement were on.
	WouldRecoverUnattended int `json:"would_recover_unattended"`
	// DegradedGPUHours is real: the hardware was genuinely degraded for this
	// long. Only the recovery is hypothetical.
	DegradedGPUHours float64 `json:"degraded_gpu_hours"`
	// WouldRecoverGPUHours is the share belonging to incidents the ladder
	// carried to RESOLVED. Nothing was returned to service; this is what
	// enforcement would have addressed.
	WouldRecoverGPUHours float64 `json:"would_recover_gpu_hours"`
	// MTTR here is how long the ladder TOOK to decide, not how long a repair
	// took — no repair happened.
	MTTR RecoveryLatency `json:"mttr"`
}

// RecoveryLatency is a duration distribution computed from the raw incident
// durations, not from histogram buckets: with the store as the source there
// is no reason to report an interpolated quantile when the exact one is
// available. Percentiles are nearest-rank.
type RecoveryLatency struct {
	Samples     int     `json:"samples"`
	MeanSeconds float64 `json:"mean_seconds"`
	P50Seconds  float64 `json:"p50_seconds"`
	P90Seconds  float64 `json:"p90_seconds"`
}

// RecoveryClassReport is one problem class's contribution to the window.
type RecoveryClassReport struct {
	Class               ProblemClass    `json:"class"`
	Incidents           int             `json:"incidents"`
	DegradedGPUHours    float64         `json:"degraded_gpu_hours"`
	RecoveredGPUHours   float64         `json:"recovered_gpu_hours"`
	Recovered           int             `json:"recovered"`
	RecoveredUnattended int             `json:"recovered_unattended"`
	MTTR                RecoveryLatency `json:"mttr"`
}

// RecoveryOpenIncident is an incident still costing capacity at the end of
// the window, with the GPU-hours it has cost inside it.
type RecoveryOpenIncident struct {
	ID               string        `json:"id"`
	Class            ProblemClass  `json:"class"`
	Node             string        `json:"node"`
	GPUUUID          string        `json:"gpu_uuid,omitempty"`
	State            IncidentState `json:"state"`
	OpenedAt         time.Time     `json:"opened_at"`
	DegradedGPUHours float64       `json:"degraded_gpu_hours"`
}
