// SPDX-License-Identifier: Apache-2.0

package azure

import (
	"errors"
	"slices"
	"testing"

	"github.com/kubeneuron/kubeneuron/internal/cloud/removefirst"
	"github.com/kubeneuron/kubeneuron/pkg/cloud"
)

// The grow is handed to the caller before it is sent, with the size read
// and the absolute target; a failed record sends nothing.
func TestReplaceRecordsTheGrowBeforeSendingIt(t *testing.T) {
	f := newFakeAKS(3)
	target := testMachine(1)
	var got []removefirst.Grow
	wantErr := errors.New("record failed")
	_, err := testProvider(f).ReplaceWithIntents(t.Context(), testInstance(1), removefirst.Intents{
		Grow:   func(g removefirst.Grow) error { got = append(got, g); return wantErr },
		Delete: func(string, string, string) error { t.Fatal("delete intent before the grow"); return nil },
	})
	want := removefirst.Grow{Pool: testPool, Machine: target.Name, Before: 3, Target: 4, Marker: target.Name + ":3"}
	if !errors.Is(err, wantErr) || len(got) != 1 || got[0] != want || len(f.calls) != 0 || f.count != 3 {
		t.Fatalf("err=%v grows=%+v calls=%v count=%d", err, got, f.calls, f.count)
	}

	// Recorded, the grow is sent and the delete intent follows.
	var deletes int
	_, err = testProvider(f).ReplaceWithIntents(t.Context(), testInstance(1), removefirst.Intents{
		Grow:   func(removefirst.Grow) error { return nil },
		Delete: func(string, string, string) error { deletes++; return nil },
	})
	if err != nil || deletes != 1 || f.count != 3 || slices.Contains(f.machines, target) {
		t.Fatalf("err=%v deletes=%d count=%d machines=%v", err, deletes, f.count, f.machines)
	}
}

// A retry of a pool already grown for the machine records no second grow.
func TestReplaceRecordsNoGrowForAPoolAlreadyGrown(t *testing.T) {
	f := newFakeAKS(2)
	f.LoseNextAnswer()
	p := testProvider(f)
	if err := p.Replace(t.Context(), testInstance(0)); err == nil {
		t.Fatal("lost answer reported success")
	}
	_, err := p.ReplaceWithIntents(t.Context(), testInstance(0), removefirst.Intents{
		Grow:   func(removefirst.Grow) error { t.Fatal("recorded a second grow"); return nil },
		Delete: func(string, string, string) error { return nil },
	})
	if err != nil || f.requests != 1 || f.count != 2 {
		t.Fatalf("err=%v grow requests=%d count=%d", err, f.requests, f.count)
	}
}

func TestObserveRemoveFirstGrow(t *testing.T) {
	g := removefirst.Grow{Pool: testPool, Machine: testMachine(0).Name, Before: 2, Target: 3, Marker: testMachine(0).Name + ":2"}
	for _, tc := range []struct {
		name  string
		setup func(f *fakeAKS)
		want  removefirst.GrowState
		size  int32
	}{
		{"not sent", func(*fakeAKS) {}, removefirst.GrowNotApplied, 2},
		{"applied", func(f *fakeAKS) { grow(f, g.Marker) }, removefirst.GrowApplied, 3},
		{"applied, machine not listed yet", func(f *fakeAKS) { grow(f, g.Marker); f.machines = f.machines[:2] }, removefirst.GrowUnknown, 0},
		{"busy", func(f *fakeAKS) { f.state = "Scaling" }, removefirst.GrowUnknown, 0},
		{"failed", func(f *fakeAKS) { grow(f, g.Marker); f.state = "Failed" }, removefirst.GrowChanged, 0},
		{"another tag", func(f *fakeAKS) { grow(f, testMachine(1).Name+":2") }, removefirst.GrowChanged, 3},
		{"resized by someone else", func(f *fakeAKS) { f.count = 5 }, removefirst.GrowChanged, 5},
		{"tagged at another size", func(f *fakeAKS) { grow(f, g.Marker); f.count = 4 }, removefirst.GrowChanged, 4},
		{"autoscaled", func(f *fakeAKS) { f.autoscaling = true }, removefirst.GrowChanged, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeAKS(2)
			tc.setup(f)
			obs, err := testProvider(f).ObserveRemoveFirstGrow(t.Context(), g)
			if err != nil || obs.State != tc.want || obs.Size != tc.size || len(f.calls) != 0 {
				t.Fatalf("obs=%+v err=%v writes=%v; want %q size %d", obs, err, f.calls, tc.want, tc.size)
			}
		})
	}
}

// grow puts the pool one machine larger with the replacing tag value.
func grow(f *fakeAKS, value string) {
	f.count++
	f.machines = append(f.machines, testMachine(f.next))
	f.next++
	f.tags[replacingTag] = value
}

func TestSettleGrownPool(t *testing.T) {
	old := testMachine(0)
	t.Run("back to size", func(t *testing.T) {
		f := newFakeAKS(2)
		grow(f, old.Name+":2")
		f.machines, f.count = f.machines[1:], 2
		res, err := testProvider(f).SettleGrownPool(t.Context(), testPool, old.Name)
		if _, tagged := f.tags[replacingTag]; err != nil || tagged || res.Extra || !res.Untagged || f.tags["team"] != "ml" {
			t.Fatalf("res=%+v err=%v tags=%v", res, err, f.tags)
		}
	})
	t.Run("one node more", func(t *testing.T) {
		f := newFakeAKS(2)
		grow(f, old.Name+":2")
		f.machines = f.machines[1:]
		res, err := testProvider(f).SettleGrownPool(t.Context(), testPool, old.Name)
		if !errors.Is(err, cloud.ErrNeedsOperator) || !res.Extra || res.Size != 3 || len(f.calls) != 0 {
			t.Fatalf("res=%+v err=%v calls=%v", res, err, f.calls)
		}
	})
	t.Run("another machine's tag", func(t *testing.T) {
		f := newFakeAKS(2)
		grow(f, testMachine(1).Name+":2")
		res, err := testProvider(f).SettleGrownPool(t.Context(), testPool, old.Name)
		if err != nil || res != (removefirst.SettleResult{}) || len(f.calls) != 0 {
			t.Fatalf("res=%+v err=%v calls=%v", res, err, f.calls)
		}
	})
}

// A rejected initial request is a refusal; capacity codes still read through.
func TestRefusedKeepsCapacityCodes(t *testing.T) {
	err := refused(armFailure("BadRequest", "QuotaExceeded"))
	if !errors.Is(err, removefirst.ErrRefused) || capacityFailure(err) == nil {
		t.Fatalf("err=%v", err)
	}
	if plain := errors.New("context deadline exceeded"); refused(plain) != plain {
		t.Fatal("a lost answer was read as a refusal")
	}
}
