# Security review — v0.7.0 GPU reliability observability (source review)

Status: **source review of the v0.7 work in this tree, written ahead of the
release.** v0.7.0 is not tagged, and the release plan's local, kind and
hardware gates have not been run on this code. This document reviews the
files as committed (the exporter profile, the scrape label contract, the
recording and shadow rules and the Alertmanager route) and the unit tests
that pin them. It is not a third-party audit and it is **not a hardware
validation**: nothing reviewed here has executed against a GPU, a live
dcgm-exporter or a live Alertmanager in this tree, and the review says
nothing about how the exporter behaves with the wider field list on real
hardware. The project's latest hardware evidence remains the v0.6.0-era
Tesla T4 run, which predates every file reviewed here. Report anything that
contradicts this page through
[SECURITY.md](https://github.com/kubeneuron/kubeneuron/blob/main/SECURITY.md).

## Scope, and why it is short

v0.7 adds an observability layer and nothing else: a metric catalog, a wider
dcgm-exporter metrics profile, a few scrape labels, recording rules, five
Grafana boards and shadow-only alerts. It adds no code to the controller,
the agent or the operator, no action, no playbook rung, no webhook path, no
CRD field, no store migration and no RBAC for any KubeNeuron component. The
previous reviews ([v0.5.0](security-review-v0.5.0.md),
[v0.6.0](security-review-v0.6.0.md)) and the [design document](design.md)
still describe every authority-bearing surface, unchanged. Three things in
v0.7 touch a trust boundary, and this review covers exactly those:

1. the ConfigMap-driven exporter profile,
2. the read-only node metadata vmagent now reads, and the RBAC it rests on,
3. the shadow alerts and their routing to humans.

## 1. The exporter profile is a ConfigMap, owned by GPU Operator

The reliability profile is the `dcgmExporter.config.data` block of
`deploy/kubernetes/dependencies/gpu-operator/values.yaml`. GPU Operator
renders it into the `kubeneuron-dcgm-metrics` ConfigMap and points
dcgm-exporter at it through ClusterPolicy; KubeNeuron never reads, writes
or mounts it.

- **No KubeNeuron component gains a verb.** The profile is applied by the
  operator's `helm upgrade`, by a human. The KubeNeuron operator, controller
  and agent ClusterRoles are byte-identical to v0.6.1; the v0.7 commits touch
  no file under `config/rbac` or the Helm chart's RBAC templates.
- **Content is a list of DCGM field names.** The CSV grammar dcgm-exporter
  accepts is `FIELD, type, help`; no field in it is executable, no path, no
  URL. A unit test in `internal/gpucatalog` parses the block with the same
  grammar and pins every row to a catalog field with a canonical mapping in
  both directions, so a row cannot appear in the profile that the catalog
  does not know, and `DCGM_FI_DEV_XID_ERRORS` (230),
  `DCGM_FI_DEV_GPU_RECOVERY_ACTION` (1523) and the profiling fields are
  pinned absent. The `helm template` of the pinned chart with these values
  is also rendered in a test and the ConfigMap checked.
- **What widens is what the exporter publishes on its own `/metrics`**, in
  the `gpu-operator` namespace, to whatever can already reach that Service.
  The added series are device telemetry (clocks, temperatures, power, ECC
  counters, PCIe and NVLink counters, framebuffer use); none carries a
  workload secret, and the `pod`/`namespace`/`container` labels the exporter
  stamps were already present in v0.6.1 under the exporter's Kubernetes
  mapping. The profile lists no DCGM profiling field except the two PCIe
  byte-rate fields (1009/1010), which the catalog places in tier T0; whether
  those two require the exporter to enable DCGM's profiling module on the
  pinned version is not established by this project and is noted as such.
- **It is reversible without KubeNeuron.** The documented rollback is the
  nine-series v0.6.1 list under the same `helm upgrade` plus an exporter
  DaemonSet restart; `internal/depprofile` pins that the README's rollback
  snippet is the v0.6.1 list exactly. The agent's own GPU health path
  (`dcgmi dmon -e 230`, `nvidia-smi -q`) does not read the exporter and is
  unaffected in both directions.
- **Residual risk, stated.** Whether the pinned dcgm-exporter starts cleanly
  with field ids the GPU does not implement is an assumption the hardware
  gate is designed to check, not something this review can establish. An
  exporter that failed to start would make `GpuExporterDown` fire for the
  node, which is a controller-facing rule mapped to a driver-hang class; the
  rollback above is the remedy, and that path is unchanged from v0.6.1.

## 2. vmagent reads node metadata, read-only, on RBAC KubeNeuron does not own

The `dcgm-exporter` scrape job in
`deploy/kubernetes/dependencies/observability/vmagent.yaml` sets
`attach_metadata: {node: true}` so a `node_pool` label can be derived from
a node label, and copies `UUID`, `modelName` and `GPU_I_PROFILE` into
`gpu_uuid`, `gpu_model` and `mig_profile`.

- **Read-only, and on the VictoriaMetrics operator's own generated role.**
  Attaching node metadata makes vmagent list and watch Node objects. The
  profile ships no RBAC for that: the VictoriaMetrics operator generates,
  for a cluster-wide VMAgent, a ClusterRole with `get`, `list`, `watch` on
  core `nodes` (and the other discovery resources) bound to the
  ServiceAccount it creates, and no write verb. This was verified in the
  pinned operator version's source rather than assumed, and
  `internal/depprofile/rbac_test.go` fails if the operator pin moves without
  that statement being re-verified. `internal/depprofile/scrape_test.go`
  pins the two VMAgent settings that would silently lose the generated role
  (a custom `serviceAccountName`, `ingestOnlyMode`). The README names the one
  configuration (a namespace-restricted operator) where an administrator
  must add a minimal read-only ClusterRole by hand, and says to grant
  nothing beyond `get/list/watch` on `nodes`.
- **No label is invented.** Every relabel rule matches only a non-empty
  value, so a node without any of the four pool labels gets no `node_pool`
  label, never an empty string or a placeholder; `internal/depprofile`
  evaluates the rules against synthetic targets to pin that. `cluster` is
  not set by default and is only ever an administrator-provided external
  label on the VMAgent resource; `driver` is deliberately not produced,
  because neither candidate source has been verified and a wrong value would
  create false peers for the fail-slow score. A test pins that no rule
  produces `cluster` or `driver`.
- **Bounded cardinality.** The new labels are copies of labels the exporter
  already stamps (`UUID`, `modelName`) or one value per node (`node_pool`),
  so the series count does not grow; the originals are kept for existing
  rules. No KubeNeuron series gains a label.
- **Nothing in KubeNeuron consumes these labels.** The controller's
  Alertmanager webhook path and `alertClassMap` are unchanged; the labels
  exist for recording rules and dashboards only.

## 3. Shadow alerts go to humans, and the controller has no authority over them

`configs/vmalert/gpu-shadow-rules.yaml` (deployed as the
`kubeneuron-gpu-shadow` VMRule) carries seven `ShadowGpu*` alerts over the
canonical and derived `kubeneuron_gpu_*` series. They are observation for
people. The property under review is that no shadow alert can become an
incident, and it holds at three independent levels, each pinned by a test
so that removing any one of them fails the build:

1. **Names** (`internal/detect/shadow_isolation_test.go`). Every alert is
   `ShadowGpu*`, never `Gpu*`. The controller webhook route in
   `observability/vmalertmanager.yaml` matches
   `alertname =~ "Gpu.*|KubeNeuronAgentDown"` and cannot match a shadow
   name.
2. **Routing** (`internal/depprofile/shadow_routing_test.go`). Every rule
   carries `kubeneuron_shadow="true"`, and the Alertmanager config has an
   explicit route for that matcher to the `human-default` receiver placed
   before the webhook route, with `continue: false`. The test evaluates the
   rendered route tree: a shadow label set resolves to `human-default`, and
   a `GpuDoubleBitEccVolatile` label set still resolves to the `kubeneuron`
   webhook receiver, unchanged. The same package lints every label name in
   the rules and matchers against the Prometheus grammar
   (`internal/metrics` does the same for the dashboard expressions) and
   forbids the Kubernetes-style dotted-and-slashed spelling of the shadow
   label, and its underscore-sanitised form, under `configs/`, `deploy/`
   and `docs/`, so a route that silently never matched cannot be
   introduced.
3. **Code** (`internal/detect/shadow_isolation_test.go`). No `ShadowGpu*`
   name is in `alertClassMap`; `SignalFromAlert` returns `ok=false` for
   every shadow alert name; and `alertClassMap` itself is pinned to its
   pre-v0.7 golden list. A shadow alert that somehow reached the webhook is
   therefore dropped as unknown, by code that v0.7 did not change.

Further properties the review confirmed from the files:

- **No new controller authority and no new controller code.** The v0.7
  commits add no non-test file under `internal/controller`,
  `internal/decision`, `internal/autonomy`, `internal/detect` or
  `internal/agent`; the only `internal/detect` change is the isolation test.
  The controller exports no shadow metric, keeps no shadow state and has no
  shadow log; the dashboards read shadow state from vmalert's own
  `ALERTS{kubeneuron_shadow="true"}` series. Disabling the layer is deleting
  the VMRule.
- **Shadow rules cannot act on missing data.** They read only recorded
  series, never a dcgm-exporter series directly, and use no `or vector(0)`,
  `default` or `absent()`; a test pins all three. A GPU that stops reporting
  produces no shadow alert, and the existing `GpuExporterDown` behaviour is
  neither changed nor extended.
- **The human receiver is a placeholder.** `human-default` ships without a
  configuration, as in v0.6.1; an empty receiver drops notifications. That
  is a completeness gap for operators, not a security one: a dropped shadow
  notification causes nothing to happen, which is also what a delivered one
  causes.
- **Severity.** Every shadow rule carries `severity: info`, so a pager
  routed on `severity=critical` will not page on a shadow alert.

## Limitations

- This is a review of files and tests, by the project, not an audit by a
  third party, and it has run nothing against hardware, a live
  dcgm-exporter, a live VictoriaMetrics operator or a live Alertmanager.
  The routing test parses the shipped config and walks the route tree in
  Go; it is not `amtool config routes test` against a running Alertmanager.
  `amtool check-config` on the deployed Alertmanager config and
  `vmalert -dryRun` on the recording and shadow rule files both pass on
  this tree, which establishes that the files parse and are syntactically
  valid, and nothing about delivery, routing in a live Alertmanager or
  behaviour on hardware.
- The read-only `nodes` grant was verified by reading the pinned operator's
  source, not by inspecting a generated ClusterRole in a cluster.
- The exporter's behaviour with field ids the GPU does not implement is
  unverified, and so is the claim that the added series carry nothing
  sensitive on hardware this project has not seen; both are limits of a
  source review.
- Nothing here changes the conclusions of the earlier reviews; the
  authority-bearing surfaces of the product are as they describe.
