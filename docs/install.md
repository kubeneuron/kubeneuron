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
kubectl get crd | grep kubeneuron.io   # seven CRDs Established
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
    notifications, and real playbooks. `--uninstall` reverts it.

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

New to this? [docs/pilot-checklist.md](pilot-checklist.md) walks the whole
path from a green install to a first incident visible in the panel. Reference the API Secret from
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
