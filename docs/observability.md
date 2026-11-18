# Observability

KubeNeuron detects and recovers without any metrics stack. The metrics path
is for you: dashboards, metric-based detection of the faults the agent cannot
see itself, and the numbers behind them. This page says what each part of
that path promises, which combinations work, and what happens when a part of
it stops. Field-by-field configuration is in
[Production install](install.md#metrics-path-specobservability).

## What runs where

```text
controller, agents, operator ──OTLP/HTTP──▶ Collector ──OTLP──▶ metrics store ◀── Grafana
dcgm-exporter (if present)  ◀──scrape─────┘                       │
                                                                   ▼
                                     vmalert: recording rules, and metric alerts ──▶ controller

agents ──mTLS──▶ controller        (detection and recovery: no metrics involved)
controller ──▶ database            (incidents, approvals, audit trail, GPU-hours)
```

`deploy/install.sh` writes `spec.observability` with `collector`, `store`
and `grafana` all `Managed`, so the operator runs all four parts in the
installation namespace. An installation created without these blocks, or
upgraded from a release that had none, keeps them `Disabled`: an upgrade
never starts a Collector, a store or Grafana by itself. Re-running the
installer without `--observability` leaves an existing installation's
`observability` block as it is, External endpoints and Secret references
included; only a new installation gets `Managed` by default. An explicit
`--observability managed` or `--observability disabled` always applies.
Before it applies anything Managed, the installer prints: "Installing
third-party components, unmodified, pulled from their publishers:
OpenTelemetry Collector, VictoriaMetrics, vmalert (Apache-2.0) and Grafana
(AGPL-3.0). Use --observability disabled to skip them."
`spec.observability.alertmanager` is optional and informational: absent
means none, and the Managed store sends its alerts to the controller
without one.

## The three contracts

**1. KubeNeuron's own metrics.** The controller, agents and operator keep
their Prometheus `/metrics` endpoints and, with a Collector, push the same
registry over OTLP: one set of instruments, the same `kubeneuron_*` names
and labels on both paths. The names stay as they are; the OpenTelemetry GPU
semantic conventions are still in development, and
[Metrics](reference-metrics.md) lists every series. Feed one backend from
OTLP or from a scrape of `/metrics`, never both, or every series is stored
twice. Each series carries `k8s.namespace.name`, `kubeneuron.installation`
and `k8s.node.name`, never the names of your pods, jobs or namespaces.

**2. The metrics store.** The Managed store is single-node VictoriaMetrics
(Apache-2.0) on a 20Gi claim with 30 days of retention. It has no backup and
no authentication: a NetworkPolicy admits only the Collector, vmalert and
Grafana. It is a starter store; for anything you need to keep, point the
Collector at your own store instead. The Managed store comes with vmalert,
which writes the `kubeneuron_gpu_*` series the dashboards read and sends
metric alerts to the controller. An External store gets neither: run
`configs/vmalert/gpu-recording-rules.yaml` and
`configs/vmalert/gpu-alert-rules.yaml` against it yourself.

**3. Dashboards.** A Managed Grafana runs the unmodified Grafana OSS image
(AGPL-3.0), pulled from Docker Hub, with the six boards from
`deploy/grafana` and the store as a read-only datasource. With your own
Grafana, import those JSON files and add the store as a Prometheus
datasource.

The audit trail and the GPU-hours report never travel over this path: they
stay in the controller's database.

## Combinations

| Collector | Store | Grafana | Works |
|---|---|---|---|
| Managed | Managed | Managed or Disabled | yes: the default |
| Managed | External | Managed (with `grafana.datasource.url`) or Disabled | yes: your store, our Collector and rules you run |
| External | External | Managed (with `grafana.datasource.url`) or Disabled | yes: your Collector routes the metrics; the store is only Grafana's datasource |
| **External** | **Managed** | any | **not offered**: the Managed store accepts writes only from the Managed Collector and has no authentication to open it to yours |
| Disabled | Disabled | Disabled | yes: no metrics path at all |

The installation is rejected for the External Collector with a Managed
store, and for a Managed Grafana without a store to read.

## Detection from metric alerts

On NVIDIA six fault classes reach KubeNeuron only as metric alerts, because
the agent's kernel-log path does not see them: `thermal`, `power`,
`row-remap-budget`, `pcie`, `exporter-down` and `agent-down`. With the
Managed store vmalert evaluates their rules and the controller opens the
incidents; the table of alerts and thresholds is in
[Production install](install.md#metrics-path-specobservability).
`exporter-down` and `agent-down` only record and notify: no data is not
evidence of a GPU fault.

What has been checked: each rule against synthetic series in a
VictoriaMetrics and vmalert of the pinned versions, and delivery to the
controller with the real vmalert request. For `pcie` only the rule
expression is checked. None of the six has yet been tried on NVIDIA
hardware with the fault itself.

`AlertDetectionConfigured` on the KubeNeuron resource says whether metric
alerts reach the controller: `True` with the Managed store once vmalert
runs; with an External store `False` until the controller accepts the first
alert.

## When telemetry is lost

| What stops | What you lose | What keeps working |
|---|---|---|
| Collector | new metrics; `ObservabilityCollectorReady` shows the loss. No incident opens: the liveness alerts fire only while the controller's own series still reaches the store, and it comes through the same Collector | detection from the agents, recovery, approvals, audit |
| Metrics store | dashboards and the six metric-alert classes | detection from the agents, recovery, approvals, audit |
| vmalert | the `kubeneuron_gpu_*` series the GPU dashboards read, and the six metric-alert classes; `ObservabilityRulesReady` turns `False` | detection from the agents, recovery, approvals, audit |
| Grafana | dashboards | everything else |
| Controller | incidents from metric alerts until it is back; vmalert sends a firing alert again at every evaluation, so it arrives then | metrics and dashboards |
| everything (`--observability disabled`) | metrics, dashboards and the six metric-alert classes | detection from the agents, recovery, approvals, audit |

A component never waits for telemetry: a failed export is dropped and
counted in `kubeneuron_telemetry_export_failures_total`, and the next one
carries current values. `ObservabilityCollectorReady`,
`ObservabilityStoreReady`, `ObservabilityRulesReady` and
`ObservabilityGrafanaReady` report each Managed part.

## Uninstalling

`deploy/install.sh --uninstall` keeps the metrics store's claim, with up to
30 days of metrics including node names, and prints the command to erase
it. `--uninstall --delete-data` deletes it together with the controller's
state, after confirmation. Only the claim labelled as this installation's
store is deleted: another installation's in the same namespace and an
External store are never touched.
