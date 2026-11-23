# Node replacement rehearsal

Run `make test-replacement-kind` with Docker, kind, kubectl, Go and jq.
Set `REPLACEMENT_PROVIDER` to `aws` (default), `gke` or `aks` to select the simulated cloud API.
The controller runs its provider logic against the simulator. The agent reports a synthetic Tesla T4.
Two injected journal faults exercise add-first and remove-first in one pool and workflow store.
In the AWS scenario, three more faults check policy qualification and configuration reload before drain, under Automatic and after a change to Approval.
`make test-replacement-assertions` checks the reload assertion against recorded incidents without a cluster.
Live AWS, GKE and AKS behavior, physical GPU health and CUDA execution require separate validation.
The runner builds images locally, loads them into kind and never publishes them.
Local image tags default to the kind cluster name. Set `REPLACEMENT_IMAGE_TAG` to override them.
