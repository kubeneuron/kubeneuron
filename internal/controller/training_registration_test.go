// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"github.com/kubeneuron/kubeneuron/internal/inventory"
	"github.com/kubeneuron/kubeneuron/internal/platform"
	"github.com/kubeneuron/kubeneuron/pkg/types"
	"testing"
	"time"
)

type trainingNodeIdentity struct {
	platform.Platform
	uid string
}

func (p trainingNodeIdentity) NodeUID(context.Context, string) (string, bool, error) {
	return p.uid, p.uid != "", nil
}

func TestTrainingNodeRequiresCurrentRegistration(t *testing.T) {
	c, st := newIngestTestController(t)
	ctx := t.Context()
	for _, tc := range []struct {
		name, uid string
		seen      time.Time
		want      bool
	}{
		{"fresh", "original", time.Now(), true}, {"replaced", "successor", time.Now(), false}, {"deleted", "", time.Now(), false},
		{"discovered-only", "original", time.Time{}, false}, {"stale", "original", time.Now().Add(-4 * inventory.Interval), false},
		{"future", "original", time.Now().Add(time.Hour), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := st.UpsertNode(ctx, &types.Node{Name: tc.name, UID: "original", Platform: "kubernetes", AgentLastSeen: tc.seen}); err != nil {
				t.Fatal(err)
			}
			c.platform = trainingNodeIdentity{uid: tc.uid}
			if got := c.TrainingNodeMonitored(ctx, tc.name); got != tc.want {
				t.Fatalf("monitored=%t want %t", got, tc.want)
			}
		})
	}
}

func TestTrainingNodeFromKubernetesInventoryRequiresLiveUID(t *testing.T) {
	c, st := newIngestTestController(t)
	if err := st.UpsertKubernetesInventory(t.Context(), &types.Node{Name: "worker", UID: "original"}); err != nil {
		t.Fatal(err)
	}
	for _, live := range []string{"original", "replacement", ""} {
		c.platform = trainingNodeIdentity{uid: live}
		if got := c.TrainingNodeMonitored(t.Context(), "worker"); got != (live == "original") {
			t.Errorf("live UID %q monitored=%t", live, got)
		}
	}
}
