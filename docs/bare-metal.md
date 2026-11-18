# Bare metal (alpha)

**Alpha, not validated on real hardware.**

Choose `--policy-pack bare-metal-alpha` explicitly on a fresh installation
to install the node-level recovery playbooks. The default cloud pack uses
provider APIs. An agent reboot restarts the host operating system. GPU reset
and driver operations can interrupt workloads using the device.

Start in `DryRun` to inspect the proposed steps. For live node-level
recovery use an approval policy. An Automatic policy that reaches an agent
action through an escalation also fails validation.

## NVIDIA diagnostics and reset checks

Diagnostics and some pre-reset checks require a working DCGM client that
you supply under NVIDIA's terms. GPU discovery with `nvidia-smi` alone
does not enable node-level actions. See [DCGM client](install.md#dcgm-client)
for the agent's behavior when runtime attestation is unavailable.

## What has been tested

Tests with simulated devices exercise action refusal and execution. Physical
bare-metal GPU reset, host reboot and driver recovery have yet to be validated.
The historical EKS hardware run used a synthetic fault to exercise cordon,
drain and EC2 instance termination. That run ended at termination. See the
[capability reference](reference-capabilities.md) for evidence by action.
