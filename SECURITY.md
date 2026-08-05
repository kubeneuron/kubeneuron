# Security Policy

## Reporting a vulnerability

Please do **not** open a public issue for security problems.

Report vulnerabilities privately via
[GitHub Security Advisories](https://github.com/kubeneuron/kubeneuron/security/advisories/new)
or by email to <andrey.petruk@gmail.com> with the subject prefix
`[kube-neuron security]`.

Include a description of the issue, reproduction steps, the affected
component (operator, controller, agent, CLI, deployment manifests), and any
suggested remediation. You should receive an acknowledgment within 7 days.

## Supported versions

The project is pre-1.0. Only the most recent tagged release receives security
fixes; older tags are not patched. The `main` branch is a development branch
and carries no support guarantee.

## Scope notes

- `executionMode: Enabled` (real destructive remediation) is a supported,
  off-by-default mode confined by `spec.safety.destructiveExecution`: a
  non-empty node selector plus an exact acknowledgement string, enforced at
  admission, compilation, controller dispatch, and the agent executor.
  **Bypasses of that confinement — executing a destructive action on a node
  outside the declared selector, arming an agent the controller did not
  arm, or resuming an approval for content the human never saw — are
  exactly the class of report we most want.** Fail-closed rejection paths
  working as documented are not vulnerabilities.
- The trust boundaries of interest are: agent ↔ controller (mTLS + Pod-bound
  token; controller-served arming header), human/CLI/panel ↔ operator API
  (static bearer tokens, password users with server-side sessions, or OIDC —
  decisions are audited under the verified identity), Alertmanager ↔ webhook
  (bearer token), and the operator's Kubernetes RBAC surface.
- Vulnerabilities in third-party dependencies (VictoriaMetrics, Alertmanager,
  dcgm-exporter, GPU Operator) should be reported upstream; version-bump
  requests here are welcome as regular issues.
