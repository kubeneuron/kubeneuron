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

// Replace deletes the node's instance from its node pool's managed instance
// group. The controller has already cordoned and drained the node: GKE does
// neither.
//
// On a pool under the cluster autoscaler only the instance is deleted; the
// autoscaler decides whether another comes. On a fixed-size pool the delete
// shrinks the pool by one, and the pool's size is then set back through GKE,
// which brings up a new node. GKE sets that size in every zone of the pool,
// so a multi-zone pool is replaced only while all its zones are the same
// size. An instance already gone is a finished replacement.
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
	ops, err := p.api.Operations(ctx)
	if err != nil {
		return fmt.Errorf("gcp: listing GKE operations: %w", err)
	}
	if op, ok := p.running(ops, t.pool.Name); ok {
		return fmt.Errorf("gcp: node pool %s has GKE operation %s running (%s); waiting", t.pool.Name, op.Name, op.Type)
	}
	if t.pool.Autoscaling {
		if err := p.api.DeleteInstances(ctx, t.group, []string{t.name}, requestID("replace", t.id())); err != nil {
			return fmt.Errorf("gcp: deleting instance %s from node pool %s: %w", t.name, t.pool.Name, err)
		}
		return nil
	}
	size, err := p.zoneSize(ctx, t.pool)
	if err != nil {
		return err
	}
	if err := p.api.DeleteInstances(ctx, t.group, []string{t.name}, requestID("replace", t.id())); err != nil {
		return fmt.Errorf("gcp: deleting instance %s from node pool %s: %w", t.name, t.pool.Name, err)
	}
	if _, err := p.api.SetPoolSize(ctx, t.pool.Name, size); err != nil {
		if capacityError(err) {
			err = fmt.Errorf("%w: %w", cloud.ErrReplacementCapacity, err)
		}
		return fmt.Errorf("gcp: instance %s is deleted, but setting node pool %s back to %d nodes per zone failed; the pool is one node short until its size is set again: %w", t.name, t.pool.Name, size, err)
	}
	return nil
}

// zoneSize is the one size all zones of a fixed-size pool share, each settled.
func (p *Provider) zoneSize(ctx context.Context, pl nodePool) (int32, error) {
	var size int32 = -1
	for _, ref := range pl.Groups {
		g, err := p.api.Group(ctx, ref)
		if err != nil {
			return 0, fmt.Errorf("gcp: reading instance group %s: %w", ref.name, err)
		}
		if !g.Stable {
			return 0, fmt.Errorf("gcp: instance group %s of node pool %s is still changing; waiting", ref.name, pl.Name)
		}
		if size >= 0 && g.TargetSize != size {
			return 0, fmt.Errorf("gcp: the zones of node pool %s differ in size (%d and %d nodes); GKE would set every zone to one size, so replacement is unavailable", pl.Name, size, g.TargetSize)
		}
		size = g.TargetSize
	}
	if size < 0 {
		return 0, fmt.Errorf("gcp: node pool %s has no instance group", pl.Name)
	}
	return size, nil
}
