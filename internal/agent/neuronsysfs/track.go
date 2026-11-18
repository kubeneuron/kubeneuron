// SPDX-License-Identifier: Apache-2.0

package neuronsysfs

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// Class is what a finding is about, from a closed list.
type Class string

const (
	ClassHBMECCUncorrectable  Class = "hbm-ecc-uncorrectable"
	ClassSRAMECCUncorrectable Class = "sram-ecc-uncorrectable"
	// ClassHBMECCRepairable is an observation, not a diagnosis: the device
	// can repair these.
	ClassHBMECCRepairable Class = "hbm-ecc-repairable"
	// ClassDeviceAbsent is a device that was in the tree and is no longer.
	ClassDeviceAbsent Class = "device-absent"
)

// Kind is what a finding asks of the controller.
type Kind string

const (
	// KindFault is a fault of the device.
	KindFault Kind = "fault"
	// KindObservation is recorded and acted on by nothing: a repairable
	// error, a counter that was not zero when there was no baseline to tell
	// when it rose, a device gone from the tree.
	KindObservation Kind = "observation"
)

// Finding is one change a read found on one device.
type Finding struct {
	Device int
	Class  Class
	Kind   Kind
	// Increase is how much an error counter rose; PresentAtStart marks a
	// counter that was not zero when there was no baseline.
	Increase       uint64
	PresentAtStart bool
}

// faultCounters are the counters whose rise is a finding, and its class and
// kind. The corrected counters are not among them.
var faultCounters = []struct {
	counter Counter
	class   Class
	kind    Kind
}{
	{MemECCUncorrected, ClassHBMECCUncorrectable, KindFault},
	{SRAMECCUncorrected, ClassSRAMECCUncorrectable, KindFault},
	{MemECCRepairableUncorrected, ClassHBMECCRepairable, KindObservation},
}

// Tracker turns the counters of each read into findings. Its baselines
// outlive the agent: they are kept in a file in the agent's state directory,
// bound to the boot they were taken in, so an error that happened while the
// agent was down is still a rise when it comes back.
//
// The component list is a state, not a counter: the caller reports each
// device's components as read, and whoever keeps the device's health turns
// that into a fault or its clearing.
type Tracker struct {
	path   string
	bootID string
	// baseline is the committed last value per device and counter; known is
	// false until a baseline was committed, or loaded from the file.
	baseline map[string]uint64
	known    bool
	// pending is what Commit makes the baseline.
	pending map[string]uint64
	// present is the devices of the last read, from the baselines after a
	// restart, so one that is gone is said once.
	present map[int]bool
}

const stateVersion = 1

type stateFile struct {
	Version  int               `json:"version"`
	BootID   string            `json:"boot_id"`
	Baseline map[string]uint64 `json:"baseline"`
}

// NewTracker loads the baselines kept at path for this boot. A missing file,
// another boot's or one this version does not read starts with no baseline;
// an unreadable one is returned as an error beside a usable tracker, so the
// caller can say so and carry on.
func NewTracker(path, bootID string) (*Tracker, error) {
	t := &Tracker{path: path, bootID: bootID, baseline: map[string]uint64{}, present: map[int]bool{}}
	if path == "" || bootID == "" {
		return t, nil
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return t, nil
	}
	if err != nil {
		return t, fmt.Errorf("read neuron sysfs baselines: %w", err)
	}
	var sf stateFile
	if err := json.Unmarshal(data, &sf); err != nil {
		return t, fmt.Errorf("decode neuron sysfs baselines: %w", err)
	}
	// Another boot's counters were reset with the driver; this version
	// cannot read another format. Either way: no baseline.
	if sf.Version != stateVersion || sf.BootID != bootID {
		return t, nil
	}
	for k, v := range sf.Baseline {
		t.baseline[k] = v
		if device, _, ok := strings.Cut(k, "/"); ok {
			if n, err := strconv.Atoi(device); err == nil {
				t.present[n] = true
			}
		}
	}
	t.known = true
	return t, nil
}

func counterKey(device int, c Counter) string { return strconv.Itoa(device) + "/" + string(c) }

// Observe takes one read and returns its findings, ordered by device, then
// class. It writes nothing: the caller calls Commit once the findings are
// delivered, so a crash in between finds them again rather than losing them.
// A device that could not be read keeps its baselines.
func (t *Tracker) Observe(devices []Device) []Finding {
	var out []Finding
	t.pending = make(map[string]uint64, len(t.baseline))
	for k, v := range t.baseline {
		t.pending[k] = v
	}
	seen := map[int]bool{}
	for _, d := range devices {
		seen[d.Index] = true
		if d.Err != "" {
			continue
		}
		for _, fc := range faultCounters {
			v, ok := d.Counters[fc.counter]
			if !ok {
				continue
			}
			key := counterKey(d.Index, fc.counter)
			base, had := t.baseline[key]
			switch {
			case !t.known || !had:
				// No baseline to tell when it rose: recorded, never a fault.
				if v > 0 {
					out = append(out, Finding{Device: d.Index, Class: fc.class, Kind: KindObservation, PresentAtStart: true})
				}
			case v > base:
				out = append(out, Finding{Device: d.Index, Class: fc.class, Kind: fc.kind, Increase: v - base})
			}
			// A value below the baseline is a reset or a cleared counter: it
			// is the new baseline, with no finding.
			t.pending[key] = v
		}
	}
	for device := range t.present {
		if !seen[device] {
			out = append(out, Finding{Device: device, Class: ClassDeviceAbsent, Kind: KindObservation})
		}
	}
	t.present = seen
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Device != out[j].Device {
			return out[i].Device < out[j].Device
		}
		return out[i].Class < out[j].Class
	})
	return out
}

// Commit makes the last Observe's values the baselines and saves them.
func (t *Tracker) Commit() error {
	if t.pending == nil {
		return nil
	}
	t.baseline, t.pending, t.known = t.pending, nil, true
	return t.save()
}

// save writes the baselines with a temporary file, fsync and rename, so a
// crash leaves the old file or the new one.
func (t *Tracker) save() (retErr error) {
	if t.path == "" || t.bootID == "" {
		return nil
	}
	data, err := json.Marshal(stateFile{Version: stateVersion, BootID: t.bootID, Baseline: t.baseline})
	if err != nil {
		return err
	}
	dir := filepath.Dir(t.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create neuron sysfs state directory: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".neuron-sysfs-*")
	if err != nil {
		return fmt.Errorf("create neuron sysfs baselines: %w", err)
	}
	tmpPath := tmp.Name()
	defer func() {
		if err := os.Remove(tmpPath); err != nil && !os.IsNotExist(err) && retErr == nil {
			retErr = err
		}
	}()
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write neuron sysfs baselines: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("sync neuron sysfs baselines: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, t.path); err != nil {
		return fmt.Errorf("replace neuron sysfs baselines: %w", err)
	}
	dirFile, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer func() { _ = dirFile.Close() }()
	return dirFile.Sync()
}
