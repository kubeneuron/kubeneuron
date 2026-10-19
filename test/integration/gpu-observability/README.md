# kind GPU observability fixture

Manifests for `hack/kind-gpu-observability.sh` (`make
test-gpu-observability-kind`; offline half: `make
verify-gpu-observability-fixture`). The harness creates its own disposable
kind cluster (`kubeneuron-gpu-observability`, one control plane and two
workers, the digest-pinned `kindest/node:v1.33.12`), refuses to touch a
cluster of that name that already exists, deletes it on exit unless
`KEEP_CLUSTER=1`, and asserts everything through the VictoriaMetrics,
Alertmanager, recorder-sink and Grafana HTTP APIs: every GET by way of the
API server's service proxy, and the few POSTs (probe alerts, Grafana panel
queries) as `kubectl exec -i` of the fixture's own recorder binary in client
mode (`alertsink -post`, body on stdin) inside the human sink pod, because
that proxy rewrites the request Content-Type and the two servers reject it.
No port-forward, every call bounded.

## What runs

| Piece | Source | Notes |
| --- | --- | --- |
| Exporter stand-in | `hack/dcgmfixture` (Go, stdlib only) | Built on the host, packaged `FROM scratch` with `Dockerfile` here, loaded into kind. Serves `DCGM_FI_*` text exposition with the real dcgm-exporter label shape (`gpu`, `UUID`, `pci_bus_id`, `device`, `modelName`, `Hostname`, `DCGM_FI_DRIVER_VERSION`, `pod`, `namespace`, `container`). |
| Discovery target | `fixture.yaml.tmpl` | Service `nvidia-dcgm-exporter` in namespace `gpu-operator`, port `gpu-metrics`: exactly what the shipped relabeling keeps. One Deployment per worker so the same job sees a node with and without a node-pool label. |
| vmagent | `vmagent.yaml` + `extract-scrape-config.py` | Scrape config is `spec.inlineScrapeConfig` and `spec.scrapeInterval` of the checked-in `deploy/kubernetes/dependencies/observability/vmagent.yaml`, extracted verbatim; nothing is added or removed. The RBAC is the read-only discovery access (nodes for `attach_metadata`) the VictoriaMetrics operator would generate. |
| vmsingle | `vmsingle.yaml` | emptyDir store, 1d retention. |
| vmalert | `vmalert.yaml` | Loads `configs/vmalert/gpu-recording-rules.yaml` and `configs/vmalert/gpu-shadow-rules.yaml` byte-for-byte from a ConfigMap, writes recorded series and `ALERTS` state back into vmsingle, notifies the fixture Alertmanager. The controller-facing rule files are deliberately not loaded. |
| Alertmanager | `alertmanager.yaml.tmpl` + `extract-alertmanager-config.py` | Config is `spec.configRawYaml` of the checked-in `observability/vmalertmanager.yaml`: `route`, `inhibit_rules` and the `kubeneuron` receiver (URL, bearer `credentials_file`) verbatim. The one test-only difference: the shipped `human-default` receiver has no delivery integration (it is the placeholder a real pager replaces), so the extractor gives it exactly one `webhook_configs` entry pointing at the human recorder sink, and refuses to run if that receiver ever gains an integration of its own. The bearer-token Secret is mounted where the shipped `credentials_file` expects it, with a per-run token. |
| Recorder sinks | `hack/alertsink` (Go, stdlib only) + `alert-sinks.yaml.tmpl` | Two copies of one program that stores every request in memory, answers 200 and reports them at `GET /requests`; it acts on nothing. `alert-sink-human` is the human-default delivery target. The controller sink is deployed under the shipped receiver's own Service name, namespace and port (`kubeneuron-controller.kube-neuron.svc:8080`, read from the config at render time), so the receiver is used exactly as checked in and a mis-routed alert would be counted here. |
| Grafana | `grafana.yaml`, `grafana-datasources.yaml`, `grafana-dashboards-provider.yaml` | `grafana/grafana` pinned by tag and digest in `versions.lock.yaml` (`images.grafana`, fixture-only). A file-provisioned Prometheus-API datasource (`uid: kubeneuron-vmsingle`) on vmsingle and a file provider over the five `deploy/grafana` boards. Anonymous Admin, no outbound calls (update checks, plugin preinstall, analytics and the news feed are off); no credential of any kind. |

All VictoriaMetrics images use the `v1.147.0` tag, Alertmanager the
`v0.32.1` tag and Grafana the `12.4.13` tag plus digest pinned in
`deploy/kubernetes/dependencies/versions.lock.yaml`; the offline verifier
fails if a manifest drifts from the lock or names a mutable tag.

## Device set

Node A (labelled `kubeneuron.io/node-pool=fixture-fallback-pool` and
`eks.amazonaws.com/nodegroup=fixture-eks-pool`): seven `Tesla T4` (gpu 0–6,
gpu 6 slow, gpu 2 reporting `DCGM_FI_DEV_MEMORY_UNREPAIRABLE_FLAG` = 1) and one
slow `NVIDIA A10G` (gpu 7). Node B (no node-pool label): one healthy
`NVIDIA A10G`. A slow device runs SM at 1000 MHz against a 1590 MHz maximum,
reports clock-event bits 8|32, and its power-violation counter grows at
0.12 s/s. Every other value is a constant or a counter the exporter grows
linearly with run time. The spec string (`Tesla T4:7:6:2;NVIDIA A10G:1:0`)
is pinned in the harness, in `fixture.yaml.tmpl` and in
`hack/dcgmfixture/main_test.go`; the verifier fails if they drift.

Deliberately omitted so the harness can prove absent-not-zero: field 153
(`DCGM_FI_DEV_GPU_TEMP_LIMIT`), 393–396 (remapped rows, row-remap failure and
pending) and every NVLink counter (409+). `hack/dcgmfixture/main_test.go`
checks that every `DCGM_FI_*` name the recording rules read is either emitted
or on that omission list, so an added rule source cannot go untested by
accident.

## Shadow routing

The unrepairable device is the input for the shipped
`ShadowGpuMemoryBeyondRepair` rule (`kubeneuron_gpu_memory_hard_fault_active{component=~"row_remap_failed|unrepairable"} == 1`,
`for: 5m`, the shortest hold in the shadow file; the harness reads the hold
from the file and fails if that rule stops being the shortest). The rule is
not edited. The harness waits for vmalert to record it as firing, then
asserts, in this order:

1. the human sink received a delivery whose `receiver` is `human-default`,
   whose alert carries `kubeneuron_shadow="true"`, the unrepairable device's
   `gpu_uuid`, `component="unrepairable"` and a `Shadow:` summary;
2. Alertmanager's own API (`/api/v2/alerts`) lists exactly one active alert of
   that name with receivers `[human-default]`;
3. the controller sink has recorded **exactly zero** requests, and
   Alertmanager's `alertmanager_notifications_failed_total` is 0 (nothing was
   attempted and lost);
4. two API-injected probes (never rules; alertname `GpuFixtureRoutingProbe`,
   Gpu-prefixed on purpose because the controller route matches
   `alertname =~ "Gpu.*"`): one with the shadow label reaches the human sink
   while the controller sink stays at zero; one without it reaches the
   controller sink with the shipped webhook path and `Bearer <token>` from the
   shipped `credentials_file`, and never the human sink. That is what makes
   the zero in step 3 a routing result rather than a dead receiver.

The fail-slow score also satisfies `ShadowGpuFailSlow`; its 30m hold is
longer than the run, so the harness asserts it is pending and never firing.
No sink does anything with a delivery; the controller is not involved.

## Dashboards

The five boards are exported with `${DS_PROMETHEUS}` as every datasource uid
(the form the Grafana import dialog binds to a datasource of your choice).
The harness performs that binding structurally (every string value equal to
the placeholder becomes `kubeneuron-vmsingle`, nothing else changes; the
verifier reverts the substitution and requires the checked-in document back),
puts the result into the file provider, and asserts through the Grafana API:
health and version; the datasource exists and its health check against
vmsingle passes; each board is listed, provisioned (read-only), with the
checked-in title, panel count and seven variables, and every datasource
reference resolves to the provisioned uid; every variable query
(`label_values(metric, label)`, issued the way Grafana issues it, through the
datasource proxy) returns `success`; and every panel target (48 expressions,
each variable replaced by its `All` value `.*`) executes through
`/api/ds/query` without a query error.

That last check is a syntax and wiring check on synthetic data: an empty
result is fine (several boards read controller series this fixture never
produces), a query error is not. It says nothing about panel semantics,
values or what the boards look like.

## What a green run proves, and what it does not

It proves, with synthetic series on CPU-only kind nodes and the pinned
images, that: the shipped scrape contract and recording rules wire source
series into the canonical and derived series the catalog promises (unit
factors, identity labels, `node_pool`/`cluster` presence, bitmask decode,
the thermal-headroom fallback, absent-never-0 for omitted sources, one
fail-slow score for the one deviated device of seven and none for a
two-device group); that one shipped `ShadowGpu*` rule fires on a synthetic
input and the shipped Alertmanager route tree delivers it to `human-default`
and never to the controller webhook; and that Grafana provisions the five
boards against a vmsingle datasource with every variable and panel query
executing.

It proves nothing about DCGM, dcgm-exporter, NVIDIA drivers, real GPU
faults, MIG, a real pager integration, the KubeNeuron controller (which
never runs here) or any hardware; the exporter is a Go program printing
numbers and the receivers are recorders. These are rows of the v0.7 G2 gate,
not G2. Not covered here: the `hack/hw-e2e.sh` `test-observability` phase
and anything on real GPUs.
