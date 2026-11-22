// SPDX-License-Identifier: Apache-2.0

package azure

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/kubeneuron/kubeneuron/pkg/cloud"
)

// A fixed-size pool grows first, the old machine goes once the new one is
// listed, and the pool ends at its size with only its own tags.
func TestReplaceGrowsFixedPoolBeforeDeleting(t *testing.T) {
	f := newFakeAKS(3)
	target := testMachine(1)
	if err := testProvider(f).Replace(t.Context(), testInstance(1)); err != nil {
		t.Fatal(err)
	}
	want := []string{
		`write count=4 tag="` + target.Name + `:3"`,
		"delete " + target.Name,
		`write count=3 tag=""`,
	}
	if !slices.Equal(f.calls, want) {
		t.Fatalf("calls = %q, want %q", f.calls, want)
	}
	if f.count != 3 || len(f.machines) != 3 || slices.Contains(f.machines, target) {
		t.Fatalf("pool count=%d machines=%v", f.count, f.machines)
	}
	if _, tagged := f.tags[replacingTag]; tagged || f.tags["team"] != "ml" {
		t.Fatalf("tags = %v", f.tags)
	}
}

// An autoscaled pool keeps its size: only the machine is deleted.
func TestReplaceOnAutoscaledPoolOnlyDeletes(t *testing.T) {
	f := newFakeAKS(2)
	f.autoscaling = true
	if err := testProvider(f).Replace(t.Context(), testInstance(0)); err != nil {
		t.Fatal(err)
	}
	if want := []string{"delete " + testMachine(0).Name}; !slices.Equal(f.calls, want) {
		t.Fatalf("calls = %q, want %q", f.calls, want)
	}
}

// A retry after the pool was grown deletes without growing it again, and a
// retry after the replacement finished does nothing.
func TestReplaceRetryKeepsOneOperation(t *testing.T) {
	f := newFakeAKS(2)
	p := testProvider(f)
	target := testMachine(0)
	// The first attempt grew the pool and stopped before the delete.
	if err := f.WritePool(t.Context(), testPool, mustPool(t, f), 3, map[string]string{"team": "ml", replacingTag: target.Name + ":2"}); err != nil {
		t.Fatal(err)
	}
	f.calls = nil
	if err := p.Replace(t.Context(), testInstance(0)); err != nil {
		t.Fatal(err)
	}
	if want := []string{"delete " + target.Name, `write count=2 tag=""`}; !slices.Equal(f.calls, want) {
		t.Fatalf("calls = %q, want %q", f.calls, want)
	}
	f.calls = nil
	if err := p.Replace(t.Context(), testInstance(0)); err != nil {
		t.Fatalf("retry of a finished replacement: %v", err)
	}
	if len(f.calls) != 0 {
		t.Fatalf("a finished replacement made calls: %q", f.calls)
	}
}

// No quota or capacity: the failure is the contract's capacity verdict, and
// nothing is deleted, on the first attempt or a retry.
func TestReplaceCapacityFailureDeletesNothing(t *testing.T) {
	for _, scenario := range []string{"scale-refused", "no-new-machine"} {
		t.Run(scenario, func(t *testing.T) {
			f := newFakeAKS(2)
			if scenario == "scale-refused" {
				f.growErr = armFailure("ScaleVMSSAgentPoolFailed", "QuotaExceeded")
			} else {
				f.growsMachine = false
			}
			p := testProvider(f)
			err := p.Replace(t.Context(), testInstance(0))
			if !errors.Is(err, cloud.ErrReplacementCapacity) {
				t.Fatalf("err = %v, want ErrReplacementCapacity", err)
			}
			if err := p.Replace(t.Context(), testInstance(0)); err == nil {
				t.Fatal("a retry of a failed growth succeeded")
			}
			for _, c := range f.calls {
				if strings.HasPrefix(c, "delete") {
					t.Fatalf("deleted with no successor: %q", f.calls)
				}
			}
		})
	}
}

// A failure that is not about capacity is not reported as one.
func TestReplaceOtherFailureIsNotCapacity(t *testing.T) {
	f := newFakeAKS(2)
	f.growErr = armFailure("AuthorizationFailed", "AuthorizationFailed")
	err := testProvider(f).Replace(t.Context(), testInstance(0))
	if err == nil || errors.Is(err, cloud.ErrReplacementCapacity) {
		t.Fatalf("err = %v, want a failure that is not capacity", err)
	}
}

// If the delete does not bring the pool back to its size, nothing shrinks it:
// that would delete a machine AKS picks.
func TestReplaceNeverShrinksAPoolLeftLarger(t *testing.T) {
	f := newFakeAKS(2)
	f.shrinkOnDelete = false
	p := testProvider(f)
	err := p.Replace(t.Context(), testInstance(0))
	if err == nil || !strings.Contains(err.Error(), "needs an operator") {
		t.Fatalf("err = %v, want an operator to be needed", err)
	}
	if err := p.Replace(t.Context(), testInstance(0)); err == nil {
		t.Fatal("a retry hid a pool left larger")
	}
	for _, c := range f.calls {
		if strings.HasPrefix(c, "write count=2") {
			t.Fatalf("shrank the pool: %q", f.calls)
		}
	}
}

// One replacement per pool: another machine's tag means wait.
func TestReplaceWaitsForAnotherReplacementInThePool(t *testing.T) {
	f := newFakeAKS(3)
	f.tags[replacingTag] = testMachine(2).Name + ":3"
	f.count = 4
	err := testProvider(f).Replace(t.Context(), testInstance(0))
	if err == nil || !strings.Contains(err.Error(), "waiting") || len(f.calls) != 0 {
		t.Fatalf("err = %v calls = %q", err, f.calls)
	}
}

func TestReplaceWaitsForABusyPool(t *testing.T) {
	f := newFakeAKS(2)
	f.state = "Scaling"
	err := testProvider(f).Replace(t.Context(), testInstance(0))
	if err == nil || !strings.Contains(err.Error(), "busy") || len(f.calls) != 0 {
		t.Fatalf("err = %v calls = %q", err, f.calls)
	}
}

// Anything that is not a machine of this cluster is refused before any write.
func TestReplaceRefusesAWrongNode(t *testing.T) {
	other := strings.Replace(testInstance(0), testSub, "99999999-2222-3333-4444-555555555555", 1)
	for name, id := range map[string]string{
		"other subscription":   other,
		"other cluster":        strings.Replace(testInstance(0), "mc_ml_training_eastus", "mc_ml_other_eastus", 1),
		"not an AKS scale set": strings.Replace(testInstance(0), testVMSS, "batch-vmss", 1),
		"not listed in pool":   testInstance(7),
		"standalone VM":        "/subscriptions/" + testSub + "/resourceGroups/mc_ml_training_eastus/providers/Microsoft.Compute/virtualMachines/gpu-0",
	} {
		t.Run(name, func(t *testing.T) {
			f := newFakeAKS(2)
			err := testProvider(f).Replace(t.Context(), id)
			if name == "not listed in pool" {
				// A machine absent from its pool is an already finished
				// replacement, never a write.
				err = nil
			}
			if len(f.calls) != 0 {
				t.Fatalf("calls = %q for %s (err %v)", f.calls, name, err)
			}
			if name != "not listed in pool" && err == nil {
				t.Fatalf("Replace(%s) succeeded", id)
			}
		})
	}
}

func mustPool(t *testing.T, f *fakeAKS) pool {
	t.Helper()
	p, err := f.Pool(t.Context(), testPool)
	if err != nil {
		t.Fatal(err)
	}
	return p
}
