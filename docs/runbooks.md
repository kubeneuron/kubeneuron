# Alert runbooks

One entry per shipped alert. `runbook_url` on every rule points at the
matching heading. General triage order: check the incident the alert opened
(`kubeneuronctl incidents --node <node>`), then the dashboard, then this
page. In DryRun mode every "playbook" reference below is a logged plan, not
an executed action.

## GPU signal alerts (feed the controller webhook)

### GpuDoubleBitEccVolatile

Uncorrectable (double-bit) ECC error in the last 5 minutes. **Critical.**
Workloads on this GPU may have produced corrupt results. Expect a
`drain-and-reset` incident; verify workloads rescheduled, then confirm row
remap took effect (see `GpuRowRemapPending`). Recurrence after a reset →
RMA path.

### GpuDoubleBitEccAggregate

Lifetime DBE count is elevated (>4). Not an active fault by itself — an
RMA-evaluation signal. Check the trend on the ECC dashboard panel; pair
with `GpuRemappedRowsHigh` to decide replacement.

### GpuRowRemapPending

A row remap is queued and needs a GPU reset to apply. Expect a
`reset-when-idle` incident; the reset must wait for the GPU to be idle. If
it stays pending for days, the node never idles — schedule a maintenance
window.

### GpuRowRemapFailure

Row remap **failed**: memory can no longer be remapped around the fault.
**Critical.** The device is done — expect the `rma` playbook: drain,
quarantine, hardware escalation. Do not return the GPU to service.

### GpuRemappedRowsHigh

≥8 uncorrectable remapped rows: the remap budget (bank exhaustion proxy) is
nearly spent. Plan RMA before `GpuRowRemapFailure` fires in production
hours.

### GpuNvlinkCrcErrors

Sustained NVLink CRC errors. Check cabling/backplane seating and NVLink
topology first — this is frequently mechanical. Expect `drain-and-reset`;
if errors persist across reset, escalate to hardware.

### GpuPcieReplayHigh

PCIe replay storm (>1000/h). Observe first (the shipped policy does):
transient storms accompany thermal events. Persistent → reseat the card /
riser; recurring after reseat → RMA.

### GpuThermalThrottle

HW slowdown/thermal throttle asserted ≥10m. **Facility signal, not a GPU
fault — do not reset.** Check inlet temperature, airflow, and neighboring
nodes; a rack-level pattern means cooling, a single-GPU pattern means a dead
fan or dried paste.

### GpuTempCritical

Sustained >88 °C. Drain the node and investigate cooling immediately;
sustained critical temperature degrades the device.

### GpuPowerBrake

External HW power brake asserted ≥10m — PSU/PDU/facility power event.
Check the power side; not remediable from software.

### GpuExporterDown

dcgm-exporter on the node stopped answering scrapes for 5m: driver hang,
exporter crash, or node failure. Per-GPU alerts from this node are inhibited
while this fires. Check the exporter pod, then `nvidia-smi` responsiveness
on the node; a hung driver follows the drain → reset → reboot ladder.

### KubeNeuronAgentDown

The KubeNeuron agent on the node is unreachable (3m). Automation for this
node is effectively suspended — the controller will not act without a live,
acknowledged agent. Check the agent pod (`kubectl -n <ns> logs ds/<name>-agent`),
its `/readyz`, and the mTLS material if it logs TLS errors.

## Self-monitoring alerts (route to humans)

### KubeNeuronControllerDown

The controller is unreachable (3m): no ingestion, no reconcile walk, no
notifications. Downstream KubeNeuron alerts are inhibited while this fires.
Check the Deployment (`Recreate` on SQLite installs — a rollout implies a
short gap; PostgreSQL HA rolls without one), the PVC, and controller logs. Signals are not lost: Alertmanager retries and agents
spool events durably.

### KubeNeuronIncidentNeedsHuman

Automation deliberately stopped: quarantine after flapping, a rejected
approval, or an exhausted escalation ladder. Read the incident's audit
trail (`kubeneuronctl incidents show <id>`), fix or RMA the underlying
issue, then `resolve` with your actor identity. The node stays cordoned
until a human decides.

### KubeNeuronSignalsDropped

The ingest queue overflowed — incidents may be missing. Usually a signal
storm or a stalled store. Check controller CPU/IO and the reconcile
latency panel; consider raising alert grouping in Alertmanager during the
storm.

### KubeNeuronNotificationsDropped

The notification delivery queue overflowed; **approval requests may be
among the lost messages**. Check Slack/webhook reachability. Pending
approvals are still visible via `kubeneuronctl incidents --state AWAITING_APPROVAL`
— nothing is lost except the ping.

### KubeNeuronAgentSpoolBacklog

An agent has >100 undelivered events for 15m: the controller is unreachable
*from that node* (network policy, DNS, TLS). The spool is durable and
bounded; events replay on reconnect. Check connectivity from the node to
the controller Service on :8443.

### KubeNeuronTLSCertExpiringSoon

A loaded TLS artifact expires in under 30 days. **Do not wait**: rotation
is manual and phased. Follow the rotation procedure with
`hack/tls-rotate.sh` (server and client directions separately). The fleet
client leaf has a hard 100-day ceiling; an expired leaf takes agent
transport down fleet-wide.

### KubeNeuronIncidentExpired

An approval request timed out (default TTL 12h) and the incident moved to
`EXPIRED`. **Nothing uncordons the node on expiry** — if the ladder had
already run its cordon/drain rungs, the node is still unschedulable and
its workloads are gone.

1. Find it and see how far it got:
   `kubeneuronctl incidents --state EXPIRED` then
   `kubeneuronctl incidents show <id>` — the audit trail names the last
   executed step.
2. Decide the disposition. An expired incident is closed: it cannot be
   approved. Either fix the fault out of band and release the node, or
   re-open remediation with
   `kubeneuronctl remediate <node> --class <class> --actor <you>`.
3. Release the node when you are done:
   `kubectl uncordon <node>` (check `kubectl get node <node>` first — a
   node cordoned by something else must not be uncordoned by reflex).

Prevention: shorten `spec.approvals.ttl` so a forgotten request fails fast,
or route approvals to a channel with an on-call rotation. Notifications are
droppable by design, so treat this alert — not the Slack ping — as the
reliable signal.

### KubeNeuronAgentNeverAcked

The agent process is alive and serving metrics, but the controller has
never acknowledged its registration — so this node's faults are invisible
and no remediation can execute on it, while the pod looks healthy.
`KubeNeuronAgentDown` will NOT fire for this.

1. Read the agent's own verdict:
   `kubectl -n <ns> logs ds/<name>-agent | grep -i "registration"` — look
   for `controller registration never acknowledged` and the error it
   carries (TLS, 401/403, connection refused).
2. Follow the identity ladder in
   [troubleshooting](troubleshooting.md#agent-cannot-register): CA
   mismatch, expired leaf (hard 100-day ceiling), URI-SAN vs. a recreated
   root UID, Pod-token race.
3. **If the whole fleet is affected at once** (expired or wrong CA), do not
   reach for the phased rotation — that is a planned-change tool. Use
   `hack/tls-emergency-recover.sh` with explicitly approved replacement
   leaf Secrets; it retains both CA bundles and records recovery
   annotations.

### KubeNeuronStackRestoreFailing

The accelerator-stack janitor keeps failing to restore a node whose vendor
stack was quiesced for a reset. That node's GPU monitoring is **down**, and
because `GpuExporterDown` inhibits its per-GPU alerts, the blind spot hides
itself. Act on this alert, not on the silence.

1. Read why the janitor is holding:
   `kubectl -n <ns> logs deploy/<name>-controller | grep -iE "restore|quiesce"`.
   The janitor deliberately holds the restore until every incident that
   quiesced the node has been rewound — an incident stuck mid-rewind blocks
   it.
2. Clear the blocker: resolve or escalate that incident
   (`kubeneuronctl incidents show <id>`, then `resolve` if the hardware is
   fine). The next janitor pass restores the stack.
3. Last resort, by hand on the node: restore the GPU-operator labels the
   quiesce removed (`nvidia.com/gpu.deploy.*=true`) and confirm the
   device-plugin and DCGM pods return. Re-check
   `kubeneuron_stack_restore_failures_total` stops growing.

### KubeNeuronAuthFailureBurst

More than ten rejected authentications in five minutes on the named API
surface. Either a misconfigured client (a rotated token that some caller
did not pick up) or someone probing. The per-source-IP limiter throttles
online brute force, so this is a signal to investigate, not an outage.
Check the controller log lines `operator authentication failed` for the
source, and verify the token Secrets match what callers hold.

### KubeNeuronActionQueueStuck

Agent actions have been pending for 30 minutes: work was dispatched that
nobody claimed. Usually the target node's agent is unreachable,
unregistered (see **KubeNeuronAgentNeverAcked**), or its lease keeps
expiring. Identify the node from the incident
(`kubeneuronctl incidents --state EXECUTING`) and check that agent first.

### KubeNeuronAgentEventsRejected

The controller semantically rejected events from this node's agent, so the
agent dropped them rather than spooling forever. Detections are being
discarded. Almost always a partial upgrade: a newer agent encoding that
this controller does not understand. Compare image versions and complete
the rollout.

### KubeNeuronReconcileSlow

The reconcile walk's p99 exceeded 5s for 15 minutes: approvals resume late
and verification windows stretch. Check the open-incident count
(`kubeneuron_incidents`) and store latency — SQLite on a slow PVC and a
large open-incident backlog are the usual causes.
