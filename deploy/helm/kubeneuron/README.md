# kubeneuron Helm chart

Installs the KubeNeuron CRDs and operator. It deliberately does **not**
create an installation: the root `KubeNeuron` object, its TLS Secrets, and
API/webhook tokens are a separate, reviewed step — see the
[production install guide](../../../docs/install.md).

```sh
helm install kubeneuron ./deploy/helm/kubeneuron \
  --namespace kube-neuron --create-namespace \
  --set image.tag=vX.Y.Z
```

The chart's manifests mirror `config/default` + `config/rbac` (a repo test
pins the RBAC rules together); `kubectl apply -k config/default` remains the
kustomize-native equivalent.

## CRDs and upgrades

CRDs live in `crds/`: Helm installs them on first install but **never
upgrades or deletes them**. On chart upgrades apply the release's CRDs
explicitly first:

```sh
kubectl apply -f kubeneuron-install-vX.Y.Z.yaml --server-side
helm upgrade kubeneuron ./deploy/helm/kubeneuron --set image.tag=vX.Y.Z
```

See [docs/upgrade.md](../../../docs/upgrade.md) for the full order and
rollback rules.

## Accelerator adapters

`agentAdapters` lists accelerator adapters the operator enables on the agents
of every KubeNeuron installation. Each entry has the shape of
`spec.agent.adapters` (`vendor`, `options`, optional `hostTooling`), and an
installation's own entry replaces the default for the same vendor. The chart
passes the list to the operator as `--default-agent-adapters`; it is empty by
default, and NVIDIA observation is on either way.

```sh
helm upgrade --install kubeneuron ./deploy/helm/kubeneuron \
  --namespace kube-neuron --set image.tag=vX.Y.Z \
  --set-json 'agentAdapters=[{"vendor":"amd","options":{"tool":"amd-smi"}}]'
```

The adapter must be registered in the agent image. See
[docs/operations.md](../../../docs/operations.md#accelerator-adapters-in-the-agent).
