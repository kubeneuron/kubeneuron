# KubeNeuron quickstart — dry-run demo in ~15 minutes

Goal: watch a synthetic XID 79 travel signal → incident → escalation ladder
→ approval → verification → resolved, with every side effect a logged
no-op, on any machine with Go 1.25+ (no GPU, no cluster).

## 1. Build and test

```sh
git clone <this repo> && cd kube-neuron
make build && make test
```

## 2. Run the automated end-to-end demo

The fastest way to see the whole loop is the E2E suite, which drives real
HTTP against the real controller:

```sh
go test ./test/e2e/ -v -run TestDryRunLadderWithApproval
```

Read the test: it posts an agent event for XID 79 ("GPU has fallen off the
bus"), waits for the incident to park in `AWAITING_APPROVAL` on the reboot
step, approves it via `POST /api/v1/incidents/{id}/approve`, and asserts
the full dry-run audit trail down to `RESOLVED`.

## 3. Run the controller interactively

The controller currently supports Kubernetes deployments only. Its agent API
authenticates Kubernetes workload identity, so `-platform baremetal` exits
before opening listeners rather than presenting a runnable-but-unsafe setup.
For a local ingestion demonstration, use the E2E suite above; for a cluster,
use the operator-managed installation in the README.

## 4. Operate an installed controller

After the Kubernetes installation is reachable, use `kubeneuronctl` with the
operator token and public controller address to list incidents, approve a
pending step, or invoke the global pause. The exact token retrieval and TLS
configuration are deployment-specific; see [operations.md](operations.md).

## 5. What to read next

- [design.md](design.md) — architecture and implementation boundaries.
- [operations.md](operations.md) — tokens, pausing, backup/restore, metrics.
- [xid-catalog.md](xid-catalog.md) — why each XID maps to its playbook.
- `ROADMAP.md` — what exists, what does not, and in which order it lands.
