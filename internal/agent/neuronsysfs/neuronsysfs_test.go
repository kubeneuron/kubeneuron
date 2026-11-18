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
	for name, setup := range map[string]func(root string){
		"not a number": func(root string) { writeFile(t, hw(root, 0, "mem_ecc_uncorrected"), "lots of text from the node") },
		"too long": func(root string) {
			writeFile(t, hw(root, 0, "health_status/unhealthy_components"), strings.Repeat("hbm,", 2000))
		},
		"too many devices": func(root string) {
			for i := 0; i <= maxDevices; i++ {
				if err := os.MkdirAll(filepath.Join(root, "neuron"+strconv.Itoa(i)), 0o755); err != nil {
					t.Fatal(err)
				}
			}
		},
	} {
		root := t.TempDir()
		setup(root)
		_, present, err := Read(root)
		var re *ReadError
		if !present || !errors.As(err, &re) {
			t.Fatalf("%s: present %v err %v", name, present, err)
		}
		// The reason is from the closed list; the node's text is not in it.
		if strings.Contains(err.Error(), "lots of text") {
			t.Fatalf("%s: the error repeats the node's text: %v", name, err)
		}
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
	_, _, err := Read(root)
	var re *ReadError
	if !errors.As(err, &re) || re.Reason != ReasonPermissionDenied {
		t.Fatalf("err %v", err)
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

func observe(t *testing.T, tr *Tracker, devices ...Device) []Finding {
	t.Helper()
	findings, err := tr.Observe(devices)
	if err != nil {
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

// A component is a fault after two reads in a row, once, and cleared by a
// read of the same device without it; a driver without health_status says
// nothing about it.
func TestTrackerComponents(t *testing.T) {
	tr, _ := NewTracker("", "")
	if got := observe(t, tr, device(0, nil, "hbm")); len(got) != 0 {
		t.Fatalf("one read %+v", got)
	}
	got := observe(t, tr, device(0, nil, "hbm"))
	if !reflect.DeepEqual(got, []Finding{{Device: 0, Class: ClassUnhealthyComponent, Kind: KindFault, Component: "hbm"}}) {
		t.Fatalf("two reads %+v", got)
	}
	if got := observe(t, tr, device(0, nil, "hbm")); len(got) != 0 {
		t.Fatalf("a third read %+v", got)
	}
	// No health_status in this read: nothing changes.
	if got := observe(t, tr, device(0, nil)); len(got) != 0 {
		t.Fatalf("no health_status %+v", got)
	}
	// Another device's read clears nothing here.
	if got := observe(t, tr, device(1, nil, "")); len(got) != 0 {
		t.Fatalf("another device %+v", got)
	}
	got = observe(t, tr, device(0, nil, ""))
	if !reflect.DeepEqual(got, []Finding{{Device: 0, Class: ClassUnhealthyComponent, Kind: KindCleared, Component: "hbm"}}) {
		t.Fatalf("cleared %+v", got)
	}
	// A component seen once and gone again is nothing.
	observe(t, tr, device(0, nil, "sram"))
	if got := observe(t, tr, device(0, nil, "")); len(got) != 0 {
		t.Fatalf("seen once %+v", got)
	}
}
