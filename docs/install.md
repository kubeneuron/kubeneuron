# Production install (operator-managed Kubernetes)

The operator-managed path is the only supported authenticated deployment
shape. It requires Kubernetes **1.29+** (the CRDs use quantity CEL
functions); the integration harness pins and tests v1.33.

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
| `store` | the operator runs single-node VictoriaMetrics as `<name>-metrics-store` on a PersistentVolumeClaim | the collector exports to `endpoint` | the collector drops what it receives |

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
  collector decides where metrics go. With an external collector an external
  store only names the data source for a Managed Grafana.
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
  behind a NetworkPolicy that admits only the collector; where NetworkPolicy
  is not enforced, the store is reachable inside the cluster without
  authentication.
- The collector's NetworkPolicy admits OTLP from the installation's
  controller and agents and from the operator. For an external store it
  allows egress to the store's port on any address, since a NetworkPolicy
  cannot name a host.
- Both images are pinned by digest in the operator (override with
  `--otel-collector-image` and `--metrics-store-image`, for a mirror) and in
  `deploy/kubernetes/dependencies/versions.lock.yaml`; they are pulled from
  their publishers, not redistributed by KubeNeuron.
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
    notifications, and real playbooks. `--uninstall` removes the installation while retaining SQLite state;
    `--uninstall --delete-data` explicitly removes that state after confirmation.
    Reinstall with the same name, namespace and storage settings to reuse it.

    The one-liner:
    ```sh
    curl -sfL https://github.com/kubeneuron/kubeneuron/releases/latest/download/install.sh \
      | bash -s -- --version latest
    ```
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
    run the same script the release pipeline runs on itself:

    ```sh
    hack/verify-release.sh v0.2.2
    ```

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
and `<name>-webhook-token`; create them by hand only on the manual path, and
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
