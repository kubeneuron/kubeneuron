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
3. Incident audit-trail drill-down, with the approval round's impact
   preview (v0.8 development): node and topology, what the pending step
   would touch, which workloads are eligible for a checkpoint request,
   each workload's owner chain and resolved context (batch Job, Kueue,
   Ray), checkpoint groups with their on-node count and a cluster total
   that may be unknown, warnings, when the preview was computed and how
   old it is, and the digest an Approve from the drawer binds to; an
   unavailable preview offers Recompute. The round's recorded decision
   says whether it was bound to the preview that is still current. When
   the controller answers an Approve / Reject with 409 (the round or the
   preview changed), the panel drops its copy, reloads the preview, and
   asks the operator to review it and decide again — nothing is
   resubmitted on its own.
4. Node inventory with agent heartbeat age, per-node pause state, and the
   read-only device topology classification (topology, whether device
   actions are eligible or refused, reason codes) fetched per node while
   the Fleet view is open.

The static page itself is public; every API call it makes requires the
operator token, and mutations record the actor in the audit log.

**Target v2 (roadmap):** the React/TypeScript application described in
[docs/design.md](../docs/design.md) §5 — viewer/operator/admin roles, OIDC,
SSE live updates, metrics proxy, and validated configuration editing. When
it lands, `make web` will build into `dist/` and this embedded pipeline
stays unchanged.
