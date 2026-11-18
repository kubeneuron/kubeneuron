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

var _ platform.SuccessorNodeRecycler = (*Platform)(nil)

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
	var nodes []*corev1.Node
	if lp := p.nodeLister.Load(); lp != nil {
		cached, err := (*lp).List(labels.Everything())
		if err == nil {
			nodes = cached
		}
	}
	if nodes == nil {
		list, err := p.client.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
		if err != nil {
			return platform.SuccessorNode{}, err
		}
		for i := range list.Items {
			nodes = append(nodes, &list.Items[i])
		}
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
	out := platform.SuccessorNode{
		Found: true, Name: found.Name, UID: string(found.UID), CreatedAt: found.CreationTimestamp.Time,
		Unschedulable: found.Spec.Unschedulable, Allocatable: extendedResourceValues(found.Status.Allocatable),
	}
	for _, condition := range found.Status.Conditions {
		if condition.Type == corev1.NodeReady {
			out.Ready = condition.Status == corev1.ConditionTrue
		}
	}
	return out, nil
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
