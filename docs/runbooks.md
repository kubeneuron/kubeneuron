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
Check the Deployment (`Recreate` — a rollout implies a short gap), the PVC,
and controller logs. Signals are not lost: Alertmanager retries and agents
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
