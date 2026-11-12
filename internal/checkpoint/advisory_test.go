// SPDX-License-Identifier: LicenseRef-KubeNeuron

package checkpoint

import (
	"strings"
	"testing"
	"time"
)

func validAdvisory() Advisory {
	return Advisory{
		IncidentID: "inc-8f2a", Round: 3,
		AdvisedAt: time.Date(2026, 11, 3, 10, 14, 2, 0, time.UTC),
		Reason:    "ecc-dbe", NextAction: "platform.drain",
	}
}

func TestAdvisoryKeysAreOwnedAndDisjointFromRequestKeys(t *testing.T) {
	adv := validAdvisory()
	ann := adv.Annotations()
	if len(ann) != 6 {
		t.Fatalf("advisory renders %d keys, want 6: %v", len(ann), ann)
	}
	requestKeys := validRequest().Annotations()
	for key := range ann {
		if !IsOwnedAnnotation(key) {
			t.Errorf("%s is not an owned annotation", key)
		}
		if _, clash := requestKeys[key]; clash {
			t.Errorf("%s is both an advisory and a request key", key)
		}
		if key == AnnotationOptIn || key == AnnotationMaxWait || key == AnnotationState {
			t.Errorf("%s is workload-owned and must never be rendered by an advisory", key)
		}
	}
	for key := range WithdrawalAnnotations(adv.AdvisedAt) {
		if !IsOwnedAnnotation(key) {
			t.Errorf("%s is not an owned annotation", key)
		}
	}
	if ann[AnnotationAdvisory] != AdvisoryValue || ann[AnnotationAdvisoryRound] != "3" {
		t.Fatalf("unexpected rendering: %v", ann)
	}
}

func validRequest() Request {
	return Request{IncidentID: "inc-x", RequestedAt: time.Unix(0, 0).UTC(), DeadlineAt: time.Unix(60, 0).UTC(), Reason: "ecc-dbe", NextAction: "platform.drain"}
}

func TestAdvisoryRoundTrips(t *testing.T) {
	adv := validAdvisory()
	got, ok := ParseAdvisory(adv.Annotations())
	if !ok {
		t.Fatal("rendered advisory does not parse")
	}
	if got != adv {
		t.Fatalf("round trip changed the advisory:\n got %+v\nwant %+v", got, adv)
	}
	if !adv.SameRound(got) {
		t.Fatal("a parsed stamp is not recognised as the same round")
	}
	if IsAdvisoryWithdrawn(adv.Annotations()) {
		t.Fatal("a fresh advisory reads as withdrawn")
	}
	ann := adv.Annotations()
	for k, v := range WithdrawalAnnotations(adv.AdvisedAt.Add(time.Minute)) {
		ann[k] = v
	}
	if !IsAdvisoryWithdrawn(ann) {
		t.Fatal("a withdrawn advisory does not read as withdrawn")
	}
	if _, ok := ParseAdvisory(ann); !ok {
		t.Fatal("a withdrawn advisory must still parse: the round it advised stays true")
	}
}

func TestParseAdvisoryFailsClosed(t *testing.T) {
	base := validAdvisory().Annotations()
	for name, mutate := range map[string]func(map[string]string){
		"not in force":   func(m map[string]string) { m[AnnotationAdvisory] = "True" },
		"no advisory":    func(m map[string]string) { delete(m, AnnotationAdvisory) },
		"blank incident": func(m map[string]string) { m[AnnotationAdvisoryIncident] = "  " },
		"round zero":     func(m map[string]string) { m[AnnotationAdvisoryRound] = "0" },
		"round negative": func(m map[string]string) { m[AnnotationAdvisoryRound] = "-2" },
		"round garbage":  func(m map[string]string) { m[AnnotationAdvisoryRound] = "three" },
		"round missing":  func(m map[string]string) { delete(m, AnnotationAdvisoryRound) },
	} {
		t.Run(name, func(t *testing.T) {
			ann := map[string]string{}
			for k, v := range base {
				ann[k] = v
			}
			mutate(ann)
			if _, ok := ParseAdvisory(ann); ok {
				t.Fatalf("parsed a stamp that is not an advisory: %v", ann)
			}
			if IsAdvisoryWithdrawn(ann) {
				t.Fatal("a non-advisory reads as withdrawn")
			}
		})
	}
	// A withdrawn-at key with nothing beside it is residue, not a withdrawal.
	if IsAdvisoryWithdrawn(WithdrawalAnnotations(time.Now())) {
		t.Fatal("a lone withdrawn-at key reads as a withdrawal")
	}
}

func TestSameRoundComparesIncidentAndRoundOnly(t *testing.T) {
	adv := validAdvisory()
	later := adv
	later.AdvisedAt = adv.AdvisedAt.Add(time.Hour)
	if !adv.SameRound(later) {
		t.Fatal("a different timestamp must not make a different round")
	}
	other := adv
	other.Round = 4
	if adv.SameRound(other) {
		t.Fatal("a different round compared equal")
	}
	foreign := adv
	foreign.IncidentID = "inc-other"
	if adv.SameRound(foreign) {
		t.Fatal("a different incident compared equal")
	}
	if (Advisory{}).SameRound(Advisory{}) {
		t.Fatal("two empty advisories must not compare as the same round")
	}
}

func TestAdvisoryValidate(t *testing.T) {
	for name, tc := range map[string]struct {
		mutate func(*Advisory)
		want   string
	}{
		"valid":           {mutate: func(*Advisory) {}, want: ""},
		"blank incident":  {mutate: func(a *Advisory) { a.IncidentID = " " }, want: "names no incident"},
		"padded incident": {mutate: func(a *Advisory) { a.IncidentID = " inc" }, want: "surrounding whitespace"},
		"round zero":      {mutate: func(a *Advisory) { a.Round = 0 }, want: "round must be positive"},
		"blank reason":    {mutate: func(a *Advisory) { a.Reason = "" }, want: "names no reason"},
		"blank next":      {mutate: func(a *Advisory) { a.NextAction = "" }, want: "names no next action"},
		"zero time":       {mutate: func(a *Advisory) { a.AdvisedAt = time.Time{} }, want: "no advised-at"},
		"unrepresentable": {mutate: func(a *Advisory) { a.AdvisedAt = time.Date(99999, 1, 1, 0, 0, 0, 0, time.UTC) }, want: "not representable"},
	} {
		t.Run(name, func(t *testing.T) {
			adv := validAdvisory()
			tc.mutate(&adv)
			err := adv.Validate()
			switch {
			case tc.want == "" && err != nil:
				t.Fatalf("unexpected error: %v", err)
			case tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)):
				t.Fatalf("want error containing %q, got %v", tc.want, err)
			}
		})
	}
}

func TestAdvisoryPolicyIsBoundedAndRequiresCoordination(t *testing.T) {
	for name, tc := range map[string]struct {
		policy Policy
		want   string
	}{
		"off everywhere": {policy: Policy{}},
		"on inside an enabled policy": {policy: func() Policy {
			p := enabledPolicy()
			p.Advisory = AdvisoryPolicy{Enabled: true, Budget: DefaultAdvisoryBudget}
			return p
		}()},
		"at the ceiling": {policy: func() Policy {
			p := enabledPolicy()
			p.Advisory = AdvisoryPolicy{Enabled: true, Budget: AdvisoryBudgetCeiling}
			return p
		}()},
		"above the ceiling": {policy: func() Policy {
			p := enabledPolicy()
			p.Advisory = AdvisoryPolicy{Enabled: true, Budget: AdvisoryBudgetCeiling + time.Second}
			return p
		}(), want: "exceeds the"},
		"zero budget": {policy: func() Policy {
			p := enabledPolicy()
			p.Advisory = AdvisoryPolicy{Enabled: true}
			return p
		}(), want: "must be positive"},
		"on inside a disabled policy": {
			policy: Policy{Advisory: AdvisoryPolicy{Enabled: true, Budget: time.Second}},
			want:   "requires checkpointCoordination.enabled",
		},
		"off with a stray budget inside a disabled policy": {
			policy: Policy{Advisory: AdvisoryPolicy{Budget: time.Second}},
		},
	} {
		t.Run(name, func(t *testing.T) {
			err := tc.policy.Validate()
			switch {
			case tc.want == "" && err != nil:
				t.Fatalf("unexpected error: %v", err)
			case tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)):
				t.Fatalf("want error containing %q, got %v", tc.want, err)
			}
		})
	}
	p := enabledPolicy()
	p.Advisory = AdvisoryPolicy{Enabled: true, Budget: 7 * time.Second}
	if got := p.Clone().Advisory; got != p.Advisory {
		t.Fatalf("Clone dropped the advisory policy: %+v", got)
	}
}

func TestAdvisoryOutcomesAreClosed(t *testing.T) {
	want := []AdvisoryOutcome{"stamped", "withdrawn", "unreachable", "skipped"}
	got := AdvisoryOutcomes()
	if len(got) != len(want) {
		t.Fatalf("outcomes %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("outcomes %v, want %v", got, want)
		}
	}
}

// A withdrawal older than the advisory's own stamp belongs to the round
// before it: a later round stamped over a withdrawn notice is in force.
func TestWithdrawalOlderThanTheStampIsNotThisRounds(t *testing.T) {
	first := validAdvisory()
	ann := first.Annotations()
	for k, v := range WithdrawalAnnotations(first.AdvisedAt.Add(time.Minute)) {
		ann[k] = v
	}
	if !IsAdvisoryWithdrawn(ann) {
		t.Fatal("round 1 withdrawn after its stamp must read as withdrawn")
	}
	second := first
	second.Round = 2
	second.AdvisedAt = first.AdvisedAt.Add(2 * time.Minute)
	for k, v := range second.Annotations() {
		ann[k] = v // the withdrawn-at key of round 1 stays, as on the object
	}
	if IsAdvisoryWithdrawn(ann) {
		t.Fatal("round 2, stamped after round 1's withdrawal, reads as withdrawn")
	}
	for k, v := range WithdrawalAnnotations(second.AdvisedAt) {
		ann[k] = v // withdrawn at the same instant it was stamped: still a withdrawal
	}
	if !IsAdvisoryWithdrawn(ann) {
		t.Fatal("a withdrawal at the stamp's own instant must read as withdrawn")
	}
}
