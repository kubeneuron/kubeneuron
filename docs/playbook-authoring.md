# Playbook authoring

Remediation behavior is data: **policies** bind a normalized problem class
to a **playbook**, and a playbook is an ordered list of typed steps with an
optional escalation target. On Kubernetes both arrive as CRDs
(`GPURemediationPolicy`, `GPUPlaybook`); the operator compiles them into
the controller's runtime files.

`DryRun` is the default and every step is an audited no-op in it. Real
execution requires `executionMode: Enabled`, which in turn requires
`spec.safety.destructiveExecution` naming the permitted nodes and carrying the
exact acknowledgement string — and each step still passes the safety, runtime
attestation, and approval gates at execution time.

## The mental model

```
signal (class, vendor) ──policy (exact vendor, then first generic fallback)──▶ playbook
playbook: step₁ → step₂ → … → verify quiet window → RESOLVED
   any step fails ──▶ on_failure.escalate_to (next rung) ──▶ … ──▶ NEEDS_HUMAN
```

- One open incident per (target, class); duplicate signals attach to it.
- A step marked `approval: Required` parks the incident in
  `AWAITING_APPROVAL` until a human decides (TTL-bounded, never
  auto-approved).
- After the last step the incident waits out the configured quiet window in
  `VERIFYING`; a recurring signal there fails verification and escalates.
- The playbook `cooldown` throttles repeat runs on the same target.

## Actions (closed set)

`GPUReset`, `Reboot`, `QuiesceAcceleratorStack` and `RestoreAcceleratorStack`
have been exercised against real NVIDIA hardware; a successful GPU reset has
not, because no cloud VM tested so far permits one (see below).

| CRD action | Intended effect | Notes |
|---|---|---|
| `Observe` | records + notifies | observe-first playbooks hold in OBSERVING; policy `params.threshold`/`window` control escalation |
| `Cordon` / `Uncordon` | Kubernetes cordon | intended to annotate the node with the incident reason |
| `Drain` | Eviction API drain | intended PDB-aware behavior; set a generous `timeout`; `params.force` — see below |
| `EvictGPUWorkload` | evicts only GPU-consuming pods | intended XID 94 targeted restart |
| `GPUReset` | `nvidia-smi --gpu-reset` on the node | refuses while any process holds the device node, naming the holders; pair with `QuiesceAcceleratorStack` |
| `QuiesceAcceleratorStack` / `RestoreAcceleratorStack` | stops/restarts the GPU vendor's own monitoring on the node | required before `GPUReset` on a GPU Operator cluster (see below) |
| `RunDiag` | `dcgmi diag -r <params.diag_level>` | levels 1–3 when DCGM is available |
| `CollectBundle` | `nvidia-bug-report.sh` | the agent runs it host-side into a bundle |
| `Reboot` | asks the host's init to reboot, from PID 1's namespaces | **must** set `approval: Required`; idempotent on `boot_id` so a retry cannot bounce the node twice |
| `RecycleNode` | cloud stop/start of the instance | approval-forced; AWS via scoped IRSA; refuses ASG members |
| `ReplaceNode` | terminates the instance for the autoscaler to replace | approval-forced; AWS via scoped IRSA |
| `IdleCheck` / `WaitIdle` | GPU idleness probes | the action journal persists intent and outcome across agent restarts |
| `VerifyGPUHealth` / `VerifyNodeHealth` | health probes | agent heartbeat freshness is exercised; DCGM verification is roadmap |
| `OpenTicket` | notification with quarantine note | intended end of the ladder (RMA) |
| `CheckpointWarning` | asks opted-in jobs to checkpoint before the deadline the cloud announced | only in a `cloud-interruption` policy's playbook, beside `Cordon`; a policy of another class that reaches it is rejected; needs no approval; see [Announced cloud interruptions](cloud-interruptions.md) |

`DriverReload`/`DriverReinstall`/`RunScript` are reserved for a future
host-provisioned scripts directory. They must use fixed names (or a strictly
validated `params.script`), never command content from the CRD.

### `Drain` and `params.force`

A pod with no controller has nothing that would recreate it elsewhere, so
evicting it destroys work outright. `kubectl drain` refuses such a node for that
reason and makes you type `--force`; `Drain` refuses for the same reason, before
it evicts anything, and names the pods.

`params.force: "true"` is how a playbook says the eviction should happen anyway.
It is off by default, there is no global switch, and **the step must also set
`approval: Required`** — the loader refuses the playbook otherwise. The pods it
destroys are usually somebody's `kubectl run` or the debug shell an engineer
left open on the very node that is failing, so the eviction should be a decision
written down in a playbook and confirmed by a human, not a default nobody chose.

Approval is required because every other gate that reasons about blast radius —
the action registry, the destructive-step confinement, the compiler's whole-VM
rule — sees an unchanged `Drain`. An ordinary drain moves work; this one ends
it. `RecycleNode` and `ReplaceNode` force approval because they destroy an
instance; this destroys a tenant's running job.

The pods a forced drain destroys are named in the step's log line and counted by
`kubeneuron_forced_unmanaged_evictions_total`, so "where did my job go" has an
answer that does not depend on a Kubernetes Event for a pod that no longer
exists.

The trade-off is real in both directions. Without `force`, one transient bare
pod makes that node undrainable — and because every rung of the shipped ladder
begins with `cordon, drain`, the refusal fires at each one in turn: four rungs,
four pages, and no repair, ending in `NEEDS_HUMAN`. Nothing more destructive
runs, which is the point, but nothing is fixed either. Decide per playbook which
is worse for the fault class it handles.

Two spellings that used to fail quietly now fail at load: a non-boolean value
(`force: yes` read as "no" and you found out at 3am), and `force` on any action
other than `Drain`, where it was validated and then silently ignored.

### Checkpoint coordination before `Drain` and `EvictGPUWorkload`

> This protocol has not been exercised on GPU hardware or by a real
> checkpointing workload.

A training job that can checkpoint would rather be told than evicted. When the
installation enables `spec.safety.checkpointCoordination`, the two disruption
steps gain a bounded pre-phase: opted-in Pods on the node are told, through
annotations on their own Pod, that a disruption is coming and by when, and the
step waits for them to acknowledge or for the deadline, whichever is first. It
is a property of the steps, not a playbook action: nothing changes in a
playbook, and every existing `Drain`/`EvictGPUWorkload` gets it. The full
design is in [checkpoint coordination](checkpoint-coordination-design.md).

**The policy (installation-wide, default off).** The block must name the
namespaces whose Pods may opt in; an enabled block without that allowlist is
rejected by the CRD:

```yaml
spec:
  safety:
    checkpointCoordination:
      enabled: true
      namespaces: [ml-training, ml-research]   # explicit allowlist, required
      defaultWait: 5m                          # for a Pod that requests nothing
      maxWait: 15m                             # ceiling; at most 30m
      skipClasses: [fell-off-bus, gpu-lost]    # the default: device already gone
```

**The workload opts in** with one annotation and may *request* a shorter
wait. The request only ever shortens the policy: the wait granted is the
request when present, parseable and positive, `defaultWait` otherwise, and
never more than `maxWait`. `"True"` and `"yes"` are not opt-ins.

```yaml
apiVersion: v1
kind: Pod
metadata:
  namespace: ml-training
  annotations:
    kubeneuron.io/checkpoint: "true"
    kubeneuron.io/checkpoint-max-wait: "8m"
spec:
  containers:
    - name: trainer
      image: registry.example.com/trainer:1.4.2
      volumeMounts:
        - name: podinfo
          mountPath: /etc/podinfo
          readOnly: true
  volumes:
    - name: podinfo
      downwardAPI:
        items:
          - path: annotations
            fieldRef:
              fieldPath: metadata.annotations
```

**KubeNeuron requests** by patching five annotations onto that Pod, and the
Downward API volume above delivers them to the container with **no RBAC at
all**: the trainer polls `/etc/podinfo/annotations` for
`kubeneuron.io/checkpoint-deadline-at` (an absolute RFC 3339 time),
`kubeneuron.io/checkpoint-reason` (the problem class),
`kubeneuron.io/checkpoint-next-action` (`platform.drain` or
`platform.evict_gpu_workload`), `kubeneuron.io/checkpoint-incident`, and
`kubeneuron.io/checkpoint-requested-at`. The kubelet refreshes the file on
its sync loop, not instantly; budget for that when choosing a short
`checkpoint-max-wait`.

**The workload acknowledges** on one of two paths:

- **Exit.** Write the checkpoint and terminate. The Pod reaching
  `Succeeded`/`Failed`, being deleted, or disappearing is the
  acknowledgement (counted as `exited`). No rights, no library.
- **Patch** `kubeneuron.io/checkpoint-state: "complete:<incident>"` onto its
  own Pod, where `<incident>` is exactly the value of
  `kubeneuron.io/checkpoint-incident` it read from the request (counted as
  `acknowledged`). This path requires the workload to **already hold
  `patch` on its own Pod**, for example through a training operator's
  ServiceAccount; KubeNeuron grants no such rights and never writes or
  deletes this key. The acknowledgement is bound to the request: a plain
  `complete`, a value naming another incident (for example one left over
  from an earlier disruption of the same long-lived Pod), a different case or
  surrounding whitespace is not an acknowledgement, and neither is
  `in-progress`. There is no "extend". The same incident recognizes its
  bound answer after a controller restart.

  ```sh
  # inside the container, after the checkpoint is durably written
  INCIDENT=$(grep '^kubeneuron.io/checkpoint-incident=' /etc/podinfo/annotations | cut -d= -f2- | tr -d '"')
  kubectl annotate pod "$POD_NAME" --overwrite \
    "kubeneuron.io/checkpoint-state=complete:${INCIDENT}"
  ```

**What the wait cannot do.** The deadline is operator policy, stamped on the
Pod as an absolute time so a controller restart resumes the same window. Each
Pod gets its own deadline (its `checkpoint-max-wait` request or the policy
default, never more than `maxWait`), the step waits until the latest of them,
and every deadline is bounded also by the step's own `timeout` minus a
reserve kept for the disruption itself: a `Drain` with a `timeout` shorter than
`maxWait` waits less, and one too short to leave anything after the reserve
does not wait at all. The reserve is kept whatever the platform does: the
whole pre-phase, including the listing, the patches and its own audit rows,
runs under its own deadline equal to that budget and borrows nothing from the
step's remaining time, so a slow API cannot spend the time the disruption
needs. The deadline stamped on a Pod is measured from the moment that window
opened, before the listing, so a slow listing shortens what a Pod is promised
rather than extending it past the window. When the deadline passes, or the
budget runs out, the step disrupts exactly as before; a Pod still open when
the budget ends before its own deadline is counted `unreachable` rather than
`expired`. Neither is a failure and neither escalates.
Pods that appear on the node after the step started are not asked. A `Drain`
asks only the Pods it will evict: finished Pods, mirror and DaemonSet Pods,
and (unless the step sets `force: "true"`) Pods without a controller are
neither asked nor waited for, since the drain leaves them alone.
`EvictGPUWorkload` asks every GPU-holding Pod. A `checkpoint-max-wait`
shorter than a second is honored as written; the stamped deadline keeps the
fraction. In `DryRun` nothing is patched and nothing waits; the audited
output says how many Pods would have been asked.

**Advisory at approval park (default off).** With
`checkpointCoordination.advisory.enabled: true`, the moment an incident parks
for human approval with a `Drain` or `EvictGPUWorkload` still downstream in
its playbook, every eligible Pod — the same eligibility as the request:
allowlisted namespace, opted in, not a skipped class, and one the disruption
would actually touch — is stamped with a closed set of **advisory** keys,
distinct from the request keys:

```yaml
kubeneuron.io/checkpoint-advisory:             "true"
kubeneuron.io/checkpoint-advisory-at:          "2026-11-03T10:14:02Z"
kubeneuron.io/checkpoint-advisory-incident:    "inc-8f2a"
kubeneuron.io/checkpoint-advisory-round:       "3"              # the approval round
kubeneuron.io/checkpoint-advisory-reason:      "ecc-dbe"
kubeneuron.io/checkpoint-advisory-next-action: "platform.drain"
```

An advisory carries no deadline and starts no wait: it is the minutes a
human takes to decide, handed to the job as lead time. The request with its
deadline still arrives at the disruption step exactly as above. One stamp
goes out per Pod per approval round (a re-park is a new round and a new
stamp; a controller restart finds its own stamp and writes nothing). If the
round ends without the disruption — rejected, expired, resolved by hand —
the controller stamps `kubeneuron.io/checkpoint-advisory-withdrawn-at`, best
effort; the advisory keys stay as the record of which round was advised, and
no workload-owned key is ever removed. A trainer reading its own annotations
should treat the advisory as "checkpoint if you can" and the request as "you
have until this time". Dry-run stamps nothing and audits how many Pods it
would have advised. The advisory uses the same allowlist, Pod-identity
confinement and `pods: patch` grant as the request and adds no RBAC; its
`budget` (default 10s, at most 60s) bounds one round and never delays the
approval. Historical v0.8.0 tests exercised this feature on CPU-only kind and on one Tesla T4 (`hack/hw-e2e.sh
test-checkpoint`); its hardware evidence is limited to that device.

**Checkpoint groups (default off).** A Pod may declare
`kubeneuron.io/checkpoint-group: "<name>"` (a DNS label, at most 63
characters; anything else is ignored and shown as `group-invalid` in the
impact preview). The group is every live Pod in the same namespace with the
same value. With `checkpointCoordination.groups.enabled: true`, when a
`Drain` or `EvictGPUWorkload` on a node coordinates with a member, the
member's **peers** receive the same five request keys — the same incident,
their own deadline from the same formula, from the same request time — and
nothing else: disruption stays on the one node, peers are never evicted,
their nodes are never cordoned, and the step waits for them no longer than
the same bounded window (`min(maxWait, budget)`; a peer asking for less gets
less, nothing a peer carries gets it more). Each member acknowledges for
itself (`checkpoint-state: complete:<incident>`, or exit); there is no group
leader and no aggregate state on any Pod. `groups.crossNode` is **off by
default**: the group is then the members on the disrupted node, which is
lead time for local ranks with zero new blast radius; `true` stamps peers on
other nodes in allowlisted namespaces. A group above `groups.maxGroupSize`
(default 64, at most 512) has its peers skipped — the step asks only the
node's members, counts the skip, and the preview flags the group. There is
no derivation of a group from an owner reference: the annotation is the
whole contract, and the examples show a Job template and a Ray worker group
setting it.

## Why a GPU reset needs a quiesce step

Draining the node is not enough to reset a GPU. NVIDIA's own components keep
open handles on the device nodes: on a stock GPU Operator install, measured on
live hardware, `nv-hostengine`, `dcgm-exporter` and the device plugin each hold
`/dev/nvidia0` without ever appearing as compute applications. With any of them
running, `nvidia-smi --gpu-reset` fails with exit 19 and the text "currently in
use by another process", which names nothing.

`QuiesceAcceleratorStack` does two things. It switches the GPU Operator's
components off through the vendor's own `nvidia.com/gpu.deploy.*` node labels,
touching only those that were running so a restore never enables something the
cluster had deliberately switched off. Then it asks the node itself to release
what no label can reach — chiefly `nvidia-persistenced`, an ordinary host
service that holds the device even on a fully drained node — and to confirm,
from its own process table, that nothing holds the GPU any more.

That confirmation is deliberately the node's job. Inferring it from pod labels
failed on a real cluster: the device plugin came from the machine image rather
than the GPU Operator, carried labels the controller did not recognise, and the
step reported a settled stack while the plugin still held `/dev/nvidia0`. If
anything still holds the device when the step's timeout expires, the step fails
with those processes named rather than clearing a reset that cannot work.

Two consequences worth knowing:

- **DCGM supplies the attestation the reset gate requires, and stopping it
  erases that attestation.** The quiesce step therefore validates the evidence
  first and pins it for the rest of the playbook. If the evidence is not already
  good enough to admit a reset, nothing is switched off at all.
- **Monitoring always comes back.** `RestoreAcceleratorStack` is the explicit
  step, but the controller also restores automatically once the incident stops
  running, so a playbook that fails at the reset cannot leave the cluster blind.

Components outside the GPU Operator are not covered, and this is not
hypothetical: on EKS the NVIDIA device plugin ships in the machine image as a
`kube-system` DaemonSet with no `nvidia.com/gpu.deploy.*` label, so nothing
KubeNeuron does stands it down. Measured on that cluster, the quiesce released
everything else — the DCGM engine, the exporter, the persistence daemon — and
then failed by name on the one it could not reach:

```
quiesce_accelerator_host: GPU 0 is still held by 3 process(es):
  nvidia-device-p(11621) holds /dev/nvidia-uvm, ... /dev/nvidia0, ... /dev/nvidiactl
```

Stand such a plugin down yourself before the reset, or the step will keep
failing this way. It fails loudly rather than proceeding into a reset that
cannot succeed.

## Compiler-enforced safety rules

The operator rejects (fail-closed), among others:

- `Reboot` without `approval: Required`;
- a GPU-targeted playbook containing node-scoped steps unless it declares
  `effects: [nodeScheduling]`;
- policies referencing unknown playbooks, escalation to missing playbooks,
  duplicate playbook names, invalid durations.

The controller applies the whole-node rule to file playbooks too (the
`-playbooks` directory, read at start and on every reload): a step with
`agent.reboot`, `platform.recycle_node` or `platform.replace_node` that does
not say `approval: required` is refused, and the reload keeps the running
configuration. The shipped file ladders in `configs/playbooks` ask a human at
the same steps as the CRD pack in `config/policies`: at the first step that
ends running work (drain, eviction or device reset), and at every reboot.

File playbooks are decoded strictly: an unknown or misspelt key (for example
`approve: required`) and a second YAML document in one file are errors at
start and on reload, never silently ignored.

A file configuration with `dry_run: false` must also say where destructive
steps may run: either `destructive_execution_node_selector` with the node
labels, or `destructive_execution_all_nodes: true` to allow every node. A live
configuration with neither is refused at start, and a reload keeps the
running configuration. The operator always compiles the selector from
`spec.safety.destructiveExecution.nodeSelector`.

## A worked example

```yaml
apiVersion: kubeneuron.io/v1alpha1
kind: GPUPlaybook
metadata: { name: drain-and-reset }
spec:
  kubeNeuronRef: fleet
  target: GPU
  cooldown: "1h"
  # Cordon/Drain and the quiesce steps act on the whole node, so a GPU-target
  # playbook must declare that effect; the compiler rejects it otherwise.
  effects: [nodeScheduling]
  steps:
    - { name: cordon,   action: Cordon }
    - { name: drain,    action: Drain,    timeout: "30m" }
    - { name: quiesce,  action: QuiesceAcceleratorStack, timeout: "5m" }
    - { name: reset,    action: GPUReset, timeout: "5m" }
    - { name: restore,  action: RestoreAcceleratorStack, timeout: "5m" }
    - { name: verify,   action: VerifyGPUHealth, params: { quiet_window: "10m" } }
    - { name: uncordon, action: Uncordon }
  onFailure: { escalateTo: reboot }
---
apiVersion: kubeneuron.io/v1alpha1
kind: GPURemediationPolicy
metadata: { name: ecc-dbe }
spec:
  kubeNeuronRef: fleet
  priority: 10
  match: { class: ecc-dbe }
  playbookRef: drain-and-reset
  mode: Approval
```

## Recovery modes

Each `GPURemediationPolicy` says who authorises a step that stops work for the
incidents it matches (`spec.mode`):

| Mode | What happens |
|---|---|
| `DryRun` | Every step is simulated and audited; nothing executes. |
| `Approval` (default) | Each step that stops work waits for a named person. A `CheckpointWarning` runs without approval. |
| `Automatic` | The same steps run without a person, within `spec.automatic`. |

The installation-wide `spec.safety.executionMode` still applies on top: under
`DryRun` nothing executes whatever the policy says.

> **Warning.** Automatic mode acts without asking. For the node pools and fault classes you list here, KubeNeuron drains the node and restarts or replaces it through your cloud provider's API as soon as the conditions of your policy are met. Workloads on that node are stopped. A replacement instance may not be available, and the cloud provider bills instances according to its own terms. When replacement adds a node first, two instances run and are billed until the old one is removed, and your quota must allow the extra instance. You are responsible for the policy you set here and for its effect on your workloads. Start in dry-run, then in approval mode, before enabling automatic mode.

An automatic policy is an approval given in advance, not the absence of one.
Every step it starts is audited with the actor `policy:<name>`, the policy's
generation, and who it declares enabled automatic mode and when
(`enabledBy`, `enabledAt`). KubeNeuron records these as declared and cannot
verify them; the Kubernetes API audit log shows who actually changed the
policy. Any identifier your organisation uses is enough; it is stored in the
policy and in the audit trail. `kubeneuronctl incident show` lists, per executed step, whether a
person approved it or a policy authorised it, and `kubectl get gpolicy` shows
each policy's mode and its effective mode (`DryRun` while the installation is
not `Enabled`).
For the `cloud-interruption` class, `spec.interruption.lead` (default `10m`,
from `1m` to `60m`) is how long before the announced deadline the warning and
the cordon start; see [Announced cloud interruptions](cloud-interruptions.md).

`spec.automatic` must name the node pools (`nodeSelector`, never empty) and
carries the limits:

- `maxConcurrentNodes` (and optionally `maxConcurrentNodesPerPool` with
  `poolLabel`): nodes the policy recovers at once. A reached limit holds the
  step until it clears.
- `nodeCooldown`: minimum time between two automatic recoveries of one node.
- `checkpointWait`: the longest wait for a job's checkpoint before a disruption
  (needs `spec.safety.checkpointCoordination`).
- `maxUnhealthyPercent` and `minUnhealthyNodes` (default 2): the circuit
  breaker. When more than this share of a pool's nodes (selected by
  `poolLabel`) have an open incident or are not Ready, and at least
  `minUnhealthyNodes` of them, the step goes to a person. The breaker stays
  tripped for that revision of the policy; a person returns it to automatic by
  changing the policy.
- `qualifiedProfiles`: names of `AcceleratorRuntimeProfile` objects. A
  replacement runs without approval only when the node's profile is listed
  here and the incident's [evidence](reference-api.md#incident-evidence)
  locates the fault on this node (`this-node`). With an empty list, a node on
  an unlisted or unknown profile, or a fault that is `shared` or `unknown`,
  the replacement waits for approval and the incident states the reason; the
  policy's `AutomaticReplacement` condition shows an empty list at once.
  Listing a profile is the operator's decision; KubeNeuron does not verify
  that automatic replacement is safe for it. Steps that replace no node, and
  DryRun, are unchanged.
- Per-node pause and maintenance windows hold automatic steps exactly as they
  hold every other step.

A step goes to a person (approval) when the node is outside the policy's pools,
when the circuit breaker trips, or when the step runs a command on the node
itself (GPU reset, reboot, driver reload or reinstall, scripts): node-level
actions are alpha, not validated on real hardware, and no automatic policy can
authorise them. A pool the cloud provider has not confirmed
can be recovered without its own repair interfering is different: recovery is
unavailable there in every mode, the step is held, and no approval is asked.
Switching a policy back to `Approval` parks the next step of incidents already
in progress.

Two more fields apply to the steps a policy runs, in every mode; a person's
approval does not lift them:

- `replacement.strategy`: `AddFirst` (default) brings up the new node and
  waits until it is ready with its accelerators before the old one is drained
  and removed; `RemoveFirst` removes the old node first and is used only when
  named. Where the node's provider or pool cannot prepare a replacement
  first, a replace step under `AddFirst` is held as unavailable and no
  approval is asked; it is never done the other way. Add-first is available
  on fixed-size AKS node pools and on fixed-size GKE Standard node pools in one
  zone (both experimental; on GKE, not validated against a live GKE cluster;
  start in DryRun); on other providers and pools, a policy that
  needs node replacement names `RemoveFirst` explicitly. See [Add-first replacement](#add-first-replacement).
- `replacement.successorTimeout` (default `30m`, from `5m` to `2h`): how long
  the prepared replacement may take to become ready under `AddFirst`.
  Add-first replacement creates one additional node in the pool before the old
  node is drained. Your cloud provider bills both nodes while they overlap,
  and the additional node needs quota. If the new node does not become ready
  within `successorTimeout`, KubeNeuron removes it only when it is the node
  this operation created and no workload runs on it. Otherwise the pool stays
  one node larger until a person removes it, and the incident says so.
- `recycle.enabled` (default false): the optional restart through the cloud API.
  Without it, a recycle is skipped as not applicable: it is not attempted, not
  counted as failed or as an escalation, and does not use up the escalation
  cap. A rung that recovers only by recycling is skipped before it starts
  (before its cordon), and the incident continues with the first rung on its
  `on_failure.escalate_to` path that does not; a recycle followed by a replace
  in the same rung moves on to the next step. The audit trail records
  `recycle skipped: …`. A recycle the cloud provider does not allow for the
  node is skipped the same way. When the escalation path ends or returns to a
  rung already passed, the incident is held as unavailable before the cordon.
  A recycle the provider stops allowing after its rung has started, with no
  replace after it in the rung, fails the rung: that is an ordinary
  escalation, and it spends an attempt. A skip is not a recovery and never leads to verification.
  A restart through the cloud API drains the node before it is tried, so it can interrupt the workload before a prepared replacement is ready; the add-first guarantee does not cover it.

Both are checked before the first disruptive step, together with the cloud
provider's eligibility. When no recovery step on the incident's path (its
playbook and the playbooks it escalates to) can run under the policy — for
a replace under `AddFirst`, even after an enabled recycle the replace would
back up, or a recycle-only path without `recycle.enabled` — the incident is
held before the cordon: the node is not cordoned or drained for a recovery
that cannot follow, and no approval is asked. Once the policy changes, the
hold lifts and the audit trail records `recovery-available-again`.

An automatic policy authorises only cloud recovery ladders: playbooks with a
recycle or replace step through the cloud API on their path. On any other
playbook the step is parked for a person.

Automatic mode is never enabled by default, by the installer or by a sample.
To enable it, copy the commented example, fill in the pools, limits and your
name, and keep the acknowledgement text as it is. It belongs on a cloud ladder
(cordon, drain and replacement through the cloud API):

```yaml
apiVersion: kubeneuron.io/v1alpha1
kind: GPURemediationPolicy
metadata: { name: fell-off-bus-cloud }
spec:
  kubeNeuronRef: fleet
  priority: 10
  match: { class: fell-off-bus }
  playbookRef: cloud-replace  # a cloud ladder: cordon, drain, replace through the cloud API
  mode: Approval              # state the mode explicitly
  # Automatic mode never runs node-level actions (GPU reset, reboot, driver
  # operations); those always wait for a person.
  # mode: Automatic
  # automatic:
  #   nodeSelector: { node.kubernetes.io/instance-type: p4d.24xlarge }
  #   poolLabel: eks.amazonaws.com/nodegroup
  #   maxConcurrentNodes: 1
  #   nodeCooldown: 6h
  #   checkpointWait: 10m
  #   maxUnhealthyPercent: 25
  #   minUnhealthyNodes: 2
  #   qualifiedProfiles: [p4d-h100]   # AcceleratorRuntimeProfile names; listing one is your decision
  #   enabledBy: "Jane Doe, platform team"
  #   enabledAt: "2026-11-15T09:00:00Z"
  #   acknowledgement: "I understand that automatic mode drains and replaces nodes without asking"
  # replacement: { strategy: AddFirst }
  # recycle: { enabled: false }
```

### Add-first replacement

Under `replacement.strategy: AddFirst`, a rung that replaces the node prepares
a replacement before draining the affected node. If the prepared replacement
is not ready, KubeNeuron does not start draining or deleting the old node
under this policy and shows why it is waiting.

The phases, in order, as the audit trail and `kubeneuronctl incident show`
report them:

1. **Planned** (`successor-planned`). The provider reads the node's pool and
   records its size and members. Nothing changes in the cloud. Under
   `Approval`, this is when the step is parked; the approval request says
   "This step creates one temporary additional node in pool <pool>. The cloud
   provider bills it until the old node is deleted." Rejecting it ends the
   operation as `not-created`.
2. **Requested** (`successor-requesting`, `successor-requested`). The pool is
   asked for one more node. The same limits as for any other disruptive step
   (concurrency, cooldown, unhealthy share) are checked first. The request
   time is recorded before the request, so a restart continues the same
   operation and never asks twice.
3. **Created** (`successor-created`). Exactly one new machine in the pool is
   tied to the operation. If there is none, or more than one could be ours,
   the operation is handed to an operator (see below).
4. **Ready** (`successor-ready`). The new node is Ready and schedulable, it
   joined after the request, its KubeNeuron agent registered after the request
   and reports its GPUs, and it offers at least the accelerators the old node
   had. Until then the audit records `successor-waiting` with the reason.
5. **Cordon, drain, replace.** Only now is the old node cordoned and drained
   as the playbook says, and the replace step removes the old node, leaving
   the pool at its size before the request (`successor-replaced`).

Under `DryRun`, nothing is created: the simulated step carries the same
sentence as the approval request.

**Timeout.** If the replacement is not ready within
`replacement.successorTimeout` of the request, the old node is left alone and
the step fails like any other, so the ladder escalates or stops. A
replacement that was created is cordoned and removed only if it runs nothing
but DaemonSet pods (`successor-released`); if other work has already been
scheduled on it, it is uncordoned and kept, and the incident records that the
pool is one node larger. KubeNeuron never evicts work to clean up. The same
release runs when the incident is resolved, rejected or abandoned during the
overlap. While the old node is gone, a replacement is never removed.

**Restarts and later approvals.** Under `Approval`, a controller restart
while the replacement is prepared returns the step to a new approval round.
The replacement already requested keeps coming up, but nothing is drained
without the new approval, and if the replacement is still not ready after
`replacement.successorTimeout` it is released as above. A replacement that
is ready stays, and is billed, while the incident waits for the approval of
its next step, until the incident ends.

**Transient errors.** A failed cloud or cluster call while the replacement
comes up is recorded (`successor-retrying`) and retried until
`replacement.successorTimeout`; it does not fail the step, since the request
may already have been accepted.

**What the replacement must offer** is recorded when the operation is
planned: each accelerator resource of the old node, at the larger of what
the node advertises and what its agent last reported. A device that has
fallen off the bus therefore still counts. A node with no accelerators on
record is held as unavailable before anything is requested.

**No capacity.** If the pool has no capacity or quota for one more node, the
operation ends as `capacity-denied` and the old node is not cordoned or
drained.

**Needs an operator** (`successor-needs-operator`). KubeNeuron stops changing
the pool and records why when:

- the pool changed in a way that cannot be tied to this operation (no new
  machine, or more than one);
- the replacement runs work and was kept after a timeout;
- the pool's members changed before the replacement existed, so restoring
  its size could remove a node KubeNeuron did not create.

Check the pool's size and members against the incident's record, remove what
is not needed, and resolve the incident.

**Billing and quota.** Add-first replacement creates one additional node in
the pool before the old node is drained. Your cloud provider bills both nodes
while they overlap, and the additional node needs quota. If the new node does
not become ready within `successorTimeout`, KubeNeuron removes it only when it
is the node this operation created and no workload runs on it. Otherwise the
pool stays one node larger until a person removes it, and the incident says
so.
The pool needs headroom for one more node in its size limit and in your quota
for that instance type. KubeNeuron shows only how long the overlap lasted, in
the incident and in the `kubeneuron_replacement_overlap_seconds` histogram
(labels `provider`, `pool`, `outcome`); what it costs depends on your
provider's rates.

In Automatic mode a replacement policy with `AddFirst` creates billed nodes
without a person approving each one.

### Verification after a replacement

The node that was replaced is gone, so its incident is verified on the node
that took its place. That node is found in one of two ways:

- **Add-first:** the cloud operation named it. It is the node KubeNeuron
  accepted (`successor-ready`), and its name and UID must still be the ones
  that were accepted.
- **Remove-first:** no cloud operation of this release names the new node.
  Before the delete, KubeNeuron records the old node's pool from its pool
  label (`eks.amazonaws.com/nodegroup`, `cloud.google.com/gke-nodepool` or
  `kubernetes.azure.com/agentpool`) and the UIDs of the pool's nodes
  (`replacement-pool`). A node is new when its UID is not in that set, so a
  node that comes back under the old name counts as new. It is accepted only
  if it is the only new node of the pool. If two or more nodes joined, for
  example during a scale-out, KubeNeuron cannot tell which one replaced the
  node, and it does not guess.

The new node must be Ready and schedulable, and offer at least the
accelerators the incident covered. Which report is then expected follows the
accelerators the incident covered:

| Accelerator | What is confirmed |
|---|---|
| NVIDIA (`nvidia.com/...`) | a fresh agent report from the new node lists a ready device, or a driver probe ran after the replacement |
| AWS Neuron (`aws.amazon.com/neuron...`) | a fresh agent report from the new node lists a device: device presence observed; runtime not attested |
| any other, or a pool whose nodes run no agent | no agent report exists for it, so KubeNeuron uses the node's Kubernetes readiness and advertised accelerator capacity; where the old node ran an agent, the new node's agent must have reported too |

The resolution says in its first sentence how the node was found:
`Replaced by node <new>, named by the cloud operation.` or `Node <new> is
Ready. It was the only node that joined pool <pool> after the removal.` Its
second sentence says what was confirmed: the driver check that passed,
`Device presence observed; runtime not attested.`, or `Verification used
Kubernetes node readiness and advertised accelerator capacity.` The same
text is in the RESOLVED notification. `kubeneuronctl incidents show` prints
the new node on a `replaced:` line. The audit entry records
`infrastructure_evidence: verified_replacement_node`, `replacement_node`,
`replacement_node_uid` and `replacement_correlation` (`successor` or `pool`).
The incident keeps its original target. `kubeneuronctl report` counts it as
recovered, and the degraded time ends when the new node is confirmed. This
says the node was confirmed, not that a workload resumed.

If that evidence is not there by the verification deadline, the incident
goes to `NEEDS_HUMAN` with what is missing, for example:

- `No node joined pool <pool> after the removal.`
- `New node <new> is not Ready.`
- `No fresh accelerator report from new node <new>.`
- `New node <new> reports no accelerator devices.`
- `Several nodes joined pool <pool> after the removal. KubeNeuron could not determine which one replaced the node.`

Such an incident is not counted as recovered, and no second replacement is
started.

## Authoring guidelines

1. **Every rung must leave the node safe on failure** — cordon before
   anything disruptive, and remember failure walks *up* the ladder, so the
   next rung inherits a cordoned, drained node.
2. **Put verification inside the playbook** and rely on the quiet window;
   a signal recurring during `VERIFYING` is your escalation trigger.
3. **Reserve approvals for irreversibility** (reboot, driver reinstall,
   RMA quarantine). Approval fatigue is a real failure mode.
4. **Tune cooldowns against your flap window** — the flap detector
   quarantines a (target, class) that reopens 3× in 24 h by default; a
   cooldown shorter than your typical recurrence just burns rungs.
5. **Test in dry-run**: `kubeneuronctl remediate <node> --class <class>`
   drives the exact ladder as audited no-ops.

### Cloud recovery availability

Cloud playbooks can be installed before `spec.cloud.provider` is configured:
this keeps detection and incident reporting available. The installation then
reports `CloudRecoveryAvailable=False`, reason `ProviderNotConfigured`.
Configuring a provider changes this condition to `Unknown`, not `True`: the
controller must still verify each incident's pinned node identity, permissions,
repair policy and replacement strategy before it can cordon or drain. A human
approval cannot override missing provider eligibility. Incident details show
the last recorded provider and policy assessments, including their timestamps;
these assessments are not promises of replacement capacity or recovery success.
