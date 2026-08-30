package baremetal

import (
	"context"
	"testing"
)

// TestBaremetalCountsCordonHolders is the compile-time guarantee made concrete.
//
// Counting holders used to be an OPTIONAL interface with a fallback to the
// unguarded Cordon/Uncordon pair, and a platform that did not implement it —
// this one — silently got the single-owner behaviour. That is the P0 the
// counting exists to prevent: a machine has several GPUs, two incidents can
// hold it, and the first to finish handed it back while the other was still
// resetting a GPU on it. A new adapter would have compiled, passed its own
// tests, and been wrong only in production.
func TestBaremetalCountsCordonHolders(t *testing.T) {
	p, err := New("", Hooks{})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	if err := p.CordonForOwner(ctx, "node-a", "inc-1", "ecc-dbe"); err != nil {
		t.Fatal(err)
	}
	if err := p.CordonForOwner(ctx, "node-a", "inc-2", "xid-79"); err != nil {
		t.Fatal(err)
	}

	released, remaining, err := p.ReleaseCordonOwners(ctx, "node-a", []string{"inc-1"})
	if err != nil {
		t.Fatal(err)
	}
	if released {
		t.Fatal("the first incident to finish released a node the second still holds; on a " +
			"multi-GPU machine that returns tenant work to a node whose other GPU is about " +
			"to be reset")
	}
	if remaining != 1 {
		t.Fatalf("remaining holders = %d, want 1", remaining)
	}

	released, remaining, err = p.ReleaseCordonOwners(ctx, "node-a", []string{"inc-2"})
	if err != nil {
		t.Fatal(err)
	}
	if !released || remaining != 0 {
		t.Fatalf("the last holder leaving did not release the node: released=%v remaining=%d",
			released, remaining)
	}

	// Releasing an owner that is not there is a no-op, not an error: steps are
	// retried and replayed, and a release that already happened must stay quiet.
	if _, _, err := p.ReleaseCordonOwners(ctx, "node-a", []string{"inc-1"}); err != nil {
		t.Fatalf("a replayed release returned an error: %v", err)
	}
}

// TestBaremetalUnownedCordonIsItsOwnHolder: a caller acting on the node rather
// than on behalf of an incident still must not release somebody else's hold,
// and must not have its own hold released by an incident finishing.
func TestBaremetalUnownedCordonIsItsOwnHolder(t *testing.T) {
	p, err := New("", Hooks{})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	if err := p.Cordon(ctx, "node-a", "operator did this by hand"); err != nil {
		t.Fatal(err)
	}
	if err := p.CordonForOwner(ctx, "node-a", "inc-1", "ecc-dbe"); err != nil {
		t.Fatal(err)
	}

	released, _, err := p.ReleaseCordonOwners(ctx, "node-a", []string{"inc-1"})
	if err != nil {
		t.Fatal(err)
	}
	if released {
		t.Fatal("an incident's release returned a node that was also being held without an " +
			"owner; the unowned hold is somebody's deliberate act and outlives the incident")
	}

	if err := p.Uncordon(ctx, "node-a"); err != nil {
		t.Fatal(err)
	}
	if released, _, err := p.ReleaseCordonOwners(ctx, "node-a", []string{"nobody"}); err != nil || released {
		t.Fatalf("releasing an absent owner on a free node reported a release: released=%v err=%v",
			released, err)
	}
}
