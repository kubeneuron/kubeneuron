// SPDX-License-Identifier: Apache-2.0

// Package neuronsysfs reads the health of AWS Neuron devices from the files
// the Neuron kernel driver exposes under /sys/devices/virtual/neuron_device,
// and turns them into findings: an uncorrectable ECC error, a failing device
// component. AWS Neuron devices are the accelerators (AWS Trainium and AWS
// Inferentia) that the aws.amazon.com/neuron resource counts. KubeNeuron is
// not affiliated with AWS and is unrelated to the AWS Neuron SDK.
//
// EXPERIMENTAL, NOT VALIDATED AGAINST A LIVE AWS TRAINIUM OR INFERENTIA NODE.
// The file names and their meanings are taken from the AWS Neuron sysfs user
// guide and the Neuron driver release notes (reviewed 2026-11-18):
// https://awsdocs-neuron.readthedocs-hosted.com/en/v2.25.0/tools/neuron-sys-tools/neuron-sysfs-user-guide.html
// https://awsdocs-neuron.readthedocs-hosted.com/en/v2.32.0/release-notes/components/runtime.html
// Every test tree here is synthetic.
//
// It only reads. The driver's counters are cleared by writing to them, and
// nothing here ever writes, so another tool reading the same counters sees
// what it would have seen without KubeNeuron. It reads no serial number and
// no other identifier of the hardware: a device is its index, neuron<N>.
package neuronsysfs

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// DefaultRoot is where the Neuron driver puts its devices. sysfs is not
// namespaced per device, so an unprivileged container sees it under its own
// read-only /sys.
const DefaultRoot = "/sys/devices/virtual/neuron_device"

const (
	// maxDevices bounds one read: the largest Neuron instances have 16 or 32
	// devices, so more is a tree this package does not understand.
	maxDevices = 64
	// maxFileBytes bounds every file read: a value is a number or a short
	// list of component names.
	maxFileBytes = 4 << 10
)

// Counter is one of the driver's error counters, by its file under
// stats/hardware.
type Counter string

const (
	// MemECCUncorrected counts HBM errors ECC could not correct and the
	// device cannot repair.
	MemECCUncorrected Counter = "mem_ecc_uncorrected"
	// MemECCRepairableUncorrected counts HBM errors ECC could not correct
	// but the device can repair.
	MemECCRepairableUncorrected Counter = "mem_ecc_repairable_uncorrected"
	// SRAMECCUncorrected counts SRAM errors ECC could not correct.
	SRAMECCUncorrected Counter = "sram_ecc_uncorrected"
	// MemECCCorrected and SRAMECCCorrected count corrected errors (driver
	// 2.30.2 and later, by the release notes); they are read for metrics,
	// never for findings.
	MemECCCorrected  Counter = "mem_ecc_corrected"
	SRAMECCCorrected Counter = "sram_ecc_corrected"
)

// Counters is every counter read, in a fixed order.
var Counters = []Counter{MemECCUncorrected, MemECCRepairableUncorrected, SRAMECCUncorrected, MemECCCorrected, SRAMECCCorrected}

// components is the closed list of the names unhealthy_components can carry,
// by the release notes of driver 2.30.2. Anything else is UnknownComponent:
// text read from the node never leaves this package.
var components = map[string]bool{
	"hbm": true, "sram": true, "firmware_service_api": true, "firmware_service_processor": true,
	"link_intraserver": true, "link_host": true, "link_efa": true, "link_interserver": true,
	"link_switch": true, "axi_fabric": true,
}

// UnknownComponent stands for a component name not in the closed list.
const UnknownComponent = "unknown-component"

// Device is what one read found for one device. A counter or the component
// list is absent when its file is: an older driver does not have it, and
// that is no signal rather than a healthy one.
type Device struct {
	Index    int
	Counters map[Counter]uint64
	// Components is the device's failing components, each a name from the
	// closed list or UnknownComponent, sorted; HasComponents is false when
	// the driver has no health_status.
	Components    []string
	HasComponents bool
}

// Reason is why the source could not be read, from a closed list.
type Reason string

const (
	ReasonPermissionDenied Reason = "permission-denied"
	ReasonReadError        Reason = "read-error"
	ReasonTooManyDevices   Reason = "too-many-devices"
	ReasonUnexpectedValue  Reason = "unexpected-value"
)

// ReadError is a read that failed, with its reason.
type ReadError struct {
	Reason Reason
	Err    error
}

func (e *ReadError) Error() string { return fmt.Sprintf("neuron sysfs: %s: %v", e.Reason, e.Err) }
func (e *ReadError) Unwrap() error { return e.Err }

// Read reads every device under root. present is false when root does not
// exist: a node with no Neuron driver has no such source, which is neither a
// fault nor an unavailable source. A *ReadError says why a present tree could
// not be read.
func Read(root string) (devices []Device, present bool, err error) {
	entries, err := os.ReadDir(root)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, true, readError(err)
	}
	for _, e := range entries {
		index, ok := deviceIndex(e.Name())
		if !ok {
			continue
		}
		if len(devices) == maxDevices {
			return nil, true, &ReadError{Reason: ReasonTooManyDevices, Err: fmt.Errorf("more than %d devices", maxDevices)}
		}
		d, err := readDevice(filepath.Join(root, e.Name()), index)
		if err != nil {
			return nil, true, err
		}
		devices = append(devices, d)
	}
	sort.Slice(devices, func(i, j int) bool { return devices[i].Index < devices[j].Index })
	return devices, true, nil
}

// deviceIndex is N of a directory named neuron<N>.
func deviceIndex(name string) (int, bool) {
	digits, ok := strings.CutPrefix(name, "neuron")
	if !ok || digits == "" || len(digits) > 3 {
		return 0, false
	}
	for _, r := range digits {
		if r < '0' || r > '9' {
			return 0, false
		}
	}
	n, err := strconv.Atoi(digits)
	return n, err == nil
}

func readDevice(dir string, index int) (Device, error) {
	d := Device{Index: index, Counters: map[Counter]uint64{}}
	hardware := filepath.Join(dir, "stats", "hardware")
	for _, c := range Counters {
		v, ok, err := readCounter(filepath.Join(hardware, string(c)))
		if err != nil {
			return Device{}, err
		}
		if ok {
			d.Counters[c] = v
		}
	}
	list, ok, err := readFile(filepath.Join(hardware, "health_status", "unhealthy_components"))
	if err != nil {
		return Device{}, err
	}
	if ok {
		d.HasComponents = true
		d.Components = parseComponents(list)
	}
	return d, nil
}

// readCounter reads a counter that is either a file holding a number or, as
// the guide describes its counters, a directory whose total file holds it.
func readCounter(path string) (uint64, bool, error) {
	info, err := os.Stat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, readError(err)
	}
	if info.IsDir() {
		path = filepath.Join(path, "total")
	}
	text, ok, err := readFile(path)
	if err != nil || !ok {
		return 0, false, err
	}
	v, err := strconv.ParseUint(strings.TrimSpace(text), 10, 64)
	if err != nil {
		// The value itself is not repeated: it is text from the node.
		return 0, false, &ReadError{Reason: ReasonUnexpectedValue, Err: fmt.Errorf("%s is not a counter", filepath.Base(path))}
	}
	return v, true, nil
}

// readFile reads at most maxFileBytes of a file; ok is false when it does
// not exist.
func readFile(path string) (string, bool, error) {
	f, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return "", false, nil
	}
	if err != nil {
		return "", false, readError(err)
	}
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(io.LimitReader(f, maxFileBytes+1))
	if err != nil {
		return "", false, readError(err)
	}
	if len(data) > maxFileBytes {
		return "", false, &ReadError{Reason: ReasonUnexpectedValue, Err: fmt.Errorf("%s is longer than %d bytes", filepath.Base(path), maxFileBytes)}
	}
	return string(data), true, nil
}

// parseComponents maps a comma-separated list to names from the closed list,
// each once.
func parseComponents(list string) []string {
	seen := map[string]bool{}
	var out []string
	for _, part := range strings.Split(list, ",") {
		name := strings.TrimSpace(part)
		if name == "" {
			continue
		}
		if !components[name] {
			name = UnknownComponent
		}
		if !seen[name] {
			seen[name] = true
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

func readError(err error) error {
	if errors.Is(err, fs.ErrPermission) {
		return &ReadError{Reason: ReasonPermissionDenied, Err: err}
	}
	return &ReadError{Reason: ReasonReadError, Err: err}
}
