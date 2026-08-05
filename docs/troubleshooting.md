# Troubleshooting

Symptom-first. `<ns>`/`<name>` are your installation namespace and root
object name.

## The root KubeNeuron is not Ready

```sh
kubectl get kubeneuron <name> -o jsonpath='{range .status.conditions[*]}{.type}={.status} {.reason}: {.message}{"\n"}{end}'
```

- `ConfigurationValid=False` — the message names the offending object and
  field; validation is deliberately fail-closed (unsupported modes, label
  matchers, credential refs, reboot without approval…). Fix the CR; the
  operator requeues automatically.
- `Ready=False/ReconciliationFailed` with an ownership message — a
  pre-existing object with a managed name is not owned by this
  installation. The operator never adopts; rename or delete the collider.
- Readiness waits on the PVC: it must be Bound, at requested capacity, with
  no resize in flight — check `kubectl -n <ns> describe pvc`.

## Agent pods are Running but not Ready

Readiness = a durable controller registration acknowledgment in the last
90 s. In order:

1. Controller up? `kubectl -n <ns> logs deploy/<name>-controller`
2. TLS: agent logs showing certificate verification errors → wrong CA
   bundle, expired leaf (>100-day leaves are rejected), or a URI SAN that
   does not match `spiffe://kubeneuron.io/installation/<ROOT_UID>/agent`
   (did you recreate the root? the UID changed).
3. Token: `403` on registration → TokenReview rejected the projected token
   (wrong audience/ServiceAccount) or the Pod/DaemonSet binding check
   failed.
4. A first `403` right after a rollout is normal (token projection racing
   the first heartbeat); it must clear within one 30 s retry.

## An incident is stuck

`kubeneuronctl incidents show <id>` — the audit trail states the reason in
plain text. Common holds (all deliberate, all retried every tick):

| Stuck in | Likely hold |
|---|---|
| `EVALUATING`, step 0 | global pause, per-node pause (`GPUNodeConfig`), an active maintenance window, playbook cooldown, or a gate concurrency cap |
| `AWAITING_APPROVAL` | nobody decided; check Slack/panel; expires to `EXPIRED` after the TTL |
| `OBSERVING` | below the policy threshold — that's the design |
| `NEEDS_HUMAN` | ladder exhausted, flap quarantine, or a rejected approval; fix the node, then `kubeneuronctl resolve <id>` |
| `EXECUTING` for very long | a drain honoring PDBs, or (non-dry-run) an action waiting for the agent — check the actions queue and agent logs |

## Signals arrive but no incident opens

- Unknown XID / alertname: only cataloged codes open incidents; extend via
  `GPUSignalMapping`. The raw event is still archived in the `events`
  table.
- Alert path: node identity comes from `node`/`Hostname` labels (falling
  back to port-stripped `instance`); if these don't match agent-registered
  node names you get *two* incidents, not none — fix your relabeling.
- Queue overflow: check `kubeneuron_signals_dropped_total` — critical
  signals wait briefly for space, others drop with a counted log line.

## Duplicate or missing remediation after restarts

The agent single-flights actions and records each one in a crash-safe
action journal: intent is persisted before a side effect starts and the
outcome before it is reported, so a crash between dispatch and result
recovers as a conservative *outcome unknown* rather than a second firing.
`executionMode: Enabled` remains off by default and confined by
`spec.safety.destructiveExecution`. Capture the incident audit trail and
agent logs to establish what actually ran on an outcome-unknown action.

## Metrics/panel/CLI return 401 or 503

`503 operator API disabled` — the controller has no `-api-token-file`.
`401` — wrong token (constant-time compared; no partial match hints).
`/metrics` and `/healthz` never require tokens; operator API routes require
the API token, the Alertmanager webhook requires its distinct webhook token in
an operator-managed installation, and agent routes use mTLS plus Pod-bound
identity.

## Collecting evidence for a bug report

```sh
kubectl -n <ns> logs deploy/<name>-controller --since=1h > controller.log
kubeneuronctl incidents show <id> > incident.txt
kubectl -n <ns> exec deploy/<name>-controller -- sqlite3 /var/lib/kube-neuron/kubeneuron.db ".backup /tmp/db"  # optional
```

Include the root's `status.conditions`, the compiled config digest, and
binary versions (`--version`).
