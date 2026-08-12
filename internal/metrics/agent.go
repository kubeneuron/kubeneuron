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

	// AgentEventsRejected counts events the controller permanently rejected
	// (HTTP 400/413) and the agent therefore dropped instead of spooling or
	// replaying — a nonzero rate means an agent is emitting payloads the
	// controller refuses and detections are being discarded.
	AgentEventsRejected = promauto.NewCounter(prometheus.CounterOpts{
		Name: "kubeneuron_agent_events_rejected_total",
		Help: "Agent events permanently rejected by the controller and dropped.",
	})

	// AgentRegistrationAcks counts durable controller registration
	// acknowledgments; staleness here mirrors the /readyz probe.
	AgentRegistrationAcks = promauto.NewCounter(prometheus.CounterOpts{
		Name: "kubeneuron_agent_registration_acks_total",
		Help: "Durable controller registration acknowledgments received.",
	})

	// AgentDetections counts observed detection signals by their source:
	// "kmsg" (NVRM Xid lines), "kmsg-amd" (amdgpu kernel families),
	// "gpuhealth" (the DCGM/nvidia-smi poll) and "amdhealth" (the
	// amd-smi/rocm-smi poll). The vendor paths are separate label values on
	// purpose — a dead AMD source is otherwise indistinguishable from a
	// healthy AMD fleet. It counts detections before agent-side
	// deduplication, so the sources can be compared independently.
	AgentDetections = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "kubeneuron_agent_detections_total",
		Help: "GPU fault detections observed by the agent, labeled by detection source.",
	}, []string{"source"})

	// AgentHealthSource reports which second-source health probe actually
	// served the most recent poll: "dcgm", "nvidia-smi", or "none". Exactly
	// one label value is 1 at a time.
	//
	// It exists because nothing observable distinguished the three. DCGM is
	// PREFERRED and nvidia-smi is the fallback, so a fleet whose DCGM engine
	// is unreachable degrades silently to a narrower source and looks
	// identical from outside — and the hardware harness had no way to prove
	// the DCGM path had run at all, which is how a phase that proved nothing
	// was reported as evidence that it had.
	AgentHealthSource = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "kubeneuron_agent_health_source",
		Help: "Which second-source GPU health probe served the last poll (1 for the active source).",
	}, []string{"source"})

	// AgentDetectionsDeduplicated counts detections dropped because an
	// equivalent fault (same GPU and XID) was already emitted within the
	// dedup window — most often the same fault seen by both kmsg and DCGM.
	AgentDetectionsDeduplicated = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "kubeneuron_agent_detections_deduplicated_total",
		Help: "Detections suppressed by agent-side deduplication, labeled by the source that was dropped.",
	}, []string{"source"})
)
