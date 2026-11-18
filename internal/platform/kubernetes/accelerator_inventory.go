// SPDX-License-Identifier: Apache-2.0

package kubernetes

import (
	"context"
	"sort"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"

	"github.com/kubeneuron/kubeneuron/pkg/types"
)

// inventoryResources are the extended resources counted as whole accelerator
// devices for a node without an agent, with the unit of one count. Partition
// resources (MIG instances, NeuronCores) are not devices and are left out.
var inventoryResources = map[corev1.ResourceName]types.DeviceUnit{
	"nvidia.com/gpu":        types.DeviceUnitGPU,
	"amd.com/gpu":           types.DeviceUnitGPU,
	"google.com/tpu":        types.DeviceUnitTPUChip,
	"aws.amazon.com/neuron": types.DeviceUnitAWSDevice,
}

// AcceleratorNodes implements platform.AcceleratorInventory: every Node that
// advertises a whole accelerator device, with its UID and the capacity and
// allocatable count of each such resource. It reads the synced node cache,
// or the API server while the cache is not synced or holds no node.
func (p *Platform) AcceleratorNodes(ctx context.Context) ([]types.Node, error) {
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
	var out []types.Node
	for _, n := range nodes {
		if devices := nodeDevices(n); len(devices) > 0 {
			out = append(out, types.Node{Name: n.Name, UID: string(n.UID), Devices: devices})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func nodeDevices(n *corev1.Node) []types.DeviceCount {
	var out []types.DeviceCount
	for name, unit := range inventoryResources {
		capacity, ok := n.Status.Capacity[name]
		if !ok || capacity.Value() <= 0 {
			continue
		}
		var allocatable int64
		if q, ok := n.Status.Allocatable[name]; ok {
			allocatable = q.Value()
		}
		out = append(out, types.DeviceCount{
			Resource: string(name), Capacity: capacity.Value(), Allocatable: allocatable, Unit: unit,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Resource < out[j].Resource })
	return out
}
