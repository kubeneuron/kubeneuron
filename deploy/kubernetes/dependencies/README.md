# KubeNeuron external dependency profile

This directory is a version-pinned reference profile for the upstream
components that supply GPU telemetry, metric storage, rule evaluation, and
alert delivery to KubeNeuron. It is a convenience, not a dependency: bring
your own Prometheus/Alertmanager stack if you have one — KubeNeuron only
needs DCGM metrics scraped and an Alertmanager that can reach the webhook.

## Ownership boundary

These resources are not owned by the KubeNeuron reconciler:

- NVIDIA GPU Operator owns GPU drivers and its node operands, including
  dcgm-exporter.
- VictoriaMetrics Operator owns VMSingle, VMAgent, VMAlert, VMAlertmanager,
  their workloads, and their storage objects.
- A cluster administrator owns installation, platform-specific settings,
  backup, upgrades, and removal of both upstream operators.

The KubeNeuron API reserves `Managed` for future upstream-resource discovery,
but the operator rejects it because that discovery/readiness path does
not exist. Even though dedicated upstream operators own this profile, declare
its endpoints to KubeNeuron as `External`. Deleting a `KubeNeuron` custom
resource does not delete anything in this directory.

Do not install this profile together with `deploy/kubernetes/base`. The older
static stack contains duplicate dcgm-exporter, VictoriaMetrics, vmalert, and
Alertmanager workloads with different versions and service names.

## Locked versions

The machine-readable source of truth is [versions.lock.yaml](versions.lock.yaml).
The lock was verified against the official artifacts on 2026-07-11.

| Component | Pin | Installation source |
|---|---:|---|
| NVIDIA GPU Operator chart and operator/validator | `v26.3.3` | NVIDIA NGC Helm repository |
| Node Feature Discovery chart/image | `0.18.3` / `v0.18.3` | Bundled GPU Operator dependency |
| NVIDIA driver | `580.126.20` | GPU Operator chart value; runtime image tag includes the detected node OS suffix |
| NVIDIA driver manager | `v0.11.0` | GPU Operator chart value |
| NVIDIA Container Toolkit | `v1.19.1` | GPU Operator chart value |
| NVIDIA Device Plugin / GPU Feature Discovery | `v0.19.3` | GPU Operator chart value |
| NVIDIA dcgm-exporter | `4.5.3-4.8.2-distroless` | GPU Operator chart value |
| NVIDIA MIG Manager | `v0.14.2` | GPU Operator chart value |
| VictoriaMetrics Operator | `v0.73.1` | Upstream no-webhook release manifest |
| VictoriaMetrics, vmagent, and vmalert | `v1.147.0` | Explicit custom-resource image tags |
| Alertmanager | `v0.32.1` | Explicit custom-resource image tag |

The locked GPU Operator chart SHA-256 is
`59abb5852a24b3ae0ef757bfea3051f419acbf559ee5efd72f0672d28af56a68`.
The locked VictoriaMetrics Operator manifest SHA-256 is
`28f0ab848df7a00f9d46685cd2864024a0d998df4840bb9a1fce471dd4163628`.

The chart and release manifest are checksum-locked. Container images are
version-tagged, not digest-pinned; mirror and digest-pin them before using this
profile in a supply-chain environment that requires immutable images.

The lock also carries an `images.grafana` entry (tag plus digest). The profile
installs no Grafana; that pin is consumed only by the kind GPU observability
fixture (`test/integration/gpu-observability/`), which provisions the
checked-in `deploy/grafana` boards into that exact image.

## What this profile installs

```text
NVIDIA GPU Operator -> dcgm-exporter Service
                              |
                              v
                         VMAgent
                              |
                              v
                         VMSingle
                              |
                              v
VMRule -----------------> VMAlert -> VMAlertmanager
                                      |              \
                                      v               v
                         KubeNeuron controller    human-default receiver
                         webhook (Gpu* alerts)    (ShadowGpu* and
                                                   KubeNeuron* alerts)
```

Three VMRules are deployed from `observability/rules.yaml`: `kubeneuron-gpu`
(the controller-facing `Gpu*` alerts and KubeNeuron self-monitoring),
`kubeneuron-gpu-recording` (the v0.7 canonical and derived `kubeneuron_gpu_*`
recording rules, no alerts) and `kubeneuron-gpu-shadow` (the v0.7
`ShadowGpu*` alerts, which route to humans only; see
[Shadow-only alert routing](#shadow-only-alert-routing)).

The generated in-cluster integration endpoints are:

| Dependency | Endpoint |
|---|---|
| VictoriaMetrics | `http://vmsingle-kubeneuron.kubeneuron-monitoring.svc:8429` |
| Alertmanager | `http://vmalertmanager-kubeneuron.kubeneuron-monitoring.svc:9093` |
| Controller webhook target | `http://kubeneuron-controller.kube-neuron.svc:8080/api/v1/webhooks/alertmanager` |

For a KubeNeuron custom resource that uses this profile, declare each
observability dependency as an external endpoint and use the first two
addresses:

```yaml
observability:
  victoriaMetrics:
    mode: External
    endpoint: http://vmsingle-kubeneuron.kubeneuron-monitoring.svc:8429
  alertmanager:
    mode: External
    endpoint: http://vmalertmanager-kubeneuron.kubeneuron-monitoring.svc:9093
```

The repository sample uses these same profile endpoints.

## Prerequisites and decisions

Before installation, confirm all of the following:

- The operator has cluster-administrator access and has `kubectl`, Helm 3,
  `curl`, `sha256sum`, and `jq`.
- The Kubernetes and host OS/container-runtime combination appears in the
  [NVIDIA GPU Operator 26.3 platform support matrix](https://docs.nvidia.com/datacenter/cloud-native/gpu-operator/26.3/platform-support.html).
- GPU nodes have compatible kernels and homogeneous supported operating-system
  versions, or use a reviewed pre-installed driver configuration.
- No incompatible cluster-wide Node Feature Discovery installation already
  exists. If one does, set `nfd.enabled: false` in
  [gpu-operator/values.yaml](gpu-operator/values.yaml).
- Decide whether GPU Operator installs the driver. This profile explicitly
  enables driver `580.126.20`, the toolkit, device plugin, and bounded automatic
  driver rollout. Set `driver.enabled: false` only after validating the
  pre-installed-driver path in NVIDIA's documentation.
- A `ReadWriteOnce` StorageClass can bind the VMSingle `100Gi` request. Set an
  explicit `storageClassName` in `observability/vmsingle.yaml` if the cluster
  has no suitable default, and decide its PV reclaim policy before continuing.
- The cluster can pull from `nvcr.io`, `registry.k8s.io`, and the image
  registries listed in the lock.
- This is a dedicated VictoriaMetrics Operator installation. If the cluster
  already has a compatible shared operator, reuse it deliberately and do not
  later remove its release or CRDs.

The GPU namespace enforces the privileged Pod Security level because NVIDIA's
node operands require host access. Review that exception under the cluster's
security policy.

## Install in dependency order

Run commands from the repository root. Downloading and verifying artifacts
before applying them prevents a mutable remote response from bypassing the
lock.

```sh
PROFILE=deploy/kubernetes/dependencies
ARTIFACT_DIR="$(mktemp -d)"

GPU_CHART="$ARTIFACT_DIR/gpu-operator-v26.3.3.tgz"
helm pull gpu-operator \
  --repo https://helm.ngc.nvidia.com/nvidia \
  --version v26.3.3 \
  --destination "$ARTIFACT_DIR"
printf '%s  %s\n' \
  59abb5852a24b3ae0ef757bfea3051f419acbf559ee5efd72f0672d28af56a68 \
  "$GPU_CHART" | sha256sum -c -

VM_OPERATOR_MANIFEST="$ARTIFACT_DIR/vm-operator-v0.73.1-no-webhook.yaml"
curl -fsSL \
  https://github.com/VictoriaMetrics/operator/releases/download/v0.73.1/install-no-webhook.yaml \
  -o "$VM_OPERATOR_MANIFEST"
printf '%s  %s\n' \
  28f0ab848df7a00f9d46685cd2864024a0d998df4840bb9a1fce471dd4163628 \
  "$VM_OPERATOR_MANIFEST" | sha256sum -c -
```

### 1. NVIDIA GPU Operator

Review `gpu-operator/values.yaml` against the target platform before running
the Helm command. Keep the deterministic release name `gpu-operator`; the
upgrade and removal procedures depend on it.

```sh
kubectl apply -k "$PROFILE/gpu-operator"
helm upgrade --install gpu-operator "$GPU_CHART" \
  --namespace gpu-operator \
  --values "$PROFILE/gpu-operator/values.yaml" \
  --wait \
  --timeout 30m

kubectl wait --for=jsonpath='{.status.state}'=ready \
  clusterpolicy/cluster-policy --timeout=30m
kubectl rollout status daemonset/nvidia-dcgm-exporter \
  --namespace gpu-operator --timeout=15m
kubectl get endpoints nvidia-dcgm-exporter --namespace gpu-operator
kubectl get nodes -o json | jq \
  '.items[] | select(.status.allocatable["nvidia.com/gpu"] != null) |
   {name: .metadata.name, gpus: .status.allocatable["nvidia.com/gpu"]}'
```

Do not continue until `cluster-policy` is ready, every intended GPU node
reports the expected allocatable GPU count, and the `nvidia-dcgm-exporter`
Service has endpoints.

### 2. VictoriaMetrics Operator

The no-webhook artifact intentionally avoids a cert-manager dependency. Its
CRDs are larger than the client-side-apply annotation limit, so use
server-side apply.

```sh
kubectl apply --server-side \
  --field-manager=kubeneuron-dependencies \
  -f "$VM_OPERATOR_MANIFEST"

kubectl wait --for=condition=Established --timeout=5m \
  crd/vmsingles.operator.victoriametrics.com \
  crd/vmagents.operator.victoriametrics.com \
  crd/vmalertmanagers.operator.victoriametrics.com \
  crd/vmalerts.operator.victoriametrics.com \
  crd/vmrules.operator.victoriametrics.com
kubectl rollout status deployment/vm-operator \
  --namespace vm --timeout=10m
```

The remote Kustomize entry point in `victoria-metrics-operator/` exists for
rendering and comparison. The checksum-verified local artifact above is the
authoritative installation path.

### 3. Observability resources

The aggregate `observability/kustomization.yaml` is useful for rendering and
later idempotent convergence. Initial installation is deliberately staged so
each dependency has a readiness gate.

```sh
kubectl apply -f "$PROFILE/observability/namespace.yaml"

kubectl apply -f "$PROFILE/observability/vmsingle.yaml"
kubectl wait --for=jsonpath='{.status.phase}'=Bound --timeout=10m \
  pvc/vmsingle-kubeneuron --namespace kubeneuron-monitoring
kubectl rollout status deployment/vmsingle-kubeneuron \
  --namespace kubeneuron-monitoring --timeout=10m

kubectl apply -f "$PROFILE/observability/vmagent.yaml"
kubectl rollout status deployment/vmagent-kubeneuron \
  --namespace kubeneuron-monitoring --timeout=10m

kubectl apply -f "$PROFILE/observability/vmalertmanager.yaml"
kubectl rollout status statefulset/vmalertmanager-kubeneuron \
  --namespace kubeneuron-monitoring --timeout=10m

kubectl apply -f "$PROFILE/observability/vmalert.yaml"
kubectl rollout status deployment/vmalert-kubeneuron \
  --namespace kubeneuron-monitoring --timeout=10m

kubectl apply -f "$PROFILE/observability/rules.yaml"
kubectl get vmsingle,vmagent,vmalertmanager,vmalert,vmrule \
  --namespace kubeneuron-monitoring
```

In a separate terminal, port-forward `service/vmagent-kubeneuron` on port
`8429` and inspect `/targets`. Then port-forward
`service/vmsingle-kubeneuron` on port `8429` and query
`DCGM_FI_DEV_GPU_TEMP`. Do not treat the profile as healthy until every
intended dcgm-exporter endpoint is up and a DCGM series is queryable.

### 4. KubeNeuron

Install KubeNeuron CRDs/operator separately only after the dependencies are
healthy. Apply a reviewed KubeNeuron resource with the endpoints above last.
The Alertmanager receiver can exist before the controller and will retry, but
that temporary delivery failure is not dependency readiness.

## dcgm-exporter metrics profile (reliability profile)

[gpu-operator/values.yaml](gpu-operator/values.yaml) creates a
`kubeneuron-dcgm-metrics` ConfigMap through the chart-supported
`dcgmExporter.config` fields, and ClusterPolicy points dcgm-exporter at it.
Since v0.7 the list is the **reliability profile**: exactly the tier T0
(baseline) and tier T1 (reliability) fields of the
[GPU metric catalog](../../../docs/reference-gpu-metrics.md) that carry a
canonical `kubeneuron_gpu_*` mapping and are not deferred, each under the
series name the pinned exporter uses for its field id. A unit test in
`internal/gpucatalog` pins the list to the catalog in both directions, so a
field cannot be added to one without the other.

**What this changes, and what it does not.** The profile adds observability
only: it tells the exporter to emit more series. It adds no detector
threshold and no playbook, and it starts no remediation. The
controller-facing `Gpu*` rules in the `kubeneuron-gpu` VMRule still read
only the nine series the v0.6.1 profile listed; every series the reliability
profile adds is read by the recording rules only (the
`kubeneuron-gpu-recording` VMRule, which produces the canonical and derived
`kubeneuron_gpu_*` series), and the only alerts over those are the
shadow-only `ShadowGpu*` rules, which route to humans and open no incident.
The agent's own GPU health path (`dcgmi dmon` on field 230 and `nvidia-smi`)
is unchanged and independent of this profile.

**Unvalidated by construction.** Only the nine v0.6.1 series were ever
scraped by a recorded run of this project (Tesla T4, v0.6.0-era). Every other
row is listed from NVIDIA documentation and has not been observed on any GPU
by this project. Several are documented as absent on the Tesla T4 (NVLink
counters, row remapping), unknown for it (memory temperature and its limit,
the unrepairable-memory flag) or unvalidated anywhere (the sync-boost,
board-limit and reliability throttle counters 242/243/245, the DCGM 4.x
field 1507). The exporter is expected to skip a field the GPU does not report
rather than fail to start; that expectation is an assumption the hardware
gate checks, not something this profile asserts. Deliberately absent:
`DCGM_FI_DEV_XID_ERRORS` (230, a sticky last-XID gauge with no canonical
series), `DCGM_FI_DEV_GPU_RECOVERY_ACTION` (1523, cataloged only),
`DCGM_FI_DEV_PSTATE` (190), every profiling field except the PCIe byte rates
(`DCGM_FI_PROF_*`, tier T2, including the NVLink byte rates 1011/1012) and
every vGPU field.

### Rollout

The profile is the default of this directory: the `helm upgrade --install`
in step 1 applies it. On an existing installation, re-run the same command
with the updated values and let the exporter pick the ConfigMap up:

```sh
helm upgrade gpu-operator "$GPU_CHART" \
  --namespace gpu-operator \
  --values "$PROFILE/gpu-operator/values.yaml" \
  --wait \
  --timeout 30m
kubectl rollout restart daemonset/nvidia-dcgm-exporter --namespace gpu-operator
kubectl rollout status daemonset/nvidia-dcgm-exporter \
  --namespace gpu-operator --timeout=15m
```

GPU Operator updates the ConfigMap in place; dcgm-exporter reads its metrics
list at start, so the restart is what makes the new list take effect. Verify
with a series the v0.6.1 profile did not have, for example
`DCGM_FI_DEV_ECC_SBE_VOL_TOTAL` or `DCGM_FI_DEV_POWER_USAGE`, on the
exporter's `/metrics` or in VMSingle. A series documented as absent for the
GPU model (for example any `DCGM_FI_DEV_NVLINK_*` on a Tesla T4) is expected
to be missing, not zero.

### Rollback

Rolling back is the same command with the v0.6.1 list. Replace
`dcgmExporter.config.data` in `gpu-operator/values.yaml` with the nine-series
list below (byte-for-byte the v0.6.1 content; `git show v0.6.1:deploy/kubernetes/dependencies/gpu-operator/values.yaml`
has the whole file), then run the rollout commands above:

```yaml
    data: |-
      # Format: DCGM FIELD, Prometheus metric type, help message
      DCGM_FI_DEV_ECC_DBE_VOL_TOTAL, counter, Total volatile double-bit ECC errors.
      DCGM_FI_DEV_ECC_DBE_AGG_TOTAL, counter, Total aggregate double-bit ECC errors.
      DCGM_FI_DEV_ROW_REMAP_PENDING, gauge, Whether a row remap is pending.
      DCGM_FI_DEV_ROW_REMAP_FAILURE, gauge, Whether row remapping has failed.
      DCGM_FI_DEV_UNCORRECTABLE_REMAPPED_ROWS, counter, Uncorrectable remapped rows.
      DCGM_FI_DEV_NVLINK_CRC_FLIT_ERROR_COUNT_TOTAL, counter, Total NVLink flow-control CRC errors.
      DCGM_FI_DEV_PCIE_REPLAY_COUNTER, counter, Total PCIe retries.
      DCGM_FI_DEV_CLOCK_THROTTLE_REASONS, gauge, Active clock-throttle reason bitmask.
      DCGM_FI_DEV_GPU_TEMP, gauge, GPU temperature in degrees Celsius.
```

Every controller-facing alert rule reads one of these nine, so the
incident-opening rules keep their inputs. What the rollback removes is the
input of the v0.7 layer built on the added series: the canonical and derived
`kubeneuron_gpu_*` series they fed stop being recorded (no rule synthesises
a zero, so they go absent), the v0.7 dashboards show No data for those
panels, and the shadow alerts over them fall silent. Nothing breaks, and the
forward rollout restores all of it. To take the v0.7 rules out as well,
`kubectl delete vmrule kubeneuron-gpu-recording kubeneuron-gpu-shadow
--namespace kubeneuron-monitoring`; the controller-facing `kubeneuron-gpu`
VMRule is a separate object and keeps working. No controller, agent or
store state is involved in either step.

## Scrape label contract (dcgm-exporter series)

[observability/vmagent.yaml](observability/vmagent.yaml) declares, in its
`dcgm-exporter` job, which labels a dcgm-exporter series carries and where
each comes from. Everything in it is observability only: it copies or
derives labels that already exist, invents none, and starts nothing. **No
shipped rule or dashboard may require an optional label** (`node_pool`,
`mig_profile`, `cluster`, `pod`/`namespace`/`container`): a query over those
must work when the label is absent. A unit test in `internal/depprofile`
parses the inline scrape config as relabel rules and evaluates them, so the
table below is pinned, not described.

| Label | Source | When the source is missing |
|---|---|---|
| `node` | relabel of the pod's node name (`__meta_kubernetes_pod_node_name`) | always present for pod-backed endpoints |
| `gpu_uuid` | metric relabel copying the exporter's `UUID` label; `UUID` is kept | the exporter always sets it |
| `gpu_model` | metric relabel copying the exporter's `modelName` label; `modelName` is kept | the exporter always sets it |
| `gpu`, `pci_bus_id`, `device`, `Hostname` | exporter | as emitted |
| `pod`, `namespace`, `container` | exporter's Kubernetes mapping | empty while no workload holds the GPU; optional |
| `node_pool` | node label via `attach_metadata: {node: true}`, lowest precedence first: `kubeneuron.io/node-pool` (manual fallback), `karpenter.sh/nodepool`, `cloud.google.com/gke-nodepool`, `eks.amazonaws.com/nodegroup` (wins when several exist); each rule matches only a non-empty value (`regex: (.+)`) | **absent**, never an empty string or `unknown`; optional |
| `mig_profile` | metric relabel copying the exporter's `GPU_I_PROFILE`, which exists only with MIG enabled | absent without MIG (always absent on a Tesla T4); optional and unvalidated |
| `cluster` | **administrator-provided**: `spec.externalLabels.cluster` on the VMAgent resource (VictoriaMetrics operator field). Not set by default and not derivable from in-cluster metadata | absent; optional |
| `driver` | **intentionally absent** (see below) | absent |

### Setting `cluster`

No in-cluster object names the cluster, so this profile ships no default and
no placeholder. If more than one cluster writes into one store, stamp the
name on every series vmagent writes by setting `externalLabels` on the
VMAgent resource. Either add it to a copy of `observability/vmagent.yaml`:

```yaml
spec:
  externalLabels:
    cluster: prod-eu-west-1
```

or patch the installed resource in place (the operator rolls the vmagent
Deployment):

```sh
kubectl patch vmagent kubeneuron --namespace kubeneuron-monitoring \
  --type merge \
  -p '{"spec": {"externalLabels": {"cluster": "prod-eu-west-1"}}}'
kubectl rollout status deployment/vmagent-kubeneuron \
  --namespace kubeneuron-monitoring --timeout=10m
```

To remove it, patch `{"spec": {"externalLabels": null}}`. The label is
present exactly when you set it; nothing shipped assumes it exists.

### Driver label decision (v0.7 M2)

`driver` is **not** produced. Two candidate sources exist, and neither has
been verified end to end by this project, so neither is wired in:

- GPU Feature Discovery (`gfd.enabled: true` in the values, image `v0.19.3`)
  labels nodes with `nvidia.com/cuda.driver-version.full` (and the deprecated
  `nvidia.com/cuda.driver.major`/`.minor`/`.rev`). With node metadata
  attached, a relabel of
  `__meta_kubernetes_node_label_nvidia_com_cuda_driver_version_full` would
  carry it. The label name is documented upstream; that the pinned GFD sets
  it on the nodes this profile runs on has not been observed by any run of
  this project.
- dcgm-exporter can stamp `DCGM_FI_DRIVER_VERSION` as a label on every
  series when the metrics CSV lists it with type `label` (its stock
  `default-counters.csv` does). This profile does not list it, because the
  profile is pinned to the catalog's metric fields and a `label` row is not
  one; adding it is a separate, reviewed change.

Until one path is verified in a cluster, no rule or dashboard may assume a
`driver` label, and the fail-slow peer group is `(gpu_model[, node_pool])`
without it. A placeholder would be worse than absence: it would make peers
of GPUs that are not.

### Read-only access to nodes

`attach_metadata: {node: true}` makes vmagent list and watch Node objects
through the Kubernetes API. This profile ships no RBAC of its own for that.
The VictoriaMetrics operator generates, for every VMAgent it reconciles in
cluster-wide mode, a ClusterRole and ClusterRoleBinding named
`monitoring:kubeneuron-monitoring:vmagent-kubeneuron`, bound to the
ServiceAccount `vmagent-kubeneuron` it also creates, and that role grants
`get`, `list`, `watch` on core `nodes`, `nodes/metrics`, `services`,
`endpoints`, `pods` and `namespaces` (plus endpointslices, ingresses and the
`/metrics` non-resource URLs). No write verb is granted. This was verified in
the VictoriaMetrics operator v0.73.1 source
(`internal/controller/operator/factory/vmagent/rbac.go`, functions
`getClusterWideRules` and `createK8sAPIAccess`), not assumed from
documentation; a unit test fails if the operator pin changes without this
statement being re-verified.

Two conditions keep that role in place, and the same test pins both in
`observability/vmagent.yaml`: the resource must not set
`serviceAccountName` (a custom account would not be bound to the generated
role) and must not set `ingestOnlyMode: true` (then the operator generates
no scrape RBAC at all). The operator must also run cluster-wide, which the
pinned `install-no-webhook.yaml` does (`WATCH_NAMESPACE` is empty); an
operator restricted to a namespace list generates namespaced Roles without
`nodes`, and `node_pool` would then be silently absent on every series. In
that configuration, add a ClusterRole with exactly `get`, `list`, `watch` on
core `nodes` bound to `vmagent-kubeneuron`; nothing more is needed and
nothing more should be granted.

## Shadow-only alert routing

The `kubeneuron-gpu-shadow` VMRule (canonical file
`configs/vmalert/gpu-shadow-rules.yaml`, group `gpu-shadow`) carries the v0.7
`ShadowGpu*` alerts: PCIe link width degraded, NVLink link degraded,
fail-slow, thermal headroom low, memory thermal headroom low, memory beyond
repair and row remap pending. They read only the recorded `kubeneuron_gpu_*`
series, never a dcgm-exporter series directly, and they are observation for
humans: no shadow alert opens an incident or reaches the controller. The
boundary is enforced at three levels, each pinned by a unit test
(`internal/detect/shadow_isolation_test.go`,
`internal/depprofile/shadow_routing_test.go`):

1. **Names.** Every alert is `ShadowGpu*`, never `Gpu*`, so the controller
   webhook route in `observability/vmalertmanager.yaml`
   (`alertname =~ "Gpu.*|KubeNeuronAgentDown"`) cannot match it.
2. **Routing.** Every rule carries `kubeneuron_shadow="true"`, and the
   Alertmanager config has an explicit route for that label to the
   `human-default` receiver placed *before* the webhook route, with
   `continue: false`. The routing test evaluates the rendered route tree:
   a shadow label set lands on `human-default`, and a `GpuDoubleBitEccVolatile`
   label set still lands on `kubeneuron`, unchanged.
3. **Code.** No `ShadowGpu*` name is in the controller's alert-to-class map,
   whose content is pinned to its pre-v0.7 golden list, so the controller
   would drop a shadow alert as unknown even if it were mis-routed.

The label is spelled `kubeneuron_shadow` because Prometheus label names are
`[a-zA-Z_][a-zA-Z0-9_]*`; a Kubernetes-style spelling with a dot and a
slash is invalid in rule labels, Alertmanager matchers and `ALERTS{}`
selectors, and a test forbids that spelling (and its underscore-sanitised
form) anywhere in `configs/`, `deploy/` and `docs/`.
Every shadow rule has `severity: info`. The `human-default` receiver is a
placeholder in this profile (an empty receiver drops notifications), so wire
it to a real channel before expecting to see a shadow alert outside the
dashboards, which read them from vmalert's `ALERTS{kubeneuron_shadow="true"}`
series. Disabling the shadow layer is `kubectl delete vmrule
kubeneuron-gpu-shadow --namespace kubeneuron-monitoring`; the controller
holds no state about it. Each shadow alert has a runbook entry in
`docs/runbooks.md`, and no shadow rule has been exercised against GPU
hardware by this project.

## Alert-rule scope

The following problem classes the controller can map from an alert name
have no shipped rule in this profile, because no scraped series carries
them (the agent reports driver hangs, lost devices and failed diagnostics
through its own event path, not through a metric an alert could read):

- `GpuLost`
- `GpuDriverHang`
- `GpuDcgmDiagFailed`

They may be added only with an input series, a scrape configuration and an
integration test proving that series exists. `KubeNeuronAgentDown` does
ship (group `gpu-liveness`, read from the `kubeneuron-agent` scrape job).
Hardware-specific DCGM fields can still be absent on unsupported GPU models;
validate rule inputs against every target GPU family before relying on them.

## Upgrade order

Never make dependency upgrades from the KubeNeuron reconciler. Update the
lock, values, and manifests through a separate reviewed change, verify new
artifact checksums, read every intervening upstream release note, and keep
KubeNeuron remediation in `DryRun` throughout the maintenance window.

1. Back up or snapshot the VMSingle volume and record current CR, workload,
   target, and rule health.
2. Apply the new checksum-verified VictoriaMetrics Operator CRDs/operator
   server-side. Wait for `deployment/vm-operator` before touching its CRs.
3. Update and apply one lifecycle file at a time, waiting between them:
   `vmsingle.yaml`, `vmagent.yaml`, `vmalertmanager.yaml`, `vmalert.yaml`, then
   `rules.yaml`.
4. Upgrade GPU Operator independently with the newly verified chart archive:

   ```sh
   helm upgrade gpu-operator "$GPU_CHART" \
     --namespace gpu-operator \
     --values "$PROFILE/gpu-operator/values.yaml" \
     --wait \
     --timeout 30m
   ```

   Wait for `cluster-policy` readiness, GPU allocatable resources, exporter
   endpoints, and vmagent targets after the rollout.
5. Upgrade KubeNeuron separately after all dependency health gates pass.

Do not downgrade VictoriaMetrics storage or NVIDIA driver components merely
by restoring an older tag. Check the upstream compatibility and rollback
instructions first; on-disk formats and host changes can make a tag rollback
unsafe.

## Removal order and retained data

Removal is the reverse dependency order. Keep both upstream operators running
until they have finalized their custom resources.

1. Remove KubeNeuron configuration/root resources and its runtime first, after
   making a separate decision about the controller's SQLite data.
2. Stop evaluation and delivery, then ingestion:

   ```sh
   kubectl delete -f "$PROFILE/observability/rules.yaml" --wait
   kubectl delete -f "$PROFILE/observability/vmalert.yaml" --wait
   kubectl delete -f "$PROFILE/observability/vmagent.yaml" --wait
   kubectl delete -f "$PROFILE/observability/vmalertmanager.yaml" --wait
   ```

3. Back up/snapshot VMSingle and inspect the StorageClass and PV reclaim
   policy. Delete VMSingle only when its data disposition is explicit:

   ```sh
   kubectl delete -f "$PROFILE/observability/vmsingle.yaml" --wait
   kubectl get pvc,pv --namespace kubeneuron-monitoring
   ```

   `removePvcAfterDelete: false` preserves `pvc/vmsingle-kubeneuron` when the
   VMSingle CR is deleted. Deleting the namespace still deletes that PVC; a
   `Delete` reclaim policy can then destroy its PV and data. Leave the
   namespace/PVC in place, or snapshot/copy the data and verify the retained
   PV, before deleting the namespace. Do not use `kubectl delete -k
   observability` as a shortcut.
4. Remove VictoriaMetrics Operator only if it is dedicated to this profile and
   no VictoriaMetrics CR remains anywhere in the cluster:

   ```sh
   kubectl get vmsingle,vmagent,vmalertmanager,vmalert,vmrule --all-namespaces
   kubectl delete -f "$VM_OPERATOR_MANIFEST" --wait
   ```

   Its release manifest includes cluster-wide CRDs. Removing it while another
   installation uses those CRDs is destructive.
5. Remove GPU Operator last, and only if no other GPU workload depends on it:

   ```sh
   helm uninstall gpu-operator --namespace gpu-operator --wait
   kubectl delete -f "$PROFILE/gpu-operator/namespace.yaml" --wait
   ```

   `operator.cleanupCRD: false` intentionally leaves NVIDIA CRDs for an
   explicit cluster-administrator cleanup. NVIDIA driver modules can remain
   loaded after Helm removal; follow the upstream uninstall procedure before
   deleting CRDs, unloading modules, or rebooting nodes.

VMSingle retains metrics for 30 days but this profile provides no backup job.
Alertmanager has no persistent volume, so silences and notification state are
ephemeral. Mirror and digest-pin the images before relying on this profile in production.

## Deliberate exclusions

- cert-manager: not needed because the VictoriaMetrics Operator
  `install-no-webhook` artifact is used.
- PostgreSQL: the KubeNeuron controller supports it as the HA workflow
  store, but this dependency profile does not provision a PostgreSQL server
  — bring your own and reference its DSN Secret in `spec.workflowStore`.
- ClickHouse: raw-event archive ingestion is not implemented and the operator
  rejects enabled archive configuration.
- Grafana, node_exporter, ingress, external authentication, TLS, HA, and
  automated backup: outside this reference profile.
- NVIDIA GDS/GDRCopy, vGPU/VFIO, KubeVirt/Kata sandbox, and confidential
  computing operands: explicitly disabled in the base values; enable and pin
  them only in a reviewed platform-specific overlay.
- KubeNeuron operator/controller/agent images: built and released separately;
  their current development samples still use mutable tags.
- KubeNeuron-agent alert rules: excluded until their metrics path exists, as
  described above.

## Official upstream references

- [NVIDIA GPU Operator 26.3 installation](https://docs.nvidia.com/datacenter/cloud-native/gpu-operator/26.3/getting-started.html)
- [NVIDIA GPU Operator 26.3 release notes](https://docs.nvidia.com/datacenter/cloud-native/gpu-operator/26.3/release-notes.html)
- [NVIDIA GPU Operator 26.3 uninstall](https://docs.nvidia.com/datacenter/cloud-native/gpu-operator/26.3/uninstall.html)
- [NVIDIA GPU Operator v26.3.3 release](https://github.com/NVIDIA/gpu-operator/releases/tag/v26.3.3)
- [VictoriaMetrics Operator setup](https://docs.victoriametrics.com/operator/setup/)
- [VictoriaMetrics Operator v0.73.1 release](https://github.com/VictoriaMetrics/operator/releases/tag/v0.73.1)
- [VictoriaMetrics Operator VMSingle lifecycle and storage](https://docs.victoriametrics.com/operator/resources/vmsingle/)
- [VictoriaMetrics v1.147.0 release](https://github.com/VictoriaMetrics/VictoriaMetrics/releases/tag/v1.147.0)
- [Alertmanager v0.32.1 release](https://github.com/prometheus/alertmanager/releases/tag/v0.32.1)
