package nvml

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// minorRunner fakes `nvidia-smi -q -i <index>` so an enumeration index resolves
// to the given device minor. The leading whitespace mirrors nvidia-smi's own
// indented "Minor Number" line.
func minorRunner(minor int) runner {
	return func(_ context.Context, _ string, _ ...string) ([]byte, error) {
		return []byte(fmt.Sprintf("        Minor Number                        : %d\n", minor)), nil
	}
}

// fakeProc builds a procfs tree: pid -> comm, and pid -> fd targets.
func fakeProc(t *testing.T, procs map[string]map[string][]string) string {
	t.Helper()
	root := t.TempDir()
	for pid, spec := range procs {
		dir := filepath.Join(root, pid)
		if err := os.MkdirAll(filepath.Join(dir, "fd"), 0o755); err != nil {
			t.Fatal(err)
		}
		for comm, targets := range spec {
			if err := os.WriteFile(filepath.Join(dir, "comm"), []byte(comm+"\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			for i, target := range targets {
				link := filepath.Join(dir, "fd", string(rune('0'+i)))
				if err := os.Symlink(target, link); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	return root
}

func TestDeviceHoldersNamesTheProcessesThatBlockAReset(t *testing.T) {
	// The exact shape measured on a stock GPU Operator node: three NVIDIA
	// components hold /dev/nvidia0 without being compute applications, which is
	// why nvidia-smi --gpu-reset returned exit 19 while the idle check passed.
	root := fakeProc(t, map[string]map[string][]string{
		"11999": {"dcgm-exporter": {"/dev/nvidia0", "/dev/nvidia-uvm", "/dev/nvidiactl"}},
		"19146": {"nv-hostengine": {"/dev/nvidia0"}},
		"11758": {"nvidia-device-p": {"/dev/nvidia0"}},
		"2":     {"kthreadd": {"/dev/null"}},
		"self":  {"ignored": {"/dev/nvidia0"}},
	})
	s := NewSMI("")
	s.ProcRoot = root
	s.run = minorRunner(0) // enumeration index 0 -> device minor 0

	holders, err := s.DeviceHolders(0)
	if err != nil {
		t.Fatal(err)
	}
	byCommand := map[string]int{}
	for _, h := range holders {
		byCommand[h.Command]++
	}
	for _, want := range []string{"dcgm-exporter", "nv-hostengine", "nvidia-device-p"} {
		if byCommand[want] == 0 {
			t.Fatalf("holders = %v, want %s named", holders, want)
		}
	}
	if byCommand["kthreadd"] != 0 {
		t.Fatalf("holders = %v, must not include unrelated processes", holders)
	}
	// "self" is not a numeric PID and must not be counted twice.
	if byCommand["ignored"] != 0 {
		t.Fatalf("holders = %v, non-numeric /proc entries are not processes", holders)
	}
	// Only the target device counts, so dcgm-exporter appears once even though
	// it also holds the shared control nodes.
	if byCommand["dcgm-exporter"] != 1 {
		t.Fatalf("holders = %v, want only the target device counted", holders)
	}

	rendered := FormatHolders(holders)
	if !strings.Contains(rendered, "nv-hostengine(19146)") {
		t.Fatalf("FormatHolders() = %q, want the PID an operator can act on", rendered)
	}
}

// Resetting one GPU must not require stopping work on its siblings. Every CUDA
// process on the machine opens the shared control nodes, so counting those as
// blocking would make a per-device reset impossible on a multi-GPU node.
func TestDeviceHoldersBlockOnlyOnTheTargetDevice(t *testing.T) {
	root := fakeProc(t, map[string]map[string][]string{
		"100": {"trainer-gpu1": {"/dev/nvidia1", "/dev/nvidiactl", "/dev/nvidia-uvm"}},
		"200": {"monitor": {"/dev/nvidiactl"}},
		"300": {"trainer-gpu0": {"/dev/nvidia0", "/dev/nvidiactl"}},
	})
	s := NewSMI("")
	s.ProcRoot = root
	s.run = minorRunner(0) // enumeration index 0 -> device minor 0

	holders, err := s.DeviceHolders(0)
	if err != nil {
		t.Fatal(err)
	}
	if len(holders) != 1 || holders[0].Command != "trainer-gpu0" {
		t.Fatalf("holders = %v, want only the process holding /dev/nvidia0", holders)
	}
	if holders[0].Device != "/dev/nvidia0" {
		t.Fatalf("device = %q, want the target device", holders[0].Device)
	}

	all, err := s.DeviceHolders(-1)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 {
		t.Fatalf("DeviceHolders(-1) = %v, want one holder per numbered device", all)
	}
}

func TestDeviceHoldersFailsClosedOnUnreadableProcfs(t *testing.T) {
	s := NewSMI("")
	s.ProcRoot = filepath.Join(t.TempDir(), "absent")
	s.run = minorRunner(0)
	if _, err := s.DeviceHolders(0); err == nil {
		t.Fatal("an unreadable process table must be an error, not an empty holder list")
	}
}

func TestDeviceHoldersFailsClosedOnAnUninspectableProcess(t *testing.T) {
	root := t.TempDir()
	pid := filepath.Join(root, "4321")
	if err := os.MkdirAll(pid, 0o755); err != nil {
		t.Fatal(err)
	}
	// A non-directory fd entry models a procfs read failure without relying on
	// permission bits, which root test processes can bypass.
	if err := os.WriteFile(filepath.Join(pid, "fd"), []byte("uninspectable"), 0o600); err != nil {
		t.Fatal(err)
	}
	s := NewSMI("")
	s.ProcRoot = root
	s.run = minorRunner(0)
	if _, err := s.DeviceHolders(0); err == nil {
		t.Fatal("an uninspectable process must block a physical reset")
	}
}

// TestDeviceHoldersResolvesMinorNotEnumerationIndex is the regression for the
// device-node/enumeration-index divergence. After an off-the-bus renumber,
// /dev/nvidia<index> can name a different device than enumeration index
// <index>; the holder scan must follow the driver's minor number. Here
// enumeration index 0 lives on minor 3.
func TestDeviceHoldersResolvesMinorNotEnumerationIndex(t *testing.T) {
	root := fakeProc(t, map[string]map[string][]string{
		"700": {"real-holder": {"/dev/nvidia3"}}, // the device actually being reset
		"800": {"decoy": {"/dev/nvidia0"}},       // the naive node == index path
	})
	s := NewSMI("")
	s.ProcRoot = root
	s.run = minorRunner(3) // enumeration index 0 -> device minor 3

	holders, err := s.DeviceHolders(0)
	if err != nil {
		t.Fatal(err)
	}
	if len(holders) != 1 || holders[0].Command != "real-holder" || holders[0].Device != "/dev/nvidia3" {
		t.Fatalf("holders = %v, want only the /dev/nvidia3 holder for enumeration index 0", holders)
	}
}

// TestDeviceHoldersFailsClosedWhenMinorUnresolvable proves an unresolvable
// device minor blocks the reset rather than silently falling back to node ==
// index.
func TestDeviceHoldersFailsClosedWhenMinorUnresolvable(t *testing.T) {
	s := NewSMI("")
	s.ProcRoot = t.TempDir()
	s.run = func(_ context.Context, _ string, _ ...string) ([]byte, error) {
		return []byte("Attached GPUs : 1\n"), nil // no Minor Number field
	}
	if _, err := s.DeviceHolders(0); err == nil {
		t.Fatal("an unresolvable device minor must block a reset, not default to the enumeration index")
	}
}

// TestDeviceHoldersByUUIDResolvesMinorByUUID proves the UUID-addressed holder
// scan resolves the device minor from the driver by UUID (nvidia-smi -q -i
// <uuid>), so the scan follows the same physical device as a UUID-bound reset
// rather than a renumbered index. Here the UUID resolves to minor 3.
func TestDeviceHoldersByUUIDResolvesMinorByUUID(t *testing.T) {
	root := fakeProc(t, map[string]map[string][]string{
		"700": {"real-holder": {"/dev/nvidia3"}},
		"800": {"decoy": {"/dev/nvidia0"}},
	})
	var selector string
	s := NewSMI("")
	s.ProcRoot = root
	s.run = func(_ context.Context, _ string, args ...string) ([]byte, error) {
		for i, a := range args {
			if a == "-i" && i+1 < len(args) {
				selector = args[i+1]
			}
		}
		return []byte("        Minor Number                        : 3\n"), nil
	}

	holders, err := s.DeviceHoldersByUUID("GPU-aaaa-1111")
	if err != nil {
		t.Fatal(err)
	}
	if selector != "GPU-aaaa-1111" {
		t.Fatalf("minor resolution -i selector = %q, want the UUID", selector)
	}
	if len(holders) != 1 || holders[0].Command != "real-holder" || holders[0].Device != "/dev/nvidia3" {
		t.Fatalf("holders = %v, want only the /dev/nvidia3 holder resolved via the UUID", holders)
	}
}

func TestDeviceHoldersSkipsNonDeviceNVIDIAPaths(t *testing.T) {
	root := fakeProc(t, map[string]map[string][]string{
		"300": {"capwatcher": {"/dev/nvidia-caps/nvidia-cap1"}},
	})
	s := NewSMI("")
	s.ProcRoot = root
	holders, err := s.DeviceHolders(-1)
	if err != nil {
		t.Fatal(err)
	}
	if len(holders) != 0 {
		t.Fatalf("holders = %v, /dev/nvidia-caps entries are not GPU device nodes", holders)
	}
}
