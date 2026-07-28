# KubeNeuron Web UI

The controller serves the embedded control panel from this directory at
`GET /` and `GET /ui/` on the public listener.

**Panel v1 (current):** a zero-dependency single-file application
(`dist/index.html`) — no Node.js, npm, or build step. It talks to the
operator REST API with the bearer token you paste at the login screen (kept
in `sessionStorage` only). Surfaces:

1. Global pause banner with pause/resume controls.
2. Incidents with state badges, playbook progress, and per-incident
   Approve / Reject / Resolve actions (audited with the actor field).
3. Incident audit-trail drill-down.
4. Node inventory with agent heartbeat age and per-node pause state.

The static page itself is public; every API call it makes requires the
operator token, and mutations record the actor in the audit log.

**Target v2 (roadmap):** the React/TypeScript application described in
[docs/design.md](../docs/design.md) §5 — viewer/operator/admin roles, OIDC,
SSE live updates, metrics proxy, and validated configuration editing. When
it lands, `make web` will build into `dist/` and this embedded pipeline
stays unchanged.
