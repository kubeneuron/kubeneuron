# Production install (operator-managed Kubernetes)

The operator-managed path is the only supported authenticated deployment
shape. It requires Kubernetes **1.29+** (the CRDs use quantity CEL
functions); the integration harness pins and tests v1.33.

## 0. Dependencies (separately owned)

KubeNeuron never creates its observability stack. Install and verify the
version-pinned dependency profile first — GPU Operator (supplies
dcgm-exporter) and the VictoriaMetrics operator — following
[`deploy/kubernetes/dependencies/`](https://github.com/kubeneuron/kubeneuron/tree/main/deploy/kubernetes/dependencies).

## 1. CRDs and operator

```sh
kubectl apply -k config/default
kubectl get crd | grep kubeneuron.io   # six CRDs Established
kubectl -n kube-neuron get deployment kubeneuron-operator
```

!!! tip "One-command install"
    [`deploy/install.sh`](https://github.com/kubeneuron/kubeneuron/blob/main/deploy/install.sh)
    (also attached to every release as `install-vX.Y.Z.sh`) performs the
    whole procedure below in one run: CRDs + operator, generated tokens
    and a panel admin password, a self-signed 90-day TLS bootstrap, a
    starter observe-only configuration, and a readiness wait — then
    prints your sign-in credentials. It is the fastest path to a working
    DryRun installation; return to this page for production TLS,
    notifications, and real playbooks. `--uninstall` reverts it.

    The one-liner:
    ```sh
    curl -sfL https://github.com/kubeneuron/kubeneuron/releases/latest/download/install.sh \
      | bash -s -- --version v0.1.1
    ```
    Pin the version in anything repeatable: it selects the matching signed
    images and install manifest. `--version latest` exists for a quick
    look, not for a production install.

## 2. Namespace and root object

Create the runtime namespace yourself (the operator deliberately never
creates or owns it), then the root `KubeNeuron`. Start from
[`config/samples/`](https://github.com/kubeneuron/kubeneuron/tree/main/config/samples)
and review every image and endpoint. Keep `executionMode: DryRun`.

Read back the installation UID — the certificate identity depends on it:

```sh
ROOT_UID=$(kubectl get kubeneuron <name> -o jsonpath='{.metadata.uid}')
```

## 3. TLS bootstrap (manual, four Secrets)

Agent↔controller traffic requires TLS 1.3 mTLS plus a projected Pod-bound
ServiceAccount token. No cert-manager or service mesh is required, but you
(or an offline issuer) must create the material once:

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
custom key). References must omit `namespace`. The operator only wires the
volume mounts — it never reads, owns, or generates Secret data.

!!! note "Rotation"
    Issuance, renewal, and revocation are manual at this stage; certificate
    changes require coordinated workload restarts. The kind harness tests
    an immutable-versioned rotation procedure end to end — follow that
    pattern (new versioned Secrets → expand trust → switch leaf → contract
    trust).

!!! tip "cert-manager convenience path"
    [`deploy/cert-manager/`](https://github.com/kubeneuron/kubeneuron/tree/main/deploy/cert-manager)
    automates the four Secrets with two dedicated in-cluster CAs and
    auto-renewing 90-day leaves (renewed 35 days out, ahead of the 30-day
    expiry alert). Leaf renewal still needs a workload rollout to take
    effect; CA rotation deliberately stays the manual expand/contract
    procedure above.

## 4. Tokens and notifications

```sh
openssl rand -hex 32 > operator-token
openssl rand -hex 32 > webhook-token
kubectl -n <ns> create secret generic kubeneuron-operator-api-token --from-file=token=operator-token
kubectl -n <ns> create secret generic kubeneuron-alertmanager-webhook-token --from-file=token=webhook-token
kubectl -n <ns> create secret generic kubeneuron-slack --from-literal=webhook-url=https://hooks.slack.com/services/...
```

Reference `kubeneuron-alertmanager-webhook-token` from
`spec.notifications.webhookToken`: it is mandatory, so the operator-managed
Alertmanager endpoint is never anonymous. Copy the same Secret into the
monitoring namespace so Alertmanager can present it — the dependency
profile's `VMAlertmanager` mounts it and sends
`Authorization: Bearer <token>` with every webhook delivery:

```sh
kubectl -n <ns> get secret kubeneuron-alertmanager-webhook-token -o yaml \
  | sed 's/namespace: <ns>/namespace: kubeneuron-monitoring/' | kubectl apply -f -
``` Reference the API Secret from
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
audit trails of the incidents the system *would* have remediated. The current
operator rejects `executionMode: Enabled` at CEL admission and during
compilation: its Kubernetes agent image does not yet provide the host GPU
tooling, script provisioning, or crash-safe action journal needed for real
side effects.
