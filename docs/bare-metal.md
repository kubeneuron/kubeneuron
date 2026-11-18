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

## Known issues

- **Uncertain action outcomes.** After dispatch, a single error reading the
  action record can fail the step and trigger its configured escalation.
  The agent may already have reset the GPU or rebooted the node. Check the
  node and action record before approving a further intervention. After a
  controller restart, re-evaluating an interrupted step also leaves its
  existing queued action in place.
- **Reset checks.** The controller checks the last reported device holders
  without checking the age of that observation. The agent still performs
  its local preflight. When node labels or accelerator evidence are missing,
  the controller measures the grace period from the incident's last state
  transition. A late first hold can therefore reach quarantine immediately.
- **Diagnostics.** A health check can show `timed_out` while its diagnostic
  action is still leased to the agent. Check the action outcome before
  starting another diagnostic. The `run_diag` command uses the pinned
  DCGM client path and omits the host-engine endpoint configured for
  observation. A successful `verify.gpu_health` step checks actuator
  health; confirming physical recovery requires separate GPU evidence.
- **Deployment limits.** The SSH actuator is a stub. The Kubernetes agent
  runs privileged and shares the host PID namespace even when node-level
  actions are disabled. See [Data and privacy](data-and-privacy.md).
