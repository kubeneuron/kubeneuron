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
| `kubeneuronctl incidents [--state ...] [--node ...]` | list incidents |
| `kubeneuronctl incidents show <id>` | one incident with its full audit trail |
| `kubeneuronctl report [--since 30d] [--json]` | recovery report for a window: GPU-hours degraded and recovered, share recovered without a human, cost by class, MTTR, incidents still open |
| `kubeneuronctl approve <incident-id> --actor <who> [--round <n>]` | approve the pending risky step; `--round` (from the notification) is refused if the request changed since it was displayed |
| `kubeneuronctl reject <incident-id> --actor <who> [--round <n>]` | reject the pending risky step |
| `kubeneuronctl resolve <incident-id> --actor <who>` | manually resolve an incident |
| `kubeneuronctl remediate <node> --class <problem-class> --actor <who>` | manually open an incident for a node |
| `kubeneuronctl pause` / `kubeneuronctl resume` | global automation pause (big red button) |

`--actor` defaults to `$USER` and is recorded verbatim in the audit trail —
use a real, attributable identity, not a shared team name. Decision commands
also take `--reason`, recorded alongside the decision. `incidents` filters
with `--state/-s` and shows detail via `incidents show <id>`; `remediate`
targets a GPU with `--gpu-uuid`/`--gpu`.

## `report` — what the fleet got back

`kubeneuronctl report` is the capacity-owner artifact: how much accelerator
capacity was degraded over a window, how much of it came back, and how much
came back without waking anybody. `--since` accepts `24h`, `7d`, `4w`
(default `7d`); `--json` emits the same numbers as a document.

### On a dry-run fleet

Dry-run is the shipped default, and the [pilot
checklist](pilot-checklist.md) tells you to stay in it until you have watched
the system decide. Every number above is zero for that whole period, by
construction: dry-run executes nothing, so it recovers nothing, and counting
its synthetic successes as recovered capacity would make the one number this
report exists to be trusted on the one that lies out of the box.

So those incidents are reported separately, under `SIMULATED`, with the same
arithmetic and conditional names:

```
SIMULATED — what dry-run WOULD have done (nothing was executed)
  incidents             37
  would recover         31 (27 without asking a human)
  degraded GPU-hours    412.8   ← real: the hardware was degraded this long
  would recover         366.5   ← hypothetical: no capacity was returned
  ladder decision time  p50 12m22s  p90 52m0s
```

The degraded hours are real — the hardware was genuinely in that state. Only
the recovery is hypothetical. Read this as *what the policy would have
chosen*, which is exactly what a dry-run pilot is evaluating, and never as
capacity returned. In `--json` it is the `simulated` object, absent entirely
when the window contains no dry-run incidents.

The numbers come from the controller's **incident store**, not from
Prometheus: the store is the ground truth the outcome metrics are derived
from, so the report is exact rather than bucket-interpolated, it survives a
counter reset, and it works on a fresh install with no monitoring stack
attached. The controller aggregates server-side over
`GET /api/v1/report/recovery` — the definition of "recovered" lives in one
place instead of in every client. The dashboard row (`deploy/grafana/`) answers
the same question from `kubeneuron_degraded_gpu_seconds_total` and friends,
where the question is a shape over time rather than a total.

```
$ kubeneuronctl report --since 30d
window:    2026-07-06T12:00:00Z .. 2026-08-05T12:00:00Z (30d)
incidents: 37 in window, 2 still open

degraded GPU-hours     412.8
recovered GPU-hours    366.5       88.8% of degraded
incidents recovered    31 of 37    83.8%
  without a human      27 of 31    87.1% of recovered
MTTR (resolved, n=31)  p50 12m22s  p90 52m0s  mean 19m6s

top problem classes by degraded GPU-hours:
CLASS             INCIDENTS  DEGRADED  RECOVERED  UNATTENDED  MTTR P50  MTTR P90
fell-off-bus      9          233.5     216.0      5           33m0s     52m0s
ecc-dbe           21         142.2     138.5      19          10m20s    19m40s
thermal-throttle  7          37.0      12.0       3           6m40s     10m10s

still open (2), oldest first:
ID                        STATE        CLASS         NODE    GPU       AGE      DEGRADED
fell-off-bus-gpu-a3-7f21  NEEDS_HUMAN  fell-off-bus  gpu-a3            52h0m0s  416.0
ecc-dbe-gpu-b7-91c4       EXECUTING    ecc-dbe       gpu-b7  GPU-9f3c  14m0s    0.2

legend:
  ...
```

### What each number counts

The command prints this legend on every run, because each of these has a
plausible wrong reading:

- **recovered** — the incident reached `RESOLVED`. `NEEDS_HUMAN` and
  `EXPIRED` do not count, and an incident parked for a human keeps accruing
  degraded time until it is resolved or expires. (The Prometheus histogram
  stops its clock at the park; the report is deliberately the stricter of the
  two, because the strict reading cannot overclaim.)
- **unattended** — resolved without ever minting an approval round, i.e. no
  human decided anything. This is the automation's yield.
- **GPU-hours** — *degraded* GPU-time, not *lost* GPU-time: a degraded GPU may
  still have been serving. One GPU for a GPU-scoped incident; the node's
  registered GPU inventory for a node-scoped one. A node with unknown
  inventory is charged one GPU and the run prints a note saying how many
  incidents that affected — the report undercounts rather than invents.
- **window membership** — an incident counts when its degraded interval
  overlaps the window, including one opened long before it. Degraded GPU-hours
  are clipped to the window; MTTR is not, because a clipped duration is not a
  recovery time.
- **MTTR** — full open-to-resolved wall time of incidents that resolved inside
  the window. Percentiles are nearest-rank over the real durations, so every
  value printed is a time some incident actually took.
- **still open** — every incident that is not lifecycle-terminal at the window
  end, oldest first, `NEEDS_HUMAN` included.

Two runs over unchanged data print identical numbers: the window is anchored
to the controller's clock (the one that stamped the incident rows) and
returned in the output, and GPU-hours are quantized to 1e-4.

## Exit codes

`0` on success; non-zero with the server's error message on any HTTP or
transport failure (including `401` for a wrong token and `409` for a
decision on an incident that is not awaiting approval).
