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
| `kubeneuron_actions_pending` | gauge | — | agent actions dispatched and not yet claimed; sustained non-zero means an unreachable or unregistered agent |
| `kubeneuron_reconcile_seconds` | histogram | — | reconcile-walk duration; p99 above a few seconds delays approvals and verification |
| `kubeneuron_stack_restore_failures_total` | counter | — | failed accelerator-stack restores by the janitor — a growing rate means a node's GPU monitoring is staying down |
| `kubeneuron_runtime_config_info` | gauge | `digest` | identity of the loaded runtime configuration (always 1); a digest lagging `KubeNeuron.status.configDigest` is a rollout that never landed |
| `kubeneuron_incident_duration_seconds` | histogram | `class`, `outcome` | open-to-halted wall time — MTTR, split by how the incident ended |
| `kubeneuron_incidents_recovered_total` | counter | `class`, `unattended` | incidents that reached RESOLVED; `unattended="true"` never needed a human decision. Dry-run incidents are excluded — dry-run executes nothing, so it recovers nothing |
| `kubeneuron_degraded_gpu_seconds_total` | counter | `class`, `outcome` | GPU-seconds charged when an incident reached a terminal state; the `outcome="resolved"` share is capacity returned to service (÷3600 for GPU-hours). Recorded once per incident, on its terminal transition only, and never for dry-run — so an incident parked in `NEEDS_HUMAN` contributes **nothing here** until somebody closes it |
| `kubeneuron_degraded_gpus` | gauge | `class`, `owner` | accelerators under a non-terminal incident right now. `owner="human"` is the `NEEDS_HUMAN` population — capacity that stays lost until a person acts — and is exactly what the counter above cannot show. Node-scoped incidents expand to the node's GPU count |
| `kubeneuron_workloads_evicted_total` | counter | `reason` | GPU workloads moved off a node ahead of a destructive step, by problem class — what remediation cost. No `node` label by design: this control plane replaces nodes, so node names are an unbounded set and a per-node series would grow for the life of the process. Per-node detail lives in the incident record and audit trail |
| `kubeneuron_destructive_steps_deferred_total` | counter | `reason` | destructive steps that did **not** run, by the guard that stopped them — what remediation deliberately did not cost |
| `kubeneuron_gate_denials_total` | counter | — | steps denied by the safety gate (pause, cooldown, concurrency, capability) |
| `kubeneuron_escalations_total` | counter | — | ladder escalations after step/verification failures |
| `kubeneuron_notifications_dropped_total` | counter | `kind` (`event`, `approval_request`, `dead_letter`) | notifications lost to queue overflow or dead-lettered after delivery retries |
| `kubeneuron_auth_failures_total` | counter | `api` (`operator`, `webhook`, `agent`) | rejected authentication attempts; repeated failures from one source are throttled with `429` |
| `kubeneuron_tls_certificate_not_after_seconds` | gauge | `certificate` | NotAfter (unix s) of every loaded TLS artifact; bundles report their earliest expiry |

`kubeneuron_destructive_steps_deferred_total` `reason` values: `not_idle`
(an idle guard found the device busy), `device_holders` (processes hold the GPU
that KubeNeuron cannot release), `maintenance_window`, `node_paused` (a
GPUNodeConfig pause), `global_pause` (the fleet-wide switch), `concurrency_cap`,
`playbook_cooldown`, `unarmed_agent`, `confinement` (outside the declared
destructiveExecution blast radius), `recycle_not_viable`, and
`accelerator_evidence` (a reset held for missing or stale runtime evidence).
It counts *decisions*, so a hold that persists — a maintenance window, a paused
node — is counted again on every reconcile pass: read it with `rate()` as "how
much protection is currently in force", and one-shot refusals as single events.
Dry-run incidents and playbooks with no disruptive rung never appear, because
neither was going to touch a workload.

`not_idle` counts only a guard the agent reported as a refusal: the device was
genuinely held by live processes. An idle probe that could not run at all — a
missing `nvidia-smi`, a wedged driver, a timeout — fails the same step and
fails just as closed, but it is not evidence that a workload was spared, so it
is deliberately absent. An agent older than the refusal field reports no
refusals, which under-counts protection rather than inventing it. A `not_idle`
refusal also stops the ladder and hands the incident to a human rather than
escalating, because every rung above the guard is more destructive than the one
it just stopped — expect `kubeneuron_incidents_recovered_total` not to move for
these and `NEEDS_HUMAN` to gain one.

The two recovery series answer different questions and must be read together.
The counter is what FINISHED: closed incidents, charged once, so a park and
unpark cannot bill the same hour twice. The gauge is what is HAPPENING: it
includes every parked incident, which the counter deliberately omits. To get
degraded GPU-hours over a window including parked incidents, integrate the
gauge (`sum_over_time(kubeneuron_degraded_gpus[7d]) * <scrape interval> / 3600`)
or ask `kubeneuronctl report`, which computes it from the incident store and
does charge parked incidents.

`certificate` label values: `controller-server-leaf`, `agent-client-ca`,
`public-server-leaf` (when public TLS is enabled) on the controller;
`fleet-client-leaf`, `controller-server-ca` on the agent.

## Agent

| Series | Type | Meaning |
|---|---|---|
| `kubeneuron_agent_events_posted_total` | counter | XID events delivered to the controller |
| `kubeneuron_agent_events_spooled_total` | counter | events diverted to the durable spool after a failed post |
| `kubeneuron_agent_spool_depth` | gauge | undelivered events currently spooled |
| `kubeneuron_agent_detections_total` | counter | `node`, `source` | faults observed per detection source (`kmsg`, `gpuhealth`) — the only way to tell which source saw a fault |
| `kubeneuron_agent_detections_deduplicated_total` | counter | `node` | detections collapsed as duplicates of one physical fault across sources |
| `kubeneuron_agent_events_rejected_total` | counter | `node` | events the controller semantically rejected and the agent dropped — non-zero means detections are being discarded |
| `kubeneuron_agent_registration_acks_total` | counter | durable controller registration acknowledgments |

Both processes also export the standard Go runtime and process collectors.

## Shipped alerts

The dependency profile's `kubeneuron-self` VMRule group covers:
`KubeNeuronControllerDown`, `KubeNeuronAgentDown`,
`KubeNeuronIncidentNeedsHuman`, `KubeNeuronSignalsDropped`,
`KubeNeuronNotificationsDropped`, `KubeNeuronAgentSpoolBacklog`, and
`KubeNeuronTLSCertExpiringSoon` (30 days before expiry), plus the
round-12 additions: `KubeNeuronAuthFailureBurst`,
`KubeNeuronStackRestoreFailing`, `KubeNeuronActionQueueStuck`,
`KubeNeuronIncidentExpired`, `KubeNeuronAgentEventsRejected`,
`KubeNeuronAgentNeverAcked`, and `KubeNeuronReconcileSlow`. Running your own
Prometheus instead of the reference profile? Load
[`configs/vmalert/self-rules.yaml`](https://github.com/kubeneuron/kubeneuron/blob/main/configs/vmalert/self-rules.yaml)
— every rule links to its [runbook](runbooks.md) entry. GPU-signal alerts
(`Gpu*`) live in the `gpu-*` groups and feed the controller webhook; unit
tests pin the deployed rules to both canonical files.
