# Node replacement rehearsal

Run `make test-replacement-kind` with Docker, kind, kubectl, Go and jq.
Set `REPLACEMENT_PROVIDER` to `aws` (default), `gke` or `aks` to select the simulated cloud API.
The controller runs its provider logic against the simulator. The agent reports a synthetic Tesla T4.
Two injected journal faults exercise add-first and remove-first in one pool and workflow store.
After each replacement removes a machine, and before the runner deletes its Node, a `KubeNeuronAgentDown` alert for that node goes through the vmalert webhook: the replacing incident must record it, and no `agent-down` incident may open.
Each of the two faults is also written on its Node as the EKS node monitoring agent writes it: `AcceleratedHardwareReady=False` with reason `NvidiaXID79Error`.
The runner stops the removed machine and deletes its Node only after node health has reported the fault again while the incident verifies the replacement: the incident must record that report and must not count it as a recurrence.
In the AWS scenario, three more faults check policy qualification and configuration reload before drain, under Automatic and after a change to Approval.
`make test-replacement-assertions` checks the reload assertion against recorded incidents without a cluster.
Live AWS, GKE and AKS behavior, physical GPU health and CUDA execution require separate validation.
The runner builds images locally, loads them into kind and never publishes them.
Local image tags default to the kind cluster name. Set `REPLACEMENT_IMAGE_TAG` to override them.

## Controller restart after remove-first

Run the cases separately, one at a time, with simulated providers only.

```sh
REPLACEMENT_PROVIDER=aws make test-replacement-restart-kind
REPLACEMENT_PROVIDER=aks make test-replacement-restart-kind
REPLACEMENT_PROVIDER=aks RESTART_SCENARIO=identity-changed make test-replacement-restart-kind
REPLACEMENT_PROVIDER=aks RESTART_SCENARIO=lost-grow make test-replacement-restart-kind
REPLACEMENT_PROVIDER=gke RESTART_SCENARIO=lost-resize make test-replacement-restart-kind
```

During remove-first, the simulator records the accepted delete on the rehearsal
volume before the controller receives a response. The runner replaces the
controller pod, reconnects to its API and checks the incident audit against the
original operation. The resolved cases require one delete call, one deletion
and release of the incident's remediation ownership. The AKS identity-change
case reuses the machine name with a different provider ID and requires
NEEDS_HUMAN with the settlement command for that incident.
On AKS, the resolved cases also require one grow, the pool back at its node
count before the grow and no `kubeneuron-replacing` tag. In `lost-grow` the
grow is applied and its answer lost; only the new controller deletes the
machine. In `lost-resize` the GKE resize that sets the pool back is applied
and its answer lost; the new controller sends no second resize and ends the
plan on the pool's final state.
Set `KIND_ARTIFACTS_DIR` to retain the simulator snapshot and incident audit.
