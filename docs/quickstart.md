# First run on Kubernetes

## Install in DryRun mode

With execution enabled, a KubeNeuron recovery policy can terminate a cloud
instance to replace the node. A fresh installation selects the cloud recovery pack and DryRun. Cloud recovery
uses provider APIs. Reboot through the node agent requires the explicit
`--policy-pack bare-metal-alpha` option (Alpha, not validated on real hardware). The cluster operator is responsible for enabling execution and
reviewing the policies that can act on workloads.

```sh
curl -fsSL https://github.com/kubeneuron/kubeneuron/releases/download/v1.0.0-rc.2/install.sh | bash -s -- --version v1.0.0-rc.2
```

A release candidate is a pre-release and installs only by its explicit
version. There is no latest release yet. Upgrading from 0.x is not supported.

Use a cluster running Kubernetes 1.32 or later with a default StorageClass.
Follow the commands for your platform and release. This example uses Linux amd64 and the default
installation. Replace every occurrence of `v1.0.0-rc.2` in this example with the
version you installed. The [installation guide](install.md) explains how to
verify the signature on that release's checksum file.
On macOS the installer prints `shasum -a 256 -c` instead of `sha256sum -c`; use the commands it printed.

The third command reads the API token from the Kubernetes Secret
and passes it to kubeneuronctl through the KUBENEURONCTL_TOKEN environment variable.

In this terminal, download the CLI for the installed release and verify its checksum.

```sh
cli_dir=$(mktemp -d) && (cd "$cli_dir" && curl -fsSLO https://github.com/kubeneuron/kubeneuron/releases/download/v1.0.0-rc.2/kubeneuronctl-v1.0.0-rc.2-linux-amd64.tar.gz && curl -fsSLO https://github.com/kubeneuron/kubeneuron/releases/download/v1.0.0-rc.2/checksums-v1.0.0-rc.2.txt && awk -v file=kubeneuronctl-v1.0.0-rc.2-linux-amd64.tar.gz '$2 == file {print; found++} END {exit found != 1}' checksums-v1.0.0-rc.2.txt > cli.sha256 && sha256sum -c cli.sha256 && tar -xzf kubeneuronctl-v1.0.0-rc.2-linux-amd64.tar.gz kubeneuronctl-v1.0.0-rc.2-linux-amd64 && mv kubeneuronctl-v1.0.0-rc.2-linux-amd64 kubeneuronctl)
```

In a second terminal, start port forwarding to the controller and leave it running.

```sh
kubectl -n kube-neuron port-forward service/kubeneuron-controller 8080:8080
```

Back in the first terminal, load the API token from the Kubernetes Secret and check the controller's status.

```sh
KUBENEURONCTL_TOKEN=$(kubectl -n kube-neuron get secret kubeneuron-operator-api-token -o jsonpath='{.data.token}' | base64 -d) && export KUBENEURONCTL_TOKEN && "$cli_dir/kubeneuronctl" --server http://127.0.0.1:8080 status
```

The first line, `controller: <version>`, identifies the controller that answered.

## Check GPU visibility

```sh
"$cli_dir/kubeneuronctl" --server http://127.0.0.1:8080 nodes
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

By default the agent reads the GPUs through the NVIDIA container runtime and
needs none of these mounts; see
[NVIDIA host tooling](operations.md#nvidia-host-tooling-in-the-agent-daemonset).
`RuntimeDidNotExposeGPUs` means Kubernetes advertises NVIDIA GPUs but the
agents see none; `DeviceAccessDisabled` means `spec.agent.deviceAccess` is
`None`. `AgentRuntimeClassPodsNotCreated` and `AgentRuntimeClassNotStarting`
mean the agent Pods do not run with the RuntimeClass set in
`spec.agent.runtimeClassName`.

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
"$cli_dir/kubeneuronctl" --server http://127.0.0.1:8080 remediate quickstart-demo --class xid-app
incident=$("$cli_dir/kubeneuronctl" --server http://127.0.0.1:8080 incidents | awk '$3 == "xid-app" && $4 == "quickstart-demo" && !id {id=$1} END {print id}')
"$cli_dir/kubeneuronctl" --server http://127.0.0.1:8080 incidents show "$incident"
```

Look for `dry-run:   true` and the installation's observe playbook.
The incident can still be in `OBSERVING` when you first inspect it.
Run the last command again to see its current state and audit trail.
For the local Go walkthrough, see the [developer demo](developer-demo.md).
