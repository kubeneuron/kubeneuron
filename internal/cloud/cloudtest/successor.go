// SPDX-License-Identifier: Apache-2.0

package cloudtest

import (
	"context"
	"errors"
	"testing"

	"github.com/kubeneuron/kubeneuron/pkg/cloud"
)

// SuccessorCloud is the fake cloud behind a provider under the successor
// contract checks. Each method controls or reads the pool of the case's
// instance.
type SuccessorCloud interface {
	// HoldNextRequest makes the next request to grow the pool accepted but
	// leaves it in progress: no new instance appears until Deliver.
	HoldNextRequest()
	// Deliver brings up the instance of the held request.
	Deliver()
	// LoseNextAnswer makes the next request reach the cloud and be accepted,
	// but fail for the caller as if the answer was lost.
	LoseNextAnswer()
	// Requests is how many requests to grow the pool the cloud accepted.
	Requests() int
	// Size is the pool's requested size.
	Size() int
}

// SuccessorCase is what a provider's successor contract run needs: a fresh
// provider and fake cloud per check, and a pinned instance in a pool that
// can grow by one.
type SuccessorCase struct {
	New      func(t *testing.T) (cloud.SuccessorProvider, SuccessorCloud)
	Instance string
}

// SuccessorConformance checks the parts of the successor contract a
// provider must keep whatever its cloud: a request still in progress is
// waited for, not an error; a retry after a lost answer does not grow the
// pool again; and an operation whose request may still be in flight is not
// undone on the evidence of unchanged members alone.
func SuccessorConformance(t *testing.T, c SuccessorCase) {
	t.Helper()
	ctx := context.Background()
	plan := func(t *testing.T, p cloud.SuccessorProvider, op string) cloud.Successor {
		t.Helper()
		s, err := p.PrepareSuccessor(ctx, cloud.Successor{Operation: op, Instance: c.Instance})
		if err != nil || s.Phase != cloud.SuccessorPlanned {
			t.Fatalf("plan = %+v, %v; want phase %q", s, err, cloud.SuccessorPlanned)
		}
		return s
	}

	t.Run("a request in progress is waited for", func(t *testing.T) {
		p, fake := c.New(t)
		size := fake.Size()
		s := plan(t, p, "op-progress")
		if fake.Requests() != 0 {
			t.Fatalf("planning sent %d requests, want none", fake.Requests())
		}
		fake.HoldNextRequest()
		s, err := p.PrepareSuccessor(ctx, s)
		if err != nil || s.Phase != cloud.SuccessorRequested {
			t.Fatalf("request = %+v, %v; want phase %q", s, err, cloud.SuccessorRequested)
		}
		for range 2 {
			if s, err = p.PrepareSuccessor(ctx, s); err != nil || s.Phase != cloud.SuccessorRequested {
				t.Fatalf("no new instance yet = %+v, %v; want phase %q and no error", s, err, cloud.SuccessorRequested)
			}
		}
		fake.Deliver()
		if s, err = p.PrepareSuccessor(ctx, s); err != nil || s.Phase != cloud.SuccessorCreated || s.ProviderID == "" {
			t.Fatalf("delivered = %+v, %v; want phase %q with a providerID", s, err, cloud.SuccessorCreated)
		}
		if fake.Requests() != 1 || fake.Size() != size+1 {
			t.Fatalf("requests %d, size %d; want 1 and %d", fake.Requests(), fake.Size(), size+1)
		}
		if err := p.ReplaceWithSuccessor(ctx, s); err != nil {
			t.Fatalf("replace: %v", err)
		}
		if fake.Size() != size {
			t.Fatalf("size after replace = %d, want %d", fake.Size(), size)
		}
	})

	t.Run("a retry after a lost answer grows the pool once", func(t *testing.T) {
		p, fake := c.New(t)
		size := fake.Size()
		planned := plan(t, p, "op-lost")
		fake.HoldNextRequest()
		fake.LoseNextAnswer()
		if _, err := p.PrepareSuccessor(ctx, planned); err == nil {
			t.Fatal("a lost answer was reported as success")
		}
		// The caller recorded only the plan; it asks again after a restart.
		s, err := p.PrepareSuccessor(ctx, planned)
		if err != nil || s.Phase != cloud.SuccessorRequested {
			t.Fatalf("retry = %+v, %v; want phase %q", s, err, cloud.SuccessorRequested)
		}
		if fake.Requests() != 1 || fake.Size() != size+1 {
			t.Fatalf("requests %d, size %d after the retry; want 1 and %d", fake.Requests(), fake.Size(), size+1)
		}
	})

	t.Run("a request that may be in flight is not undone", func(t *testing.T) {
		p, fake := c.New(t)
		size := fake.Size()
		planned := plan(t, p, "op-uncertain")
		fake.HoldNextRequest()
		fake.LoseNextAnswer()
		if _, err := p.PrepareSuccessor(ctx, planned); err == nil {
			t.Fatal("a lost answer was reported as success")
		}
		// The members are those before, but the request is still in flight.
		err := p.ReleaseSuccessor(ctx, planned)
		if !errors.Is(err, cloud.ErrNeedsOperator) {
			t.Fatalf("release = %v, want ErrNeedsOperator", err)
		}
		if fake.Size() != size+1 {
			t.Fatalf("release changed the size to %d, want %d left for the operator", fake.Size(), size+1)
		}
	})

	t.Run("a plan never requested is released without a change", func(t *testing.T) {
		p, fake := c.New(t)
		size := fake.Size()
		planned := plan(t, p, "op-unsent")
		if err := p.ReleaseSuccessor(ctx, planned); err != nil {
			t.Fatalf("release of an unsent plan: %v", err)
		}
		if fake.Requests() != 0 || fake.Size() != size {
			t.Fatalf("requests %d, size %d; want 0 and %d", fake.Requests(), fake.Size(), size)
		}
	})
}
