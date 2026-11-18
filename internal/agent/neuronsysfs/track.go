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
	ClassHBMECCRepairable   Class = "hbm-ecc-repairable"
	ClassUnhealthyComponent Class = "unhealthy-component"
)

// Kind is what a finding asks of the controller.
type Kind string

const (
	// KindFault is a fault of the device.
	KindFault Kind = "fault"
	// KindObservation is recorded and acted on by nothing: a repairable
	// error, or a counter that was not zero when there was no baseline to
	// tell when it rose.
	KindObservation Kind = "observation"
	// KindCleared is a component fault the device no longer reports.
	KindCleared Kind = "cleared"
)

// Finding is one thing a read found on one device.
type Finding struct {
	Device int
	Class  Class
	Kind   Kind
	// Component is the failing component of an unhealthy-component finding:
	// a name from the closed list or UnknownComponent.
	Component string
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

// componentReads is how many reads in a row must report a component before
// it is a fault: a state, unlike a counter, can be read in passing.
const componentReads = 2

// Tracker turns reads into findings. Its counter baselines outlive the
// agent: they are kept in a file in the agent's state directory, bound to
// the boot they were taken in, so an error that happened while the agent was
// down is still a rise when it comes back.
type Tracker struct {
	path   string
	bootID string
	// baseline is the last value per device and counter; known is false
	// until a baseline exists, from the file or from a first read.
	baseline map[string]uint64
	known    bool
	// componentSeen counts the reads in a row that reported a component;
	// faulted marks a component reported as a fault and not yet cleared.
	componentSeen map[string]int
	faulted       map[string]bool
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
	t := &Tracker{path: path, bootID: bootID, baseline: map[string]uint64{},
		componentSeen: map[string]int{}, faulted: map[string]bool{}}
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
	}
	t.known = true
	return t, nil
}

func counterKey(device int, c Counter) string { return strconv.Itoa(device) + "/" + string(c) }

// Observe takes one read and returns its findings, ordered by device, then
// class. It saves the new baselines before it returns them: a finding the
// caller loses is not found again, an error saving them is returned beside
// the findings.
func (t *Tracker) Observe(devices []Device) ([]Finding, error) {
	var out []Finding
	changed := false
	for _, d := range devices {
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
			if !had || v != base {
				t.baseline[key] = v
				changed = true
			}
		}
		out = append(out, t.components(d)...)
	}
	if !t.known {
		t.known = true
		changed = true
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Device != out[j].Device {
			return out[i].Device < out[j].Device
		}
		return out[i].Class < out[j].Class
	})
	if !changed {
		return out, nil
	}
	return out, t.save()
}

// components is the component findings of one device: a fault after
// componentReads reads in a row, cleared by a read of the same device that
// no longer reports it. A device without health_status says nothing.
func (t *Tracker) components(d Device) []Finding {
	if !d.HasComponents {
		return nil
	}
	var out []Finding
	reported := map[string]bool{}
	for _, name := range d.Components {
		reported[name] = true
		key := strconv.Itoa(d.Index) + "/" + name
		t.componentSeen[key]++
		if t.componentSeen[key] >= componentReads && !t.faulted[key] {
			t.faulted[key] = true
			out = append(out, Finding{Device: d.Index, Class: ClassUnhealthyComponent, Kind: KindFault, Component: name})
		}
	}
	prefix := strconv.Itoa(d.Index) + "/"
	keys := make([]string, 0, len(t.componentSeen))
	for key := range t.componentSeen {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		name, ok := strings.CutPrefix(key, prefix)
		if !ok || reported[name] {
			continue
		}
		delete(t.componentSeen, key)
		if t.faulted[key] {
			delete(t.faulted, key)
			out = append(out, Finding{Device: d.Index, Class: ClassUnhealthyComponent, Kind: KindCleared, Component: name})
		}
	}
	return out
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
