package nvml

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// defaultSysfsRoot is where the kernel exposes PCI device attributes. The agent
// container shares the host's sysfs, so the default reads the real hardware.
const defaultSysfsRoot = "/sys"

// ResetCapability describes whether the kernel can reset a GPU's PCI function.
type ResetCapability struct {
	// Supported is true when the kernel exposes a reset for the device.
	Supported bool
	// Methods is the kernel's ordered reset-method list ("flr", "bus", "pm"),
	// available since Linux 5.13. Empty on older kernels even when a reset
	// exists — Supported is the authoritative field.
	Methods []string
	// Detail explains a negative result in operator-facing terms.
	Detail string
}

// ResetCapability reports whether the GPU at the given index can actually be
// reset on this machine.
//
// This is a property of the platform, not of the driver or the card. On a
// virtualized cloud instance the hypervisor does not expose a reset method to
// the guest: measured on an AWS g4dn.xlarge, /sys/bus/pci/devices/<addr>/reset
// does not exist and nvidia-smi --gpu-reset fails with "GPU is currently in use
// by another process" even with nothing using the GPU, no persistence daemon,
// and the kernel modules unloaded. That message names no cause, so a fleet can
// spend a long time believing it has a software problem.
//
// Probing it during attestation lets the capability gate refuse a reset before
// a playbook cordons and drains a node for one that cannot succeed.
func (s *SMI) ResetCapability(ctx context.Context, index int) (ResetCapability, error) {
	address, err := s.pciAddressFor(ctx, index)
	if err != nil {
		return ResetCapability{}, err
	}
	root := s.SysfsRoot
	if strings.TrimSpace(root) == "" {
		root = defaultSysfsRoot
	}
	deviceDir := filepath.Join(root, "bus", "pci", "devices", address)
	if _, err := os.Stat(deviceDir); err != nil {
		// Without the device directory nothing can be concluded, so this is an
		// error rather than "no reset": a silent negative would permanently
		// disable resets on a machine whose sysfs simply is not mounted.
		return ResetCapability{}, fmt.Errorf("reading PCI attributes for GPU %d at %s: %w", index, address, err)
	}
	capability := ResetCapability{}
	if _, err := os.Stat(filepath.Join(deviceDir, "reset")); err == nil {
		capability.Supported = true
	} else if !os.IsNotExist(err) {
		return ResetCapability{}, fmt.Errorf("reading reset attribute for GPU %d: %w", index, err)
	}
	if data, err := os.ReadFile(filepath.Join(deviceDir, "reset_method")); err == nil {
		capability.Methods = strings.Fields(string(data))
	}
	// A kernel that lists reset methods but exposes no reset attribute, or one
	// whose method list is empty, cannot reset the function.
	if capability.Supported && len(capability.Methods) == 0 {
		if _, err := os.Stat(filepath.Join(deviceDir, "reset_method")); err == nil {
			capability.Supported = false
			capability.Detail = fmt.Sprintf("the kernel lists no usable reset method for PCI device %s", address)
		}
	}
	if !capability.Supported && capability.Detail == "" {
		capability.Detail = fmt.Sprintf(
			"the kernel exposes no PCI reset for device %s; on a virtualized instance the hypervisor withholds it and no GPU reset can succeed",
			address)
	}
	return capability, nil
}

// pciAddressFor returns the sysfs-form PCI address ("0000:00:1e.0") of a GPU.
// nvidia-smi prints "00000000:00:1E.0", which sysfs does not recognise.
func (s *SMI) pciAddressFor(ctx context.Context, index int) (string, error) {
	gpus, err := s.refresh(ctx)
	if err != nil {
		return "", err
	}
	for _, g := range gpus {
		if g.info.Index == index {
			return sysfsPCIAddress(g.pci), nil
		}
	}
	return "", fmt.Errorf("no GPU with index %d", index)
}

// sysfsPCIAddress converts the normalized "dddd:bb:dd" key back into the
// sysfs directory name, which always carries a function suffix.
func sysfsPCIAddress(normalized string) string {
	if normalized == "" {
		return ""
	}
	if strings.Count(normalized, ":") == 2 && !strings.Contains(normalized, ".") {
		return normalized + ".0"
	}
	return normalized
}
