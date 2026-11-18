# kubeneuronctl reference

The operator CLI is a thin client over the [REST API](reference-api.md).

## Connection

Every command takes:

| Flag | Default | Meaning |
|---|---|---|
| `--server` | `http://localhost:8080` | controller public URL (use `https://` with `spec.tls.publicServerSecretRef`) |
| `--token` / `--token-file` / `KUBENEURONCTL_TOKEN` | — | operator API bearer token (prefer the file or env form) |

In-cluster, port-forward first:
`kubectl -n <ns> port-forward svc/<name>-controller 8080:8080`.

## Commands

| Command | Purpose |
|---|---|
| `kubeneuronctl status` | fleet health summary: nodes, incident counts by state, pause state |
| `kubeneuronctl nodes` | registered nodes with GPU inventory and heartbeat age |
| `kubeneuronctl nodes topology <node> [--json]` | one node's read-only device topology classification (`GET /api/v1/nodes/{node}/topology`): a `NODE`, `TOPOLOGY`, `DEVICE ACTIONS`, `ATTRIBUTION`, `REASONS`, `DRA`, `OBSERVED AT`, `INPUTS DIGEST`, `VERSION` table, or the full `topology-classification/v1` document with `--json`. `TOPOLOGY` is one of `whole-device`, `mig`, `time-sliced`, `dra-managed`, `mixed`, `unknown`; `DEVICE ACTIONS` is `eligible` or `refused`; `DRA` shows the observation state with its stable reason when not observed. It is the only `nodes` subcommand, it writes nothing and changes nothing in admission. A controller older than v0.8.0 has no such route, so against one the command fails with an HTTP 404 that can be indistinguishable from an unknown node; a `503` is the controller refusing to assess (no classifier wired, or a store that cannot retain accelerator reports), not a classification (since v0.8.0, see the [REST API reference](reference-api.md#device-topology-read-only-v080)) |
| `kubeneuronctl health [--node <node>] [--json]` | the controller's last read of every accelerator health source (`GET /api/v1/accelerator-health`): a `NODE`, `SOURCE`, `PROFILE`, `RULE`, `DEVICE`, `STATE`, `REASON`, `READ`, `WHY` table, or the view as JSON. `(all)` in `NODE` is a source row about every node, such as `not-read-this-term`. See [accelerator health](accelerator-health.md#seeing-what-was-read) |
| `kubeneuronctl incidents [--state ...] [--node ...]` | list incidents |
| `kubeneuronctl incidents show <id>` | one incident with its full audit trail |
| `kubeneuronctl incidents impact <id> [--round <n>] [--json]` | the approval round's impact preview (`GET /api/v1/incidents/{id}/impact-preview`): node and topology, the pending step and the disruptions downstream, workload counts and who can checkpoint, groups, warnings, and the `DIGEST` an approve binds to; `--json` prints the `impact-preview/v1` document (since v0.8.0; a controller older than v0.8.0 answers 404) |
| `kubeneuronctl report [--since 30d] [--json]` | recovery report for a window: GPU-hours degraded and recovered, share recovered without a human, cost by class, MTTR, incidents still open |
| `kubeneuronctl approve <incident-id> --actor <who> [--round <n>] [--impact-digest <sha256:…>]` | approve the pending risky step; `--round` (from the notification) is refused if the request changed since it was displayed, `--impact-digest` (also from the notification, or from `incidents impact`) is refused if the round's preview changed since it was read. With `--round` and no `--impact-digest`, the command reads the round's current preview and, when one is computed, prints it and **stops** with the exact bound command to re-run — it never binds an approve to a preview the human did not read (v0.8.0); against a controller without the route, or a round without a computed preview, the approve goes out unbound, as before |
| `kubeneuronctl reject <incident-id> --actor <who> [--round <n>]` | reject the pending risky step |
| `kubeneuronctl acknowledge <incident-id> --actor <who> [--resource-version <n>]` | record custody of an active incident without changing its state |
| `kubeneuronctl resolve <incident-id> --actor <who> [--resource-version <n>]` | manually resolve an incident through the idempotent v0.4 lifecycle API |
| `kubeneuronctl remediate <node> --class <problem-class> --actor <who>` | manually open an incident for a node |
| `kubeneuronctl pause` / `kubeneuronctl resume` | global automation pause (big red button) |

`--actor` defaults to `$USER` and is recorded verbatim in the audit trail —
use a real, attributable identity, not a shared team name. Decision commands
also take `--reason`, recorded alongside the decision. `incidents` filters
with `--state/-s` and shows detail via `incidents show <id>`; `remediate`
targets a GPU with `--gpu-uuid`/`--gpu`.

## Remediation intelligence commands (v0.4.0)

Every command below creates its own `Idempotency-Key`. For an important
operator action, retain the returned JSON/resource ID; a client retry after a
transport failure should repeat the same HTTP request and key rather than
create a second review or diagnostic run. Mutations that accept
`--resource-version` use `0` as an explicit “read current” compatibility
mode; scripts should prefer the version returned by `show`.

| Command | Purpose |
|---|---|
| `kubeneuronctl readiness [node]` | fleet table or full node decision snapshot/evidence explanation; the single-node JSON also carries the additive `topology` object when the controller has the device topology classifier wired (v0.8.0), which explains and never changes the `decision` beside it; the field is omitted only by a controller without the classifier, and a classifier that cannot assess the node fails the whole single-node request with a `503` rather than dropping the field |
| `kubeneuronctl candidates upload <file> [--tenant … --cluster … --expires-at …]` | strict candidate upload; it never applies the file to Kubernetes |
| `kubeneuronctl candidates list`, `show <id>`, `revoke <id> --reason …` | inspect or logically revoke retained candidate configurations |
| `kubeneuronctl preview <candidate-id>` | capture inventory and create a deterministic policy-impact preview. The JSON includes a per-node `runtime_contract_impact` (`assessment: "PreDeployStatic"`; v0.5.0) that states whether the candidate replaces the runtime profile set, how the candidate profiles statically select the node, and the post-deploy attestation requirement (`FreshRequired`, `NotRequired`, or `NotApplicable`). Static selection uses only the candidate profiles, the captured node name and labels, and the vendor identity recorded in the captured report; the report's attestation content, observation time, and the agent heartbeat never prove a candidate. A candidate profile that selects a node, even one identical to the live profile, therefore shows an `after` decision that fails closed with `FreshRequired`. A policy-only candidate reports `NoProfileChange` and keeps the live profile decision. The command has no apply, deploy, promote, or approve mode |
| `kubeneuronctl health-check <node> --profile Passive|Quick|Extended --reason …` | request bounded diagnostics; Extended requires a Kubernetes-authenticated bearer identity in the verified `diagnostics-extended` and `disruption-budget-approver` groups plus a live maintenance window. The client never sends authorization grants. |
| `kubeneuronctl health-check list`, `show <run-id>`, `cancel <run-id>` | inspect or cancel durable diagnostic work |
| `kubeneuronctl simulate <node> --class … --rationale … [--action … --scope … --device-id …]` | create a no-side-effect frozen remediation graph |
| `kubeneuronctl simulate list`, `show <id>`, `incident <id> --rationale …` | inspect simulations or make a permitted one an incident with an auditable rationale |
| `kubeneuronctl autonomy create <json-file>`, `list`, `show <plan-id>` | create/read a bounded GPUAutonomyPlan envelope |
| `kubeneuronctl autonomy attach-simulation <plan-id> <simulation-id>` | bind a permitted matching simulation to a draft plan |
| `kubeneuronctl autonomy approve|pause|resume|rollback <plan-id> …` | role-bound approval and explicitly reasoned lifecycle transitions |
| `kubeneuronctl autonomy rollout <plan-id>` | inspect canary, bake, expansion, decision, and effect observations |
| `kubeneuronctl audit-events [--kind … --tenant … --cluster … --actor … --request-id … --cursor …]` | query the append-only v0.4 operational audit explorer; pass back its opaque cursor to continue a page |
| `kubeneuronctl runtime-contracts coverage <node> --vendor <v>` | print one node's read-only `runtime-contract-coverage/v1` assessment as JSON: `selection`, `attestation`, `verification_depth`, the selected profile identity for `Exact`, reason codes, and the observed heartbeat/report timestamps. `--vendor` (`nvidia`, `amd`, `intel`, or `google`) is required. Nothing is written and nothing in admission changes (v0.5.0) |
| `kubeneuronctl runtime-contracts coverage --vendor <v> [--tenant … --cluster … --limit … --cursor …]` | fleet coverage table (`NODE SELECTION ATTESTATION VERIFICATION PROFILE REASONS`) in stable node-name order, `--limit` 1–500; when a page continues, the command prints the `--cursor` value to pass back. The page fails as a whole if any node's coverage cannot be built, so a row is never silently missing. `coverage` is the only subcommand |
| `kubeneuronctl runtime-qualifications create --nodes … --vendor … --min-samples … --min-duration … --expires-at … [--tenant … --cluster … --actor …]` | freeze an explicit node cohort against its runtime profile and start an evidence-only qualification; `--min-duration` is a Go duration, `--expires-at` is RFC3339 |
| `kubeneuronctl runtime-qualifications list [--state … --tenant … --cluster … --since … --until … --cursor … --limit … --include-expired=…]`, `show <id>` | inspect qualifications as JSON; read `effective_state` and `ready_for_approval`, which are computed at read time, rather than the stored `state`, because a qualification past `expires_at` stays stored as `ReadyForApproval` until an observation records `Expired` |
| `kubeneuronctl runtime-qualifications observe <id> [--resource-version … --actor …]` | record one fresh observation of the frozen cohort with idempotency/version fencing. There is no approve, promote, apply, enable, or delete subcommand: a ready qualification is evidence for a separate human decision, not authority |

`runtime-contracts`, the `runtime_contract_impact` block in `preview` output,
and `runtime-qualifications` are the v0.5.0 GPU Runtime Contract Lifecycle
scope, released on 2026-09-20. They are unit-tested and
the CPU-only kind integration harness runs them against a real controller
with synthetic accelerator evidence; no GPU hardware run has called them.
Against a v0.4.0 controller
the two new command groups fail with
`404`, and `preview` JSON has no `runtime_contract_impact`. They are
evidence-only views; none of them changes what the controller admits.

The installed controller has no default hardware autonomy executor. A plan can
therefore advance visibly through a `simulation-only` canary, but the CLI does
not claim that a device action happened unless the rollout reports a
hardware-qualified effect reference. See the [REST API reference](reference-api.md#gpuautonomyplan)
for the exact plan contract and safety gates.

## Recovery report

Run `kubeneuronctl report --since 7d` for the trailing week. The controller
uses its incident store and returns the exact window boundaries. Use
`--json` for the API representation. The panel's Overview uses the same
endpoint. Both show the node, vendor when known, and degraded time for each
incident. An empty window displays “No incidents in this window.” Interruptions
announced by the cloud are left out of every number, and both say how many
there were, in the dry-run simulation too.

### How we count

**Recovered GPU-hours** sums the degraded GPU-hours of incidents that ended
with an audited remediation step followed by a verified resolution. Overlapping
intervals on one GPU count once. Manual closures and observe-only incidents
are excluded. It measures degraded time that ended in a verified recovery, not
capacity confirmed to be serving work again.

**Degraded GPU-hours** sums each incident's time within the selected window
multiplied by its affected GPU count. A degraded GPU may still serve work.
**Time to recover** shows the median full duration from open to resolution for
incidents that completed in the window with audit evidence of a remediation
step. Observations that close without a remediation contribute degraded time.
**GPU-hours saved (estimate)** counts the time from verified resolution until
the incident's opening time plus the manual baseline, for each affected GPU
with a known count. A resolution at or after that deadline contributes zero.
Overlapping intervals on the same GPU count once, including overlaps between
node-wide and device incidents. DryRun uses the same calculation separately.
The baseline is the
team's estimate of manual recovery time. Set `spec.manualRecoveryBaseline`
on the installation or `manual_recovery_baseline` in controller file
configuration using a duration such as `4h`. There is no default. The report
shows the configured baseline beside the estimate. Where a node's GPU count
is missing, the report flags its one-GPU assumption for degraded time and
omits that incident from the savings calculation.

DryRun appears separately. The “would have been saved” estimate assumes the
simulated decision time would equal the actual recovery time. Neither this
estimate nor a completed infrastructure remediation establishes that a
training job resumed or that a cloud bill decreased.

## Exit codes

`0` on success; non-zero with the server's error message on any HTTP or
transport failure (including `401` for a wrong token and `409` for a
decision on an incident that is not awaiting approval).
