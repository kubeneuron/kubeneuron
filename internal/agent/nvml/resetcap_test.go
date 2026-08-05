package nvml

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeSysfs builds /sys/bus/pci/devices/<addr>/ with the given attribute files.
func fakeSysfs(t *testing.T, addr string, attrs map[string]string) string {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(root, "bus", "pci", "devices", addr)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, content := range attrs {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func resetCapSMI(t *testing.T, sysfs string) *SMI {
	t.Helper()
	s, _ := newTestSMI(nil)
	s.SysfsRoot = sysfs
	if err := s.Init(); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestResetCapabilityOnHardwareThatSupportsIt(t *testing.T) {
	// smiInventory places GPU 0 at 00000000:3B:00.0, which sysfs names 0000:3b:00.0.
	sysfs := fakeSysfs(t, "0000:3b:00.0", map[string]string{
		"reset":        "",
		"reset_method": "flr bus\n",
	})
	got, err := resetCapSMI(t, sysfs).ResetCapability(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Supported {
		t.Fatalf("capability = %+v, want supported", got)
	}
	if len(got.Methods) != 2 || got.Methods[0] != "flr" {
		t.Fatalf("methods = %v, want the kernel's list", got.Methods)
	}
}

// The measured shape of an AWS g4dn.xlarge: the device is present, but the
// hypervisor exposes no reset for it, and no software change can add one.
func TestResetCapabilityOnAVirtualizedInstance(t *testing.T) {
	sysfs := fakeSysfs(t, "0000:3b:00.0", map[string]string{"vendor": "0x10de\n"})
	got, err := resetCapSMI(t, sysfs).ResetCapability(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if got.Supported {
		t.Fatal("a device with no reset attribute must not report a usable reset")
	}
	// The operator must not be sent hunting for a configuration mistake.
	if !strings.Contains(got.Detail, "virtualized") || !strings.Contains(got.Detail, "0000:3b:00.0") {
		t.Fatalf("detail = %q, want the cause and the device named", got.Detail)
	}
}

func TestResetCapabilityRejectsAnEmptyMethodList(t *testing.T) {
	// Some kernels keep the reset attribute while listing no usable method.
	sysfs := fakeSysfs(t, "0000:3b:00.0", map[string]string{
		"reset":        "",
		"reset_method": "\n",
	})
	got, err := resetCapSMI(t, sysfs).ResetCapability(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if got.Supported {
		t.Fatalf("capability = %+v, want unsupported when no method is listed", got)
	}
}

func TestResetCapabilityFailsClosedWithoutSysfs(t *testing.T) {
	s := resetCapSMI(t, filepath.Join(t.TempDir(), "absent"))
	if _, err := s.ResetCapability(context.Background(), 0); err == nil {
		// An unreadable sysfs proves nothing; reporting "no reset" would
		// permanently disable resets on a machine that can perform them.
		t.Fatal("want an error, not a silent negative")
	}
}

func TestResetCapabilityUnknownGPUIndex(t *testing.T) {
	s := resetCapSMI(t, fakeSysfs(t, "0000:3b:00.0", map[string]string{"reset": ""}))
	if _, err := s.ResetCapability(context.Background(), 7); err == nil {
		t.Fatal("want an error for a GPU index that does not exist")
	}
}
