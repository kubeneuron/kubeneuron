package controller

import (
	"context"
	"errors"
	"testing"

	"github.com/kubeneuron/kubeneuron/internal/platform"
	"github.com/kubeneuron/kubeneuron/internal/store"
	"github.com/kubeneuron/kubeneuron/pkg/types"
)

// janitorPlatform reports cordoned nodes and records what was released.
type janitorPlatform struct {
	stackPlatform
	cordoned   []platform.CordonedNode
	uncordoned []string
}

func (p *janitorPlatform) CordonedNodes(context.Context) ([]platform.CordonedNode, error) {
	return p.cordoned, nil
}

func (p *janitorPlatform) Uncordon(_ context.Context, node string) error {
	p.uncordoned = append(p.uncordoned, node)
	return nil
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
