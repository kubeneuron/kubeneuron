// SPDX-License-Identifier: Apache-2.0

package kubernetes

import (
	"context"
	"sort"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"

	"github.com/kubeneuron/kubeneuron/internal/interruption"
)

// NodeInterruptions returns the interruptions announced on the cluster's
// nodes by the cloud's own Kubernetes components (interruption.FromNode),
// from the node cache when it has synced, else from a live List. Every node
// is read, not only those advertising an accelerator: a node under host
// maintenance may already have lost its device plugin.
func (p *Platform) NodeInterruptions(ctx context.Context) ([]interruption.Notice, error) {
	var nodes []*corev1.Node
	if lp := p.nodeLister.Load(); lp != nil {
		if cached, err := (*lp).List(labels.Everything()); err == nil {
			nodes = cached
		}
	}
	if nodes == nil {
		list, err := p.client.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
		if err != nil {
			return nil, err
		}
		for i := range list.Items {
			nodes = append(nodes, &list.Items[i])
		}
	}
	var out []interruption.Notice
	for _, n := range nodes {
		out = append(out, interruption.FromNode(interruptionNode(n))...)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out, nil
}

func interruptionNode(n *corev1.Node) interruption.Node {
	out := interruption.Node{Name: n.Name, ProviderID: n.Spec.ProviderID, Labels: n.Labels}
	for _, t := range n.Spec.Taints {
		out.Taints = append(out.Taints, interruption.Taint{Key: t.Key, Value: t.Value})
	}
	for _, c := range n.Status.Conditions {
		out.Conditions = append(out.Conditions, interruption.Condition{
			Type: string(c.Type), Status: string(c.Status), LastTransition: c.LastTransitionTime.Time,
		})
	}
	return out
}
