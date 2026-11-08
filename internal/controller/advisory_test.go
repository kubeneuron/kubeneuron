package controller

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/kubeneuron/kubeneuron/internal/checkpoint"
	"github.com/kubeneuron/kubeneuron/internal/metrics"
	"github.com/kubeneuron/kubeneuron/internal/platform"
	"github.com/kubeneuron/kubeneuron/internal/playbook"
	"github.com/kubeneuron/kubeneuron/pkg/types"
)

// gatedPlaybook parks at its first rung and drains afterwards: the shape an
// advisory exists for.
func gatedPlaybook() *playbook.Playbook {
	return &playbook.Playbook{Name: "gated", Target: "node", Steps: []playbook.Step{
		{Name: "cordon", Action: "platform.cordon", Approval: "required"},
		{Name: "drain", Action: "platform.drain"},
	}}
}

// advisoryPolicy coordinates for every class, so the fixture incident's
// fell-off-bus (a class the default policy skips, and one the store keeps
// immutable after creation) is advised. The skipped-class case opts back
// into the default skip list explicitly.
func advisoryPolicy() checkpoint.Policy {
	p := enabledPolicy()
	p.SkipClasses = nil
	p.Advisory = checkpoint.AdvisoryPolicy{Enabled: true, Budget: checkpoint.DefaultAdvisoryBudget}
	return p
}

// advisoryFixture is checkpointFixture around a gated playbook: an
// EVALUATING incident of a coordinated class whose first rung needs approval.
func advisoryFixture(t *testing.T, book *playbook.Playbook, plat platform.Platform, policy checkpoint.Policy) (*Controller, *types.Incident, *fakeClock) {
	t.Helper()
	c, _, inc := protectionFixture(t, book, nil, plat, nil)
	c.mutateRuntimeConfig(func(rc *RuntimeConfig) { rc.Checkpoint = policy })
	clock := &fakeClock{now: time.Date(2026, 11, 5, 9, 0, 0, 0, time.UTC)}
	c.checkpointNow = clock.Now
	c.checkpointPoll = time.Millisecond
	return c, inc, clock
}

func (p *checkpointPlatform) advisories() []checkpoint.Advisory {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]checkpoint.Advisory(nil), p.advised...)
}

func (p *checkpointPlatform) withdrawals() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.withdrawn...)
}

type advisoryCounts map[checkpoint.AdvisoryOutcome]float64

func snapshotAdvisories() advisoryCounts {
	out := advisoryCounts{}
	for _, o := range checkpoint.AdvisoryOutcomes() {
		out[o] = testutil.ToFloat64(metrics.CheckpointAdvisories.WithLabelValues(string(o)))
	}
	return out
}

func assertAdvisories(t *testing.T, before advisoryCounts, want advisoryCounts) {
	t.Helper()
	after := snapshotAdvisories()
	for _, o := range checkpoint.AdvisoryOutcomes() {
		if delta := after[o] - before[o]; delta != want[o] {
			t.Fatalf("advisories[%s] moved by %v, want %v", o, delta, want[o])
		}
	}
}

// eventually polls cond for up to five seconds: reconcile may hand an
// incident to a worker, so what it did is observed, not assumed.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func park(t *testing.T, c *Controller, inc *types.Incident) *types.Incident {
	t.Helper()
	if err := c.advanceEvaluating(context.Background(), inc); err != nil {
		t.Fatal(err)
	}
	fresh, err := c.store.GetIncident(context.Background(), inc.ID)
	if err != nil {
		t.Fatal(err)
	}
	if fresh.State != types.StateAwaitingApproval {
		t.Fatalf("state = %s, want AWAITING_APPROVAL", fresh.State)
	}
	return fresh
}

// A park with a drain downstream advises every eligible workload once, with
// the round, the class and the disruption that may follow, and audits one
// row naming them. Nothing is requested and nothing is waited for.
func TestAdvisoryStampsEligibleWorkloadsAtPark(t *testing.T) {
	plat := &checkpointPlatform{basePlatform: basePlatform{workloads: []platform.Workload{
		optedIn("trainer-a", true),
		optedIn("trainer-b", false),
		{Name: "bystander", Namespace: "training", Kind: "Pod", UID: "uid-bystander"},
		{Name: "elsewhere", Namespace: "research", Kind: "Pod", UID: "uid-elsewhere",
			Annotations: map[string]string{checkpoint.AnnotationOptIn: checkpoint.OptInValue}},
	}}}
	c, inc, clock := advisoryFixture(t, gatedPlaybook(), plat, advisoryPolicy())
	before := snapshotAdvisories()

	fresh := park(t, c, inc)
	if fresh.ApprovalEpoch != 1 {
		t.Fatalf("epoch = %d, want 1", fresh.ApprovalEpoch)
	}
	got := plat.advisories()
	if len(got) != 2 {
		t.Fatalf("advised %d workloads, want the two opted-in ones in the allowlisted namespace: %+v", len(got), got)
	}
	for _, adv := range got {
		if adv.IncidentID != inc.ID || adv.Round != 1 || adv.Reason != string(types.ClassFellOffBus) || adv.NextAction != "platform.drain" || !adv.AdvisedAt.Equal(clock.Now()) {
			t.Fatalf("advisory = %+v, want round 1 of %s, class fell-off-bus, before platform.drain at the fixture clock", adv, inc.ID)
		}
	}
	for _, call := range plat.Calls() {
		if strings.HasPrefix(call, "request ") || strings.HasPrefix(call, "observe ") {
			t.Fatalf("an advisory round must not request or observe: %v", plat.Calls())
		}
	}
	assertAdvisories(t, before, advisoryCounts{checkpoint.AdvisoryStamped: 2})
	results := auditResults(t, c, inc)
	requireAudit(t, results, "round 1: advised 2 of 4 workload(s) training/trainer-a, training/trainer-b before platform.drain")
	trail, _ := c.store.AuditTrail(context.Background(), inc.ID)
	var found bool
	for _, e := range trail {
		if e.Action == auditActionAdvisory {
			found = true
			if e.Actor != "system" {
				t.Fatalf("advisory row actor = %q, want system", e.Actor)
			}
		}
	}
	if !found {
		t.Fatal("no audit row carries the checkpoint-advisory action")
	}
}

// A round that ends without its disruption withdraws the notices it
// stamped: a rejection here. Only this round's notices, only on the
// incident's node, never a workload-owned key.
func TestAdvisoryIsWithdrawnWhenTheRoundIsRejected(t *testing.T) {
	trainer := optedIn("trainer", true)
	plat := &checkpointPlatform{basePlatform: basePlatform{workloads: []platform.Workload{trainer}}}
	c, inc, _ := advisoryFixture(t, gatedPlaybook(), plat, advisoryPolicy())
	ctx := context.Background()
	fresh := park(t, c, inc)

	// The live listing now carries the stamp, as the object would.
	stamped := plat.advisories()[0]
	plat.mu.Lock()
	for k, v := range stamped.Annotations() {
		plat.workloads[0].Annotations[k] = v
	}
	plat.mu.Unlock()
	before := snapshotAdvisories()

	if err := c.DecideApproval(ctx, inc.ID, "alice", "cli", types.ApprovalRejected, fresh.ApprovalEpoch, "not now", ""); err != nil {
		t.Fatal(err)
	}
	c.reconcile(ctx)
	eventually(t, "NEEDS_HUMAN after the rejection", func() bool {
		after, err := c.store.GetIncident(ctx, inc.ID)
		return err == nil && after.State == types.StateNeedsHuman
	})
	eventually(t, "the withdrawal", func() bool { return len(plat.withdrawals()) == 1 })
	if got := plat.withdrawals(); got[0] != "training/trainer" {
		t.Fatalf("withdrawals = %v, want the one stamped workload", got)
	}
	eventually(t, "the withdrawal audit row", func() bool {
		for _, r := range auditResults(t, c, inc) {
			if strings.Contains(r, "round 1: withdrew 1 advisory notice(s) training/trainer") {
				return true
			}
		}
		return false
	})
	assertAdvisories(t, before, advisoryCounts{checkpoint.AdvisoryWithdrawn: 1})
}

// An approval moves the round into its step's execution: the notice stands
// (the request with a deadline follows at the drain), so nothing is withdrawn.
func TestAdvisoryStandsWhenTheRoundIsApproved(t *testing.T) {
	trainer := optedIn("trainer", true)
	plat := &checkpointPlatform{basePlatform: basePlatform{workloads: []platform.Workload{trainer}}}
	c, inc, _ := advisoryFixture(t, gatedPlaybook(), plat, advisoryPolicy())
	ctx := context.Background()
	fresh := park(t, c, inc)
	stamped := plat.advisories()[0]
	plat.mu.Lock()
	for k, v := range stamped.Annotations() {
		plat.workloads[0].Annotations[k] = v
	}
	plat.mu.Unlock()

	if err := c.DecideApproval(ctx, inc.ID, "alice", "cli", types.ApprovalApproved, fresh.ApprovalEpoch, "", ""); err != nil {
		t.Fatal(err)
	}
	c.reconcile(ctx)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		after, err := c.store.GetIncident(ctx, inc.ID)
		if err != nil {
			t.Fatal(err)
		}
		if after.State != types.StateAwaitingApproval {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got := plat.withdrawals(); len(got) != 0 {
		t.Fatalf("an approved round withdrew its notice: %v", got)
	}
}

// A re-park is a new round and a new stamp; the same round twice is one.
func TestAdvisoryIsOncePerRound(t *testing.T) {
	trainer := optedIn("trainer", true)
	plat := &checkpointPlatform{basePlatform: basePlatform{workloads: []platform.Workload{trainer}}}
	c, inc, _ := advisoryFixture(t, gatedPlaybook(), plat, advisoryPolicy())
	ctx := context.Background()
	fresh := park(t, c, inc)
	if n := len(plat.advisories()); n != 1 {
		t.Fatalf("advised %d, want 1", n)
	}
	// Ticks while parked and undecided advise nothing more.
	c.reconcile(ctx)
	c.reconcile(ctx)
	if n := len(plat.advisories()); n != 1 {
		t.Fatalf("advised %d after idle ticks, want still 1", n)
	}
	// A decision bound to an older round re-parks into round 2.
	if err := c.DecideApproval(ctx, inc.ID, "alice", "cli", types.ApprovalApproved, fresh.ApprovalEpoch, "", ""); err != nil {
		t.Fatal(err)
	}
	swapped := &playbook.Playbook{Name: "gated", Target: "node", Steps: []playbook.Step{
		{Name: "cordon", Action: "platform.cordon", Approval: "required", Params: map[string]string{"note": "edited"}},
		{Name: "drain", Action: "platform.drain"},
	}}
	engine, err := playbook.NewEngine(map[string]*playbook.Playbook{"gated": swapped}, nil)
	if err != nil {
		t.Fatal(err)
	}
	c.SetEngine(engine)
	c.reconcile(ctx)
	eventually(t, "the re-park into round 2", func() bool {
		after, err := c.store.GetIncident(ctx, inc.ID)
		return err == nil && after.State == types.StateAwaitingApproval && after.ApprovalEpoch == 2
	})
	eventually(t, "the round 2 stamp", func() bool { return len(plat.advisories()) == 2 })
	if got := plat.advisories(); got[1].Round != 2 {
		t.Fatalf("advisories = %+v, want a second stamp for round 2", got)
	}
}

// What a round skips, and says so: a dry run, a skipped class, a playbook
// with no disruption downstream, a platform that cannot stamp, a node with
// no eligible workload, and a listing that fails. Each is one skipped count
// and one audit row; none stamps.
func TestAdvisorySkipsAreCountedAndAudited(t *testing.T) {
	rebootOnly := &playbook.Playbook{Name: "gated", Target: "node", Steps: []playbook.Step{
		{Name: "reboot", Action: "agent.reboot", Approval: "required"},
	}}
	for name, tc := range map[string]struct {
		book   *playbook.Playbook
		plat   platform.Platform
		policy checkpoint.Policy
		dryRun bool
		want   string
	}{
		"dry run": {
			book: gatedPlaybook(), dryRun: true,
			plat: &checkpointPlatform{basePlatform: basePlatform{workloads: []platform.Workload{optedIn("trainer", true)}}},
			want: "dry-run: would have advised 1 of 1 workload(s) training/trainer before platform.drain",
		},
		"skipped class": {
			book: gatedPlaybook(),
			plat: &checkpointPlatform{basePlatform: basePlatform{workloads: []platform.Workload{optedIn("trainer", true)}}},
			policy: func() checkpoint.Policy {
				p := advisoryPolicy()
				p.SkipClasses = checkpoint.DefaultSkipClasses()
				return p
			}(),
			want: "skipped (class-skipped: fell-off-bus)",
		},
		"no disruption downstream": {
			book: rebootOnly,
			plat: &checkpointPlatform{basePlatform: basePlatform{workloads: []platform.Workload{optedIn("trainer", true)}}},
			want: "skipped (no drain or GPU-workload eviction downstream of the parked step)",
		},
		"platform without the capability": {
			book: gatedPlaybook(),
			plat: &basePlatform{workloads: []platform.Workload{optedIn("trainer", true)}},
			want: "does not support checkpoint coordination",
		},
		"nothing eligible": {
			book: gatedPlaybook(),
			plat: &checkpointPlatform{basePlatform: basePlatform{workloads: []platform.Workload{
				{Name: "bystander", Namespace: "training", Kind: "Pod", UID: "uid-b"},
			}}},
			want: "skipped (no eligible workload among 1 listed on n1)",
		},
		"listing failed": {
			book: gatedPlaybook(),
			plat: &checkpointPlatform{basePlatform: basePlatform{listErr: errors.New("apiserver down")}},
			want: "skipped (workload listing failed)",
		},
	} {
		t.Run(name, func(t *testing.T) {
			policy := tc.policy
			if !policy.Enabled {
				policy = advisoryPolicy()
			}
			c, inc, _ := advisoryFixture(t, tc.book, tc.plat, policy)
			inc.DryRun = tc.dryRun
			if err := c.store.UpdateIncident(context.Background(), inc); err != nil {
				t.Fatal(err)
			}
			before := snapshotAdvisories()
			park(t, c, inc)
			assertAdvisories(t, before, advisoryCounts{checkpoint.AdvisorySkipped: 1})
			requireAudit(t, auditResults(t, c, inc), tc.want)
			if cp, ok := tc.plat.(*checkpointPlatform); ok && len(cp.advisories()) != 0 {
				t.Fatalf("a skipped round stamped: %+v", cp.advisories())
			}
		})
	}
}

// Off is off: with the advisory disabled, or coordination disabled, a park
// neither lists nor stamps nor audits anything about it.
func TestAdvisoryIsSilentWhenDisabled(t *testing.T) {
	for name, policy := range map[string]checkpoint.Policy{
		"advisory off":     enabledPolicy(),
		"coordination off": {},
	} {
		t.Run(name, func(t *testing.T) {
			plat := &checkpointPlatform{basePlatform: basePlatform{workloads: []platform.Workload{optedIn("trainer", true)}}}
			c, inc, _ := advisoryFixture(t, gatedPlaybook(), plat, policy)
			before := snapshotAdvisories()
			park(t, c, inc)
			assertAdvisories(t, before, advisoryCounts{})
			for _, call := range plat.Calls() {
				if strings.HasPrefix(call, "list ") || strings.HasPrefix(call, "advise ") {
					t.Fatalf("a disabled advisory touched the platform: %v", plat.Calls())
				}
			}
			for _, r := range auditResults(t, c, inc) {
				if strings.Contains(r, "advis") {
					t.Fatalf("a disabled advisory wrote an audit row: %q", r)
				}
			}
		})
	}
}

// A stamp that fails is unreachable, counted, and does not stop the others;
// the park has already happened and the human is notified regardless.
func TestAdvisoryUnreachableIsCountedAndNeverBlocksThePark(t *testing.T) {
	plat := &checkpointPlatform{basePlatform: basePlatform{workloads: []platform.Workload{
		optedIn("trainer-a", true), optedIn("trainer-b", true),
	}}}
	plat.advise = func(w platform.Workload, _ checkpoint.Advisory) error {
		if w.Name == "trainer-a" {
			return errors.New("patch refused")
		}
		return nil
	}
	c, inc, _ := advisoryFixture(t, gatedPlaybook(), plat, advisoryPolicy())
	before := snapshotAdvisories()
	park(t, c, inc)
	assertAdvisories(t, before, advisoryCounts{checkpoint.AdvisoryStamped: 1, checkpoint.AdvisoryUnreachable: 1})
	requireAudit(t, auditResults(t, c, inc), "round 1: advised 1 of 2 workload(s) training/trainer-b before platform.drain; 1 unreachable training/trainer-a")
	notifier := c.notifier.(*recordingNotifier)
	notifier.mu.Lock()
	defer notifier.mu.Unlock()
	if len(notifier.approvals) != 1 {
		t.Fatalf("approval notifications = %v, want exactly one", notifier.approvals)
	}
}

// The round's budget is the whole of what it may spend: a platform that
// hangs on the stamp costs the budget, not the park, and the stamp is
// counted unreachable.
func TestAdvisoryIsBoundedByItsBudget(t *testing.T) {
	plat := &checkpointPlatform{basePlatform: basePlatform{
		workloads: []platform.Workload{optedIn("trainer", true)},
		blockOn:   map[string]bool{"advise": true},
	}}
	policy := advisoryPolicy()
	policy.Advisory.Budget = 50 * time.Millisecond
	c, inc, _ := advisoryFixture(t, gatedPlaybook(), plat, policy)
	before := snapshotAdvisories()
	start := time.Now()
	park(t, c, inc)
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("the park took %v; the advisory budget was 50ms", elapsed)
	}
	assertAdvisories(t, before, advisoryCounts{checkpoint.AdvisoryUnreachable: 1})
	requireAudit(t, auditResults(t, c, inc), "1 unreachable training/trainer")
}

// A withdrawal is best effort and bounded: a listing that fails is counted
// unreachable and the state change it follows has already happened.
func TestAdvisoryWithdrawalFailureIsCountedNotFatal(t *testing.T) {
	plat := &checkpointPlatform{basePlatform: basePlatform{workloads: []platform.Workload{optedIn("trainer", true)}}}
	c, inc, _ := advisoryFixture(t, gatedPlaybook(), plat, advisoryPolicy())
	ctx := context.Background()
	fresh := park(t, c, inc)
	plat.mu.Lock()
	plat.listErr = errors.New("apiserver down")
	plat.mu.Unlock()
	before := snapshotAdvisories()
	if err := c.DecideApproval(ctx, inc.ID, "alice", "cli", types.ApprovalRejected, fresh.ApprovalEpoch, "", ""); err != nil {
		t.Fatal(err)
	}
	c.reconcile(ctx)
	eventually(t, "NEEDS_HUMAN regardless of the withdrawal", func() bool {
		after, err := c.store.GetIncident(ctx, inc.ID)
		return err == nil && after.State == types.StateNeedsHuman
	})
	eventually(t, "the unreachable count", func() bool {
		return snapshotAdvisories()[checkpoint.AdvisoryUnreachable]-before[checkpoint.AdvisoryUnreachable] == 1
	})
}
