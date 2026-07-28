# KubeNeuron

**GPU failure detection and remediation for NVIDIA clusters, with a
Kubernetes-native configuration model.**

KubeNeuron is intended to turn GPU and driver signals into an audited,
policy-driven remediation workflow. The target escalation ladder ranges from
observation and workload eviction through GPU reset, node drain, reboot, and
hardware escalation.

> **Status: Kubernetes dry-run preview; not production-ready.** The
> controller's dry-run workflow, audit trail, durable controller queue,
> authenticated operator API, CLI, CRD reconciliation, and agent mTLS plus
> Pod-bound identity are exercised. The operator rejects `executionMode:
> Enabled` both at CEL admission and during compilation: its managed DaemonSet
> does not yet supply host GPU tooling or approved scripts, and its agent-side
> completion cache is not crash-safe across a restart. **No NVIDIA, NVML,
> DCGM, GPU action, or real-hardware remediation claim is made from this
> CPU-only environment.**

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

| Binary | Responsibility | Current maturity |
|---|---|---|
| `kubeneuron-operator` | Watches KubeNeuron CRDs, validates and compiles their configuration, and reconciles the controller and agent Kubernetes workloads. | Working preview reconciler; SQLite store; `DryRun`/`Paused`; `Enabled` is deliberately rejected until the managed agent runtime is hardware-verified. Alertmanager webhook authentication is mandatory; Paused also requires an API token. |
| `kubeneuron-controller` | Ingests Alertmanager and agent events and owns incident, policy, safety, and workflow execution. | Full dry-run workflow implemented: state walk, safety gates, approvals, escalation, transactional audit, durable action queue, authenticated operator REST API. |
| `kubeneuron-agent` | Runs on GPU nodes, watches kernel events, reports inventory/events, and executes queued actions. | Registration/events use mTLS plus projected Pod-bound identity on the operator path. The binary has typed action contracts, including idle checks, but the managed image currently lacks the host GPU binaries and scripts required to claim action execution; `Enabled` remains unavailable. |
| `kubeneuronctl` | Operator-facing CLI for status, incidents, approvals, manual remediation, and pause/resume. | All declared commands implemented against the operator REST API. |

VictoriaMetrics, vmalert, Alertmanager, Grafana, dcgm-exporter, and
node_exporter are integrations rather than KubeNeuron binaries. The
KubeNeuron operator does not deploy VictoriaMetrics, Alertmanager, PostgreSQL,
or ClickHouse; those dependencies remain external or are owned by their
dedicated upstream operators.

See the [design document](docs/design.md) for the target architecture and its
explicit implementation boundaries.

## Kubernetes API

The `kubeneuron.io/v1alpha1` API currently defines seven cluster-scoped
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

Configuration objects select their root installation through
`spec.kubeNeuronRef`. The operator compiles the supported subset into runtime
ConfigMaps, annotates managed workloads with a deterministic configuration
digest, and reports validation/readiness on the root `KubeNeuron` status.
All seven kinds are consumed: signal mappings override the detection catalog,
maintenance windows and node-config pauses hold automation, and unsupported
sub-fields inside any of them still fail validation rather than being
silently serialized.
The API is `v1alpha1` and may change without compatibility guarantees.

## Build and local checks

Go 1.25 or newer is required.

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

No GPU is needed for the unit tests. The retired plaintext Compose, systemd,
and static Kubernetes scaffolds have been removed: they described the old
bare-metal path and could not run against the current fail-closed binaries.
The operator-managed Kubernetes path is the only authenticated deployment
shape at this checkpoint.

`make test-integration-kind` is the slower Kubernetes integration target. It
requires Docker, jq, curl, OpenSSL, checksum-pinned kind v0.32.0, and kubectl
v1.33.12; multi-node kind needs raised inotify limits
(`fs.inotify.max_user_instances=512`, `max_user_watches=524288`). The harness
builds static local images and creates a digest-pinned Kubernetes v1.33.12
kind cluster with one control plane and two workers (`WORKER_NODES`
configurable), runs the 53-case CEL admission matrix, and checks operator
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
multi-node cluster with an agent per worker. Its spoof test still rejects an
arbitrary other node name; replaying real node-A credentials against node B
remains future work. It does not validate NVIDIA drivers, real
NVML, DCGM, GPU telemetry or actions, automated issuance/expiry detection,
CA revocation/recovery, the future action RPC, crash-safe action completion
across an agent restart, or remediation behavior.

## Operator preview install

The CRDs use Kubernetes quantity CEL functions, so the preview profile requires
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
of the CRs does not imply that the unfinished controller/agent execution path
is production-ready. Omit `config/samples` if you only want to install the API
and operator.

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
retained. Backup, restore, and an explicit retention policy are not yet
implemented.

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
a failed or unused candidate. The helper never creates, changes, prints,
owns, or deletes Secret data; inspect its exact arguments with
`hack/tls-rotate.sh --help`.

[`hack/tls-emergency-recover.sh`](hack/tls-emergency-recover.sh) is the
separate, explicit recovery path for an already-unready root caused by unusable
leaf material. It requires an approval flag and two distinct, immutable,
unowned pre-provisioned leaf Secrets. It cryptographically checks them against
the current CAs and installation identity, records their names and UIDs, keeps
both CA references unchanged, and waits for controller/agent rollouts and
`Ready=True`. It cannot repair an expired, revoked, or compromised CA and is
not an issuance or revocation protocol.

These are manual, dependency-free procedures, not automated PKI. Issuance,
expiry scheduling/alerts, CA recovery/revocation orchestration, CRL/OCSP, and
hot reload remain unimplemented. A controller TLS rollout uses the
single-replica `Recreate` Deployment and causes real ingress downtime.
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
webhook requires `spec.notifications.webhookToken`; an intentionally direct
development controller may omit its flag, but that configuration is not a
supported operator installation.

The remaining `deploy/` content is current: version-pinned third-party
dependency profiles under `deploy/kubernetes/dependencies/`, the SQLite
backup CronJob example under `deploy/kubernetes/backup/`, and the Grafana
dashboard under `deploy/grafana/`. The retired static Kubernetes, systemd,
and Compose scaffolds were removed because they lacked the managed DaemonSet
ownership and projected identity required by the controller.

## Safety posture

The target design includes dry-run, concurrency limits, cooldowns, flap
detection, typed actions, approval gates, idempotency, and a durable audit
trail. The current skeleton provides some foundations, not the completed
safety case:

- shipped file configuration and an omitted CR `executionMode` default to
  dry-run;
- `executionMode: Paused` starts the controller with the global gate closed
  and requires `spec.notifications.operatorAPIToken`; `Enabled` is rejected
  until the managed agent runtime has host tooling, crash-safe completion, and
  hardware-gated verification;
- the API reserves `Postgres` as a future workflow-store value, but the
  operator rejects it because the controller has no PostgreSQL backend;
- observability currently accepts only credential-free `External` endpoints;
  `Managed` discovery/readiness and ClickHouse archival are rejected until
  their runtime paths exist;
- CRD playbooks accept a closed set of typed actions rather than arbitrary
  shell commands;
- credentials are represented by Secret references, not inline values.

Approval delivery, full action execution and verification, and the complete
pause/resume control path are still roadmap work.

## Configuration sources

- [`api/v1alpha1/`](api/v1alpha1/) — Kubernetes API types.
- [`config/crd/bases/`](config/crd/bases/) — generated CRD manifests.
- [`config/samples/`](config/samples/) — development custom-resource examples.
- [`configs/policies.yaml`](configs/policies.yaml) and
  [`configs/playbooks/`](configs/playbooks/) — current file-based controller
  input used by local and bare-metal development.
- [`configs/vmalert/gpu-rules.yaml`](configs/vmalert/gpu-rules.yaml) — alert
  rules.
- [`docs/xid-catalog.md`](docs/xid-catalog.md) — XID classification rationale.

## Product tour

[docs/one-pager.md](docs/one-pager.md) is the one-page pitch: problem,
differentiators, proof points.
[docs/product-tour.md](docs/product-tour.md) walks the whole workflow with
screenshots and a recorded demo captured on a live EKS cluster with a real
Tesla T4: a kernel-injected XID 79 opens an incident, the playbook ladder
runs, a named human approves the reboot step, and the audit trail records
every transition.

## Roadmap

The execution plan with per-item status is
[PRODUCTION_READINESS_PLAN.md](PRODUCTION_READINESS_PLAN.md)
(ROADMAP.md is its historical predecessor). Current status:

**Done and released (v0.1.0):** the complete DryRun control plane —
incident workflow, agent action dispatch with lease/boot-ID protocol,
approvals with verifiable actor identity (Kubernetes TokenReview + RBAC),
REST API/CLI/web panel, metrics/dashboards/alerts/runbooks and the docs
site, hardened TLS lifecycle with rotation and cert-manager convenience,
PostgreSQL HA store with leader election and proven no-duplicate
failover, notification channels (Slack/webhook/PagerDuty) with
retry/dead-letter, signed multi-arch images with SBOM. GPU runtime access
uses the real `nvidia-smi` binary through opt-in host mounts
(`spec.agent.hostTooling`), verified end-to-end on a live T4 node —
kernel XID through the full dry-run remediation ladder.

**Remaining, deliberately hardware-gated:** a standing GPU lab CI target
(destructive reset/reboot validation) and an NVML/DCGM event stream as a
second detection source beside kmsg. `executionMode: Enabled` stays
rejected by construction until the hardware verification matrix in
[PRODUCT_PLAN.md](PRODUCT_PLAN.md) passes — dry-run is the supported
production mode today.

**Later evaluations:** ClickHouse archival, Slurm, and ticketing
integrations.

## Contributing

Contributions are welcome. Start with [CONTRIBUTING.md](CONTRIBUTING.md) and
read [docs/design.md](docs/design.md) before changing API or safety-sensitive
behavior.

## License

[Apache 2.0](LICENSE)
