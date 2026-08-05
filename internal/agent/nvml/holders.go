package nvml

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// defaultProcRoot is the procfs the agent scans for device holders. The managed
// DaemonSet runs with hostPID, so this is the host's process table.
const defaultProcRoot = "/proc"

// DeviceHolder is a process with an open handle on an NVIDIA device node.
//
// It is not the same thing as a compute application. nvidia-smi's
// --query-compute-apps only lists processes with a CUDA context, but any open
// handle blocks a reset: on a stock GPU Operator node, nv-hostengine,
// dcgm-exporter and nvidia-device-plugin all hold /dev/nvidia0 without ever
// appearing as compute apps. nvidia-smi --gpu-reset then fails with exit 19 and
// the misleading text "currently in use by another process", naming nothing.
type DeviceHolder struct {
	PID     int
	Command string
	Device  string
}

func (h DeviceHolder) String() string {
	return fmt.Sprintf("%s(%d) holds %s", h.Command, h.PID, h.Device)
}

// FormatHolders renders holders for an operator-facing message.
func FormatHolders(holders []DeviceHolder) string {
	parts := make([]string, 0, len(holders))
	for _, h := range holders {
		parts = append(parts, h.String())
	}
	return strings.Join(parts, ", ")
}

// DeviceHolders lists processes holding the given GPU's device node. Passing a
// negative index scans every numbered /dev/nvidia* device.
//
// The device node is addressed by its driver-assigned MINOR number, not the
// NVML enumeration index. The two are not the same: minor numbers are ordered
// by driver registration, so after the very off-the-bus renumber a reset is
// remediating, /dev/nvidia<index> can point at a different device than
// enumeration index <index>. Resolving the minor from the driver keeps the
// holder preflight pointed at the device actually being reset.
//
// A process or FD that disappears during the scan is skipped, because procfs
// races are normal. Any other read failure is an unknown holder and therefore
// fails the scan: a physical reset must never treat an inaccessible process as
// evidence that the GPU is free.
func (s *SMI) DeviceHolders(index int) ([]DeviceHolder, error) {
	// index < 0 means "scan every numbered device"; there is no single minor to
	// resolve. For a specific GPU, translate the enumeration index to its device
	// minor before building the node path.
	minor := index
	if index >= 0 {
		resolved, err := s.deviceMinor(index)
		if err != nil {
			return nil, err
		}
		minor = resolved
	}
	return s.deviceHoldersForMinor(minor)
}

// DeviceHoldersByUUID lists processes holding the device identified by its stable
// UUID. It resolves the device minor from the driver via `nvidia-smi -q -i <uuid>`,
// so the holder scan follows the same physical device as a UUID-bound reset even
// when an enumeration index has shifted. HARDWARE-DEPENDENT: that nvidia-smi -q
// accepts a UUID for -i needs confirmation on a real GPU.
func (s *SMI) DeviceHoldersByUUID(uuid string) ([]DeviceHolder, error) {
	minor, err := s.deviceMinorByUUID(uuid)
	if err != nil {
		return nil, err
	}
	return s.deviceHoldersForMinor(minor)
}

// deviceHoldersForMinor scans the process table for open handles on the device
// node with the given minor (or every numbered device when minor < 0).
func (s *SMI) deviceHoldersForMinor(minor int) ([]DeviceHolder, error) {
	root := s.ProcRoot
	if strings.TrimSpace(root) == "" {
		root = defaultProcRoot
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, fmt.Errorf("scanning %s for device holders: %w", root, err)
	}
	want := deviceNamesFor(minor)
	var holders []DeviceHolder
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue // not a process directory
		}
		fdDir := filepath.Join(root, entry.Name(), "fd")
		fds, err := os.ReadDir(fdDir)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue // process exited between the root and fd scans
			}
			return nil, fmt.Errorf("scanning %s for device holders: %w", fdDir, err)
		}
		command := processCommand(root, entry.Name())
		seen := map[string]bool{}
		for _, fd := range fds {
			target, err := os.Readlink(filepath.Join(fdDir, fd.Name()))
			if err != nil {
				if errors.Is(err, os.ErrNotExist) {
					continue // FD closed while the directory was being scanned
				}
				return nil, fmt.Errorf("reading %s for device holders: %w", filepath.Join(fdDir, fd.Name()), err)
			}
			if !want(target) || seen[target] {
				continue
			}
			seen[target] = true
			holders = append(holders, DeviceHolder{PID: pid, Command: command, Device: target})
		}
	}
	sort.Slice(holders, func(i, j int) bool {
		if holders[i].PID != holders[j].PID {
			return holders[i].PID < holders[j].PID
		}
		return holders[i].Device < holders[j].Device
	})
	return holders, nil
}

// deviceNamesFor matches the device nodes whose holders block a reset of the
// given GPU: the numbered device itself, and nothing else.
//
// The shared control nodes /dev/nvidiactl and /dev/nvidia-uvm are deliberately
// excluded. Every CUDA process on the machine opens them, so treating them as
// blocking would mean that resetting one GPU on an eight-GPU node requires
// stopping work on the other seven — turning "replace one bad card" into
// "quiesce the whole node". Modern drivers reset a single device as long as
// that device is idle. The known exception is NVLink: devices in one link
// clique reset together, which is a topology question this scan does not
// answer and which the driver enforces on its own.
//
// Being slightly permissive here is the safer error. If the driver still
// refuses, the reset fails and says so; being too strict blocked resets that
// were legitimately possible.
func deviceNamesFor(index int) func(string) bool {
	if index < 0 {
		return func(target string) bool {
			return strings.HasPrefix(target, "/dev/nvidia") && isNumberedDevice(target)
		}
	}
	device := fmt.Sprintf("/dev/nvidia%d", index)
	return func(target string) bool { return target == device }
}

// isNumberedDevice distinguishes /dev/nvidia0 from /dev/nvidia-caps/... and
// other non-device entries under the same prefix.
func isNumberedDevice(target string) bool {
	suffix := strings.TrimPrefix(target, "/dev/nvidia")
	if suffix == "" {
		return false
	}
	_, err := strconv.Atoi(suffix)
	return err == nil
}

// deviceMinor resolves an enumeration index to the device node's minor number
// from the driver, via `nvidia-smi -q -i <index>`'s "Minor Number" field. That
// field is the driver's own index->minor mapping; it is authoritative even when
// the two have diverged after a renumber. Fail closed: an unresolvable minor
// must block a reset, never fall back to assuming node == index.
//
// HARDWARE-DEPENDENT: the "Minor Number" line's presence and exact spelling in
// nvidia-smi -q output needs confirmation on a real GPU.
func (s *SMI) deviceMinor(index int) (int, error) {
	return s.deviceMinorFor(strconv.Itoa(index), fmt.Sprintf("GPU %d", index))
}

// deviceMinorByUUID resolves the device minor from the driver by the device's
// stable UUID (nvidia-smi -q accepts a UUID for -i), so the resolution is immune
// to an enumeration renumber between an incident opening and a reset.
func (s *SMI) deviceMinorByUUID(uuid string) (int, error) {
	return s.deviceMinorFor(uuid, "GPU "+uuid)
}

// deviceMinorFor is deviceMinor parameterized by the -i selector (an index or a
// UUID) and a human label for errors.
func (s *SMI) deviceMinorFor(selector, label string) (int, error) {
	ctx, cancel := context.WithTimeout(context.Background(), smiProbeTimeout)
	defer cancel()
	out, err := s.run(ctx, s.Path, "-q", "-i", selector)
	if err != nil {
		return 0, fmt.Errorf("resolving device minor for %s: %w (output: %s)", label, err, firstLine(out))
	}
	minor, err := parseMinorNumber(out)
	if err != nil {
		return 0, fmt.Errorf("resolving device minor for %s: %w", label, err)
	}
	return minor, nil
}

// parseMinorNumber extracts the "Minor Number : N" value from nvidia-smi -q
// output. [N/A] (a device with no minor, e.g. a bare MIG parent) and a missing
// field are both errors: the holder scan needs a concrete device node.
func parseMinorNumber(out []byte) (int, error) {
	for _, line := range strings.Split(string(out), "\n") {
		key, value, ok := strings.Cut(line, ":")
		if !ok || strings.TrimSpace(key) != "Minor Number" {
			continue
		}
		minor, err := strconv.Atoi(strings.TrimSpace(value))
		if err != nil {
			return 0, fmt.Errorf("unexpected Minor Number %q", strings.TrimSpace(value))
		}
		if minor < 0 {
			return 0, fmt.Errorf("unexpected negative Minor Number %d", minor)
		}
		return minor, nil
	}
	return 0, fmt.Errorf("no Minor Number in nvidia-smi -q output")
}

func processCommand(root, pid string) string {
	data, err := os.ReadFile(filepath.Join(root, pid, "comm"))
	if err != nil {
		return "unknown"
	}
	return strings.TrimSpace(string(data))
}
