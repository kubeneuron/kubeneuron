package controller

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/kubeneuron/kubeneuron/internal/notify"
	"github.com/kubeneuron/kubeneuron/internal/platform"
	"github.com/kubeneuron/kubeneuron/internal/playbook"
	"github.com/kubeneuron/kubeneuron/internal/safety"
	storesqlite "github.com/kubeneuron/kubeneuron/internal/store/sqlite"
	"github.com/kubeneuron/kubeneuron/pkg/types"
)

// livePlatform reports what the CLUSTER says right now.
type livePlatform struct {
	platform.Platform
	labels map[string]string
}

func (p *livePlatform) Name() string { return "live" }
func (p *livePlatform) ListNodes(context.Context) ([]types.Node, error) {
	return []types.Node{{Name: "n1", UID: "n1-uid", Labels: p.labels}}, nil
}

// TestConfinementFollowsTheLiveCluster pins the freshness of the one lookup
// where a stale answer is a safety failure rather than a display glitch.
//
// nodeLabelsForConfinement used to prefer the store's cached node record
// whenever it carried any labels at all, falling back to the platform only
// when it carried none. Both directions were wrong, and a real cordon phase on
// a live cluster is what surfaced it: labelling a node into the declared blast
// radius had no effect until the inventory happened to sync, and the resolved
// non-match QUARANTINES rather than retrying, so the incident never recovered.
func TestConfinementFollowsTheLiveCluster(t *testing.T) {
	cases := []struct {
		name       string
		stored     map[string]string
		live       map[string]string
		wantResult confinementResult
		why        string
	}{
		{
			name:       "operator just took the node OUT of scope",
			stored:     map[string]string{"blast": "yes", "kubernetes.io/hostname": "n1"},
			live:       map[string]string{"kubernetes.io/hostname": "n1"},
			wantResult: confinementOutOfScope,
			why:        "a cached label that still matches lets a destructive step run on a node the operator just removed from the blast radius",
		},
		{
			name:       "operator just brought the node INTO scope",
			stored:     map[string]string{"kubernetes.io/hostname": "n1"},
			live:       map[string]string{"blast": "yes", "kubernetes.io/hostname": "n1"},
			wantResult: confinementAllowed,
			why:        "a stale cache refuses — and out-of-scope quarantines, so the incident never retries",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st, err := storesqlite.Open(":memory:")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = st.Close() })
			ctx := context.Background()
			if err := st.UpsertNode(ctx, &types.Node{
				Name: "n1", UID: "n1-uid", Labels: tc.stored, AgentLastSeen: time.Now(),
			}); err != nil {
				t.Fatal(err)
			}
			log := slog.New(slog.NewTextHandler(io.Discard, nil))
			c := New(st, st, nil,
				safety.NewGate(safety.Limits{MaxConcurrentRemediations: 2}),
				nil, &livePlatform{labels: tc.live}, nil, &notify.Log{Logger: log}, log)
			c.mutateRuntimeConfig(func(rc *RuntimeConfig) {
				rc.DestructiveSelector = map[string]string{"blast": "yes"}
			})

			inc := &types.Incident{
				ID: "inc-1", Target: types.Target{Node: "n1"}, Class: types.ClassFellOffBus,
				State: types.StateEvaluating,
			}
			step := &playbook.Step{Name: "cordon", Action: "platform.cordon"}
			_, got := c.destructiveStepConfinement(ctx, inc, step)
			if got != tc.wantResult {
				t.Fatalf("confinement = %v, want %v — %s", got, tc.wantResult, tc.why)
			}
		})
	}
}

// labelerPlatform can read one exact node's labels without going through the
// GPU-filtered inventory, and reports an EMPTY GPU inventory — a node whose
// device plugin is down, or simply a node with no GPUs.
type labelerPlatform struct {
	platform.Platform
	labels map[string]string
}

func (p *labelerPlatform) Name() string { return "labeler" }
func (p *labelerPlatform) ListNodes(context.Context) ([]types.Node, error) {
	return nil, nil // the GPU inventory does not have it
}
func (p *labelerPlatform) NodeLabels(_ context.Context, node string) (map[string]string, bool, error) {
	if node != "n1" {
		return nil, false, nil
	}
	return p.labels, true, nil
}

// TestConfinementResolvesForANodeOutsideTheGPUInventory covers the failure a
// real cluster showed and no unit test had: confinement asked the GPU-filtered
// inventory what labels a node carries.
//
// A node that has dropped out of that inventory — a device plugin restarting,
// a driver reloading, which are exactly the conditions remediation exists for
// — became permanently UNRESOLVABLE, so every destructive step on it held
// forever and the machine sat cordoned and drained with nothing to advance it
// and nobody paged. The codebase had already learned this lesson once, for
// NodeExists; the same reasoning had not reached the more dangerous caller.
func TestConfinementResolvesForANodeOutsideTheGPUInventory(t *testing.T) {
	st, err := storesqlite.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ctx := context.Background()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	c := New(st, st, nil,
		safety.NewGate(safety.Limits{MaxConcurrentRemediations: 2}),
		nil, &labelerPlatform{labels: map[string]string{"blast": "yes"}}, nil,
		&notify.Log{Logger: log}, log)
	c.mutateRuntimeConfig(func(rc *RuntimeConfig) {
		rc.DestructiveSelector = map[string]string{"blast": "yes"}
	})

	inc := &types.Incident{
		ID: "inc-1", Target: types.Target{Node: "n1"}, Class: types.ClassFellOffBus,
		State: types.StateEvaluating,
	}
	step := &playbook.Step{Name: "cordon", Action: "platform.cordon"}
	reason, got := c.destructiveStepConfinement(ctx, inc, step)
	if got != confinementAllowed {
		t.Fatalf("confinement = %v (%s), want allowed: the node carries the selector label, "+
			"and being absent from the GPU inventory must not make its blast radius unknowable", got, reason)
	}
}
