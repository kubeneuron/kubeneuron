package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// Agent-side metrics, served from the agent's health listener. The shared
// default registry also exports Go runtime and process collectors.
var (
	// AgentEventsPosted counts XID events delivered to the controller.
	AgentEventsPosted = promauto.NewCounter(prometheus.CounterOpts{
		Name: "kubeneuron_agent_events_posted_total",
		Help: "Agent events successfully posted to the controller.",
	})

	// AgentEventsSpooled counts events diverted to the durable spool after a
	// failed post; a growing rate means the controller is unreachable.
	AgentEventsSpooled = promauto.NewCounter(prometheus.CounterOpts{
		Name: "kubeneuron_agent_events_spooled_total",
		Help: "Agent events written to the local spool after a failed post.",
	})

	// AgentSpoolDepth is the current number of spooled, undelivered events.
	AgentSpoolDepth = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "kubeneuron_agent_spool_depth",
		Help: "Events currently queued in the agent's durable spool.",
	})

	// AgentRegistrationAcks counts durable controller registration
	// acknowledgments; staleness here mirrors the /readyz probe.
	AgentRegistrationAcks = promauto.NewCounter(prometheus.CounterOpts{
		Name: "kubeneuron_agent_registration_acks_total",
		Help: "Durable controller registration acknowledgments received.",
	})
)
