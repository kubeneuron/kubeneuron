# Production install (operator-managed Kubernetes)

The operator-managed path is the only supported authenticated deployment
shape. It requires Kubernetes **1.29+** (the CRDs use quantity CEL
functions); the integration harness pins and tests v1.33.

## Recovery policies installed by the script

On a fresh installation, `deploy/install.sh` selects `--policy-pack cloud`
and sets the installation's execution mode to `DryRun`. The default policies
use `Approval`. Five classes select the replacement playbook: `fell-off-bus`, `gpu-lost`, `ecc-dbe`, `nvlink` and `accelerator-fault`. `cloud-interruption` selects its own playbook, which sends a checkpoint warning and, after approval, cordons the node. The other bundled bindings select observation.

To choose a ladder for one class, edit its `GPURemediationPolicy`.
Set `spec.playbookRef` to the installation's observe, cloud-replace or
cloud-recycle playbook. The recycle ladder additionally requires
`spec.recycle.enabled: true` and escalates to replacement. The default
replacement strategy is `AddFirst`, which needs a provider that can prepare
a replacement first: today, AWS on a single-zone EKS managed node group
whose size is declared fixed, Azure on a fixed-size AKS node pool and GCP on
a fixed-size GKE Standard node pool in one zone. All three are experimental;
each completed once on a live cluster with CPU nodes and synthetic
accelerator capacity, on pre-release builds (see the
[live cloud evidence table](reference-capabilities.md#live-cloud-evidence)).
With `replacement.successorZone: AnyPoolZone`, add-first also supports fixed-size multi-zone EKS and AKS pools. This placement was tested with simulated providers only.
Start in DryRun. Elsewhere,
an incident whose ladder includes replacement stays in `EVALUATING` before cordon. Its
audit records `recovery-unavailable` and explains that the provider lacks
add-first and the policy has not selected remove-first. See
[Add-first replacement](playbook-authoring.md#add-first-replacement). Check the installation's
`CloudRecoveryAvailable` condition for provider configuration and the
incident for the eligibility decision on its node.

A repeat install preserves edited policies and reports missing cloud pack
objects. On a fresh installation, `--policy-pack bare-metal-alpha` explicitly
selects the node-level pack (Alpha, not validated on real hardware). See [Bare metal (alpha)](bare-metal.md) for its
prerequisites and hardware validation status.

## Kernel log from the node's journal

With node-level actions disabled, the agent has no access to `/dev/kmsg`.
It reads kernel fault records from the node's systemd journal instead
(experimental). The agent has read-only access to the node's systemd
journal directories. It reads only kernel entries (`_TRANSPORT=kernel`),
matches them against its fault patterns and sends only the classified
event to the controller. It does not store, forward or log journal
entries, and does not read entries of other transports.

When the journal is disabled or unavailable and node-level actions are disabled, numeric NVIDIA XIDs depend on a reachable DCGM hostengine exposing field 230. The shipped exporter profile omits that field. Polling its last value can miss repeated identical XIDs. Without the hostengine, numeric XIDs are unavailable.

The agent mounts one journal directory per node, `/var/log/journal` or
`/run/log/journal`. The controller picks it from the node's OS image and
records the choice in the node label `kubeneuron.io/journal-dir` (`var` or
`run`). It sets the label only on accelerator nodes where the agent runs,
and changes it only on nodes that the installation's
`spec.agent.nodeSelector` matches, so installations with their agents on
separate node pools keep to their own nodes. Installations whose agents
share a node are not supported: their agents share the node's state
directory.
The agent DaemonSets are `<name>-agent` (no journal mount, for nodes
without the label), `<name>-agent-detect-journal-var` and
`<name>-agent-detect-journal-run`; each node runs exactly one of them. When the controller labels a node, its agent Pod moves to the matching journal DaemonSet. Its spool and cursors stay in `/var/lib/kube-neuron`. Scripts that wait for agent rollouts should select all three with `app.kubernetes.io/instance=<name>,kubeneuron.io/agent=true`. A
node whose OS image is not in the table keeps `<name>-agent`, and its
kernel log source reads `os-image-not-recognized` in `kubeneuronctl health`.
If the journal Pod cannot start within three minutes, the controller
removes the label (`journal-mount-failed`) and tries again after an hour or
after the node's OS image or your override changes. See the
[capability matrix](reference-capabilities.md#kernel-log-from-the-journal)
for which OS images were checked.

In every mode, including `DryRun`, KubeNeuron adds one label,
`kubeneuron.io/journal-dir`, to accelerator nodes to choose the agent's
journal mount. It runs no recovery action. Set
`spec.agent.kernelLog.journal.enabled: false` to remove the label and the
mount.

For an image the table does not know, set the directory by node selector;
the first matching override wins, and `none` mounts nothing:

```yaml
spec:
  agent:
    kernelLog:
      journal:
        nodeOverrides:
          - nodeSelector: {node.kubernetes.io/instance-type: p5.48xlarge}
            dir: var
```

`deploy/install.sh --uninstall` removes the label when no other
installation remains in the cluster. After an uninstall with Helm or by
hand, remove it yourself:

```sh
kubectl label nodes --all kubeneuron.io/journal-dir-
```

## DCGM client

DCGM is not bundled in the agent image. The agent uses the narrower
`nvidia-smi` health source unless a working `dcgmi` client is provided.
Without DCGM runtime attestation, accelerator readiness stays degraded and
destructive actions remain blocked. Client delivery is managed separately.

## 0. Dependencies (separately owned)

Install and verify the version-pinned dependency profile first — GPU
Operator (supplies dcgm-exporter) and the VictoriaMetrics operator —
following
[`deploy/kubernetes/dependencies/`](https://github.com/kubeneuron/kubeneuron/tree/main/deploy/kubernetes/dependencies).
The metrics path of KubeNeuron's own components can instead be run by the
operator; see [Metrics path](#metrics-path-specobservability).

### Metrics path (`spec.observability`)

`spec.observability.collector` and `spec.observability.store` each take
`mode: Managed | External | Disabled`; a block left out is `Disabled`.

| | Managed | External | Disabled |
|---|---|---|---|
| `collector` | the operator runs an OpenTelemetry Collector (`otel/opentelemetry-collector-k8s`) as `<name>-otel-collector`; the controller and agents export OTLP/HTTP to it | the controller and agents export to `endpoint` | nothing is exported |
| `store` | the operator runs single-node VictoriaMetrics as `<name>-metrics-store` on a PersistentVolumeClaim, and vmalert as `<name>-metrics-rules` for its recording rules | the collector exports to `endpoint` | the collector drops what it receives |
| `grafana` | the operator runs Grafana as `<name>-grafana` with the store as its datasource and the dashboards provisioned | your own Grafana at `endpoint`; load `deploy/grafana/*.json` into it | no Grafana |

- **An external store must accept OTLP over HTTP**: VictoriaMetrics
  (`http://<host>:8428/opentelemetry`) or Prometheus started with
  `--web.enable-otlp-receiver` (`http://<host>:9090/api/v1/otlp`). Prometheus
  remote-write is not supported. Each series carries the resource attributes
  `k8s.namespace.name`, `kubeneuron.installation` and `k8s.node.name` (no
  names of your pods, jobs or namespaces); VictoriaMetrics stores them as
  labels, Prometheus only those listed in
  `otlp.promote_resource_attributes`.
- **Credentials never go in the URL.** `authorizationSecretRef` names a
  Secret key in the installation namespace whose value is the whole
  `Authorization` header, for example `Bearer <token>`.
- **An external collector with a Managed store is rejected**: the external
  collector decides where metrics go. With an external collector the
  store's `endpoint` is not used.
- **The Managed store has no backup.** Its claim (`storage.size`, default
  `20Gi`; `storage.storageClassName`; `retention`, default `30d`) survives a
  switch to External or Disabled and the deletion of the installation: the
  operator never deletes it; delete it by hand. After the installation is
  deleted, the claim still holds up to 30 days of metrics (the configured
  `retention`), including node names. To erase them:
  `kubectl delete pvc <name>-metrics-store -n <namespace>`, where `<name>` is
  the KubeNeuron resource and `<namespace>` its `spec.namespace`. Until the
  claim is Bound, `ObservabilityStoreReady` is `False` with reason
  `StoragePending`. `storage.ephemeral: true` uses an emptyDir instead and
  loses the data with the pod.
- **The Managed store has no authentication.** It is a ClusterIP Service
  behind a NetworkPolicy that admits only the collector and the recording
  rules evaluator; where NetworkPolicy is not enforced, the store is
  reachable inside the cluster without authentication.
- **The Managed store comes with its rules.** A vmalert
  (`<name>-metrics-rules`) evaluates
  `configs/vmalert/gpu-recording-rules.yaml` against the store and writes
  the `kubeneuron_gpu_*` series the dashboards read back into it.
  `ObservabilityRulesReady` reports it. **With an External store the
  recording rules are yours to run**: load that file into the rule
  evaluator of your Prometheus or VictoriaMetrics (the file is plain PromQL,
  see [observability](observability.md)), or the GPU dashboards stay empty.
- **The Managed store detects the faults that reach KubeNeuron on NVIDIA
  only as metric alerts.** The same vmalert evaluates
  `configs/vmalert/gpu-alert-rules.yaml` and sends what fires to the
  controller (`/api/v1/webhooks/vmalert`, the Alertmanager API v2 body),
  with the webhook token from `notifications.webhookToken` as a bearer
  header read from a file, never in the URL; no Alertmanager is installed,
  and `spec.observability.alertmanager` is optional (absent means none).
  The six classes, with the thresholds of the dependency profile:

  | Class | Alert | Fires when |
  |---|---|---|
  | `thermal` | `GpuThermalThrottle`, `GpuTempCritical` | HW slowdown or thermal throttle for 10m; temperature above 88 °C for 5m |
  | `power` | `GpuPowerBrake` | HW power brake for 10m |
  | `row-remap-budget` | `GpuRemappedRowsHigh` | 8 or more uncorrectable remapped rows for 10m |
  | `pcie` | `GpuPcieReplayHigh` | more than 1000 PCIe replays in an hour |
  | `exporter-down` | `GpuExporterDown` | dcgm-exporter `up` is 0, or the node's GPU series stopped arriving, for 5m |
  | `agent-down` | `KubeNeuronAgentDown` | the node's agent series stopped arriving, for 3m |

  **`exporter-down` and `agent-down` never start an action**: they say
  that data stopped arriving, not that a GPU failed, so the controller only
  records and notifies them, in every mode and whatever policy binds them.
  A node removed by a scale-down or replaced under the same name opens
  neither, though its series stay "seen in the last hour" for a while.
  The alert expressions aggregate to the node and GPU, so the workload
  labels never reach an alert, also with `keepWorkloadLabels: true`.
  vmalert reaches nothing but the store and the controller's port 8080;
  with `tls.publicServerSecretRef` it speaks https and verifies the
  controller with that Secret's `ca.crt`. Without a `ca.crt` there,
  `AlertDetectionConfigured` is `False` with reason
  `NotifierCAUnavailable`: add the CA that signed `tls.crt` under that key.
  Otherwise it is `True` once vmalert is available.
  **With an External store the alert rules are yours to run** against your
  store, pointed at the controller webhook. `AlertDetectionConfigured` is
  `False` with reason `ExternalStore` until the controller accepts the first
  alert, then `True` with reason `AlertReceived` and the time it arrived.
- The collector's NetworkPolicy admits OTLP from the installation's
  controller and agents and from the operator. For an external store it
  allows egress to the store's port on any address, since a NetworkPolicy
  cannot name a host.
- **GPU metrics from the dcgm-exporter already in the cluster.** A Managed
  collector scrapes dcgm-exporter, for example the one GPU Operator
  installs. **KubeNeuron does not install dcgm-exporter**; without one there
  are no GPU metrics. The operator looks for the Service
  `collector.dcgmExporter.serviceName` (default `nvidia-dcgm-exporter`) in
  `collector.dcgmExporter.namespace`; with no namespace it takes every
  match in the cluster. It scrapes the Ready pods behind the Service's
  `gpu-metrics` port (job `dcgm-exporter`, every 30 seconds, 10-second
  timeout, at most 5000 samples a pod per scrape); a host-network exporter
  is scraped on its node's address. The series travel over OTLP, so in the
  store `job` arrives as `service.name="dcgm-exporter"` and `instance` as
  `service.instance.id`; write your own rules against those.
- **Target updates are not immediate.** The pods are looked up again every
  two minutes and written to a ConfigMap the collector reads without
  restarting; the kubelet takes up to another minute or two to refresh the
  mounted file, and the collector rereads it every minute. A new GPU node
  is scraped within about five minutes.
- Series carry `node`, `gpu_uuid`, `gpu_model` and `kubeneuron.installation`,
  plus `node_pool` and `mig_profile` where they exist, the same labels as
  the dependency profile's vmagent job. **The `pod`, `namespace`,
  `container` and `pod_uid` labels that dcgm-exporter adds for the workload
  holding a GPU are dropped before anything is stored**;
  `keepWorkloadLabels: true` keeps them, and then the store holds the names
  of your workloads. Any other label you enable in dcgm-exporter itself is
  stored as the exporter sends it; KubeNeuron does not filter it.
- `ObservabilityGPUMetricsReady` reports what is scraped, `NotFound` when
  there is no such Service and `NoReadyPods` when it has no Ready pods.
  `dcgmExporter.scrape: false` turns scraping off. **If you already scrape
  dcgm-exporter into the store you point KubeNeuron at, set
  `scrape: false`**, or every GPU series is stored twice.
- The collector's NetworkPolicy lets it reach only the exporter pods on
  that port (by the node's address for a host-network exporter); a
  NetworkPolicy in the exporter's namespace must admit it as well.
- The four images (collector, store, vmalert, Grafana) are pinned by digest
  in the operator (override with `--otel-collector-image`,
  `--metrics-store-image`, `--metrics-rules-image` and `--grafana-image`,
  for a mirror) and in
  `deploy/kubernetes/dependencies/versions.lock.yaml`; they are pulled from
  their publishers, not redistributed by KubeNeuron.
- **A Managed Grafana installs Grafana (AGPL-3.0), unmodified, pulled from
  Docker Hub (`grafana/grafana`).** It needs a Managed or External store and shows
  the six boards from `deploy/grafana` with the store as a read-only
  datasource. With an External store set `grafana.datasource.url` to the
  store's query address (`http://<host>:8428` for VictoriaMetrics,
  `http://<host>:9090` for Prometheus) and, if it needs one,
  `grafana.datasource.authorizationSecretRef` to a Secret key holding the
  whole `Authorization` header; Grafana sends it as a header, never in the
  URL. `ObservabilityGrafanaReady` reports the Deployment.
- **It is reachable only from inside the installation namespace**: a
  ClusterIP Service behind a NetworkPolicy that admits port 3000 from that
  namespace alone. With a Managed store Grafana reaches nothing but DNS
  and the store; with an External store, egress is open on the datasource
  port to any address, because the store's address cannot be named in the
  policy.
  Open it with
  `kubectl port-forward svc/<name>-grafana 3000:3000 -n <namespace>` and
  sign in as `admin` with the password from
  `kubectl get secret <name>-grafana-admin -n <namespace> -o jsonpath='{.data.admin-password}' | base64 -d`.
- **Anonymous access, sign-up, snapshots, public dashboards, the plugin
  catalogue, usage reporting and update checks are off.** Boards edited in
  the UI are not kept: Grafana runs on an emptyDir and the provisioned
  boards win on restart.
- **The admin account stays in the cluster.** The operator generates the
  password once into the Secret `<name>-grafana-admin`, which belongs to the
  installation and is deleted with it; switching Grafana to Disabled
  removes Grafana but keeps the Secret until then. A password you put in
  the Secret yourself is not overwritten; restart the Deployment
  (`kubectl rollout restart deployment/<name>-grafana -n <namespace>`) to
  apply it.
- `spec.observability.victoriaMetrics` is deprecated and is not a metrics
  destination: the collector never sends to it, and `ObservabilityStoreReady`
  says so until `store` is set. Set together with `store`, the installation
  is rejected.

## 1. CRDs and operator

```sh
kubectl apply -k config/default
kubectl get crd | grep kubeneuron.io   # eight CRDs Established
kubectl -n kube-neuron get deployment kubeneuron-operator
```

!!! tip "One-command install"
    [`deploy/install.sh`](https://github.com/kubeneuron/kubeneuron/blob/main/deploy/install.sh)
    (also attached to every release as `install-vX.Y.Z.sh`) performs the
    whole procedure below in one run: CRDs + operator, generated tokens
    and a panel admin password, a starter observe-only configuration, and
    a readiness wait — then prints your sign-in credentials. It does not
    generate TLS material; the operator issues that itself. It is the fastest path to a working
    DryRun installation; return to this page for production TLS,
    notifications, and real playbooks. In every mode, including `DryRun`,
    KubeNeuron adds one label, `kubeneuron.io/journal-dir`, to accelerator
    nodes to choose the agent's journal mount. It runs no recovery action.
    Set `spec.agent.kernelLog.journal.enabled: false` to remove the label
    and the mount. For a new installation it writes the
    whole metrics path as `Managed` (see [Observability](observability.md))
    and first names the third-party components it pulls;
    `--observability disabled` leaves it off. A re-run on an existing
    installation keeps what it has: the API and webhook tokens, the panel
    users and the KubeNeuron's spec, including its `observability` block
    (External endpoints and Secret references) or its lack of one. It
    updates only the component images and, when you pass it,
    `--observability`. `--uninstall` removes the
    installation while retaining SQLite state and the metrics store's claim;
    `--uninstall --delete-data` explicitly removes both after confirmation.
    Reinstall with the same name, namespace and storage settings to reuse it. See the [uninstall guide](uninstall.md) for Helm removal, data retention and node cleanup.

    The one-liner:
    ```sh
    curl -sfL https://github.com/kubeneuron/kubeneuron/releases/download/v1.0.0-rc.2/install.sh \
      | bash -s -- --version v1.0.0-rc.2
    ```
    Once the installation is Ready, follow the [Quickstart](quickstart.md) to connect kubeneuronctl, check the controller's status, and open a test incident.

    Pin a concrete version in anything repeatable: it selects the matching
    signed images and install manifest.

!!! note "What the release pins, and how to check it yourself"
    Every release publishes `images.txt` — the four images by digest — and
    both the install manifest and the versioned installer deploy those exact
    digests, so a re-pushed tag cannot change what an already-downloaded
    artifact installs. Override either component with `CONTROLLER_IMAGE` /
    `AGENT_IMAGE` if you mirror images into your own registry.

    The assets are covered by a single `checksums-vX.Y.Z.txt`, signed with
    cosign against this repository's GitHub OIDC identity. To verify a release
    end to end — asset set, checksum coverage, signature, and digest pinning —
    set `RELEASE_TAG` to the tag you downloaded and run:

    ```sh
    hack/verify-release.sh "$RELEASE_TAG"
    ```

    For v1.0.0-rc.2 and later, the minimum supported verifier for both
    checksum bundles and image signatures is Cosign 3.1.4. The standard
    Cosign 2.5.2 image verification command does not find the new image
    signatures. To verify the signed
    checksum file from the release downloads:

    ```sh
    identity="https://github.com/kubeneuron/kubeneuron/.github/workflows/release.yaml@refs/tags/$RELEASE_TAG"
    issuer=https://token.actions.githubusercontent.com
    sums="checksums-$RELEASE_TAG.txt"
    cosign verify-blob --bundle "$sums.sigstore.json" --certificate-identity "$identity" --certificate-oidc-issuer "$issuer" "$sums"
    ```

    For v1.0.0-rc.1 use its original
    detached signature and certificate. With the same variables, run:

    ```sh
    cosign verify-blob --signature "$sums.sig" --certificate "$sums.pem" --certificate-identity "$identity" --certificate-oidc-issuer "$issuer" "$sums"
    ```

    Verify each published image using
    `cosign verify --certificate-identity "$identity" --certificate-oidc-issuer "$issuer" <image@digest>`.

## 2. Namespace and root object

Create the runtime namespace yourself (the operator deliberately never
creates or owns it), then the root `KubeNeuron`. Start from
[`config/samples/`](https://github.com/kubeneuron/kubeneuron/tree/main/config/samples)
and review every image and endpoint. Keep `executionMode: DryRun`.

Read back the installation UID — the certificate identity depends on it:

```sh
ROOT_UID=$(kubectl get kubeneuron <name> -o jsonpath='{.metadata.uid}')
```

## 3. TLS (operator-issued by default)

Agent↔controller traffic requires TLS 1.3 mTLS plus a projected Pod-bound
ServiceAccount token. By default (`spec.tls.issuer: Operator`) the
operator issues and auto-renews all TLS material itself — no cert-manager,
service mesh, or manual Secrets are required. Foreign material in the
managed Secrets is only watched and warned about, never overwritten, and
per-workload TLS revisions roll exactly the consumers that mount changed
material.

To bring your own PKI instead, you (or an offline issuer) create the
material once, as four Secrets:

1. **Controller serving leaf** — `serverAuth`, subject/SAN covering
   `<name>-controller.<namespace>.svc`.
2. **Agent fleet client leaf** — `clientAuth`, a current non-CA leaf valid
   **at most 100 days**, whose *only* URI SAN is exactly:

   ```
   spiffe://kubeneuron.io/installation/<ROOT_UID>/agent
   ```

   This proves membership in this installation; node identity comes from
   the projected token, so the shared leaf is not a node credential.
3. Two CA bundles: the CA that signed the agent leaf (verified by the
   controller) and the CA that signed the controller leaf (verified by
   agents).

Create them in `spec.namespace` under the names referenced by `spec.tls`
(key-pair Secrets as `kubernetes.io/tls`; CA bundles under `ca.crt` or a
custom key). References must omit `namespace`. With bring-your-own
material the operator only wires the volume mounts — it never reads,
owns, or generates Secret data.

!!! note "Rotation"
    Operator-issued material renews automatically, and renewal rolls
    exactly the workloads that mount the changed material. For
    bring-your-own material, issuance, renewal, and revocation are manual
    and certificate changes require coordinated workload restarts. The
    kind harness tests an immutable-versioned rotation procedure end to
    end — follow that pattern (new versioned Secrets → expand trust →
    switch leaf → contract trust).

!!! tip "cert-manager convenience path"
    [`deploy/cert-manager/`](https://github.com/kubeneuron/kubeneuron/tree/main/deploy/cert-manager)
    automates the four Secrets with two dedicated in-cluster CAs and
    auto-renewing 90-day leaves (renewed 35 days out, ahead of the 30-day
    expiry alert). Leaf renewal still needs a workload rollout to take
    effect; CA rotation deliberately stays the manual expand/contract
    procedure above.

## 4. Tokens and notifications

The one-command installer already creates these as `<name>-operator-api-token`
and `<name>-webhook-token`, and a re-run keeps them; create them by hand
only on the manual path, and
keep the names consistent with whichever path you used:

```sh
openssl rand -hex 32 > operator-token
openssl rand -hex 32 > webhook-token
kubectl -n <ns> create secret generic <name>-operator-api-token --from-file=token=operator-token
kubectl -n <ns> create secret generic <name>-webhook-token --from-file=token=webhook-token
kubectl -n <ns> create secret generic <name>-slack --from-literal=webhook-url=https://hooks.slack.com/services/...
```

Reference `<name>-webhook-token` from
`spec.notifications.webhookToken`: it is mandatory, so the operator-managed
Alertmanager endpoint is never anonymous. Copy the same Secret into the
monitoring namespace so Alertmanager can present it — the dependency
profile's `VMAlertmanager` mounts it and sends
`Authorization: Bearer <token>` with every webhook delivery:

```sh
kubectl -n <ns> get secret <name>-webhook-token -o yaml \
  | sed 's/namespace: <ns>/namespace: kubeneuron-monitoring/' | kubectl apply -f -
```

Reference the API Secret from
`spec.notifications.operatorAPIToken` to enable `kubeneuronctl`, the web
panel, and `http_sd`; it is mandatory when `executionMode: Paused` so an
authenticated operator can resume the gate. Slack is optional and is
referenced from `spec.notifications.slack`.

## 5. Configuration objects

Apply your `GPUPlaybook` and `GPURemediationPolicy` set (samples provide a
starting pair), plus optional `GPUMaintenanceWindow`, `GPUSignalMapping`,
and `GPUNodeConfig` objects. Everything compiles into digest-covered
ConfigMaps; any change rolls the controller automatically. Validation is
fail-closed twice — at admission (CEL) and in the operator compiler.

## 6. Verify

```sh
kubectl get kubeneuron <name> -o jsonpath='{.status.conditions}'   # Ready=True/RuntimeAvailable
kubectl -n <ns> get deploy,ds,pvc                                  # controller 1/1, agents ready
curl -H "Authorization: Bearer $(cat operator-token)" http://<controller>:8080/api/v1/nodes
```

Agent DaemonSet readiness means "durable controller registration
acknowledged within 90 s" — it is a connectivity signal, not GPU health.

## 7. Burn in

Run at least two weeks in `DryRun`, watching `kubeneuron_*` metrics and the
audit trails of the incidents the system *would* have remediated.
`executionMode: Enabled` is supported but off by default: admission,
compilation, controller dispatch, and the agent executor all require
`spec.safety.destructiveExecution` with a non-empty node selector and an
exact acknowledgement, and the agent records every side effect in a
crash-safe action journal (intent before start, outcome before reporting).
Confine any move to `Enabled` to the narrowest node selector that covers
the fleet you intend.


### EKS managed nodegroups

Add-first and remove-first replacement each completed once on a live EKS
cluster with CPU nodes and synthetic accelerator capacity, on pre-release
builds; neither is validated on accelerator hardware (see the
[live cloud evidence table](reference-capabilities.md#live-cloud-evidence)).
Start in `DryRun` mode. Remove-first terminates the EC2 instance first, then
its Auto Scaling group launches a successor. The default `AddFirst` strategy
raises the group's size first. Declare the group size fixed. The default
`SameZone` placement requires a single-zone group. For a multi-zone group,
set `replacement.successorZone: AnyPoolZone` if workloads can run in any
zone of the group. This placement was tested with simulated providers only
(see [cloud-aws.md](cloud-aws.md)). On an ineligible group the incident waits before cordon and says why.

For an AWS installation, set `spec.cloud.aws.clusterName` on the KubeNeuron
resource to the name of the EKS cluster. The operator passes it to the
controller. Each node's `eks.amazonaws.com/nodegroup` label identifies the
group to inspect. The controller verifies instance membership through AWS
before using the group's repair policy. Omitting the cluster name keeps
cloud recovery unavailable.
