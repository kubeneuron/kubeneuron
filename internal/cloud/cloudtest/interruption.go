// SPDX-License-Identifier: Apache-2.0

package cloudtest

import (
	"context"
	"testing"

	"github.com/kubeneuron/kubeneuron/pkg/cloud"
)

// InterruptionCloud is the fake cloud behind a provider under the
// interruption contract checks.
type InterruptionCloud interface {
	// Announce makes the cloud announce one interruption of the case's
	// instance, as the provider's source would deliver it.
	Announce(kind cloud.InterruptionKind)
}

// InterruptionCase is what a provider's interruption contract run needs: a
// fresh provider over a fresh fake cloud with no announcement yet, and the
// node of the instance the cloud announces for.
type InterruptionCase struct {
	New  func(t *testing.T) (cloud.InterruptionAware, InterruptionCloud)
	Node cloud.NodeRef
}

// InterruptionConformance checks what the controller relies on of any
// InterruptionAware provider: nothing announced is an empty answer, not an
// error; an announcement is reported valid and for the instance it names; and
// the same announcement read again keeps its key, so the controller can tell
// it from a new one.
func InterruptionConformance(t *testing.T, c InterruptionCase) {
	t.Helper()
	ctx := context.Background()
	nodes := []cloud.NodeRef{c.Node}

	p, fake := c.New(t)
	got, err := p.Interruptions(ctx, nodes)
	if err != nil || len(got) != 0 {
		t.Fatalf("nothing announced: %+v, %v; want none and no error", got, err)
	}

	fake.Announce(cloud.InterruptionSpotReclaim)
	first, err := p.Interruptions(ctx, nodes)
	if err != nil || len(first) != 1 {
		t.Fatalf("one announcement: %+v, %v", first, err)
	}
	if err := first[0].Validate(); err != nil {
		t.Fatalf("announcement %+v: %v", first[0], err)
	}
	if first[0].InstanceID != c.Node.Identity.InstanceID || first[0].Kind != cloud.InterruptionSpotReclaim {
		t.Fatalf("announcement %+v, want spot-reclaim of %s", first[0], c.Node.Identity.InstanceID)
	}
	again, err := p.Interruptions(ctx, nodes)
	if err != nil {
		t.Fatal(err)
	}
	for _, i := range again {
		if i.Kind == cloud.InterruptionSpotReclaim && i.Key != first[0].Key {
			t.Fatalf("the same announcement read again has key %q, first %q", i.Key, first[0].Key)
		}
	}

	fake.Announce(cloud.InterruptionRebalance)
	both, err := p.Interruptions(ctx, nodes)
	if err != nil {
		t.Fatal(err)
	}
	// A queue may not report the first announcement again; the second one
	// must be there, under a key of its own.
	found := false
	for _, i := range both {
		if i.Kind != cloud.InterruptionRebalance {
			continue
		}
		found = true
		if i.Key == first[0].Key {
			t.Fatalf("a second announcement reused the first one's key: %+v", both)
		}
	}
	if !found {
		t.Fatalf("second announcement not reported: %+v", both)
	}
}
