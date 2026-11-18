// SPDX-License-Identifier: Apache-2.0

package gcp

import (
	"errors"

	"github.com/kubeneuron/kubeneuron/pkg/cloud"
	"strings"
	"testing"
)

func autoscaled(n int, zones ...string) *fakeGKE {
	f := newFakeGKE(n, zones...)
	f.autoscaling = true
	return f
}

// On an autoscaled pool the named instance is deleted and the size is left
// to the autoscaler.
func TestReplaceOnAnAutoscaledPoolOnlyDeletes(t *testing.T) {
	f := autoscaled(3)
	p := testProvider(f)
	if err := p.Replace(t.Context(), testInstance(1)); err != nil {
		t.Fatal(err)
	}
	if len(f.calls) != 1 || f.calls[0] != "delete "+testZone+" "+testName(testZone, 1) || f.groups[0].target != 2 {
		t.Fatalf("calls=%q target=%d", f.calls, f.groups[0].target)
	}
	// The instance is gone: a retry is a finished replacement.
	f.calls = nil
	if err := p.Replace(t.Context(), testInstance(1)); err != nil || len(f.calls) != 0 {
		t.Fatalf("retry: %v, calls %q", err, f.calls)
	}
}

// A fixed-size pool is never replaced remove-first: no call records that its
// size was lowered, so a lost second call would leave it short for good.
func TestReplaceRefusesAFixedSizePool(t *testing.T) {
	f := newFakeGKE(3)
	if err := testProvider(f).Replace(t.Context(), testInstance(1)); !errors.Is(err, errFixedPoolRemoveFirst) || len(f.calls) != 0 {
		t.Fatalf("Replace = %v, calls %q", err, f.calls)
	}
}

func TestReplaceRefusesWithoutAChange(t *testing.T) {
	for name, setup := range map[string]func(*fakeGKE){
		"pool operation": func(f *fakeGKE) { f.addOp("UPGRADE_NODES", "RUNNING", poolLink(testPool)) },
		"cluster autorepair": func(f *fakeGKE) {
			f.addOp(opAutoRepair, "PENDING", strings.TrimSuffix(poolLink(testPool), "/nodePools/gpu"))
		},
		"autopilot": func(f *fakeGKE) { f.autopilot = true },
	} {
		f := autoscaled(2, testZone, "us-central1-b")
		setup(f)
		if err := testProvider(f).Replace(t.Context(), testInstance(0)); err == nil || len(f.calls) != 0 {
			t.Fatalf("%s: %v, calls %q", name, err, f.calls)
		}
	}
}

// Remove-first is refused before the cordon exactly where Replace refuses it.
func TestCheckRemoveFirst(t *testing.T) {
	if err := testProvider(autoscaled(3)).CheckRemoveFirst(t.Context(), testInstance(1)); err != nil {
		t.Fatalf("autoscaled pool: %v", err)
	}
	for name, f := range map[string]*fakeGKE{
		"fixed":     newFakeGKE(3),
		"autopilot": func() *fakeGKE { f := autoscaled(3); f.autopilot = true; return f }(),
	} {
		err := testProvider(f).CheckRemoveFirst(t.Context(), testInstance(1))
		if !errors.Is(err, cloud.ErrRemoveFirstUnavailable) || len(f.calls) != 0 {
			t.Fatalf("%s: %v, calls %q", name, err, f.calls)
		}
	}
}
