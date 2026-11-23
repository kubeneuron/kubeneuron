# KubeNeuron

KubeNeuron helps teams that train models on Kubernetes recover cloud capacity after an accelerator or node fault. It detects the fault, replaces the affected node when policy permits, and closes the incident once the controller verifies the new node. In Automatic mode this can complete without waking an operator to repair the node manually.

## v1.0.0-rc.2 candidate scope and evidence

The cloud policies are written for fleets using NVIDIA GPUs, AMD Instinct on Azure, Google TPU and AWS Trainium. AMD Instinct, Google TPU and AWS Trainium have not been used in a live run of this candidate. In DryRun, operators can inspect proposed actions. Approval mode asks an operator to authorize disruptive steps. Automatic mode executes eligible steps under the configured policy.

| Cloud | Simulated provider | Live CPU replacement with synthetic accelerator resource | Live accelerator validation |
|---|---|---|---|
| Amazon Elastic Kubernetes Service (EKS) | Replacement tested | Add-first and remove-first each completed once, on pre-release builds | Add-first and remove-first on a Tesla T4 node pool each completed once, with a fault injected as an XID 79 kernel-log line on a healthy GPU, on build adcda3d4ae80834dd08f99a99f7b0a3d463ea8d4 — the code released as v1.0.0-rc.2 (the commits after it change only a test script, the version, the changelog and the documentation). Device health was not checked by KubeNeuron. |
| Azure Kubernetes Service (AKS) | Replacement tested | Add-first and remove-first on a fixed-size single-zone node pool each completed once, on a pre-release build | Unverified |
| Google Kubernetes Engine (GKE) | Replacement tested | Add-first and remove-first on a fixed-size single-zone Standard node pool each completed once, on pre-release builds | Unverified |

On Amazon EKS, in a node pool with real NVIDIA Tesla T4 GPUs, KubeNeuron replaced a node twice under an Automatic policy, with no manual approvals: add-first, then remove-first on the node that had replaced the first. Each fault was injected as an NVIDIA XID 79 line in the node's kernel log; the GPU itself was healthy. KubeNeuron verified each new node by readiness, advertised capacity and the device inventory its agent reported, and the driver answered; device health was not checked. Separately from KubeNeuron, the run checked the GPU on each new node with nvidia-smi (Tesla T4, driver 580.178.04) and the CUDA vectorAdd sample, which passed. This was done once, on build adcda3d4ae80834dd08f99a99f7b0a3d463ea8d4 — the code released as v1.0.0-rc.2 (the commits after it change only a test script, the version, the changelog and the documentation).

In the earlier CPU replacement runs the nodes were CPU nodes with synthetic accelerator capacity, and the controller verified the new node by node readiness and advertised capacity. Device health on the new node was not checked. On EKS the controller was updated during the add-first incident. On GKE the controller was updated between the two runs. The runs were not repeated on v1.0.0-rc.1 or v1.0.0-rc.2. GKE remove-first on autoscaled pools, multi-zone pools and Autopilot were not run live. In those runs, and with the simulated providers, the incident resolved by itself after the controller confirmed the new node. Use `kubeneuronctl pools` to inspect add-first eligibility and `kubeneuronctl incidents show <id>` to see how the controller identified the node and what it verified. The incident records infrastructure recovery. It does not show that a training job resumed. See the [capability and evidence matrix](docs/reference-capabilities.md#live-cloud-evidence) for individual checks.

Upgrading from 0.x releases is not supported. Uninstall the 0.x installation, then install v1.0. See the [clean installation steps](docs/upgrade.md#moving-from-0x-to-v10).

[Installation guide](docs/install.md) · [Documentation](docs/index.md)

## Architecture

Kubernetes configuration and runtime execution are deliberately separate:

```text
KubeNeuron custom resources
        |
        v
kubeneuron-operator ------> generated runtime ConfigMaps
        |                              |
        +------> controller Deployment + agent DaemonSet

GPU nodes                           runtime control plane
-----------                         ---------------------
dcgm-exporter ---- metrics -------> metrics/alerting stack
kubeneuron-agent -- mTLS events --> kubeneuron-controller
                   + Pod token
kubeneuron-agent <- actions ------- remediation workflow
```

The repository builds four custom binaries:

| Binary | Role in cloud recovery |
|---|---|
| `kubeneuron-operator` | Reconciles installation settings and deploys the controller and agents. |
| `kubeneuron-controller` | Opens incidents, applies the recovery policy, requests replacements and verifies the new node. |
| `kubeneuron-agent` | Runs without privileges by default and reports node signals and accelerator inventory to the controller. |
| `kubeneuronctl` | Shows pool eligibility and incident evidence, and lets operators approve steps or pause recovery. |

VictoriaMetrics, vmalert, Alertmanager, Grafana, dcgm-exporter, and
node_exporter are integrations rather than KubeNeuron binaries. The
KubeNeuron operator does not deploy VictoriaMetrics, Alertmanager, PostgreSQL,
or ClickHouse; those dependencies remain external or are owned by their
dedicated upstream operators.

See the [design document](docs/design.md) for the target architecture and its
explicit implementation boundaries.

## Kubernetes API

The `kubeneuron.io/v1alpha1` API currently defines eight cluster-scoped
custom resources:

| Kind | Purpose |
|---|---|
| `KubeNeuron` | Root installation, runtime images, target namespace, safety mode, store, and integration references. |
| `GPURemediationPolicy` | Maps a normalized problem class to a playbook. |
| `GPUPlaybook` | Defines a typed sequence of allow-listed remediation actions. |
| `GPUSignalMapping` | Declarative XID/alert classification overrides, compiled into the detection catalog (xid/alertmanager sources; label matchers rejected fail-closed). |
| `GPUMaintenanceWindow` | Bounded automation pause for selected nodes; compiled into the runtime and enforced by the reconcile walk (matchLabels selectors, pauseAutomation only). |
| `GPUNodeConfig` | Per-node settings; `paused` is enforced by the walk (SSH/BMC credential refs rejected fail-closed until an actuator consumes them). |
| `AcceleratorRuntimeProfile` | Server-owned accelerator runtime contract (selector, pinned driver/DCGM versions, allowed semantic actions); gates physical reset eligibility, never enables execution. |
| `GPUAutonomyPlan` | A narrow, expiring, declarative envelope for one approved action class; validation/status are CRD-native, while frozen simulation, approvals, rollout observations, and effects remain durable controller records. |

Configuration objects select their root installation through
`spec.kubeNeuronRef`; `GPUAutonomyPlan` is intentionally independent so a
GitOps write cannot itself trigger an effect. The operator compiles the supported subset into runtime
ConfigMaps, annotates managed workloads with a deterministic configuration
digest, and reports validation/readiness on the root `KubeNeuron` status.
All eight kinds are consumed or reconciled: signal mappings override the
detection catalog, maintenance windows and node-config pauses hold automation,
accelerator profiles narrow action authority, and autonomy plans receive a
fail-closed status projection. Unsupported sub-fields still fail validation
rather than being silently serialized.
The API is `v1alpha1` and may change without compatibility guarantees.

## Build and local checks

Go 1.26 or newer is required (the module sets `toolchain go1.26.11`; a Go 1.25
install with the default `GOTOOLCHAIN=auto` downloads it).

```sh
make build
make test
make lint
make test-integration-kind

./bin/kubeneuron-operator --version
./bin/kubeneuron-controller --version
./bin/kubeneuron-agent --version
./bin/kubeneuronctl --version
```

No GPU is needed for the unit tests.

`make test-integration-kind` is the slower Kubernetes integration target. It
requires Docker, jq, curl, OpenSSL, checksum-pinned kind v0.32.0, and kubectl
v1.33.12; multi-node kind needs raised inotify limits
(`fs.inotify.max_user_instances=512`, `max_user_watches=524288`). The harness
builds static local images and creates a digest-pinned Kubernetes v1.33.12
kind cluster with one control plane and two workers (`WORKER_NODES`
configurable), runs the 85-case CEL admission matrix, and checks operator
readiness, all 11 ownership references, collision failure/non-adoption,
recovery, least-privilege RBAC, durable registration-readiness loss/recovery,
an acknowledged no-op reconciliation, and preservation of the unowned TLS
input Secrets. It also tests the mTLS/identity boundary with valid and invalid
certificates, malformed/wrong tokens, a deleted-Pod token, spoofed node
payloads, and distinct public API/webhook bearer tokens. The same run exercises
the manual immutable/versioned server and client TLS-rotation procedure,
including plan collisions, failed leaf activation, failed trust contraction,
ordered rollback, fresh post-controller registration acknowledgments, and
rejection of retired trust. It additionally induces `RuntimeUnavailable` with
a bad server leaf and proves explicit emergency recovery with pre-provisioned
server/client leaves under the existing CAs.
It deletes its cluster by default; set
`KEEP_CLUSTER=1 KEEP_RESOURCES=1` to retain a run for inspection.
`make kind-clean` deletes that cluster later but deliberately leaves the
kubeconfig path untouched, because the path is configurable and may contain
user-managed contexts.

This is deliberately a CPU-only Kubernetes reconciliation test. A passing run
validates the agent mTLS transport, Kubernetes Pod/node authorization, and a
durable SQLite registration acknowledgment, plus the specifically tested
manual routine TLS-rotation and dual-leaf recovery contracts — now across a
multi-node cluster with an agent per worker. The test checks that an agent cannot register under an arbitrary other node name.

## Operator installation

The CRDs use Kubernetes quantity CEL functions, so the installation requires
Kubernetes 1.29 or newer. A separately owned, version-pinned GPU/observability
dependency profile and its install, upgrade, and removal gates are documented
in [`deploy/kubernetes/dependencies/`](deploy/kubernetes/dependencies/). Install
and verify those dependencies first when using the supplied sample endpoints.

The Kustomize entry points then install the CRDs/operator and apply the narrow
development configuration the runtime currently accepts:

```sh
kubectl apply -k config/default
kubectl apply -k config/samples

kubectl get crd | grep kubeneuron.io
kubectl -n kube-neuron get deployment kubeneuron-operator
kubectl get kubeneurons.kubeneuron.io
```

Review the sample image references and all safety/store settings before
applying them. The samples are development examples, and successful creation
of the CRs does not imply that the controller/agent execution path
is production-ready for your environment. Omit `config/samples` if you only
want to install the API and operator.

Operator-managed SQLite uses a `ReadWriteOnce` PersistentVolumeClaim. Its
request defaults to `5Gi` and may be increased, but not decreased; the
`workflowStore.sqlite` object is required for SQLite so storage intent cannot
be reset by removing and re-adding optional settings. The workflow-store type
and storage-class intent cannot be changed after creation. Readiness remains
false until the claim is Bound, reports the requested capacity, has no active
resize state, and the current controller/agent workload generations are fully
available. Deleting the root
`KubeNeuron` also garbage-collects its owned claim, after which the
StorageClass/PersistentVolume reclaim policy determines whether the data is
retained. The controller provides a SQLite snapshot endpoint and the
[operations runbook](docs/operations.md#sqlite-workflow-store-backup-and-restore)
defines restore; schema migrations are forward-only, so a rollback restores a
pre-upgrade snapshot. Retention controls cover terminal operational resources,
idempotency records, and audit chains as described in that runbook.

The public listener (operator API, Alertmanager webhook, control panel, and
metrics) serves plain HTTP by default and should sit behind a TLS-terminating
Ingress/Gateway; alternatively set the optional
`spec.tls.publicServerSecretRef` to a `kubernetes.io/tls`-style Secret and the
controller serves that listener over TLS 1.3 directly, switching its managed
probes to HTTPS. Alertmanager and `kubeneuronctl` must then be pointed at
`https://` with the matching CA.

The operator path has a dependency-free identity boundary: it needs no Istio,
cert-manager, or external PKI service, but an installer must provision four
TLS Secrets in `spec.namespace` and omit `namespace` from their CR references:

- a controller `kubernetes.io/tls`-style Secret with `tls.crt`/`tls.key`; its
  `serverAuth` certificate covers
  `<installation>-controller.<namespace>.svc`;
- an agent-client CA bundle, using `ca.crt` unless its reference selects a
  different key;
- a shared fleet `kubernetes.io/tls`-style client Secret whose current, non-CA
  `clientAuth` leaf is valid for at most 100 days and has exactly one URI SAN:
  `spiffe://kubeneuron.io/installation/<KubeNeuron-UID>/agent`;
- a controller-server CA bundle for agents, again defaulting to `ca.crt`.

Before applying the root, also create an unowned `operatorAPIToken` Secret
(key `token`) and an unowned `webhookToken` Secret (key `token`) in the same
namespace. `webhookToken` is mandatory so the managed Alertmanager ingress is
never anonymous; `operatorAPIToken` is mandatory for `Paused` mode and enables
the authenticated operator API in other modes.

The root UID is available after the `KubeNeuron` object is created, so manual
bootstrap creates the root, reads `.metadata.uid`, issues the fleet leaf, and
then creates the four referenced Secrets. The kind harness demonstrates this
with ephemeral local CAs. The shared leaf proves installation membership, not
node identity. Every agent request also carries an explicitly projected,
one-hour Pod-bound ServiceAccount token with audience
`kubeneuron-controller`; the controller performs TokenReview and verifies the
live ServiceAccount UID, Pod UID/owner/labels/node binding, DaemonSet, and Node.
The agent rereads the projected token for every request.

Both proofs are intentional: server authentication prevents the bearer token
from being sent to an impersonated controller; the fleet certificate gates
installation membership; and the Pod-bound token distinguishes the current
node workload within that fleet. Neither client proof is treated as sufficient
by itself.

Certificates and trust bundles are loaded only at process start. The supported
routine lifecycle deliberately uses new, uniquely versioned Secrets with
`immutable: true`; changing a Secret reference changes only the consuming Pod
template and lets the operator perform a controlled process rollout. Do not
mutate Secret data under an existing name: there is no content watcher or hot
reload, and such a change does not trigger a rollout.

[`hack/tls-rotate.sh`](hack/tls-rotate.sh) applies one resumable phase at a
time. Root annotations record the current rotation ID, direction, phase,
declared Secret names/keys, and their UIDs; every continuation must match that
bound plan. Rotation IDs must be globally unique; the helper rejects reuse of
the currently recorded terminal ID but does not retain older transaction
history. These annotations are current transaction state, not an append-only
audit log. The helper requires the old
leaf/CA names plus three pairwise-distinct, unowned immutable candidates: an
old+new CA bundle, the new key-pair Secret, and a new-only CA bundle. Server
and client directions must be rotated separately:

- server: expand agent trust, activate the controller leaf, then retire old
  agent trust;
- client: expand controller trust, activate the fleet client leaf, then retire
  old controller trust.

Each phase waits for the exact Secret reference to reach the affected
Deployment or DaemonSet, its rollout, the exact root generation/phase, and
root readiness. Any controller replacement additionally requires every
managed agent's `/readyz` acknowledgment sequence to advance after the
replacement is ready, proving a fresh durable heartbeat without comparing
clocks across nodes. Trust retirement requires
`--approve-retire-old-trust`. Before retirement, `rollback-leaf` followed by
`rollback-trust` restores the starting references in safe order. After a
failed contraction, `rollback-retirement` first restores overlap trust, after
which the same leaf/trust rollback remains safe. Re-running a recorded phase
performs the same convergence checks without repatching it. Rollback only
requires material needed by that rollback, so an administrator can quarantine
a failed or unused candidate. Each phase is recorded with a patch checked
against the root `resourceVersion` the helper validated; the operator's own
status writes advance that version, so a lost race is re-read and retried
(bounded by `PATCH_CONFLICT_RETRIES`, default 5) only while `spec.tls` and
every TLS transaction annotation are exactly as validated. A root that already
carries precisely the planned result is accepted without a second write; any
other concurrent change, such as a different transaction claiming the root, is
refused with a drift diagnostic and nothing is written. The helper never
creates, changes, prints, owns, or deletes Secret data; inspect its exact
arguments with `hack/tls-rotate.sh --help`.

[`hack/tls-emergency-recover.sh`](hack/tls-emergency-recover.sh) is the
separate, explicit recovery path for an already-unready root caused by unusable
leaf material. It requires an approval flag and two distinct, immutable,
unowned pre-provisioned leaf Secrets. It cryptographically checks them against
the current CAs and installation identity, records their names and UIDs, keeps
both CA references unchanged, and waits for controller/agent rollouts and
`Ready=True`. Its single patch follows the same `resourceVersion` retry rule
as the rotation helper and additionally refuses to retry once the root has
become Ready. It cannot repair an expired, revoked, or compromised CA and is
not an issuance or revocation protocol.

These manual, dependency-free procedures cover the externally-issued path.
With `spec.tls.issuer: Operator`, the operator now issues the installation's
mTLS material itself and renews it automatically before expiry — reissuing the
managed Secrets (marked by provenance so material it does not own is reported
but never touched) and rolling the consumers, since certificates are still
read only at process start. CA recovery/revocation orchestration and CRL/OCSP
remain unimplemented. A controller TLS rollout uses the single-replica
`Recreate` Deployment and causes real ingress downtime.
Replacing only a leaf under the same retained CA is renewal, not revocation of
a stolen old leaf; effective revocation in this contract rotates/removes the
issuing CA and replaces every old consumer process.

The agent `/readyz` endpoint now means that the agent received a successful
controller acknowledgment after the controller durably stored its narrow
registration payload within the last 90 seconds. The DaemonSet checks it every
10 seconds with a one-failure threshold, so Kubernetes Pod readiness converges
after the next probe. Before posting, the agent requires an exact versioned
capability token; this prevents a new narrow-payload agent from corrupting
inventory through a legacy full-node controller endpoint, but it is not a
complete rolling-upgrade protocol. The server supplies the heartbeat time and
agent registration cannot overwrite controller-owned platform, labels,
actuation addresses, or pause state. This is a connectivity/persistence signal,
not GPU health. Registration and event routes exist only on the controller's
TLS 1.3 client-certificate listener at port 8443. Public health and the
Alertmanager webhook remain on port 8080. In the operator-managed path the
webhook requires `spec.notifications.webhookToken`; a direct development
controller must explicitly pass `-allow-insecure-webhook` to opt out, but that
configuration is not a supported operator installation.

The remaining `deploy/` content is current: version-pinned third-party
dependency profiles under `deploy/kubernetes/dependencies/`, the SQLite
backup CronJob example under `deploy/kubernetes/backup/`, and the Grafana
dashboard under `deploy/grafana/`.

## Safety posture

The target design includes dry-run, concurrency limits, cooldowns, flap
detection, typed actions, approval gates, idempotency, and a durable audit
trail. The shipped implementation provides real foundations, not the
completed safety case:

- shipped file configuration and an omitted CR `executionMode` default to
  dry-run;
- `executionMode: Paused` starts the controller with the global gate closed
  and requires `spec.notifications.operatorAPIToken`; `Enabled` lifts dry-run
  but only under `spec.safety.destructiveExecution` — a non-empty node
  selector and the exact acknowledgement sentence — and arms exclusively the
  agents on the named nodes, so the blast radius is declared, not global;
- the API accepts `SQLite` (single controller) and `Postgres` (two replicas
  with Lease-based leader election and readiness that follows leadership);
- the metrics path (`spec.observability`) is `Managed` (Collector, metrics
  store with its rules, Grafana), `External` or `Disabled`; an External
  Collector with the Managed store is not offered (docs/observability.md);
- ClickHouse archival is rejected until its runtime path exists;
- CRD playbooks accept a closed set of typed actions rather than arbitrary
  shell commands;
- credentials are represented by Secret references, not inline values.

Approval delivery (Slack, generic webhook, PagerDuty), verification before
resolution, and the pause/resume control path are implemented. The EKS, AKS and GKE replacement paths each completed once on a live cluster with CPU nodes and synthetic accelerator capacity, on pre-release builds. On accelerator hardware, only the EKS Tesla T4 run above was done: each replacement order once, with an injected fault on a healthy GPU. AKS and GKE are not validated on accelerator hardware. Start in DryRun. See the [live cloud evidence table](docs/reference-capabilities.md#live-cloud-evidence) for what was tested and how.

## Configuration sources

- [`api/v1alpha1/`](api/v1alpha1/) — Kubernetes API types.
- [`config/crd/bases/`](config/crd/bases/) — generated CRD manifests.
- [`config/samples/`](config/samples/) — development custom-resource examples.
- [Cloud recovery policies](config/cloud-policies/) are the default for a fresh
  installation.
- [`configs/policies.yaml`](configs/policies.yaml) and
  [`configs/playbooks/`](configs/playbooks/) — current file-based controller
  input used by local and bare-metal development.
- [`configs/vmalert/gpu-rules.yaml`](configs/vmalert/gpu-rules.yaml) — alert
  rules.
- [`docs/xid-catalog.md`](docs/xid-catalog.md) — XID classification rationale.

Bare-metal deployments and node-level actions such as GPU reset are alpha and outside the v1.0 cloud validation scope. The candidate has no real-hardware validation for these paths. See [Bare metal (alpha)](docs/bare-metal.md) for the required policy and operator permissions.

## Security

Report vulnerabilities using the [security policy](SECURITY.md). Review [Data and privacy](docs/data-and-privacy.md) for agent privileges, network connections and data retention.

## Community

Questions, incident stories, and "is this a KubeNeuron problem or a driver
problem?" belong in the [Discord](https://discord.gg/HHhHFT8v7W); design
proposals and long-form discussion belong in
[GitHub Discussions](https://github.com/kubeneuron/kubeneuron/discussions).

## Contributing

Contributions are welcome. Start with [CONTRIBUTING.md](CONTRIBUTING.md) and
read [docs/design.md](docs/design.md) before changing API or safety-sensitive
behavior.

## License

Version 1.0 is released under Apache-2.0. Each 0.x release remains under the license under which it was published. 0.x releases are no longer distributed from this repository. Source for the GPL and LGPL components of 0.x images is available on request. See
[LICENSE](LICENSE) and [NOTICE](NOTICE); third-party components are listed in
THIRD_PARTY_LICENSES. "KubeNeuron" and its logo are trademarks of Andrii Petruk;
see [TRADEMARKS.md](TRADEMARKS.md).

KubeNeuron is an independent project. It is not affiliated with, sponsored by, or endorsed by NVIDIA, AMD, Intel, Amazon Web Services, Google, Microsoft, Grafana Labs, VictoriaMetrics, the OpenTelemetry project, or the Kubernetes project. All product names and trademarks are the property of their respective owners and are used only to describe compatibility.
