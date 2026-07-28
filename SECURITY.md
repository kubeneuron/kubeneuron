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

- KubeNeuron is **not production-ready**; `executionMode: Enabled` is
  deliberately rejected. Reports about the documented, fail-closed rejection
  paths working as documented are not vulnerabilities.
- The trust boundaries of interest are: agent ↔ controller (mTLS + Pod-bound
  token), human/CLI ↔ operator API (bearer token), Alertmanager ↔ webhook
  (bearer token), and the operator's Kubernetes RBAC surface.
- Vulnerabilities in third-party dependencies (VictoriaMetrics, Alertmanager,
  dcgm-exporter, GPU Operator) should be reported upstream; version-bump
  requests here are welcome as regular issues.
