// Package metrics exposes the controller's Prometheus instrumentation. All
// series carry the kubeneuron_ prefix; scraping is served on the public
// listener at /metrics (no node secrets appear in label values).
package metrics

import (
	"crypto/x509"
	"encoding/pem"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/kubeneuron/kubeneuron/pkg/types"
)

var (
	// SignalsTotal counts normalized signals entering the incident pipeline.
	SignalsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "kubeneuron_signals_total",
		Help: "Normalized signals ingested, by source and problem class.",
	}, []string{"source", "class"})

	// SignalsDropped counts signals lost to ingest-queue overflow.
	SignalsDropped = promauto.NewCounter(prometheus.CounterOpts{
		Name: "kubeneuron_signals_dropped_total",
		Help: "Signals dropped because the ingest queue was full.",
	})

	// DuplicateEvents counts at-least-once replays rejected by event ID.
	DuplicateEvents = promauto.NewCounter(prometheus.CounterOpts{
		Name: "kubeneuron_events_duplicate_total",
		Help: "Agent events ignored as replay duplicates.",
	})

	// IncidentsOpened counts incident creation by class.
	IncidentsOpened = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "kubeneuron_incidents_opened_total",
		Help: "Incidents opened, by problem class.",
	}, []string{"class"})

	// StepsExecuted counts playbook step outcomes.
	StepsExecuted = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "kubeneuron_steps_total",
		Help: "Playbook steps executed, by outcome (ok, failed, dry_run).",
	}, []string{"outcome"})

	// GateDenials counts safety-gate refusals (pause, cooldown, concurrency).
	GateDenials = promauto.NewCounter(prometheus.CounterOpts{
		Name: "kubeneuron_gate_denials_total",
		Help: "Steps denied by the safety gate.",
	})

	// EscalationsTotal counts ladder escalations.
	EscalationsTotal = promauto.NewCounter(prometheus.CounterOpts{
		Name: "kubeneuron_escalations_total",
		Help: "Playbook escalations after step or verification failures.",
	})

	// StackRestoreFailures counts failed attempts by the accelerator-stack
	// janitor to restore a quiesced node's monitoring. A growing rate means a
	// node's GPU monitoring is staying down and its agent needs attention;
	// the once-only stuck-restore notification names the node.
	StackRestoreFailures = promauto.NewCounter(prometheus.CounterOpts{
		Name: "kubeneuron_stack_restore_failures_total",
		Help: "Failed accelerator-stack restore attempts by the janitor.",
	})

	// --- Recovery outcome: what the fleet got back -------------------------
	//
	// These three answer the question an operator's budget holder asks:
	// how much accelerator capacity was degraded, how much of it came back,
	// and how much of that needed a human. Everything else in this file is
	// process telemetry; this block is the outcome.

	// IncidentDuration measures open-to-halted wall time per incident —
	// KubeNeuron's MTTR. Labelled by how it ended, because "resolved in 4
	// minutes" and "escalated to a human after 4 hours" are different
	// stories that a single average would blend into nonsense.
	IncidentDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name: "kubeneuron_incident_duration_seconds",
		Help: "Wall time from incident open to its halting state, by class and outcome.",
		// 30s to ~9h: fast automated recoveries at the bottom, approval
		// waits (12h default TTL) spilling into the top bucket.
		Buckets: []float64{30, 60, 300, 900, 1800, 3600, 7200, 14400, 32400},
	}, []string{"class", "outcome"})

	// IncidentsRecovered counts incidents that ended RESOLVED, split by
	// whether a human decision was needed. unattended="true" is the
	// automation's actual yield: degradation the fleet absorbed without
	// waking anybody.
	IncidentsRecovered = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "kubeneuron_incidents_recovered_total",
		Help: "Incidents that reached RESOLVED, by class and whether they needed a human decision.",
	}, []string{"class", "unattended"})

	// DegradedGPUSeconds accumulates GPU-seconds spent under an incident that
	// REACHED A TERMINAL STATE, labelled by how it ended. It is deliberately
	// NOT called "unavailable": a degraded GPU may still have been serving.
	// Divide by 3600 for GPU-hours; the outcome="resolved" share is the
	// capacity remediation brought back.
	//
	// "Terminal" is load-bearing and is the one thing to understand before
	// reading this series. It is recorded exactly once, when the incident
	// resolves or expires, so a park/unpark cycle cannot charge the same time
	// twice — and so an incident sitting in NEEDS_HUMAN contributes NOTHING
	// here until somebody closes it. That population is the fleet's worst
	// capacity loss, so read this counter beside kubeneuron_degraded_gpus,
	// which is the scrape-time gauge of what is degraded right now, including
	// everything a human still owns. `kubeneuronctl report` answers the
	// windowed version of the same question from the incident store and does
	// charge parked incidents.
	DegradedGPUSeconds = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "kubeneuron_degraded_gpu_seconds_total",
		Help: "GPU-seconds charged when an incident reached a terminal state, by class and outcome (resolved = returned to service). Parked incidents are counted by kubeneuron_degraded_gpus until they close.",
	}, []string{"class", "outcome"})

	// --- Protection: what the fleet did NOT lose ---------------------------
	//
	// Sibling of the recovery-outcome block above, and outcome rather than
	// process telemetry for the same reason: the system protects workloads
	// constantly — evicting GPU pods ahead of a destructive step, refusing a
	// reset while the device is busy, holding for a maintenance window,
	// blocking on a concurrency cap — and until these two series none of it
	// was countable. The number of times automation chose NOT to disrupt is
	// the protection story; without it, a platform team can only see the
	// disruptions that did happen.

	// WorkloadsEvicted counts GPU workloads moved off a node ahead of a
	// destructive step, by the problem class that caused it. reason is the
	// incident's class rather than the step name: "which faults cost us
	// evictions" is the question a capacity owner asks, and the step is always
	// the same one.
	//
	// There is deliberately no node label. Node names are not a bounded set in
	// a KubeNeuron fleet — this control plane REPLACES nodes, so every
	// ReplaceNode mints a name that never appears again, and a cluster
	// autoscaler mints more. A per-node series would grow for the life of the
	// process and never shrink, which is the classic way a metric takes the
	// monitoring stack down at the moment the fleet is busiest. Which node was
	// evicted is recorded where it can be queried without unbounded retention
	// cost: the incident record and its audit trail.
	WorkloadsEvicted = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "kubeneuron_workloads_evicted_total",
		Help: "GPU workloads evicted ahead of a destructive step, by problem class.",
	}, []string{"reason"})

	// DestructiveStepsDeferred counts destructive steps that did NOT run, by
	// why. Use the Defer* constants below — the label set is closed, so a new
	// deferral path adds a named constant rather than an ad-hoc string.
	//
	// It counts DECISIONS, not incidents: a hold that persists (a maintenance
	// window, a paused node) is re-decided on every reconcile pass and counts
	// again each time, so rate() reads as "how much protection is currently in
	// force" while a one-shot refusal appears once. Deduplicating per incident
	// would need in-memory state that a controller restart leaks.
	//
	// Dry-run incidents and playbooks with no disruptive rung are deliberately
	// excluded by the caller: neither was ever going to touch a workload, and
	// counting them would inflate the protection story with events that risked
	// nothing.
	DestructiveStepsDeferred = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "kubeneuron_destructive_steps_deferred_total",
		Help: "Destructive steps that did not run, by the guard that stopped them.",
	}, []string{"reason"})

	// RuntimeConfigInfo is an info metric identifying the loaded runtime
	// configuration: exactly one series with the digest of the
	// operator-compiled snapshot currently live in this process. Alert on it
	// disagreeing with KubeNeuron.status.configDigest for longer than the
	// kubelet ConfigMap sync period — that is a config rollout that never
	// landed. Reset+set on every successful reload keeps it single-series.
	RuntimeConfigInfo = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "kubeneuron_runtime_config_info",
		Help: "Identity of the loaded runtime configuration (value is always 1).",
	}, []string{"digest"})

	// AuthFailures counts rejected authentication attempts by API surface
	// (operator, webhook, agent). A burst is either a misconfigured client
	// or someone probing; both deserve an alert before they page as an
	// outage elsewhere.
	AuthFailures = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "kubeneuron_auth_failures_total",
		Help: "Rejected authentication attempts by API surface.",
	}, []string{"api"})

	// NotificationsDropped counts notifications lost to delivery-queue
	// overflow, by kind (event, approval_request). Approval requests are
	// among the droppable items, so this must be alertable.
	NotificationsDropped = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "kubeneuron_notifications_dropped_total",
		Help: "Notifications dropped because the delivery queue was full.",
	}, []string{"kind"})

	// ReconcileSeconds observes one full reconcile pass over the live
	// incident set. A growing tail warns of store contention or an incident
	// backlog long before signal-queue drops appear.
	ReconcileSeconds = promauto.NewHistogram(prometheus.HistogramOpts{
		Name:    "kubeneuron_reconcile_seconds",
		Help:    "Duration of one reconcile pass over non-terminal incidents.",
		Buckets: prometheus.ExponentialBuckets(0.001, 4, 8), // 1ms .. ~65s
	})

	// ActionsPending is the durable action-queue depth (queued work agents
	// have not yet completed).
	ActionsPending = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "kubeneuron_actions_pending",
		Help: "Durable queued actions not yet completed by agents.",
	})

	// TLSCertificateNotAfter exposes the expiry (unix seconds) of every TLS
	// artifact a process loaded at startup. Certificates load only at
	// process start, so these values are exact for the process lifetime;
	// alerting on them replaces an unmonitored calendar obligation (the
	// fleet leaf has a hard 100-day ceiling).
	TLSCertificateNotAfter = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "kubeneuron_tls_certificate_not_after_seconds",
		Help: "NotAfter of loaded TLS material (unix seconds), by artifact; bundles report their earliest expiry.",
	}, []string{"certificate"})
)

// Deferral reasons for DestructiveStepsDeferred. The set is closed on
// purpose: a metric whose label values are invented at each call site cannot
// be alerted on, and the value of this series is completeness — a missing
// path makes it lie by omission.
const (
	// DeferNotIdle: an idle guard (agent.idle_check / agent.wait_idle) refused
	// because the device was still in use, so the destructive rung it guards
	// never ran. Counted only when the agent said so in
	// types.ActionResult.Refusal — an idle probe that could not run at all
	// (missing nvidia-smi, wedged driver, timeout) fails the same step but is
	// not evidence that any workload was spared, and is deliberately absent
	// from this series.
	DeferNotIdle = "not_idle"
	// DeferDeviceHolders: processes hold the GPU that KubeNeuron cannot
	// release, so a reset playbook is refused before its first disruptive step.
	DeferDeviceHolders = "device_holders"
	// DeferMaintenanceWindow: a declared maintenance window covers the node.
	DeferMaintenanceWindow = "maintenance_window"
	// DeferNodePaused: the node is paused by a GPUNodeConfig.
	DeferNodePaused = "node_paused"
	// DeferConcurrencyCap: MaxConcurrentRemediations or MaxConcurrentReboots is
	// full — the guard that stops a fleet-wide fault from draining half the
	// cluster at once.
	DeferConcurrencyCap = "concurrency_cap"
	// DeferPlaybookCooldown: the target is inside a cooldown recorded by an
	// earlier run of this playbook (or of this action class on the gate).
	DeferPlaybookCooldown = "playbook_cooldown"
	// DeferUnarmedAgent: the node's agent is not armed for destructive
	// execution, so the step is held or routed away before a human is asked to
	// approve something the node will refuse.
	DeferUnarmedAgent = "unarmed_agent"
	// DeferConfinement: the node is outside — or cannot be proven inside —
	// spec.safety.destructiveExecution.nodeSelector, the declared blast radius.
	DeferConfinement = "confinement"
	// DeferRecycleNotViable: the instance behind the node provably cannot be
	// stop/started (an autoscaling-group member), so the recycle is refused
	// before approval.
	DeferRecycleNotViable = "recycle_not_viable"
	// DeferGlobalPause: the safety gate's big red button is down. Distinct from
	// node_paused, which is one node declared out of scope by configuration;
	// this is a human having stopped all automation fleet-wide, and blending
	// the two would hide which lever is actually holding a remediation.
	DeferGlobalPause = "global_pause"
	// DeferAcceleratorEvidence: the runtime evidence a capability-gated reset
	// needs is missing, stale, or bound to different hardware. The step is
	// held, not escalated, so this is genuinely a deferral: fail-closed on
	// absent evidence is the single most common reason a reset does not run.
	DeferAcceleratorEvidence = "accelerator_evidence"
)

// RecordCertBundleExpiry parses PEM material and records the earliest
// NotAfter under the given artifact name. Unparseable material records
// nothing — the TLS layer itself will reject it loudly.
func RecordCertBundleExpiry(name string, pemData []byte) {
	earliest := time.Time{}
	rest := pemData
	for {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			break
		}
		if block.Type != "CERTIFICATE" {
			continue
		}
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			continue
		}
		if earliest.IsZero() || cert.NotAfter.Before(earliest) {
			earliest = cert.NotAfter
		}
	}
	if !earliest.IsZero() {
		TLSCertificateNotAfter.WithLabelValues(name).Set(float64(earliest.Unix()))
	}
}

// Handler serves the default registry.
func Handler() http.Handler { return promhttp.Handler() }

// incidentStates is the fixed label set for the state gauge.
var incidentStates = []types.IncidentState{
	types.StateOpen, types.StateObserving, types.StateEvaluating,
	types.StateAwaitingApproval, types.StateExecuting, types.StateVerifying,
	types.StateNeedsHuman, types.StateResolved, types.StateExpired,
}

// stateCollector exports kubeneuron_incidents{state} from a count function
// evaluated at scrape time.
type stateCollector struct {
	desc  *prometheus.Desc
	count func() map[types.IncidentState]int
}

// RegisterIncidentStates installs the per-state incident gauge; count is
// called on every scrape and must be cheap (a single GROUP BY).
func RegisterIncidentStates(count func() map[types.IncidentState]int) {
	prometheus.MustRegister(&stateCollector{
		desc: prometheus.NewDesc(
			"kubeneuron_incidents",
			"Incidents currently in each state.",
			[]string{"state"}, nil,
		),
		count: count,
	})
}

// DegradedGPUsGauge is the scrape-time companion to
// DegradedGPUSeconds: how many accelerators are under a non-terminal incident
// right now, split by problem class and by whether automation is still working
// the incident or a human owns it.
//
// It exists because DegradedGPUSeconds is recorded once, on the terminal
// transition, so an incident parked in NEEDS_HUMAN contributes NOTHING to it —
// permanently, and for exactly the population that matters most, the ones
// automation could not recover. `kubeneuronctl report` deliberately keeps
// charging those incidents, so the two answers disagreed. Integrating this
// gauge over time (`sum_over_time`) closes the gap without double-counting the
// counter, which is the idiomatic split: a counter for what finished, a gauge
// for what is still happening.
type degradedGPUCollector struct {
	desc  *prometheus.Desc
	count func() map[DegradedKey]int
}

// DegradedKey identifies one currently-degraded population.
type DegradedKey struct {
	Class string
	// Owner is "automation" while the ladder is still working the incident,
	// and "human" once it has been handed over. The distinction is the point:
	// capacity sitting in NEEDS_HUMAN is lost until somebody acts, and that is
	// a different operational fact from capacity mid-remediation.
	Owner string
}

// RegisterDegradedGPUs installs the currently-degraded gauge; count is called
// on every scrape and must be cheap.
func RegisterDegradedGPUs(count func() map[DegradedKey]int) {
	prometheus.MustRegister(&degradedGPUCollector{
		desc: prometheus.NewDesc(
			"kubeneuron_degraded_gpus",
			"Accelerators currently under a non-terminal incident, by problem class and who owns it.",
			[]string{"class", "owner"}, nil,
		),
		count: count,
	})
}

func (c *degradedGPUCollector) Describe(ch chan<- *prometheus.Desc) { ch <- c.desc }

func (c *degradedGPUCollector) Collect(ch chan<- prometheus.Metric) {
	for key, n := range c.count() {
		ch <- prometheus.MustNewConstMetric(
			c.desc, prometheus.GaugeValue, float64(n), key.Class, key.Owner)
	}
}

func (c *stateCollector) Describe(ch chan<- *prometheus.Desc) { ch <- c.desc }

func (c *stateCollector) Collect(ch chan<- prometheus.Metric) {
	counts := c.count()
	// A nil map means "this process is not the one counting" — the standby of a
	// leader-elected pair, whose collector declines rather than reading the
	// shared store a second time. Publish nothing at all in that case.
	//
	// Enumerating the state list regardless would emit a full set of zeros,
	// which is a different claim: `sum()` still adds up correctly, but any
	// panel or alert reading the raw series sees a replica confidently
	// asserting there are no incidents in any state.
	if counts == nil {
		return
	}
	for _, state := range incidentStates {
		ch <- prometheus.MustNewConstMetric(
			c.desc, prometheus.GaugeValue, float64(counts[state]), string(state))
	}
}
