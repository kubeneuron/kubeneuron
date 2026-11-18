// SPDX-License-Identifier: Apache-2.0

package kubernetes

import (
	"context"
	"errors"

	"github.com/kubeneuron/kubeneuron/pkg/cloud"
	"github.com/kubeneuron/kubeneuron/pkg/types"
)

// tpuResource is the extended resource of a GKE TPU node.
const tpuResource = "google.com/tpu"

// cloudHealth is the configured provider's accelerator health read, when it
// has one and the operator turned it on.
func (p *Platform) cloudHealth() (cloud.AcceleratorHealthReader, bool) {
	if p.recycler == nil {
		return nil, false
	}
	r, ok := p.recycler.(cloud.AcceleratorHealthReader)
	if !ok || !r.AcceleratorHealthEnabled() {
		return nil, false
	}
	return r, true
}

// CloudAcceleratorHealthEnabled reports whether accelerator health is read
// from the cloud's monitoring.
func (p *Platform) CloudAcceleratorHealthEnabled() bool {
	_, ok := p.cloudHealth()
	return ok
}

// CloudAcceleratorHealth reads, from the cloud's monitoring, the health of
// every TPU node's instance. Without a TPU node nothing is asked of the
// cloud. The projects asked are those of the nodes' provider IDs. A node
// whose provider ID names no instance has no points. When the cloud's read
// fails, its reason is set on every node, with no points, and the nodes are
// returned together with the error.
func (p *Platform) CloudAcceleratorHealth(ctx context.Context) ([]types.NodeHealthSnapshot, error) {
	r, ok := p.cloudHealth()
	if !ok {
		return nil, nil
	}
	snaps, err := p.NodeHealth(ctx)
	if err != nil {
		return nil, err
	}
	var out []types.NodeHealthSnapshot
	ids := map[string]string{}
	var instances []string
	for _, snap := range snaps {
		if !hasDevice(snap, tpuResource) {
			continue
		}
		// Only the cloud's read is evaluated here; the Node's own signals
		// are the Kubernetes source's.
		snap.Taints, snap.Conditions = nil, nil
		if id, err := p.recycler.InstanceID(snap.ProviderID); err == nil {
			ids[snap.Node] = id
			instances = append(instances, id)
		}
		out = append(out, snap)
	}
	if len(out) == 0 {
		return nil, nil
	}
	points, err := r.AcceleratorHealth(ctx, instances)
	if err != nil {
		reason := cloud.HealthReadAPIError
		var herr *cloud.HealthReadError
		if errors.As(err, &herr) {
			reason = herr.Reason
		}
		for i := range out {
			out[i].CloudError = reason
		}
		return out, err
	}
	for i := range out {
		for _, pt := range points[ids[out[i].Node]] {
			out[i].Cloud = append(out[i].Cloud, types.CloudHealthPoint{At: pt.At, Status: pt.Status, Category: pt.Category})
		}
	}
	return out, nil
}

func hasDevice(snap types.NodeHealthSnapshot, resource string) bool {
	for _, d := range snap.Devices {
		if d.Resource == resource && d.Capacity > 0 {
			return true
		}
	}
	return false
}
