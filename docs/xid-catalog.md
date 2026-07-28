# XID Error Catalog

Which NVIDIA XID errors KubeNeuron reacts to, and why. The authoritative
classification table is `internal/detect/xid.go`; this document explains the
reasoning. Reference: [NVIDIA XID errors documentation](https://docs.nvidia.com/deploy/xid-errors/).

XID codes not listed here are still counted (metrics + event archive) but
open no incident.

## Hardware-critical — act immediately

| XID | Meaning | Response | Why |
|---|---|---|---|
| **48** | Double-bit ECC error | `drain-and-reset` | Uncorrectable memory fault. Workloads on this GPU are producing corrupt results or crashing. After reset, check that row remap took effect; recurrence points to RMA. |
| **95** | Uncontained ECC error | `drain-and-reset` | GPU state is corrupt beyond containment. Same treatment as 48; recurrence → RMA. |
| **64** | Row remap failed | `rma` | The row remap could not be recorded. Drain and reset, then re-check row-remap state and diagnostics; a recurrence after a clean reset is the signal to quarantine, collect a bundle, and escalate the hardware. |
| **79** | GPU has fallen off the bus | `fell-off-bus` (drain → reboot, approval) | The device vanished from PCIe; a GPU reset can't reach it. Only a reboot (or power cycle) can bring it back. Recurrence indicates hardware (riser, power, board) → RMA. |
| **74** | NVLink error | `drain-and-reset` | Link-level fault. Often recoverable by reset, but recurring NVLink errors within 24 h suggest cabling/topology — escalate to reboot and flag in the ticket. |

## Driver / GSP — reset ladder

| XID | Meaning | Response | Why |
|---|---|---|---|
| **119** | GSP RPC timeout | `gpu-reset` → reboot → driver-reinstall | GSP firmware stopped responding. Usually a reset clears it; persistent recurrence means a driver/firmware problem. |
| **120** | GSP error | same as 119 | Same subsystem, same ladder. |

## Contained / informational — surgical response

| XID | Meaning | Response | Why |
|---|---|---|---|
| **94** | Contained ECC error (A100+) | `workload-restart` | The error was contained to one workload's context. Only that workload is corrupt — evict it, keep the GPU in service. ≥3 per week → drain-and-reset (containment is masking a degrading part). |
| **63** | Row remap recorded | `reset-when-idle` | The recovery mechanism worked: a failing row was retired. The remap applies on the next reset, so there is no urgency — wait for idle instead of evicting work. |
| **92** | High single-bit ECC rate | observe | Corrected errors — no data corruption. Watch the trend; combine with volatile SBE counters before acting. |

## Application-level — observe first

| XID | Meaning | Response | Why |
|---|---|---|---|
| **13** | Graphics engine exception | observe, threshold 3/1h | Almost always a workload bug (bad kernel, OOB access). Suspect hardware only when different workloads trip it on the same GPU. |
| **31** | GPU memory page fault | observe, threshold 3/1h | Same: an application dereferencing bad pointers, not a hardware fault — unless it recurs across workloads. |
| **43** | Reset channel verification error | count only | Usually secondary to an application termination or another GPU fault. Tracked for correlation, never actioned alone. |
| **46** | GPU stopped processing | count only | App crash noise. Tracked for correlation, never actioned alone. |

## Non-XID signals (metrics path)

See `configs/vmalert/gpu-rules.yaml`:

- **ECC DBE volatile counters** — metrics backstop for XID 48/95.
- **Row remap pending / failure / uncorrectable rows ≥ 8** — remap lifecycle
  and bank-exhaustion proxy; the last is an RMA-evaluate signal.
- **Thermal throttle / temp > 88 °C / power brake** — facility problems.
  Draining protects workloads, but resetting the GPU fixes nothing — these
  route to observe/ticket, not to the reset ladder.
- **NVLink CRC, PCIe replay storms** — link degradation trends.
- **Exporter down / GPU count below inventory / nvidia-smi timeout** —
  liveness triage: distinguishes driver hang (reset ladder) from node death
  (reboot) from a fell-off-bus GPU (count mismatch).
- **Agent heartbeat stale** — meta-signal: automation on that node is
  suppressed because post-action verification is impossible.
