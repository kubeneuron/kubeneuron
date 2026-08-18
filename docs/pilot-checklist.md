# Pilot checklist — from `install.sh` to a first real incident

The install one-liner leaves you with a healthy control plane that does not
yet see your fleet. This page is the ordered list of what stands between
that and an incident appearing in the panel on a real EKS GPU cluster.
Every step is five minutes or less; the whole list is under an hour.

Names below assume the default root object name `kubeneuron` and namespace
`kube-neuron`. Substitute yours consistently — the Secret names install.sh
creates are all `<name>-…`.

## 0. Is your fleet a good fit?

Read this before the rest. Every fact below is stated somewhere in
[the capability matrix](reference-capabilities.md), but nobody assembles them
into the question you actually have, so here it is assembled.

| Your fleet | Detection | Protection (cordon, drain, taint) | Repair |
|---|---|---|---|
| **NVIDIA on AWS** | proven on hardware (kernel XID) | proven | proven — but the validated repair is `ReplaceNode`, which throws the machine away |
| **NVIDIA on GCP / Azure** | proven on hardware | proven | **the ladder tops out at a reboot rung that has never executed** — no cloud provider is implemented but AWS |
| **NVIDIA on bare metal** | proven on hardware | proven | **your pilot would be the first real per-device GPU reset anywhere** |
| **AMD, anywhere** | shipped, synthetic fixtures only — never run on AMD silicon | proven | detect, protect and close only: no arming, no reset |
| **Intel** | none | — | — |

Three rows deserve a sentence rather than a cell.

**"Proven on hardware" is narrower than it sounds.** What ran on a real T4 is
the kernel-log transport: an XID *we injected* into `/dev/kmsg`, classified,
opening an incident. That is the load-bearing path and it works. But the
faults were injected rather than emitted by a dying GPU, and until now the
stand's agent had no host tooling — so the incidents it opened were
node-scoped, with no per-device attribution and no second detection source
behind them. Read the cell as *the detection pipeline is proven end to end on
real hardware*, not as *every detector has met a real fault*. The capability
matrix splits this hair per row; this table cannot.

**Bare metal is the sharp one.** In a cloud VM there is no PCI reset, so the
agent refuses a per-device reset on measured evidence — a refusal that has
been validated. On bare metal `/sys/bus/pci/devices/<addr>/reset` exists, the
capability is granted, and that code path runs for the first time in your
cluster. Keep `executionMode: DryRun` longer than you otherwise would, and
tell us before you arm it.

**AMD detects but does not repair.** Faults are recognised, classified, and
the node is cordoned and drained; the incident then resolves on the agent
heartbeat rather than on a device attestation, which the controller logs as
reduced verification depth. Nothing arms, and no reset exists. That is a
useful product — it is not the whole one.

If you are on NVIDIA/AWS, the rest of this page is an hour. If you are not,
say so when you get in touch: which cloud you run decides what we build next.

## 1. Dependencies, before KubeNeuron

Install the pinned observability + GPU profile:
[`deploy/kubernetes/dependencies/`](https://github.com/kubeneuron/kubeneuron/tree/main/deploy/kubernetes/dependencies).
You need DCGM exporting metrics and Alertmanager reachable in-cluster.

On EKS specifically:
- the **EBS CSI addon** must be installed before the controller's PVC can
  bind, and a default StorageClass must exist (`gp2`/`gp3` annotated
  `storageclass.kubernetes.io/is-default-class=true`);
- install the GPU operator with **`dcgm.enabled=true`** — the agent's
  attestation and the second detection source both read DCGM;
- the EKS NVIDIA AMI already ships the driver, so `driver.enabled=false`.

## 2. Install KubeNeuron

```sh
curl -sfL https://github.com/kubeneuron/kubeneuron/releases/latest/download/install.sh \
  | bash -s -- --version latest
```

Confirm: `kubectl get crd | grep kubeneuron.io` shows seven CRDs Established,
and `kubectl -n kube-neuron get kubeneuron kubeneuron -o jsonpath='{.status.conditions[?(@.type=="Ready")].reason}'`
prints `RuntimeAvailable`.

## 3. Point the installation at your real observability endpoints

`install.sh` writes **placeholders** (`vmsingle-unset`, `alertmanager-unset`)
so the object validates before your stack exists. Replace them:

```sh
kubectl -n kube-neuron patch kubeneuron kubeneuron --type merge -p '{
  "spec": {"observability": {
    "victoriaMetrics": {"mode": "External", "endpoint": "http://vmsingle-vm.monitoring.svc:8429"},
    "alertmanager":    {"mode": "External", "endpoint": "http://alertmanager.monitoring.svc:9093"}
  }}}'
```

## 4. Let Alertmanager authenticate to the webhook

The webhook requires a bearer token. Copy the one install.sh generated into
your Alertmanager's namespace and reference it from the receiver:

```sh
TOKEN=$(kubectl -n kube-neuron get secret kubeneuron-webhook-token \
  -o jsonpath='{.data.token}' | base64 -d)
kubectl -n monitoring create secret generic kubeneuron-webhook-token \
  --from-literal=token="$TOKEN" --dry-run=client -o yaml | kubectl apply -f -
```

Receiver URL:
`http://kubeneuron-controller.kube-neuron.svc:8080/api/v1/webhooks/alertmanager`
with `Authorization: Bearer <token>`.

## 5. Apply a policy set that covers real classes

The starter policy binds one class (`xid-app`). Every other class your
rules emit — `ecc-dbe`, `thermal`, `fell-off-bus`, … — would open an
incident with **no playbook**, which observes and quiet-resolves without
remediating.

Be clear about what the samples do and do not fix. They add exactly one more
class (`gsp-error`), so applying them takes you from one bound class to two,
out of the twenty-odd the detectors can emit. They are a worked example of the
shape, not a starter pack:

```sh
kubectl apply -k config/samples
kubectl get gpuremediationpolicy -o wide   # Ready=True, resolvedPlaybook set
```

**Write a policy and a playbook for each class you actually care about.** This
is the step most worth your time, and the one whose absence is least visible
later: an unbound class does not error, does not alert, and does not appear as
a gap. It observes, quiet-resolves, and is then counted as *recovered* in the
capacity report — so an incomplete policy set does not merely fail to help,
it inflates the number you take to whoever pays for the fleet.

Reconcile the two lists before you go further:

```sh
# What your rules can emit, against what you have bound.
kubectl get gpuremediationpolicy -o jsonpath='{range .items[*]}{.spec.match.class}{"\n"}{end}' | sort -u
```

Late-binding covers the race: an incident opened before its policy existed
is bound as soon as the policy lands.

## 6. Give the agent host tooling (real GPU nodes)

The agent image is distroless; `nvidia-smi`, `dcgmi`, and the driver
libraries come from the host. Declaring the block is what enables it — there is
no `enabled` field, and the paths below are the AL2023 NVIDIA AMI's layout:

```sh
kubectl -n kube-neuron patch kubeneuron kubeneuron --type merge -p '{
  "spec": {"agent": {"hostTooling": {
    "binDir": "/usr/bin",
    "libDirs": ["/usr/lib64"],
    "dcgmEndpoint": "nvidia-dcgm.gpu-operator.svc:5555"}}}}'
```

`dcgmEndpoint` is not optional in practice, even though the API allows omitting
it. The agent pod is not host-networked, so `dcgmi`'s local default reaches no
engine: leave it empty and runtime attestation stays degraded and the second
detection source quietly falls back to `nvidia-smi` — which is the narrower
source, and nothing says so out loud. The value above is the GPU operator's
Service; it carries `internalTrafficPolicy: Local`, which is what keeps the
evidence node-local. Verify that before pointing it anywhere else, or
attestation becomes hearsay about another node's hardware. It also requires the
standalone engine from §1 (`dcgm.enabled=true`) — the operator ships it off.

Declaring the block also arms `--require-real-driver`, so an agent that cannot
find `nvidia-smi` now fails to start instead of silently running the fake
driver. Confirm the rollout completed, then that nothing reports the fallback:

```sh
kubectl -n kube-neuron rollout status ds/kubeneuron-agent
kubectl -n kube-neuron logs ds/kubeneuron-agent | grep -i "fake GPU driver"
```

The second command should print nothing. A crash-looping agent here means the
paths above do not match your AMI — read the container log, not the events.

## 7. Prove the pipeline end to end, synthetically

Send a fault-shaped alert to the webhook and watch an incident open — no
real hardware failure required:

```sh
kubectl -n kube-neuron port-forward svc/kubeneuron-controller 8080:8080 &
curl -sS -X POST http://127.0.0.1:8080/api/v1/webhooks/alertmanager \
  -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{"alerts":[{"status":"firing",
        "labels":{"alertname":"GpuDoubleBitEccVolatile","severity":"critical",
                  "node":"<a-real-gpu-node>","gpu":"0"},
        "annotations":{"summary":"synthetic pilot check"}}]}'

kubeneuronctl incidents            # the incident is here
```

Resolve it when you are satisfied:
`kubeneuronctl resolve <id> --actor you --reason "synthetic pilot check"`.

## 8. Wire the alerts that watch KubeNeuron itself

Load [`configs/vmalert/self-rules.yaml`](https://github.com/kubeneuron/kubeneuron/blob/main/configs/vmalert/self-rules.yaml)
into your Prometheus (the dependency profile's VMRule already carries them).
Every rule links to a [runbook](runbooks.md) entry. The ones that matter on
day one: `KubeNeuronAgentNeverAcked`, `KubeNeuronIncidentExpired`,
`KubeNeuronStackRestoreFailing`.

## 9. Stay in DryRun until you have watched it decide

`executionMode: DryRun` is the default and it executes nothing: every step
is logged and audited as "would execute". Run the pilot here first.

While you are here, `kubeneuronctl report` prints a `SIMULATED` section: how
many incidents the ladder would have carried to resolution, how many without
asking anybody, and the GPU-hours involved. The degraded hours are real; the
recovery is not. That section is the argument for phase two — take it to
whoever pays for the fleet before you enable enforcement, not after. When
you are ready for real remediation, confine it explicitly — see
[install.md](install.md#enabling-real-execution) for
`spec.safety.destructiveExecution` (a non-empty node selector plus the exact
acknowledgement string) and start with one canary node pool.

## Before you page anyone

- Backups: apply the CronJob in `deploy/kubernetes/backup/` and rehearse
  the [restore](operations.md) once against a real snapshot.
- Approvals: point `spec.notifications` at a channel with an on-call
  rotation. An unanswered approval expires (default 12h) and leaves the
  node cordoned — `KubeNeuronIncidentExpired` is the alert that tells you.
