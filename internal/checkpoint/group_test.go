package checkpoint

import (
	"strings"
	"testing"
	"time"
)

func TestGroupOfReadsOnlyAValidDNSLabel(t *testing.T) {
	for name, tc := range map[string]struct {
		value   string
		present bool
		want    string
		invalid bool
	}{
		"absent":        {present: false},
		"valid":         {present: true, value: "run-42", want: "run-42"},
		"single char":   {present: true, value: "a", want: "a"},
		"empty":         {present: true, value: "", invalid: true},
		"uppercase":     {present: true, value: "Run-42", invalid: true},
		"leading dash":  {present: true, value: "-run", invalid: true},
		"dot":           {present: true, value: "run.42", invalid: true},
		"too long":      {present: true, value: strings.Repeat("a", 64), invalid: true},
		"max length":    {present: true, value: strings.Repeat("a", 63), want: strings.Repeat("a", 63)},
		"spaces around": {present: true, value: " run ", invalid: true},
	} {
		t.Run(name, func(t *testing.T) {
			w := Workload{Namespace: "ml", Name: "p", Annotations: map[string]string{}}
			if tc.present {
				w.Annotations[AnnotationGroup] = tc.value
			}
			got, invalid := w.Group()
			if got != tc.want || invalid != tc.invalid {
				t.Fatalf("Group() = %q, %v; want %q, %v", got, invalid, tc.want, tc.invalid)
			}
			key, ok := GroupOf(w)
			if ok != (tc.want != "") || (ok && key != (GroupKey{Namespace: "ml", Name: tc.want})) {
				t.Fatalf("GroupOf = %+v, %v", key, ok)
			}
		})
	}
}

func TestGroupPolicyIsBoundedAndRequiresCoordination(t *testing.T) {
	for name, tc := range map[string]struct {
		policy Policy
		want   string
	}{
		"off": {policy: Policy{}},
		"on with default size": {policy: func() Policy {
			p := enabledPolicy()
			p.Groups = GroupPolicy{Enabled: true, MaxGroupSize: DefaultMaxGroupSize}
			return p
		}()},
		"at the ceiling": {policy: func() Policy {
			p := enabledPolicy()
			p.Groups = GroupPolicy{Enabled: true, MaxGroupSize: MaxGroupSizeCeiling}
			return p
		}()},
		"above the ceiling": {policy: func() Policy {
			p := enabledPolicy()
			p.Groups = GroupPolicy{Enabled: true, MaxGroupSize: MaxGroupSizeCeiling + 1}
			return p
		}(), want: "exceeds the"},
		"zero size": {policy: func() Policy {
			p := enabledPolicy()
			p.Groups = GroupPolicy{Enabled: true}
			return p
		}(), want: "must be positive"},
		"on inside a disabled policy": {policy: Policy{Groups: GroupPolicy{Enabled: true, MaxGroupSize: 8}}, want: "requires checkpointCoordination.enabled"},
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
	p.Groups = GroupPolicy{Enabled: true, MaxGroupSize: 9, CrossNode: true}
	if got := p.Clone().Groups; got != p.Groups {
		t.Fatalf("Clone dropped the group policy: %+v", got)
	}
}

// Whatever a peer carries — no request, a longer request, a garbage request,
// a request longer than the ceiling — its wait is BoundedWait: never above
// maxWait and never above the budget. The group cannot extend the window.
func TestPeerWaitNeverExceedsTheCeilingForAnyAnnotations(t *testing.T) {
	policy := enabledPolicy()
	policy.Groups = GroupPolicy{Enabled: true, MaxGroupSize: 64, CrossNode: true}
	budgets := []time.Duration{0, time.Second, 4 * time.Minute, 15 * time.Minute, time.Hour}
	requests := []string{"", "1s", "4m", "15m", "16m", "30m", "24h", "garbage", "-5m", "0s", "1ms"}
	for _, budget := range budgets {
		for _, req := range requests {
			w := optedIn("training", map[string]string{AnnotationGroup: "run-1", AnnotationMaxWait: req})
			if req == "" {
				delete(w.Annotations, AnnotationMaxWait)
			}
			wait := policy.BoundedWait(w, budget)
			ceiling := policy.MaxWait
			if budget > 0 && budget < ceiling {
				ceiling = budget
			}
			if wait > ceiling || wait < 0 {
				t.Fatalf("budget %v request %q: wait %v exceeds ceiling %v", budget, req, wait, ceiling)
			}
			if wait > MaxWaitCeiling {
				t.Fatalf("wait %v above the installation ceiling", wait)
			}
		}
	}
}
