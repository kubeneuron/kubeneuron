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

## Remediation scripts on nodes

`driver_reload`/`driver_reinstall`/`run_script` are binary-level action
contracts only. `hostTooling.scriptsDir` mounts operator-provisioned scripts
read-only at the agent's `--scripts-dir`, but `executionMode: Enabled` is
still rejected and the operator never sets `-enable-destructive-actions`.
Do not treat these actions as available in a Kubernetes installation until
the host-runtime and hardware validation checkpoint is completed.
