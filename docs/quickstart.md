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

```sh
openssl rand -hex 32 > /tmp/kn-token

./bin/kubeneuron-controller \
  -platform baremetal -inventory configs/inventory.example.yaml \
  -db /tmp/kubeneuron.db \
  -api-token-file /tmp/kn-token \
  -agent-listen :8443 -agent-tls-cert ... # see note below
```

> The controller requires the mTLS agent listener configuration on the
> Kubernetes path. For a pure ingestion demo without agents, the E2E suite
> above is the supported route; the operator-managed install
> (README “Operator preview install”) is the supported cluster route.

## 4. Poke it

```sh
export KUBENEURONCTL_TOKEN=$(cat /tmp/kn-token)

# open an incident manually (class selects the playbook via policy)
./bin/kubeneuronctl --server http://localhost:8080 remediate node-a --class ecc-dbe

./bin/kubeneuronctl --server http://localhost:8080 incidents
./bin/kubeneuronctl --server http://localhost:8080 incidents show <id>
./bin/kubeneuronctl --server http://localhost:8080 approve <id>
./bin/kubeneuronctl --server http://localhost:8080 status
```

Or open `http://localhost:8080/` in a browser, paste the token, and drive
the same flow from the embedded panel.

## 5. What to read next

- [design.md](design.md) — architecture and implementation boundaries.
- [operations.md](operations.md) — tokens, pausing, backup/restore, metrics.
- [xid-catalog.md](xid-catalog.md) — why each XID maps to its playbook.
- `ROADMAP.md` — what exists, what does not, and in which order it lands.
