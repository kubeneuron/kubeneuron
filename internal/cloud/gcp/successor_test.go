// SPDX-License-Identifier: Apache-2.0

package gcp

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/codes"

	"github.com/kubeneuron/kubeneuron/internal/cloud/cloudtest"
	"github.com/kubeneuron/kubeneuron/pkg/cloud"
)

func TestSuccessorConformance(t *testing.T) {
	cloudtest.SuccessorConformance(t, cloudtest.SuccessorCase{Offline: &Provider{},
		Instance: testInstance(1),
		New: func(*testing.T) (cloud.SuccessorProvider, cloudtest.SuccessorCloud) {
			f := newFakeGKE(3)
			f.now = time.Now().UTC()
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
			s.RequestedAt = fakeStartTime
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

func created(t *testing.T, p *Provider) cloud.Successor {
	t.Helper()
	return prepare(t, p, prepare(t, p, prepare(t, p, newOperation(), cloud.SuccessorPlanned), cloud.SuccessorRequested), cloud.SuccessorCreated)
}

// The pool grows by one through GKE, the one new member is the successor,
// and the replacement deletes only the old instance, which leaves the pool at
// its size. Nothing names the operation in Google Cloud.
func TestAddFirstGrowsThePoolAndRemovesTheOldInstance(t *testing.T) {
	f := newFakeGKE(3)
	p := testProvider(f)
	s := prepare(t, p, newOperation(), cloud.SuccessorPlanned)
	if len(f.calls) != 0 || s.Pool != testPool || s.State[stateSize] != "3" {
		t.Fatalf("planning wrote %q; plan %+v", f.calls, s)
	}
	s = prepare(t, p, s, cloud.SuccessorRequested)
	if !slices.Equal(f.calls, []string{"setSize gpu 4"}) || s.State[stateOperation] == "" {
		t.Fatalf("calls=%q state=%v after the request", f.calls, s.State)
	}
	s = prepare(t, p, s, cloud.SuccessorCreated)
	if want := "gce://" + testProject + "/" + testZone + "/" + testName(testZone, 3); s.ProviderID != want {
		t.Fatalf("successor %q, want %q", s.ProviderID, want)
	}
	f.calls = nil
	if err := p.ReplaceWithSuccessor(t.Context(), s); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(f.calls, []string{"delete " + testZone + " " + testName(testZone, 1)}) || f.groups[0].target != 3 {
		t.Fatalf("calls=%q target=%d", f.calls, f.groups[0].target)
	}
	f.calls = nil
	if err := p.ReplaceWithSuccessor(t.Context(), s); err != nil || len(f.calls) != 0 {
		t.Fatalf("retry: %v, calls %q", err, f.calls)
	}
	for _, op := range f.ops {
		if strings.Contains(op.Target, "inc-1") {
			t.Fatalf("operation %+v names the incident", op)
		}
	}
}

func TestAddFirstIsUnavailableWhereGKEOwnsTheSize(t *testing.T) {
	for name, tc := range map[string]struct {
		f    *fakeGKE
		want string
	}{
		"autoscaled": {func() *fakeGKE { f := newFakeGKE(3); f.autoscaling = true; return f }(), "autoscaler"},
		"multi-zone": {newFakeGKE(3, testZone, "us-central1-b"), "zones"},
		"autopilot":  {func() *fakeGKE { f := newFakeGKE(3); f.autopilot = true; return f }(), "Autopilot"},
	} {
		p := testProvider(tc.f)
		if err := p.CheckAddFirst(t.Context(), testInstance(1)); !errors.Is(err, cloud.ErrAddFirstUnavailable) || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%s: CheckAddFirst = %v", name, err)
		}
		if _, err := p.PrepareSuccessor(t.Context(), newOperation()); !errors.Is(err, cloud.ErrAddFirstUnavailable) {
			t.Fatalf("%s: plan = %v", name, err)
		}
	}
	if err := testProvider(newFakeGKE(3)).CheckAddFirst(t.Context(), testInstance(1)); err != nil {
		t.Fatalf("fixed-size pool: %v", err)
	}
}

func TestAddFirstRefusesAPoolChangedSinceThePlan(t *testing.T) {
	f := newFakeGKE(3)
	p := testProvider(f)
	planned := prepare(t, p, newOperation(), cloud.SuccessorPlanned)
	f.groups[0].target = 2
	f.groups[0].members = f.groups[0].members[1:]
	if _, err := p.PrepareSuccessor(t.Context(), planned); !errors.Is(err, cloud.ErrAddFirstUnavailable) || len(f.calls) != 0 {
		t.Fatalf("request = %v, calls %q; want a refusal without a call", err, f.calls)
	}
}

// Another operation on the pool is waited for, not joined.
func TestAddFirstWaitsForAForeignOperation(t *testing.T) {
	f := newFakeGKE(3)
	p := testProvider(f)
	planned := prepare(t, p, newOperation(), cloud.SuccessorPlanned)
	op := f.addOp("UPGRADE_NODES", "RUNNING", poolLink(testPool))
	if _, err := p.PrepareSuccessor(t.Context(), planned); err == nil || errors.Is(err, cloud.ErrNeedsOperator) || len(f.calls) != 0 {
		t.Fatalf("request = %v, calls %q", err, f.calls)
	}
	op.Status = opDone
	prepare(t, p, planned, cloud.SuccessorRequested)
}

// Two resizes the plan had not seen cannot be told apart.
func TestAddFirstWithTwoUnknownResizesNeedsAnOperator(t *testing.T) {
	f := newFakeGKE(3)
	p := testProvider(f)
	planned := prepare(t, p, newOperation(), cloud.SuccessorPlanned)
	f.addOp(opSetSize, opDone, poolLink(testPool))
	f.addOp(opSetSize, opDone, poolLink(testPool))
	if _, err := p.PrepareSuccessor(t.Context(), planned); !errors.Is(err, cloud.ErrNeedsOperator) || len(f.calls) != 0 {
		t.Fatalf("request = %v, calls %q", err, f.calls)
	}
}

// Resizes from before the plan are not taken for this operation's.
func TestAddFirstIgnoresResizesFromBeforeThePlan(t *testing.T) {
	f := newFakeGKE(3)
	f.addOp(opSetSize, opDone, poolLink(testPool))
	p := testProvider(f)
	s := prepare(t, p, prepare(t, p, newOperation(), cloud.SuccessorPlanned), cloud.SuccessorRequested)
	if f.requests != 1 || s.State[stateOperation] == "operation-1" {
		t.Fatalf("requests=%d state=%v", f.requests, s.State)
	}
}

// No capacity: the new member fails to come up, the settle reports it, and
// the release deletes just that member.
func TestAddFirstWithoutCapacityDeletesOnlyTheNewMember(t *testing.T) {
	f := newFakeGKE(3)
	f.createErrors = []string{"ZONE_RESOURCE_POOL_EXHAUSTED"}
	p := testProvider(f)
	s := prepare(t, p, prepare(t, p, newOperation(), cloud.SuccessorPlanned), cloud.SuccessorRequested)
	if _, err := p.PrepareSuccessor(t.Context(), s); !errors.Is(err, cloud.ErrReplacementCapacity) {
		t.Fatalf("settle = %v, want capacity", err)
	}
	f.calls = nil
	if err := p.ReleaseSuccessor(t.Context(), s); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(f.calls, []string{"delete " + testZone + " " + testName(testZone, 3)}) || f.groups[0].target != 3 {
		t.Fatalf("calls=%q target=%d", f.calls, f.groups[0].target)
	}
	f.calls = nil
	if err := p.ReleaseSuccessor(t.Context(), s); err != nil || len(f.calls) != 0 {
		t.Fatalf("second release: %v, calls %q", err, f.calls)
	}
}

func TestAddFirstFailedResizeIsCapacity(t *testing.T) {
	f := newFakeGKE(3)
	p := testProvider(f)
	s := prepare(t, p, newOperation(), cloud.SuccessorPlanned)
	f.HoldNextRequest()
	s = prepare(t, p, s, cloud.SuccessorRequested)
	f.held = false
	for i := range f.ops {
		f.ops[i].Status, f.ops[i].ErrorCode, f.ops[i].Error = opDone, codes.ResourceExhausted, "quota"
	}
	f.groups[0].target = 3
	if _, err := p.PrepareSuccessor(t.Context(), s); !errors.Is(err, cloud.ErrReplacementCapacity) {
		t.Fatalf("settle = %v, want capacity", err)
	}
	f.calls = nil
	if err := p.ReleaseSuccessor(t.Context(), s); err != nil || len(f.calls) != 0 {
		t.Fatalf("release = %v, calls %q", err, f.calls)
	}
}

func TestAddFirstWithTwoNewMembersNeedsAnOperator(t *testing.T) {
	f := newFakeGKE(3)
	p := testProvider(f)
	s := prepare(t, p, prepare(t, p, newOperation(), cloud.SuccessorPlanned), cloud.SuccessorRequested)
	f.groups[0].target = 5
	f.groups[0].grow(5, nil)
	if _, err := p.PrepareSuccessor(t.Context(), s); !errors.Is(err, cloud.ErrNeedsOperator) {
		t.Fatalf("settle = %v, want needs-operator", err)
	}
	f.calls = nil
	if err := p.ReleaseSuccessor(t.Context(), s); !errors.Is(err, cloud.ErrNeedsOperator) || len(f.calls) != 0 {
		t.Fatalf("release = %v, calls %q; want needs-operator and no change", err, f.calls)
	}
}

func TestAddFirstReleaseDeletesOnlyTheSuccessor(t *testing.T) {
	f := newFakeGKE(3)
	p := testProvider(f)
	s := created(t, p)
	f.calls = nil
	if err := p.ReleaseSuccessor(t.Context(), s); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(f.calls, []string{"delete " + testZone + " " + testName(testZone, 3)}) || f.groups[0].target != 3 {
		t.Fatalf("calls=%q target=%d", f.calls, f.groups[0].target)
	}
}

// A delete whose result is unknown is never retried blindly.
func TestAddFirstUnknownDeleteNeedsAnOperator(t *testing.T) {
	f := newFakeGKE(3)
	p := testProvider(f)
	s := created(t, p)
	f.deleteErr = errors.New("context deadline exceeded")
	if err := p.ReplaceWithSuccessor(t.Context(), s); !errors.Is(err, cloud.ErrNeedsOperator) {
		t.Fatalf("replace = %v, want needs-operator", err)
	}
	if err := p.ReleaseSuccessor(t.Context(), s); !errors.Is(err, cloud.ErrNeedsOperator) {
		t.Fatalf("release = %v, want needs-operator", err)
	}
}

func TestAddFirstReplaceWithoutTheSuccessorNeedsAnOperator(t *testing.T) {
	f := newFakeGKE(3)
	p := testProvider(f)
	s := created(t, p)
	f.groups[0].members = f.groups[0].members[:3]
	f.calls = nil
	if err := p.ReplaceWithSuccessor(t.Context(), s); !errors.Is(err, cloud.ErrNeedsOperator) || len(f.calls) != 0 {
		t.Fatalf("replace = %v, calls %q", err, f.calls)
	}
}

// The pool is never shrunk through GKE, whatever happens.
func TestAddFirstNeverShrinksThroughGKE(t *testing.T) {
	f := newFakeGKE(3)
	p := testProvider(f)
	s := created(t, p)
	_ = p.ReleaseSuccessor(t.Context(), s)
	s = created(t, testProvider(f))
	_ = p.ReplaceWithSuccessor(t.Context(), s)
	for _, c := range f.calls {
		if strings.HasPrefix(c, "setSize") && c != "setSize gpu 4" {
			t.Fatalf("calls %q", f.calls)
		}
	}
}

// A member still being created is waited for.
func TestAddFirstWaitsForTheNewMemberToRun(t *testing.T) {
	f := newFakeGKE(3)
	p := testProvider(f)
	s := prepare(t, p, prepare(t, p, newOperation(), cloud.SuccessorPlanned), cloud.SuccessorRequested)
	f.groups[0].members[3].Action, f.groups[0].members[3].Status = "CREATING", ""
	if s, err := p.PrepareSuccessor(t.Context(), s); err != nil || s.Phase != cloud.SuccessorRequested {
		t.Fatalf("settle = %+v, %v; want to wait", s, err)
	}
	f.groups[0].members[3].Action, f.groups[0].members[3].Status = "NONE", "RUNNING"
	prepare(t, p, s, cloud.SuccessorCreated)
}

// A resize still running is not undone, even before its group has grown.
func TestAddFirstReleaseWaitsForARunningResize(t *testing.T) {
	f := newFakeGKE(3)
	p := testProvider(f)
	s := prepare(t, p, newOperation(), cloud.SuccessorPlanned)
	f.HoldNextRequest()
	s = prepare(t, p, s, cloud.SuccessorRequested)
	f.groups[0].target = 3
	f.calls = nil
	if err := p.ReleaseSuccessor(t.Context(), s); !errors.Is(err, cloud.ErrNeedsOperator) || len(f.calls) != 0 {
		t.Fatalf("release = %v, calls %q; want needs-operator and no change", err, f.calls)
	}
}

// A zone out of GPUs keeps retrying the new member while GKE's resize is
// not done: the release still deletes that member by name and needs no
// operator.
func TestAddFirstReleaseAfterAStockoutDeletesTheMember(t *testing.T) {
	f := newFakeGKE(3)
	f.createErrors = []string{"ZONE_RESOURCE_POOL_EXHAUSTED"}
	p := testProvider(f)
	s := prepare(t, p, prepare(t, p, newOperation(), cloud.SuccessorPlanned), cloud.SuccessorRequested)
	for i := range f.ops {
		f.ops[i].Status = "RUNNING"
	}
	if _, err := p.PrepareSuccessor(t.Context(), s); !errors.Is(err, cloud.ErrReplacementCapacity) {
		t.Fatalf("settle = %v, want capacity", err)
	}
	f.calls = nil
	if err := p.ReleaseSuccessor(t.Context(), s); err != nil {
		t.Fatalf("release = %v", err)
	}
	if !slices.Equal(f.calls, []string{"delete " + testZone + " " + testName(testZone, 3)}) || f.groups[0].target != 3 {
		t.Fatalf("calls=%q target=%d", f.calls, f.groups[0].target)
	}
}

// A resize someone made while the step waited for approval is not taken for
// the operation's own: the request is refused and nothing more is changed.
func TestAddFirstDoesNotTakeAResizeMadeBeforeTheRequest(t *testing.T) {
	f := newFakeGKE(3)
	p := testProvider(f)
	planned := prepare(t, p, newOperation(), cloud.SuccessorPlanned)
	// A person grows the pool by one; the request goes out an hour later.
	if _, err := f.SetPoolSize(t.Context(), testPool, 4); err != nil {
		t.Fatal(err)
	}
	planned.RequestedAt = f.now.Add(time.Hour)
	f.calls = nil
	if _, err := p.PrepareSuccessor(t.Context(), planned); !errors.Is(err, cloud.ErrAddFirstUnavailable) || len(f.calls) != 0 {
		t.Fatalf("request = %v, calls %q", err, f.calls)
	}
	if err := p.ReleaseSuccessor(t.Context(), planned); err != nil || len(f.calls) != 0 {
		t.Fatalf("release = %v, calls %q", err, f.calls)
	}
}

// A resize after the request time that did not grow the pool by exactly one
// is not the operation's either.
func TestAddFirstDoesNotTakeAResizeOfAnotherSize(t *testing.T) {
	f := newFakeGKE(3)
	p := testProvider(f)
	planned := prepare(t, p, newOperation(), cloud.SuccessorPlanned)
	planned.RequestedAt = f.now
	if _, err := f.SetPoolSize(t.Context(), testPool, 5); err != nil {
		t.Fatal(err)
	}
	f.calls = nil
	if _, err := p.PrepareSuccessor(t.Context(), planned); !errors.Is(err, cloud.ErrAddFirstUnavailable) || len(f.calls) != 0 {
		t.Fatalf("request = %v, calls %q", err, f.calls)
	}
}

// The operation's own resize, whose answer was lost, is found with the
// request time set.
func TestAddFirstFindsItsLostResizeAfterTheRequestTime(t *testing.T) {
	f := newFakeGKE(3)
	p := testProvider(f)
	planned := prepare(t, p, newOperation(), cloud.SuccessorPlanned)
	planned.RequestedAt = f.now.Add(30 * time.Second) // GKE's clock is behind
	f.LoseNextAnswer()
	if _, err := p.PrepareSuccessor(t.Context(), planned); err == nil {
		t.Fatal("a lost answer was reported as success")
	}
	s := prepare(t, p, planned, cloud.SuccessorRequested)
	if f.requests != 1 || s.State[stateOperation] == "" {
		t.Fatalf("requests=%d state=%v", f.requests, s.State)
	}
}
