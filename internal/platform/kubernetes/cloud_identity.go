// SPDX-License-Identifier: Apache-2.0

package kubernetes

import (
	"context"
	"fmt"
	"maps"

	"github.com/kubeneuron/kubeneuron/pkg/cloud"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// CloudNode takes all fields from one live Node read, not an inventory cache.
func (p *Platform) CloudNode(ctx context.Context, name string) (cloud.NodeRef, error) {
	if p.recycler == nil {
		return cloud.NodeRef{}, fmt.Errorf("no cloud provider is configured")
	}
	n, err := p.client.CoreV1().Nodes().Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return cloud.NodeRef{}, err
	}
	id, err := p.recycler.InstanceID(n.Spec.ProviderID)
	if err != nil {
		return cloud.NodeRef{}, err
	}
	ref := cloud.NodeRef{Identity: cloud.NodeIdentity{
		Node: n.Name, NodeUID: string(n.UID), ProviderID: n.Spec.ProviderID,
		InstanceID: id, BootID: n.Status.NodeInfo.BootID,
	}, Labels: maps.Clone(n.Labels), Annotations: maps.Clone(n.Annotations)}
	if err := ref.Identity.Validate(); err != nil {
		return cloud.NodeRef{}, err
	}
	for _, condition := range n.Status.Conditions {
		if condition.Type == corev1.NodeReady {
			ref.Ready = condition.Status == corev1.ConditionTrue
		}
	}
	return ref, nil
}
