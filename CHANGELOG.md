# Changelog

All notable changes to this project are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html)
with a pre-1.0 caveat: minor versions may contain breaking changes while the
API is `v1alpha1`.

## [Unreleased]

## [v0.3.0] - 2026-08-27

A minor rather than a patch: this carries schema migrations on both engines
(sqlite 0020, postgres 0011) and changes behaviour operators will notice.

**Read this before upgrading if you run a documented restore.** The backup and
restore procedure in `docs/operations.md` was wrong in a way that could corrupt
the database it exists to restore. It said "stop the controller so nothing
writes the database" and scaled the Deployment to zero — but the operator owns
that replica count and reconciles it straight back, so the wipe replaced the
SQLite file underneath a running writer. Both that procedure and the node-
maintenance one now stop the operator first and wait for the Pod, not for
`kubectl rollout status`, which reports a scale-to-zero complete the instant
after the scale. The integration suite verified the stop the same broken way,
which is why this survived: it had been proving a procedure it never performed.

### Fixed — incidents could not tell two GPUs apart, and cordons had one owner

- **Two unattributed GPUs on one node collided into one incident.** A kernel
  fault that knocks a card off the bus prints a PCI address and nothing else, so
  the incident's identity was `(node, '', class)` and the second card's fault was
  folded into the first card's incident — never remediated, never reported.
- **The precise signal that arrived seconds later was thrown away.** When the
  vendor tool resolved that same address to a UUID, the agent's dedup window
  discarded it as a repeat. The ladder then cordoned and drained the node and
  refused the reset permanently as "target unattributed", for a device whose
  exact identity had been available all along. Incidents are now promoted onto
  their UUID atomically when it arrives.
- **A finished incident uncordoned a node another was still remediating.** The
  uncordon step called the unguarded release with no ownership check, so the
  first incident to finish returned the whole machine to the scheduler while a
  GPU on it was being reset — and restored from whichever snapshot survived the
  second cordon, which could take a human's own cordon and their
  `karpenter.sh/do-not-disrupt` pin with it. A cordon is now held by a set of
  owners and released only when the last one leaves.
- **A human's verdict on one hold stranded every other hold on that node**, and
  did so as a deadlock: the mark blocked release of the other holds and clears
  only when the owner set empties, which it then never could. Eight GPUs out of
  the fleet permanently, with no page — the stuck-cordon signal needs a live
  incident, and retention had swept it.
- **The concurrency cap stopped applying to sibling GPUs.** The safety gate keyed
  an unattributed target by the bare node name, so once one incident per device
  became possible, a PCIe switch failure taking eight cards off one node ran
  eight ladders against a cap set to two — the correlated failure the cap exists
  for.
- **Degraded-capacity was overcounted eightfold.** Both counters asked "do we
  know the UUID" where the question is "one card or the whole machine", so a
  PCI-only incident was charged the node's entire inventory. That number goes in
  front of whoever pays for the fleet.
- A drain now records the pods a forced eviction destroys, and the ones whose
  local scratch it takes with them. A cordoned node whose owner annotation
  cannot be parsed is counted by a new gauge rather than only logged once.

### Fixed — raised against the release candidate, before wide rollout

- **A timing-out action could not report why.** The lease ended exactly at the
  action's own deadline, so the executor's result — the reason, the processes
  still holding the device — arrived a moment after the store stopped accepting
  it. The controller saw a bare timeout with no cause, and the row read as
  reclaimable: for the shipped 12h `WaitIdle` rung, another twelve hours of a
  card out of service before anything changed. The lease now covers the deadline
  plus a reporting grace, and that grace is one constant three components share
  rather than a number two of them knew.
- **A human could not take a node over.** The restore snapshot answers "what was
  here before us"; nothing answered "what did a person decide after us". An
  engineer who kept a cordoned node out of service set the same flag KubeNeuron
  had — there is only one — so the last release read the marks as its own and
  returned the machine. `kubeneuron.io/cordon-handoff` is the one key KubeNeuron
  never writes, checked with a JSON-Patch `test` op so a claim landing mid-
  release cannot be raced past. See `docs/operations.md`.
- **Counted cordon ownership was optional**, with a fallback to the unguarded
  pair — so a platform that simply did not implement it silently got back the
  P0 the counting exists to prevent. It is part of the platform interface now:
  forgetting it is a compile error, and bare metal, which had exactly that gap,
  counts holders.
- **Policies could not be scoped to a vendor.** A problem class is not
  vendor-specific while the ladder answering it is, so AMD nodes selected the
  NVIDIA ladder and were refused at the capability gate — after the cordon and
  the drain. Policies now take an optional `match.vendor`; unscoped ones still
  match everything. A signal naming no vendor matches no scoped policy, because
  these ladders reset hardware. Vendor adapters and AMD/TPU remediation are
  deliberately still absent: what this enables today is a separate, safe AMD
  ladder that stops short of a reset.

### Fixed — found by an independent review of the release candidate

An external reviewer was given the four commits and asked for defects at the
SEAMS between them, without being told what had already been found. All six of
its findings were real, and every one lived between changes rather than inside
one — which is where three internal review passes had stopped finding anything.

- **A promoted incident leaked its safety-gate slot permanently.** The durable
  bit said WHETHER a remediation slot was held, never under which key — and the
  key stopped being derivable once a promotion could change an incident's target
  mid-flight. A step goroutine carries the incident as it was when its step
  began, so it released the pre-promotion key while the reservation sat under
  the new one: a unit of `MaxConcurrentRemediations` gone for the life of the
  process, charged to an incident that had already finished, with no incident
  visibly responsible. The controller now records the key it reserved under.
- **A node-scoped signal joined a device incident.** The node-scoped lookup
  constrained the GPU UUID alone, and every PCI-only incident has an empty UUID
  too — so an alert or a manual trigger attached to the oldest same-class
  incident about ONE CARD, drove that card's ladder, and left the node-scoped
  fault with no incident at all.
- **A human takeover was ignored on pre-upgrade cordons.** The annotation was
  honoured in the restore path but not on the release path a cordon from an
  older build takes, so an operator could claim such a node and watch the
  finishing incident hand it back anyway.
- **Bare metal recorded a cordon owner a failed hook never earned.** The owner
  went into the set before the hook ran; if the hook failed, the next incident
  saw "already down", reported success, and never retried it — and the ladder
  drained and reset a node that had never been taken out of service.
- **The janitor asked for a capability wider than it used**, so an adapter with
  the required pair but not the held-mark method fell through to the reason-only
  release and lost owner counting entirely. The test fake had exactly that gap,
  which means every janitor test had been exercising the fallback.
- **Two policy lookups discarded the vendor.** The late bind built a signal from
  the class alone, so an incident that raced a policy rollout could never bind
  its vendor's ladder; and the observation threshold came from the first policy
  of that class, so an AMD incident escalated on NVIDIA's timing. Selection now
  lives in one predicate all three paths ask.

Two of these were hidden by tests that could not fail: one released the slot
under the promoted target — the one thing production could not do — and the
other satisfied its interface through an embedded nil.

### Fixed — the hardware stand

Three paid runs died holding a live GPU cluster, each on a different mechanism:
a teardown piped through a dead `tee`, a `curl` with no `--max-time` in an EXIT
trap, and a bash sitting in `do_wait` for a port-forward child that never
exited. The last cost 11h38m. Closing causes one at a time did not work; phases
now run in their own process group under a deadline.

A new `test-drain` phase puts a tenant workload and one bare pod on the GPU node
and asserts the drain refuses **having evicted nothing** — the difference
between a pre-flight refusal and one issued after the damage, which only a real
pod list can show. Run 9 passed seven phases on a real T4 and exercised that for
the first time in nine paid runs.


Rounds 15-26. An independent review per round, and **eight paid runs on real
NVIDIA hardware** — an EKS g4dn.xlarge with a Tesla T4 — which is where most of
what follows came from.

**The fifth and sixth runs passed every phase.** The fifth was the first
end-to-end green run in this project's history, and the four before it are why
it took five: each one failed somewhere new, and every failure was a real
defect in the product or in the stand that was supposed to prove it.

One pattern is worth stating on its own, because it shaped this whole stretch:
every round found the previous round's regression, and by round 21 a reviewer
named the reason. It was the same defect class each time — **a set of states
enumerated inline at a new site instead of asked of one shared predicate.** The
action queue terminalises three ways; the prune knew three, a discard knew two,
and a probe added later knew two different ones. Each omission wedged a node's
GPU monitoring for the retention window. The fix that finally held was not any
of the one-line corrections but `QueuedAction.Terminal()` plus a matching SQL
constant, with regression tests driven from the state STRINGS so that adding a
state without teaching the predicate fails. The class recurred once more after
that, one queue over and one call site over, and those are now converted too.

### Rounds 23-26: what the reviews found where nobody had looked

Round 25's reviewers produced a coverage report — which files had never been
read adversarially — and round 26 sent three readers into exactly those. Ranked
by what each cost an operator:

- **The reset-evidence hold never ended.** Both of its siblings in the same
  function bound their equivalent wait, because evidence that is merely late
  becomes evidence that is never coming and the two are indistinguishable from
  inside the controller. This one held forever, and the shipped ladder reaches
  the reset rung *after* cordon and drain — so a node whose evidence can never
  arrive sat cordoned and emptied of tenant work indefinitely, in EVALUATING
  rather than NEEDS_HUMAN: on no alert, in nobody's queue, with a deferral
  counter climbing and nothing else saying a word.
- **A grace clamp added in round 24 reintroduced the force-delete its own API
  contract calls unexpressible.** It reached `GracePeriodSeconds: 0` by
  arithmetic rather than by any branch: the PDB retry loop recomputes it every
  five seconds, so the tail of every contended drain force-deleted. A tenant who
  set `terminationGracePeriodSeconds` specifically to checkpoint before a GPU
  reset got none — and the drain then reported the node drained, which is worse
  than the timeout it replaced, because a timeout escalates visibly.
- **One Intel GPU was counted as a thousand.** `gpu.intel.com/millicores` was
  read as whole devices, so a single node-scoped incident billed a thousand
  GPU-hours of degraded capacity and tripped every fleet-fraction alert. It had
  seven siblings in both directions — including NVIDIA time-slicing replicas on
  the primary vendor, and entire Habana and AWS Neuron fleets that were
  invisible to the control plane, silently, because the unrecognised-vendor
  warning only fires for GPU-shaped resource names. Replaced with one
  classification table that fleet membership and counting both read.
- **A maintenance window could be accepted by `kubectl` and never applied.**
  `GPUMaintenanceWindow` and `GPUNodeConfig` compile into the snapshot and can
  fail the whole installation, and neither ever received a status. A window
  written with `matchExpressions` left `windows.yaml` unchanged, so automation
  was not paused while a technician worked the row — and the status signal
  pointed at the wrong objects, with three innocent siblings showing
  `CompilationFailed` while the culprit looked clean.
- **Editing one signal mapping rolled the entire agent fleet.** The agent pod
  template carried the config digest, left over from the retired two-DaemonSet
  era. The agent mounts no ConfigMap and reads no snapshot; on 500 nodes at
  `maxUnavailable: 1` that was a multi-hour rolling detection blind spot walking
  the fleet.
- Smaller, same shape: the quiesce pin froze the controller's own *authority*
  rather than the evidence a quiesce destroys, so a revoked profile went on
  granting resets and the node-identity check compared a value to itself; the
  agent's copy of the runtime-version rule disagreed with the controller's on a
  bare-major pin, which stamps every report on such a fleet degraded and denies
  every reset; the reset preflight tested a deploy label's *presence* while the
  quiesce requires the value `"true"`, clearing a node for cordon and drain
  ahead of a reset that could not run; a declared forbidden holder longer than
  15 characters could never match, because that is where Linux truncates a
  process name — and the CRD's own standing example was 16.

`params.force` on a drain was added this round and finished in the same round
after review: it now requires `approval: Required`, names the pods it destroys
in the audit trail, counts them in
`kubeneuron_forced_unmanaged_evictions_total`, and is rejected on actions that
would ignore it.

**Run 8 passed every phase, and then found a defect in the stand itself.** The
EXIT-trap cleanup added in round 24 — which exists so a phase cannot leave an
open incident for the next one to attach to — called the controller API through
a `curl` with no `--max-time`. A `kubectl port-forward` keeps its local listener
bound after the remote end dies, so the connect succeeds and the read never
returns: the trap hung for two hours with the GPU cluster still billing. That is
the second time this stand's cost guarantee has died on a cleanup step that
could not fail (the first was a teardown piped through `tee`), so every API call
is now bounded, the port-forward is probed rather than assumed live, and `make
lint` fails the build on an unbounded `curl` in the script that holds a paid
cluster.

**And the run showed that the drain has never really been exercised.** The
destructive ladder does contain a real `Drain` and does run it in Enabled mode —
but the stand places no workload on the GPU node, so that drain has always
walked an empty pod list, and no phase has ever asserted anything about it. The
most expensive defect of round 26 lived in exactly that code, which is why four
green runs did not find it. A new `test-drain` phase puts a tenant Deployment
and one bare pod on the node and asserts the refusal happens with **nothing
evicted** — the difference between a pre-flight refusal and one issued after the
damage, which only a real pod list can show. Not yet validated on a paid stand.

**A test-suite finding worth recording on its own.** The postgres half of the
round-22/25 queue claims had never been run — only sqlite. Writing it exposed
something worse than a missing engine: cutting `terminalActionStates` down to
`('done')` left the action-side guards *passing* on both engines. One asks the
Go predicate, one only ever uses `"done"`, and the single test that covered
`dead` reached that state by burning the claim-attempt budget and `t.Skip`ped
when it did not get there. So the SQL half of the defect class that recurred for
nine consecutive rounds was unguarded, through four rounds of green runs. The
states are now set directly, on both engines, with nothing to skip.

### Proven on hardware, for the first time

The stand had never run the agent against a real driver: it never set
`spec.agent.hostTooling`, and the distroless agent image carries no
`nvidia-smi`, so every previous run — including those recorded as evidence —
executed a simulator on a real T4. With that fixed, the second run proved end
to end, on hardware: a confined `ReplaceNode` terminating a real EC2 instance
with the approver audited and the incident resolved; the dry-run ladder with
the controller itself reporting `execution_mode: dry-run` while it ran; a
recurrence inside the verification quiet window escalating rather than
resolving; and the threshold path holding sub-threshold and escalating on the
third signal. Teardown swept the account to zero across nine resource classes.

What the fifth run proved, in one pass on one T4: an XID injected into the
kernel log opening an incident that walks cordon → drain → approval with the
approver audited and the controller itself confirming it was in dry-run; the
DCGM detection source observing an injected fault; a same-class recurrence
inside the verification quiet window escalating rather than resolving; the
threshold path holding sub-threshold and escalating on the third signal; and a
confined `ReplaceNode` terminating a real EC2 instance and resolving the
incident. The account swept to zero afterwards, verified independently across
nine resource classes.

### The DCGM detection source could not work at all

Two defects, one behind the other, and only real hardware could show either.

The agent bundles its own `dcgmi` because the GPU Operator keeps one inside its
own container. That client was pinned to **4.6.1**, and no GPU Operator release
ships a host engine that new — v25.3 ships 4.3.1, v25.10 ships 4.4.x, v26.3
ships 4.5.2. DCGM tolerates an engine newer than the client and not the
reverse, so **the second detection source could not function in the deployment
the documentation recommends, on any operator version.** The client is now
pinned to 4.3.1, the oldest engine supported.

Worse than the failure was the silence: the fallback to `nvidia-smi` was logged
at Debug, below the agent's own level, so the deeper source could fail on every
poll for the life of the process and say nothing. Only a gauge nobody is told
to read showed it. It is now a warning, once per outage, naming the failing
command and what to check — and that warning is what found the second defect
after the first was fixed.

**With both closed, the source now works, and that is recorded evidence.** The
fifth and sixth hardware runs each injected a DCGM field value on a real Tesla
T4 and watched the agent's `gpuhealth` source observe it — 59 seconds on the
first of them. `docs/reference-capabilities.md` promotes the polled-telemetry
row for NVIDIA from *shipped, not hardware-validated* to *shipped &
hardware-validated* on the strength of these runs, which is the only kind of
evidence that page accepts.

### Fixed — the emergency stop, which did not stop things

An operator switching a running installation to `DryRun` is the documented way
to halt a runaway remediation. Four separate defects meant it did not:

- **It removed the blast radius from every incident already in flight.** The
  dry-run flag is stamped when an incident opens and was never re-read; the
  operator compiles the destructive node selector only for an `Enabled`
  install; and an empty selector reads as "no confinement configured".
- **Every agent step stayed simulated after enabling**, because the dry-run
  actuator wrapper was installed once at process start and configuration
  reloads in place. Platform steps went live while agent steps returned a
  successful `DRY-RUN: would execute …` — the ladder drained the node for real,
  counted the reboot as executed, and resolved the incident with the fault
  untouched.
- **...and fixing that broke the janitor's host restore**, which calls the
  actuator directly: a simulated restore returned OK, so the janitor cleared
  the durable marker that would have retried and left the node's GPU monitoring
  off permanently. The wrapper now refuses to simulate an undo.
- **The dashboard and the report disagreed about what had happened.** Execution
  followed the live gate while accounting followed the stamped flag, so a fleet
  whose faults stopped recurring after the stop was told it had recovered those
  GPU-hours.
- **The janitor began writing `NoSchedule` taints in dry-run**, and at one point
  removing protective ones. Placing a mark now reads the mode; keeping one reads
  the incident, which are different questions.

### Fixed — elsewhere

- **Setting `executionMode: DryRun` to stop damage removed the blast radius
  from every incident already in flight.** The dry-run flag is stamped when an
  incident opens and was never re-read; the operator compiles the destructive
  node selector only for an `Enabled` install; and an empty selector reads as
  "no confinement configured". Execution now consults the live gate.
- **Enabling a running installation left every agent step simulated.** The
  dry-run actuator wrapper was installed once at process start, and
  configuration reloads in place, so platform steps went live while every agent
  step returned a successful `DRY-RUN: would execute ...` — the ladder drained
  the node for real, counted the reboot as executed, and resolved the incident
  with the fault untouched.
- **...and the fix for that broke the janitor's host restore**, which calls the
  actuator directly. Switching a running installation to DryRun made the
  restore return a synthetic OK, so the janitor cleared the durable marker that
  would have retried and never looked again, leaving the node's GPU monitoring
  off permanently. The wrapper now refuses to simulate an undo.
- **A stock install reported 100% recovered, 100% unattended, having touched
  nothing.** `install.sh` bound one problem class, and the report counted
  "reached RESOLVED" as recovered without checking that anything ran.
- **The idle-guard refusal code never survived the store**, pinning
  `kubeneuron_destructive_steps_deferred_total{reason="not_idle"}` at zero.
- Six reachable standard-library advisories, via Go 1.25.13.
- `docs/pilot-checklist.md` documented a `hostTooling.enabled` field that does
  not exist; a reader writing `enabled: false` to turn it OFF would have turned
  it ON.
- Store-derived gauges were published by every replica, so `sum(...)` doubled
  on a PostgreSQL HA pair, and were collected on the scrape path with no
  deadline.

### Added

- **A baseline policy pack** (`config/policies/`) binding every problem class
  the detectors can emit. **Nothing in it is armed:** a human decides before any
  step that ends running work. Four bindings cannot execute on AMD — a policy
  matches on class alone and those ladders repair through an NVIDIA-scoped
  reset — and a test now derives the emitting vendors from the detector tables
  and requires each such pairing to be declared.
- `recovered` requires an audited transition into `EXECUTING` for a real
  remediation step; everything else that closes lands in a "nothing done"
  bucket.
- Gates that can actually fail: `shellcheck` over every shipped script,
  JSON patch bodies walked against the CRD schema, a code-to-dashboard coverage
  check, release-asset completeness, and a mirror that fails on additions.
- Panels for the protection metrics, which had none.

### Fixed — the agent, which nobody had reviewed

Rounds 21 and 22 were the first time anyone read `internal/agent/nvml` and
`internal/agent/dcgm` adversarially. Both findings are the same shape: a probe
whose failure looked like a result.

- **`nvidia-smi` placeholders were accepted as device identities.** Only the
  index was validated. A driver that is up but unhappy prints `[N/A]` or `ERR!`
  rather than failing, so two GPUs could share the UUID `[N/A]` — their
  registration entry, their accelerator-report device, and the target key of
  any incident opened for either. The inventory now fails closed.
- **The idle guard could wedge a node permanently.** It ran a second probe,
  `--query-accounted-apps`, whose underlying NVML call documents returning
  processes *in running or terminated state* from a buffer that survives until
  explicitly cleared. On a node with accounting mode enabled, once any job had
  run the device was never idle again — and that refusal deliberately does not
  escalate, so the incident parked for a human while the control plane recorded
  that live work had been spared. Removed; the compute-apps probe and the
  `/proc` holder scan already cover it.
- `run_diag` resolved `dcgmi` from `PATH`, which the agent puts host tooling at
  the front of — so the version the agent *attests* came from the pinned client
  while the diagnostic that advances or halts a ladder came from whatever the
  node carried.
- Dead-lettered work — the moment something permanently stops being retried —
  left no trace on either queue. `kubeneuron_dead_lettered_total` and a panel.

### Also fixed, in the things that were supposed to prove all of the above

`hack/verify-release.sh` reported OK on a release with no installer and no
signature. `hack/mirror.sh` checked one direction while claiming two. The
kind suite's dead-port-forward retry could not retry a dead port-forward — the
order of its checks was wrong, and underneath that the restart allocated a new
local port the pre-expanded curl argv could not follow. The hardware stand sweep
returned early on a clean account, deleted an ECR tag that never existed, and
double-deleted a volume it found twice; its cleanup trap fired everywhere
except where it was needed.

---

Round 15. Two independent reviews of released v0.2.3 — one adversarial on the
code, one on what the project has evidence for — plus public CI's own verdict,
which had been red on every Dependabot pull request since the 1.25.13 Go
advisories landed.

**If you have ever set `spec.safety.executionMode: DryRun` on a running
installation to stop a remediation, read the first Fixed entry.**

### Security

- **Go 1.25.13.** Six standard-library advisories, all reachable from shipped
  code, all present in the v0.2.3 binaries and images: `encoding/xml` recursion
  depth on the EC2 decoder in the ReplaceNode path, `encoding/asn1` recursion
  depth in `pki.LoadAuthority`, and four in `net/http` reachable from the
  controller's TLS listener, the agent's health server and every AWS SDK
  request. Nine version pins move together — go.mod, `build/Dockerfile`, and
  the `go-version` in the CI, release and hardware workflows — because a
  workflow pinned below go.mod does not fail, it silently downloads the newer
  toolchain and the pin becomes a lie about what built the artifact.

### Fixed

- **What a pilot installs now remediates more than one problem class, and the
  recovery report no longer counts the rest as recovered.** `deploy/install.sh`
  bound exactly one class (`xid-app`, observe-only) and `config/samples` added
  one more, out of the seventeen `pkg/types` declares and the detectors emit.
  An unbound class is not a quiet gap: the incident opens, observes, and
  quiet-resolves without remediating, and `internal/controller/report.go`
  counted "reached RESOLVED" as recovered with no check that anything ran — and
  as recovered *unattended*, because an incident nothing acted on never asks
  anybody's permission. An installation binding one class out of twenty
  therefore reported near-total unattended recovery, and
  `docs/pilot-checklist.md` told the operator to take that number to whoever
  pays for the fleet. Three changes, together:
  - `config/policies/` is a baseline pack binding every class the detectors can
    emit, with the severity and blast-radius reasoning written beside each
    binding. `install.sh` applies a copy — a piped install has no checkout to
    read it from, and `internal/operator/policy_pack_test.go` pins the two
    together on which class gets which ladder. Nothing in it is armed: every
    step that ends running work — drain, targeted eviction, device reset,
    reboot — requires an approval, and a test fails the build if one stops
    doing so. Two bindings deliberately differ from the file-based
    `configs/policies.yaml`, which was corrected to match: `driver-hang` no
    longer answers a dead exporter by draining a node full of training jobs,
    and `gpu-lost` no longer spends a cordon and a drain reaching a reset that
    is structurally impossible on a device with no UUID.
  - The recovery report reads the audit trail for a transition into `EXECUTING`
    and puts a close with no remediation step in its own `nothing done` bucket
    — never in `recovered`, `recovered_unattended`, recovered GPU-hours, or
    MTTR. A notification-only step is not remediation. The simulated (dry-run)
    section carries the same split, so a pilot's projection cannot silently
    include "we would have done nothing".
  - `internal/operator/policy_pack_test.go` fails the build when a class the
    detectors can emit has no binding, with a reason required per exception.
    `diag-failure` is the only entry — nothing emits it — and that exception
    fails the moment something does.
- **Switching to `DryRun` to stop damage removed the blast radius from every
  incident already in flight.** `inc.DryRun` is stamped when an incident opens
  and was never re-read, the operator compiles
  `spec.safety.destructiveExecution.nodeSelector` only for an `Enabled`
  install, and an empty selector is read as "no confinement configured". So the
  documented emergency stop emptied the selector while in-flight ladders kept
  their live flag: a destructive step refused a second earlier for being outside
  the declared blast radius became allowed on any node in the cluster.
  Execution now consults the live gate, monotonically toward safety — an
  incident opened in `DryRun` still never becomes live.
- **The idle-guard refusal code never survived the store**, so
  `kubeneuron_destructive_steps_deferred_total{reason="not_idle"}` was
  structurally pinned at zero and `idleGuardRefused` could not be true. The
  field is `json:"-"` for wire-compatibility reasons that are correct about the
  wire; the store is not the wire, and the controller reads results from the
  store rather than from the agent's HTTP response. No migration; results
  written by earlier builds decode unchanged.
- **`hack/verify-release.sh` reported OK on a release with no installer, no
  manifest and no signature** — every downstream check is a loop over a glob,
  and an unmatched glob simply skipped its loop. It now asserts the asset set is
  complete before checking that it is correct.
- **`hack/mirror.sh` checked one direction while its header claimed two.** The
  array holding the mirror's own state was assigned and never read, so a
  scratch file left beside the code was copied into the public checkout and
  published with no assertion at all. It now fails on additions and honours
  `.gitignore`.
- Every XID-opened incident carried no vendor: v0.2.3 added the evidence key to
  one of two near-identical signal builders, and the controller calls the other.
- Store-derived gauges (`kubeneuron_degraded_gpus`, `kubeneuron_incidents`,
  `kubeneuron_actions_pending`) were published by every replica, so `sum(...)`
  double-counted on a PostgreSQL HA pair, and were collected on the scrape path
  with no deadline. Leader-only now, under a five-second budget.
- `docs/pilot-checklist.md` told operators to enable host tooling with a
  `hostTooling.enabled` field that does not exist; the CRD's structural schema
  prunes it silently, and a reader writing `enabled: false` to turn it OFF
  would have turned it ON.

### Added

- **The hardware GPU stand now runs against a real driver.** Every previous run
  — including those recorded as evidence — executed the agent on the fake NVML
  driver on a real Tesla T4, because the stand never set `spec.agent.
  hostTooling` and the distroless agent image carries no `nvidia-smi`. Nothing
  detected it: no accelerator report was produced, every incident was
  node-scoped rather than device-scoped, the DCGM watcher was never built, and
  no run ever armed a node. The stand now arms host tooling, installs the GPU
  operator with its standalone DCGM engine, and asserts the agent is not on the
  fake driver before any NVIDIA phase runs.
- The `test-dcgm` and `test-verify-recur` phases existed in the script and in no
  workflow, so the only two phases exercising the DCGM source and the
  `VERIFYING`-recurrence escalation had never run.
- The destructive phase waits for the CONTROLLER to report the mode and
  confinement it will act under, not for the operator to stamp the root object
  Ready — the window between them is where three earlier runs were lost.
- The teardown sweep no longer reports clean because its filters matched
  nothing: instances are found by EKS's own tag as well as ours, `DELETE_FAILED`
  is in the CloudFormation status filter, volumes are found by the tag the EBS
  CSI driver actually writes, the run's ECR images are deleted, and the reaper
  enumerates clusters through the EKS API rather than parsing a JSON shape that
  can silently yield nothing.
- `shellcheck` runs over `hack/*.sh` and `deploy/install.sh` in `make lint`;
  `hack/verify-spec-paths.py` now walks JSON patch bodies against the CRD schema
  rather than only dotted prose; the dashboard test suite gained a code→panel
  coverage check and now parses label matchers as its doc always claimed.
- Panels for the protection family — degraded accelerators by owner, workloads
  evicted, destructive steps deferred, and the agent's active detection source.
- `docs/operations.md` gained a section on draining the controller's own node,
  which hangs by design and said so only in a code comment.

## [v0.2.3] - 2026-08-12

Release evidence: full CI on the public repository green as the tag's gate —
including the new `image` job, which builds all four published images with the
classic builder and asserts what only the artifact can show, and the kind
integration suite building the PRODUCTION `build/Dockerfile` rather than a
stand-in. That suite now also lets a playbook really change a cluster: one
phase arms a disposable worker, drives a cordon for real, and proves the
janitor gives the capacity back — the first time a destructive step and its
recovery have run under test outside unit tests.

This release is mostly the closing of fail-open paths that two review rounds
and that new phase found. Four of them could not be seen by any unit test:
they live at the seam between the operator, a ConfigMap and a live cluster,
and the kind suite could not see them either, because every other phase runs
in dry-run and so had never switched execution mode or applied confinement for
real. **If you run AMD, or you have ever changed `spec.safety.executionMode`
on a running installation, read the Fixed section before upgrading.**

### Fixed
- **`spec.safety.executionMode` had no effect on a running controller**
  (`cmd/kubeneuron-controller/reload.go`, `internal/safety/limits.go`). The
  operator deliberately keeps the config-digest off the controller's pod
  template — under leader election a rollout deadlocks, because only the
  leader is Ready — so configuration reloads in place. But the reload
  re-installed playbooks, profiles and the confinement selector and never the
  safety gate's dry-run flag: it was read once at process start, and
  `SetDryRun` had no caller anywhere in the tree. Switching to `Enabled` left
  an installation that believed it was armed executing nothing; switching back
  to `DryRun` — the lever an operator pulls to STOP damage — left it
  executing. `Gate.ApplyLimits` now moves the limits with the file they travel
  in, leaving live state (held slots, cooldowns, pause) untouched.
- **Blast-radius confinement followed a cache instead of the cluster**
  (`internal/controller/noderesolution.go`). `nodeLabelsForConfinement`
  preferred the store's cached node record whenever it carried any labels at
  all. Both directions were wrong and one is dangerous: a cached label that
  still matches lets a destructive step run on a node the operator has just
  taken OUT of the declared radius, and a label that has not caught up yet
  refuses a node just brought IN — where a resolved non-match QUARANTINES
  rather than retrying, so that incident never recovers on its own. The
  platform is now asked first; on Kubernetes that is the watch-maintained
  informer cache, so it is both fresher and cheaper than an apiserver call.
- **Confinement was unresolvable for a node absent from the GPU inventory**
  (`internal/platform`, `internal/platform/kubernetes`,
  `internal/controller/noderesolution.go`). Labels were read through
  `ListNodes`, which is the GPU-filtered inventory — so a node that dropped
  out of it, a device plugin restarting or a driver reloading, made the blast
  radius permanently unknowable. Every destructive step then held forever and
  the machine sat cordoned and drained with nothing to advance it and nobody
  paged, in exactly the conditions remediation exists for. The codebase had
  already learned this for `NodeExists`; the same reasoning had not reached
  the more dangerous caller. New `platform.NodeLabeler` reads the Node object
  itself.
- **A device-scoped incident on a vendor this build cannot attest now
  resolves instead of dead-ending** (`internal/controller/reconcile.go`).
  `verifyRuntimeEvidence` read an NVIDIA accelerator report for every
  GPU-scoped incident regardless of the incident's vendor, and only the NVIDIA
  adapter exists — so on a perfectly healthy AMD node no report was ever
  produced, the incident held for the evidence deadline and then parked in
  `NEEDS_HUMAN`, **after the cordon and drain had already run**, with a reason
  naming no action anybody could take. Every such incident landed as
  `outcome="needs_human"`, so an AMD fleet's recovery report read 0% recovered
  for a system that had recovered them. Where this build has no runtime
  adapter for a vendor, verification now falls back to the same durable
  heartbeat a node-scoped incident uses, and says so — in the log and in
  `docs/reference-capabilities.md` — rather than presenting it as the same
  check. NVIDIA is unchanged and still fails closed on a missing report,
  because there the absence means a degraded agent, not an absent runtime.
  `TestAttestedVendorsMatchTheAdapters` fails the build if an adapter lands
  without being declared.
- **An incident learns its vendor from any signal that names one**
  (`internal/store/sqlcore/core.go`, `internal/controller/controller.go`,
  `internal/detect/xid.go`). `UpdateIncident`'s SQL did not list the `vendor`
  column, so the field was writable only at open — and only from a neutral
  fault envelope, since XIDs, Alertmanager alerts and manual triggers name no
  vendor. Because AMD and NVIDIA deliberately share problem classes, an
  incident opened by any of those and later joined by an AMD fault stayed
  vendorless. The column now persists, a later signal backfills it (first
  identification wins), and an XID declares NVIDIA — which is not an inference
  but what the encoding means.
- **A reset the node's runtime cannot serve is refused before the cordon**
  (`internal/controller/holders.go`). The incident-side vendor check cannot
  see a vendorless incident, so a manual trigger on an AMD node bound to a
  reset playbook passed every check, cordoned and drained the node, and only
  then failed at the reset step on missing evidence — a hold with no deadline,
  re-denying every tick, node left drained, nobody paged. The node's own
  reports now answer it. It requires positive evidence of the mismatch: a node
  that has simply not reported yet is left alone, because "no evidence yet"
  and "evidence of a different runtime" are not the same claim.
- **The device-holder preflight reads the incident's own vendor**
  (`internal/controller/holders.go`). It asked for an NVIDIA report on every
  incident, found nothing on other vendors, and silently no-opped — protecting
  nothing outside NVIDIA while appearing to run.
- **An idle guard that fails now stops the ladder regardless of WHY it
  failed** (`internal/controller/protection.go`,
  `internal/controller/execution.go`). The round-13 fix keyed that decision on
  the agent's refusal code, and an agent that predates the code sends none —
  so a new controller read the absence as "not a refusal" and escalated to a
  more destructive rung on a device the guard had just failed to clear.
  `docs/upgrade.md` mandates controller-first, so every not-yet-upgraded node
  in a rolling upgrade ran with the guard inverted. Absence of evidence must
  not select the destructive branch: the control decision now asks only "was
  this a guard?", while the protection metric keeps the narrower question,
  where under-counting is the honest direction. The regression test covers
  both skew directions — and its playbook now has a real escalation target,
  without which it passed either way.
- **The refusal code travels in a header, not the result body**
  (`pkg/types/types.go`, `internal/agent/agent.go`,
  `internal/httpapi/httpapi.go`). The result route strict-decodes, so the new
  field made every result from a newer agent a `400 unknown field "refusal"`
  on any controller that predates it — a rollback, or an upgrade that reaches
  agents first, would have had results rejected, retried, and timed out. The
  codebase already states this rule where the v2 registration route is
  defined; this broke it. `AgentActionRefusalHeader` is ignored by every
  version in both directions, and a test now fails the build if anything
  creeps back onto that body.

### Added
- **`kubeneuronctl report` answers the dry-run pilot's question**
  (`internal/controller/report.go`, `pkg/types/report.go`,
  `cmd/kubeneuronctl/report.go`). Dry-run is the shipped default and
  `docs/pilot-checklist.md` tells operators to stay in it until they have
  watched the system decide — for that whole period every number in the report
  was zero by construction, so the question that decides whether to enable
  enforcement had a blank table for an answer. Those incidents now aggregate
  into a separate `SIMULATED` section: how many the ladder would have carried
  to resolution, how many without asking anybody, and the GPU-hours involved.
  The headline stays dry-run-free — that exclusion is what keeps the one
  number this report exists to be trusted on from lying out of the box — and
  every simulated field is named in the conditional, because the degraded
  hours are real while the recovery is not.
- **`execution_mode` and `confinement` on `GET /api/v1/runtime-config`**
  (`internal/httpapi`, `cmd/kubeneuron-controller/reload.go`). Configuration
  reloads in place rather than rolling the Deployment, so an operator who
  switched `spec.safety.executionMode` had the CR, the config digest and
  `/readyz` all agreeing with them and no way at all to see whether the
  running process had picked the change up. A digest is the identity of a
  configuration, not evidence that its most consequential field took effect.
  Both fields are read from the live gate, so they report what IS, not what
  was requested.
- **`kubeneuron_agent_health_source`** (`internal/metrics`,
  `internal/agent/gpuhealth`). DCGM is the preferred second detection source
  and `nvidia-smi` is the narrower fallback, and nothing observable
  distinguished them: a fleet whose DCGM engine became unreachable degraded
  silently to a source that sees less. It also gave the hardware harness the
  positive signal it lacked — see below.
- **A real cordon and uncordon in the kind suite**
  (`hack/kind-integration.sh`). Every other phase runs in dry-run, so the
  cordon path had never cordoned anything and the cordon janitor — the code
  that gives a pilot its capacity back after an incident resolves — had never
  executed outside unit tests. The new phase arms one disposable worker,
  drives a cordon-only playbook for real, asserts the Node is unschedulable,
  resolves the incident, and asserts the janitor gives it back. The
  installation is restored to dry-run on any exit, including a failure
  mid-phase.

  It found four defects on its way to passing, every one of them invisible to
  unit tests because they live at the seam between the operator, a ConfigMap
  and a live cluster — and invisible to this suite before, because every other
  phase runs in dry-run and so had never switched the mode or applied
  confinement for real. All four are listed under Fixed.
- **A fleet-fit table** (`docs/pilot-checklist.md`, linked from the README).
  Every fact in it was already stated in the capability matrix; nobody
  assembled them into the question an evaluator actually has. NVIDIA on AWS is
  validated end to end; NVIDIA elsewhere tops out at a reboot rung that has
  never executed; AMD detects, protects and closes but arms nothing; Intel is
  a seam. The bare-metal row says plainly that such a pilot would be the first
  real per-device GPU reset anywhere.
- **`kubeneuron_degraded_gpus`, the gauge the counter cannot be**
  (`internal/metrics`, `internal/controller/reconcile.go`).
  `kubeneuron_degraded_gpu_seconds_total` is charged once, on the terminal
  transition — which is what stops a park/unpark cycle billing the same hour
  twice, and also means an incident sitting in `NEEDS_HUMAN` contributes
  **nothing** to it, permanently, for exactly the population whose capacity is
  most thoroughly lost. `kubeneuronctl report` deliberately keeps charging
  those, so the two answers disagreed while both claimed to measure
  "GPU-seconds under an open incident". The new gauge reports what is degraded
  right now, split by whether automation still owns the incident or a human
  does; the counter's help text and `docs/reference-metrics.md` now say what
  each one actually measures and how to read them together.
- **`make gates` and `make gates-full`.** The documented gate set lived only
  as CI job steps and as prose in a checkpoint document — survivable while
  every push ran CI, not once Actions was disabled on the development
  repository. Two tiers on purpose: bundling a four-second check with a
  forty-minute one is how the four-second check stops being run.
- **`hack/mirror.sh`.** The mirror procedure is now the enforcement boundary
  between development and public CI, and it was a sequence of commands typed
  by hand with a five-item exclusion list held in somebody's head. It now
  lives in one place, and the script asserts afterwards that the published
  tree differs from the development tree by exactly those exclusions — the
  check the hand-typed procedure never made. It prepares and reports; it does
  not commit or push.
- **The kind integration suite now builds the image that actually ships**
  (`hack/kind-integration.sh`). It packaged the host-built binaries into `FROM
  scratch` — an artifact nobody deploys. The real one is distroless, carries
  `nsenter` and `dcgmi` that the agent shells out to, and runs as a different
  user, so every property that divergence hid was invisible to the one suite
  best placed to catch it. That is how `Reboot` reached live hardware and
  exited 127 for want of `nsenter`. The whole 73-check suite passes against
  the production image; set `INTEGRATION_IMAGE_DOCKERFILE` to the old file for
  a fast local loop.
- **`hack/verify-image.sh` and a CI job that runs it.** Its host-tool check
  asserts on the image filesystem, not on a command's output: the first
  version grepped the combined output of running the binary, and when the
  binary is absent Docker's own error (`exec: "/bin/nsenter": no such file`)
  contains the word — so a gate added specifically to catch a missing
  `nsenter` passed an image with no `nsenter`. Verified against the controller
  image, which has neither tool.  Builds all four
  published targets with the classic builder — the path `make docker` takes,
  which has broken on its own — then asserts what only the artifact can show:
  every entrypoint starts and names itself, the agent carries a working
  `nsenter` and `dcgmi`, each image runs as the user its deployment assumes
  (root for the agent, 65532 for the rest), and the OCI source and licence
  labels are present.
- **`hack/verify-release.sh` and a `release-verify` job.** Everything else
  verifies inputs; nothing looked at the finished release the way a stranger
  does. It downloads the published assets and checks the asset set is closed
  (a release once shipped CI diagnostics as user-facing assets), that the
  checksum file covers *every* asset rather than merely matching the ones it
  lists, that the cosign signature over it verifies against the repository's
  GitHub OIDC identity, and that what a user applies names the digests
  `images.txt` names.
- **A release is gated on a rehearsed upgrade** (`upgrade-rehearsal` in
  `release.yaml`). `hack/kind-upgrade.sh` ran by hand before the last two
  tags; no tag can now be published without it converging the previous
  release to HEAD in the documented order on a real cluster.
- **`hack/verify-spec-paths.py`, wired into the docs lint.** Every `spec.a.b`
  path a published document names must exist in a generated CRD.
  `docs/upgrade.md` named `spec.execution.confinement` twice — in the section
  written specifically to make a blast-radius change safe to upgrade through
  — and every other check passed. An invented field is the most expensive
  kind of documentation defect: it reads as authoritative, an operator acts on
  it, and the action silently does nothing. Justified exceptions live in
  `hack/spec-path-exceptions.txt` with their reason beside them.
- **The hardware DCGM phase can now fail** (`hack/hw-e2e.sh`). Its fallback
  branch asserted only the absence of a parse warning, and `parseDCGMXID` had
  no code path that could emit one — so the phase passed without exercising
  DCGM at all, and v0.2.2's release notes then cited it as evidence. It now
  makes two assertions the code can violate: no unreadable-layout warning, and
  `kubeneuron_agent_health_source` reporting `dcgm` as the source actually
  serving the node. The second closes the hole the first cannot: a parser that
  is never invoked also emits no warning.
- **The DCGM parser reports output it cannot read**
  (`internal/agent/gpuhealth`). `parseDCGMXID` silently skipped every
  unreadable line, so a DCGM release that changed the `dmon` layout would have
  produced zero candidates forever — a detection source going dark with
  nothing anywhere saying so. Silence is still correct for the fault path; the
  count is not. This also made the hardware harness's DCGM fallback assertion
  ("no gpuhealth parse warnings") vacuous, because no code path could emit
  one.
- **The Grafana dashboard is checked against the code**
  (`internal/metrics/dashboard_test.go`). The dashboard was the only shipped
  artifact whose correctness nothing compiled: a renamed metric or a dropped
  label does not break a build, does not fail a test, and does not error at
  query time — the panel just renders empty, which reads as "nothing is wrong"
  precisely when somebody is looking for what is wrong. Two tests now fail the
  build if a panel queries a metric this build does not register, or groups by
  a label its metrics do not carry.

### Changed
- **The released installer pins the controller and the agent by digest**
  (`deploy/install.sh`, `.github/workflows/release.yaml`). The install
  manifest was carefully digest-pinned and even refused any tag-shaped
  reference — but it only carries the operator. `install.sh` resolved the
  controller and the agent, the two components that actually run the fleet, as
  moveable tags, so "digest-pinned install" was true of one image out of
  three. The release now stamps the published digests into the installer it
  ships, and `hack/verify-release.sh` fails a release where it has not.
- **Fleet membership is anchored to recognised vendor domains, and the
  blast-radius change v0.2.2 made silently is now disclosed**
  (`internal/platform/kubernetes`). v0.2.2 replaced the `nvidia.com/gpu`
  test with a vendor-neutral one so AMD and Intel nodes stop being
  invisible; what its notes failed to say is that this also widened the set
  of machines a playbook may cordon, drain, taint and reboot. The matcher
  had no lower bound either — it accepted any domain whose resource merely
  looked GPU-shaped, so a third-party counter such as
  `example.com/gpu-licence` admitted a CPU node to the fleet.

  Fleet membership now requires the resource's domain to be one KubeNeuron
  recognises (`nvidia.com`, `amd.com`, `intel.com`, `gpu.intel.com`,
  `habana.ai`, `aliyun.com`). A GPU-shaped resource from any other domain is
  ignored for membership and logged once, loudly, because an accelerator we
  fail to recognise is an invisible node and that is the other failure worth
  catching.

  The eviction path keeps the generous matcher on purpose. The asymmetry is
  the point: over-matching there costs one extra eviction on a node already
  being drained, while under-matching leaves a live training job on a device
  about to be reset. Over-matching on the membership side hands a playbook a
  machine it should never have been offered.

  [`docs/upgrade.md`](docs/upgrade.md) now opens with the audit — a `jq`
  one-liner that prints the fleet as the new matcher sees it, so you can
  diff it against `kubeneuronctl nodes` before upgrading.
- **`docs/reference-capabilities.md` stopped contradicting its own table.**
  The "two silent no-ops" section still described both vendor-neutrality gaps
  as open after they had been closed, and its AMD fault-row count was a number
  somebody typed once. `hack/verify-docs.sh` now derives that count from
  `internal/detect/fault.go`, so a new `(vendor, code)` row the matrix does not
  know about fails the build. Dashboard panels built on `increase()` carry an
  explicit counter-reset caveat: these are process-lifetime counters and a
  controller restart reads as a cliff.

### Fixed
- **Dry-run no longer writes a real taint, and an unparked incident is marked
  again** (`internal/controller/degradedtaint.go`). A dry-run installation was
  writing genuine `NoSchedule`/`PreferNoSchedule` marks — a cluster-wide
  scheduling change for every workload, from a mode whose whole promise is
  that it changes nothing. Separately, halting removes the mark, so an
  incident a human sent back from `NEEDS_HUMAN` to `EVALUATING` ran unmarked
  until the janitor happened to notice: the scheduler was free to pile new
  work onto failing hardware precisely while somebody was working the
  incident.
- **The taint janitor confirms before it removes a mark**
  (`internal/controller/degradedtaint.go`). It lists marked nodes and then
  lists open incidents; anything that opened between the two reads was
  invisible to the second, and the node looked abandoned. Removing on that
  basis returns a node to the scheduler's rotation while a fault is being
  worked on it, so the removal is now confirmed with a per-node read. Leaving
  a stale mark one pass longer costs nothing and self-heals; the other
  direction does not.
- **A busy device and a broken idle probe are no longer the same event**
  (`internal/agent/nvml`, `internal/agent/executor`, `internal/controller`).
  Every idle-guard failure counted as `kubeneuron_destructive_steps_deferred_
  total{reason="not_idle"}` — including a missing `nvidia-smi`, a wedged
  driver, and a timeout, none of which is evidence that a workload was spared.
  The agent now reports `ActionResult.Refusal` (a wire field with a closed set
  of codes) and only a real refusal counts; an agent too old to send the field
  under-counts protection rather than inventing it.
- **An idle refusal stops the ladder instead of escalating it**
  (`internal/controller/execution.go`). A guard that refused because live work
  held the device escalated to the failure playbook, whose rungs are by
  construction bigger hammers than the one just stopped — the guard became a
  trigger for something more destructive. The incident is now handed to a
  human with the holders named in the audit trail, keeping whatever cordon the
  ladder already applied.
- **A replaced AMD GPU no longer inherits its predecessor's counters**
  (`internal/agent/amdhealth`). Counter series are keyed by GPU index (the
  `rocm-smi` fallback reports no UUID, so UUID keying would re-baseline on
  every failover and hide a real increase), which meant an RMA'd device was
  compared against the dead one's lifetime totals. A replacement reading
  HIGHER replayed the difference as one enormous delta and opened a critical
  uncorrectable-ECC incident on a GPU installed minutes earlier. A change
  between two known UUIDs on the same index now starts the series over.
- **`kubeneuron_workloads_evicted_total` lost its `node` label**
  (`internal/metrics`). Node names are not a bounded set in a KubeNeuron
  fleet — this control plane replaces nodes, so every `ReplaceNode` mints a
  name that never recurs, and an autoscaler mints more. The series grew for
  the life of the process and never shrank. Per-node detail is in the incident
  record and audit trail, where querying it costs no retention. **Dashboards
  or alerts that group this counter by node must drop the grouping.**
- **The recovery report refuses rather than truncating**
  (`internal/controller/report.go`). The query behind `kubeneuronctl report`
  had no bound, so a large fleet over a long window loaded every incident into
  memory at once. A plain `LIMIT` would have been worse: a truncated set still
  aggregates cleanly and returns a capacity number that looks authoritative
  and is wrong. It now reads one row past a 100,000-incident cap and asks for
  a narrower window if it hits it.
- **The GPU-inventory fallback says when it is guessing**
  (`internal/controller/reconcile.go`). A node-scoped incident whose node
  inventory could not be read is charged one GPU; on an 8-GPU node that
  understates recovered capacity eightfold, and the store error behind it was
  silently swallowed. It is now logged, and distinguished from a node that
  genuinely has no registered inventory.
- **The amdgpu kernel parser no longer reads two all-clear lines as faults**
  (`internal/agent/kmsg/amdgpu.go`). `ring sdma0 timeout, but soft recovered`
  is the driver reporting that it killed the offending job and the engine
  kept running — no reset, no lost device — and it was being classified as a
  ring-timeout hang, which cordons and drains a node that never stopped
  working. Worse, a kernel that soft-recovers once tends to do it on a loop.
  Separately, `no uncorrectable hardware errors detected` contains the fault
  spelling as a literal substring, so every clean RAS poll opened an ECC
  incident on a healthy GPU — the loudest possible false positive, on the
  class that ends in node replacement. Both lines now produce a logged
  refusal instead of a fault. The test that asserted the soft-recovery line
  WAS a fault had encoded the bug; it now asserts the refusal.
- **XGMI link errors need a reporting delta before they page anyone**
  (`internal/agent/amdhealth`). The fabric counter reported on any increment
  while classifying CRITICAL, and it moves on corrected link retries as well
  as on real degradation. It now uses the same accumulate-then-report rule as
  the correctable-ECC rate, with `--amd-xgmi-link-min-delta` to set it from a
  real fleet's baseline.
- **A retired page and an exhausted spare-page budget are no longer the same
  event** (`internal/agent/amdhealth`, `internal/detect/fault.go`). Every
  retirement mapped to `ClassRowRemapOK` — the recovery mechanism working —
  with nothing that could ever say the device had run out of spares to retire
  into. The new `amd/page-retirement-exhausted` code maps to
  `ClassRowRemapFailure`, the peer of NVIDIA's XID 64, and is emitted as a
  LEVEL rather than a counter so a GPU already past its budget when the agent
  starts is reported instead of being absorbed by the startup baseline.
  `--amd-bad-page-threshold` is unset by default and makes no claim until an
  operator supplies the number: the budget is SKU-specific and guessing it
  would end in an unwarranted node replacement.
- **Node inventory counts devices for the vendor that advertises them**
  (`internal/platform/kubernetes`). The device count was still read from
  `nvidia.com/gpu` alone, so every AMD and Intel node was inventoried as
  zero GPUs and fell through to the report's `AssumedSingleGPU` fallback:
  the recovered GPU-hours of a non-NVIDIA fleet were charged at one device
  per incident no matter how many were really degraded. Counts now come
  from whichever recognised vendor resource the node carries, preferring
  whole devices over MIG partitions so the mixed strategy does not
  double-count the same silicon, and ignoring `gpu-mem`-style resources
  that measure MiB rather than devices.

## [v0.2.2] - 2026-08-05

Release evidence: full CI (both store backends plus the kind integration
suite) green as the tag's gate; `hack/kind-upgrade.sh` converged
v0.2.2-rc.3 → HEAD in the documented order with a seeded incident and its
audit trail surviving the store migration; and a live hardware run
(kubeneuron-e2e11, EKS g4dn.xlarge / Tesla T4) passed all five phases —
the XID-79 dry-run ladder with the approver audited, a DCGM phase, a
recurrence during VERIFYING escalating to NEEDS_HUMAN instead of
resolving, XID-92 threshold accumulation, and a confined `ReplaceNode`
terminating the real instance — with teardown sweeping the account to
zero leftovers. Three release-candidate cuts preceded this tag and each
found one defect the pipeline could not show any other way.

**Correction (2026-08-10):** this paragraph originally described the DCGM
phase as "the DCGM source parsing live `dmon` output". The run log shows
that phase took its fallback branch (`DCGM injection unavailable`), and
that branch asserts only the absence of `gpuhealth` parse warnings — which
`parseDCGMXID` has no code path to emit. The assertion could not fail, so
the phase proved nothing about DCGM. The DCGM source remains **not
hardware-validated**, exactly as `docs/reference-capabilities.md` has said
throughout; only this release-evidence sentence overstated it. The harness
assertion is being replaced with one the parser can violate.

### Added
- **AMD detection, so "vendor-neutral" is a fact and not only a seam**
  (`internal/agent/amdhealth`): `amd-smi metric --json` with a
  `rocm-smi` fallback, emitting neutral `(vendor, source, code)` faults —
  `ecc-uncorrectable`, `ecc-correctable-rate`, `page-retirement`,
  `xgmi-link-error`, `thermal-throttle`, `gpu-lost` — through the same
  event path as the NVIDIA source, with the same fail-closed posture
  (real-binary gate, counters baselined on the first poll, absent ≠ zero,
  unparseable output degrades to observed-only). The kernel watcher gained
  the `amdgpu` line families on the SAME cursor, boot-scoping, and ack
  watermark. Every problem class reachable through the XID table is now
  also reachable through a neutral `(vendor, code)` row, and a test fails
  the build if that ever stops being true. **The fixtures are synthetic:
  the amd-smi JSON schema is reconstructed, not captured from hardware —
  the capability matrix says so.**
- `docs/reference-capabilities.md` — a per-vendor capability matrix whose
  every cell is derived from the code and cites the file that backs it,
  machine-checked by `hack/verify-docs.sh` in BOTH directions: an
  implementation that lands without a matrix update fails CI, and a matrix
  that claims support with no implementation behind it fails too. It
  earned its keep immediately, catching both directions during this
  change.
- `docs/mig-decision.md` and `docs/checkpoint-coordination-design.md` —
  the plan's two "decide before building" items, each ending in a
  recommendation rather than a survey: the physical GPU stays the
  remediation unit (per-instance reset is rejected permanently, not
  deferred), and checkpoint coordination is designed as an opt-in Pod
  annotation with a controller→workload notification patched onto the Pod
  itself, because an HTTP call from the controller to a workload endpoint
  would be an SSRF primitive aimed at the component holding cluster and
  cloud credentials.
- Protection is now countable (`kubeneuron_workloads_evicted_total`,
  `kubeneuron_destructive_steps_deferred_total` over a closed set of
  reasons): the number of times automation chose NOT to disrupt was
  previously invisible, which is why the clause was undersold. Twelve
  deferral paths are instrumented; dry-run incidents and observe-only
  playbooks are deliberately excluded, because neither was ever going to
  touch a workload.
- Opt-in degraded-node taint (`spec.safety.taintDegradedNodes`, default
  off, `PreferNoSchedule`): a node under an open incident stops attracting
  new GPU work before it is cordoned. A janitor converges the marks both
  ways from what the cluster reports, so a controller that dies mid-mark
  cannot leave one behind.
- `kubeneuronctl report --since 30d` and a recovery row at the top of the
  Grafana dashboard: GPU-hours degraded and recovered, the share recovered
  without a human, cost by class, MTTR percentiles, and what is still
  open. The report computes from the incident store rather than
  Prometheus — exact instead of bucket-interpolated, survives a counter
  reset, and works on a fresh install with no monitoring stack — through a
  new `GET /api/v1/report/recovery` so "recovered" is defined once,
  server-side, instead of once per client.
- **The product definition is now a gradeable contract.** KubeNeuron is
  "a vendor-neutral GPU fleet reliability control plane that detects
  degradation, protects workloads, automates safe recovery, and measures
  recovered" — and `docs/definition-plan.md` grades every clause against
  the code (shipped / partial / stated), with the work that closes each
  gap and the release it lands in. Vendor-neutrality is stated honestly:
  the fault envelope and accelerator seam are vendor-agnostic by
  construction, every shipping detection path is NVIDIA, and the AMD
  track is planned rather than implied.
- Recovery outcome metrics — the "measures recovered" clause, made true:
  `kubeneuron_incident_duration_seconds{class,outcome}` (MTTR, split by
  how the incident ended), `kubeneuron_incidents_recovered_total{class,unattended}`
  (what the automation absorbed without waking anyone), and
  `kubeneuron_degraded_gpu_seconds_total{class,outcome}` (GPU-seconds
  under an open incident; the resolved share is capacity returned to
  service). Emitted once per incident on the committed halting
  transition, scaled by the node's accelerator inventory for node-scoped
  incidents. Everything else the controller exports is process telemetry;
  this is the outcome a capacity owner budgets against.
- Self-health alert rules for KubeNeuron itself
  (`configs/vmalert/self-rules.yaml`, mirrored into the deployed VMRule and
  pinned by a unit test): auth-failure bursts, failing stack restores, a
  stuck action queue, expired approvals, rejected agent events, an agent
  that is alive but **never acknowledged** (which `KubeNeuronAgentDown`
  cannot see, because a rejected registration keeps serving metrics), and a
  slow reconcile walk. Each links to a new runbook entry with
  detection → diagnosis → the safe action. Seven metrics that shipped with
  no alert and no documentation now have both.
- `docs/pilot-checklist.md`: the ordered path from a green install to a
  first incident on a real EKS GPU cluster — real observability endpoints
  (install.sh writes placeholders), the webhook token copy, a policy set
  that covers more than one class, `hostTooling`, and a synthetic webhook
  injection that proves the pipeline without waiting for a fault.
- The documented SQLite restore is now executable and rehearsed: a
  `restore-helper.yaml` pod mounts the state volume (the controller image
  is distroless and the claim is RWO, so the old "copy into a helper pod"
  instruction had no helper pod to copy into), the procedure is numbered
  end to end, and the kind suite runs backup → wipe the live database →
  restore → assert the incident history survived on every run.
- The controller publishes the identity of the configuration it is actually
  running: the operator-compiled snapshot digest appears on `/readyz`
  (`ready config=<digest>`), as the `kubeneuron_runtime_config_info` metric,
  and via `GET /api/v1/runtime-config` (digest + load time + shape counts).
  A digest lagging `KubeNeuron.status.configDigest` is a config rollout that
  has not landed — previously invisible outside controller logs, which is
  exactly how the first live hardware run lost an incident to it.
- Narrow late-bind: an incident that opened BEFORE its policy existed
  (playbook binding is open-time) is bound in OBSERVING once a matching
  policy appears — with the write-fence `StateChangedAt` bump and a
  `bind-playbook` audit row. Only never-bound incidents, only in OBSERVING;
  bound incidents are never moved between playbooks.
- Approve/reject decisions land in the audit trail at decide time, carrying
  the `reason` the API and CLI accept (it was previously dropped silently);
  the entry is `approval-approved`/`approval-rejected` under the deciding
  actor.
- A MIG compute-instance UUID (`MIG-…`) is refused by the `gpu_reset`
  preflight fail-closed: the physical GPU is the remediation unit
  (documented as a design invariant); per-instance reset would take down
  every instance on the parent on the evidence of one.
- Documentation anti-rot: `hack/verify-docs.sh` (wired into a cheap
  `docs-lint` workflow) fails the build on forbidden stale-status claims
  (`hack/stale-claims.txt`) and on migration heads that disagree with the
  filesystem. Its first run immediately caught a fifth "PostgreSQL is not
  implemented" claim surviving in `deploy/kubernetes/dependencies/`.
- Two hardware-harness phases written for the next paid run (marked
  unexercised): `test-dcgm` (DCGM field-value injection exercising the
  gpuhealth source, with a parse-clean fallback) and `test-verify-recur`
  (a recurrence during VERIFYING must escalate, not resolve). Plus
  `docs/hw-e2e-dispatch.md`: the exact OIDC trust/permission policy recipe
  for dispatching hw-e2e from GitHub Actions (deliberately not scripted —
  durable account infrastructure).

### Changed
- The release workflow is gated and digest-pinned: publishing jobs run only
  after the FULL CI (build/vet, both store backends, kind integration)
  passes for the tag; the single-file install manifest pins images by
  digest (a re-pushed tag can never change what a downloaded manifest
  installs); the GitHub release publishes non-draft with the image-digest
  table and a signed checksums file; per-job least-privilege permissions.
  `hw-e2e.yaml` gains the `test-threshold` phase the harness already had.

### Fixed
- Two silent no-ops the capability audit found, both wrong in the
  dangerous direction: a hardcoded `nvidia.com/gpu` decided what counts as
  a GPU, so an AMD or Intel node inventoried as ZERO GPUs — invisible to
  the entire control plane — and `evict_gpu_workload` reported "evicted 0
  GPU workloads" as success while leaving live jobs on a device the ladder
  was about to reset, which is the exact disruption that step exists to
  prevent. Resource matching is now vendor-neutral (including NVIDIA MIG
  partitions) and errs toward recognising an accelerator; the eviction
  step says plainly when it matched nothing and how many pods it
  considered.
- **The round-11 arming-grace fix inverted the bug it was closing.** The
  hold anchor was cleared on every non-hold verdict, including the
  `proceed` returned for steps that are not agent-destructive at all — and
  the playbook-scope preflight walks EVERY step each pass, so on the
  standard cordon→drain→reboot ladder the first two rungs reset the third's
  anchor every pass. The grace never expired and a never-armable in-scope
  node held in EVALUATING silently forever: no escalation, no page, no
  audit row. The anchor is now touched only by verdicts about
  agent-destructive steps, and a refusal leaves it for the escalating
  transition to clear (clearing before the commit would grant a fresh
  grace after every transition conflict). Regression test uses a
  three-step ladder — the shape that was broken.
- The published runtime-config identity could go permanently stale: the
  reload change-detector hashed the configuration files but not the
  operator's `config-digest` markers, and the policies and playbooks
  ConfigMaps sync independently — so a playbooks-only rollout never
  triggered a reload and the advertised digest stayed wrong forever. The
  markers now feed the detector, disagreeing markers (mid-sync) publish no
  identity at all rather than one describing half of what is loaded, and a
  vanished marker clears the info metric instead of leaving a stale series.
- Release pipeline, all latent since before v0.2.1: images were published
  to the **legacy hyphenated GHCR namespace** no manifest, chart, doc, or
  upgrade harness has referenced since v0.1.x (the digest-pinned manifest
  job would have failed deterministically on the first real tag); a
  prerelease tag would have become `releases/latest`, which is what the
  advertised install one-liner downloads; and the arm64 image compiled
  under QEMU emulation because the build stage lacked
  `--platform=$BUILDPLATFORM` despite cross-compile args being plumbed.
  The install-manifest tag-leak assertion now rejects any tag-shaped
  reference, not only `v`-prefixed ones.
- `hack/kind-upgrade.sh` has been unrunnable since 2026-07-28: one image
  path was missed in the namespace rename, so the substitution guard
  killed every run. Fixed, and a baseline whose release published no
  assets (a release run that died early) now falls back to building the
  install manifest from that tag's own tree.
- The two hardware-harness phases written blind in round 11 were both
  broken in ways only a paid run would have revealed: `test-dcgm` probed
  the agent's metrics with `wget` inside a **distroless image that has
  neither wget nor a shell** (now a disposable curl pod against the agent
  Pod IP), and `test-verify-recur` re-injected the same XID inside the
  agent's 2-minute dedup window (silently swallowed) and left a
  NEEDS_HUMAN incident that the destructive phase's fault would have
  ATTACHED to, failing the next phase deterministically (now spaced past
  the window and resolved at the end).
- The hw-e2e reaper — the watchdog whose failure means "a paid cluster may
  be leaking" — could never have assumed the documented OIDC role: it
  declares no environment, so its token subject is the branch form and an
  environment-scoped secret is invisible to it. `docs/hw-e2e-dispatch.md`
  now documents both subjects and a repository-level secret.
- The docs-lint's migration-head check could not fire on the drift it
  guards (a commit adding only a `.sql` file matched no trigger path).
  Its first run after the fix caught three more stale claims: PostgreSQL
  "reserved, operator rejects it" and "child-resource status is still work
  to do" in design.md, and pre-v0.2 "preview/unfinished" language in the
  dependency profile.
- The arming-propagation grace is anchored to the FIRST observation of the
  hold (in-memory, cleared on any transition), not to `StateChangedAt` —
  which pre-ages while an incident sits in EVALUATING behind a per-node
  pause, maintenance window, or playbook cooldown, and made the very first
  arming check after such a hold escalate to a human with a false
  "never-armable" diagnosis when the agent was one registration tick from
  arming (round-11 review F1). The escalation message now reports the
  actually-observed grace.
- The Helm chart's ClusterRole regained the operator's `secrets` rule
  (create/get/list/update/watch): the PKI work added it to `config/rbac`
  but not the chart, so a Helm-installed operator could not issue or renew
  TLS material — caught by the public CI's chart-vs-kustomize assertion,
  which fails the whole run on any rule divergence.
- Cheap round-11 review cleanups: design.md's route table names the real
  `POST /api/v1/login` route; the README describes the live destructive
  run's actual closure (resolved through the verification quiet window,
  not the vanished-node janitor's "replaced" path); docs/upgrade.md
  documents the one-time double workload roll caused by the per-workload
  TLS digest format change; a cancelled hw-e2e run no longer leaks the
  operator-image repoint loop on the runner.

## [v0.2.1] - 2026-08-05

### Added
- Second GPU fault detection source beside the kernel log: a DCGM poll
  (`dcgmi dmon -e 230`) with an `nvidia-smi -q` ECC/row-remap fallback,
  normalized into the same event and deduplicated against kmsg within a
  short window so one fault never opens two incidents. Runs only on the
  real driver.
- Vendor-neutral fault envelope on the agent event (`AgentEvent.Fault`,
  a `FaultSignal` of vendor/source/code/attributes): an honest landing
  place for a fault that is not an XID. The `nvidia-smi` ECC/row-remap
  fallback now emits neutral NVIDIA faults (`ecc-dbe`, `row-remap-failure`)
  instead of synthesizing fake XID 48/64; XID stays the NVIDIA-native
  encoding for the kmsg line and DCGM's real last-XID field. The detector
  maps `(vendor, code)` to the same `ProblemClass` the former synthesized
  XID produced, and cross-source dedup now keys on that shared class so a
  kmsg XID and the neutral fallback fault for one condition still collapse
  to one incident. A future AMD/Intel source adds a `FaultSignal` and a
  `faultTable` row with no XID pretense. Additive and optional; the wire
  stays backward-tolerant. (`GPUSignalMapping` neutralization is follow-on.)
- Hardware GPU end-to-end CI target (`.github/workflows/hw-e2e.yaml`,
  `hack/hw-e2e.sh`): `workflow_dispatch` + weekly cron on ephemeral EKS
  with always-on teardown and a leak sweep, gated behind a typed
  confirmation and the `gpu-lab` environment; a separate reaper workflow
  force-deletes any cluster past its lifetime. Per-commit CI stays
  CPU-only. **Now proven against a live lab** (kubeneuron-e2e10,
  us-east-1, g4dn.xlarge/Tesla T4): all three test phases pass — the
  XID-79 dry-run ladder (cordon→drain→approval→dry-run reboot, ~90s, the
  approval-round notification and `token:`-prefixed approver in the
  audit), the confined destructive ReplaceNode (real
  `ec2:TerminateInstances` under the run-scoped IRSA role, nodegroup
  replacement, resolved through the ladder), and a NEW `test-threshold`
  phase — XID 92, the only code with distinct pipeline behavior: one
  signal must hold in OBSERVING, the third (each injected past the
  agent's 2-minute dedup window) crosses the policy threshold and the
  observe-only ladder resolves. Running it for the first time surfaced
  and fixed twelve dead assumptions in the harness (EOL EKS default,
  pre-refactor `spec.cloud` shape, the XID *name* used where the problem
  *class* belongs, `NEEDS_HUMAN` instead of `AWAITING_APPROVAL`, a
  nonexistent "Approve" audit action and the un-prefixed approver, the
  nested detail response, halted incidents matched by `wait_for_incident`,
  the operator-image repoint racing `install.sh`'s re-apply, an invented
  mapping source, asserting the vanished-node janitor's closure on the
  happy path, no wait for the controller's ConfigMap hot-reload, plus an
  `up-finish` resume command) — and two real product gaps now fixed in
  their own entries (GPUSignalMapping status, TLS revision scope).

### Added
- `GPUSignalMapping` reaches the neutral fault encoding: source `"fault"` with
  `spec.faults[]{vendor, code}` overrides classification of vendor-neutral
  fault codes, so remapping a condition (e.g. `nvidia/ecc-dbe`) applies to the
  nvidia-smi/DCGM fallback source exactly as `xidCodes` applies to the kmsg
  source — previously a user-visible policy split between the two encodings
  of one physical condition. An override can also make an unknown vendor code
  actionable. CEL matrix +3.
- Per-instance recycle viability at admission: cloud providers now answer
  `CheckRecycle` for the exact instance, beyond their static capability. The
  AWS provider refuses stop/start for autoscaling-group members (the group's
  health check terminates a stopped member mid-recycle); the controller
  escalates a `recycle_node` step to the next rung the moment it becomes
  current — before requesting approval — instead of a human approving a step
  that fails by timeout. `RecycleNode` re-checks before issuing any stop.
- Fleet-wide detection under `executionMode: Enabled`: arming narrows the
  primary agent DaemonSet to the destructive blast radius, which silently
  dropped fault detection on unarmed GPU nodes. The operator now maintains a
  detection-only companion DaemonSet (`<name>-agent-detect`) on exactly the
  complement (required anti-affinity per destructive selector key), removed
  again outside Enabled. The agent authenticator accepts both DaemonSets
  (`--agent-daemonset` is comma-separated) with the owner UID still verified
  against the live object; readiness covers the companion and tolerates an
  empty complement.

### Fixed
- The kmsg watcher no longer loses XIDs printed while the agent was down:
  a durable, crash-safe sequence cursor resumes from the oldest buffered
  record. The cursor and the cross-source dedup set now advance only after
  an event is accepted by the controller or fsynced to the spool, so a
  failed or interrupted delivery replays instead of being dropped.
- The kmsg cursor is now boot-scoped: `/dev/kmsg` sequence numbers restart
  after a reboot, so a stale pre-reboot cursor no longer suppresses every
  XID printed after a node reboot (a normal escalation-ladder rung).
- The event spool repairs a torn final line on open instead of gluing the
  next append onto it and silently dropping both on replay.
- Cross-source deduplication no longer opens two incidents for one fault
  when the kmsg side cannot attribute the GPU (the expected XID 79 case,
  where the device vanishes from `nvidia-smi`): it falls back to a
  node+XID key so the DCGM observation collapses against it.
- `gpu_reset` resolves the target index from the incident's GPU UUID at
  execution time and fails closed if the UUID is absent, gone, or now maps
  to a different index — a topology change (XID 79, driver reload) can no
  longer reset a healthy neighbor by a stale index.
- `UpdateIncident` uses optimistic concurrency (a version guard) so a
  concurrent signal ingest and a step-completion write can no longer lose
  an update or, on PostgreSQL, regress an incident's state/step and
  re-execute a non-idempotent platform action.
- Rewinding an incident to its quiesce step no longer collides action IDs
  with the completed original, so the re-quiesce actually re-executes
  instead of replaying a stale success and escalating on it.
- Controller-side destructive steps (cordon/drain/recycle/replace) are now
  confined to `spec.safety.destructiveExecution.nodeSelector`: a non-dry-run
  incident targeting a node outside the selector (e.g. from an Alertmanager
  webhook) fails closed to NEEDS_HUMAN instead of executing.
- An incident whose GPU could not be attributed (empty UUID — the expected
  XID 79 case) no longer cordons and drains a node and then holds forever on
  a reset it can never satisfy; the impossible reset is refused up front and
  the incident goes NEEDS_HUMAN.
- The event spool now self-heals a torn tail left by a failed in-process
  append, not only one found at open, so the next event cannot be glued onto
  a partial line and lost.
- The DCGM/nvidia-smi source persists its emitted (GPU, XID) set boot-scoped,
  so an agent restart (e.g. a DaemonSet rollout) no longer re-emits a
  retained last-XID and re-opens incidents for long-remediated faults.
- The safety gate rebuilds its concurrency/reboot occupancy from durable
  EXECUTING incidents on leader acquisition, so the concurrency caps hold
  across a controller failover.
- Smaller: the unattributed dedup key keeps the PCI address so two GPUs
  failing with the same XID on one node stay distinct; `paramInt` rejects
  trailing garbage; `CompleteAction` returns 5xx (not 403) on a store outage.
- Action cancellation now actually works: the incident ID is carried on the
  queued action (it was smuggled through an unset param, so every enqueued
  action had an empty `incident_id` and `CancelPendingActionsForIncident`
  matched nothing) — a superseded destructive step is tombstoned instead of
  being handed to a returning agent hours later; the claim query also skips
  actions of terminalized incidents and dead-letters poison actions/events.
- The Alertmanager webhook fails closed when no token is configured, validates
  the alert severity, and drops alerts for nodes not in inventory — a spoofed
  label can no longer drive cordon/drain on an arbitrary node.
- A threshold-crossed incident with no bound playbook no longer livelocks
  OBSERVING↔EVALUATING (it holds/quiet-resolves); a destructive step whose node
  labels are transiently unresolvable holds-and-retries instead of quarantining
  terminally; an approval is never recorded against an unresolvable step.
- Quiesce host state survives a partial-quiesce retry (it no longer conflicts
  the stored pre-mutation snapshot against the partially-mutated live state);
  `waitDeviceReleased` is bounded so a never-releasing holder can't hang the
  agent's action loop; the reset's idle/holder preflights and persistence-mode
  restore address the GPU by UUID, matching the reset itself.
- PKI leaf renewal runs independently of config compilation (an inert
  playbook can no longer freeze it), a managed leaf that no longer chains to
  the current CA is reissued, and the password-login and bearer throttles no
  longer lock out valid operators behind a shared NAT while still rate-limiting
  bad credentials. OIDC surfaces an id-token claims-decode error and sets
  cookies Secure under proxied TLS.
- "Destructive" is now an `internal/action` registry fact rather than a
  hand-maintained map, so the controller-side blast-radius confinement can
  never drift from the action set.
- The PostgreSQL action queue no longer double-leases one action to
  concurrent claimers (the outer update re-checks the claimable state; the
  event outbox uses `FOR UPDATE SKIP LOCKED`), preserving one lease per node.
- Device holders are now persisted, so the pre-disruption reset refusal and
  `spec.safety.quiesce.forbidResetWhenPresent` actually take effect (they
  were dead code — the store dropped the field on upsert).
- The kmsg cursor advances only to a contiguous-ack watermark, so an event
  that fails both delivery and spool is no longer skipped when a later event
  is acknowledged.
- The DCGM/nvidia-smi source emits a fault that first appears after startup
  (rather than baselining it away permanently), and same-source distinct
  XIDs that share a problem class are no longer over-collapsed by dedup.
- Controller-side escalation rejects cyclic ladders at compile/load, caps
  attempts to `NEEDS_HUMAN`, and honors cooldown on escalated rungs — no
  unbounded destructive loop; an obstructed reset escalates instead of
  looping in `EVALUATING`.
- An approval is bound to the specific step (playbook + name + action +
  content hash); a hot config reload that swaps the action under a granted
  approval re-parks instead of executing an action the human never saw.
- A `RecycleNode`/`ReplaceNode` playbook is rejected at compile when no cloud
  provider is configured; `refresh` (`nvidia-smi`) has its own timeout so a
  wedged driver can't hang the agent's main loop; `gpu_reset` targets the GPU
  by UUID and scopes persistence-mode and holder checks to the device;
  orphaned leased actions of terminalized incidents are dead-lettered; a
  boot-bound action requires a matching boot ID to complete; OIDC requires a
  verified email; PKI renewal is capped so a decade-long CA does not freeze
  reconcile years early; negative durations and NAT-shared auth lockouts are
  handled.
- **The nvidia-smi/DCGM fallback detection source survives the durable event
  outbox.** The `events` table stored only the XID, and the controller
  classifies an event after reading it back from the outbox, so a fallback
  event carrying `XID=0` plus a neutral fault (`ecc-dbe`) lost its fault on
  the round trip and was durably acknowledged as non-actionable — a double-bit
  ECC error seen only by the second source opened no incident. The fault
  envelope and PCI address are now persisted (migrations 0015 SQLite /
  0006 PostgreSQL) and rehydrated; legacy rows scan back to no fault.
  Regression tests cross the real durable seam on both engines.
- Audit-retention pruning deletes a pruned terminal incident's spared
  expired-lease actions in the same transaction. Deleting only the incident
  made the terminal-incident claim guard vacuous (its join found no row) and
  handed a stale `gpu_reset`/`reboot` back to the node's next claim.
- The accelerator-stack janitor can no longer freeze the controller: its
  agent restore waits a bounded 30s inside the reconcile loop (the action
  stays queued; the next tick re-attaches by deterministic ID), where it
  previously waited on the process context — one quiesced node with a dead
  agent halted all incident processing and signal ingestion until restart.
  Every playbook step also now gets a default 30-minute timeout when it
  declares none, so a silent agent cannot leak the step's goroutine and gate
  slot forever.
- `quiesce_accelerator_stack` is destructive in the action registry: standing
  down DCGM and the device plugin on a node outside the declared
  `spec.safety.destructiveExecution` blast radius is now refused like any
  other destructive platform step.
- An approval decision binds to the step identity recorded when the incident
  parked — what the human was shown — not the step current at click time. A
  playbook hot-swap between the approval request and the click could
  previously make the decision capture the swapped-in step, which then matched
  itself at resume; now it mismatches and the incident re-parks for a fresh
  approval of the step that will actually run.
- `MaxConcurrentRemediations` caps concurrent remediations, not concurrent
  steps: a target's gate slot is held from its first admitted step until the
  incident terminalizes, instead of being released between steps — which let
  unrelated targets interleave steps far past the cap. Leader failover
  re-seeds the slots of every mid-remediation incident.
- The stack janitor stamps its queued restore action with the owning
  incident's ID (or none), not the node name; and the operator renews TLS
  material even when listing child configuration fails, matching the existing
  invalid-configuration path — an apiserver blip can no longer freeze
  certificate renewal.
- **(Round 7, superseding the entry above)** The janitor's restore carries NO
  incident stamp at all: the janitor acts precisely when the owner is halted,
  and the terminal-incident claim guard refuses actions stamped with a halted
  incident — the round-5 stamp made the restore permanently unclaimable
  (monitoring stayed down; each tick burned the full bounded wait).
  Provenance moved to action params, and a regression test crosses the real
  EnqueueAction→ClaimNextAction seam. The janitor's bounded wait is also now
  ONE budget per reconcile tick shared across all quiesced nodes, and the
  cordon janitor's per-tick node List is served from the informer cache.
- The agent drops an event the controller permanently rejects instead of
  retrying or spooling it forever (spool replay is head-of-line: one poison
  event silenced every detection behind it). The poison verdict requires the
  explicit `X-KubeNeuron-Event-Rejected` marker the controller sets only on
  semantic rejections — a bare 400 (an older controller strict-decoding a
  newer agent's payload during a rolling upgrade, or a middlebox) keeps
  spooling and drains after the skew clears. Dropped events are not
  remembered into dedup, so the sibling source's valid encoding of the same
  fault stays deliverable; counted in
  `kubeneuron_agent_events_rejected_total`.
- Remediation-slot ownership is durable: `remediation_slot_held` on the
  incident row (sqlite migration 0016 / postgres 0007, with backfill), set
  atomically with the first EXECUTING transition and cleared atomically with
  the halting one. The leader-failover rebuild reads the bit instead of
  inferring from state/StepIndex — the inference dropped an escalated
  incident's slot (escalation resets StepIndex mid-remediation) and the cap
  undercounted across failover.
- Agent arming is controller-visible data (round 7): registration protocol v2
  (`/api/v1/agents/register/narrow-v2`) always declares whether the agent
  runs `--enable-destructive-actions`; agents probe v2 and fall back to v1
  omitting the field, so every mixed-version pairing degrades to "unknown"
  (sqlite 0017 / pg 0008: tri-state `nodes.agent_arming`). The controller
  escalates an agent-destructive step at admission on a fresh, explicit
  "unarmed" declaration — before an approval is requested, and before a
  ladder cordons a node for a reboot its agent will refuse. Unknown or stale
  declarations change nothing; the agent executor stays the enforcement.
- **The two-DaemonSet scheme is retired (round 8): arming is controller-SERVED
  data.** One agent DaemonSet covers the whole fleet in every execution mode —
  no `--enable-destructive-actions` arg is rendered, no blast-radius
  narrowing, no detection-only companion (the `-agent-detect` DaemonSet is
  removed on upgrade). The agent boots unarmed and adopts the
  `X-KubeNeuron-Agent-Arming` answer on each v2 registration response,
  computed with the same selector-vs-labels match as blast-radius confinement
  so the two boundaries cannot disagree; every uncertainty answers unarmed.
  The agent arms/disarms live (logged loudly), refuses to arm on a non-real
  GPU driver, and reports its effective state back — closing the
  declared-vs-acked loop for the admission gate. The flag survives as a
  static pin for bare-metal use. Mixed-version pairings all degrade to
  today's behavior.
- The approval protocol is first-class rounds (round 8, hardened in round 9):
  each park mints an epoch and its request record atomically (sqlite 0018 /
  pg 0009: `incidents.approval_epoch`, `approvals.park_epoch`); decisions
  bind to a round and a re-park orphans them by construction, so a playbook
  swap AFTER a decision can never execute under it. (Round 10 closes the
  other half — the re-park landing between the notification and the click —
  by carrying the round through the notification channels and refusing a
  decision pinned to a superseded round.) Epoch 0 — the entire pre-upgrade
  population, requests and orphaned decisions alike — is never consulted as
  a round: such parks are migrated into round 1 and re-decided, closing a
  review-found hole where a stale pre-upgrade approval could pair with a
  different park's request and execute an unapproved step. A park pending
  across the upgrade needs one re-approval.
- The three background janitors run off the walk thread on their own
  goroutine, so a fleet event (dead agents, mass cordons) no longer stretches
  reconcile ticks or delays signal draining. Remediation-slot ownership,
  optimistic incident versioning, and the mutex-guarded caches were already
  cross-thread-safe, which is what made the move cheap.
- One configuration snapshot is pinned per incident advance (carried on the
  call-tree context, inherited by the step goroutine), completing the
  runtime-config work: an advance and the step it spawns can never mix
  configuration generations. Round 9 extends the same pin to each janitor
  pass.
- `StateChangedAt` is now the incident write-fence (round 9): the janitors
  run concurrently with the walk, and the quiesce rewind — a field-only
  rewrite — bumps it so `transition()`/`parkForApproval()` detect a conflict
  instead of silently writing a stale `StepIndex` back over the rewind (a
  review-found race that could have driven a reset against a restored
  stack). The janitor also holds the stack restore until every rewind has
  committed, and the concurrency model now runs as a model in a `-race`
  walk+janitor test.
- Undo is always allowed (round 9): `restore_accelerator_host` executes even
  on a DISARMED agent — it can only replay a prior quiesce's crash-safe host
  snapshot — so shrinking `destructiveExecution.nodeSelector` (or switching
  `Enabled` off) mid-remediation no longer strands a quiesced node with its
  monitoring down. A persistently failing restore is now surfaced once per
  node as a needs-human notification plus
  `kubeneuron_stack_restore_failures_total`, instead of a silent per-tick
  log.
- The approval round travels all the way to the click (round 10): every
  approval notification renders its round and suggests `kubeneuronctl
  approve <id> --round <n>`; the decision API accepts the round the client
  displayed (`park_epoch` in the decision body, `--round` on the CLI, sent
  automatically by the operator panel) and refuses a decision pinned to a
  superseded round with a "re-read the incident" error instead of silently
  binding the click to a request the human never saw. Omitting the round
  (older clients, raw curl) keeps the bind-to-current behavior, which the
  resume-time request check still guards.
- A node inside the destructive-execution blast radius whose agent freshly
  registers unarmed is now held as arming-in-flight for a bounded grace
  (2 minutes, four registration ticks) instead of parking for approval;
  past the grace the declaration is treated as a verdict — the agent cannot
  adopt served arming (non-real GPU driver, static pin) — and the incident
  escalates to a human early (round 10). Previously such a node looped
  approve → executor refusal → escalate, spending a human approval on a
  step the node could never execute.
- The kind admission suite's self-check counted 70 expected CEL checks but
  the `faults` signal-matcher cases had grown the matrix to 73, failing the
  run after every check passed; the count is 73 again. Two timing races in
  the kind harness are also fixed: the rogue-certificate scenario accepted
  only the `tls: bad certificate` alert spelling, but TLS 1.3 rejects the
  client cert after the handshake completes from the client's view, so the
  agent nondeterministically sees `broken pipe` instead — the assertion now
  also accepts the (deterministic) never-acknowledged registration warning;
  and the rotation scope assertions could capture their "before" workload
  generations while the previous phase's trailing operator update was still
  landing — they now wait for generation stability with completed rollouts
  first.
- `GPUSignalMapping` now publishes the same generation-bound status the
  other child configuration kinds have: `Ready=True/Compiled` with a digest
  of its compiled override, or `Ready=False/CompilationFailed` when the
  installation's configuration is invalid. The CRD had declared the status
  subresource since the mappings became consumable (N2), but the operator
  never wrote it — a selected mapping gave no positive feedback and could
  only be inferred from the root. Found by the live AWS hardware run, whose
  harness waits on the mapping's own Ready. RBAC adds
  `gpusignalmappings/status` (get/patch/update).
- TLS rotation phases roll only their consumers again: the PKI rework had
  collapsed the per-workload TLS digest into one union hash of all four
  roles stamped on BOTH workloads, so expanding the agents' server-CA trust
  also rolled the controller Deployment (and vice versa) — violating the
  rotation protocol's scope guarantee and briefly interrupting the
  controller on every trust expansion. The revision is split
  (`TLSRevisions{Controller, Agent}`): each workload's digest hashes
  exactly the Secret data it mounts. Caught deterministically by the kind
  rotation-scope assertion; pinned by a unit test per role.
- An agent that boots straight into a persistent registration failure (a
  rejected client certificate, a wrong controller URL) was silent at the
  default log level forever: "acknowledgment lost" only fired after a prior
  acknowledgment, and retry failures log at Debug — the single boot-time
  warning could carry a transient error (connection refused) rather than
  the persistent one. Never-acknowledged agents now warn once with the
  current error after `RegistrationStaleAfter` (found by the kind rogue-
  certificate scenario, whose log assertion depends on the TLS error being
  visible).

### Changed
- **Breaking (`v1alpha1`):** `spec.cloud` is now provider-scoped — the AWS
  region and IRSA role ARN move under `spec.cloud.aws.{region,iamRoleARN}`
  (previously top-level `spec.cloud.{region,iamRoleARN}`). The cloud seam is
  provider-neutral: each provider parses its own `providerID` and declares
  which node-remediation primitives it supports, and the operator rejects a
  `RecycleNode`/`ReplaceNode` playbook a configured provider cannot perform.
  Adding a new cloud (GCP/Azure) is a new package plus one registry line, not
  a core edit. AWS runtime behavior is unchanged.
- Internal: action metadata (executor kind, safety-gate class, forced
  approval, capability gate, cloud primitive) is unified into one declarative
  registry (`internal/action`) that the operator compiler, controller
  dispatch, playbook validation, and safety gate all derive from, replacing
  the copies previously smeared across those layers. Behavior-preserving; a
  test pins the CRD enum ↔ registry bijection.
- Docs: `executionMode: Enabled` arming narrows the agent DaemonSet to the
  named nodes — documented as a warning, since it also confines fault
  detection to those nodes. (Superseded in this release by the detection-only
  companion DaemonSet above.) The canonical `docs/design.md` and `PRODUCT_PLAN.md`
  gained a superseded-status banner (their v0.1.x status claims are stale;
  README/CHANGELOG/PRODUCTION_READINESS_PLAN are authoritative);
  `docs/design.md` now documents the three architectural seams (action
  registry, cloud provider seam, fault envelope) as invariants.
- The controller rejects an agent event carrying both a nonzero XID and a
  neutral fault (400): classification is Fault-first, so the XID would be
  silently ignored; ambiguity is refused, never interpreted.
- Node inventory on the Kubernetes platform is served from an informer-backed
  watch cache once synced (live List until then, and as fallback):
  blast-radius confinement resolves node labels on the destructive-admission
  path, which previously paid an apiserver round trip per check.
- Internal, from the sixth (structural) round: `reconcile.go` split along its
  seams (state walk / admission / execution / node resolution);
  `types.IncidentState.Halted()` is the single definition of "automation has
  ended", pinned to the store's claim-guard SQL by test; `EnqueueAction`
  lost its duplicate incident-ID parameter (`Action.IncidentID` is the only
  spelling); system-level tests now cross the real durable pipeline
  (ingest → outbox → classify-the-row-read-back → incident), the seam the
  round-5 critical hid in.
- Internal, from the seventh (structural) round: the reloadable runtime
  configuration is one immutable snapshot behind an atomic pointer — the
  reload path installs it whole (mixed config generations are impossible
  mid-pass), the previously UNGUARDED timing fields' data race is fixed and
  pinned by a `-race` test, and a step goroutine completes against the engine
  that admitted it. The action registry is closed (the last three wire-string
  matches now derive from registry facts; the `gateAction` string fallback is
  gone; the agent-dedup-is-catalog-blind boundary is pinned). The execution
  half of the pipeline gained its assembled-system rig: a fake agent speaking
  the real claim/complete protocol against the real store and actuator, over
  five scenarios including controller death mid-step (exactly-once
  re-attach), lease rebinding across a node reboot, and
  cancellation-vs-claim.

## [v0.2.0] - 2026-08-01

Production-readiness for EKS. Destructive execution is now a supported,
off-by-default mode confined by `spec.safety.destructiveExecution`.

### Added
- Cloud GPU node remediation: `ReplaceNode` (terminate → node-group
  replacement) as the primary primitive on autoscaled fleets and
  `RecycleNode` (stop/start) for self-managed nodes, driven controller-side
  through IRSA. Validated end to end on live EKS.
- Operator-issued mTLS on `spec.tls.issuer: Operator`, with automatic
  renewal and a rollout of the consumers on reissue.
- Evidence-based reset refusal: the agent refuses a per-device GPU reset
  where the guest has no PCI reset (virtualized instances) and the cloud
  replace stands in.

### Changed
- Controller configuration hot-reloads in place instead of rolling the
  Deployment, removing the HA leader-election rollout deadlock so config
  changes apply without downtime.
- Agent host state (persistence mode, DCGM) is snapshotted crash-safe
  across restarts.

## [v0.1.1] - 2026-07-28

First release from the public repository.

### Changed
- Module path and container image namespace moved to
  `github.com/kubeneuron/kubeneuron` and
  `ghcr.io/kubeneuron/kubeneuron/*`; install manifests, Helm values, and
  the installer follow.

### Added
- Password and OIDC single sign-on for the control panel
  (`spec.auth.users`, `spec.auth.oidc`) with server-side sessions; audit
  actors carry the verified identity.
- Redesigned control panel: sidebar app shell, per-GPU fleet health grid,
  incident filters, and an incident drawer with playbook progress and the
  audit timeline.
- `deploy/install.sh`: one-command installation (pipe-safe, `curl … | bash`)
  and `kubeneuronctl passwd` for panel password hashes.
- Product tour with screenshots and a recorded demo, plus a one-page
  product overview.

## [v0.1.0] - 2026-07-26

First tagged release: the complete DryRun control plane with a shippable,
signed artifact set. Everything below the Phase 6 gate from
`PRODUCTION_READINESS_PLAN.md` that does not require GPU hardware is in.
`executionMode: Enabled` remains rejected by construction.

### Added (since the plan audit)
- PostgreSQL workflow store (shared `sqlcore` engine, conformance-tested
  against PostgreSQL 16) with Lease-based leader election: two controller
  replicas, readiness follows leadership, retention leader-gated; SQLite
  remains the single-replica/dev option. Failover replay is proven to
  attach to the same queued action — no duplicate side effect.
- Server-side action protocol: per-claim attempt counter, executor
  boot-ID binding (a result posted after an unnoticed reboot is rejected),
  and pending-only cancellation when an incident escalates or quarantines.
- Opt-in NVIDIA host tooling for the agent DaemonSet
  (`spec.agent.hostTooling`): read-only mounts of `nvidia-smi`/driver
  libraries/remediation scripts into the distroless image, arming
  `--require-real-driver`.
- Verifiable operator identity: the operator API accepts any Kubernetes
  bearer token (TokenReview + SubjectAccessReview against RBAC on the
  root `kubeneurons` object); the shared static token is break-glass and
  its self-asserted actor is recorded as `token:<name>`.
- Notification reliability: per-channel retry with backoff and
  dead-lettering, a generic JSON webhook channel, and PagerDuty Events v2
  (dedup by incident, critical paging on needs-human/approvals,
  auto-resolve).
- Token hygiene: `kubeneuron_auth_failures_total`, per-source throttling
  of failed attempts, and hot token rotation (files re-read, no restart).
- cert-manager convenience path (`deploy/cert-manager/`) for the
  four-Secret TLS model with auto-renewing 90-day leaves.
- Kind harness: controller-restart-mid-playbook phase (durable approval
  state, no re-executed step) and a 60-case CEL admission matrix.
- Operator emits Kubernetes Events for reconcile outcomes; readiness
  reflects informer-cache sync.

### Added
- Production readiness plan (`PRODUCTION_READINESS_PLAN.md`) with a phased
  path to v1.0 and a defect register.
- `SECURITY.md`, issue/PR templates, `CODEOWNERS`, Dependabot configuration.
- Release workflow: multi-arch distroless images for all four binaries
  published to GHCR with SBOM, cosign keyless signatures, and checksums;
  single-file CRD/operator install manifest attached to releases.
- `make docker` target building the four production images locally.

### Added (Phase 3, in progress)
- Gate cooldowns and flap history persist across controller restarts
  (`safety_state` snapshots restored on startup).
- The agent serves `/metrics`; vmagent scrapes the controller, agent, and
  operator; a `kubeneuron-self` alert group covers controller down,
  NEEDS_HUMAN, dropped signals/notifications, spool backlog, and TLS
  certificates expiring within 30 days
  (`kubeneuron_tls_certificate_not_after_seconds`).
- Admission-time CEL for the remaining config CRDs: unsupported policy match
  fields, Reboot-without-approval playbooks, malformed durations, and
  SSH/BMC references are rejected before the compiler sees them.
- Optional `spec.tls.publicServerSecretRef`: the controller's public
  listener (operator API, webhook, panel, metrics) serves TLS 1.3 when set,
  so bearer tokens stop crossing the network in cleartext; probes and the
  managed Deployment switch automatically (CEL matrix: 51 cases).

### Changed
- CI: pinned golangci-lint with a repo `.golangci.yml`, added `govulncheck`
  and strict docs builds, aligned Go toolchain versions across jobs.
- CRD validation rejects floating (`:latest` or untagged) controller/agent
  image references.
- Alert rules unified: the deployed VMRule mirrors the canonical vmalert
  file (a unit test pins them together); rules referencing never-exported
  agent series were removed.
- The kind integration harness runs a 3-node cluster by default
  (`WORKER_NODES`) and passes the full matrix on it; fixes surfaced by the
  run include a CEL cost-budget bound on `acceleratorruntimeprofiles`
  (previously uninstallable on Kubernetes 1.33), degraded-vs-stale
  readiness semantics, and all-pod agent log aggregation.
- Workflow-store backups moved to an authenticated `GET /api/v1/backup`
  snapshot endpoint with a rewritten curl-based CronJob; restore is proven
  in the e2e suite.
- Observability completion: dashboard panels for every shipped alert
  (availability, notification drops, spool backlog, TLS expiry, reconcile
  latency), Alertmanager routing/inhibition policy with an authenticated
  webhook (the shipped config previously omitted the mandatory bearer
  token), `runbook_url` on every rule, and per-alert runbooks
  (`docs/runbooks.md`).
- Real incidents (non-dry-run) now require runtime evidence — fresh agent
  heartbeat plus a ready accelerator report listing the target GPU — before
  resolving; missing evidence fails closed to NEEDS_HUMAN.
- A Helm chart (`deploy/helm/kubeneuron`) installs the CRDs and operator;
  CI pins it to the kustomize manifests. Upgrade runbook and REST
  API/CLI/metrics reference pages added to the docs site.

### Removed
- Tracked merge-conflict artifacts (`*.orig`, `*.rej`) and agent-session
  scratch documents.
- Legacy, non-functional deploy paths (`deploy/kubernetes/base` + overlays,
  `deploy/systemd`, `deploy/compose`) that predate the mandatory mTLS agent
  transport; the production image build moved to `build/Dockerfile`.

### Fixed
- `web/dist` (embedded control panel) is now tracked; a fresh clone builds
  the controller again.
- Safety gate concurrency slots are refcounted per target: one incident
  finishing no longer releases a slot a concurrent incident on the same node
  still holds, and recording a playbook cooldown no longer frees live slots.
- Flap detection counts resolve→reopen cycles instead of every new incident,
  no longer double-counts retried transitions, and prunes stale pairs.
- Incidents crossing the async notification queue are deep-copied (data
  race), nil incidents no longer panic the Slack notifier, and dropped
  notifications are counted in `kubeneuron_notifications_dropped_total`.
- Node drain retries PDB-blocked evictions (HTTP 429) until the drain
  timeout instead of escalating to a more destructive playbook rung.
- The agent action journal compacts acknowledged entries older than 24h
  instead of permanently refusing work at its 10k/64MiB limits.
- The SQLite store enforces retention (`-store-retention`, default 90 days,
  for events/outbox/actions; opt-in `-store-audit-retention` for terminal
  incidents with audit history), checkpoints the WAL, opens with an asserted
  `synchronous(FULL)`, and refuses databases from newer binaries.
- The spool syncs its directory after atomic replacement and creates its
  directory on open; the executor idempotency cache evicts expired entries.
- Detection-catalog occurrence thresholds (XID 13/31/43) are now enforced as
  the default observation policy instead of being documentation-only.
- An XID that cannot be attributed to a GPU reports index -1 instead of
  blaming GPU 0; the HTTP API doc comment no longer advertises unregistered
  routes.
