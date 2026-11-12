// SPDX-License-Identifier: LicenseRef-KubeNeuron

package controller

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/kubeneuron/kubeneuron/internal/checkpoint"
	"github.com/kubeneuron/kubeneuron/internal/platform"
	"github.com/kubeneuron/kubeneuron/internal/playbook"
	"github.com/kubeneuron/kubeneuron/internal/safety"
	"github.com/kubeneuron/kubeneuron/internal/store"
	"github.com/kubeneuron/kubeneuron/internal/store/sqlite"
	"github.com/kubeneuron/kubeneuron/pkg/types"
)

// Regressions from the v0.8.0 pre-tag review (2026-11-05): each test below
// reproduced a defect before its fix and pins the corrected behaviour.

// An installation switched to DryRun after an incident opened Enabled must
// stamp nothing on a workload: the advisory reads the live gate the way
// step execution does, not only the stamp the incident was opened with.
func TestAdvisoryHonoursTheRuntimeDryRunSwitch(t *testing.T) {
	plat := &checkpointPlatform{basePlatform: basePlatform{workloads: []platform.Workload{optedIn("trainer", true)}}}
	c, inc, _ := advisoryFixture(t, gatedPlaybook(), plat, advisoryPolicy())
	if inc.DryRun {
		t.Fatal("fixture incident must be opened Enabled")
	}
	dry := safety.Limits{MaxConcurrentRemediations: 4, MaxConcurrentReboots: 1, DryRun: true}
	c.mutateRuntimeConfig(func(rc *RuntimeConfig) { rc.SafetyLimits = &dry })
	before := snapshotAdvisories()

	park(t, c, inc)
	if got := plat.advisories(); len(got) != 0 {
		t.Fatalf("a DryRun installation stamped %d advisory notice(s): %+v", len(got), got)
	}
	assertAdvisories(t, before, advisoryCounts{checkpoint.AdvisorySkipped: 1})
	requireAudit(t, auditResults(t, c, inc), "dry-run: would have advised 1 of 1 workload(s) training/trainer before platform.drain")
}

// A transition that lost its conflict check never happened: the notice it
// would have ended still stands and nothing is withdrawn.
func TestFailedTransitionWithdrawsNothing(t *testing.T) {
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

	// Somebody else moved the row's fence after this goroutine's snapshot.
	moved, err := c.store.GetIncident(ctx, inc.ID)
	if err != nil {
		t.Fatal(err)
	}
	moved.StateChangedAt = moved.StateChangedAt.Add(time.Second)
	if err := c.store.UpdateIncident(ctx, moved); err != nil {
		t.Fatal(err)
	}
	err = c.transition(ctx, fresh, types.StateNeedsHuman, "alice", "manual-resolve", "racing", nil)
	if !errors.Is(err, store.ErrConflict) {
		t.Fatalf("transition err = %v, want ErrConflict", err)
	}
	time.Sleep(50 * time.Millisecond)
	if got := plat.withdrawals(); len(got) != 0 {
		t.Fatalf("a transition that never committed withdrew the notice: %v", got)
	}
	after, _ := c.store.GetIncident(ctx, inc.ID)
	if after.State != types.StateAwaitingApproval {
		t.Fatalf("state = %s, want the park untouched", after.State)
	}
}

// refusingPreviewStore is the fixture store with the preview table refusing
// writes on demand: what a recompute must survive without lying.
type refusingPreviewStore struct {
	*sqlite.Store
	refuse bool
}

func (s *refusingPreviewStore) PutImpactPreview(ctx context.Context, p *types.ImpactPreview) error {
	if s.refuse {
		return fmt.Errorf("disk full")
	}
	return s.Store.PutImpactPreview(ctx, p)
}

// A recompute key answers with the preview THAT key computed, not the
// round's newest; a recompute whose record the store refused fails without
// consuming its key, so the same key succeeds once the store is back.
func TestRecomputeIsIdempotentOnItsOwnResultAndNeverOnAFailedWrite(t *testing.T) {
	c, inc, plat, _, clock := impactFixtureClock(t, false)
	ctx := context.Background()
	wrapped := &refusingPreviewStore{Store: c.store.(*sqlite.Store)}
	c.store = wrapped
	park(t, c, inc)
	clock.Advance(time.Minute)

	a, replayed, err := c.RecomputeImpactPreview(ctx, inc.ID, "alice", "key-a")
	if err != nil || replayed {
		t.Fatalf("recompute A = %+v replayed=%v err=%v", a, replayed, err)
	}
	plat.mu.Lock()
	plat.workloads = plat.workloads[:1]
	plat.mu.Unlock()
	clock.Advance(time.Minute)
	b, replayed, err := c.RecomputeImpactPreview(ctx, inc.ID, "bob", "key-b")
	if err != nil || replayed || b.Digest == a.Digest {
		t.Fatalf("recompute B = %+v replayed=%v err=%v, want a new digest", b, replayed, err)
	}
	again, replayed, err := c.RecomputeImpactPreview(ctx, inc.ID, "alice", "key-a")
	if err != nil || !replayed || again.Digest != a.Digest {
		t.Fatalf("replay of A = %+v replayed=%v err=%v, want A's own digest %s (B is %s)", again, replayed, err, a.Digest, b.Digest)
	}
	latest, err := c.store.(store.ImpactPreviewStore).LatestImpactPreview(ctx, inc.ID, 1)
	if err != nil || latest.Digest != b.Digest {
		t.Fatalf("latest = %+v err=%v, want B current", latest, err)
	}

	wrapped.refuse = true
	clock.Advance(time.Minute)
	if _, _, err := c.RecomputeImpactPreview(ctx, inc.ID, "carol", "key-c"); !errors.Is(err, ErrImpactPreviewUnavailable) {
		t.Fatalf("recompute with a refusing store err = %v, want ErrImpactPreviewUnavailable", err)
	}
	if latest, _ := c.store.(store.ImpactPreviewStore).LatestImpactPreview(ctx, inc.ID, 1); latest.Digest != b.Digest {
		t.Fatalf("a refused write changed the current preview to %s", latest.Digest)
	}
	for _, r := range auditResults(t, c, inc) {
		if strings.Contains(r, "recompute requested") && !strings.Contains(r, a.Digest) && !strings.Contains(r, b.Digest) {
			t.Fatalf("a digest that was never stored was audited: %s", r)
		}
	}
	wrapped.refuse = false
	clock.Advance(time.Minute)
	cc, replayed, err := c.RecomputeImpactPreview(ctx, inc.ID, "carol", "key-c")
	if err != nil || replayed || cc.Digest == b.Digest {
		t.Fatalf("retry under the same key = %+v replayed=%v err=%v, want a fresh recompute", cc, replayed, err)
	}
}

// Once the coordination window has closed, the remaining groups are skipped
// in one row and nothing is listed: the audit of N groups cannot spend the
// reserve the disruption itself owns.
func TestGroupWalkStopsWhenTheWindowHasClosed(t *testing.T) {
	c, plat, _, _ := groupFixture(t, groupPolicy(true, 64))
	inc, err := c.store.GetIncident(context.Background(), fixtureIncidentID)
	if err != nil {
		t.Fatal(err)
	}
	candidates := []platform.Workload{
		grouped("a-0", "training", "n1", "run-a"), grouped("b-0", "training", "n1", "run-b"), grouped("c-0", "training", "n1", "run-c"),
	}
	closed, cancel := context.WithCancel(context.Background())
	cancel()
	before := groupCounts()
	started := time.Now()
	peers := c.groupPeers(closed, plat, groupPolicy(true, 64), inc, &playbook.Step{Name: "drain", Action: "platform.drain"}, candidates)
	if len(peers) != 0 {
		t.Fatalf("peers = %v, want none after the window closed", peers)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("the walk took %v after the window closed", elapsed)
	}
	for _, call := range plat.Calls() {
		if strings.HasPrefix(call, "group ") {
			t.Fatalf("a group was listed after the window closed: %v", plat.Calls())
		}
	}
	after := groupCounts()
	if after[checkpoint.OutcomeSkipped]-before[checkpoint.OutcomeSkipped] != 3 {
		t.Fatalf("skipped moved by %v, want 3", after[checkpoint.OutcomeSkipped]-before[checkpoint.OutcomeSkipped])
	}
	rows := 0
	for _, r := range auditResults(t, c, inc) {
		if strings.Contains(r, "skipped (coordination budget spent before the group was listed)") {
			rows++
			if !strings.Contains(r, "training/run-a, training/run-b, training/run-c") {
				t.Fatalf("the one skip row does not name every group: %s", r)
			}
		}
	}
	if rows != 1 {
		t.Fatalf("skip rows = %d, want exactly one for the three groups", rows)
	}
}

// Without a group listing the preview says the total is unknown; the
// on-node count is never shown as the group's size.
func TestPreviewSaysWhenAGroupTotalIsUnknown(t *testing.T) {
	plat := &checkpointPlatform{basePlatform: basePlatform{workloads: []platform.Workload{
		grouped("rank-0", "training", "n1", "run-42"), grouped("rank-1", "training", "n1", "run-42"),
	}}}
	plat.groups = func(string, string, int) ([]platform.Workload, bool, error) { return nil, false, fmt.Errorf("listing refused") }
	policy := advisoryPolicy()
	policy.Groups = checkpoint.GroupPolicy{Enabled: true, MaxGroupSize: 64}
	c, inc, _ := advisoryFixture(t, gatedPlaybook(), plat, policy)
	c.notifier = &contextNotifier{}
	c.mutateRuntimeConfig(func(rc *RuntimeConfig) { rc.Impact = impactPolicy(false) })
	fresh := park(t, c, inc)
	rec, err := c.store.(store.ImpactPreviewStore).LatestImpactPreview(context.Background(), inc.ID, fresh.ApprovalEpoch)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(rec.Payload), `"members_total_known":false`) || !strings.Contains(string(rec.Payload), `"GroupTotalUnknown"`) {
		t.Fatalf("preview does not say the total is unknown: %s", rec.Payload)
	}
}
