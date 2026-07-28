# Contributing to KubeNeuron

Thanks for contributing. KubeNeuron is still an early skeleton, so keep a
clear distinction between implemented behavior and target design in code,
tests, and documentation.

## Before you start

- Read the [design document](docs/design.md), especially its current-state
  caveats and operator ownership boundaries.
- Check open issues and discussions to avoid duplicate work.
- Discuss non-trivial API changes, new playbook actions, platform
  implementations, or storage changes before investing in an implementation.
- Keep user-facing text, comments, and documentation in English.

## Development setup

Requirements: Go 1.25+, `make`, and optionally Docker for the development
Compose topology. Kubernetes API/operator work also needs `kubectl`; the
checked-in integration target uses Docker, jq, kind v0.32.0, and kubectl
v1.33.12 to create a digest-pinned Kubernetes v1.33.12 cluster. Kubernetes
1.29+ remains the API minimum, while unit tests require no cluster or GPU.

```sh
make build      # build all four binaries into bin/
make test       # run Go unit tests
make lint       # go vet; also runs golangci-lint when installed
make test-integration-kind  # run the slower, CPU-only real-cluster checks
```

The build outputs are `kubeneuron-operator`, `kubeneuron-controller`,
`kubeneuron-agent`, and `kubeneuronctl`.

Local container images for the four binaries can be built with `make docker`
(see `build/Dockerfile`). The end-to-end dry-run behavior is exercised by
`go test ./test/e2e/...` and the kind integration harness.

## Kubernetes API and operator changes

The API source lives in `api/v1alpha1/`; generated CRDs live in
`config/crd/bases/`. If markers or API fields change, regenerate both deepcopy
methods and CRDs with the pinned controller-tools version used by the
repository:

```sh
go run sigs.k8s.io/controller-tools/cmd/controller-gen@v0.17.3 \
  object paths=./api/v1alpha1

go run sigs.k8s.io/controller-tools/cmd/controller-gen@v0.17.3 \
  crd:allowDangerousTypes=true \
  paths=./api/v1alpha1 \
  output:crd:artifacts:config=config/crd/bases
```

Commit generated output with its source change and review the CRD diff for
scope, required fields, defaults, enum values, and status subresources. Do not
hand-edit generated CRDs.

For the repeatable CEL and operator smoke check:

```sh
make test-integration-kind
```

The target creates and normally deletes its own `kubeneuron-integration` kind
cluster. Set `KEEP_CLUSTER=1 KEEP_RESOURCES=1` to retain it, or use
`make kind-clean` afterward. The cleanup target deliberately leaves the
configurable kubeconfig path untouched. The harness refuses an existing
kubeconfig or named smoke fixture and reuse mode is only for a dedicated,
disposable kind cluster. It checks real API-server admission, readiness,
ownership, collision failure/non-adoption, recovery, RBAC, and idempotent
reconciliation. It also exercises mTLS and Pod-bound identity rejection,
induces a rogue-certificate agent rollout, proves readiness becomes stale
during a controller outage and then recovers after another durable
registration acknowledgment, and exercises routine server/client certificate
rotation with immutable versioned Secrets. The rotation smoke covers ordered
trust overlap, leaf activation, explicitly approved trust retirement, an
invalid-server-material failure/rollback, failed trust-contraction recovery,
same-ID/plan collision rejection, bound Secret UIDs, fresh post-controller
acknowledgments, scoped workload rollouts, and old-trust rejection. It does
not install the GPU Operator or observability
dependencies and makes no NVIDIA, NVML, DCGM, GPU-action, automated-PKI,
emergency-revocation, or remediation claim.

For a manual Kustomize-only smoke check against another cluster:

```sh
kubectl apply -k config/default
kubectl apply -k config/samples

kubectl get crd | grep kubeneuron.io
kubectl -n kube-neuron get deployment kubeneuron-operator
kubectl get kubeneurons.kubeneuron.io
```

The samples contain development settings. Review image references, Secret
references, store selection, and `executionMode` before applying them to any
shared cluster. Installing cleanly is not evidence that the unfinished
runtime can safely remediate real nodes.

When changing the reconciler, add focused tests for validation, ownership,
idempotent convergence, status conditions, and deletion/update behavior. The
operator must not silently accept fields the current runtime cannot execute.
VictoriaMetrics, Alertmanager, PostgreSQL, and ClickHouse lifecycle belongs to
their dedicated operators or external installations, not this reconciler.
The pinned preview dependency profile and its separate lifecycle are described
in [`deploy/kubernetes/dependencies/`](deploy/kubernetes/dependencies/).

## Pull requests

- Keep each PR focused on one logical change.
- Add tests for new behavior. Detection mapping, state transitions,
  configuration compilation, and safety gates need especially strong
  coverage.
- Run `make test` and `make lint` before requesting review.
- Follow standard Go style; exported identifiers need useful doc comments.
- Update the README/design status when a placeholder becomes real, and avoid
  documenting a roadmap item as available behavior.

## Safety-sensitive code

Changes to `internal/operator`, `internal/actuator`,
`internal/agent/executor`, `internal/playbook`, or `internal/safety` need
explicit failure-mode review.

- Actions must be typed, idempotent, bounded by timeouts, and compatible with
  dry-run.
- Never add an execution path that bypasses the safety gates or audit trail.
- Reboot-class and more destructive actions must require explicit approval.
- Configuration must fail closed when it references an unsupported action,
  missing dependency, or unresolved Secret.
- Tests must cover retry/reconcile behavior so a repeated event cannot repeat
  a destructive side effect unintentionally.

## Reporting bugs

Include what you expected, what happened, the relevant operator,
controller, and agent logs, the custom-resource status, and your environment
(Kubernetes version or bare-metal distribution, driver, and DCGM versions).
If an incident exists, include its audit data while removing credentials and
other sensitive values.

## License

By contributing, you agree that your contributions are licensed under the
[Apache 2.0 License](LICENSE).
