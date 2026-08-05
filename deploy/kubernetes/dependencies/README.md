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
                                      |
                                      v
                         KubeNeuron controller webhook
```

The generated in-cluster integration endpoints are:

| Dependency | Endpoint |
|---|---|
| VictoriaMetrics | `http://vmsingle-kubeneuron.kubeneuron-monitoring.svc:8428` |
| Alertmanager | `http://vmalertmanager-kubeneuron.kubeneuron-monitoring.svc:9093` |
| Controller webhook target | `http://kubeneuron-controller.kube-neuron.svc:8080/api/v1/webhooks/alertmanager` |

For a KubeNeuron custom resource that uses this profile, declare each
observability dependency as an external endpoint and use the first two
addresses:

```yaml
observability:
  victoriaMetrics:
    mode: External
    endpoint: http://vmsingle-kubeneuron.kubeneuron-monitoring.svc:8428
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
`service/vmsingle-kubeneuron` on port `8428` and query
`DCGM_FI_DEV_GPU_TEMP`. Do not treat the profile as healthy until every
intended dcgm-exporter endpoint is up and a DCGM series is queryable.

### 4. KubeNeuron

Install KubeNeuron CRDs/operator separately only after the dependencies are
healthy. Apply a reviewed KubeNeuron resource with the endpoints above last.
The Alertmanager receiver can exist before the controller and will retry, but
that temporary delivery failure is not dependency readiness.

## Alert-rule scope

[gpu-operator/values.yaml](gpu-operator/values.yaml) creates a
`kubeneuron-dcgm-metrics` ConfigMap through the chart-supported
`dcgmExporter.config` fields. It enumerates exactly the DCGM fields referenced
by this profile's hardware, thermal/power, and PCIe/NVLink rules.

The following canonical development rules are intentionally excluded from
this deployment profile because the current KubeNeuron agent exposes no
working metrics endpoint and VMAgent has no agent scrape target:

- `GpuLost`
- `GpuDriverHang`
- `GpuDcgmDiagFailed`
- `KubeNeuronAgentDown`

They may be added only with the runtime metrics implementation, a scrape
configuration, and an integration test proving each input series exists.
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
