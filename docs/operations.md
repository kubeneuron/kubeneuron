# KubeNeuron operations guide

Day-2 procedures for a running installation. Everything here assumes the
operator-managed Kubernetes path; `<ns>` is your `spec.namespace` and
`<name>` the root `KubeNeuron` object name.

## Tokens

| Token | Set with | Used by |
|---|---|---|
| Operator API | controller `-api-token-file` | `kubeneuronctl`, the web panel, vmagent `http_sd` |
| Alertmanager webhook | controller `-webhook-token-file` | Alertmanager `http_config.authorization` |
| Slack webhook URL | `spec.notifications.slack` Secret | controller notifier |
| Generic webhook URL (+ optional bearer) | `spec.notifications.webhook` Secret (keys `url`, `token`) | controller notifier |
| PagerDuty routing key | `spec.notifications.pagerduty` Secret (key `routing-key`) | controller notifier (Events API v2) |

Generate strong random tokens (`openssl rand -hex 32`) and reference their
Secrets through `spec.notifications.operatorAPIToken` and
`spec.notifications.webhookToken`; the operator mounts them into the
controller without reading their contents. `webhookToken` is mandatory for
every operator-managed installation. Without `-api-token-file` the operator
API and panel are disabled (fail closed); the API token is mandatory for
`Paused` mode so an authenticated operator can resume it.

**Prefer per-person credentials over the shared token.** Managed
installations also accept any Kubernetes bearer token on the operator API
(`--api-authn-kubernetes`, set automatically by the operator): the
controller verifies the caller with `TokenReview` and admits them only if
Kubernetes RBAC grants `get` (reads) / `update` (mutations) on this
installation's `kubeneurons.kubeneuron.io` object. Audit rows then carry
the verified principal. Actions taken with the shared static token are
recorded as `token:<claimed name>` — treat that token as break-glass and
grant humans RBAC instead:

```sh
kubectl create clusterrole kubeneuron-operators \
  --verb=get,update --resource=kubeneurons.kubeneuron.io \
  --resource-name=<name>
kubectl create clusterrolebinding kubeneuron-operators \
  --clusterrole=kubeneuron-operators --user=alice@example.com
kubeneuronctl --token "$(kubectl create token sre-bot -n ops)" ...
```

**Rotation:** update the token Secret in place
(`kubectl -n <ns> create secret generic <name> --from-literal=token="$(openssl rand -hex 32)" --dry-run=client -o yaml | kubectl apply -f -`).
The kubelet refreshes the mounted file within about a minute and the
controller re-reads it every 10 seconds — no restart. Rotate immediately
if `kubeneuron_auth_failures_total` spikes. Repeated failed attempts from
one source are throttled (20 failures/minute → `429`), and every failure
is logged with the source address.

## Notification delivery guarantees

Every external channel (Slack, generic webhook, PagerDuty) runs behind its
own asynchronous queue so an outage can never stall ingestion or the
reconcile walk. Failed deliveries are retried 4 times with exponential
backoff (~21 s total); after the final failure the payload is
**dead-lettered to the controller log** (`notification dead-lettered after
retries`, full message included for manual replay) and
`kubeneuron_notifications_dropped_total{reason="dead_letter"}` increments —
the `KubeNeuronNotificationsDropped` alert covers both queue overflow and
dead-letters. The audit trail, not any notification channel, remains the
system of record.

PagerDuty mapping: one KubeNeuron incident = one PagerDuty alert
(`dedup_key` = incident ID). `needs_human`, `expired`, and approval
requests page at `critical`; lifecycle updates re-trigger at
`warning`/`info`; incident resolution resolves the PagerDuty alert.

The generic webhook receives `POST` JSON
`{"version":1,"kind":"<event|approval_required>","step?","message?","incident":{...}}`
with an optional `Authorization: Bearer` header from the Secret's `token`
key. Consumers must ignore unknown fields.

## Pausing automation

Three independent mechanisms, all fail toward not acting:

1. **Global pause (big red button):** `kubeneuronctl pause` /
   `kubeneuronctl resume`, the panel button, or
   `POST/DELETE /api/v1/pause`. Also available at install time via
   `executionMode: Paused` (controller starts gate-closed).
2. **Per-node pause:** create a `GPUNodeConfig` with `paused: true` for the
   node. The compiled set is authoritative: deleting the CR unpauses.
3. **Maintenance windows:** `GPUMaintenanceWindow` with a time range and
   optional `matchLabels` node selector. Incidents hold their position and
   continue after the window closes; recorded approvals survive the wait.

## SQLite workflow store: backup and restore

The controller's state (incidents, audit, approvals, node inventory, action
queue) lives in a single SQLite database on the operator-managed PVC
(`<name>-controller-state`), written in WAL mode by exactly one controller
Pod.

**Backup (online, consistent):** the controller serves an authenticated
snapshot endpoint — a `VACUUM INTO` copy taken while it keeps running, so no
`sqlite3` binary or `kubectl exec` is needed:

```sh
curl --fail -H "Authorization: Bearer $OPERATOR_TOKEN" \
  http://<name>-controller.<ns>.svc:8080/api/v1/backup \
  -o kubeneuron-$(date +%F).db
```

With `spec.tls.publicServerSecretRef` set, use `https://` and the serving
CA. Alternatively snapshot the PVC with your CSI driver's VolumeSnapshot —
take the snapshot while the controller is scaled to zero, or accept that WAL
replay happens on restore.

**Restore:**

```sh
kubectl -n <ns> scale deploy/<name>-controller --replicas=0
kubectl -n <ns> cp ./kubeneuron-YYYY-MM-DD.db <helper-pod>:/var/lib/kube-neuron/kubeneuron.db
# also delete kubeneuron.db-wal / kubeneuron.db-shm if present
kubectl -n <ns> scale deploy/<name>-controller --replicas=1
```

The controller applies schema migrations forward automatically on start
(`schema_version` table); never restore a database from a *newer* version
of the controller onto an older binary.

**Scheduled backups:** the CronJob template in
[`deploy/kubernetes/backup/`](https://github.com/kubeneuron/kubeneuron/tree/main/deploy/kubernetes/backup)
downloads the snapshot endpoint daily onto a dedicated backup PVC with dated
retention. It needs only the operator-token Secret and network reach to the
controller Service — no `pods/exec` RBAC and nothing extra in the distroless
image. Rehearse the restore procedure above against a downloaded snapshot
before relying on the schedule.

**Retention:** the PVC is owned by the root `KubeNeuron`; deleting the root
garbage-collects the claim, and the StorageClass reclaim policy then
decides whether data survives. Use `Retain` for production. The claim can
grow (`workflowStore.sqlite.size`) but never shrink. High-volume operational
tables (events, outbox, completed actions) are pruned hourly with a 90-day
default (`-store-retention`). Audit rows and their incidents are append-only
by default; budget roughly a few KB per incident step, or opt in to
`-store-audit-retention` to prune *terminal* incidents with their audit and
approval history after a window you choose.

## PostgreSQL workflow store (HA installations)

Setting `spec.workflowStore.type: Postgres` with a DSN Secret
(`secretRef`, key `dsn`, e.g.
`postgres://user:pass@host:5432/kubeneuron?sslmode=require`) makes the
controller stateless: no state PVC, **two replicas** with Lease-based
leader election, rolling updates, and readiness that follows leadership —
Services always route humans, Alertmanager, and agents to the single
elected writer. A deposed leader exits immediately and rejoins as a
standby; the new leader reloads persisted cooldowns and flap history
before its first reconcile pass.

KubeNeuron does **not** deploy or manage PostgreSQL. Use your platform's
operator (CloudNativePG, RDS, …) and own its HA, backups, and PITR there:

- Backups: `pg_dump`/`pg_basebackup` or your provider's snapshots — the
  SQLite `GET /api/v1/backup` endpoint deliberately does not exist in
  Postgres mode.
- Point-in-time recovery: enable WAL archiving in your PostgreSQL
  deployment; KubeNeuron's schema carries a `schema_version` table, and an
  older controller binary refuses a newer schema after a careless restore.
- Retention inside the store (`-store-retention`, `-store-audit-retention`)
  works identically to SQLite and runs on the leader only.

## Monitoring KubeNeuron itself

- `GET /metrics` on the controller's public port: incident states,
  signal/step/gate/escalation counters, notification drops, and TLS expiry
  (`kubeneuron_*`). The agent serves `GET /metrics` on its health port
  (events posted/spooled, spool depth, registration acks). The dependency
  profile's vmagent scrapes controller, agent, and operator pods by their
  managed labels. Import
  [`deploy/grafana/kubeneuron-dashboard.json`](https://github.com/kubeneuron/kubeneuron/blob/main/deploy/grafana/kubeneuron-dashboard.json).
- The dependency profile ships a `kubeneuron-self` rule group that covers
  the recommended alerts: `KubeNeuronControllerDown`,
  `KubeNeuronIncidentNeedsHuman`, `KubeNeuronSignalsDropped`,
  `KubeNeuronNotificationsDropped`, `KubeNeuronAgentSpoolBacklog`, and
  `KubeNeuronTLSCertExpiringSoon` (30 days before any loaded certificate
  expires — the fleet leaf has a hard 100-day ceiling and no auto-renewal).
- Agent health: DaemonSet readiness *is* the durable-registration signal
  (stale after 90 s without a controller acknowledgment); the
  `KubeNeuronAgentDown` rule fires on an unreachable agent metrics endpoint.

## Bare-metal scrape discovery

`GET /api/v1/targets?port=9400` (operator token required) serves Prometheus
`http_sd` groups for every registered node. vmagent example:

```yaml
scrape_configs:
  - job_name: dcgm
    http_sd_configs:
      - url: http://<controller>:8080/api/v1/targets
        authorization:
          credentials_file: /etc/vmagent/kubeneuron-token
```

## NVIDIA host tooling in the agent DaemonSet

The agent image is distroless and carries no NVIDIA userspace. On GPU nodes,
opt in to read-only host mounts:

```yaml
spec:
  agent:
    hostTooling: {}           # defaults match the AL2023 EKS NVIDIA AMI
    # binDir: /usr/bin        # directory containing nvidia-smi (and dcgmi)
    # libDirs: ["/usr/lib64"] # driver userspace libraries
    # scriptsDir: /path/on/host  # optional, mounted at /etc/kube-neuron/scripts
```

The host bin directory is mounted at `/host/nvidia/bin` and prepended to the
agent's `PATH`; library directories become `LD_LIBRARY_PATH`, and the first
library directory is additionally mounted at `/lib64` — the scratch agent
image has no ELF interpreter, and executing a dynamically linked
`nvidia-smi` resolves its `PT_INTERP` (`/lib64/ld-linux-x86-64.so.2`)
before `LD_LIBRARY_PATH` is consulted, so `libDirs[0]` must contain the
dynamic loader (the `/usr/lib64` default does on AL2023). Setting
`hostTooling` also arms `--require-real-driver`: if `nvidia-smi` is missing
at the declared path, the agent refuses to start instead of silently running
the fake driver, and a wrong `binDir`/`libDirs` keeps the Pod in
`ContainerCreating` (`HostPathDirectory` requires the directory to exist on
the node). Keep `hostTooling`
unset on CPU-only node pools — use `nodeSelector` to split GPU and CPU
installs if needed.

## DCGM runtime attestation (required for destructive actions)

Before the controller admits a real reset or reboot, the node's agent must
attest the running DCGM version. Without that evidence the accelerator
report stays `degraded` and every destructive step is refused — the system
fails closed, which is why this section matters before you enable anything.

**The engine is not there by default.** The NVIDIA GPU Operator ships the
standalone host engine disabled (`dcgm.enabled=false`) because its metrics
exporter embeds a private one that nothing else can reach. Enable it:

```sh
helm upgrade --install gpu-operator nvidia/gpu-operator -n gpu-operator   --set dcgm.enabled=true
```

**The client ships with the agent.** With the GPU Operator installed, `dcgmi`
lives inside NVIDIA's own container and is *not* on the node — verified
directly on a live cluster — so mounting host binaries cannot find it and
attestation stays degraded. The agent image therefore carries its own pinned
DCGM client, and uses it by default.

**If you already installed DCGM on your nodes**, that client is *not* picked up
automatically, even though `hostTooling.binDir` comes first on the agent's PATH
(it has to, so `nvidia-smi` matches the host driver). This is deliberate: the
attested version is whatever client answers, and an `AcceleratorRuntimeProfile`
pins one exact value for the whole fleet. Letting the node decide would make
that value a property of how each node was provisioned — nodes built from
different images would attest different versions and part of the fleet would go
degraded for no visible reason. Name it explicitly when you mean it:

```yaml
agent:
  hostTooling:
    binDir: /usr/bin
    dcgmiPath: /usr/bin/dcgmi   # must live directly in binDir
```

Then pin your profile's `runtimeVersion` to *that* client's version. A mismatch
names the binary that produced the version, so "which client answered" is never
something you have to guess at.

Then point the agent at it:

```yaml
agent:
  hostTooling:
    dcgmEndpoint: nvidia-dcgm.gpu-operator.svc:5555
```

**Why a Service and not the node's address.** Measured on a live cluster:
the operator runs the engine as an ordinary pod with no host port, so
neither `<node-ip>:5555` nor the node's loopback reaches it — both are
refused with `unable to establish a connection`. The Service is the only
path, and it is safe for this purpose because the operator sets
`internalTrafficPolicy: Local`: a request from a pod only ever lands on the
engine sharing its node, so the attestation describes the hardware the
agent is actually standing on. If you point `dcgmEndpoint` at any other
Service, check that policy first — without it, attestation silently becomes
a statement about someone else's GPU.

!!! warning "DCGM has no authentication"
    Once the standalone engine is enabled, **any pod on that node can use
    the full DCGM API** — no credentials, no authorization. Verified from
    an unprivileged pod: it read the GPU model, PCI address, and UUID, and
    reached the configuration API. An attacker with a foothold in any
    workload can therefore inventory your accelerators, run diagnostics
    that disrupt training jobs, and alter power, clock, and ECC settings.
    This is upstream behaviour, not a KubeNeuron setting.
    `internalTrafficPolicy: Local` confines the blast radius to one node's
    engine. Treat GPU nodes accordingly: restrict who may schedule there.

!!! note "The attested version is the client's, not the engine's"
    `dcgmi --version` answers from the local binary and never contacts the
    engine, and DCGM exposes no way to ask the engine for its build. A newer
    client serving an older engine is normal with the GPU Operator. So an
    `AcceleratorRuntimeProfile`'s `runtimeVersion` pins the client the agent
    image ships; the engine is attested separately, by a live connection whose
    GPU count must match the independent `nvidia-smi` inventory. Both must hold
    before a report becomes `ready`.

    Matching is on major and minor, not the patch level: `dcgm-4.6` and
    `dcgm-4.6.1` both accept an attested `dcgm-4.6.2`. Otherwise every bump of
    the bundled client would degrade each fleet still pinning the previous
    patch — an outage caused by a release rather than by anything on the nodes.
    A newer *minor* is still refused, because a profile names the runtime it was
    reviewed against.

## Resetting a GPU means stopping NVIDIA's own monitoring

A drained node is still not a resettable one. Measured on live hardware:
`nv-hostengine`, `dcgm-exporter` and the device plugin each hold an open handle
on `/dev/nvidia0` without appearing as compute applications, and
`nvidia-smi --gpu-reset` fails with exit 19 while any of them runs.

Not all of them are Kubernetes workloads: `nvidia-persistenced` is a host
service, and on EKS the device plugin comes from the machine image rather than
the GPU Operator.

Put a `QuiesceAcceleratorStack` step before `GPUReset` and a
`RestoreAcceleratorStack` step after it. The quiesce switches the GPU Operator's
components off through its own `nvidia.com/gpu.deploy.*` node labels (touching
only those that were running), stops the persistence daemon, and then has the
node confirm from its own process table that nothing holds the GPU. A holder
that outlasts the step's timeout fails the step by name — the controller never
guesses that the stack has settled. Because stopping DCGM also erases the
attestation the reset gate reads, the quiesce step validates that evidence first
and pins it for the rest of the playbook — and refuses to stop anything when the
evidence would not have admitted a reset anyway.

Monitoring is restored automatically once the incident stops running, so a
playbook that fails at the reset never leaves the cluster blind.

!!! note "A device plugin from the machine image is not covered"
    On EKS the NVIDIA device plugin is a `kube-system` DaemonSet from the AMI
    with no `nvidia.com/gpu.deploy.*` label, so the quiesce cannot stand it
    down. It keeps `/dev/nvidia0` open and the step fails naming it. Stand it
    down yourself before a reset on such a cluster.

!!! warning "GPU reset is impossible on most cloud VMs"
    On AWS EC2 g4dn (passthrough T4) the guest has no PCI reset for the device
    — `/sys/bus/pci/devices/<addr>/reset` does not exist — and the reset fails
    even with zero holders, persistence mode off, and the NVIDIA kernel modules
    unloaded. NVIDIA's "currently in use by another process" text is generic and
    misleading here. Validate resets on bare metal or on-prem.

    The agent probes for this during attestation, so such a node **does not
    advertise `reset-device` at all** and the accelerator report says why:

    ```
    physical-device reset unavailable: GPU 0 cannot be reset: the kernel
    exposes no PCI reset for device 0000:00:1e.0; on a virtualized instance
    the hypervisor withholds it and no GPU reset can succeed
    ```

    A playbook targeting such a fleet is refused before it cordons or drains
    anything. Route those clusters through reboot and node replacement instead.

## GPU reset on cloud VMs: recycle or replace the instance

A hardware GPU reset is impossible on a virtualized instance — the hypervisor
withholds the PCI reset from the guest (measured on AWS g4dn), so the agent does
not advertise `reset-device` there. The cloud-native equivalent is to
reinitialize the instance itself:

- **ReplaceNode** terminates the instance; the node group's ASG or Karpenter
  provisions a fresh node. This is the primary primitive on any autoscaled
  fleet — an EKS managed node group, a self-managed ASG, or Karpenter — because
  the group's health check already owns instance lifecycle. The replacement
  boots clean, attests through the full pipeline, and the terminated node's
  incident closes as replaced once the node object disappears.
- **RecycleNode** stops and starts the *same* EC2 instance. Stop/start detaches
  it from its physical host and reattaches it, so the GPU passthrough is torn
  down and re-established from scratch — same node, EBS volumes and IP, a clean
  GPU. Use it **only on nodes that are not under an autoscaler.** On an ASG-backed
  group (which includes every EKS managed node group) the group's health check
  reaps the instance the moment it stops and launches a replacement — measured
  live: a recycle of a managed-node-group instance was overtaken by the ASG,
  which terminated the stopped node and brought up a new one mid-recycle. There
  the recycle wins you nothing that ReplaceNode does not, and races the ASG.

Because a recycle is not done when the instance is merely powered on — the OS,
kubelet and agent take minutes more to return — RecycleNode waits for the node
to become `Ready` again before the ladder advances to `verify`. On an autoscaled
group that node never comes back (the ASG replaced it), so the wait times out
with a message telling you to use ReplaceNode: the honest signal that RecycleNode
was the wrong action there.

Both restart or destroy the whole VM, so the compiler forces `approval:
Required` on them, exactly as for `Reboot`. Both are driven by the controller,
never the agent — the agent dies the moment its instance stops and could never
issue the Start that follows.

Enable it with a provider-scoped `spec.cloud` block. The provider selects which
per-provider block applies; the provider-specific settings live inside it, so a
future cloud adds its own block rather than widening a shared top level:

```yaml
spec:
  cloud:
    provider: aws
    aws:
      region: us-east-1
      # Set as the eks.amazonaws.com/role-arn annotation on the controller
      # ServiceAccount, so IRSA grants the EC2 permissions below. Omit only if
      # the role is attached to the controller some other way.
      iamRoleARN: arn:aws:iam::123456789012:role/kubeneuron-recycle
```

The `iamRoleARN` must grant an IRSA role scoped to the cluster's own instances:

```json
{
  "Effect": "Allow",
  "Action": ["ec2:StopInstances", "ec2:StartInstances",
             "ec2:TerminateInstances", "ec2:DescribeInstances"],
  "Resource": "*",
  "Condition": {"StringEquals": {"aws:ResourceTag/kubernetes.io/cluster/<cluster>": "owned"}}
}
```

Each provider declares which primitives it can perform safely: AWS advertises
both recycle (stop/start) and replace (terminate). If a playbook uses a cloud
action the configured provider does not declare — a `RecycleNode` under a
provider whose only restart is a soft reboot, say — the operator **rejects the
installation at compile time** with a clear message, rather than the controller
discovering the gap mid-incident. A provider new to KubeNeuron is a new package
plus one registry line; no workflow or controller code changes to add it.

Without a cloud provider configured at all, a `RecycleNode`/`ReplaceNode` step
fails closed with a clear message rather than reporting a node recycled that was
never touched.

On an autoscaled fleet — the common EKS case — the ladder is just cordon, drain,
replace; there is no node to uncordon because a fresh one takes its place:

```yaml
steps:
  - {name: cordon,  action: Cordon}
  - {name: drain,   action: Drain, timeout: "10m"}
  - {name: replace, action: ReplaceNode, approval: Required, timeout: "10m"}
# no verify/uncordon: the terminated node's incident closes as replaced, and
# the ASG's new node attests through the normal pipeline on its own.
```

Only on a fleet whose nodes are *not* under an autoscaler does recycle-first make
sense, with replace as the escalation rung:

```yaml
steps:
  - {name: cordon,   action: Cordon}
  - {name: drain,    action: Drain, timeout: "10m"}
  - {name: recycle,  action: RecycleNode, approval: Required, timeout: "10m"}
  - {name: verify,   action: VerifyNodeHealth, timeout: "10m"}
  - {name: uncordon, action: Uncordon}
onFailure: {escalateTo: replace-node}   # ReplaceNode as the next rung
```

## Rebooting a node from the agent container

The agent image is distroless and its own PID namespace is not the host's, so a
plain `systemctl reboot` exits 127. The `Reboot` action instead enters PID 1's
namespaces (`nsenter`, shipped in the agent image) and asks the host's init. The
managed DaemonSet already runs privileged with `hostPID`, which is what makes
this reachable.

When destructive actions are armed, the agent probes this mechanism at startup
and logs the result. A node that cannot reboot says so then — not after an
operator has approved the most destructive step a playbook has. Hosts with a
different init can replace the whole command with the agent's
`--reboot-command`; it is never derived from a playbook or action parameter.

The action is idempotent on `boot_id`: if the controller stamped the boot ID it
observed and the node has since rebooted, a retry reports success without
rebooting again.

## TLS material and its renewal

The controller and its agents authenticate to each other with mTLS. The operator
issues that material and renews it: a long-lived authority (10 years) signs
short-lived certificates (90 days), and each certificate is replaced once less
than a third of its life remains. Renewal needs no coordination because the
signer never changes — everyone already trusts it — and the workloads mounting a
replaced certificate are rolled automatically.

The asymmetry is deliberate. Rotating an authority means every party must trust
the new one *before* anything presents a certificate signed by it, which is a
multi-phase rollout that has to survive restarts. Rotating a leaf under a stable
authority is a single write. Replacing the authority therefore stays a rare,
operator-driven event.

**Bringing your own material** still works. Create the four Secrets named in
`spec.tls` before installing — from cert-manager, a corporate CA, anything — and
the operator will not touch them. It watches them instead, and raises a
`TLSMaterialExpiring` warning event when one is close to expiry, because nothing
else will:

```
Secret kubeneuron-agent-client-ca was not issued by KubeNeuron and expires
2027-07-29T10:14:00Z; nothing will renew it
```

Secrets the operator issued carry `kubeneuron.io/managed-pki: "true"`. That label
is the whole distinction: without it the material is treated as somebody else's
and is never overwritten.

!!! warning "Installations created before this existed"
    `deploy/install.sh` used to generate the four Secrets with `openssl` and
    nothing renewed them. Those Secrets have no `kubeneuron.io/managed-pki`
    label, so the operator will report their expiry but will not replace them.
    To hand them over, delete them and let the operator reissue:

    ```sh
    kubectl -n kube-neuron delete secret \
      kubeneuron-controller-tls kubeneuron-controller-server-ca \
      kubeneuron-agent-tls kubeneuron-agent-client-ca
    ```

    The controller and agents reconnect once the new material rolls out.

## Remediation scripts on nodes

`driver_reload`/`driver_reinstall`/`run_script` are binary-level action
contracts only. `hostTooling.scriptsDir` mounts operator-provisioned scripts
read-only at the agent's `--scripts-dir`. `executionMode: Enabled` arms
`-enable-destructive-actions`, but only on the nodes named by
`spec.safety.destructiveExecution.nodeSelector` (see the warning below), and a
per-device hardware GPU *reset* still refuses on virtualized instances that
have no guest PCI reset — there the cloud `ReplaceNode` primitive stands in.

!!! warning "Arming destructive execution narrows where the agent runs"
    `executionMode: Enabled` merges
    `spec.safety.destructiveExecution.nodeSelector` into the agent DaemonSet's
    own node selector, so the agent — and therefore GPU fault **detection** —
    is scheduled **only** on the armed nodes. This is deliberate: the
    destructive-capable binary never lands on a node you did not name. But it
    means arming a *subset* of the fleet silently stops detecting faults on
    every other node. In production, set the selector to match **all** GPU
    nodes you want covered, not a narrow subset; if you must arm a subset while
    keeping detection fleet-wide, run a second `DryRun` installation for the
    unarmed nodes.

## Hardware GPU end-to-end CI

Real-NVIDIA validation runs on an **ephemeral** EKS cluster (a g4dn.xlarge /
Tesla T4 GPU node beside a small CPU nodegroup), never on per-commit CI. Every
push and pull request stays CPU-only (`.github/workflows/ci.yaml`); the GPU
suite is a separate, gated target that always destroys its cluster afterward.

- **Workflow:** `.github/workflows/hw-e2e.yaml`.
- **Driver script:** `hack/hw-e2e.sh` (all the heavy logic; the YAML is thin).
- **Watchdog:** `.github/workflows/hw-e2e-reaper.yaml` runs `hack/hw-e2e.sh reap`
  on its own 30-minute schedule.

### When it runs

- **`workflow_dispatch`** — a human triggers it from the Actions tab and must
  type the phrase `RUN GPU HARDWARE E2E` into the `confirm` input. A wrong or
  empty phrase fails the run before any cloud resource is created.
- **Weekly `schedule`** — Monday 07:00 UTC, so a regression is caught even
  when nobody dispatches it.
- **Before every release tag** — running it green is a mandatory gate before
  pushing a `v*` tag. It is not wired into `release.yaml`; run it by hand (or
  confirm the latest weekly run is green) and only then tag.

A permanent self-hosted GPU runner is intentionally out of scope; if a physical
lab box ever appears it would host a nightly destructive ladder, tracked
separately.

### What it proves

1. Stands up the cluster with `eksctl`, installs the **EBS CSI addon** (a
   prerequisite — the controller's SQLite PVC never binds without it), marks
   `gp2` the default StorageClass, and installs the NVIDIA GPU operator.
2. Builds and pushes the operator/controller/agent images to ECR and installs
   KubeNeuron through `deploy/install.sh` (stays `DryRun`).
3. Injects a kernel **XID 79** into a GPU node's `/dev/kmsg` and asserts the
   incident walks cordon → drain → approval → dry-run reboot ladder, with the
   approver identity recorded in the audit trail.
4. Flips to `executionMode: Enabled` under a confined
   `spec.safety.destructiveExecution` block (node selector + the exact
   acknowledgement string) and asserts the **ReplaceNode** path closes the
   incident as replaced. A hardware GPU *reset* is impossible on a virtualized
   g4dn, so ReplaceNode is the destructive primitive exercised here.

!!! note "CSI volumes need `fsGroup`"
    The controller mounts its SQLite PVC through the EBS CSI driver, whose
    volumes are only writable when the pod sets an `fsGroup`. This is captured
    as an explicit prerequisite because a live run failed on it before the
    addon and `fsGroup` were in place.

### Required secrets, vars, and environment

| Kind        | Name                   | Purpose                                            |
| ----------- | ---------------------- | -------------------------------------------------- |
| secret      | `AWS_GPU_LAB_ROLE_ARN` | IAM role assumed via GitHub OIDC (no static keys). |
| var         | `AWS_REGION`           | e.g. `us-east-1`.                                   |
| var         | `ECR_REGISTRY`         | ECR host `<acct>.dkr.ecr.<region>.amazonaws.com`.  |
| environment | `gpu-lab`              | Add required reviewers; gates every dispatch/cron. |

No account id or credential is committed. The assumed role needs permission to
run `eksctl` (EKS, CloudFormation, EC2, IAM for the cluster's service roles),
push to ECR, and — for the sweep — terminate EC2, delete CloudFormation stacks
and EBS volumes, and delete the recycle IAM role.

### Cost and teardown guarantee

The teardown step runs `if: always()` — on success, failure, or cancellation.
It runs `eksctl delete cluster --force` and then **sweeps for leaks**: it
asserts there is no surviving cluster, no non-terminated `kubeneuron:e2e` EC2
instance, no `eksctl-<cluster>-*` CloudFormation stack, no orphaned e2e EBS
volume (a real run once leaked a 1 GiB SQLite volume), and deletes any
manually-created recycle IAM role. If it cannot delete something it fails
loudly rather than leaving a paid resource running.

The reaper is the second line of defence: it force-deletes any
`kubeneuron-e2e*` cluster whose `kubeneuron:e2e-expires-at` tag has passed, so a
wedged runner that never reaches teardown still cannot leak a cluster past its
max lifetime (`MAX_LIFETIME_MINUTES`, default 180). The job's own
`timeout-minutes: 120` bounds a single run; the reaper bounds everything else.

You can run the teardown and sweep locally against a stuck run:

```sh
CLUSTER_NAME=kubeneuron-e2e-gh42 AWS_REGION=us-east-1 hack/hw-e2e.sh teardown
```
