// SPDX-License-Identifier: Apache-2.0

package kubernetes

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	corev1 "k8s.io/api/core/v1"

	"github.com/kubeneuron/kubeneuron/pkg/cloud"
	"github.com/kubeneuron/kubeneuron/pkg/types"
)

const (
	// tpuResource is the extended resource of a GKE TPU node.
	tpuResource = "google.com/tpu"
	// labelTPUAccelerator is the GKE label naming a TPU node's accelerator.
	labelTPUAccelerator = "cloud.google.com/gke-tpu-accelerator"
	// cloudHealthTimeout bounds one read of the cloud's monitoring, so a
	// call that hangs does not stop the health loop.
	cloudHealthTimeout = 20 * time.Second
)

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
	nodes, err := p.allNodes(ctx)
	if err != nil {
		return nil, err
	}
	readAt := time.Now()
	var out []types.NodeHealthSnapshot
	ids := map[string]string{}
	var instances []string
	for _, n := range nodes {
		if !isTPUNode(n) {
			continue
		}
		// Only the cloud's read is evaluated here; the Node's own signals
		// are the Kubernetes source's.
		snap := types.NodeHealthSnapshot{Node: n.Name, UID: string(n.UID), Labels: n.Labels, Devices: nodeDevices(n),
			ReadAt: readAt, ProviderID: n.Spec.ProviderID, CreatedAt: n.CreationTimestamp.Time}
		if id, err := p.recycler.InstanceID(snap.ProviderID); err == nil {
			ids[snap.Node] = id
			instances = append(instances, id)
		}
		out = append(out, snap)
	}
	if len(out) == 0 {
		return nil, nil
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Node < out[j].Node })
	points, err := p.readCloudHealth(ctx, r, instances)
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

// readCloudHealth asks the cloud, for at most cloudHealthTimeout. A call
// that outlasts it, even one that ignores its context, is an api-error.
func (p *Platform) readCloudHealth(ctx context.Context, r cloud.AcceleratorHealthReader, instances []string) (map[string][]cloud.HealthPoint, error) {
	timeout := p.cloudHealthTimeout
	if timeout <= 0 {
		timeout = cloudHealthTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	type answer struct {
		points map[string][]cloud.HealthPoint
		err    error
	}
	done := make(chan answer, 1)
	go func() {
		points, err := r.AcceleratorHealth(ctx, instances)
		done <- answer{points, err}
	}()
	select {
	case a := <-done:
		return a.points, a.err
	case <-ctx.Done():
		return nil, &cloud.HealthReadError{Reason: cloud.HealthReadAPIError,
			Err: fmt.Errorf("no answer from the cloud's monitoring within %s: %w", timeout, ctx.Err())}
	}
}

// isTPUNode reports whether the Node is a GKE TPU node: it lists the TPU
// resource, even at zero (a failed chip can take the count down), or
// carries GKE's TPU accelerator label. The cloud's read must go on for the
// node exactly when its devices go missing.
func isTPUNode(n *corev1.Node) bool {
	if _, ok := n.Status.Capacity[tpuResource]; ok {
		return true
	}
	return n.Labels[labelTPUAccelerator] != ""
}
