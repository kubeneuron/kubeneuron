# Changelog

All notable changes to this project are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html)
with a pre-1.0 caveat: minor versions may contain breaking changes while the
API is `v1alpha1`.

## [Unreleased]

### Read before upgrading

- Agent authentication no longer asks the Kubernetes API on every request. The
  controller reuses a successful token review for up to 60 seconds and never
  past the token's expiry; after that it asks the Kubernetes API again. On
  every request it checks the Pod and ServiceAccount UIDs from the token
  against its cached view of the cluster, which follows the API through a
  watch. So an agent's access ends when the controller sees the Pod's deletion
  through the watch, or, if the watch lags, when the next token review is
  rejected by the Kubernetes API. When the check of the Pod and ServiceAccount
  fails, the controller discards the reused review and asks the Kubernetes API
  again, so a token whose Pod is gone is answered with 401, as in
  v1.0.0-rc.1.
- After a restore, the controller can pause automation on its own. It keeps
  a generation number in the workflow store and a copy in the annotation
  `kubeneuron.io/store-generation` on its Lease `<installation>-controller`,
  and compares them when it starts or becomes leader. When the store is older
  than the copy, for example after a restore of a copy made through the
  backup endpoint (the backup CronJob), the controller pauses automation,
  says so in `kubeneuronctl status` and the panel, and sends one
  `store_older` notice. Automation stays paused until you run
  `kubeneuronctl resume`. An existing pause is kept with its actor. A restore
  from `pg_dump` or a volume snapshot may not be detected; after any restore,
  check `kubeneuronctl status` as described in the restore section of
  `docs/operations.md`. With SQLite, where there is no leader election, the
  controller now creates the Lease itself. With no copy yet (a first
  installation, an upgrade from v1.0.0-rc.1) or a copy that cannot be read,
  the controller does not pause and logs one line.
- The operator grants the controller a Role with list and watch access to
  ServiceAccounts and DaemonSets in the installation namespace. Until the Role
  is available, the controller uses live API reads for these resources. It logs
  a warning when a cache has not synchronized and counts these reads in
  `kubeneuron_agent_auth_live_gets_total`.
- `install.sh --uninstall --delete-data` on the last installation now also empties the agent data directory `/var/lib/kube-neuron` on the nodes where this installation's agent Pods were running at the time of the uninstall, including the action journal and NVIDIA diagnostic bundles; it lists the nodes where this could not be confirmed. This cannot be undone: the action journal exists only on the node, so copy it first if you need the record.
- Helm chart: `helm uninstall` now runs a check and stops when a KubeNeuron installation still exists; `--no-hooks` skips the check.
- The workflow store is migrated (SQLite 0041, PostgreSQL 0032) to record
  which notification channels accepted an approval request. Approval rounds
  that are waiting for a decision at the upgrade are requested once more on
  each configured channel.

### Added

- kubeneuronctl --ca-file (or KUBENEURONCTL_CA_FILE) trusts a private CA for the controller's certificate in addition to the system roots.

- `kubeneuronctl status` shows the version of the controller that answered the request.

- For multi-zone EKS and AKS pools using `AnyPoolZone`, `kubeneuronctl pools`
  warns that a replacement may join another zone. The incident impact preview
  reports how many pods have zone constraints from persistent volumes or
  required scheduling rules. The resolution includes the counts when the
  successor joins a different zone.
- The controller now has cluster-wide `get` access to persistent volume
  claims and persistent volumes to inspect zone affinity. It reads Kubernetes
  object metadata and specs, without reading volume contents.
- A remove-first replacement on a fixed-size AKS or GKE pool records each pool resize before it is sent. After a controller restart, an AKS resize is compared with the record and never sent again. A GKE resize not yet sent is sent after the restart only if the policy would start the replace step now, and is held while remediation is paused, the policy is in DryRun or the configuration is not loaded. If the step is interrupted after the node is removed (for example by a controller restart), under a policy in Approval mode the incident needs an operator at once, since no approval can be requested for a node that is already gone. The incident, `kubeneuronctl status` and `kubeneuronctl pools` show the pool's node count and what the resize waits for. After a restart, a replace step whose GKE plan already ended restored moves on without checking the deleted node again, and the node that joined the pool is verified as its replacement. A plan whose cloud operation cannot be read ends on the pool's final state and says so. On AKS, the `kubeneuron-replacing` tag is now also removed when the delete is confirmed after a restart, including while remediation is paused, and the incident's audit trail records it. An AKS resize that the cloud accepts and then fails for lack of capacity fails the step as before; the pool is handed to an operator only if it was left changed. When the controller does not repeat a recorded delete or resize, its log says once which check held it. Tested with simulated providers only.

- After a controller restart, a remove-first replacement on EKS and AKS continues from the recorded target instead of stopping: the controller checks whether the original machine is gone and repeats the removal only for that machine. If an AKS machine with the same name cannot be confirmed as the original, the incident waits for the operator. Tested with simulated providers only.

- A retained Kubernetes agent registration now exposes `deleted_at` when a
  complete inventory observes its node missing. Recovery reports can still
  use the recorded GPU inventory after the node disappears.

### Changed

- The restore guide now includes PostgreSQL restore steps and checks for automation pauses, approval rounds and node cordons after restoring either workflow store.

- The description of the `KubeNeuronNotificationsDropped` alert, its runbook entry and the help text of `kubeneuron_notifications_dropped_total` no longer name a full queue as the only cause. They now say to check the `kind` label: `dead_letter` means delivery attempts failed, `event` or `approval_request` means the channel queue was full.

- Notifications: the controller log lines for a failed delivery attempt, a dead-lettered notification or approval request, and a full queue now name the channel (`channel`), so with several channels the log says which channel it was.

- `kubeneuronctl status` says when the CLI and the controller are different versions and links to the release of the controller's version. It compares only what the controller returned; the CLI contacts nothing else.

- Panel: when an Approve or Reject arrives after the approval round was already decided, the panel now shows the recorded decision, who made it, through which channel and when, and offers to refresh the incident. The API answer for this case carries the decision in new `X-Approval-Actor`, `X-Approval-Channel` and `X-Approval-Decided-At` headers next to `X-Approval-Conflict` and `X-Approval-Decision`.

- kubeneuronctl connection, authentication and TLS errors now say what to do next and print the underlying error on a separate cause: line. If you parse these error texts, update your parser.

- Approval requests now say when they expire and what approving, rejecting or not deciding does, in Slack, webhook, PagerDuty, the panel and kubeneuronctl incidents show. Webhook and PagerDuty messages carry both the approve and the reject command. If an approval request expires after earlier steps already changed the node, the incident now waits for the operator instead of closing as expired. If you parse the text of these notifications, update your parser.

- Agents now space out action polls when the queue is empty or the controller is unavailable, and stagger registration heartbeats. On an idle node, the agent now waits up to 15 seconds between polls (previously about 5 seconds), so a newly queued action is picked up later than before.
- `install.sh --uninstall` now stops before removing anything when the operator is not installed and the stored data is to be kept, names the operator's namespace in its closing commands, and on the last installation removes the `karpenter.sh/do-not-disrupt` annotation that KubeNeuron itself had set.

- install.sh --uninstall stops when an incident has a cloud replacement in progress (override with --skip-open-incident-check), turns back on the NVIDIA GPU Operator components that KubeNeuron had turned off on a node, and lists nodes that still have a KubeNeuron cordon or taint with the commands to remove them. Uninstall now needs jq, curl and openssl on the machine that runs it.

- After the upgrade, a KubeNeuron cordon or degraded taint whose incident is no longer in the store is kept and reported to the operator instead of being removed. Check those nodes and remove the cordon or taint yourself.

- New notice kind `store_older` when the controller pauses automation because the workflow store is older than the one it last used. The webhook payload has only `version`, `kind` and `message`, no `incident` object. PagerDuty receives a `critical` event with `dedup_key` `store-older` that is not resolved automatically.

- New notice kind `node_kept` for a kept cordon or degraded taint. The webhook payload has no `incident` object, only `node` and, when recorded, `incident_id`. PagerDuty receives a `critical` event that is not resolved automatically. Consumers that expect an `incident` object in every notice need to handle it.

- `kubeneuronctl incidents impact` lists the workloads on the node by namespace and name, the first 12 as in the panel, and shows the reasons workloads cannot checkpoint and the checkpoint groups as readable text. `--json` output, the digest and webhook notices are unchanged.

- When automatic repair stops after repeated failures, an approval decision, or missing recovery evidence, the incident now tells the operator what to check and how to close it.

- When an add-first replacement node is kept because it runs a workload or its removal cannot be confirmed, the incident names the pool, explains the possible extra charge and gives the command to record the outcome.

- Replacement incidents use the cloud provider's error code to explain capacity failures and suggest an operator action. When the code does not distinguish capacity from quota, the incident asks the operator to check both. The classification follows provider documentation and has been tested with simulated errors. These failures have not been observed on live clusters.

- After installation, use the printed port-forward and status commands. The status command reads the API token from the installation's Kubernetes Secret. The installer no longer prints the token value.

- Release images and the checksum file use Cosign 3 Sigstore bundles. The installation guide includes verification commands for both the new format and the detached signatures used by rc.1 and rc.2.

### Fixed

- While KubeNeuron replaced a node, a signal from the machine it had already removed (for example agent-down, when observability is enabled) opened a separate observation incident with its own notifications. Such a signal is now recorded in the audit trail of the incident that replaced the node.
- Backups: the scheduled-backup CronJob template did not work as shipped. It named the controller of an installation called `fleet` but the token Secret of the default installation, so it made no backup for either without further edits. Its defaults now match the installer's defaults; for another installation, one documented `sed` command sets the name and the namespace together. The CronJob and the restore helper now set the fields the Pod Security `restricted` profile requires. The CronJob also sets fsGroup, so on volume types that apply it the Job's non-root user can write to a newly provisioned backup volume.
- Backups: an interrupted download in the scheduled-backup CronJob could leave a truncated file under a dated name, and retention counted it as a copy. The Job now downloads to a temporary name and keeps the copy only after checking its length against the response and against the SQLite header; otherwise the Job fails and the existing copies stay as they were. Retention counts only complete copies. An incomplete copy left by an earlier version of the template is reported in the Job log by name and is not removed. The download time limit and the number of retries are set in the template (`MAX_TIME`, `RETRIES`).

- Notifications: an approval request that a notification channel did not accept, for example during a channel outage longer than the delivery retries, was not sent again, so the round could expire without a request reaching that channel. The controller now records which channels accepted the request of the current round and sends it again with growing pauses to a channel that has not, also after a restart, until the round is decided or expires. A channel can receive the same request more than once: when it accepted the request but answered with an error or not in time, or when the controller did not record the delivery before it stopped. Each request sent again to a channel is counted in `kubeneuron_approval_request_redeliveries_total` and logged as a warning.

- PagerDuty: an alert summary that reaches the Events v2 length limit is now cut between characters, so it no longer ends in a broken character. The approve and reject commands stay complete in the alert's custom details.

- Store: timestamps with different fractional-second precision were compared as text. Within the same second, a queued action could occasionally be picked before an older one, and time filters and retention cutoffs could misplace a record. Ordering and comparisons of the affected timestamps are now chronological. Existing values are rewritten to a fixed width once, when the controller first starts after the upgrade; the schema version does not change. Records of the operational audit log are not modified.

- The KubeNeuron CRD now installs on Kubernetes 1.29, 1.30 and 1.31. Its validation rules for Secret references named the `namespace` field unescaped, which these versions refuse to compile, so the CRD was rejected there; the rules now write it as `__namespace__` and check the same thing.

- install.sh --uninstall no longer suggests deleting the full install manifest. That command also deleted the kube-neuron namespace, with any volume claims the uninstall had just kept there, and all KubeNeuron CRDs and custom resources. The script now prints commands that remove only the operator.

- The controller no longer removes a KubeNeuron cordon or degraded taint when its store has no record of the incident that placed it (after a reinstall with a new store, or after restoring an older backup). It keeps the cordon or taint and tells the operator once, naming the node and, when it is recorded, the incident.
- When a GKE remove-first replacement needs an operator, the incident explains how to check the pool and settle the cloud operation. If the pool has been removed, the incident no longer suggests resizing it.
- Add-first: a replacement node that KubeNeuron left in the pool — because it was already running a workload, or because its removal could not be confirmed — is no longer dropped from the incident's record when a later attempt plans its own replacement. The incident now waits for the operator and names the pool (and the node, when it was kept for a workload); `kubeneuronctl resolve <id> --cloud-operation-settled --reason "<what you found>"` then closes it. If the command is run while a later attempt is still in progress, it settles only the earlier node's record, and the incident stays open.

- The pools API and `kubeneuronctl pools` explain why add-first is unavailable on a multi-zone GKE pool and advise using a single-zone pool or selecting RemoveFirst.
- The optional AMD detection source reads `amd-smi metric --json` both as a
  device array (ROCm 6.4 and earlier) and as the object with a `gpu_data`
  array that amd-smi prints from ROCm 7.0. Before, the second form gave no
  readings and the agent fell back to rocm-smi. Any other output is logged
  as an unreadable amd-smi result, never read as zero devices. Not validated
  on AMD hardware.
- The optional AMD detection source reads only the amd-smi fields that the
  amd-smi CLI prints. From amd-smi the agent reads the device list (PCI
  address, UUID), ECC error counts and hotspot temperature. XGMI link errors,
  retired pages and thermal throttling are not read from amd-smi in this
  release. Not validated on AMD hardware. The PCI address and UUID come from
  `amd-smi list --json`; if that fails, events still report and say that the
  PCI address was not read. `--amd-xgmi-link-min-delta` and
  `--amd-bad-page-threshold` stay accepted; with amd-smi they have no effect
  in this release.

## [v1.0.0-rc.2] - 2026-11-24

### Hardware evidence

On Amazon EKS, in a node pool with real NVIDIA Tesla T4 GPUs, KubeNeuron replaced a node twice under an Automatic policy, with no manual approvals: add-first, then remove-first on the node that had replaced the first. Each fault was injected as an NVIDIA XID 79 line in the node's kernel log; the GPU itself was healthy. KubeNeuron verified each new node by readiness, advertised capacity and the device inventory its agent reported, and the driver answered; device health was not checked. Separately from KubeNeuron, the run checked the GPU on each new node with nvidia-smi (Tesla T4, driver 580.178.04) and the CUDA vectorAdd sample, which passed. This was done once, on build 30396cd34b576f7ac250deaeca7f3d43a2ab4638 — the code released as v1.0.0-rc.2 (the release commit changes only the changelog and the documentation).

### Security

- Updated the controller’s transitive OIDC dependency `github.com/go-jose/go-jose/v4` to v4.1.6 to address GHSA-mgcc-cww5-fp2q, GHSA-vvm8-gf78-28g4 and GHSA-4w3g-jfj2-cw2q.

### Added

- `spec.agent.runtimeClassName` runs the agent Pods with a RuntimeClass, for
  nodes where the NVIDIA container runtime is configured but is not the
  default, such as the NVIDIA GPU Operator 25.10 or later with CDI
  (`runtimeClassName: nvidia`). Optional and unset by default; nothing
  changes for existing installations. When no agent Pod can be created with
  it, or the Pods' sandbox does not start, `GPUVisibility` and
  `kubeneuronctl status` report `AgentRuntimeClassPodsNotCreated` or
  `AgentRuntimeClassNotStarting` instead of waiting for agents, also when
  setting the field stops the rollout of a running installation. Not yet
  exercised on real hardware. The table in docs/operations.md now says, per
  node image, whether the agent sees GPUs without node-level actions.

- **`kubeneuronctl status` says when the controller has not loaded a
  published configuration.** After five minutes it prints `configuration
  published <t> ago is not loaded by the controller`.
  `GET /api/v1/runtime-config` now also returns `published`: the
  `config_digest` the operator published and when (`published_at`), or the
  `error` reading it.

- **The agent reads NVIDIA GPUs without node-level actions.** By default
  (`spec.agent.deviceAccess: NvidiaRuntime`) it asks the NVIDIA container
  runtime for the node's GPUs and `nvidia-smi`, stays unprivileged with a
  read-only root filesystem, and adds no hostPath mount for GPU access. It reports each GPU's
  model and UUID and the driver version. Where the runtime exposes no GPU, the agent reports none
  and keeps watching the kernel log; `GPUVisibility` and `kubeneuronctl
  status` report `RuntimeDidNotExposeGPUs`. `deviceAccess: None` turns it
  off. The upgrade rolls every agent DaemonSet once. Not yet exercised on
  real hardware.

- Before an incident, `kubeneuronctl pools` explains when Automatic replacement would need approval because no profile selects a node, profiles overlap, or the selected profile is absent from the policy. The policy condition `QualifiedProfilesResolved` identifies missing profile references. During an incident the hold explains what to create or change.

- Add-first replacement can use any configured zone of a fixed-size EKS or AKS pool when the remediation policy selects `replacement.successorZone: AnyPoolZone`. The default is `SameZone`. Cross-zone placement was tested with simulated providers only.

- **`kubeneuronctl pools` shows whether a pool's nodes can be replaced at
  all.** A new REPLACEMENT column, before ADD-FIRST, reports `available`,
  `unavailable — <reason> — <action>` or `unknown — <reason>` from the same
  check an incident runs, for `RemoveFirst` pools too, within the existing
  background check budget. A reason that holds every replacement (EKS node
  auto repair, a permission, no cloud provider) moves from ADD-FIRST to
  REPLACEMENT, and ADD-FIRST shows `see REPLACEMENT`. `--json` adds
  `replacement_status`, `replacement_reason_code`, `replacement_reason` and
  `replacement_action`; the existing fields are unchanged.

- A remove-first replacement on a fixed-size GKE pool (zonal group of up to
  100 instances) or a fixed AKS pool (up to 100 instances) compares the pool's members before the first change and
  after the resize. When exactly one instance is new, the resolution names it
  (`replacement_correlation: resize-diff`, with `replacement_provider_id`),
  and a second incident on the same node takes it as evidence instead of
  going to an operator. Otherwise the node is found by the pool, as before.
  No new cloud permission or tag. Exercised with simulated providers only.

### Changed

- When a reset playbook has no device ID, the incident now explains why the reset cannot run and asks the operator to check the device and resolve the incident. `kubeneuronctl incidents show` displays the reported vendor and PCI address when the GPU UUID is missing.

- `kubeneuronctl status` uses singular counts where appropriate and omits the unconfirmed reset notice when node-level actions are disabled. On NVIDIA nodes, `kubeneuronctl health` explains why Kubernetes GPU health conditions can be unknown. The JSON output retains the condition names.

- Node replacement holds now explain how to configure a cloud provider or check the node’s provider ID when an incident has no matching instance.

- Impact previews are enabled by default at approval rounds. Set `spec.safety.impactPreview.enabled: false` to opt out. Previews are advisory and workload-context resolvers remain opt-in.

- CRD field descriptions (`kubectl explain`), the `kubeneuronctl audit-events`
  help, the API's 503 for an unavailable operations service and the incident
  acknowledgement and resolution errors no longer name 0.x versions. Behaviour
  is unchanged.

- The release workflow's upgrade and rollback rehearsal now starts from
  `v1.0.0-rc.1` instead of skipping. On kind it upgrades both workflow stores
  to the new release, checks that rc.1 refuses the migrated store, restores
  the pre-upgrade snapshot and upgrades again.

### Fixed

- **A step a person approved, a step that needs no approval, and a switch to
  DryRun wait until the controller has loaded a change.** An approved
  step, or a step that needs no approval such as cordon, could
  start under the configuration being replaced, and after a switch to DryRun
  still ran for real until the reload. Now no step starts, and no step is
  parked for approval, while the loaded configuration is not the published
  one: it is held with `<step> held: the controller has not yet loaded the
  published configuration` (`held by policy <name>` under an automatic
  policy), the approval stays recorded, and the decision is taken again under
  the loaded configuration. Notification steps are not held. The held row
  and `policy-authorised` record the configuration digests in their params.
  Present in v1.0.0-rc.1.

- On Kubernetes, the next inventory summary drops the heartbeat and GPU count
  of a deleted node, including when a new node reuses its name.

- The node list and Prometheus service discovery filter retained records by
  current Kubernetes node name and UID, dropping deleted nodes and old
  registrations for reused names.

- **The record of a node with a registered agent takes the node's current
  labels.** It kept the labels the node had when KubeNeuron first read it
  from Kubernetes, or none, for as long as the node existed. The inventory
  read now updates them while the node is the one the agent registered; a
  node recreated with the same name is updated once its agent registers.
  Decisions that select nodes by label already read the cluster; this keeps
  the recorded labels, as `GET /api/v1/nodes/{node}` shows them, current, and
  the last labels of a deleted node are those it had when it went. Present in
  v1.0.0-rc.1.

- **A node recreated with the same name no longer inherits the earlier
  node's journal label or observation profile.** The GPUs an agent had
  reported made a node of that name an accelerator node for
  `kubeneuron.io/journal-dir`, so a new CPU node could get the label, and a
  journal mount failure kept the label off the new node for an hour. Both now
  apply only to the node object (UID) they were seen on. The runtime profile
  an agent observes with is chosen for the node object it authenticated on:
  if the node by that name is now another object, or its UID cannot be read,
  the request fails and the agent sends no report until it succeeds. Verification of an
  incident without a current accelerator report asks the same about the
  incident's node and holds when it cannot tell. Present in v1.0.0-rc.1.

- Corrected the AMD documentation. The operator-managed agent detects matching kernel-log events and does not enable AMD telemetry polling.

- **An automatic step waits until the controller has loaded a policy
  change.** The operator publishes a change at once and the policy's
  conditions show it, but the controller loads it from its mounted copy,
  which the kubelet refreshed a minute or more later. Until then an
  Automatic policy authorised steps under the previous revision, also after
  the change turned Automatic off or removed a qualified profile. Now the
  controller compares the configuration it has loaded with the one published
  and, while they differ or the published one cannot be read, holds the step
  with `the controller has not yet loaded the published configuration` and
  retries it once that configuration is loaded. The operator grants the
  controller `get` on the `<name>-runtime` ConfigMap only. To stop automation at once, use
  `kubeneuronctl pause`; a cloud operation that has already started may still
  complete. Present in v1.0.0-rc.1.

- **Maintenance windows and destructive-scope checks read the node's current
  labels.** A window that selects nodes by label was matched against the labels
  recorded for the node, which were not updated after its agent registered: a node
  labelled into a window afterwards was not covered, and automation could act on
  it during the window; a node removed from a window a GPUAutonomyPlan refers to
  could still pass the plan's window check. Labels are now read from the
  Kubernetes node; if they cannot be read, automation holds. Present in
  v1.0.0-rc.1.

- **After an automatic replacement, the circuit breaker no longer counts the
  replaced node.** The breaker read its pool from the nodes KubeNeuron had
  recorded, which keep a node whose agent registered after the node is
  deleted, and keep the labels it first had. A later incident in the same pool
  counted the replaced node as unhealthy and stopped at approval with
  `circuit breaker: <k> of <n> nodes in the pool are unhealthy`; in a small
  pool, on the second incident, and the breaker then stayed tripped until the
  policy was edited. A node relabelled into the pool after its agent
  registered was not counted at all. The breaker and `maxConcurrentNodesPerPool`
  now read the pool from the cluster's nodes and their current labels; a node
  still joining, such as an add-first successor not yet accepted, is not
  counted. An open recovery counts toward the per-pool limit of the pool its
  step was authorised in until it ends, also after it removed its node; the
  `policy-authorised` audit row records that pool as `pool_label` and `pool`.
  Present in v1.0.0-rc.1.

- **A replacement whose new node lists its GPUs resolves without an attested
  runtime.** With the agent reading NVIDIA GPUs through the container runtime,
  a typical install (no DCGM available to the agent, no runtime profile, a GPU
  that cannot be reset on a virtualized instance) sent the old and the new
  node's devices as `degraded`, and verification went to `NEEDS_HUMAN` with
  `New node <node> reports no accelerator devices.` although the report listed
  them. The new node now needs at least as many devices as the old node's
  agent reported (recorded as `device_count` on `replaced-node-devices`), and
  a not-ready report or fewer devices go to a person with what they say. An
  agent whose GPUs cannot be reset now records the node health probe that
  passed through `nvidia-smi` on its devices, as it already did where reset is
  available. The resolution says what was observed, for example `Accelerator
  inventory and driver probe observed; DCGM not available to the agent, device
  health not checked.`: KubeNeuron verified the new node by readiness,
  advertised capacity and the device inventory its agent reported, and the
  driver answered; device health was not checked. An agent without `dcgmi`
  now says `DCGM is not available to the agent; device health is not checked`
  instead of the exec error.

- A replacement no longer waits for an accelerator report from the new node
  when the replaced node's agent never reported NVIDIA or AWS Neuron
  accelerators, for example because the container runtime did not expose the
  GPUs to it. The new node is then confirmed by its Kubernetes readiness,
  advertised accelerator capacity and a fresh agent heartbeat, and the
  resolution says `Verification used Kubernetes node readiness and advertised
  accelerator capacity.` If the new node reports its devices anyway, they are
  checked and the resolution says so. What the old node's agent had reported
  is recorded on a `replaced-node-devices` audit row before the replacement
  starts. A node whose agent had reported, even one whose report a fault
  later emptied, still needs the new node's report.

- An incident that opened before its node could be matched to a cloud instance no longer always waits for a manual resolve: the controller checks again every minute and continues once the same Kubernetes node (same UID) is matched. The late match is recorded in the audit trail as cloud-instance-matched. If the node was re-created, or the incident did not record the node's UID, it still needs kubeneuronctl resolve.

- The `VERIFYING` quiet window now starts from when a signal last joined the
  incident, stored as `last_signal_at` (schema migration SQLite 0038,
  PostgreSQL 0029; existing rows take their `updated_at`). In v1.0.0-rc.1 it
  read the incident's last update time, and each write that was not a signal
  had to avoid moving it. Now any write may update the incident, and only a
  recurring signal escalates. Going back to v1.0.0-rc.1 after this migration
  needs the pre-upgrade backup (see `docs/upgrade.md`).
- The `kubeneuronctl health` table has a `CLASS` column, the incident class
  a fault on the row opens. Rows of one rule that differ only by class no
  longer look like duplicates.
- `kubeneuronctl incidents show` names `incidents impact` in its `approval:`
  line only when the round has an impact preview; otherwise it says why there
  is none (previews are off, or the reason this round's preview could not be
  computed) instead of pointing at a command that answers 404.
- An incident closed because its node is not in the cluster, and KubeNeuron
  had no record of the node when the incident opened, is audited as
  `node-unknown` instead of `node-replaced`.

## [v1.0.0-rc.1] - 2026-11-21

### Added

- **EKS: add-first replacement is enabled (experimental).** On a single-zone
  managed node group whose size is declared fixed
  (`kubeneuron.io/fixed-size=true`), the AWS provider now advertises add-first:
  the new node joins and is accepted before the old one is cordoned and
  removed. `kubeneuronctl pools` no longer reports EKS add-first as disabled.
  Evidence: yes, once, on pre-release builds: EKS add-first replacement
  completed on a live cluster (CPU nodes, synthetic accelerator capacity;
  verification depth: node readiness and advertised capacity). The controller
  was updated during the incident. Not repeated on v1.0.0-rc.1. It is not
  validated on accelerator hardware. When the
  old node's agent reported no devices, the `successor-ready` row now says the
  successor advertises the same accelerator capacity and that device health on
  it was not checked, instead of "Ready with its accelerators".

- **Kernel log from the node's journal, on by default (experimental).** With
  node-level actions disabled, the agent gets read-only access to one
  systemd journal directory of the node, `/var/log/journal` or
  `/run/log/journal`, and reads kernel fault records there instead of
  `/dev/kmsg`. The agent has read-only access to the node's systemd journal
  directories. It reads only kernel entries (`_TRANSPORT=kernel`), matches
  them against its fault patterns and sends only the classified event to the
  controller. It does not store, forward or log journal entries, and does not
  read entries of other transports. In every mode, including `DryRun`,
  KubeNeuron adds one label, `kubeneuron.io/journal-dir`, to accelerator
  nodes to choose the agent's journal mount. It runs no recovery action. Set
  `spec.agent.kernelLog.journal.enabled: false` to remove the label and the
  mount. The controller picks the directory from the node's OS image;
  `spec.agent.kernelLog.journal.nodeOverrides` sets it by node selector. The
  agent runs as `<name>-agent` on nodes without the label and as
  `<name>-agent-detect-journal-var` or `<name>-agent-detect-journal-run` on
  labelled ones; every agent DaemonSet carries `kubeneuron.io/agent=true`.
  `kubeneuronctl health` and the `KernelLogSource` condition say why a node
  has no kernel log (`disabled`, `os-image-not-recognized`,
  `journal-mount-failed`, `journal-unreadable`). Checked on kind; the read of
  journal files by an unprivileged Pod was checked on one EKS AL2023 node;
  other node images are from documentation, not checked. See
  [Kernel log from the node's journal](docs/install.md#kernel-log-from-the-nodes-journal).
- **An incident resolves after a cloud replacement.** After a replace step
  succeeds, the incident is verified on the node that took the target's
  place. Under add-first, that is the node the cloud operation named. Under
  remove-first, it is the only node whose UID was not in the pool before
  the delete. Where several nodes joined, the incident goes to a person and
  is not counted recovered. What is confirmed follows the accelerators the
  incident covered, and the resolution, the RESOLVED notification and
  `incidents show` name the new node. Without that evidence by the
  deadline, the incident goes to a person with what is missing. Before,
  every replacement ended in `NEEDS_HUMAN`. Tested with a simulated
  provider only. See
  [Verification after a replacement](docs/playbook-authoring.md#verification-after-a-replacement).
- **AWS Neuron faults from the agent, off by default (experimental).** The
  fault table maps three codes of vendor `aws` (`hbm-ecc-uncorrectable`,
  `sram-ecc-uncorrectable`, `unhealthy-component`) to the node's
  `accelerator-fault`, and a policy can scope a ladder to `vendor: aws`. A
  fault of the agent's AWS Neuron sysfs read opens an incident only with
  `spec.safety.acceleratorHealth.awsNeuronSysfs.enabled: true`; otherwise it
  is shown in `kubeneuronctl health` as not acted on for up to a day (read
  back from the event archive after a restart or a change of leader), and
  counted in `kubeneuron_accelerator_faults_not_acted_on_total{source,class}`,
  charted on the Health & Errors dashboard; see
  [Sources off by default](docs/accelerator-health.md#sources-off-by-default).
  A fault that came while an incident of its node was open is not read back
  as not acted on. Not validated against a live AWS Trainium or Inferentia
  node.
- **AWS Neuron device health read by the agent (experimental).** On a node
  with the AWS Neuron driver the agent reads each device's uncorrectable HBM
  and SRAM ECC counters and its failing components from the driver's sysfs
  tree, with no privilege and no AWS tool, and sends a rise or a component
  listed by two reads in a row as an AWS Neuron fault, with the device as
  `neuron<N>`. It reports the devices in an accelerator report of vendor
  `aws`, and an unreadable tree as an unavailable source. See
  [AWS Neuron device health from the driver's sysfs](docs/accelerator-health.md#aws-neuron-device-health-from-the-drivers-sysfs-incidents-opt-in).
  Not validated against a live AWS Trainium or Inferentia node.
- When add-first recovery is unavailable, the controller sends a notification with the reason at the start of each hold. A restart during dispatch can repeat the notification.

- **Recovery report per accelerator resource.** The report, its API and
  `kubeneuronctl report` count the window per accelerator resource, each in
  its own unit: GPU-hours for `nvidia.com/gpu` and `amd.com/gpu` apart, TPU
  chip-hours, AWS Neuron device-hours, from the device count each incident
  records when it opens; never summed across resources. Incidents with no
  recorded count are node-hours apart. `spec.recoveryCostRates` shows the
  saved hours as an estimated cost per resource; there are no default rates.
  Migrations sqlite 0036 / postgres 0027.
- **Degraded device-seconds per accelerator resource.**
  `kubeneuron_degraded_accelerator_seconds_total{class,outcome,resource}`
  counts each closed incident in its resource's own device, from the devices
  it recorded when it opened; incidents with no recorded count are not
  charged there but in node-seconds in
  `kubeneuron_degraded_node_seconds_device_count_unknown_total`. The overview
  dashboard shows each resource as its own series with its unit, unstacked,
  and the node-hours of incidents with no recorded count beside it. The
  report's classes count their hours per resource (`classes[].accelerator_groups`)
  and its incidents list what they recorded (`incident_details[].accelerators`).
- **Accelerator health view (experimental).** `kubeneuronctl health` and
  `GET /api/v1/accelerator-health` show what the leader last read from every
  accelerator health source, per node, source, rule and device, with the
  state, the reported reason and why: `no-signal` for a DRA device with no
  health taint, `not-read-this-term` for a source not read since this
  controller became leader, which holds the steps that take a node out of
  service. Read-only.
- **Accelerator health from Kubernetes nodes (experimental).** The controller
  reads the conditions and allocatable device counts that vendor and
  cloud components write to the Node of an accelerator node, through built-in
  profiles (`amd-instinct`, `aws-neuron`, `nvidia`, `google-tpu`), and opens
  an incident when a signal reports a fault on two consecutive reads, or, for
  fewer allocatable devices than capacity, for five minutes. New classes
  `accelerator-fault` and `accelerator-unallocatable`, bound in the shipped
  packs. A stale, unknown or missing signal is `unknown`, never healthy
  (`kubeneuron_accelerator_health_nodes`), and so is a condition with a
  reason its source does not document. Condition messages are not read.
  A node another repairer is acting on (`amd-gpu-unhealthy`,
  `karpenter.sh/disrupted`) is only observed, and one host of a multi-host
  TPU slice is never recycled or replaced alone. Signal names are taken from
  vendor documentation on 2026-11-18 and are not validated on real hardware.
  Accelerator nodes without kubeneuron-agent are now in the inventory, read
  from Kubernetes, with their labels and device counts.
- Training workers can report checkpoint completion, resume and progress over
  an opt-in HTTPS endpoint. Incident detail shows infrastructure readiness and
  subsequent training progress separately. An automatic drain policy can
  require a recent checkpoint report for each affected job. See the
  [training reports guide](docs/training-reports.md).

- **Device health from Kubernetes DRA device taints (experimental, opt-in).**
  With `spec.safety.acceleratorHealth.dra.enabled`, the controller lists the
  `resource.k8s.io/v1` ResourceSlices and reads the device taints of the
  NVIDIA (`gpu.nvidia.com`) and AWS Neuron (`neuron.aws.com`) DRA drivers as
  one more source of the accelerator health profiles. Only documented keys
  and values open a fault; other taints are counted as `ignored`, and a
  device without a taint is no signal, not health. Device taints are a
  Kubernetes beta feature in 1.36. The controller ClusterRole gains
  `resourceslices` get/list/watch only while it is on. Off by default; not
  run against a driver on hardware.

- **TPU health from Google Cloud Monitoring (experimental, opt-in).** With
  `spec.cloud.gcp.tpuInfraHealth.enabled`, the controller reads the Cloud
  Monitoring metric `compute.googleapis.com/instance/tpu/infra_health` for
  the instances of the cluster's TPU nodes, once a minute, as a second source
  of the `google-tpu` profile. `BAD_TPU_CHIP` or `BAD_SC_CHIP` on two
  consecutive samples opens `accelerator-fault`; `DEGRADED` and
  `NETWORKING_ISSUE` are counted as `ignored`; no sample, a stale sample or a
  failed read is `unknown`. Needs `monitoring.timeSeries.list`; off by
  default, and nothing is asked while the cluster has no TPU node
  (`kubeneuron_cloud_health_reads_total`). The metric is a Google Cloud Beta
  (pre-GA) feature that Google may change or withdraw. Not validated against
  a live GKE cluster; whether Google writes it for GKE nodes is not
  confirmed. Cloud Monitoring API read calls are billed to the project owner
  at Google Cloud rates.

- **Incident evidence.** Every incident now records, from the Kubernetes API
  only, an estimate of where the fault is (`this-node`, `shared` when the
  same class opens on several nodes within 10 minutes, grouped by incident
  ID, or `unknown`) with the rule that placed it, the vendor software state
  on the node (accelerator resource allocatable against capacity, device
  plugin and GPU Operator component pods) and whether the node advertises its
  high-performance network resource. A lost GPU while the device plugin is
  down is shown as a software state, not a hardware fault. Shown in the
  incident detail (`evidence`), `kubeneuronctl incident show`, the panel and
  `kubeneuron_incident_evidence_total`; an unread signal is `unknown`, never
  healthy. Evidence never starts or authorises a step.

- **Announced cloud interruptions (experimental, not validated against a live
  EKS, AKS or GKE cluster).** KubeNeuron reads interruption signals on nodes
  (GKE maintenance taint and label, the AKS `VMEventScheduled` condition, the
  AWS Node Termination Handler taints) and opens a `cloud-interruption`
  incident. Its playbook asks the node's opted-in jobs to checkpoint before
  the deadline the cloud announced (`CheckpointWarning`, no approval, not held
  by the breaker, limits or cooldowns) and then cordons the node (approval in
  `Approval` mode). It never drains or replaces the node. The steps start
  within `spec.interruption.lead` (default `10m`) of the deadline; a
  `rebalance` hint is only recorded. The incident ends when the node is gone
  or its notice is no longer present; a notice still present an hour after
  its deadline goes to a person with the cordon kept. An announced
  interruption is not counted as degraded capacity, in the report or the
  metrics, nor toward a repair policy's circuit breaker. A spot notice is about
  two minutes on AWS and shorter elsewhere, so a checkpoint is asked for, not
  guaranteed.
  The cloud pack adds the playbook and its policy. New metric
  `kubeneuron_cloud_interruptions_total`. See
  [Announced cloud interruptions](docs/cloud-interruptions.md).

- **AWS interruption queue (experimental, not validated against live AWS
  events).** With `spec.cloud.aws.interruptionQueueURL` set, the controller
  long-polls an SQS queue that EventBridge fills with the EC2 spot
  interruption warning, rebalance recommendation and instance state-change
  events and AWS Health scheduled changes, parsed from the AWS documentation
  examples (2026-11-18). A message is deleted only after its notice is
  written to the incident; an event for an instance outside the cluster is
  deleted and counted as `foreign`. The queue must be dedicated to one
  installation. A spot deadline is assumed to be the event's time plus two
  minutes. A queued notice is never ended by its absence: AWS Health
  closing that event, or the instance running again after a maintenance
  notice, ends it; a spot reclaim ends with the node. A moved Health
  deadline replaces the recorded one. Off by default; needs
  `sqs:ReceiveMessage` and `sqs:DeleteMessage` on the queue. See
  [The AWS interruption queue](docs/cloud-interruptions.md#the-aws-interruption-queue).

- **Add-first replacement contract.** A cloud provider can now prepare a
  replacement node before the affected node is cordoned
  (`cloud.SuccessorProvider`, `Capabilities.AddFirst`). Under
  `replacement.strategy: AddFirst`, the old node is drained only once the new
  node is Ready with at least the old node's accelerators and its agent has
  registered. `replacement.successorTimeout` (default `30m`, `5m`–`2h`)
  bounds the wait; on timeout an unused replacement is removed, and one that
  already runs work is kept and handed to an operator. The approval request
  says a temporary extra node will be created and billed. Progress is in the
  audit trail and `kubeneuronctl incident show`; the overlap is measured by
  `kubeneuron_replacement_overlap_seconds`. The Azure provider implements
  it for fixed-size AKS node pools and the GCP provider for fixed-size GKE
  Standard node pools in one zone (both experimental, not validated on
  accelerator hardware; start in DryRun; the live runs are described in the
  provider entries below); elsewhere `AddFirst` still holds before
  the cordon, and `RemoveFirst` is unchanged. See [Add-first replacement](docs/playbook-authoring.md#add-first-replacement).

- **Node conditions in incidents.** The panel and CLI show Kubernetes readiness and pressure at the last recorded transition. Each transition retains its observation in the audit trail.

- **Azure provider for AKS (experimental, not validated on accelerator
  hardware).** Once, on a pre-release build: AKS add-first replacement completed on a live cluster (CPU nodes, synthetic accelerator capacity; verification depth: node readiness and advertised capacity; device health on the new node was not checked). Not repeated on v1.0.0-rc.1. Once, on a pre-release build: AKS remove-first replacement on a fixed-size node pool completed on a live cluster (CPU nodes, synthetic accelerator capacity; verification depth: node readiness and advertised capacity). Not repeated on v1.0.0-rc.1.
  `spec.cloud.provider: azure` with `spec.cloud.azure` (subscription,
  resource group, cluster, the workload identity's client ID) replaces a
  GPU node by deleting its machine through the AKS agent pool API. A
  fixed-size pool is first grown by one, and the old machine is deleted
  only once AKS lists the new one, so missing quota or capacity fails
  before anything is deleted. An autoscaled pool keeps its size. Recycle
  is not available on AKS. Recovery runs only on Ready nodes with no
  recent AKS auto-repair event. Authentication is Azure Workload Identity
  only. The operator grants the controller `list` on events in `default`
  for this provider only, and itself now holds `list` there to grant it.
  See [Replacing GPU nodes on AKS](docs/cloud-azure.md) for the minimal
  role and what the step deletes.

- **GCP provider for GKE (experimental, not validated on accelerator
  hardware; start in DryRun).** Once, on a pre-release build: GKE Standard add-first replacement completed on a live cluster (fixed-size single-zone node pool, CPU nodes, synthetic accelerator capacity; verification depth: node readiness and advertised capacity; device health on the new node was not checked). Not repeated on v1.0.0-rc.1. Once, on a pre-release build: GKE Standard remove-first replacement on a fixed-size pool completed on a live cluster, with the pool's size restored (CPU nodes, synthetic accelerator capacity; verification depth: node readiness and advertised capacity). Not repeated on v1.0.0-rc.1. `spec.cloud.provider: gcp` with
  `spec.cloud.gcp` (project, location, cluster) replaces a GPU node on GKE
  Standard by deleting its instance from the node pool's managed instance
  group, a Compute Engine call GKE does not document for node pools.
  Remove-first on an autoscaled pool deletes the instance and leaves the
  size to the autoscaler. On a fixed-size pool, add-first grows a pool in
  one zone by one through GKE and deletes only named instances; a pool is
  never shrunk through GKE. Remove-first on a fixed-size pool, also across
  zones of equal size, deletes the instance and sets the pool back to its
  size through a plan recorded on the incident: after a restart the
  controller finishes it from the record, one plan is open per pool, no zone
  is ever lowered, and a pool it cannot set back safely ends as
  needs-operator with the `gcloud container clusters resize` command. New
  incident columns (SQLite migration 0035, PostgreSQL 0026).
  Recycle is not available. On pools with auto-repair, recovery runs only on
  Ready nodes with no auto-repair or other GKE operation running on the pool
  or cluster; Autopilot clusters are observe-only. Host maintenance is
  recorded and starts no step. Authentication is Application Default
  Credentials, on GKE Workload Identity Federation, only. See
  [Replacing GPU nodes on GKE](docs/cloud-gcp.md) for the roles and what the
  step deletes.

- **Cloud contract for a repair that cannot be turned off.** A provider can
  report `not-ready-only`: its cloud repairs only NotReady nodes, so
  recovery proceeds only while the node is Ready, before and after the
  provider's checks. A provider can ask for the node's recent events from
  named sources (reason and time, never the message; a failed or
  truncated read counts as unknown). A replacement that finds no capacity
  for a successor before deleting anything fails as "replacement capacity
  unavailable". A provider can refuse remove-first for a node
  (`cloud.RemoveFirstChecker`); the controller asks before the cordon and
  holds the step with the reason. AWS behaviour is unchanged.

- **Recovery estimates in Overview and the CLI.** Set
  `spec.manualRecoveryBaseline` to compare incident durations with your
  team's manual recovery time. Each estimate includes the baseline. DryRun
  results appear separately. Existing recovery report JSON fields remain
  available for API clients.

- **Cloud recovery pack on first install.** New installations use cloud policies
  in DryRun. Node-level policies require `--policy-pack bare-metal-alpha`
  (Alpha, not validated on real hardware). Repeat installs preserve existing policies.

- **Affected training jobs.** Incident details and approval previews show the
  accelerator workers observed on the node, grouped by verified Job, JobSet
  or RayJob. Workers with unknown owners remain visible. The incident retains
  its original map after the pods disappear. The controller receives `get` on Jobs, JobSets, RayClusters and RayJobs by default. Drain selection and impact previews now count positive accelerator requests as well as limits and ignore zero quantities.

- **The installer runs the metrics path.** `deploy/install.sh` writes
  `spec.observability` with the Collector, the metrics store and Grafana
  `Managed` for a new installation, after naming the third-party
  components it pulls; `--observability disabled` leaves them off. A
  re-run without `--observability` keeps an existing installation's
  block, External endpoints and Secret references included.
  `spec.observability.alertmanager` is now optional: absent means none. `--uninstall`
  keeps the metrics store's claim and prints how to erase it;
  `--uninstall --delete-data` deletes it too, only when it is labelled as
  this installation's. An installation without these blocks, including one
  upgraded from an earlier release, stays `Disabled` until you add them or
  re-run the installer with `--observability managed`. The sample runs the Managed stack. A new
  [Observability](docs/observability.md) page describes what each part
  promises, the combinations (an External Collector with the Managed store
  is not offered) and what happens when telemetry is lost.

- **Metric-alert detection with the Managed store.** The managed vmalert
  also evaluates `configs/vmalert/gpu-alert-rules.yaml` and sends what
  fires to the controller's new `/api/v1/webhooks/vmalert` endpoint (the
  Alertmanager API v2 body, the webhook bearer token): thermal, power,
  remapped-row budget, PCIe replays, a dead dcgm-exporter and a silent
  agent open incidents without an Alertmanager. The rules are plain PromQL
  and aggregate away the workload labels. The two liveness alerts fire
  only while the controller's own series still reaches the store, so a
  Collector outage opens no incident per node; `make test-alert-rules`
  checks both cases with vmalert-tool. `AlertDetectionConfigured`
  reports whether metric alerts reach the controller; with an External
  store it turns `True` when the controller accepts the first alert, which
  it records as `firstAlertAcceptedAt` in the inventory summary.

- **A kind smoke of the Managed metrics path.** The new
  `kind-integration-installer` CI job installs with the installer's
  defaults on synthetic DCGM input (CPU-only, no GPU) and checks that every
  Observability condition is `True`, the store runs with fsGroup 65532 on
  its claim, every dashboard series the components export reaches the
  store, no stored GPU series keeps a workload label, and vmalert's
  thermal-throttle alert reaches the controller while no liveness alert
  fires. Series that appear only on an event (incidents, steps, approvals)
  are listed, not required. The Managed pods now relabel a volume only
  when its root's group differs, so a large store starts without a full
  ownership pass.

- **Managed Grafana.** `spec.observability.grafana.mode: Managed` runs the
  official Grafana image (AGPL-3.0, unmodified, pinned by digest) with the
  store as a read-only datasource and the six dashboards provisioned,
  behind a NetworkPolicy that admits only the installation namespace, with
  anonymous access, sign-up, snapshots, public dashboards, the plugin
  catalogue and usage reporting off. The admin password is generated once
  into `<name>-grafana-admin`, which is deleted with the installation. With
  an External store `grafana.datasource` names the query address and an
  optional `Authorization` header Secret. `ObservabilityGrafanaReady`
  reports it. The legacy board's "Controller up" and "Agents down" panels
  also work on the OTLP path, which has no `up`.
- **Data and privacy page.** Documents agent privileges, operational data
  retention, monitoring destinations and the project security contact.

- **Recording rules with the Managed store.** A Managed store comes with a
  vmalert that evaluates `configs/vmalert/gpu-recording-rules.yaml` into
  it; the dashboards' `kubeneuron_gpu_*` series exist without a separate
  rule setup. `ObservabilityRulesReady` reports it. With an External store the
  rules are the user's to load.

- **GPU metrics from the existing dcgm-exporter.** A Managed collector
  scrapes the dcgm-exporter already in the cluster (the GPU Operator
  Service `nvidia-dcgm-exporter` by default, configurable in
  `spec.observability.collector.dcgmExporter`) and reports
  `ObservabilityGPUMetricsReady`. The workload labels `pod`, `namespace`,
  `container` and `pod_uid` are dropped unless `keepWorkloadLabels` is set.
  KubeNeuron does not install dcgm-exporter.

- **Managed metrics path.** `spec.observability.collector` and
  `spec.observability.store` take `Managed`, `External` or `Disabled` (the
  default when left out). Managed, the operator runs an OpenTelemetry
  Collector and a single-node VictoriaMetrics on a PersistentVolumeClaim
  that the operator never deletes (delete it by hand), both pinned by
  digest and behind NetworkPolicies, and reports
  `ObservabilityCollectorReady` and `ObservabilityStoreReady`; the
  controller and agents export OTLP with the node name as a resource
  attribute. External endpoints take their `Authorization` header from a
  Secret. `spec.observability.victoriaMetrics` is deprecated in favour of
  `store` and is not used as a metrics destination. The operator now needs
  NetworkPolicy access and delete on the Deployments, Services, ConfigMaps
  and ServiceAccounts it owns.

- **Recovery modes per policy: dry-run, approval and automatic.**
  `GPURemediationPolicy.spec.mode` selects who authorises a step that stops
  work: `DryRun` simulates the policy's incidents, `Approval` (the default for
  a policy without a mode, unchanged behaviour) waits for a named person, and
  `Automatic` runs the steps without a person within `spec.automatic`. An
  automatic policy must name its node pools and carries its limits (nodes at
  once, per pool, per-node cool-down, checkpoint wait, and a circuit breaker
  with an absolute floor that hands steps to a person when too much of a pool
  is unhealthy and stays tripped until the policy changes), who it declares
  enabled it and when, and the acknowledgement "I understand that automatic
  mode drains and replaces nodes without asking". `spec.replacement.strategy`
  (`AddFirst` by default, `RemoveFirst` only when named) and
  `spec.recycle.enabled` (off by default) bound how a node is replaced, in
  every mode: a replace step under `AddFirst` is held until the provider
  supports it, and a recycle the policy does not enable, or the provider does
  not allow, is skipped (`recycle skipped: …` in the audit trail) and the
  ladder continues with its next step or escalation. Both are checked before
  the first disruptive step: a replace under `AddFirst` anywhere on the path
  (with its escalations), or a recycle-only path without `recycle.enabled`,
  holds the incident before the node is cordoned or drained. A skip is not a
  failure or an escalation and does not count against the escalation cap; a
  recycle-only rung is skipped before its cordon. Automatic policies authorise only cloud recovery
  ladders.
  Every automatic step is audited as `policy:<name>` with the declared
  enabler, the policy generation and the time; `kubeneuronctl incident show`
  lists who approved or which policy authorised each executed step, and
  `kubectl get gpolicy` shows the mode and the effective mode. A step needing
  approval still cannot start without a person's decision or the live
  automatic policy, and a pool the provider has not confirmed is unavailable
  in every mode. Node-level actions (GPU reset, reboot, driver reload or
  reinstall, scripts) are alpha and are never authorised by an automatic
  policy; they always wait for a person. Automatic mode is never enabled by default, by the installer
  or by a sample.

  **Upgrade note:** a policy whose playbook replaces or restarts a node
  through the cloud API holds that incident before the cordon until it names
  `replacement.strategy: RemoveFirst` (until add-first replacement exists) or
  `recycle.enabled: true`.

- **Which kernel log a source reads.** An agent's available `nvidia-kmsg`
  and `amd-kmsg` signal sources carry an optional `via`, `journal` or
  `kmsg`, and `kubeneuronctl health` names it in the row's WHY. An older
  agent sends none and its row reads as before.

### Fixed

- The release install manifest keeps the operator's `root-events` Role and
  RoleBinding in `default`. The kustomize build moved them into
  `kube-neuron`, so an installation from the manifest with
  `spec.cloud.provider: azure` failed to reconcile (the operator could not
  grant the controller `list` on node events), and the operator could not
  record events on the KubeNeuron object. Installation from a repository
  checkout and the Helm chart were not affected. CI and the release workflow
  now check each object's namespace in the manifest.
- `kubeneuronctl approve` and `reject` print `approve: <id>` and `reject: <id>`
  instead of `/approve: <id>`.
- The dry-run section of `kubeneuronctl report` no longer says
  "closed with no policy bound to the class" for every resolved incident
  without a verified simulated repair. The row is now "closed unverified"
  and names the cases it counts: no policy, a playbook that only observes, or
  a ladder a person closed before it verified.
- `kubeneuronctl incidents show` prints the current approval round while an
  incident is `AWAITING_APPROVAL`, so `--round` can be used without a
  notification channel.
- An incident on a node KubeNeuron had no record of when it opened, such as
  the quickstart's `quickstart-demo`, is no longer closed "as replaced" once
  the controller sees the node is not in the cluster; the close reason says
  that KubeNeuron had no record of the node.
- On AKS and EKS too, a replacement held by its permission check names the
  missing permission, or the error code the cloud answered with, and what to
  do, instead of showing the cloud's error, which can carry the subscription
  and resource group (Azure) or the role's ARN (AWS); that error goes to the
  controller log. The three clouds now answer the operator the same way.
- On GKE, a replacement held by its permission check now says which Google
  API is disabled or which permission is missing, and what to do, instead of
  showing Google's error, which names the project; that error goes to the
  controller log. `kubeneuronctl pools` runs the same check, so a pool no
  longer shows as available while every replacement in it would hold. The
  GKE guide lists the APIs to enable, including Cloud Resource Manager, and
  the disks a deleted cluster leaves behind.
- A replacement whose cloud operation settles after the incident has entered
  `VERIFYING` (on GKE the pool's resize back to its size finishes there) no
  longer goes to `NEEDS_HUMAN` with "verification failed: signal recurred
  during quiet window". Closing the replacement plan, or an operator's
  acknowledgement, while the incident verifies is no longer read as a new
  signal; a signal that does recur still escalates.
- A remove-first replacement of a node whose agent does not inventory its
  vendor's devices (for example AMD without a device adapter) no longer goes to
  `NEEDS_HUMAN` with "The incident recorded no accelerators to compare new
  node … against": the accelerator resources the old node advertised are
  recorded with its pool, and the new node is compared with them, at the
  depth of node readiness and advertised capacity. A verification that waits
  now says why on a `verify-waiting` audit row in `incidents show`, instead of
  only in the controller's debug log.
- **EKS: a default node group no longer blocks replacement, and every hold says
  why.** A managed node group without `nodeRepairConfig`, which EKS reports
  for groups created without the setting, reads as node auto repair off, the
  EKS default; it used to hold every replacement. The settling check no longer
  uses the group's `modifiedAt`, which EKS moves without an update and which
  kept the 30-minute window from ever closing. It now uses the update history,
  and an update that only changed the group's size does not open the window.
  `kubeneuronctl incidents show`, the incident's audit and the pool's row in
  `kubeneuronctl pools` give the reason and what to do, or when the check
  passes; the controller log carries the AWS error with the incident ID. See
  [Add-first replacement on EKS](docs/cloud-aws.md#when-ekss-own-repair-holds-a-replacement).

- **EKS: add-first waits for the Auto Scaling group to start the launch.**
  The group starts the new instance some seconds after EKS reports the size
  update done. KubeNeuron used to read the empty activity history in that gap
  as an unconfirmed launch and stop the replacement for the operator, while
  the additional node was already on its way. It now waits up to five minutes
  for the launch activity before it hands the replacement over, and keeps
  waiting while the new instance is pending and its launch runs or has
  finished.

- **Add-first accepts a successor whose agent inventories no devices.** When
  the agent of the old node reported no accelerators, as for a vendor it does
  not inventory, the successor's agent was still asked for some, so the
  replacement never completed. The successor is now ready when it is Ready,
  has the old node's allocatable accelerator resources and its agent has
  registered since the request; the health of its devices is not checked in
  that case. `kubeneuronctl incidents show` says whether the wait is for the
  agent's registration or for its accelerators.

- **AKS and GKE: a fresh installation with a cloud provider becomes Ready.**
  The cloud policy pack ships an optional recycle ladder that no policy uses.
  The operator checked every playbook against the provider's capabilities,
  and only AWS can recycle a node in place, so on `azure` and `gcp` the whole
  configuration was rejected and the installation never became Ready. Only
  playbooks a policy reaches, directly or through `onFailure`, are checked
  now; a policy that reaches a recycle step is still rejected on those
  providers.

- List endpoints of the operator API answer an empty list as `[]` (or
  `"items": []`), not `null`.
- Concurrent live repair incidents on the same node share one repair owner. The
  controller identifies the node by its pinned cloud identity or the Kubernetes
  Node UID recorded when the incident opened. Waiting incidents show the owner ID before
  approval, drain or replacement. A GPU reset and a disruptive node step on the
  same node wait for each other to finish. SQLite migration 0037 and PostgreSQL
  migration 0028 persist ownership across controller restarts. After checking
  an unconfirmed replacement in the cloud provider, operators can record its
  completion or cancellation on the owning incident with
  `kubeneuronctl resolve <id> --cloud-operation-settled --reason "<what you verified>"`.

- The recovery report's GPU-hour fields and `kubeneuron_degraded_gpu_seconds_total`
  count GPUs only (`nvidia.com/gpu`, `amd.com/gpu`): an incident on TPU chips
  or AWS Neuron devices is no longer charged there as one GPU; the new
  per-resource fields count it in its own unit.

- The recovery report's GPU-hour fields and
  `kubeneuron_degraded_gpu_seconds_total` count a node-scoped incident with
  the GPUs it recorded when it opened, not the node's inventory when the
  report or the metric is read, so they agree with `accelerator_groups` after
  a node's inventory changes. Incidents opened before the record existed are
  counted from the inventory as before.

- **Re-running the installer no longer replaces what an installation
  has.** `deploy/install.sh` on an existing installation used to generate a
  new operator API token, webhook token and panel password and re-apply the
  whole KubeNeuron, which locked out clients and the Alertmanager or
  vmalert holding the old tokens and reset edits such as the execution
  mode. It now keeps the three Secrets and the KubeNeuron's spec, updating
  only the component images and an explicit `--observability`, and its
  summary says the token and sign-in were kept instead of printing them as
  new. TLS material was already the operator's and is untouched.

- **A dead GPU exporter no longer starts recovery (behaviour change of the
  dependency profile).** `GpuExporterDown` used to open a `driver-hang`
  incident, which the baseline pack bound to the in-place reset ladder, so
  a stopped dcgm-exporter on a node with an idle GPU could end in a reset
  and, by escalation, a drain. It now opens `exporter-down`, a monitoring
  gap, at severity warning instead of critical (the alert is warning on
  both metric paths), bound to observe in every shipped pack. The
  controller never runs a step for `exporter-down` or `agent-down`, in any
  mode and whatever policy binds them, and says so in the incident, the CLI
  and the panel. Neither opens an incident for a node that is no longer in
  Kubernetes or was replaced under the same name.
  `driver-hang` remains the class of an amdgpu ring timeout.

- **kmsg watcher cancellation on macOS.** The kernel-log reader opens its
  source non-blocking, so cancelling the watcher wakes a reader parked on an
  empty FIFO on darwin as it already did on Linux; the FIFO test now passes
  on both.
- Cloud recovery checks provider eligibility before requesting approval or
  starting a ladder. Missing identity or repair evidence holds execution;
  incident details and the CLI show the last recorded eligibility assessment.
  Cloud verification does not treat an old-node heartbeat as replacement proof.

- Cloud recovery incidents are no longer resolved merely because the old node
  disappears. Cleanup preserves pending verification across playbook reloads
  using durable cloud-step history, and holds when that history is unavailable.

- Installer uninstall retains SQLite state and audit history by default, waits
  for operator acknowledgment, and permits reuse on reinstall. Deleting state
  requires `--delete-data` with confirmation (or `--yes`). Uninstall only removes
  policy/playbook objects that reference its own installation, including legacy
  objects without the new installation label. Upgrade the operator before using
  the new retention-aware installer; otherwise uninstall stops before deletion.

### Security

- **Supply chain: signatures checked, inputs pinned.** `hack/verify-release.sh`
  now requires cosign and verifies the checksum file's signature and every
  image digest in `images.txt` against the exact release-workflow identity
  for the tag (`.github/workflows/release.yaml@refs/tags/<tag>`), instead of
  any identity under the repository and instead of passing with "signature not
  checked" when cosign was missing. Every GitHub Action is pinned to a full
  commit SHA, and the Go builder, distroless bases, CI service and hw-e2e
  helper images to multi-arch digests; `hack/verify-pins.sh` (in `make lint`
  and CI) fails on an unpinned action or image tag. CI now runs shellcheck
  v0.11.0 (pinned by checksum) over `hack/*.sh`, matching local `make lint`.
  Images stay built without buildx provenance attestations, as before; no
  provenance is claimed. Every image now ships `/licenses/BASE-IMAGE-PACKAGES`:
  the Debian packages of its pinned distroless base (glibc under LGPL in the
  agent's), with versions, the licences their copyright files declare and
  their Debian source locations, regenerated by
  `hack/base-image-packages.py` and checked against the pinned digests in
  `make lint` and against the images in `make verify-image`. Pull requests
  must have every commit signed off by its author (DCO), checked by
  `hack/verify-dco.sh` in its own workflow, documentation-only pull requests
  included.
- **Licence gate in CI.** The licences of all Go dependencies are checked in
  CI. `make verify-licenses` (part of `make lint`) now records the SPDX
  identifiers that each Go dependency's legal files match in
  `third-party-inventory.json`, detected from the licence text itself, and
  fails, naming the module, unless every one is on the allowlist (Apache-2.0,
  BSD-2-Clause, BSD-3-Clause, MIT, ISC, 0BSD, Zlib, BSL-1.0, and the Go patent
  grant beside BSD-3-Clause). GPL, LGPL, AGPL, MPL, proprietary or
  unrecognised text stops the build, and so does a module with no licence
  text at all; the one reviewed exception is `modernc.org/memory`'s logo
  attribution file, bound to its exact content. The base images and the
  `nsenter` we build into the agent image are not covered by this gate. The
  Helm chart now declares `artifacthub.io/license: Apache-2.0`.

- **The agent image no longer ships BusyBox.** Its `nsenter` helper is
  toybox (0BSD), statically linked against musl (MIT). The Debian base image
  still provides glibc (LGPL) as before. `nsenter` (the one host tool the
  agent needs, to reach the host's init through PID 1's namespaces) is built
  in `build/Dockerfile` from a checksum-pinned source as a single static
  binary, natively for amd64 and arm64; the per-release BusyBox source archive
  is gone with it, and the agent image's licence label reads
  `Apache-2.0 AND 0BSD AND MIT`. `make verify-image`
  fails on any BusyBox or uClibc file and runs the real
  `nsenter -t 1 -m -u -i -n -p --` in the built image. The BusyBox and
  uClibc-ng sources for the 0.x agent images are kept and are available on
  request (open an issue in this repository).

- Cloud identity changes terminate the old incident as SUPERSEDED without
  claiming recovery. Open-incident indexes permit fresh signals to create a new
  incident; retained cordon ownership is preserved. Guarded recycle verifies a
  new Ready boot of the pinned target and atomically records its updated pin and
  audit before further cloud actions. GPU-hours reports separate superseded
  targets from recovered capacity.

- **A live file configuration must name the nodes destructive steps may run
  on, and file playbooks are decoded strictly.** With `dry_run: false`, an
  empty `destructive_execution_node_selector` used to mean "every node"; the
  controller now refuses to start with it, and a reload is rejected with the
  running configuration kept. Allowing every node takes an explicit
  `destructive_execution_all_nodes: true`. File playbooks now reject unknown
  keys and extra YAML documents, so a misspelt `approve: required` is an error
  instead of a step without approval.

  **Upgrade note:** a hand-written file config with `dry_run: false` and no
  selector stops the controller from starting; add the selector (or the
  explicit all-nodes entry). A file playbook with an unknown key no longer
  loads; fix the key. Operator-managed installations are unaffected.

- **PostgreSQL: one live lease per node, and two related races closed.**
  Two agents' claims for one node that picked different actions (a plain
  claim and the restorative claim, for example) could both win on
  PostgreSQL, because the "no lease is held on this node" check read the
  statement's snapshot. Claims for one node now run one at a time under a
  transaction-scoped advisory lock. Two first accelerator reports for a node
  arriving at once no longer fail with a duplicate-key error; the newer one
  wins. An idempotency key whose previous use has expired now starts a new
  request instead of replaying the old result. SQLite behaviour is unchanged.

- **Pause and approval decisions now hold in every case.** An approved step
  no longer starts while its node is paused: the pause is checked before every
  step, including one resumed from approval, and the approval stays recorded
  until the pause is lifted. If the pause state cannot be read, the incident
  holds instead of running. The first decision of an approval round is final:
  a second approve or reject is refused with `409`, and a later approve can
  never override a reject. A request older than the approval TTL can no longer
  be decided, and a decision recorded after the expiry is ignored.

- Cloud recycle and replace execution now require the incident's pinned node
  identity and a guarded provider mutation contract. Missing or incomplete
  identity blocks execution; the controller no longer resolves a replacement
  target from the current node name at dispatch. Providers without the repair
  evidence contract remain unavailable for these actions. Eligibility is checked
  again before step admission and after checkpoint coordination, before cloud
  disruption. A refusal holds the current step instead of escalating.

- **File playbooks can no longer restart or destroy a node without a human.**
  The controller now refuses, at start and on reload, a playbook step with
  `agent.reboot`, `platform.recycle_node` or `platform.replace_node` that does
  not set `approval: required`, the rule the operator's compiler already
  applied to CRD playbooks; at execution time such a step always parks for
  approval, whatever the playbook text says. The shipped file ladders in
  `configs/playbooks` now ask for approval at the same steps as the CRD pack:
  the drain in `drain-and-reset`, `reboot`, `rma`, `fell-off-bus` and
  `driver-reinstall`, the eviction in `workload-restart`, the reset in
  `gpu-reset` and `reset-when-idle`, and the reboot in `driver-reinstall`
  (previously unapproved). A test keeps the two sets in agreement.

  **Upgrade note:** a hand-written file playbook with an unapproved reboot,
  recycle or replace step now stops the controller from starting; add
  `approval: required`. Installations that ran the shipped file ladders with
  `dry_run: false` will now see approval requests where they did not before.

- **Notification delivery errors no longer carry the endpoint secret.** A
  failed Slack, webhook or PagerDuty delivery used to log the full endpoint
  URL on every retry and in the dead-letter record, and for Slack that URL is
  the credential. Errors now keep only the scheme and host; path, query,
  userinfo, the webhook bearer token and the PagerDuty routing key are
  removed. The asynchronous delivery worker redacts again before it logs, for
  notifiers that do not do it themselves.

- **Cookie-authenticated mutations must come from the panel's own origin.** A
  state-changing operator request authenticated by the session cookie, and
  `POST /api/v1/logout`, is refused with `403` when `Sec-Fetch-Site` is not
  `same-origin`, or when its `Origin` (or `Referer`) names another host,
  including a sibling origin of the same site that `SameSite=Lax` does not
  stop; a request with none of these headers is refused as well. Its body must
  be `application/json` (a YAML type for a candidate upload), so a
  `text/plain` or form body is refused with `415`. Bearer-token requests (CLI,
  service accounts) are unchanged. Behind a reverse proxy, `X-Forwarded-Host`
  counts as the request host when proxy headers are trusted.
- **The controller configuration fails closed.** A policies file that omits
  `safety.dry_run` now runs in dry-run; live execution needs an explicit
  `dry_run: false`. The policies file and the maintenance-window, signal-mapping
  and node-config files are decoded strictly: an unknown key (for example a
  misspelt `dryRun`) or a second YAML document is an error at start-up, and a
  reload that hits one is refused and keeps the running configuration. Absent
  flap settings take the CRD defaults (3 in 24h) instead of switching flap
  detection off, and a negative approval TTL, flap setting or verify quiet
  window is rejected.

  **Upgrade note:** hand-written configuration files with keys the controller
  does not know now stop the controller from starting; remove or correct them.
  Configuration compiled by the operator is unaffected.

### Fixed

- Grant the operator status permissions for maintenance windows and node
  configurations in both installation manifests and Helm, so these resources
  can reconcile and update the runtime configuration. Also allow deletion of
  the operator-owned legacy detect DaemonSet during reconciliation.
- **An agent action can no longer run past its lease.** The lease that
  authorizes an action is not renewed, so once it expires the controller may
  hand the action out again. The agent now runs the action under a deadline
  at the lease expiry minus the result-reporting grace, instead of the
  action's full timeout from the moment it starts. An action whose lease
  leaves less than 10 seconds, or a destructive action whose lease no longer
  covers its timeout (typically one recovered after an agent restart), is not
  started and is reported as not executed. A device reset, reboot, host
  service change or script is also refused when the deadline passes during
  its preflight, so a stalled check cannot be followed by a late side effect.
- **Operational mutations are atomic.** Every mutation of an operational record
  (candidate upload and revocation, policy preview, health-check request and
  cancellation, simulation, simulation-to-incident, and every autonomy plan
  lifecycle step: create, attach simulation, approve, pause, rollback, resume)
  now claims its idempotency key, changes the record and appends its audit
  event in one transaction, as runtime-contract qualifications already did. A
  failed mutation leaves no key behind, so a retry with the same key executes
  for real instead of answering as a replay of a change that never happened
  (an emergency pause or rollback could be reported as applied when it was
  not), and a change can no longer commit without its audit event. The
  reconciler's store-only transitions (plan moves, effect admission, dispatch
  claim, outcome and block, health-check completion) are one transaction
  each. A store that cannot commit atomically is refused for these writes.

### Changed

- Add-first on an AKS node pool configured with several availability zones is now refused before any incident step. Previously the new node could start in a different zone from the node it replaced. Use a single-zone node pool, or set `RemoveFirst` in the remediation policy that applies to these nodes. For GKE, the refusal now explains that add-first would add a node in every zone of the pool.

- When a recovery policy hold lifts, the audit records `recovery-available-again`. Existing `recovery-available` entries remain readable.

- In the `pkg/cloud` module, callers must persist `RequestedAt` before requesting a successor from a saved plan. A plan without this timestamp can be released without contacting the cloud.

- Agent reports from multiple GPUs can join one open node incident of the
  same class. The incident audit records the reporting GPU UUIDs.

- **The default rule pack is plain PromQL.** The recording rules and
  `gpu-rules.yaml` used MetricsQL functions (`bitmap_and`, `label_del`,
  `median`) that only vmalert and VictoriaMetrics accept, so Prometheus
  refused to load those files. The clock-event bits are now read with
  `floor(x / bit) % 2`, a fixed label is dropped with `label_replace`, and
  the fail-slow peer median is `quantile(0.5, ...)`; series names, labels
  and when an alert fires are unchanged. The dashboards were already plain
  PromQL and are now checked too. CI (`make check-rules`) loads every rule file and every panel
  expression with promtool (Prometheus v3.16.0) and with vmalert v1.147.0
  `-dryRun`, and runs one set of rule unit tests in both promtool and
  vmalert-tool. This is a check of the files, not a run against a live
  Prometheus. A Go test also fails on a MetricsQL-only function in the
  rules or the dashboards.

- Existing cloud agents with node-level actions disabled lose local kmsg access.
  Numeric NVIDIA XIDs require DCGM field 230, which the shipped exporter profile
  omits. Polling its last value can miss repeated XIDs and the `xid-app` threshold.
  Without DCGM, numeric XIDs are unavailable. AMD kernel ring timeouts and PCIe
  errors are unavailable. Accessible AMD SMI tooling still reports ECC and page retirement.

- `kubeneuron_incidents_recovered_total` now requires an audited live
  remediation followed by verified resolution. Dashboards and alerts using
  this counter may show fewer recoveries after upgrade because manual and
  observe-only closures are excluded.

- Existing bare-metal installations switch to observation on upgrade until the
  operator and the installation both enable alpha node-level actions.
- **Automatic replacement now needs `automatic.qualifiedProfiles`.** A
  policy in Automatic mode replaces a node without approval only when the
  node's accelerator runtime profile is listed in
  `automatic.qualifiedProfiles` and the fault is located on that node. With
  an empty list, an unlisted or unknown profile, or a fault that is shared or
  cannot be located, the replacement waits for approval and the incident
  states the reason. Existing Automatic policies stop replacing nodes on
  their own until profiles are listed.

- **AWS checks replacement permissions.** Replacement requires a successful
  permission probe for `ec2:TerminateInstances` on the pinned instance.
  Replacement on EKS has not yet been validated against a live cluster.
  Start in `DryRun` mode.
  Replacement calls `TerminateInstances` first. The Auto Scaling group then
  launches a successor. Adding a node before termination is not yet supported.
  Stop/start recycle stays disabled for managed groups. To inspect native repair for an EKS managed
  nodegroup, the controller role needs `eks:DescribeNodegroup`,
  `eks:ListUpdates`, `eks:DescribeUpdate`,
  `autoscaling:DescribeAutoScalingInstances`,
  `autoscaling:DescribeAutoScalingGroups`,
  `autoscaling:DescribeScalingActivities` and `ec2:DescribeInstanceStatus`.

- **DCGM is no longer bundled in the agent image.** Removed NVIDIA's packaged
  `dcgmi`, its libraries and added `libgcc_s` dependency after license review.
  The agent uses the narrower `nvidia-smi` health source unless a working client
  is supplied; missing runtime attestation keeps destructive actions blocked.

- CI now tests linux/arm64 with native race tests, kind integration, and
  image verification alongside linux/amd64.

- **Adapter contract in `pkg/`.** The accelerator adapter contract
  moves from `internal/accelerator` to `pkg/accelerator`, now its own Go
  module (standard library only, tagged `pkg/accelerator/vX.Y.Z`). The cloud
  provider contract moves from `internal/cloud` to `pkg/cloud`, also its own
  Go module (standard library only, tagged `pkg/cloud/vX.Y.Z`). The NVIDIA and
  AWS implementations stay in `internal/`. Every source file carries an
  `SPDX-License-Identifier: Apache-2.0` header, checked by
  `hack/verify-spdx.py` in `make lint` and CI.
- **The accelerator report protocol accepts any well-formed vendor id**:
  lowercase letters, digits, and hyphens, at most 32 characters
  (`types.AcceleratorVendor.Valid`, and the same rule for
  `accelerator.Vendor.Valid`, which previously accepted any non-blank string).
  The four vendor constants stay. An adapter for a vendor outside them (AWS
  Trainium is on the roadmap) now posts accelerator reports, and the
  controller verifies that vendor's device-scoped incidents against a current
  report instead of the heartbeat alone. The vendor lists in the
  `GPURemediationPolicy` `match.vendor` enum, the `AcceleratorRuntimeProfile`
  `vendor` enum, and the HTTP API readiness and runtime-contract filters are
  replaced by the same format check. A profile for a vendor other than NVIDIA
  still admits no action: it needs a ready report, which only the NVIDIA
  preflight produces, and physical reset stays withheld from adapters without
  a preflight.
- Toolchain: `go.mod` `toolchain`, the build image, and every workflow
  `go-version` move from go1.26.10 to go1.26.11, which fixes
  [GO-2026-6859](https://pkg.go.dev/vuln/GO-2026-6859) in `net/http`.
  `govulncheck` failed on it in CI. No Go source changed.
- **Only an authenticated agent names an incident's vendor.** A `vendor`
  annotation on an Alertmanager alert or a manual trigger is no longer read as
  the incident's vendor, and the controller rejects an agent event whose fault
  names a malformed vendor id (as a poison event, `invalid-vendor`).

### Added

- **OTLP metrics export from the controller, agent and operator.** Each
  process can push the series it serves on `/metrics` to an OpenTelemetry
  Collector over OTLP/HTTP (protobuf), through the Prometheus bridge
  from the same registry, so names and labels are identical and nothing is
  counted twice. Off unless `OTEL_EXPORTER_OTLP_ENDPOINT` (or the
  metrics-specific variable) is set; the standard `OTEL_*` variables
  configure it. A failed export is dropped and counted in
  `kubeneuron_telemetry_export_failures_total`, never blocks the process, and
  the final flush at shutdown is bounded to 5 s. A test fails the build if a
  metric label could carry a person's identity, a credential, a URL or a
  workload name (namespace, pod, job).

- **Accelerator adapter registry.** `accelerator.Register`, `Build`,
  `Registered`, and `Vendors`, modelled on the cloud provider registry.
  Built-in adapters are listed in one registration file,
  `internal/accelerator/adapters/adapters.go`.
- **The agent drives every enabled adapter.** The agent builds each
  registered adapter whose vendor is enabled (`-adapter=<vendor>`, options
  with `-adapter-option=<vendor>.<key>=<value>`; NVIDIA is still enabled by
  `-nvidia-observation` with its existing settings). For each adapter it
  posts one accelerator report per node and vendor from the adapter's
  inventory, capabilities, and health checks, and sends its polled and
  streamed detections as vendor-neutral fault events. NVIDIA preflight is
  an optional extra interface: an adapter without it is observation-only
  (its report is never ready and never declares a physical-device reset).
  An adapter that fails to build fails closed for its own vendor only: it
  posts a not-ready report and no longer stops the agent from starting.
- **`spec.agent.adapters` on the KubeNeuron CRD.** An installation enables
  registered accelerator adapters without operator changes: a list of
  `{vendor, options, hostTooling}`, one entry per vendor (duplicates and
  malformed vendor ids are rejected by the API server and the operator). The
  operator renders one `--adapter` per entry and one `--adapter-option` per
  option, in a stable order; `--nvidia-observation` is unchanged. Each
  adapter's `hostTooling` mounts its host binaries, libraries, and device
  nodes (for example `amd-smi` and `/dev/kfd`) read-only under
  `/host/adapters/<vendor>/`. The operator's new `--default-agent-adapters`
  flag, set by the Helm value `agentAdapters`, adds the same entries to every
  installation. See [operations](docs/operations.md#accelerator-adapters-in-the-agent).
- **`accelerator.ErrNotPresent`.** An adapter factory returns it when the
  node has none of its vendor's hardware or tooling. On a mixed fleet the
  agent then skips that vendor on that node quietly: one info line at
  start-up, no report, no warnings.
- **Tests for the `pkg/cloud` registry** (registration, duplicates, lookup of
  an unregistered provider), now that it is a public contract.

## Earlier releases

0.x releases are no longer published, and their notes are not kept in this file. Upgrading from 0.x is not supported; see [Moving from 0.x to v1.0](docs/upgrade.md#moving-from-0x-to-v10).
