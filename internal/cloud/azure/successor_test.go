// SPDX-License-Identifier: Apache-2.0

package azure

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kubeneuron/kubeneuron/internal/cloud/cloudtest"
	"github.com/kubeneuron/kubeneuron/pkg/cloud"
)

func TestSuccessorConformance(t *testing.T) {
	cloudtest.SuccessorConformance(t, cloudtest.SuccessorCase{Offline: &Provider{},
		Instance: testInstance(1),
		New: func(*testing.T) (cloud.SuccessorProvider, cloudtest.SuccessorCloud) {
			f := newFakeAKS(3)
			return testProvider(f), f
		},
	})
}

// prepare runs PrepareSuccessor until the wanted phase, or fails.
func prepare(t *testing.T, p *Provider, s cloud.Successor, want cloud.SuccessorPhase) cloud.Successor {
	t.Helper()
	for range 3 {
		next, err := p.PrepareSuccessor(t.Context(), s)
		if err != nil {
			t.Fatalf("prepare from %q: %v", s.Phase, err)
		}
		s = next
		if s.UnrequestedPlan() {
			s.RequestedAt = time.Now().UTC()
		}
		if s.Phase == want {
			return s
		}
	}
	t.Fatalf("phase %q, want %q", s.Phase, want)
	return s
}

func newOperation() cloud.Successor {
	return cloud.Successor{Operation: "inc-1-a1", Instance: testInstance(1)}
}

// The pool grows by one with the operation's tag, the one new machine is the
// successor, and the replacement deletes only the old machine and leaves the
// pool at its size with only its own tags.
func TestAddFirstGrowsTheTaggedPoolAndRemovesTheOldMachine(t *testing.T) {
	f := newFakeAKS(3)
	p := testProvider(f)
	s := prepare(t, p, newOperation(), cloud.SuccessorPlanned)
	if len(f.calls) != 0 || s.Pool != testPool || s.State[stateCount] != "3" {
		t.Fatalf("planning wrote %q; plan %+v", f.calls, s)
	}
	s = prepare(t, p, s, cloud.SuccessorRequested)
	if f.count != 4 || f.tags[successorTag] != operationToken("inc-1-a1")+":3" || strings.Contains(f.tags[successorTag], "inc-1") || f.tags["team"] != "ml" {
		t.Fatalf("count=%d tags=%v after the request", f.count, f.tags)
	}
	s = prepare(t, p, s, cloud.SuccessorCreated)
	if want := "azure://" + testMachine(3).ResourceID; s.ProviderID != want {
		t.Fatalf("successor %q, want %q", s.ProviderID, want)
	}
	f.calls = nil
	if err := p.ReplaceWithSuccessor(t.Context(), s); err != nil {
		t.Fatal(err)
	}
	if f.calls[0] != "delete "+testMachine(1).Name || f.count != 3 || slices.Contains(f.machines, testMachine(1)) {
		t.Fatalf("calls=%q count=%d machines=%v", f.calls, f.count, f.machines)
	}
	if _, tagged := f.tags[successorTag]; tagged || f.tags["team"] != "ml" {
		t.Fatalf("tags = %v", f.tags)
	}
	// A retry after the replacement finished does nothing.
	f.calls = nil
	if err := p.ReplaceWithSuccessor(t.Context(), s); err != nil || len(f.calls) != 0 {
		t.Fatalf("retry: %v, calls %q", err, f.calls)
	}
}

func TestAddFirstIsUnavailableOnAnAutoscaledPool(t *testing.T) {
	f := newFakeAKS(3)
	f.autoscaling = true
	p := testProvider(f)
	var refusal *cloud.PoolRefusal
	if err := p.CheckAddFirst(t.Context(), testInstance(1)); !errors.As(err, &refusal) || refusal.Code != cloud.PoolAutoscaled || !errors.Is(err, cloud.ErrAddFirstUnavailable) || !strings.Contains(err.Error(), "autoscaler") {
		t.Fatalf("CheckAddFirst = %v", err)
	}
	if _, err := p.PrepareSuccessor(t.Context(), newOperation()); !errors.Is(err, cloud.ErrAddFirstUnavailable) {
		t.Fatalf("plan = %v", err)
	}
	if err := testProvider(newFakeAKS(3)).CheckAddFirst(t.Context(), testInstance(1)); err != nil {
		t.Fatalf("fixed-size pool: %v", err)
	}
}

// No quota: the request fails as capacity, and the release puts the pool
// back to its size and drops the tag.
func TestAddFirstWithoutCapacityRestoresThePool(t *testing.T) {
	f := newFakeAKS(3)
	f.growErr = armFailure("OperationNotAllowed", "QuotaExceeded")
	p := testProvider(f)
	planned := prepare(t, p, newOperation(), cloud.SuccessorPlanned)
	if _, err := p.PrepareSuccessor(t.Context(), planned); !errors.Is(err, cloud.ErrReplacementCapacity) {
		t.Fatalf("request = %v, want capacity", err)
	}
	if err := p.ReleaseSuccessor(t.Context(), planned); err != nil {
		t.Fatal(err)
	}
	if _, tagged := f.tags[successorTag]; f.count != 3 || tagged || len(f.machines) != 3 {
		t.Fatalf("count=%d tags=%v machines=%d after the release", f.count, f.tags, len(f.machines))
	}
}

func TestAddFirstRefusesAPoolChangedSinceThePlan(t *testing.T) {
	f := newFakeAKS(3)
	p := testProvider(f)
	planned := prepare(t, p, newOperation(), cloud.SuccessorPlanned)
	f.machines = append(f.machines, testMachine(9))
	f.count = 4
	f.calls = nil
	if _, err := p.PrepareSuccessor(t.Context(), planned); !errors.Is(err, cloud.ErrAddFirstUnavailable) || len(f.calls) != 0 {
		t.Fatalf("request = %v, calls %q; want a refusal without a write", err, f.calls)
	}
}

func TestAddFirstWithTwoNewMachinesNeedsAnOperator(t *testing.T) {
	f := newFakeAKS(3)
	p := testProvider(f)
	s := prepare(t, p, prepare(t, p, newOperation(), cloud.SuccessorPlanned), cloud.SuccessorRequested)
	f.machines = append(f.machines, testMachine(9))
	if _, err := p.PrepareSuccessor(t.Context(), s); !errors.Is(err, cloud.ErrNeedsOperator) {
		t.Fatalf("settle = %v, want needs-operator", err)
	}
	f.calls = nil
	if err := p.ReleaseSuccessor(t.Context(), s); !errors.Is(err, cloud.ErrNeedsOperator) || len(f.calls) != 0 {
		t.Fatalf("release = %v, calls %q; want needs-operator and no change", err, f.calls)
	}
}

func TestAddFirstReleaseDeletesOnlyTheSuccessor(t *testing.T) {
	f := newFakeAKS(3)
	p := testProvider(f)
	s := prepare(t, p, prepare(t, p, prepare(t, p, newOperation(), cloud.SuccessorPlanned), cloud.SuccessorRequested), cloud.SuccessorCreated)
	f.calls = nil
	if err := p.ReleaseSuccessor(t.Context(), s); err != nil {
		t.Fatal(err)
	}
	if f.calls[0] != "delete "+testMachine(3).Name || f.count != 3 || !slices.Contains(f.machines, testMachine(1)) {
		t.Fatalf("calls=%q count=%d machines=%v", f.calls, f.count, f.machines)
	}
	if _, tagged := f.tags[successorTag]; tagged {
		t.Fatalf("tags = %v", f.tags)
	}
}

// A delete whose result is unknown is never retried blindly nor followed by
// a release of the successor.
func TestAddFirstUnknownDeleteNeedsAnOperator(t *testing.T) {
	f := newFakeAKS(3)
	p := testProvider(f)
	s := prepare(t, p, prepare(t, p, prepare(t, p, newOperation(), cloud.SuccessorPlanned), cloud.SuccessorRequested), cloud.SuccessorCreated)
	f.deleteErr = errors.New("context deadline exceeded")
	if err := p.ReplaceWithSuccessor(t.Context(), s); !errors.Is(err, cloud.ErrNeedsOperator) {
		t.Fatalf("replace = %v, want needs-operator", err)
	}
}

// One replacement per pool: an add-first operation and a remove-first
// replacement never run on the same pool together.
func TestAddFirstAndRemoveFirstShareThePool(t *testing.T) {
	f := newFakeAKS(3)
	p := testProvider(f)
	planned := prepare(t, p, newOperation(), cloud.SuccessorPlanned)
	f.tags[replacingTag] = testMachine(0).Name + ":3"
	f.calls = nil
	if _, err := p.PrepareSuccessor(t.Context(), planned); err == nil || len(f.calls) != 0 {
		t.Fatalf("request during a remove-first replacement = %v, calls %q", err, f.calls)
	}
	delete(f.tags, replacingTag)
	prepare(t, p, planned, cloud.SuccessorRequested)
	f.calls = nil
	if err := p.Replace(t.Context(), testInstance(0)); err == nil || len(f.calls) != 0 {
		t.Fatalf("remove-first during an add-first operation = %v, calls %q", err, f.calls)
	}
}

// A requested operation whose tag is gone reached AKS: unless the pool is
// exactly as planned, that needs an operator, not a quiet "nothing to do".
func TestAddFirstReleaseWithoutItsTagNeedsAnOperator(t *testing.T) {
	f := newFakeAKS(3)
	p := testProvider(f)
	s := prepare(t, p, prepare(t, p, newOperation(), cloud.SuccessorPlanned), cloud.SuccessorRequested)
	delete(f.tags, successorTag)
	f.calls = nil
	if err := p.ReleaseSuccessor(t.Context(), s); !errors.Is(err, cloud.ErrNeedsOperator) || len(f.calls) != 0 {
		t.Fatalf("release = %v, calls %q; want needs-operator and no change", err, f.calls)
	}
}

// A release that deleted the successor but failed to drop the tag finishes
// on the next call.
func TestAddFirstReleaseFinishesAfterTheTagWriteFailed(t *testing.T) {
	f := newFakeAKS(3)
	p := testProvider(f)
	s := prepare(t, p, prepare(t, p, prepare(t, p, newOperation(), cloud.SuccessorPlanned), cloud.SuccessorRequested), cloud.SuccessorCreated)
	if err := f.DeleteMachines(t.Context(), testPool, []string{testMachine(3).Name}); err != nil {
		t.Fatal(err)
	}
	if err := p.ReleaseSuccessor(t.Context(), s); err != nil {
		t.Fatalf("release after the delete: %v", err)
	}
	if _, tagged := f.tags[successorTag]; tagged || f.count != 3 {
		t.Fatalf("count=%d tags=%v", f.count, f.tags)
	}
	if err := p.ReleaseSuccessor(t.Context(), s); err != nil {
		t.Fatalf("release once closed: %v", err)
	}
}

// A pool that failed without adding a machine is a capacity failure the
// release undoes; one that failed with a new machine needs an operator.
func TestAddFirstFailedPoolWithoutANewMachineIsCapacity(t *testing.T) {
	f := newFakeAKS(3)
	p := testProvider(f)
	s := prepare(t, p, newOperation(), cloud.SuccessorPlanned)
	f.HoldNextRequest()
	s = prepare(t, p, s, cloud.SuccessorRequested)
	f.state = "Failed"
	if _, err := p.PrepareSuccessor(t.Context(), s); !errors.Is(err, cloud.ErrReplacementCapacity) {
		t.Fatalf("settle = %v, want capacity", err)
	}
	if err := p.ReleaseSuccessor(t.Context(), s); err != nil {
		t.Fatal(err)
	}
	if _, tagged := f.tags[successorTag]; tagged || f.count != 3 {
		t.Fatalf("count=%d tags=%v after the release", f.count, f.tags)
	}

	f = newFakeAKS(3)
	p = testProvider(f)
	s = prepare(t, p, prepare(t, p, newOperation(), cloud.SuccessorPlanned), cloud.SuccessorRequested)
	f.state = "Failed"
	if _, err := p.PrepareSuccessor(t.Context(), s); !errors.Is(err, cloud.ErrNeedsOperator) {
		t.Fatalf("settle with a new machine = %v, want needs-operator", err)
	}
}
