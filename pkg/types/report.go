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
