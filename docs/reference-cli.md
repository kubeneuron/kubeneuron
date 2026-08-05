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

## Exit codes

`0` on success; non-zero with the server's error message on any HTTP or
transport failure (including `401` for a wrong token and `409` for a
decision on an incident that is not awaiting approval).
