// SPDX-License-Identifier: Apache-2.0

package kubernetes

import (
	"context"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/kubeneuron/kubeneuron/internal/evidence"
)

// NodeEvidence reads the node's extended resources and the pods on it from the
// API, for the incident evidence block. A failed pod list is carried in the
// result rather than returned, so the resources still show.
func (p *Platform) NodeEvidence(ctx context.Context, name string) (*evidence.Node, error) {
	node, err := p.client.CoreV1().Nodes().Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	out := &evidence.Node{
		Capacity:    quantities(node.Status.Capacity),
		Allocatable: quantities(node.Status.Allocatable),
	}
	pods, err := p.nodePods(ctx, name)
	if err != nil {
		out.PodsErr = err
		return out, nil
	}
	for i := range pods {
		pod := &pods[i]
		ready := false
		for _, c := range pod.Status.Conditions {
			if c.Type == corev1.PodReady && c.Status == corev1.ConditionTrue {
				ready = true
			}
		}
		out.Pods = append(out.Pods, evidence.Pod{
			Name: pod.Name, Namespace: pod.Namespace, Labels: pod.Labels,
			Running: pod.Status.Phase == corev1.PodRunning, Ready: ready,
		})
	}
	return out, nil
}

func quantities(list corev1.ResourceList) map[string]int64 {
	out := make(map[string]int64, len(list))
	for name, q := range list {
		out[string(name)] = q.Value()
	}
	return out
}
