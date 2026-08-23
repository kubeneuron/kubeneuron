package controller

import (
	"context"
	"errors"
	"testing"

	"github.com/kubeneuron/kubeneuron/internal/platform"
	kubernetesplatform "github.com/kubeneuron/kubeneuron/internal/platform/kubernetes"
	"github.com/kubeneuron/kubeneuron/internal/store"
	"github.com/kubeneuron/kubeneuron/pkg/types"
)

// janitorPlatform reports cordoned nodes and records what was released.
type janitorPlatform struct {
	stackPlatform
	cordoned   []platform.CordonedNode
	uncordoned []string
	// live overrides a node's current cordon reason, modelling a listing that
	// has gone stale between the cache read and the write.
	live map[string]string
	held map[string]bool
}

func (p *janitorPlatform) CordonedNodes(context.Context) ([]platform.CordonedNode, error) {
	return p.cordoned, nil
}

func (p *janitorPlatform) Uncordon(_ context.Context, node string) error {
	p.uncordoned = append(p.uncordoned, node)
	return nil
}

// UncordonIfReason models the live re-check: it releases only when the node
// still carries the reason the janitor decided on. `live` overrides what the
// listing said, standing in for a cordon replaced since the cache was filled.
func (p *janitorPlatform) UncordonIfReason(_ context.Context, node, expected string) (bool, error) {
	if want, overridden := p.live[node]; overridden && want != expected {
		return false, nil
	}
	p.uncordoned = append(p.uncordoned, node)
	return true, nil
}

// MarkCordonHeldIfReason models the same live re-check as UncordonIfReason:
// the listing is served from a cache, and a cordon replaced since then must
// not inherit this incident's verdict. The held mark outlives the incident
// row, so marking the wrong one strands a node for good.
func (p *janitorPlatform) MarkCordonHeldIfReason(_ context.Context, node, expected string) (bool, error) {
	if want, overridden := p.live[node]; overridden && want != expected {
		return false, nil
	}
	if p.held == nil {
		p.held = map[string]bool{}
	}
	p.held[node] = true
	return true, nil
}

// The cordon reason is the only link between a node and the incident that took
// it out of service, so the format is a contract.
func TestCordonReasonRoundTripsTheIncidentID(t *testing.T) {
	inc := &types.Incident{ID: "row-remap-ok-ip-192-168-1-1.ec2.internal-b2e0fb1a", Class: "row-remap-ok"}
	got, ok := incidentFromCordonReason(cordonReason(inc))
	if !ok || got != inc.ID {
		t.Fatalf("incidentFromCordonReason(%q) = %q, %v", cordonReason(inc), got, ok)
	}
	if _, ok := incidentFromCordonReason("cordoned by someone else"); ok {
		t.Fatal("a cordon this product did not place must not be claimed")
	}
}

// A remediation that worked and only missed its uncordon has left capacity
// stranded for no reason.
func TestResolvedIncidentReleasesItsNode(t *testing.T) {
	inc := resetIncident()
	inc.State = types.StateResolved
	p := &janitorPlatform{cordoned: []platform.CordonedNode{{Name: "node-a", Reason: cordonReason(inc)}}}
	c, st := stackTestController(t, p)
	ctx := context.Background()
	if err := st.CreateIncident(ctx, inc); err != nil {
		t.Fatal(err)
	}
	if err := st.UpdateIncident(ctx, inc); err != nil {
		t.Fatal(err)
	}

	c.reconcileCordonedNodes(ctx)
	if len(p.uncordoned) != 1 || p.uncordoned[0] != "node-a" {
		t.Fatalf("uncordoned = %v, want the node back in service", p.uncordoned)
	}
}

// An incident that ended needing a human means something judged the node unfit.
// Putting it back silently would undo that judgement.
func TestUnresolvedIncidentKeepsItsNodeCordoned(t *testing.T) {
	inc := resetIncident()
	inc.State = types.StateNeedsHuman
	p := &janitorPlatform{cordoned: []platform.CordonedNode{{Name: "node-a", Reason: cordonReason(inc)}}}
	c, st := stackTestController(t, p)
	ctx := context.Background()
	if err := st.CreateIncident(ctx, inc); err != nil {
		t.Fatal(err)
	}
	if err := st.UpdateIncident(ctx, inc); err != nil {
		t.Fatal(err)
	}

	c.reconcileCordonedNodes(ctx)
	if len(p.uncordoned) != 0 {
		t.Fatalf("uncordoned = %v, want the node left cordoned", p.uncordoned)
	}
}

func TestRunningIncidentKeepsItsOwnCordon(t *testing.T) {
	inc := resetIncident() // StateExecuting
	p := &janitorPlatform{cordoned: []platform.CordonedNode{{Name: "node-a", Reason: cordonReason(inc)}}}
	c, st := stackTestController(t, p)
	ctx := context.Background()
	if err := st.CreateIncident(ctx, inc); err != nil {
		t.Fatal(err)
	}

	c.reconcileCordonedNodes(ctx)
	if len(p.uncordoned) != 0 {
		t.Fatalf("uncordoned = %v, want the running playbook left alone", p.uncordoned)
	}
}

// A cordon whose incident has been deleted will never be cleared by anything
// else, so the node would stay out of service forever.
func TestCordonWithNoIncidentIsReleased(t *testing.T) {
	p := &janitorPlatform{cordoned: []platform.CordonedNode{
		{Name: "node-a", Reason: "kubeneuron: row-remap-ok (vanished-incident)"},
	}}
	c, _ := stackTestController(t, p)

	c.reconcileCordonedNodes(context.Background())
	if len(p.uncordoned) != 1 {
		t.Fatalf("uncordoned = %v, want the orphaned cordon released", p.uncordoned)
	}
}

func TestCordonStoreFailureKeepsNodeCordoned(t *testing.T) {
	inc := resetIncident()
	p := &janitorPlatform{cordoned: []platform.CordonedNode{{Name: "node-a", Reason: cordonReason(inc)}}}
	c, _ := stackTestController(t, p)
	c.store = failingIncidentStore{Store: c.store, err: errors.New("database unavailable")}

	c.reconcileCordonedNodes(context.Background())
	if len(p.uncordoned) != 0 {
		t.Fatalf("uncordoned = %v, store failure must retain the cordon", p.uncordoned)
	}
}

type failingIncidentStore struct {
	store.Store
	err error
}

func (s failingIncidentStore) GetIncident(context.Context, string) (*types.Incident, error) {
	return nil, s.err
}

// Repeating the warning every reconcile tick would train people to ignore it.
func TestStuckCordonIsReportedOnce(t *testing.T) {
	c, _ := stackTestController(t, &janitorPlatform{})
	if !c.markCordonReported("node-a", "inc-1") {
		t.Fatal("first report must be allowed")
	}
	if c.markCordonReported("node-a", "inc-1") {
		t.Fatal("second report must be suppressed")
	}
	if !c.markCordonReported("node-a", "inc-2") {
		t.Fatal("a different incident is a different thing to say")
	}
}

// A node replaced mid-playbook leaves an incident targeting hardware that no
// longer exists. It can never progress, so it is closed with that stated.
func TestIncidentOnAVanishedNodeIsClosed(t *testing.T) {
	p := &janitorPlatform{}
	c, st := stackTestController(t, p)
	ctx := context.Background()

	gone := &types.Incident{
		ID:     "inc-gone",
		Target: types.Target{Node: "node-deleted"},
		State:  types.StateEvaluating,
	}
	if err := st.CreateIncident(ctx, gone); err != nil {
		t.Fatal(err)
	}
	// The authoritative platform lookup, not the GPU inventory, says the node is gone.
	c.resolveIncidentsOnVanishedNodes(ctx)

	got, err := st.GetIncident(ctx, gone.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != types.StateResolved {
		t.Fatalf("state = %s, want the incident closed", got.State)
	}
}

// An orphaned EXECUTING incident (no running step goroutine) on a deleted node
// has no legal EXECUTING->RESOLVED edge. The old closure attempted exactly that
// and re-logged the rejection every reconcile tick; it must now route through a
// valid path and actually close.
func TestExecutingIncidentOnAVanishedNodeClosesWithoutChurn(t *testing.T) {
	p := &janitorPlatform{}
	c, st := stackTestController(t, p)
	ctx := context.Background()

	gone := &types.Incident{
		ID:     "inc-exec-gone",
		Target: types.Target{Node: "node-deleted"},
		State:  types.StateExecuting,
	}
	if err := st.CreateIncident(ctx, gone); err != nil {
		t.Fatal(err)
	}
	c.resolveIncidentsOnVanishedNodes(ctx)

	got, err := st.GetIncident(ctx, gone.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != types.StateResolved {
		t.Fatalf("state = %s, want the EXECUTING incident closed via a valid path", got.State)
	}
}

func TestNodePresenceFailureDoesNotResolveIncident(t *testing.T) {
	p := &janitorPlatform{stackPlatform: stackPlatform{nodeErr: errors.New("Kubernetes API unavailable")}}
	c, st := stackTestController(t, p)
	ctx := context.Background()
	inc := &types.Incident{ID: "inc-unknown", Target: types.Target{Node: "node-deleted"}, State: types.StateEvaluating}
	if err := st.CreateIncident(ctx, inc); err != nil {
		t.Fatal(err)
	}
	c.resolveIncidentsOnVanishedNodes(ctx)
	got, err := st.GetIncident(ctx, inc.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State == types.StateResolved {
		t.Fatal("an unavailable node API must not resolve an incident")
	}
}

func TestIncidentOnALiveNodeIsUntouched(t *testing.T) {
	p := &janitorPlatform{}
	c, st := stackTestController(t, p)
	ctx := context.Background()

	live := resetIncident() // targets node-a, which the platform reports
	if err := st.CreateIncident(ctx, live); err != nil {
		t.Fatal(err)
	}
	c.resolveIncidentsOnVanishedNodes(ctx)

	got, err := st.GetIncident(ctx, live.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State == types.StateResolved {
		t.Fatal("an incident on a node that still exists must not be closed")
	}
}

// TestAReplacedCordonIsNotReleased covers the node that faulted again.
//
// The janitor's listing is served from an informer cache. A stale entry is not
// a missed cordon — it is a cordon that has since been REPLACED: a node that
// resolved and immediately faulted again is cordoned by a NEW incident while
// the old reason is still cached. Releasing on that basis hands the scheduler a
// machine in the middle of its own drain, at the moment a fault is being worked
// on it.
func TestAReplacedCordonIsNotReleased(t *testing.T) {
	inc := resetIncident()
	inc.State = types.StateResolved
	p := &janitorPlatform{
		// The cache says the node is held by the resolved incident...
		cordoned: []platform.CordonedNode{{Name: "node-a", Reason: cordonReason(inc)}},
		// ...but the live node was re-cordoned by a newer one.
		live: map[string]string{"node-a": cordonReason(&types.Incident{ID: "other-id", Class: inc.Class})},
	}
	c, st := stackTestController(t, p)
	ctx := context.Background()
	if err := st.CreateIncident(ctx, inc); err != nil {
		t.Fatal(err)
	}
	if err := st.UpdateIncident(ctx, inc); err != nil {
		t.Fatal(err)
	}

	c.reconcileCordonedNodes(ctx)
	if len(p.uncordoned) != 0 {
		t.Fatalf("uncordoned = %v; the live cordon belongs to a different incident, and releasing "+
			"it returns a node to the scheduler while its own drain is in progress", p.uncordoned)
	}
}

// TestAHeldCordonSurvivesItsIncidentBeingPruned covers the invariant this file
// states at the top: an incident that ended in NEEDS_HUMAN or expired is not
// uncordoned, because putting the node back silently would undo that judgement.
//
// Retention prunes RESOLVED and EXPIRED alike, so an incident that timed out
// awaiting approval lost its row — and the janitor then read "no such
// incident" as "nothing will come back for it" and released the very cordon it
// had refused to release the day before.
func TestAHeldCordonSurvivesItsIncidentBeingPruned(t *testing.T) {
	pruned := &types.Incident{ID: "pruned-id", Class: types.ClassFellOffBus}
	p := &janitorPlatform{cordoned: []platform.CordonedNode{{
		Name:   "node-a",
		Reason: cordonReason(pruned),
		Held:   true,
	}}}
	c, _ := stackTestController(t, p)

	c.reconcileCordonedNodes(context.Background())
	if len(p.uncordoned) != 0 {
		t.Fatalf("uncordoned = %v; a cordon a human owns was released because retention swept its "+
			"incident row, which is exactly the judgement this janitor refuses to undo", p.uncordoned)
	}
}

// TestHeldMarkIsNotStampedOnAReplacedCordon covers the staleness guard that
// MarkCordonHeld went without for a round, while its sibling two lines away
// had one.
//
// Both act on a listing served from the informer cache, where a stale entry
// does not mean a missed cordon — it means a cordon that has since been
// REPLACED by a newer incident's. Releasing the wrong one is bad and
// self-heals; marking the wrong one does not, because the held mark is
// deliberately designed to outlive the incident row. When the newer incident
// is pruned, the janitor sees the mark, keeps the node cordoned, and a GPU
// node is out of the fleet permanently on the strength of a decision made
// about a different incident.
func TestHeldMarkIsNotStampedOnAReplacedCordon(t *testing.T) {
	p := &janitorPlatform{
		cordoned: []platform.CordonedNode{{
			Name:   "gpu-1",
			Reason: cordonReason(&types.Incident{ID: "inc-1", Class: types.ClassECCDBE}),
		}},
		// By the time the write lands, inc-2 owns this cordon.
		live: map[string]string{
			"gpu-1": cordonReason(&types.Incident{ID: "inc-2", Class: types.ClassECCDBE}),
		},
	}
	marked, err := p.MarkCordonHeldIfReason(context.Background(), "gpu-1",
		cordonReason(&types.Incident{ID: "inc-1", Class: types.ClassECCDBE}))
	if err != nil {
		t.Fatal(err)
	}
	if marked || p.held["gpu-1"] {
		t.Fatal("a held mark decided about inc-1 was stamped onto inc-2's live cordon; when " +
			"inc-2's row is pruned the janitor will keep this node cordoned forever")
	}
}

// TestCordonJanitorInterfaceIsSatisfied guards a failure mode this file found
// the hard way: the janitor is reached by a type assertion to
// platform.CordonJanitor, so a platform missing ONE method does not fail to
// compile — it silently stops being a janitor, and every abandoned cordon it
// would have released stays put with no error anywhere.
//
// Adding a method to that interface without adding it to a platform is
// therefore a silent capacity leak. This makes it a build failure instead.
func TestCordonJanitorInterfaceIsSatisfied(t *testing.T) {
	var _ platform.CordonJanitor = (*janitorPlatform)(nil)
	var _ platform.CordonJanitor = (*kubernetesplatform.Platform)(nil)
}
