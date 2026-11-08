package operations

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/kubeneuron/kubeneuron/internal/decision"
	"github.com/kubeneuron/kubeneuron/internal/store"
	"github.com/kubeneuron/kubeneuron/internal/store/sqlite"
	"github.com/kubeneuron/kubeneuron/internal/topology"
	"github.com/kubeneuron/kubeneuron/pkg/types"
)

func topologyTestBuilder(calls *[]string) TopologyBuilder {
	return func(_ context.Context, node string) (topology.Classification, error) {
		*calls = append(*calls, node)
		switch node {
		case "ghost":
			return topology.Classification{}, store.ErrNotFound
		case "broken":
			return topology.Classification{}, errors.New("store exploded")
		}
		return topology.Classify(topology.Snapshot{
			Node: node, NodeUID: "uid", EvaluatedAt: time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC),
			Resources: topology.Resources{Source: topology.ResourceSourceNode, Capacity: map[string]int64{"nvidia.com/mig-1g.5gb": 7}},
			DRA:       topology.DRAObservation{State: topology.DRADisabled},
		}), nil
	}
}

func newTopologyManager(t *testing.T, builder TopologyBuilder) (*Manager, *[]string) {
	t.Helper()
	st, err := sqlite.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	now := time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)
	snapshots := &[]string{}
	mgr := New(Options{
		Resources: st,
		Workflow:  st,
		Now:       func() time.Time { return now },
		BuildSnapshot: func(_ context.Context, node string, request decision.Request) (decision.Snapshot, error) {
			*snapshots = append(*snapshots, node)
			return decision.Snapshot{Version: decision.EvaluatorVersion, EvaluatedAt: now, Node: types.Node{Name: node, UID: "uid"}, Request: request}, nil
		},
		ListNodes: func(context.Context) ([]*types.Node, error) {
			return []*types.Node{{Name: "gpu-a"}, {Name: "gpu-b"}}, nil
		},
		BuildTopology: builder,
	})
	return mgr, snapshots
}

func TestNodeTopologyIsReadOnlyAndFailsClosed(t *testing.T) {
	calls := &[]string{}
	mgr, _ := newTopologyManager(t, topologyTestBuilder(calls))
	ctx := context.Background()

	item, err := mgr.NodeTopology(ctx, "gpu-a")
	if err != nil {
		t.Fatal(err)
	}
	if item.Topology != topology.TopologyMIG || item.DeviceActions != topology.DeviceActionsRefused || !item.Has(topology.ReasonMigPartitioned) {
		t.Fatalf("classification = %s", item.Summary)
	}
	if _, err := mgr.NodeTopology(ctx, "  "); err == nil {
		t.Fatal("blank node must be rejected")
	}
	if _, err := mgr.NodeTopology(ctx, "ghost"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("unknown node = %v, want ErrNotFound", err)
	}
	if _, err := mgr.NodeTopology(ctx, "broken"); err == nil || errors.Is(err, store.ErrNotFound) {
		t.Fatalf("builder failure = %v, want a plain error", err)
	}

	// No builder: unavailable, never a healthy-looking empty classification.
	without, _ := newTopologyManager(t, nil)
	if _, err := without.NodeTopology(ctx, "gpu-a"); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("no builder = %v, want ErrUnavailable", err)
	}
	var nilManager *Manager
	if _, err := nilManager.NodeTopology(ctx, "gpu-a"); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("nil manager = %v, want ErrUnavailable", err)
	}
}

func TestNodeReadinessWithTopologyEmbedsClassificationOnlyForSingleNode(t *testing.T) {
	calls := &[]string{}
	mgr, snapshots := newTopologyManager(t, topologyTestBuilder(calls))
	ctx := context.Background()

	item, err := mgr.NodeReadinessWithTopology(ctx, "gpu-a")
	if err != nil {
		t.Fatal(err)
	}
	if item.Topology == nil || item.Topology.Topology != topology.TopologyMIG {
		t.Fatalf("readiness topology = %+v, want the mig classification", item.Topology)
	}
	if item.Decision.State != decision.StateUnknown {
		t.Fatalf("decision = %+v: the classification must not change the readiness decision", item.Decision)
	}

	// The plain path and fleet pages do not classify: one snapshot per node,
	// no topology builder call.
	*calls = nil
	*snapshots = nil
	plain, err := mgr.NodeReadiness(ctx, "gpu-a")
	if err != nil || plain.Topology != nil {
		t.Fatalf("NodeReadiness = (%+v, %v), want no topology", plain.Topology, err)
	}
	page, _, err := mgr.FleetReadinessPage(ctx, ReadinessFilter{})
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range page {
		if entry.Topology != nil {
			t.Fatalf("fleet page for %s carries a topology", entry.Node)
		}
	}
	if len(*calls) != 0 {
		t.Fatalf("topology builder was consulted by non-topology paths: %v", *calls)
	}

	// A failed classification fails the request rather than silently omitting
	// the field.
	if _, err := mgr.NodeReadinessWithTopology(ctx, "broken"); err == nil {
		t.Fatal("a failed classification must fail the readiness request")
	}
	// Without a builder the field is simply absent (older controllers).
	without, _ := newTopologyManager(t, nil)
	item, err = without.NodeReadinessWithTopology(ctx, "gpu-a")
	if err != nil || item.Topology != nil {
		t.Fatalf("without builder = (%+v, %v), want readiness without topology", item.Topology, err)
	}
}
