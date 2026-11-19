// SPDX-License-Identifier: Apache-2.0

package kubernetes

import (
	"context"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"

	"github.com/kubeneuron/kubeneuron/internal/platform"
	"github.com/kubeneuron/kubeneuron/pkg/cloud"
)

var (
	_ platform.SuccessorNodeRecycler = (*Platform)(nil)
	_ platform.PoolNodeLister        = (*Platform)(nil)
)

func (p *Platform) successorProvider() (cloud.SuccessorProvider, error) {
	provider, ok := p.recycler.(cloud.SuccessorProvider)
	declared, hasCaps := p.recycler.(interface{ Capabilities() cloud.Capabilities })
	if !ok || !hasCaps || !declared.Capabilities().AddFirst {
		return nil, fmt.Errorf("%w: the cloud provider cannot prepare a successor", cloud.ErrAddFirstUnavailable)
	}
	return provider, nil
}

func (p *Platform) AddFirstAvailable(ctx context.Context, pin cloud.NodeIdentity) error {
	if p.recycler == nil {
		return fmt.Errorf("%w: no cloud provider is configured", cloud.ErrAddFirstUnavailable)
	}
	provider, err := p.successorProvider()
	if err != nil {
		return err
	}
	if err := pin.Validate(); err != nil {
		return err
	}
	return provider.CheckAddFirst(ctx, pin.InstanceID)
}

var _ platform.RemoveFirstChecker = (*Platform)(nil)

// RemoveFirstAvailable asks a provider that implements
// cloud.RemoveFirstChecker; any other provider allows remove-first wherever
// it allows Replace.
func (p *Platform) RemoveFirstAvailable(ctx context.Context, pin cloud.NodeIdentity) error {
	checker, ok := p.recycler.(cloud.RemoveFirstChecker)
	if !ok {
		return nil
	}
	if err := pin.Validate(); err != nil {
		return err
	}
	return checker.CheckRemoveFirst(ctx, pin.InstanceID)
}

func (p *Platform) PrepareSuccessor(ctx context.Context, pin cloud.NodeIdentity, s cloud.Successor) (cloud.Successor, error) {
	if p.recycler == nil {
		return s, fmt.Errorf("%w: no cloud provider is configured", cloud.ErrAddFirstUnavailable)
	}
	provider, err := p.successorProvider()
	if err != nil {
		return s, err
	}
	if s.Instance != pin.InstanceID {
		return s, fmt.Errorf("successor operation %s replaces %s, not the pinned %s", s.Operation, s.Instance, pin.InstanceID)
	}
	if s.Phase == cloud.SuccessorPlanned {
		// The request is the step that costs money and changes the pool.
		v, err := p.RecoveryEligibility(ctx, pin)
		if err != nil {
			return s, err
		}
		if !v.ReplaceAllowed {
			return s, fmt.Errorf("cloud replacement unavailable: %s", v.Reason)
		}
	}
	return provider.PrepareSuccessor(ctx, s)
}

func (p *Platform) SuccessorNode(ctx context.Context, providerID string) (platform.SuccessorNode, error) {
	if providerID == "" {
		return platform.SuccessorNode{}, nil
	}
	nodes, err := p.cachedNodes(ctx)
	if err != nil {
		return platform.SuccessorNode{}, err
	}
	var found *corev1.Node
	for _, n := range nodes {
		// Clouds differ from Kubernetes in case: Azure resource groups, for
		// one, are case-insensitive and spelled differently in each.
		if !strings.EqualFold(n.Spec.ProviderID, providerID) {
			continue
		}
		if found != nil {
			return platform.SuccessorNode{}, fmt.Errorf("two nodes carry providerID %s", providerID)
		}
		found = n
	}
	if found == nil {
		return platform.SuccessorNode{}, nil
	}
	return successorNodeOf(found), nil
}

func (p *Platform) PoolNodes(ctx context.Context, label, pool string) ([]platform.SuccessorNode, error) {
	if label == "" || pool == "" {
		return nil, nil
	}
	nodes, err := p.cachedNodes(ctx)
	if err != nil {
		return nil, err
	}
	var out []platform.SuccessorNode
	for _, n := range nodes {
		if n.Labels[label] == pool {
			out = append(out, successorNodeOf(n))
		}
	}
	return out, nil
}

// cachedNodes lists the nodes from the watch cache, or from the API server
// before the cache has started.
func (p *Platform) cachedNodes(ctx context.Context) ([]*corev1.Node, error) {
	if lp := p.nodeLister.Load(); lp != nil {
		if cached, err := (*lp).List(labels.Everything()); err == nil {
			return cached, nil
		}
	}
	list, err := p.client.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	nodes := make([]*corev1.Node, 0, len(list.Items))
	for i := range list.Items {
		nodes = append(nodes, &list.Items[i])
	}
	return nodes, nil
}

func successorNodeOf(n *corev1.Node) platform.SuccessorNode {
	out := platform.SuccessorNode{
		Found: true, Name: n.Name, UID: string(n.UID), CreatedAt: n.CreationTimestamp.Time,
		Unschedulable: n.Spec.Unschedulable, Allocatable: extendedResourceValues(n.Status.Allocatable),
	}
	for _, condition := range n.Status.Conditions {
		if condition.Type == corev1.NodeReady {
			out.Ready = condition.Status == corev1.ConditionTrue
		}
	}
	return out
}

func (p *Platform) ReleaseSuccessor(ctx context.Context, s cloud.Successor) error {
	if p.recycler == nil {
		return fmt.Errorf("no cloud provider is configured")
	}
	provider, err := p.successorProvider()
	if err != nil {
		return err
	}
	return provider.ReleaseSuccessor(ctx, s)
}

func (p *Platform) ReplacePinnedNodeWithSuccessor(ctx context.Context, pin cloud.NodeIdentity, s cloud.Successor) error {
	if p.recycler == nil {
		return fmt.Errorf("no cloud provider is configured")
	}
	provider, err := p.successorProvider()
	if err != nil {
		return err
	}
	if s.Instance != pin.InstanceID || s.Phase != cloud.SuccessorCreated {
		return fmt.Errorf("successor operation %s has not created the successor of %s", s.Operation, pin.InstanceID)
	}
	v, err := p.RecoveryEligibility(ctx, pin)
	if err != nil {
		return err
	}
	if !v.ReplaceAllowed {
		return fmt.Errorf("cloud replacement unavailable: %s", v.Reason)
	}
	return provider.ReplaceWithSuccessor(ctx, s)
}
