package checkpoint

import (
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/kubeneuron/kubeneuron/pkg/types"
)

func enabledPolicy() Policy {
	return Policy{
		Enabled:     true,
		DefaultWait: 5 * time.Minute,
		MaxWait:     15 * time.Minute,
		SkipClasses: []types.ProblemClass{types.ClassFellOffBus, types.ClassGPULost},
		Namespaces:  []string{"training", "research"},
	}
}

func optedIn(ns string, extra map[string]string) Workload {
	ann := map[string]string{AnnotationOptIn: OptInValue}
	for k, v := range extra {
		ann[k] = v
	}
	return Workload{Namespace: ns, Name: "trainer-0", UID: "uid-1", Annotations: ann}
}

func TestPolicyValidate(t *testing.T) {
	for name, tc := range map[string]struct {
		mutate  func(p *Policy)
		wantErr string
	}{
		"valid":                 {mutate: func(*Policy) {}},
		"disabled is valid":     {mutate: func(p *Policy) { *p = Policy{} }},
		"disabled ignores junk": {mutate: func(p *Policy) { p.Enabled = false; p.MaxWait = -1; p.Namespaces = nil }},
		"zero default wait":     {mutate: func(p *Policy) { p.DefaultWait = 0 }, wantErr: "defaultWait must be positive"},
		"negative default wait": {mutate: func(p *Policy) { p.DefaultWait = -time.Second }, wantErr: "defaultWait must be positive"},
		"zero max wait":         {mutate: func(p *Policy) { p.MaxWait = 0 }, wantErr: "maxWait must be positive"},
		"max above ceiling":     {mutate: func(p *Policy) { p.MaxWait = MaxWaitCeiling + time.Second }, wantErr: "exceeds the 30m0s ceiling"},
		"max at ceiling":        {mutate: func(p *Policy) { p.MaxWait = MaxWaitCeiling }},
		"default above max":     {mutate: func(p *Policy) { p.DefaultWait = 16 * time.Minute }, wantErr: "defaultWait 16m0s exceeds maxWait"},
		"default equals max":    {mutate: func(p *Policy) { p.DefaultWait = p.MaxWait }},
		"no namespaces":         {mutate: func(p *Policy) { p.Namespaces = nil }, wantErr: "non-empty namespaces allowlist"},
		"blank namespace":       {mutate: func(p *Policy) { p.Namespaces = []string{"training", " "} }, wantErr: "blank entry"},
		"blank skip class":      {mutate: func(p *Policy) { p.SkipClasses = []types.ProblemClass{""} }, wantErr: "skipClasses must not contain a blank"},
		"no skip classes":       {mutate: func(p *Policy) { p.SkipClasses = nil }},
	} {
		t.Run(name, func(t *testing.T) {
			p := enabledPolicy()
			tc.mutate(&p)
			err := p.Validate()
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("Validate() = %v, want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("Validate() = %v, want substring %q", err, tc.wantErr)
			}
		})
	}
}

func TestPolicyCloneOwnsMemoryAndIsDeterministic(t *testing.T) {
	p := Policy{Enabled: true, Namespaces: []string{"b", "a"}, SkipClasses: []types.ProblemClass{"z", "y"}}
	c := p.Clone()
	p.Namespaces[0] = "mutated"
	p.SkipClasses[0] = "mutated"
	if !reflect.DeepEqual(c.Namespaces, []string{"a", "b"}) {
		t.Fatalf("Namespaces = %v, want sorted copy", c.Namespaces)
	}
	if !reflect.DeepEqual(c.SkipClasses, []types.ProblemClass{"y", "z"}) {
		t.Fatalf("SkipClasses = %v, want sorted copy", c.SkipClasses)
	}
	empty := Policy{}.Clone()
	if empty.Namespaces != nil || empty.SkipClasses != nil {
		t.Fatalf("zero policy clone must keep nil slices, got %+v", empty)
	}
}

func TestOptInIsExact(t *testing.T) {
	for value, want := range map[string]bool{
		"true": true, "True": false, "TRUE": false, "yes": false, "1": false, " true": false, "": false,
	} {
		w := Workload{Annotations: map[string]string{AnnotationOptIn: value}}
		if got := w.OptedIn(); got != want {
			t.Errorf("OptedIn(%q) = %v, want %v", value, got, want)
		}
	}
	if (Workload{}).OptedIn() {
		t.Error("a workload without annotations must not be opted in")
	}
}

func TestRequestedMaxWaitRejectsMalformedRequests(t *testing.T) {
	for raw, want := range map[string]struct {
		d  time.Duration
		ok bool
	}{
		"8m":     {8 * time.Minute, true},
		" 90s ":  {90 * time.Second, true},
		"1h":     {time.Hour, true},
		"":       {0, false},
		"soon":   {0, false},
		"0s":     {0, false},
		"-5m":    {0, false},
		"5":      {0, false},
		"1e9999": {0, false},
	} {
		w := Workload{Annotations: map[string]string{AnnotationMaxWait: raw}}
		d, ok := w.RequestedMaxWait()
		if d != want.d || ok != want.ok {
			t.Errorf("RequestedMaxWait(%q) = %v, %v; want %v, %v", raw, d, ok, want.d, want.ok)
		}
	}
	if _, ok := (Workload{}).RequestedMaxWait(); ok {
		t.Error("absent annotation must not be a request")
	}
}

func TestWaitForClampsToPolicy(t *testing.T) {
	p := enabledPolicy()
	for name, tc := range map[string]struct {
		request string
		absent  bool
		want    time.Duration
	}{
		"no request takes default":        {absent: true, want: 5 * time.Minute},
		"shorter request is honored":      {request: "2m", want: 2 * time.Minute},
		"longer request is clamped":       {request: "1h", want: 15 * time.Minute},
		"request at ceiling":              {request: "15m", want: 15 * time.Minute},
		"malformed request takes default": {request: "forever", want: 5 * time.Minute},
		"negative request takes default":  {request: "-1h", want: 5 * time.Minute},
		"zero request takes default":      {request: "0s", want: 5 * time.Minute},
	} {
		t.Run(name, func(t *testing.T) {
			extra := map[string]string{}
			if !tc.absent {
				extra[AnnotationMaxWait] = tc.request
			}
			if got := p.WaitFor(optedIn("training", extra)); got != tc.want {
				t.Fatalf("WaitFor = %v, want %v", got, tc.want)
			}
		})
	}
	// A default larger than max is an invalid policy, but WaitFor still never
	// exceeds MaxWait even if one is installed by a path that skipped Validate.
	broken := Policy{Enabled: true, DefaultWait: time.Hour, MaxWait: time.Minute}
	if got := broken.WaitFor(optedIn("x", nil)); got != time.Minute {
		t.Fatalf("WaitFor with default > max = %v, want the max", got)
	}
	if got := (Policy{}).WaitFor(optedIn("training", map[string]string{AnnotationMaxWait: "1m"})); got != 0 {
		t.Fatalf("a disabled policy grants %v, want 0", got)
	}
}

// BoundedWait is the grant one workload actually receives: its own request or
// the default, then the budget. A neighbour's longer grant never leaks in,
// because the function only ever sees one workload.
func TestBoundedWaitIsPerWorkload(t *testing.T) {
	p := enabledPolicy()
	short := optedIn("training", map[string]string{AnnotationMaxWait: "30s"})
	silent := optedIn("training", nil)
	for name, tc := range map[string]struct {
		w      Workload
		budget time.Duration
		want   time.Duration
	}{
		"request under budget":        {short, 10 * time.Minute, 30 * time.Second},
		"default under budget":        {silent, 10 * time.Minute, 5 * time.Minute},
		"default over budget":         {silent, 90 * time.Second, 90 * time.Second},
		"request over budget":         {short, 10 * time.Second, 10 * time.Second},
		"no budget known":             {silent, 0, 5 * time.Minute},
		"negative budget means known": {short, -time.Minute, 30 * time.Second},
	} {
		t.Run(name, func(t *testing.T) {
			if got := p.BoundedWait(tc.w, tc.budget); got != tc.want {
				t.Fatalf("BoundedWait = %v, want %v", got, tc.want)
			}
		})
	}
	if got := (Policy{}).BoundedWait(silent, time.Hour); got != 0 {
		t.Fatalf("disabled policy BoundedWait = %v, want 0", got)
	}
	// The step-wide bound is the longest per-workload grant, never more.
	if shared, longest := p.SharedWait([]Workload{short, silent}, 10*time.Minute), p.BoundedWait(silent, 10*time.Minute); shared != longest {
		t.Fatalf("SharedWait = %v, want the longest BoundedWait %v", shared, longest)
	}
}

// RequestStart measures from the durable stamp when it can be trusted and
// falls back to this step's own request time otherwise. It never returns a
// time after fresh nor more than the ceiling before it; bounding the elapsed
// observation itself is ObservedWait's job, tested below.
func TestRequestStartIsDurableButBounded(t *testing.T) {
	fresh := time.Date(2026, 9, 29, 10, 0, 0, 0, time.UTC)
	for name, tc := range map[string]struct {
		durable time.Time
		want    time.Time
	}{
		"no durable stamp":            {time.Time{}, fresh},
		"earlier durable stamp":       {fresh.Add(-3 * time.Minute), fresh.Add(-3 * time.Minute)},
		"same instant":                {fresh, fresh},
		"future stamp falls back":     {fresh.Add(time.Hour), fresh},
		"stamp at the ceiling":        {fresh.Add(-MaxWaitCeiling), fresh.Add(-MaxWaitCeiling)},
		"ancient stamp floored":       {time.Date(1970, 1, 1, 0, 0, 0, 0, time.UTC), fresh.Add(-MaxWaitCeiling)},
		"just past the ceiling floor": {fresh.Add(-MaxWaitCeiling - time.Second), fresh.Add(-MaxWaitCeiling)},
	} {
		t.Run(name, func(t *testing.T) {
			got := RequestStart(tc.durable, fresh)
			if !got.Equal(tc.want) {
				t.Fatalf("RequestStart = %v, want %v", got, tc.want)
			}
			if got.After(fresh) {
				t.Fatalf("RequestStart %v is after fresh %v: a negative wait would follow", got, fresh)
			}
		})
	}
}

// ObservedWait is what the wait metric observes: never negative and never
// above MaxWaitCeiling, whatever the start and settlement were. In
// particular a start floored by RequestStart one ceiling before the request,
// followed by a settlement AFTER the request, is cut to the ceiling rather
// than exceeding it; a valid resume within the ceiling is measured exactly.
func TestObservedWaitIsBoundedToTheCeiling(t *testing.T) {
	fresh := time.Date(2026, 9, 29, 10, 0, 0, 0, time.UTC)
	ancient := time.Date(1970, 1, 1, 0, 0, 0, 0, time.UTC)
	for name, tc := range map[string]struct {
		start, settled time.Time
		want           time.Duration
	}{
		"immediate settlement":               {fresh, fresh, 0},
		"settled before start observes zero": {fresh, fresh.Add(-time.Minute), 0},
		"fresh request measured exactly":     {fresh, fresh.Add(2 * time.Minute), 2 * time.Minute},
		"valid resume spans the durable window": {
			RequestStart(fresh.Add(-3*time.Minute), fresh), fresh.Add(2 * time.Minute), 5 * time.Minute},
		"resume settling exactly at the ceiling": {
			RequestStart(fresh.Add(-MaxWaitCeiling), fresh), fresh, MaxWaitCeiling},
		"floored start plus a wait after the request is cut to the ceiling": {
			RequestStart(ancient, fresh), fresh.Add(2 * time.Minute), MaxWaitCeiling},
		"ancient start without flooring is cut to the ceiling": {ancient, fresh.Add(time.Hour), MaxWaitCeiling},
	} {
		t.Run(name, func(t *testing.T) {
			got := ObservedWait(tc.start, tc.settled)
			if got != tc.want {
				t.Fatalf("ObservedWait = %v, want %v", got, tc.want)
			}
			if got < 0 || got > MaxWaitCeiling {
				t.Fatalf("ObservedWait = %v is outside [0, %v]", got, MaxWaitCeiling)
			}
		})
	}
}

func TestSharedWaitIsBoundedByPolicyAndBudget(t *testing.T) {
	p := enabledPolicy()
	short := optedIn("training", map[string]string{AnnotationMaxWait: "1m"})
	long := optedIn("training", map[string]string{AnnotationMaxWait: "10m"})
	greedy := optedIn("training", map[string]string{AnnotationMaxWait: "24h"})
	silent := optedIn("training", nil)
	for name, tc := range map[string]struct {
		workloads []Workload
		budget    time.Duration
		want      time.Duration
	}{
		"no workloads":                     {nil, 0, 0},
		"single request":                   {[]Workload{short}, 0, time.Minute},
		"longest request wins":             {[]Workload{short, long}, 0, 10 * time.Minute},
		"default participates":             {[]Workload{short, silent}, 0, 5 * time.Minute},
		"greedy request clamped to max":    {[]Workload{greedy}, 0, 15 * time.Minute},
		"budget shorter than request":      {[]Workload{long}, 3 * time.Minute, 3 * time.Minute},
		"budget longer than request":       {[]Workload{long}, time.Hour, 10 * time.Minute},
		"zero budget means unbounded step": {[]Workload{long}, 0, 10 * time.Minute},
		"negative budget means unknown":    {[]Workload{long}, -time.Minute, 10 * time.Minute},
	} {
		t.Run(name, func(t *testing.T) {
			if got := p.SharedWait(tc.workloads, tc.budget); got != tc.want {
				t.Fatalf("SharedWait = %v, want %v", got, tc.want)
			}
		})
	}
	if got := (Policy{}).SharedWait([]Workload{long}, 0); got != 0 {
		t.Fatalf("disabled policy SharedWait = %v, want 0", got)
	}
}

func TestEligibility(t *testing.T) {
	p := enabledPolicy()
	for name, tc := range map[string]struct {
		policy Policy
		w      Workload
		class  types.ProblemClass
		want   Ineligibility
	}{
		"eligible":                 {p, optedIn("training", nil), types.ClassECCDBE, ""},
		"second allowed ns":        {p, optedIn("research", nil), types.ClassECCDBE, ""},
		"disabled policy":          {Policy{}, optedIn("training", nil), types.ClassECCDBE, IneligiblePolicyDisabled},
		"disabled beats skip":      {Policy{SkipClasses: []types.ProblemClass{types.ClassECCDBE}}, optedIn("training", nil), types.ClassECCDBE, IneligiblePolicyDisabled},
		"skipped class":            {p, optedIn("training", nil), types.ClassFellOffBus, IneligibleClassSkipped},
		"other skipped class":      {p, optedIn("training", nil), types.ClassGPULost, IneligibleClassSkipped},
		"namespace not allowed":    {p, optedIn("prod", nil), types.ClassECCDBE, IneligibleNamespace},
		"empty namespace":          {p, optedIn("", nil), types.ClassECCDBE, IneligibleNamespace},
		"not opted in":             {p, Workload{Namespace: "training"}, types.ClassECCDBE, IneligibleNotOptedIn},
		"opt-in typo":              {p, Workload{Namespace: "training", Annotations: map[string]string{AnnotationOptIn: "True"}}, types.ClassECCDBE, IneligibleNotOptedIn},
		"skip wins over namespace": {p, optedIn("prod", nil), types.ClassFellOffBus, IneligibleClassSkipped},
	} {
		t.Run(name, func(t *testing.T) {
			if got := tc.policy.Eligibility(tc.w, tc.class); got != tc.want {
				t.Fatalf("Eligibility = %q, want %q", got, tc.want)
			}
		})
	}
	all := []Workload{optedIn("training", nil), optedIn("prod", nil), {Namespace: "training", Name: "plain"}}
	got := p.Eligible(all, types.ClassECCDBE)
	if len(got) != 1 || got[0].Namespace != "training" || got[0].Name != "trainer-0" {
		t.Fatalf("Eligible = %+v, want only the opted-in allowed workload", got)
	}
	if got := p.Eligible(all, types.ClassFellOffBus); got != nil {
		t.Fatalf("Eligible for a skipped class = %+v, want nil", got)
	}
	if got := (Policy{}).Eligible(all, types.ClassECCDBE); got != nil {
		t.Fatalf("Eligible under a disabled policy = %+v, want nil", got)
	}
}

func TestRequestAnnotationsRoundTripAndStayInPrefix(t *testing.T) {
	at := time.Date(2026, 8, 5, 11, 2, 13, 0, time.FixedZone("x", 3600))
	req := Request{
		IncidentID:  "inc-8f2a",
		RequestedAt: at,
		DeadlineAt:  at.Add(8 * time.Minute),
		Reason:      types.ClassECCDBE,
		NextAction:  "platform.drain",
	}
	ann := req.Annotations()
	for key := range ann {
		if !IsOwnedAnnotation(key) {
			t.Errorf("request writes %q, outside the owned prefix", key)
		}
	}
	if ann[AnnotationDeadlineAt] != "2026-08-05T10:10:13Z" {
		t.Fatalf("deadline rendered as %q, want UTC RFC 3339", ann[AnnotationDeadlineAt])
	}
	parsed, ok := ParseRequest(ann)
	if !ok {
		t.Fatal("ParseRequest must read back what Annotations wrote")
	}
	if parsed.IncidentID != req.IncidentID || !parsed.DeadlineAt.Equal(req.DeadlineAt) ||
		!parsed.RequestedAt.Equal(req.RequestedAt) || parsed.Reason != req.Reason || parsed.NextAction != req.NextAction {
		t.Fatalf("round trip = %+v, want %+v", parsed, req)
	}
}

// A valid checkpoint-max-wait can be shorter than a second, so the stamp must
// carry the fractional second: a 500ms window written as whole seconds would
// come back as a zero-length (or one-second) window, which is not what was
// granted. Whole-second values still render without a fraction, so a stamp
// written before the fraction was kept reads back unchanged.
func TestRequestAnnotationsKeepSubSecondWindows(t *testing.T) {
	at := time.Date(2026, 8, 5, 11, 2, 13, 250_000_000, time.UTC)
	req := Request{
		IncidentID: "inc-8f2a", RequestedAt: at, DeadlineAt: at.Add(500 * time.Millisecond),
		Reason: types.ClassECCDBE, NextAction: "platform.drain",
	}
	ann := req.Annotations()
	if ann[AnnotationRequestedAt] != "2026-08-05T11:02:13.25Z" || ann[AnnotationDeadlineAt] != "2026-08-05T11:02:13.75Z" {
		t.Fatalf("rendered %q / %q, want the fractional second kept", ann[AnnotationRequestedAt], ann[AnnotationDeadlineAt])
	}
	parsed, ok := ParseRequest(ann)
	if !ok {
		t.Fatal("ParseRequest must read back a fractional stamp")
	}
	if !parsed.RequestedAt.Equal(req.RequestedAt) || !parsed.DeadlineAt.Equal(req.DeadlineAt) {
		t.Fatalf("round trip = %v..%v, want %v..%v", parsed.RequestedAt, parsed.DeadlineAt, req.RequestedAt, req.DeadlineAt)
	}
	if window := parsed.DeadlineAt.Sub(parsed.RequestedAt); window != 500*time.Millisecond {
		t.Fatalf("window after round trip = %v, want 500ms", window)
	}
	// The resume path: a same-incident stamp resolves to exactly the
	// sub-second deadline it carries, not to a proposal rounded elsewhere.
	if got := ResolveDeadline(at.Add(time.Minute), "inc-8f2a", &parsed); !got.Equal(req.DeadlineAt) {
		t.Fatalf("ResolveDeadline = %v, want the stamped %v", got, req.DeadlineAt)
	}
	// A whole-second stamp, as an earlier build wrote it, still parses.
	legacy, ok := ParseRequest(map[string]string{AnnotationIncident: "inc", AnnotationDeadlineAt: "2026-08-05T10:10:13Z", AnnotationRequestedAt: "2026-08-05T10:02:13Z"})
	if !ok || !legacy.DeadlineAt.Equal(time.Date(2026, 8, 5, 10, 10, 13, 0, time.UTC)) {
		t.Fatalf("legacy stamp = %+v, %v; want it parsed", legacy, ok)
	}
	if (Request{RequestedAt: at.Truncate(time.Second), DeadlineAt: at.Truncate(time.Second)}).Annotations()[AnnotationDeadlineAt] != "2026-08-05T11:02:13Z" {
		t.Fatal("a whole-second time must render without a fraction")
	}
}

func TestParseRequestRejectsIncompleteStamps(t *testing.T) {
	for name, ann := range map[string]map[string]string{
		"nil":                nil,
		"unrelated only":     {AnnotationOptIn: "true"},
		"no incident":        {AnnotationDeadlineAt: "2026-08-05T10:10:13Z"},
		"blank incident":     {AnnotationIncident: " ", AnnotationDeadlineAt: "2026-08-05T10:10:13Z"},
		"no deadline":        {AnnotationIncident: "inc"},
		"malformed deadline": {AnnotationIncident: "inc", AnnotationDeadlineAt: "tomorrow"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, ok := ParseRequest(ann); ok {
				t.Fatal("ParseRequest accepted an incomplete stamp")
			}
		})
	}
	// A missing requested-at is tolerated: the deadline is the durable truth.
	req, ok := ParseRequest(map[string]string{AnnotationIncident: "inc", AnnotationDeadlineAt: "2026-08-05T10:10:13Z"})
	if !ok || !req.RequestedAt.IsZero() {
		t.Fatalf("ParseRequest = %+v, %v; want ok with zero RequestedAt", req, ok)
	}
}

func TestIsOwnedAnnotation(t *testing.T) {
	for key, want := range map[string]bool{
		AnnotationOptIn:                 true,
		AnnotationMaxWait:               true,
		AnnotationState:                 true,
		AnnotationDeadlineAt:            true,
		"kubeneuron.io/checkpoint-x":    true,
		"kubeneuron.io/checkpointing":   false,
		"kubeneuron.io/cordon-reason":   false,
		"checkpoint":                    false,
		"":                              false,
		"example.com/checkpoint":        false,
		"kubeneuron.io/checkpoint/deep": false,
	} {
		if got := IsOwnedAnnotation(key); got != want {
			t.Errorf("IsOwnedAnnotation(%q) = %v, want %v", key, got, want)
		}
	}
}

// The stamped deadline is durable: the same incident never gets a later
// deadline than the one already on the object, whether that one is still
// running or already over. Only a different incident, or no stamp at all,
// starts a new window.
func TestResolveDeadlineNeverExtendsASameIncidentStamp(t *testing.T) {
	now := time.Date(2026, 8, 5, 11, 0, 0, 0, time.UTC)
	proposed := now.Add(5 * time.Minute)
	for name, tc := range map[string]struct {
		existing *Request
		want     time.Time
	}{
		"no stamp":                   {nil, proposed},
		"same incident earlier":      {&Request{IncidentID: "inc", DeadlineAt: now.Add(2 * time.Minute)}, now.Add(2 * time.Minute)},
		"same incident equal":        {&Request{IncidentID: "inc", DeadlineAt: proposed}, proposed},
		"same incident later":        {&Request{IncidentID: "inc", DeadlineAt: now.Add(time.Hour)}, proposed},
		"same incident expired":      {&Request{IncidentID: "inc", DeadlineAt: now.Add(-time.Second)}, now.Add(-time.Second)},
		"same incident expiring now": {&Request{IncidentID: "inc", DeadlineAt: now}, now},
		"same incident long expired": {&Request{IncidentID: "inc", DeadlineAt: now.Add(-time.Hour)}, now.Add(-time.Hour)},
		"other incident earlier":     {&Request{IncidentID: "other", DeadlineAt: now.Add(time.Minute)}, proposed},
		"other incident expired":     {&Request{IncidentID: "other", DeadlineAt: now.Add(-time.Minute)}, proposed},
		"empty incident":             {&Request{DeadlineAt: now.Add(time.Minute)}, proposed},
	} {
		t.Run(name, func(t *testing.T) {
			got := ResolveDeadline(proposed, "inc", tc.existing)
			if !got.Equal(tc.want) {
				t.Fatalf("ResolveDeadline = %v, want %v", got, tc.want)
			}
			if got.After(proposed) {
				t.Fatalf("ResolveDeadline = %v, later than the policy-proposed %v", got, proposed)
			}
			// An expired same-incident stamp resolves to a deadline Classify
			// already treats as over, so the restarted step disrupts at once.
			if tc.existing != nil && tc.existing.IncidentID == "inc" && !tc.existing.DeadlineAt.After(now) {
				if outcome := Classify(now, Request{IncidentID: "inc", DeadlineAt: got}, Workload{}, Observation{Found: true}); outcome != OutcomeExpired {
					t.Fatalf("expired stamp resolved to %v, which classifies as %q, want expired", got, outcome)
				}
			}
		})
	}
	// A malformed stamp never reaches ResolveDeadline: ParseRequest refuses it,
	// and the caller then proposes afresh.
	if _, ok := ParseRequest(map[string]string{AnnotationIncident: "inc", AnnotationDeadlineAt: "garbage"}); ok {
		t.Fatal("a malformed stamp must not parse into a reusable request")
	}
}

func TestClassifyCoversEveryOutcome(t *testing.T) {
	now := time.Date(2026, 8, 5, 11, 0, 0, 0, time.UTC)
	future := now.Add(time.Minute)
	past := now.Add(-time.Minute)
	requested := Workload{Namespace: "training", Name: "trainer-0", UID: "uid-1"}
	ack := StateCompleteFor("inc-1")
	// A live object carries the stamp of the request in force, which is what
	// the workload read and answered; alive observes it with the given state.
	stamp := Request{IncidentID: "inc-1", RequestedAt: past, DeadlineAt: future}.Annotations()
	alive := func(state string) Observation {
		ann := map[string]string{}
		for k, v := range stamp {
			ann[k] = v
		}
		if state != "" {
			ann[AnnotationState] = state
		}
		return Observation{Found: true, UID: "uid-1", Annotations: ann}
	}
	// restamped observes an object another incident has since stamped, with
	// the state left behind from before.
	restamped := func(incident, state string) Observation {
		obs := alive(state)
		obs.Annotations[AnnotationIncident] = incident
		return obs
	}
	unstamped := func(state string) Observation {
		return Observation{Found: true, UID: "uid-1", Annotations: map[string]string{AnnotationState: state}}
	}
	malformed := func(state string) Observation {
		obs := alive(state)
		obs.Annotations[AnnotationDeadlineAt] = "not-a-time"
		return obs
	}
	for name, tc := range map[string]struct {
		deadline time.Time
		obs      Observation
		want     Outcome
	}{
		"pending":                       {future, alive(""), OutcomePending},
		"expired":                       {past, alive(""), OutcomeExpired},
		"expired exactly at deadline":   {now, alive(""), OutcomeExpired},
		"acknowledged":                  {future, alive(ack), OutcomeAcknowledged},
		"acknowledged late still wins":  {past, alive(ack), OutcomeAcknowledged},
		"in-progress is not an ack":     {future, alive("in-progress"), OutcomePending},
		"in-progress then expiry":       {past, alive("in-progress"), OutcomeExpired},
		"Complete is not complete":      {future, alive("Complete:inc-1"), OutcomePending},
		"plain complete is unbound":     {future, alive("complete"), OutcomePending},
		"bare prefix is unbound":        {future, alive(StateCompletePrefix), OutcomePending},
		"another incident's ack":        {future, alive(StateCompleteFor("inc-0")), OutcomePending},
		"another incident's ack expiry": {past, alive(StateCompleteFor("inc-0")), OutcomeExpired},
		"padded ack is not exact":       {future, alive(" " + ack), OutcomePending},
		// A retained answer on an object whose live request is no longer the
		// one in force answers nothing: the question changed under it.
		"ack under another incident's live stamp":  {future, restamped("inc-2", ack), OutcomePending},
		"ack under another incident's stamp, late": {past, restamped("inc-2", ack), OutcomeExpired},
		"ack with no live stamp":                   {future, unstamped(ack), OutcomePending},
		"ack with a malformed live stamp":          {future, malformed(ack), OutcomePending},
		"gone":                                     {future, Observation{Found: false}, OutcomeExited},
		"terminal":                                 {future, Observation{Found: true, UID: "uid-1", Terminal: true}, OutcomeExited},
		"replaced by a new object":                 {future, Observation{Found: true, UID: "uid-2", Annotations: map[string]string{AnnotationState: ack}}, OutcomeExited},
		"unreachable before deadline":              {future, Observation{Err: errors.New("apiserver"), Found: true}, OutcomeUnreachable},
		"unreachable after deadline":               {past, Observation{Err: errors.New("apiserver"), Found: true}, OutcomeUnreachable},
		"unreachable beats ack":                    {future, Observation{Err: errors.New("x"), Found: true, UID: "uid-1", Annotations: map[string]string{AnnotationState: ack}}, OutcomeUnreachable},
	} {
		t.Run(name, func(t *testing.T) {
			if got := Classify(now, Request{IncidentID: "inc-1", DeadlineAt: tc.deadline}, requested, tc.obs); got != tc.want {
				t.Fatalf("Classify = %q, want %q", got, tc.want)
			}
		})
	}
	// Without a UID on either side there is nothing to compare, so a live
	// observation is judged on its state alone.
	if got := Classify(now, Request{IncidentID: "inc-1", DeadlineAt: future}, Workload{}, Observation{Found: true, UID: "anything"}); got != OutcomePending {
		t.Fatalf("Classify without requested UID = %q, want pending", got)
	}
	// A request that names no incident can never be acknowledged: there is
	// nothing for an acknowledgement to be bound to.
	if got := Classify(now, Request{DeadlineAt: future}, requested, alive(StateCompletePrefix)); got != OutcomePending {
		t.Fatalf("Classify with no incident = %q, want pending", got)
	}
	// Only the incident of the live stamp is compared, never its timestamps:
	// a resumed request keeps the original request time and may have had its
	// deadline capped, and is still the one request.
	capped := Request{IncidentID: "inc-1", RequestedAt: now, DeadlineAt: now.Add(30 * time.Second)}
	if got := Classify(now, capped, requested, alive(ack)); got != OutcomeAcknowledged {
		t.Fatalf("Classify with a capped in-force deadline = %q, want acknowledged", got)
	}
}

// AcknowledgesInForce binds an answer to the question still on the object: the
// live stamp must parse, name the in-force incident, and carry that incident's
// exact acknowledgement. Anything less fails closed.
func TestAcknowledgesInForceRequiresTheLiveStampToMatch(t *testing.T) {
	now := time.Date(2026, 8, 5, 11, 0, 0, 0, time.UTC)
	inForce := Request{IncidentID: "inc-1", RequestedAt: now, DeadlineAt: now.Add(time.Minute)}
	with := func(req Request, extra map[string]string) map[string]string {
		ann := req.Annotations()
		for k, v := range extra {
			ann[k] = v
		}
		return ann
	}
	ack := map[string]string{AnnotationState: StateCompleteFor("inc-1")}
	for name, tc := range map[string]struct {
		annotations map[string]string
		want        bool
	}{
		"same incident, bound ack":            {with(inForce, ack), true},
		"same incident, other timestamps":     {with(Request{IncidentID: "inc-1", RequestedAt: now.Add(-time.Hour), DeadlineAt: now.Add(time.Hour)}, ack), true},
		"same incident, plain complete":       {with(inForce, map[string]string{AnnotationState: "complete"}), false},
		"same incident, another's ack":        {with(inForce, map[string]string{AnnotationState: StateCompleteFor("inc-2")}), false},
		"same incident, no state":             {with(inForce, nil), false},
		"another incident's live stamp":       {with(Request{IncidentID: "inc-2", DeadlineAt: now.Add(time.Hour)}, ack), false},
		"no live stamp":                       {ack, false},
		"nil annotations":                     {nil, false},
		"malformed deadline":                  {with(inForce, map[string]string{AnnotationState: StateCompleteFor("inc-1"), AnnotationDeadlineAt: "soon"}), false},
		"missing incident on the live object": {with(inForce, map[string]string{AnnotationState: StateCompleteFor("inc-1"), AnnotationIncident: ""}), false},
	} {
		t.Run(name, func(t *testing.T) {
			if got := AcknowledgesInForce(inForce, tc.annotations); got != tc.want {
				t.Fatalf("AcknowledgesInForce = %v, want %v", got, tc.want)
			}
		})
	}
	if AcknowledgesInForce(Request{DeadlineAt: now}, with(Request{DeadlineAt: now}, ack)) {
		t.Fatal("an in-force request naming no incident must never be acknowledged")
	}
}

// The acknowledgement is bound to the incident that asked. It is the exact
// value StateCompleteFor renders for the checkpoint-incident annotation the
// workload read, so the same incident recognizes it after a controller restart
// and no other incident ever does.
func TestAcknowledgesIsBoundToTheIncident(t *testing.T) {
	if got := StateCompleteFor("inc-8f2a"); got != "complete:inc-8f2a" {
		t.Fatalf("StateCompleteFor = %q, want complete:inc-8f2a", got)
	}
	for name, tc := range map[string]struct {
		state, incident string
		want            bool
	}{
		"bound to the incident":       {"complete:inc-1", "inc-1", true},
		"bound to another incident":   {"complete:inc-0", "inc-1", false},
		"plain complete":              {"complete", "inc-1", false},
		"bare prefix":                 {"complete:", "inc-1", false},
		"case differs":                {"Complete:inc-1", "inc-1", false},
		"padded":                      {"complete:inc-1 ", "inc-1", false},
		"prefix of the incident":      {"complete:inc", "inc-1", false},
		"incident with a suffix":      {"complete:inc-10", "inc-1", false},
		"in-progress":                 {"in-progress", "inc-1", false},
		"empty state":                 {"", "inc-1", false},
		"no incident to bind to":      {"complete:", "", false},
		"no incident, plain":          {"complete", "", false},
		"round trip through renderer": {StateCompleteFor("inc-1"), "inc-1", true},
	} {
		t.Run(name, func(t *testing.T) {
			if got := Acknowledges(tc.state, tc.incident); got != tc.want {
				t.Fatalf("Acknowledges(%q, %q) = %v, want %v", tc.state, tc.incident, got, tc.want)
			}
		})
	}
}

func TestOutcomeSettled(t *testing.T) {
	for outcome, want := range map[Outcome]bool{
		OutcomeAcknowledged: true, OutcomeExited: true, OutcomeExpired: true, OutcomeSkipped: true,
		OutcomePending: false, OutcomeUnreachable: false, Outcome("bogus"): false,
	} {
		if got := outcome.Settled(); got != want {
			t.Errorf("%q.Settled() = %v, want %v", outcome, got, want)
		}
	}
}

// The default skip list names exactly the device-dead classes, and each call
// hands out its own slice so no caller can edit the default for another.
func TestDefaultSkipClassesAreTheDeviceDeadClassesAndNotShared(t *testing.T) {
	want := []types.ProblemClass{types.ClassFellOffBus, types.ClassGPULost}
	got := DefaultSkipClasses()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("DefaultSkipClasses() = %v, want %v", got, want)
	}
	got[0] = "mutated"
	if again := DefaultSkipClasses(); !reflect.DeepEqual(again, want) {
		t.Fatalf("DefaultSkipClasses() shares memory across calls: %v", again)
	}
	p := Policy{Enabled: true, DefaultWait: DefaultWait, MaxWait: DefaultMaxWait,
		SkipClasses: DefaultSkipClasses(), Namespaces: []string{"training"}}
	if err := p.Validate(); err != nil {
		t.Fatalf("a policy built from the defaults must validate: %v", err)
	}
	for _, class := range want {
		if got := p.Eligibility(optedIn("training", nil), class); got != IneligibleClassSkipped {
			t.Errorf("Eligibility(%q) = %q, want %q", class, got, IneligibleClassSkipped)
		}
	}
	if got := p.Eligibility(optedIn("training", nil), types.ClassECCDBE); got != "" {
		t.Errorf("Eligibility(ecc-dbe) under defaults = %q, want eligible", got)
	}
}

// The policy is read from many goroutines through a shared snapshot; none of
// its methods may write to it.
func TestPolicyMethodsNeverMutate(t *testing.T) {
	p := enabledPolicy()
	before := p.Clone()
	w := optedIn("training", map[string]string{AnnotationMaxWait: "1h"})
	done := make(chan struct{})
	for i := 0; i < 8; i++ {
		go func() {
			defer func() { done <- struct{}{} }()
			_ = p.Validate()
			_ = p.WaitFor(w)
			_ = p.SharedWait([]Workload{w, w}, time.Minute)
			_ = p.Eligibility(w, types.ClassECCDBE)
			_ = p.Eligible([]Workload{w}, types.ClassECCDBE)
			_ = p.Clone()
		}()
	}
	for i := 0; i < 8; i++ {
		<-done
	}
	if !reflect.DeepEqual(p.Clone(), before) {
		t.Fatalf("policy changed under read-only use: %+v vs %+v", p, before)
	}
}
