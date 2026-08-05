package nvidia

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/kubeneuron/kubeneuron/internal/accelerator"
	"github.com/kubeneuron/kubeneuron/internal/agent/nvml"
	"github.com/kubeneuron/kubeneuron/pkg/types"
)

// resetProbeDriver is a driver that can answer whether the platform exposes a
// PCI reset, which the fake driver deliberately cannot.
type resetProbeDriver struct {
	*nvml.Fake
	capability nvml.ResetCapability
	err        error
}

func (d *resetProbeDriver) ResetCapability(context.Context, int) (nvml.ResetCapability, error) {
	return d.capability, d.err
}

func gpuDriver(capability nvml.ResetCapability, err error) *resetProbeDriver {
	return &resetProbeDriver{
		Fake:       &nvml.Fake{GPUs: []types.GPUInfo{{Index: 0, UUID: "GPU-a", Model: "Tesla T4"}}},
		capability: capability,
		err:        err,
	}
}

// A node whose hypervisor withholds the PCI reset must not advertise the
// capability. Otherwise a playbook cordons and drains the node before anyone
// discovers the reset was never possible — the exact sequence measured on an
// AWS g4dn.xlarge.
func TestResetIsNotAdvertisedWhenThePlatformCannotDoIt(t *testing.T) {
	adapter := newAdapter(t, Config{NodeName: "n1", PartitionTopology: PartitionTopologyNone},
		gpuDriver(nvml.ResetCapability{Supported: false, Detail: "the kernel exposes no PCI reset for device 0000:00:1e.0"}, nil))

	capabilities, err := adapter.Capabilities(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if capabilities.Supports(accelerator.ActionResetDevice, accelerator.ScopePhysicalDevice) {
		t.Fatal("reset-device must not be advertised on a platform with no PCI reset")
	}
	// Health checks stay available: the node is still worth monitoring.
	if !capabilities.Supports(accelerator.ActionVerifyHealth, accelerator.ScopeNode) {
		t.Fatal("verify-health must survive")
	}

	report := adapter.Preflight(context.Background())
	if report.Readiness == PreflightEligible {
		t.Fatalf("readiness = %v, want the node ineligible for reset", report.Readiness)
	}
	joined := strings.Join(report.Reasons, " | ")
	if !strings.Contains(joined, "no PCI reset") {
		// "capability not declared" would send an operator looking for a
		// configuration mistake that does not exist.
		t.Fatalf("reasons = %q, want the platform cause named", joined)
	}
}

func TestResetIsAdvertisedWhenThePlatformSupportsIt(t *testing.T) {
	adapter := newAdapter(t, Config{NodeName: "n1", PartitionTopology: PartitionTopologyNone},
		gpuDriver(nvml.ResetCapability{Supported: true, Methods: []string{"flr"}}, nil))

	capabilities, err := adapter.Capabilities(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !capabilities.Supports(accelerator.ActionResetDevice, accelerator.ScopePhysicalDevice) {
		t.Fatal("reset-device must be advertised when the kernel exposes a reset")
	}
}

func TestResetProbeFailureIsTreatedAsNoReset(t *testing.T) {
	adapter := newAdapter(t, Config{NodeName: "n1", PartitionTopology: PartitionTopologyNone},
		gpuDriver(nvml.ResetCapability{}, errors.New("sysfs unavailable")))

	capabilities, err := adapter.Capabilities(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if capabilities.Supports(accelerator.ActionResetDevice, accelerator.ScopePhysicalDevice) {
		t.Fatal("an unanswerable probe must fail closed")
	}
}

// A driver without the probe keeps the previous behaviour; the fake driver and
// any future driver must not lose the capability by omission.
func TestDriverWithoutTheProbeKeepsResetCapability(t *testing.T) {
	adapter := newAdapter(t, Config{NodeName: "n1", PartitionTopology: PartitionTopologyNone},
		&nvml.Fake{GPUs: []types.GPUInfo{{Index: 0, UUID: "GPU-a"}}})

	capabilities, err := adapter.Capabilities(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !capabilities.Supports(accelerator.ActionResetDevice, accelerator.ScopePhysicalDevice) {
		t.Fatal("a driver that cannot answer must not be silently downgraded")
	}
}
