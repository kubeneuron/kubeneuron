// SPDX-License-Identifier: Apache-2.0

package kubernetes

import (
	"context"
	"reflect"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8stypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/kubeneuron/kubeneuron/pkg/types"
)

func TestNodeInventoryCountsWholeDevices(t *testing.T) {
	node := func(name string, capacity, allocatable corev1.ResourceList) *corev1.Node {
		return &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name, UID: k8stypes.UID("uid-" + name), Labels: map[string]string{"pool": name}},
			Status: corev1.NodeStatus{Capacity: capacity, Allocatable: allocatable}}
	}
	q := resource.MustParse
	p := &Platform{client: fake.NewSimpleClientset(
		node("tpu", corev1.ResourceList{"google.com/tpu": q("4"), "cpu": q("8")}, corev1.ResourceList{"google.com/tpu": q("3")}),
		node("trn", corev1.ResourceList{"aws.amazon.com/neuron": q("16"), "aws.amazon.com/neuroncore": q("32"), "aws.amazon.com/neurondevice": q("16")},
			corev1.ResourceList{"aws.amazon.com/neuron": q("16")}),
		node("trn-old", corev1.ResourceList{"aws.amazon.com/neurondevice": q("1")}, corev1.ResourceList{"aws.amazon.com/neurondevice": q("1")}),
		node("mig", corev1.ResourceList{"nvidia.com/mig-1g.10gb": q("7")}, nil),
		node("cpu", corev1.ResourceList{"cpu": q("8")}, nil),
	)}
	got, err := p.NodeInventory(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := []types.Node{
		{Name: "cpu", UID: "uid-cpu", Labels: map[string]string{"pool": "cpu"}},
		{Name: "mig", UID: "uid-mig", Labels: map[string]string{"pool": "mig"}},
		{Name: "tpu", UID: "uid-tpu", Labels: map[string]string{"pool": "tpu"},
			Devices: []types.DeviceCount{{Resource: "google.com/tpu", Capacity: 4, Allocatable: 3, Unit: types.DeviceUnitTPUChip}}},
		// Both names on one node: the current one only, never twice.
		{Name: "trn", UID: "uid-trn", Labels: map[string]string{"pool": "trn"},
			Devices: []types.DeviceCount{{Resource: "aws.amazon.com/neuron", Capacity: 16, Allocatable: 16, Unit: types.DeviceUnitAWSDevice}}},
		// A device plugin before Neuron 2.20 names the devices neurondevice.
		{Name: "trn-old", UID: "uid-trn-old", Labels: map[string]string{"pool": "trn-old"},
			Devices: []types.DeviceCount{{Resource: "aws.amazon.com/neurondevice", Capacity: 1, Allocatable: 1, Unit: types.DeviceUnitAWSDevice}}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v\nwant %+v", got, want)
	}
}
