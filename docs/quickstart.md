# First run on Kubernetes

## Install in DryRun mode

With execution enabled, a KubeNeuron recovery policy can terminate a cloud
instance to replace the node. A fresh installation selects the cloud recovery pack and DryRun. Cloud recovery
uses provider APIs. Reboot through the node agent requires the explicit
`--policy-pack bare-metal-alpha` option (Alpha, not validated on real hardware). The cluster operator is responsible for enabling execution and
reviewing the policies that can act on workloads.

```sh
curl -fsSL https://github.com/kubeneuron/kubeneuron/releases/latest/download/install.sh | bash -s -- --version latest
```

Use a Kubernetes 1.29+ cluster with a default StorageClass. The installer
prints the port-forward command and credentials. Run that port-forward in a
separate terminal. In your working terminal set `SERVER=http://127.0.0.1:8080`
and supply the printed API token through `KUBENEURONCTL_TOKEN` in the
environment. Use the matching release of `kubeneuronctl`.

## Check GPU visibility

```sh
kubeneuronctl --server "$SERVER" status
kubeneuronctl --server "$SERVER" nodes
kubectl get kubeneuron kubeneuron -o yaml
```

The installer selects host tooling for x86-64 GPU nodes labelled
`nvidia.com/gpu.present=true` when they all report Amazon Linux 2023 with
host drivers, or Ubuntu with the GPU Operator driver deployment label.
These labels identify a likely filesystem layout. Successful inventory
requires working driver libraries and `nvidia-smi` on the selected nodes.
Use `--profile host-al2023` or `--profile gpu-operator-ubuntu` to select
one of these layouts explicitly. With mixed or unidentified layouts,
auto-detection leaves host tooling unset. Use `--profile none` to skip
host mounts deliberately.

A `GPUVisibility` reason of `SMIMissing` means the agent could not find the
executable in the mounted binary directory. For `DriverUnavailable`, check
the host driver and its libraries. `NoGPUResource` means fresh reports
contain zero GPUs and Kubernetes advertises no accelerator resource.
When node-level actions are enabled, `ResetAvailable` requires a fresh matching DCGM reset attestation. A node
can report GPU inventory while reset is unavailable.

Compare agent inventory with the GPU resources advertised by the node's
device plugin. For driver paths and DCGM client requirements, see
[NVIDIA host tooling](operations.md#nvidia-host-tooling-in-the-agent-daemonset).

## Open a first dry-run incident

Use a fresh DryRun installation. These commands submit a synthetic XID
application fault for the fictitious node `quickstart-demo`, find its
incident, and show its policy-selected playbook and audit trail. The
installer binds this class to the observe playbook.

<!-- first-run-recipe -->
```sh
kubeneuronctl --server "$SERVER" remediate quickstart-demo --class xid-app
incident=$(kubeneuronctl --server "$SERVER" incidents | awk '$3 == "xid-app" && $4 == "quickstart-demo" && !id {id=$1} END {print id}')
kubeneuronctl --server "$SERVER" incidents show "$incident"
```

Look for `dry-run:   true` and the installation's observe playbook.
For the local Go walkthrough, see the [developer demo](developer-demo.md).
