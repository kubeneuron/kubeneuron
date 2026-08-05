package kubernetes

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/kubeneuron/kubeneuron/internal/platform"
)

var _ platform.NodeRecycler = (*Platform)(nil)

// SetInstanceRecycler wires a cloud provider that can stop/start or terminate
// the instance behind a node. Without it, RecycleNode and ReplaceNode fail
// closed with a clear message, and CloudRecyclingConfigured reports false so the
// capability is never advertised.
func (p *Platform) SetInstanceRecycler(r platform.InstanceRecycler) {
	p.recycler = r
}

// CloudRecyclingConfigured reports whether a cloud provider is available.
func (p *Platform) CloudRecyclingConfigured() bool { return p.recycler != nil }

// CheckRecycleNode reports whether the node's instance can actually be
// recycled (see platform.NodeRecycler); the verdict comes from the provider.
func (p *Platform) CheckRecycleNode(ctx context.Context, node string) error {
	id, err := p.instanceID(ctx, node)
	if err != nil {
		return err
	}
	return p.recycler.CheckRecycle(ctx, id)
}

// RecycleNode stops and starts the node's cloud instance, re-checking
// per-instance viability first so a stop is never issued against an instance
// whose node group would terminate it mid-recycle.
func (p *Platform) RecycleNode(ctx context.Context, node string) error {
	id, err := p.instanceID(ctx, node)
	if err != nil {
		return err
	}
	if err := p.recycler.CheckRecycle(ctx, id); err != nil {
		return err
	}
	return p.recycler.Recycle(ctx, id)
}

// ReplaceNode terminates the node's cloud instance.
func (p *Platform) ReplaceNode(ctx context.Context, node string) error {
	id, err := p.instanceID(ctx, node)
	if err != nil {
		return err
	}
	return p.recycler.Replace(ctx, id)
}

// NodeReady reports whether the node's Kubernetes Ready condition is True. A
// missing node is not ready (it may be mid-replacement); the caller decides how
// long to wait.
func (p *Platform) NodeReady(ctx context.Context, node string) (bool, error) {
	n, err := p.client.CoreV1().Nodes().Get(ctx, node, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, err
	}
	for _, c := range n.Status.Conditions {
		if c.Type == "Ready" {
			return c.Status == "True", nil
		}
	}
	return false, nil
}

// instanceID resolves the cloud instance ID from the Node's providerID. It
// reads the live Node so a stale inventory copy cannot recycle the wrong
// instance, delegates the providerID parsing to the configured provider (so no
// cloud-specific scheme lives here), and fails closed when no provider is set.
func (p *Platform) instanceID(ctx context.Context, node string) (string, error) {
	if p.recycler == nil {
		return "", fmt.Errorf("no cloud provider is configured; RecycleNode/ReplaceNode need one")
	}
	n, err := p.client.CoreV1().Nodes().Get(ctx, node, metav1.GetOptions{})
	if err != nil {
		return "", err
	}
	id, err := p.recycler.InstanceID(n.Spec.ProviderID)
	if err != nil {
		return "", fmt.Errorf("node %s: %w", node, err)
	}
	return id, nil
}
