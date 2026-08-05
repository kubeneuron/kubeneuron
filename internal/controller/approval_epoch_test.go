package controller

// Round-8: the approval protocol is first-class rounds. Each park mints an
// epoch and its request record atomically; decisions bind to a round; a
// re-park orphans stale decisions by construction, not by timestamp
// comparison; and rounds without request records fail closed.

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/kubeneuron/kubeneuron/internal/playbook"
	"github.com/kubeneuron/kubeneuron/internal/store"
	"github.com/kubeneuron/kubeneuron/internal/store/sqlite"
	"github.com/kubeneuron/kubeneuron/pkg/types"
)

func approvalEpochFixture(t *testing.T) (*Controller, *sqlite.Store, string) {
	t.Helper()
	pb := &playbook.Playbook{
		Name: "pb", Target: "node",
		Steps: []playbook.Step{{Name: "reboot", Action: "agent.reboot", Approval: "required"}},
	}
	engine, err := playbook.NewEngine(map[string]*playbook.Playbook{"pb": pb}, nil)
	if err != nil {
		t.Fatal(err)
	}
	c, st, _ := miniController(t, engine)
	ctx := context.Background()
	if err := st.UpsertNode(ctx, &types.Node{Name: "n1"}); err != nil {
		t.Fatal(err)
	}
	inc := &types.Incident{
		ID: "inc-epoch", Target: types.Target{Node: "n1"},
		Class: types.ClassFellOffBus, State: types.StateEvaluating, Playbook: "pb",
		OpenedAt: time.Now(), UpdatedAt: time.Now(), StateChangedAt: time.Now(),
	}
	if err := st.CreateIncident(ctx, inc); err != nil {
		t.Fatal(err)
	}
	c.reconcile(ctx) // parks: epoch 1 + its request record, atomically
	return c, st, inc.ID
}

// A park mints its epoch and request record together: immediately after the
// park, the round is fully formed and decidable.
func TestParkMintsEpochAndRequestAtomically(t *testing.T) {
	_, st, id := approvalEpochFixture(t)
	ctx := context.Background()

	inc, err := st.GetIncident(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if inc.State != types.StateAwaitingApproval || inc.ApprovalEpoch != 1 {
		t.Fatalf("after park: state=%s epoch=%d, want AWAITING_APPROVAL at epoch 1", inc.State, inc.ApprovalEpoch)
	}
	request, err := st.GetApprovalRequest(ctx, id, inc.ApprovalEpoch)
	if err != nil {
		t.Fatalf("the round's request record must exist: %v", err)
	}
	if request.StepAction != "agent.reboot" || request.StepHash == "" || request.ParkEpoch != 1 {
		t.Fatalf("request = %+v, want full identity at epoch 1", request)
	}
}

// A decision belongs to its round: after a re-park (new epoch), the old
// round's approval is invisible to resume — orphaned by construction, with no
// timestamp ordering involved.
func TestReParkOrphansThePreviousRoundsDecision(t *testing.T) {
	c, st, id := approvalEpochFixture(t)
	ctx := context.Background()

	// Approve round 1 through the real API.
	if err := c.DecideApproval(ctx, id, "alice", "cli", types.ApprovalApproved, 0, ""); err != nil {
		t.Fatal(err)
	}
	// Before the walk resumes, the playbook hot-swaps: resume detects the
	// mismatch against round 1's request and re-parks into round 2.
	swapped := &playbook.Playbook{
		Name: "pb", Target: "node",
		Steps: []playbook.Step{{Name: "reboot", Action: "platform.replace_node", Approval: "required"}},
	}
	engine2, err := playbook.NewEngine(map[string]*playbook.Playbook{"pb": swapped}, nil)
	if err != nil {
		t.Fatal(err)
	}
	c.SetEngine(engine2)
	c.reconcile(ctx)

	inc, err := st.GetIncident(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if inc.State != types.StateAwaitingApproval || inc.ApprovalEpoch != 2 {
		t.Fatalf("after mismatch: state=%s epoch=%d, want a re-park into epoch 2", inc.State, inc.ApprovalEpoch)
	}
	// Round 1's approval must be invisible to round 2.
	if _, err := st.LatestApprovalDecision(ctx, id, inc.ApprovalEpoch); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("round 2 decision lookup = %v, want ErrNotFound — the stale approval is orphaned", err)
	}
	// And the walk must NOT execute anything on further passes.
	c.reconcile(ctx)
	still, err := st.GetIncident(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if still.State != types.StateAwaitingApproval || still.StepIndex != 0 {
		t.Fatalf("state=%s step=%d, want the incident still parked", still.State, still.StepIndex)
	}
	// Approving round 2 executes the step that round 2 asked about.
	if err := c.DecideApproval(ctx, id, "alice", "cli", types.ApprovalApproved, 0, ""); err != nil {
		t.Fatal(err)
	}
	decision, err := st.LatestApprovalDecision(ctx, id, 2)
	if err != nil || decision.StepAction != "platform.replace_node" {
		t.Fatalf("round-2 decision = %+v, %v; want it bound to the round-2 request", decision, err)
	}
}

// A park that predates approval rounds (epoch 0) fails closed in both
// directions — modelled with the TRUE legacy shape: the old code always wrote
// request rows, so a real pre-upgrade incident has requests AND orphaned
// decisions all defaulted to park_epoch 0. Pairing them naively could match
// a stale approval of one park against the request of a DIFFERENT park and
// execute a step the human never approved; epoch 0 must therefore never be
// consulted as a round at all.
func TestPreEpochParkFailsClosedIntoAFreshRound(t *testing.T) {
	c, st, id := approvalEpochFixture(t)
	ctx := context.Background()

	// Regress to the real pre-upgrade shape: parked at epoch 0, with a stale
	// APPROVED decision from an earlier park (for a different step/hash) AND
	// a later request row — all at park_epoch 0, exactly as the migration
	// defaults them.
	if _, err := st.SQL.ExecContext(ctx, `UPDATE incidents SET approval_epoch=0 WHERE id=?`, id); err != nil {
		t.Fatal(err)
	}
	if _, err := st.SQL.ExecContext(ctx, `UPDATE approvals SET park_epoch=0 WHERE incident_id=?`, id); err != nil {
		t.Fatal(err)
	}
	if err := st.RecordApproval(ctx, &types.Approval{
		IncidentID: id, StepName: "old-reset", Decision: types.ApprovalApproved,
		Actor: "legacy", Channel: "cli", At: time.Now().Add(-time.Hour),
		PlaybookName: "pb", StepAction: "agent.gpu_reset", StepHash: "stale-hash",
		ParkEpoch: 0,
	}); err != nil {
		t.Fatal(err)
	}

	// The decision API refuses: epoch 0 is not a verifiable round.
	if err := c.DecideApproval(ctx, id, "alice", "cli", types.ApprovalApproved, 0, ""); err == nil {
		t.Fatal("a pre-epoch park must not accept a decision")
	}
	// Resume must NOT pair the stale approval with any epoch-0 request: it
	// migrates the park into round 1 and executes nothing.
	c.reconcile(ctx)
	inc, err := st.GetIncident(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if inc.State != types.StateAwaitingApproval || inc.ApprovalEpoch != 1 || inc.StepIndex != 0 {
		t.Fatalf("state=%s epoch=%d step=%d, want a fresh round (epoch 1) with nothing executed", inc.State, inc.ApprovalEpoch, inc.StepIndex)
	}
	if _, err := st.GetApprovalRequest(ctx, id, 1); err != nil {
		t.Fatalf("the fresh round must have its request record: %v", err)
	}
	// Further passes stay parked: the stale epoch-0 approval never resumes
	// anything.
	c.reconcile(ctx)
	if still, _ := st.GetIncident(ctx, id); still.State != types.StateAwaitingApproval || still.StepIndex != 0 {
		t.Fatalf("state=%s step=%d, want still parked", still.State, still.StepIndex)
	}
}

// R10.1: the click carries the round the human SAW. A notification renders
// round N and its suggested command pins --round N; if a re-park mints round
// N+1 before the click lands, the decision must be refused — never silently
// recorded against content the human never read. Round 0 (older clients)
// keeps the bind-to-current behavior.
func TestDecisionPinnedToDisplayedRoundRefusedAfterRePark(t *testing.T) {
	c, st, id := approvalEpochFixture(t)
	ctx := context.Background()

	// A decision pinned to the wrong round is refused even before any
	// re-park: the client's view must match the current round exactly.
	if err := c.DecideApproval(ctx, id, "alice", "cli", types.ApprovalApproved, 7, ""); err == nil {
		t.Fatal("a decision pinned to a round that never existed must be refused")
	}
	// Pinned to the current round: accepted.
	if err := c.DecideApproval(ctx, id, "alice", "cli", types.ApprovalApproved, 1, ""); err != nil {
		t.Fatalf("decision pinned to the current round refused: %v", err)
	}

	// The playbook hot-swaps before resume; the walk detects the mismatch
	// against round 1's request and re-parks into round 2. The Slack message
	// for round 1 is now stale in someone's channel.
	swapped := &playbook.Playbook{
		Name: "pb", Target: "node",
		Steps: []playbook.Step{{Name: "reboot", Action: "platform.replace_node", Approval: "required"}},
	}
	engine2, err := playbook.NewEngine(map[string]*playbook.Playbook{"pb": swapped}, nil)
	if err != nil {
		t.Fatal(err)
	}
	c.SetEngine(engine2)
	c.reconcile(ctx)
	inc, err := st.GetIncident(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if inc.ApprovalEpoch != 2 {
		t.Fatalf("epoch = %d, want re-park into round 2", inc.ApprovalEpoch)
	}

	// The click from the stale round-1 message must bounce with a re-read
	// instruction, not bind to round 2.
	if err := c.DecideApproval(ctx, id, "bob", "slack", types.ApprovalApproved, 1, ""); err == nil {
		t.Fatal("a round-1 click after the re-park into round 2 must be refused")
	}
	if _, err := st.LatestApprovalDecision(ctx, id, 2); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("round 2 must have no decision after the refused stale click, got err=%v", err)
	}
	// A fresh read shows round 2; deciding it (pinned or legacy-unpinned)
	// works.
	if err := c.DecideApproval(ctx, id, "bob", "slack", types.ApprovalApproved, 2, ""); err != nil {
		t.Fatalf("decision pinned to the fresh round refused: %v", err)
	}
	decision, err := st.LatestApprovalDecision(ctx, id, 2)
	if err != nil || decision.StepAction != "platform.replace_node" {
		t.Fatalf("round-2 decision = %+v, %v; want it bound to round 2's request", decision, err)
	}
}

// R11.2 (review F2): the decision moment — and the human's stated reason —
// lands in the audit trail at decide time, not only at resume.
func TestDecisionReasonIsAudited(t *testing.T) {
	c, st, id := approvalEpochFixture(t)
	ctx := context.Background()
	if err := c.DecideApproval(ctx, id, "alice", "cli", types.ApprovalRejected, 1, "wrong node, hardware ticket filed"); err != nil {
		t.Fatal(err)
	}
	trail, err := st.AuditTrail(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, e := range trail {
		if e.Action == "approval-rejected" && e.Actor == "alice" && e.Result == "wrong node, hardware ticket filed" {
			found = true
		}
	}
	if !found {
		t.Fatalf("audit trail lacks the decision entry with its reason: %+v", trail)
	}
}

// R11.4: an incident that opened BEFORE its policy existed (playbook binding
// is open-time) is late-bound in OBSERVING once a matching policy appears —
// with the write-fence bump and an audit row — instead of holding unbound
// until quiet-resolve buries the fault. Bound incidents are never re-bound.
func TestLateBindsIncidentOpenedBeforeItsPolicyExisted(t *testing.T) {
	emptyEngine, err := playbook.NewEngine(map[string]*playbook.Playbook{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	c, st, _ := miniController(t, emptyEngine)
	ctx := context.Background()
	if err := st.UpsertNode(ctx, &types.Node{Name: "n1"}); err != nil {
		t.Fatal(err)
	}
	inc := &types.Incident{
		ID: "inc-latebind", Target: types.Target{Node: "n1"},
		Class: types.ClassFellOffBus, State: types.StateObserving, DryRun: true,
		OpenedAt: time.Now(), UpdatedAt: time.Now(), StateChangedAt: time.Now(),
	}
	if err := st.CreateIncident(ctx, inc); err != nil {
		t.Fatal(err)
	}

	// No policy yet: the pass leaves the incident unbound.
	c.reconcile(ctx)
	got, err := st.GetIncident(ctx, inc.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Playbook != "" {
		t.Fatalf("playbook = %q, want unbound while no policy matches", got.Playbook)
	}
	fence := got.StateChangedAt

	// The policy arrives (config rollout lands). The next pass binds it.
	pb := &playbook.Playbook{
		Name: "late", Target: "node",
		Steps: []playbook.Step{{Name: "Observe", Action: "notify.observe"}},
	}
	engine, err := playbook.NewEngine(map[string]*playbook.Playbook{"late": pb},
		[]playbook.Policy{{Class: types.ClassFellOffBus, Playbook: "late"}})
	if err != nil {
		t.Fatal(err)
	}
	c.SetEngine(engine)
	c.reconcile(ctx)
	got, err = st.GetIncident(ctx, inc.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Playbook != "late" {
		t.Fatalf("playbook = %q, want late-bound %q", got.Playbook, "late")
	}
	if !got.StateChangedAt.After(fence) {
		t.Fatal("late-bind must bump StateChangedAt (the write-fence)")
	}
	trail, err := st.AuditTrail(ctx, inc.ID)
	if err != nil {
		t.Fatal(err)
	}
	bound := false
	for _, e := range trail {
		if e.Action == "bind-playbook" {
			bound = true
		}
	}
	if !bound {
		t.Fatalf("audit trail lacks the bind-playbook entry: %+v", trail)
	}
}
