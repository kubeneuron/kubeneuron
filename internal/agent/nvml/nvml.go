// Package nvml wraps the NVIDIA Management Library behind a small interface
// so the rest of the agent (and the test simulator) never touches NVML
// directly.
//
// SKELETON: the real implementation uses github.com/NVIDIA/go-nvml (cgo; the
// library dlopens libnvidia-ml at runtime, so it builds without a GPU). It
// is wired in Phase 1. The Fake implementation below keeps the agent
// buildable and testable on GPU-less machines.
package nvml

import (
	"context"
	"errors"
	"fmt"

	"github.com/kubeneuron/kubeneuron/pkg/types"
)

// ErrNotIdle means the device is genuinely still held by live processes — the
// idle guard REFUSING, which is the protection working.
//
// It exists to be distinguishable from the other way an idle check fails: a
// probe that could not run at all (nvidia-smi missing, a wedged driver, a
// timeout). Both fail closed and neither lets a reset through, but only the
// first is evidence that a workload was spared, and a control plane that
// counts the second as protection reports a broken driver as value delivered.
var ErrNotIdle = errors.New("device is not idle")

// GPUDriver is what the agent needs from NVML.
type GPUDriver interface {
	// Init attaches to the driver; Shutdown releases it.
	Init() error
	Shutdown() error
	// ListGPUs enumerates local GPUs with index, UUID, and model.
	ListGPUs(ctx context.Context) ([]types.GPUInfo, error)
	// GPUByPCIAddr resolves a PCI address (as printed in XID lines) to a GPU.
	GPUByPCIAddr(ctx context.Context, pciAddr string) (types.GPUInfo, error)
	// ResetGPU performs a GPU reset (nvmlDeviceResetGpu / nvidia-smi -r).
	// The GPU must be idle; callers drain first.
	ResetGPU(ctx context.Context, index int) error
	// EnsureIdle fails when any process still holds the GPU (or when idleness
	// cannot be determined — fail closed). Reset paths call it after the
	// drain as a final safety net.
	EnsureIdle(ctx context.Context, index int) error
	// WatchEvents subscribes to NVML XID/clock events as a second detection
	// source next to kmsg.
	WatchEvents(ctx context.Context) (<-chan types.AgentEvent, error)
	// Healthy runs a cheap liveness probe (device count query with a
	// deadline); a hang indicates a wedged driver.
	Healthy(ctx context.Context) error
}

// Fake is a GPU-less GPUDriver for development and the test simulator.
type Fake struct {
	GPUs []types.GPUInfo
}

var _ GPUDriver = (*Fake)(nil)

// Init implements GPUDriver.
func (f *Fake) Init() error { return nil }

// Shutdown implements GPUDriver.
func (f *Fake) Shutdown() error { return nil }

// ListGPUs implements GPUDriver.
func (f *Fake) ListGPUs(ctx context.Context) ([]types.GPUInfo, error) { return f.GPUs, nil }

// GPUByPCIAddr implements GPUDriver; the fake maps addresses by index order.
func (f *Fake) GPUByPCIAddr(ctx context.Context, pciAddr string) (types.GPUInfo, error) {
	if len(f.GPUs) == 0 {
		return types.GPUInfo{}, fmt.Errorf("nvml fake: no GPUs configured")
	}
	return f.GPUs[0], nil
}

// ResetGPU implements GPUDriver.
func (f *Fake) ResetGPU(ctx context.Context, index int) error { return nil }

// EnsureIdle implements GPUDriver; the fake is always idle.
func (f *Fake) EnsureIdle(ctx context.Context, index int) error { return nil }

// WatchEvents implements GPUDriver.
func (f *Fake) WatchEvents(ctx context.Context) (<-chan types.AgentEvent, error) {
	ch := make(chan types.AgentEvent)
	go func() { <-ctx.Done(); close(ch) }()
	return ch, nil
}

// Healthy implements GPUDriver.
func (f *Fake) Healthy(ctx context.Context) error { return nil }
