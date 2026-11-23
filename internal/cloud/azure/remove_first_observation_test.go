// SPDX-License-Identifier: Apache-2.0

package azure

import (
	"testing"

	"github.com/kubeneuron/kubeneuron/internal/cloud/removefirst"
)

func TestObserveRemoveFirstDelete(t *testing.T) {
	f := newFakeAKS(2)
	p := testProvider(f)
	old := testMachine(1)
	check := func(want removefirst.Observation) {
		t.Helper()
		got, err := p.ObserveRemoveFirstDelete(t.Context(), testInstance(1), testPool, old.Name)
		if got != want || err != nil || len(f.calls) != 0 {
			t.Fatalf("got %q, %v, writes %v; want %q", got, err, f.calls, want)
		}
	}
	check(removefirst.Listed)
	f.machines[1].ResourceID = testMachine(2).ResourceID
	check(removefirst.IdentityChanged)
	f.machines[1] = old
	f.machines[1].Name = testMachine(2).Name
	check(removefirst.IdentityChanged)
	f.machines = f.machines[:1]
	check(removefirst.Deleted)
}
