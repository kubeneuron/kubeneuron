// SPDX-License-Identifier: Apache-2.0

package gcp

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"

	"github.com/kubeneuron/kubeneuron/pkg/cloud"
)

// requestNamespace scopes the request IDs this provider derives: a resent
// delete carries the same ID, so Compute Engine runs it once.
var requestNamespace = uuid.MustParse("7d0f6a3e-5b0c-4d43-9a43-2b6f5d1c9e10")

func requestID(parts ...string) string {
	key := ""
	for _, p := range parts {
		key += p + "\x00"
	}
	return uuid.NewSHA1(requestNamespace, []byte(key)).String()
}

// errFixedPoolRemoveFirst refuses remove-first on a fixed-size pool: its
// size would have to be set back in a second call, and nothing records that
// the first one was made.
var errFixedPoolRemoveFirst = errors.New("a fixed-size GKE node pool is replaced only by adding first")

var _ cloud.RemoveFirstChecker = (*Provider)(nil)

// CheckRemoveFirst refuses, before the node is cordoned, the pools Replace
// refuses: a fixed-size pool, and Autopilot. A node already gone is no
// refusal: Replace finds the replacement finished.
func (p *Provider) CheckRemoveFirst(ctx context.Context, instanceID string) error {
	t, c, err := p.resolve(ctx, instanceID)
	if errors.Is(err, errGone) {
		return nil
	}
	if err != nil {
		return err
	}
	if c.Autopilot {
		return fmt.Errorf("%w: GKE Autopilot manages its nodes; KubeNeuron only observes them", cloud.ErrRemoveFirstUnavailable)
	}
	if !t.pool.Autoscaling {
		return fmt.Errorf("%w: node pool %s: %v", cloud.ErrRemoveFirstUnavailable, t.pool.Name, errFixedPoolRemoveFirst)
	}
	return nil
}

// Replace deletes the node's instance from its node pool's managed instance
// group, on a pool under the cluster autoscaler only: the autoscaler decides
// whether another comes. The controller has already cordoned and drained the
// node: GKE does neither. A fixed-size pool is replaced only by adding first
// (ReplaceWithSuccessor), and Replace refuses it before any change. An
// instance already gone is a finished replacement.
func (p *Provider) Replace(ctx context.Context, instanceID string) error {
	t, c, err := p.resolve(ctx, instanceID)
	if errors.Is(err, errGone) {
		return nil
	}
	if err != nil {
		return err
	}
	if c.Autopilot {
		return fmt.Errorf("gcp: GKE Autopilot manages its nodes; KubeNeuron only observes them")
	}
	if !t.pool.Autoscaling {
		return fmt.Errorf("gcp: node pool %s: %w", t.pool.Name, errFixedPoolRemoveFirst)
	}
	ops, err := p.api.Operations(ctx)
	if err != nil {
		return fmt.Errorf("gcp: listing GKE operations: %w", err)
	}
	if op, ok := p.running(ops, t.pool.Name); ok {
		return fmt.Errorf("gcp: node pool %s has GKE operation %s running (%s); waiting", t.pool.Name, op.Name, op.Type)
	}
	if err := p.api.DeleteInstances(ctx, t.group, []string{t.name}, requestID("replace", t.id())); err != nil {
		return fmt.Errorf("gcp: deleting instance %s from node pool %s: %w", t.name, t.pool.Name, err)
	}
	return nil
}
