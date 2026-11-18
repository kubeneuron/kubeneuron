// SPDX-License-Identifier: Apache-2.0

package neuronsysfs

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

// Every tree here is synthetic, written from the AWS documentation; none is
// a copy of a real node's sysfs.

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func hw(root string, device int, file string) string {
	return filepath.Join(root, "neuron"+strconv.Itoa(device), "stats", "hardware", file)
}

func TestReadNoDriverIsNoSource(t *testing.T) {
	devices, present, err := Read(filepath.Join(t.TempDir(), "neuron_device"))
	if err != nil || present || devices != nil {
		t.Fatalf("devices %v present %v err %v", devices, present, err)
	}
}

func TestReadCountersAndComponents(t *testing.T) {
	root := t.TempDir()
	writeFile(t, hw(root, 0, "mem_ecc_uncorrected"), "3\n")
	writeFile(t, hw(root, 0, "sram_ecc_uncorrected"), "0\n")
	// A counter the way the guide draws its counters: a directory with total.
	writeFile(t, hw(root, 0, "mem_ecc_repairable_uncorrected/total"), "5\n")
	writeFile(t, hw(root, 0, "mem_ecc_repairable_uncorrected/present"), "1\n")
	writeFile(t, hw(root, 0, "health_status/unhealthy_components"), "hbm, link_efa,hbm,something_new\n")
	// An older driver: counters, no health_status, no corrected counters.
	writeFile(t, hw(root, 1, "mem_ecc_uncorrected"), "0")
	// Not a device.
	writeFile(t, filepath.Join(root, "neuron_core0", "x"), "1")
	writeFile(t, filepath.Join(root, "uevent"), "")

	devices, present, err := Read(root)
	if err != nil || !present {
		t.Fatalf("present %v err %v", present, err)
	}
	if len(devices) != 2 || devices[0].Index != 0 || devices[1].Index != 1 {
		t.Fatalf("devices %+v", devices)
	}
	d0 := devices[0]
	if !reflect.DeepEqual(d0.Counters, map[Counter]uint64{MemECCUncorrected: 3, SRAMECCUncorrected: 0, MemECCRepairableUncorrected: 5}) {
		t.Fatalf("counters %v", d0.Counters)
	}
	// Names from the closed list, each once; anything else is unknown.
	if !d0.HasComponents || !reflect.DeepEqual(d0.Components, []string{"hbm", "link_efa", UnknownComponent}) {
		t.Fatalf("components %v %v", d0.HasComponents, d0.Components)
	}
	if d1 := devices[1]; d1.HasComponents || len(d1.Counters) != 1 {
		t.Fatalf("older driver %+v", d1)
	}
}

func TestReadRefusesWhatItDoesNotUnderstand(t *testing.T) {
	// A device's file it does not understand: that device, with a reason.
	for name, content := range map[string]struct{ file, text string }{
		"not a number": {"mem_ecc_uncorrected", "lots of text from the node"},
		"too long":     {"health_status/unhealthy_components", strings.Repeat("hbm,", 2000)},
	} {
		root := t.TempDir()
		writeFile(t, hw(root, 0, content.file), content.text)
		devices, present, err := Read(root)
		if err != nil || !present || len(devices) != 1 || devices[0].Err != ReasonUnexpectedValue || devices[0].Counters != nil {
			t.Fatalf("%s: devices %+v present %v err %v", name, devices, present, err)
		}
	}
	// A tree it does not understand: the whole read, with a reason.
	root := t.TempDir()
	for i := 0; i <= maxDevices; i++ {
		if err := os.MkdirAll(filepath.Join(root, "neuron"+strconv.Itoa(i)), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	_, present, err := Read(root)
	var re *ReadError
	if !present || !errors.As(err, &re) || re.Reason != ReasonTooManyDevices {
		t.Fatalf("too many devices: present %v err %v", present, err)
	}
}

func TestReadPermissionDenied(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("needs a user that file modes apply to")
	}
	root := t.TempDir()
	writeFile(t, hw(root, 0, "mem_ecc_uncorrected"), "1")
	if err := os.Chmod(hw(root, 0, "mem_ecc_uncorrected"), 0); err != nil {
		t.Fatal(err)
	}
	devices, _, err := Read(root)
	if err != nil || len(devices) != 1 || devices[0].Err != ReasonPermissionDenied {
		t.Fatalf("devices %+v err %v", devices, err)
	}
}

func device(index int, counters map[Counter]uint64, components ...string) Device {
	d := Device{Index: index, Counters: counters}
	if components != nil {
		d.HasComponents = true
		d.Components = components
		if len(components) == 1 && components[0] == "" {
			d.Components = nil
		}
	}
	return d
}

// observe is one read delivered: Observe, then Commit.
func observe(t *testing.T, tr *Tracker, devices ...Device) []Finding {
	t.Helper()
	findings := tr.Observe(devices)
	if err := tr.Commit(); err != nil {
		t.Fatal(err)
	}
	return findings
}

// With no baseline a counter that is not zero is recorded, never a fault;
// a rise after that is a fault at once; repairable errors are observations.
func TestTrackerCounters(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state", "neuron-sysfs.json")
	tr, err := NewTracker(path, "boot-1")
	if err != nil {
		t.Fatal(err)
	}
	got := observe(t, tr, device(0, map[Counter]uint64{MemECCUncorrected: 2, SRAMECCUncorrected: 0, MemECCRepairableUncorrected: 0}))
	if !reflect.DeepEqual(got, []Finding{{Device: 0, Class: ClassHBMECCUncorrectable, Kind: KindObservation, PresentAtStart: true}}) {
		t.Fatalf("first read %+v", got)
	}
	got = observe(t, tr, device(0, map[Counter]uint64{MemECCUncorrected: 2, SRAMECCUncorrected: 1, MemECCRepairableUncorrected: 4, MemECCCorrected: 900}))
	want := []Finding{
		{Device: 0, Class: ClassHBMECCRepairable, Kind: KindObservation, Increase: 4},
		{Device: 0, Class: ClassSRAMECCUncorrectable, Kind: KindFault, Increase: 1},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("rise %+v", got)
	}
	// The same values again: nothing. Below the baseline (a cleared
	// counter): a new baseline and nothing.
	if got := observe(t, tr, device(0, map[Counter]uint64{MemECCUncorrected: 2, SRAMECCUncorrected: 1, MemECCRepairableUncorrected: 4})); len(got) != 0 {
		t.Fatalf("unchanged %+v", got)
	}
	if got := observe(t, tr, device(0, map[Counter]uint64{MemECCUncorrected: 0, SRAMECCUncorrected: 1, MemECCRepairableUncorrected: 4})); len(got) != 0 {
		t.Fatalf("cleared %+v", got)
	}
	if got := observe(t, tr, device(0, map[Counter]uint64{MemECCUncorrected: 1, SRAMECCUncorrected: 1, MemECCRepairableUncorrected: 4})); len(got) != 1 || got[0].Kind != KindFault || got[0].Increase != 1 {
		t.Fatalf("rise after a clear %+v", got)
	}
}

// The baselines outlive the agent in the same boot, and only in it.
func TestTrackerBaselineAcrossRestarts(t *testing.T) {
	path := filepath.Join(t.TempDir(), "neuron-sysfs.json")
	tr, _ := NewTracker(path, "boot-1")
	observe(t, tr, device(0, map[Counter]uint64{MemECCUncorrected: 2}))

	// The agent comes back in the same boot: a rise while it was down is a
	// fault.
	tr, err := NewTracker(path, "boot-1")
	if err != nil {
		t.Fatal(err)
	}
	if got := observe(t, tr, device(0, map[Counter]uint64{MemECCUncorrected: 3})); len(got) != 1 || got[0].Kind != KindFault || got[0].Increase != 1 {
		t.Fatalf("after a restart %+v", got)
	}
	// Another boot: a new baseline, no fault.
	tr, err = NewTracker(path, "boot-2")
	if err != nil {
		t.Fatal(err)
	}
	if got := observe(t, tr, device(0, map[Counter]uint64{MemECCUncorrected: 5})); len(got) != 1 || got[0].Kind != KindObservation || !got[0].PresentAtStart {
		t.Fatalf("after a reboot %+v", got)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("state file %v %v", info, err)
	}
	// An unreadable file is said, and the tracker still works.
	if err := os.WriteFile(path, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	tr, err = NewTracker(path, "boot-2")
	if err == nil || tr == nil {
		t.Fatalf("tracker %v err %v", tr, err)
	}
	if got := observe(t, tr, device(0, map[Counter]uint64{MemECCUncorrected: 5})); len(got) != 1 || !got[0].PresentAtStart {
		t.Fatalf("after a bad file %+v", got)
	}
}

// A finding not committed is found again by the next agent: delivery is at
// least once.
func TestTrackerFindsAgainWhatWasNotCommitted(t *testing.T) {
	path := filepath.Join(t.TempDir(), "neuron-sysfs.json")
	tr, _ := NewTracker(path, "boot-1")
	observe(t, tr, device(0, map[Counter]uint64{MemECCUncorrected: 2}))
	if got := tr.Observe([]Device{device(0, map[Counter]uint64{MemECCUncorrected: 3})}); len(got) != 1 || got[0].Kind != KindFault {
		t.Fatalf("rise %+v", got)
	}
	// The agent stops before Commit.
	tr, err := NewTracker(path, "boot-1")
	if err != nil {
		t.Fatal(err)
	}
	if got := observe(t, tr, device(0, map[Counter]uint64{MemECCUncorrected: 3})); len(got) != 1 || got[0].Kind != KindFault || got[0].Increase != 1 {
		t.Fatalf("after a crash before Commit %+v", got)
	}
}

// A device that cannot be read hides neither the others nor itself, and
// keeps its baselines; one gone from the tree is said once, after a restart
// too.
func TestTrackerDeviceErrorsAndAbsence(t *testing.T) {
	root := t.TempDir()
	writeFile(t, hw(root, 0, "mem_ecc_uncorrected"), "1")
	writeFile(t, hw(root, 1, "mem_ecc_uncorrected"), "not a number")
	devices, present, err := Read(root)
	if err != nil || !present || len(devices) != 2 || devices[0].Err != "" || devices[1].Err != ReasonUnexpectedValue {
		t.Fatalf("devices %+v present %v err %v", devices, present, err)
	}

	path := filepath.Join(t.TempDir(), "neuron-sysfs.json")
	tr, _ := NewTracker(path, "boot-1")
	observe(t, tr, device(0, map[Counter]uint64{MemECCUncorrected: 1}), device(1, map[Counter]uint64{MemECCUncorrected: 4}))
	// Device 1 cannot be read: no finding for it, and its baseline stays.
	if got := observe(t, tr, device(0, map[Counter]uint64{MemECCUncorrected: 2}), Device{Index: 1, Err: ReasonReadError}); len(got) != 1 || got[0].Device != 0 || got[0].Kind != KindFault {
		t.Fatalf("one device failing %+v", got)
	}
	if got := observe(t, tr, device(0, map[Counter]uint64{MemECCUncorrected: 2}), device(1, map[Counter]uint64{MemECCUncorrected: 5})); len(got) != 1 || got[0].Device != 1 || got[0].Increase != 1 {
		t.Fatalf("read again %+v", got)
	}
	want := []Finding{{Device: 1, Class: ClassDeviceAbsent, Kind: KindObservation}}
	if got := observe(t, tr, device(0, map[Counter]uint64{MemECCUncorrected: 2})); !reflect.DeepEqual(got, want) {
		t.Fatalf("gone %+v", got)
	}
	if got := observe(t, tr, device(0, map[Counter]uint64{MemECCUncorrected: 2})); len(got) != 0 {
		t.Fatalf("still gone %+v", got)
	}
	// After a restart the baselines say which devices there were.
	tr, _ = NewTracker(path, "boot-1")
	if got := observe(t, tr, device(0, map[Counter]uint64{MemECCUncorrected: 2})); !reflect.DeepEqual(got, want) {
		t.Fatalf("gone, after a restart %+v", got)
	}
}
