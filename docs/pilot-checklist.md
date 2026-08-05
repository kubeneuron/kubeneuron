# Pilot checklist — from `install.sh` to a first real incident

The install one-liner leaves you with a healthy control plane that does not
yet see your fleet. This page is the ordered list of what stands between
that and an incident appearing in the panel on a real EKS GPU cluster.
Every step is five minutes or less; the whole list is under an hour.

Names below assume the default root object name `kubeneuron` and namespace
`kube-neuron`. Substitute yours consistently — the Secret names install.sh
creates are all `<name>-…`.

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
remediating. Apply the samples (or your own) so each class you care about
has a policy and a playbook:

```sh
kubectl apply -k config/samples
kubectl get gpuremediationpolicy -o wide   # Ready=True, resolvedPlaybook set
```

Late-binding covers the race: an incident opened before its policy existed
is bound as soon as the policy lands.

## 6. Give the agent host tooling (real GPU nodes)

The agent image is distroless; `nvidia-smi`, `dcgmi`, and the driver
libraries come from the host:

```sh
kubectl -n kube-neuron patch kubeneuron kubeneuron --type merge -p '{
  "spec": {"agent": {"hostTooling": {"enabled": true}}}}'
```

Confirm the agent stopped reporting the fake driver:
`kubectl -n kube-neuron logs ds/kubeneuron-agent | grep -i "fake GPU driver"`
should print nothing.

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
is logged and audited as "would execute". Run the pilot here first. When
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
