package controller

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/kubeneuron/kubeneuron/internal/platform"
	kubernetesplatform "github.com/kubeneuron/kubeneuron/internal/platform/kubernetes"
	"github.com/kubeneuron/kubeneuron/internal/playbook"
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
// ReleaseCordonOwners is the path the janitor actually takes now, and this fake
// did not have it — it satisfied the interface only through an embedded nil, so
// the old capability assertion failed and every janitor test here quietly
// exercised the reason-only FALLBACK instead. The tests passed while covering
// the wrong path.
//
// Models the same live re-check as UncordonIfReason: a cordon replaced since
// the listing is not this owner's to release.
func (p *janitorPlatform) ReleaseCordonOwners(_ context.Context, node string, owners []string) (bool, int, error) {
	for _, owner := range owners {
		if want, overridden := p.live[node]; overridden && want != owner &&
			want != platform.LegacyCordonOwner(owner) {
			continue
		}
		p.uncordoned = append(p.uncordoned, node)
		return true, 0, nil
	}
	return false, 0, nil
}

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

// --- shared cordons: one node, several remediations ---------------------------

// ownedPlatform models a platform that reference-counts its cordons: a set of
// owners per node, and the node only back in service when the last one leaves.
//
// It also keeps the OLD unowned Uncordon, because that is the behaviour these
// tests exist to keep out of the executor: a call to it here is a node handed to
// the scheduler without asking who else is holding it.
type ownedPlatform struct {
	stackPlatform
	owners map[string][]string
	reason map[string]string
	held   map[string]bool
	// heldOwners is the per-HOLD verdict the real platform records beside the
	// node-scoped one. Modelling only the node-scoped mark would hide the defect
	// these tests exist for: a verdict about one remediation answering for every
	// other hold on the same machine.
	heldOwners map[string][]string
	// uncordoned records nodes actually returned to service, by whichever path.
	uncordoned []string
	// unowned records the releases that never asked who else held the node.
	unowned []string
	// listing, when set, is what CordonedNodes reports instead of the live
	// state — the janitor reads an informer cache, and what it decides from can
	// be older than what it writes against.
	listing []platform.CordonedNode
}

func (p *ownedPlatform) CordonForOwner(_ context.Context, node, owner, reason string) error {
	if p.owners == nil {
		p.owners, p.reason = map[string][]string{}, map[string]string{}
		p.heldOwners = map[string][]string{}
	}
	if !slices.Contains(p.owners[node], owner) {
		p.owners[node] = append(p.owners[node], owner)
	}
	p.reason[node] = reason // the LAST cordoner's reason, as on a real node
	return nil
}

func (p *ownedPlatform) ReleaseCordonOwners(_ context.Context, node string, owners []string) (bool, int, error) {
	remaining := slices.DeleteFunc(slices.Clone(p.owners[node]), func(o string) bool {
		return slices.Contains(owners, o)
	})
	if len(remaining) == len(p.owners[node]) {
		return false, len(remaining), nil // not there: a no-op, not an error
	}
	p.owners[node] = remaining
	// A verdict about a hold that has left goes with it, exactly as on the node:
	// one that outlived its hold would keep the machine cordoned for good.
	p.heldOwners[node] = slices.DeleteFunc(slices.Clone(p.heldOwners[node]), func(o string) bool {
		return slices.Contains(owners, o)
	})
	if len(remaining) > 0 {
		return false, len(remaining), nil
	}
	delete(p.owners, node)
	delete(p.reason, node)
	delete(p.held, node)
	delete(p.heldOwners, node)
	p.uncordoned = append(p.uncordoned, node)
	return true, 0, nil
}

func (p *ownedPlatform) MarkCordonHeldIfOwner(_ context.Context, node, owner string) (bool, error) {
	if !slices.Contains(p.owners[node], owner) {
		return false, nil
	}
	if p.held == nil {
		p.held = map[string]bool{}
	}
	p.held[node] = true
	if !slices.Contains(p.heldOwners[node], owner) {
		p.heldOwners[node] = append(p.heldOwners[node], owner)
	}
	return true, nil
}

func (p *ownedPlatform) MarkCordonHeldIfReason(_ context.Context, node, expected string) (bool, error) {
	if p.reason[node] != expected {
		return false, nil
	}
	if p.held == nil {
		p.held = map[string]bool{}
	}
	p.held[node] = true
	return true, nil
}

func (p *ownedPlatform) UncordonIfReason(_ context.Context, node, expected string) (bool, error) {
	if p.reason[node] != expected {
		return false, nil
	}
	p.unowned = append(p.unowned, node)
	p.uncordoned = append(p.uncordoned, node)
	return true, nil
}

// Uncordon is the unguarded release: it asks nobody and empties the node.
func (p *ownedPlatform) Uncordon(_ context.Context, node string) error {
	delete(p.owners, node)
	p.unowned = append(p.unowned, node)
	p.uncordoned = append(p.uncordoned, node)
	return nil
}

func (p *ownedPlatform) CordonedNodes(context.Context) ([]platform.CordonedNode, error) {
	if p.listing != nil {
		return p.listing, nil
	}
	var out []platform.CordonedNode
	for node, owners := range p.owners {
		out = append(out, platform.CordonedNode{
			Name: node, Reason: p.reason[node], Owners: slices.Clone(owners),
			Held: p.held[node], HeldOwners: slices.Clone(p.heldOwners[node]),
		})
	}
	return out, nil
}

func (p *ownedPlatform) cordoned(node string) bool { return len(p.owners[node]) > 0 }

func sharedCordonIncident(id string, class types.ProblemClass) *types.Incident {
	return &types.Incident{ID: id, Target: types.Target{Node: "node-a"}, Class: class, State: types.StateExecuting}
}

// TestAFinishedIncidentDoesNotUncordonANodeAnotherIsRemediating is the P0.
//
// Two incidents on two GPUs of one node each run the cordon step. The uncordon
// step of whichever finishes first used to call the unguarded platform release,
// which put the whole machine back into service: the scheduler puts tenant work
// onto a node whose other GPU is about to be reset. It also restored from
// whichever snapshot survived the second cordon, so a human's own cordon and
// their karpenter.sh/do-not-disrupt pin could go with it.
func TestAFinishedIncidentDoesNotUncordonANodeAnotherIsRemediating(t *testing.T) {
	p := &ownedPlatform{}
	c, _ := stackTestController(t, p)
	ctx := context.Background()
	first := sharedCordonIncident("inc-1", types.ClassECCDBE)
	second := sharedCordonIncident("inc-2", types.ClassFellOffBus)

	for _, inc := range []*types.Incident{first, second} {
		if _, err := c.executePlatformStep(ctx, inc, "cordon", &playbook.Step{Name: "cordon"}); err != nil {
			t.Fatal(err)
		}
	}

	result, err := c.executePlatformStep(ctx, first, "uncordon", &playbook.Step{Name: "uncordon"})
	if err != nil {
		t.Fatal(err)
	}
	if !result.OK {
		t.Fatalf("uncordon step = %+v; a step that handed its own hold back has done its whole job "+
			"and must not fail the playbook", result)
	}
	if len(p.unowned) != 0 {
		t.Fatalf("the uncordon step released %v without asking who else was holding it", p.unowned)
	}
	if !p.cordoned("node-a") {
		t.Fatal("the first of two remediations to finish returned the node to service; the " +
			"scheduler puts tenant work onto a machine whose other GPU is about to be reset")
	}
	if !slices.Equal(p.owners["node-a"], []string{"inc-2"}) {
		t.Fatalf("holders = %v, want the remediation that has not finished", p.owners["node-a"])
	}

	// The last one out still returns the node, or every cordoned node is stranded.
	if _, err := c.executePlatformStep(ctx, second, "uncordon", &playbook.Step{Name: "uncordon"}); err != nil {
		t.Fatal(err)
	}
	if p.cordoned("node-a") || len(p.uncordoned) != 1 {
		t.Fatalf("uncordoned = %v, holders = %v; the last remediation to finish must put the node "+
			"back", p.uncordoned, p.owners["node-a"])
	}
}

// TestTheJanitorReleasesOnlyTheAbandonedHold covers the recovery path across the
// same node.
//
// A controller that dies mid-remediation leaves its hold on the node, and the
// janitor is what eventually clears it. It reads a listing where the REASON
// belongs to whichever incident cordoned last, so judging the node by that alone
// either releases a machine another incident is still working on, or never looks
// at the hold whose reason was overwritten and strands the node forever.
func TestTheJanitorReleasesOnlyTheAbandonedHold(t *testing.T) {
	p := &ownedPlatform{}
	c, st := stackTestController(t, p)
	ctx := context.Background()

	abandoned := sharedCordonIncident("inc-1", types.ClassECCDBE)
	abandoned.State = types.StateResolved
	running := sharedCordonIncident("inc-2", types.ClassFellOffBus)
	for _, inc := range []*types.Incident{abandoned, running} {
		if err := st.CreateIncident(ctx, inc); err != nil {
			t.Fatal(err)
		}
		if err := st.UpdateIncident(ctx, inc); err != nil {
			t.Fatal(err)
		}
		if err := p.CordonForOwner(ctx, "node-a", inc.ID, cordonReason(inc)); err != nil {
			t.Fatal(err)
		}
	}
	// The node's reason belongs to inc-2, the incident that cordoned last.
	if err := p.CordonForOwner(ctx, "node-a", abandoned.ID, cordonReason(abandoned)); err != nil {
		t.Fatal(err)
	}
	if err := p.CordonForOwner(ctx, "node-a", running.ID, cordonReason(running)); err != nil {
		t.Fatal(err)
	}

	c.reconcileCordonedNodes(ctx)

	if len(p.uncordoned) != 0 || !p.cordoned("node-a") {
		t.Fatalf("uncordoned = %v; the janitor released a node inc-2 is still remediating, on the "+
			"strength of a verdict about inc-1", p.uncordoned)
	}
	if !slices.Equal(p.owners["node-a"], []string{"inc-2"}) {
		t.Fatalf("holders = %v; the finished incident's hold must be cleared or the node stays out "+
			"of the fleet after inc-2 finishes too", p.owners["node-a"])
	}

	// Once the second incident resolves, the same pass releases the machine.
	running.State = types.StateResolved
	if err := st.UpdateIncident(ctx, running); err != nil {
		t.Fatal(err)
	}
	c.reconcileCordonedNodes(ctx)
	if p.cordoned("node-a") {
		t.Fatalf("holders = %v; both remediations are finished and the node is still cordoned",
			p.owners["node-a"])
	}
}

// TestAHumansVerdictIsRecordedAgainstTheHoldItWasAboutCoversTheSharedCordon.
//
// The held mark deliberately outlives the incident row. On a shared cordon the
// reason belongs to whichever incident cordoned LAST, so a verdict about any
// other holder never matches it — the mark is never written, and once retention
// prunes that holder's row the janitor sees an unexplained owner and puts a node
// a human took charge of back into service.
func TestAHumansVerdictIsRecordedAgainstTheHoldItWasAbout(t *testing.T) {
	p := &ownedPlatform{}
	c, st := stackTestController(t, p)
	ctx := context.Background()

	needsHuman := sharedCordonIncident("inc-1", types.ClassECCDBE)
	needsHuman.State = types.StateNeedsHuman
	running := sharedCordonIncident("inc-2", types.ClassFellOffBus)
	for _, inc := range []*types.Incident{needsHuman, running} {
		if err := st.CreateIncident(ctx, inc); err != nil {
			t.Fatal(err)
		}
		if err := st.UpdateIncident(ctx, inc); err != nil {
			t.Fatal(err)
		}
		if err := p.CordonForOwner(ctx, "node-a", inc.ID, cordonReason(inc)); err != nil {
			t.Fatal(err)
		}
	}

	c.reconcileCordonedNodes(ctx)

	if !p.held["node-a"] {
		t.Fatal("a human's verdict on inc-1 was not recorded on the node because the node's reason " +
			"belongs to inc-2; when retention prunes inc-1's row the janitor reads its hold as " +
			"unexplained and returns a machine a human took charge of to the scheduler")
	}
}

// TestAVerdictIsNotStampedOnAHoldThatHasSinceBeenReleased covers the write the
// janitor makes from a cached listing.
//
// The held mark deliberately OUTLIVES the incident row, so a wrong one is
// permanent: the janitor keeps the node cordoned forever once the row that
// explains it is pruned. On a shared cordon the reason annotation is no longer a
// usable scope for that write — it belongs to whichever incident cordoned LAST
// and stays there when any other holder leaves, so it still matches long after
// the hold this verdict was about has gone. The hold itself is the only honest
// scope.
func TestAVerdictIsNotStampedOnAHoldThatHasSinceBeenReleased(t *testing.T) {
	p := &ownedPlatform{}
	c, st := stackTestController(t, p)
	ctx := context.Background()

	halted := sharedCordonIncident("inc-1", types.ClassECCDBE)
	halted.State = types.StateNeedsHuman
	running := sharedCordonIncident("inc-2", types.ClassFellOffBus)
	for _, inc := range []*types.Incident{halted, running} {
		if err := st.CreateIncident(ctx, inc); err != nil {
			t.Fatal(err)
		}
		if err := st.UpdateIncident(ctx, inc); err != nil {
			t.Fatal(err)
		}
		if err := p.CordonForOwner(ctx, "node-a", inc.ID, cordonReason(inc)); err != nil {
			t.Fatal(err)
		}
	}
	// The listing still shows inc-1 holding the node...
	p.listing = []platform.CordonedNode{{
		Name: "node-a", Reason: cordonReason(running), Owners: []string{"inc-1", "inc-2"},
	}}
	// ...but by the time the janitor writes, inc-1's hold is gone — and the
	// reason on the node still names inc-2, exactly as the listing said.
	if _, _, err := p.ReleaseCordonOwners(ctx, "node-a", []string{"inc-1"}); err != nil {
		t.Fatal(err)
	}

	c.reconcileCordonedNodes(ctx)

	if p.held["node-a"] {
		t.Fatal("a node was marked as owned by a human on the strength of a hold that had already " +
			"been released; that mark outlives the incident row, so once inc-2's row is pruned this " +
			"GPU node stays out of the fleet forever with nothing left to explain why")
	}
}

// TestOneHumanHeldHoldDoesNotStrandTheOtherHoldsOnTheSameNode.
//
// The held mark says "a human has taken charge of THIS remediation", and it is
// the one thing that stops the janitor releasing a hold whose incident row has
// been swept. A node-scoped mark answers that question for every OTHER hold on
// the machine too, and always with "yes" — so an abandoned hold on a node that
// happens to carry one halted remediation can never be dropped, the owner set
// can never empty, and the mark that is blocking it is only cleared when the set
// DOES empty. A GPU node deadlocks out of the fleet with nothing running that
// will ever put it back, and because the incident behind the surviving hold is
// gone there is no stuck-cordon notification either: the only trace at 3am is an
// Info log line repeating on every reconcile tick.
func TestOneHumanHeldHoldDoesNotStrandTheOtherHoldsOnTheSameNode(t *testing.T) {
	p := &ownedPlatform{}
	c, st := stackTestController(t, p)
	ctx := context.Background()

	// GPU0 threw an uncorrectable ECC error, the ladder gave up, and a human owns
	// that decision. GPU3 on the same machine is being remediated in parallel.
	halted := sharedCordonIncident("inc-1", types.ClassECCDBE)
	halted.State = types.StateNeedsHuman
	leaked := sharedCordonIncident("inc-2", types.ClassFellOffBus)
	for _, inc := range []*types.Incident{halted, leaked} {
		if err := st.CreateIncident(ctx, inc); err != nil {
			t.Fatal(err)
		}
		if err := st.UpdateIncident(ctx, inc); err != nil {
			t.Fatal(err)
		}
		if err := p.CordonForOwner(ctx, "node-a", inc.ID, cordonReason(inc)); err != nil {
			t.Fatal(err)
		}
	}

	// A pass while inc-2 is still running records the human's verdict on inc-1.
	c.reconcileCordonedNodes(ctx)
	if !p.held["node-a"] {
		t.Fatal("the human's verdict on inc-1 was not recorded at all")
	}

	// inc-2 then resolves without ever reaching its uncordon step — the leak this
	// janitor exists for — and its row is swept by retention before the next pass
	// (a controller down overnight, or a store restored from backup).
	leaked.State = types.StateResolved
	if err := st.UpdateIncident(ctx, leaked); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Prune(ctx, time.Nanosecond, time.Nanosecond); err != nil {
		t.Fatal(err)
	}
	if _, err := st.GetIncident(ctx, "inc-2"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("GetIncident(inc-2) = %v, want the row to have been pruned", err)
	}

	c.reconcileCordonedNodes(ctx)
	if slices.Contains(p.owners["node-a"], "inc-2") {
		t.Fatalf("holders = %v; inc-2's abandoned hold was kept because a human's verdict about "+
			"inc-1 marked the whole NODE held, so nothing will ever drop it", p.owners["node-a"])
	}

	// The human replaces GPU0 and resolves inc-1. That was the last real hold, so
	// the machine must go back into the fleet.
	halted.State = types.StateResolved
	if err := st.UpdateIncident(ctx, halted); err != nil {
		t.Fatal(err)
	}
	c.reconcileCordonedNodes(ctx)
	if p.cordoned("node-a") {
		t.Fatalf("holders = %v; every remediation on this node is finished and it is still cordoned. "+
			"Eight H100s are out of the fleet permanently and no incident row is left to explain why",
			p.owners["node-a"])
	}
}

// TestAHumanHeldHoldSurvivesItsOwnIncidentRow is the other direction of the same
// rule, and the one the held mark was invented for.
//
// A hold a human took charge of must NOT be released when retention sweeps the
// incident row that explains it. Scoping the mark to the hold has to keep that
// working, or the fix for the strand becomes a way to hand back a machine
// somebody deliberately took out of service.
func TestAHumanHeldHoldSurvivesItsOwnIncidentRow(t *testing.T) {
	p := &ownedPlatform{}
	c, st := stackTestController(t, p)
	ctx := context.Background()

	halted := sharedCordonIncident("inc-1", types.ClassECCDBE)
	halted.State = types.StateNeedsHuman
	if err := st.CreateIncident(ctx, halted); err != nil {
		t.Fatal(err)
	}
	if err := st.UpdateIncident(ctx, halted); err != nil {
		t.Fatal(err)
	}
	if err := p.CordonForOwner(ctx, "node-a", halted.ID, cordonReason(halted)); err != nil {
		t.Fatal(err)
	}
	// A second remediation shares the machine, so this is a counted cordon and
	// the node-scoped mark is not what answers for inc-1.
	running := sharedCordonIncident("inc-2", types.ClassFellOffBus)
	if err := st.CreateIncident(ctx, running); err != nil {
		t.Fatal(err)
	}
	if err := st.UpdateIncident(ctx, running); err != nil {
		t.Fatal(err)
	}
	if err := p.CordonForOwner(ctx, "node-a", running.ID, cordonReason(running)); err != nil {
		t.Fatal(err)
	}

	c.reconcileCordonedNodes(ctx)
	if !slices.Contains(p.heldOwners["node-a"], "inc-1") {
		t.Fatalf("held holds = %v, want inc-1: the human's verdict was not recorded against the "+
			"hold it was about", p.heldOwners["node-a"])
	}

	// Retention sweeps inc-1's row. NEEDS_HUMAN incidents expire and are pruned
	// like any other, and a store restored from backup loses them all at once.
	halted.State = types.StateExpired
	if err := st.UpdateIncident(ctx, halted); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Prune(ctx, time.Nanosecond, time.Nanosecond); err != nil {
		t.Fatal(err)
	}

	c.reconcileCordonedNodes(ctx)
	if !slices.Contains(p.owners["node-a"], "inc-1") {
		t.Fatalf("holders = %v; a hold a human had taken charge of was released as soon as its "+
			"incident row was pruned, which is the inversion the held mark exists to prevent",
			p.owners["node-a"])
	}
}

// TestCordonOwnershipInterfaceIsSatisfied: the executor and the janitor reach
// owner-counted cordons by type assertion, so a platform missing ONE method does
// not fail to compile — it silently reverts to releasing nodes without asking
// who holds them. This makes that a build failure instead.
func TestCordonOwnershipInterfaceIsSatisfied(t *testing.T) {
	var _ platform.CordonOwnership = (*ownedPlatform)(nil)
	var _ platform.CordonOwnership = (*kubernetesplatform.Platform)(nil)
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
