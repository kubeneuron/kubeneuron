# REST API reference

The controller serves two listeners. Everything here is the **public
listener** (default `:8080`; plain HTTP unless
`spec.tls.publicServerSecretRef` is set). The **agent listener** (`:8443`,
TLS 1.3 with mandatory client certificates plus a projected Pod-bound token)
carries only agent registration, events, action polling, and accelerator
reports; it has no human-facing routes and is not documented for direct use.

Authentication: every `/api/v1/*` route below requires an
`Authorization: Bearer` credential or a server-side session except the
Alertmanager webhook, which requires the separate webhook token. Two
bearer credentials are accepted:

- **A Kubernetes credential (recommended, managed installations):** any
  bearer token the API server can verify — `kubectl create token <sa>`, an
  OIDC user token. The controller resolves the caller with `TokenReview`
  and authorizes with `SubjectAccessReview`: read routes require RBAC `get`
  and mutating routes RBAC `update` on the root `kubeneurons.kubeneuron.io`
  object of this installation. Audit rows record the verified principal
  (e.g. `system:serviceaccount:ops:sre-bot`); any `actor` in the body is
  ignored.
- **The shared static operator token (break-glass):** the `actor` body
  field is then required and recorded as `token:<actor>` — visibly a
  self-asserted claim, not a verified identity.

Two interactive sign-ins issue server-side sessions instead of a bearer
header: **password users** declared in `spec.auth.users`
(`POST /api/v1/login`) and **OIDC**
(`GET /api/v1/auth/oidc/login` → provider →
`GET /api/v1/auth/oidc/callback`). `GET /api/v1/session` returns the
current session's identity; audit rows record the verified user.

Without a configured operator token the operator API is disabled entirely
(fail closed).

## Health and metrics

| Route | Auth | Purpose |
|---|---|---|
| `GET /healthz` | none | liveness («ok») |
| `GET /metrics` | none | Prometheus metrics (see the [metrics reference](reference-metrics.md)) |
| `GET /`, `GET /ui/` | none (static) | embedded control panel; its API calls still need the operator token |

## Ingestion

| Route | Auth | Purpose |
|---|---|---|
| `POST /api/v1/webhooks/alertmanager` | webhook token | Alertmanager `webhook_config` receiver; firing alerts become signals |

## Incidents

| Route | Purpose |
|---|---|
| `GET /api/v1/incidents` | list; filters `?state=OPEN,EXECUTING&node=<node>&limit=<n>` |
| `GET /api/v1/incidents/{id}` | detail including the audit trail |
| `POST /api/v1/incidents` | manual remediation trigger; body `{"node","class","actor?","gpu_uuid?","gpu_index?"}` — `node` and `class` are required; `actor` only with the static token |
| `POST /api/v1/incidents/{id}/approve` | approve the pending step; body `{"actor","reason?","park_epoch?"}` |
| `POST /api/v1/incidents/{id}/reject` | reject the pending step; body `{"actor","reason?","park_epoch?"}` |
| `POST /api/v1/incidents/{id}/resolve` | manually resolve; body `{"actor"}` |

Decisions return `204 No Content`. `park_epoch` pins the decision to the
approval round shown to the human; a decision against a superseded round
is refused. With a Kubernetes credential the audit
actor is the authenticated principal and the body `actor` is ignored; with
the static token the claim is recorded as `token:<actor>`.

## Fleet

| Route | Purpose |
|---|---|
| `GET /api/v1/nodes` | registered nodes with GPU inventory and heartbeat age |
| `GET /api/v1/nodes/{node}` | one node's detail |
| `GET /api/v1/nodes/{node}/accelerators` | latest per-vendor accelerator runtime reports |
| `GET /api/v1/targets?port=<p>` | Prometheus `http_sd` target groups for registered nodes |

## Control

| Route | Purpose |
|---|---|
| `GET /api/v1/pause` | current global pause state |
| `POST /api/v1/pause` | pause all automated remediation (big red button) |
| `DELETE /api/v1/pause` | resume |

## Operations

| Route | Purpose |
|---|---|
| `GET /api/v1/backup` | streams a transactionally consistent SQLite snapshot (`VACUUM INTO`); see [operations](operations.md#sqlite-workflow-store-backup-and-restore) |

## Not implemented

Slack interactive approvals, SSE streaming, a metrics query proxy, and
versioned config editing are design targets that do **not** exist;
the API deliberately does not advertise them.
