// SPDX-License-Identifier: Apache-2.0

package kubernetes

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

// The cloud and the node spell the successor's providerID in different
// case: AKS lowercases the node resource group, ARM does not.
func TestSuccessorNodeMatchesTheProviderIDInAnyCase(t *testing.T) {
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "aks-gpu-12345678-vmss000003", UID: "uid-3"},
		Spec:       corev1.NodeSpec{ProviderID: "azure:///subscriptions/s/resourceGroups/mc_ml_training_eastus/providers/Microsoft.Compute/virtualMachineScaleSets/aks-gpu-12345678-vmss/virtualMachines/3"},
		Status:     corev1.NodeStatus{Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}},
	}
	p := &Platform{client: fake.NewSimpleClientset(node)}
	got, err := p.SuccessorNode(context.Background(),
		"azure:///subscriptions/s/resourceGroups/MC_ml_training_eastus/providers/Microsoft.Compute/virtualMachineScaleSets/aks-gpu-12345678-vmss/virtualMachines/3")
	if err != nil || !got.Found || got.Name != node.Name || !got.Ready {
		t.Fatalf("SuccessorNode = %+v, %v; want %s found and Ready", got, err, node.Name)
	}
}
