// SPDX-License-Identifier: Apache-2.0

package gcp

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/kubeneuron/kubeneuron/pkg/cloud"
)

// A fixed-size pool loses the named instance, then is set back to its size;
// nothing else is deleted.
func TestReplaceDeletesTheInstanceAndRestoresTheSize(t *testing.T) {
	f := newFakeGKE(3)
	p := testProvider(f)
	if err := p.Replace(t.Context(), testInstance(1)); err != nil {
		t.Fatal(err)
	}
	want := []string{"delete " + testZone + " " + testName(testZone, 1), "setSize gpu 3"}
	if !slices.Equal(f.calls, want) || f.groups[0].target != 3 || len(f.groups[0].members) != 3 {
		t.Fatalf("calls=%q target=%d members=%v", f.calls, f.groups[0].target, f.groups[0].members)
	}
	// The instance is gone: a retry is a finished replacement.
	f.calls = nil
	if err := p.Replace(t.Context(), testInstance(1)); err != nil || len(f.calls) != 0 {
		t.Fatalf("retry: %v, calls %q", err, f.calls)
	}
}

func TestReplaceOnAnAutoscaledPoolOnlyDeletes(t *testing.T) {
	f := newFakeGKE(3)
	f.autoscaling = true
	if err := testProvider(f).Replace(t.Context(), testInstance(1)); err != nil {
		t.Fatal(err)
	}
	if len(f.calls) != 1 || !strings.HasPrefix(f.calls[0], "delete ") || f.groups[0].target != 2 {
		t.Fatalf("calls=%q target=%d", f.calls, f.groups[0].target)
	}
}

func TestReplaceOnEvenZonesSetsEveryZoneBack(t *testing.T) {
	f := newFakeGKE(2, testZone, "us-central1-b")
	if err := testProvider(f).Replace(t.Context(), testInstance(0)); err != nil {
		t.Fatal(err)
	}
	if f.calls[1] != "setSize gpu 2" || f.groups[0].target != 2 || f.groups[1].target != 2 {
		t.Fatalf("calls=%q targets=%d,%d", f.calls, f.groups[0].target, f.groups[1].target)
	}
}

func TestReplaceRefusesWithoutAChange(t *testing.T) {
	for name, setup := range map[string]func(*fakeGKE){
		"uneven zones":   func(f *fakeGKE) { f.groups[1].target = 3 },
		"group changing": func(f *fakeGKE) { f.groups[0].stable = false },
		"pool operation": func(f *fakeGKE) { f.addOp("UPGRADE_NODES", "RUNNING", poolLink(testPool)) },
		"cluster autorepair": func(f *fakeGKE) {
			f.addOp(opAutoRepair, "PENDING", strings.TrimSuffix(poolLink(testPool), "/nodePools/gpu"))
		},
		"autopilot": func(f *fakeGKE) { f.autopilot = true },
	} {
		f := newFakeGKE(2, testZone, "us-central1-b")
		setup(f)
		if err := testProvider(f).Replace(t.Context(), testInstance(0)); err == nil || len(f.calls) != 0 {
			t.Fatalf("%s: %v, calls %q", name, err, f.calls)
		}
	}
}

func TestReplaceReportsAPoolLeftShort(t *testing.T) {
	f := newFakeGKE(3)
	f.setSizeErr = errors.New("permission denied")
	err := testProvider(f).Replace(t.Context(), testInstance(1))
	if err == nil || !strings.Contains(err.Error(), "one node short") || errors.Is(err, cloud.ErrReplacementCapacity) {
		t.Fatalf("err = %v", err)
	}
}
