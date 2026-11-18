// SPDX-License-Identifier: Apache-2.0

package kubernetes

import (
	"context"
	"sort"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"

	"github.com/kubeneuron/kubeneuron/internal/interruption"
	"github.com/kubeneuron/kubeneuron/pkg/cloud"
)

// cloudQueue is the configured provider's interruption queue, if it has one.
func (p *Platform) cloudQueue() (cloud.InterruptionQueue, bool) {
	if p.recycler == nil {
		return nil, false
	}
	q, ok := p.recycler.(cloud.InterruptionQueue)
	return q, ok
}

// CloudInterruptionQueueConfigured reports whether the cloud provider reads
// announced interruptions from a queue.
func (p *Platform) CloudInterruptionQueueConfigured() bool {
	_, ok := p.cloudQueue()
	return ok
}

// ReceiveCloudInterruptions waits for the provider's queue to deliver new
// notices; see cloud.InterruptionQueue.
func (p *Platform) ReceiveCloudInterruptions(ctx context.Context) (bool, error) {
	q, ok := p.cloudQueue()
	if !ok {
		return false, nil
	}
	return q.ReceiveInterruptions(ctx)
}

// AckCloudInterruption tells the provider's queue that a notice needs nothing
// more.
func (p *Platform) AckCloudInterruption(ctx context.Context, key string) error {
	q, ok := p.cloudQueue()
	if !ok {
		return nil
	}
	return q.AckInterruption(ctx, key)
}

// CloudInterruptions returns the notices the provider's queue holds for the
// cluster's nodes, named by node. A node whose provider ID the provider does
// not recognise is left out; the provider judges a notice for an instance
// that is not among the nodes foreign.
func (p *Platform) CloudInterruptions(ctx context.Context) ([]interruption.Notice, error) {
	q, ok := p.cloudQueue()
	if !ok {
		return nil, nil
	}
	nodes, err := p.interruptionNodes(ctx)
	if err != nil {
		return nil, err
	}
	var refs []cloud.NodeRef
	byInstance := map[string]string{}
	for _, n := range nodes {
		id, err := p.recycler.InstanceID(n.Spec.ProviderID)
		if err != nil {
			continue
		}
		byInstance[id] = n.Name
		refs = append(refs, cloud.NodeRef{Identity: cloud.NodeIdentity{
			Node: n.Name, NodeUID: string(n.UID), ProviderID: n.Spec.ProviderID,
			InstanceID: id, BootID: n.Status.NodeInfo.BootID,
		}})
	}
	got, err := q.Interruptions(ctx, refs)
	var out []interruption.Notice
	for _, i := range got {
		if node, ok := byInstance[i.InstanceID]; ok {
			out = append(out, interruption.Notice{Node: node, Interruption: i})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out, err
}

// interruptionNodes are the cluster's nodes, from the node cache when it
// holds any, else from a live List: an empty cache may not have synced.
func (p *Platform) interruptionNodes(ctx context.Context) ([]*corev1.Node, error) {
	if lp := p.nodeLister.Load(); lp != nil {
		if cached, err := (*lp).List(labels.Everything()); err == nil && len(cached) > 0 {
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
