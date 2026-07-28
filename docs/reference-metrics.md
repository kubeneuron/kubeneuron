# Metrics reference

All KubeNeuron series carry the `kubeneuron_` prefix. The controller serves
them on the public listener at `/metrics`; the agent on its health port
(`:9402`) at `/metrics`. The dependency profile's vmagent scrapes both, plus
the operator's controller-runtime metrics.

## Controller

| Series | Type | Labels | Meaning |
|---|---|---|---|
| `kubeneuron_incidents` | gauge | `state` | current incidents per state; alert on `state="NEEDS_HUMAN"` |
| `kubeneuron_signals_total` | counter | `source`, `class` | normalized signals entering the pipeline |
| `kubeneuron_signals_dropped_total` | counter | — | signals lost to ingest-queue overflow |
| `kubeneuron_events_duplicate_total` | counter | — | agent-event replays rejected by capture ID |
| `kubeneuron_incidents_opened_total` | counter | `class` | incidents opened |
| `kubeneuron_steps_total` | counter | `outcome` (`ok`, `failed`, `dry_run`) | playbook steps executed |
| `kubeneuron_gate_denials_total` | counter | — | steps denied by the safety gate (pause, cooldown, concurrency, capability) |
| `kubeneuron_escalations_total` | counter | — | ladder escalations after step/verification failures |
| `kubeneuron_notifications_dropped_total` | counter | `kind` (`event`, `approval_request`, `dead_letter`) | notifications lost to queue overflow or dead-lettered after delivery retries |
| `kubeneuron_auth_failures_total` | counter | `api` (`operator`, `webhook`, `agent`) | rejected authentication attempts; repeated failures from one source are throttled with `429` |
| `kubeneuron_tls_certificate_not_after_seconds` | gauge | `certificate` | NotAfter (unix s) of every loaded TLS artifact; bundles report their earliest expiry |

`certificate` label values: `controller-server-leaf`, `agent-client-ca`,
`public-server-leaf` (when public TLS is enabled) on the controller;
`fleet-client-leaf`, `controller-server-ca` on the agent.

## Agent

| Series | Type | Meaning |
|---|---|---|
| `kubeneuron_agent_events_posted_total` | counter | XID events delivered to the controller |
| `kubeneuron_agent_events_spooled_total` | counter | events diverted to the durable spool after a failed post |
| `kubeneuron_agent_spool_depth` | gauge | undelivered events currently spooled |
| `kubeneuron_agent_registration_acks_total` | counter | durable controller registration acknowledgments |

Both processes also export the standard Go runtime and process collectors.

## Shipped alerts

The dependency profile's `kubeneuron-self` VMRule group covers:
`KubeNeuronControllerDown`, `KubeNeuronAgentDown`,
`KubeNeuronIncidentNeedsHuman`, `KubeNeuronSignalsDropped`,
`KubeNeuronNotificationsDropped`, `KubeNeuronAgentSpoolBacklog`, and
`KubeNeuronTLSCertExpiringSoon` (30 days before expiry). GPU-signal alerts
(`Gpu*`) live in the `gpu-*` groups and feed the controller webhook; a unit
test pins the deployed rules to the canonical
`configs/vmalert/gpu-rules.yaml`.
