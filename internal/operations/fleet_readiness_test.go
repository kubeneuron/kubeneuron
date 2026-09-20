package operations

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/kubeneuron/kubeneuron/internal/decision"
	"github.com/kubeneuron/kubeneuron/internal/store/sqlite"
	"github.com/kubeneuron/kubeneuron/pkg/types"
)

// newFleetReadinessManager builds a Manager whose snapshot builder records
// every node it is asked about, so tests can prove the builder was never
// consulted for an inventory that must be rejected up front.
func newFleetReadinessManager(t *testing.T, nodes []*types.Node) (*Manager, *[]string) {
	t.Helper()
	st, err := sqlite.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	now := time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)
	built := &[]string{}
	mgr := New(Options{
		Resources: st,
		Workflow:  st,
		Now:       func() time.Time { return now },
		BuildSnapshot: func(_ context.Context, node string, request decision.Request) (decision.Snapshot, error) {
			*built = append(*built, node)
			return decision.Snapshot{
				Version:     decision.EvaluatorVersion,
				EvaluatedAt: now,
				Node:        types.Node{Name: node},
				Request:     request,
			}, nil
		},
		ListNodes: func(context.Context) ([]*types.Node, error) { return nodes, nil },
	})
	return mgr, built
}

func readinessNames(items []Readiness) string {
	out := make([]string, 0, len(items))
	for _, item := range items {
		out = append(out, item.Node)
	}
	return strings.Join(out, ",")
}

func TestFleetReadinessPageRejectsIncompleteInventory(t *testing.T) {
	// Each inventory places the defect where the sort comparator would have
	// dereferenced it before validation: last, first, and between valid nodes.
	for name, nodes := range map[string][]*types.Node{
		"nil last":         {{Name: "gpu-a"}, nil},
		"nil first":        {nil, {Name: "gpu-b"}, {Name: "gpu-a"}},
		"blank name":       {{Name: "gpu-b"}, {Name: "  "}, {Name: "gpu-a"}},
		"empty name":       {{Name: "gpu-b"}, {Name: ""}, {Name: "gpu-a"}},
		"only nil":         {nil},
		"nil after cursor": {{Name: "gpu-a"}, nil, {Name: "gpu-z"}},
	} {
		t.Run(name, func(t *testing.T) {
			mgr, built := newFleetReadinessManager(t, nodes)
			items, next, err := mgr.FleetReadinessPage(context.Background(), ReadinessFilter{AfterNode: "gpu-a"})
			if !errors.Is(err, ErrIncompleteInventory) || items != nil || next != "" {
				t.Fatalf("incomplete inventory = (%v, %q, %v), want ErrIncompleteInventory and no partial page", items, next, err)
			}
			if len(*built) != 0 {
				t.Fatalf("built snapshots for %v before rejecting the inventory; an incomplete inventory must not be evaluated at all", *built)
			}
		})
	}
}

func TestFleetReadinessPageDoesNotReorderListerInventory(t *testing.T) {
	nodes := []*types.Node{{Name: "gpu-c"}, {Name: "gpu-a"}, {Name: "gpu-b"}}
	mgr, _ := newFleetReadinessManager(t, nodes)
	items, next, err := mgr.FleetReadinessPage(context.Background(), ReadinessFilter{})
	if err != nil || next != "" || readinessNames(items) != "gpu-a,gpu-b,gpu-c" {
		t.Fatalf("page = (%v, %q, %v), want gpu-a,gpu-b,gpu-c", readinessNames(items), next, err)
	}
	if nodes[0].Name != "gpu-c" || nodes[1].Name != "gpu-a" || nodes[2].Name != "gpu-b" {
		t.Fatalf("lister inventory was reordered in place: %v %v %v", nodes[0].Name, nodes[1].Name, nodes[2].Name)
	}
}

func TestFleetReadinessPagePaginationUnchanged(t *testing.T) {
	nodes := []*types.Node{{Name: "gpu-d"}, {Name: "gpu-b"}, {Name: "gpu-a"}, {Name: "gpu-c"}}
	mgr, built := newFleetReadinessManager(t, nodes)
	ctx := context.Background()

	// A page of two proves a third exists without returning it.
	first, next, err := mgr.FleetReadinessPage(ctx, ReadinessFilter{Limit: 2})
	if err != nil || next != "gpu-b" || readinessNames(first) != "gpu-a,gpu-b" {
		t.Fatalf("first page = (%v, %q, %v)", readinessNames(first), next, err)
	}
	if got := strings.Join(*built, ","); got != "gpu-a,gpu-b,gpu-c" {
		t.Fatalf("first page built %v, want the page plus one look-ahead node", got)
	}
	second, next, err := mgr.FleetReadinessPage(ctx, ReadinessFilter{Limit: 2, AfterNode: next})
	if err != nil || next != "" || readinessNames(second) != "gpu-c,gpu-d" {
		t.Fatalf("second page = (%v, %q, %v)", readinessNames(second), next, err)
	}

	// The all-fleet helper walks the same pages and sees every node once.
	all, err := mgr.FleetReadiness(ctx)
	if err != nil || readinessNames(all) != "gpu-a,gpu-b,gpu-c,gpu-d" {
		t.Fatalf("fleet readiness = (%v, %v)", readinessNames(all), err)
	}
}
