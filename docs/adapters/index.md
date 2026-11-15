# Adapter contract

Documentation of the adapter contract and of any adapter protocol lives in
this directory and is licensed under the Apache License 2.0 (see
[LICENSE](https://github.com/kubeneuron/kubeneuron/blob/main/LICENSE)).

The contract itself is two standard-library-only Go modules:

- `github.com/kubeneuron/kubeneuron/pkg/accelerator`: accelerator adapters
  (inventory, capabilities, health, detection).
- `github.com/kubeneuron/kubeneuron/pkg/cloud`: cloud provider adapters.

No out-of-process adapter protocol is published yet.
