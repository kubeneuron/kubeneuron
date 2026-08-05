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

func (c *stateCollector) Describe(ch chan<- *prometheus.Desc) { ch <- c.desc }

func (c *stateCollector) Collect(ch chan<- prometheus.Metric) {
	counts := c.count()
	for _, state := range incidentStates {
		ch <- prometheus.MustNewConstMetric(
			c.desc, prometheus.GaugeValue, float64(counts[state]), string(state))
	}
}
