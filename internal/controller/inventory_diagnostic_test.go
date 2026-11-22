// SPDX-License-Identifier: Apache-2.0
package controller

import (
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/utils/ptr"
)

// The agent DaemonSet's arguments say how its agents read the GPUs, and so
// what an empty inventory means.
func TestInventoryDiagnosticFromTheAgentDaemonSet(t *testing.T) {
	for args, want := range map[string][]string{
		"RuntimeDidNotExposeGPUs":  {"--nvidia-observation", "--nvidia-runtime-devices"},
		"DeviceAccessDisabled":     {"--nvidia-observation"},
		"HostToolingNotConfigured": {"--nvidia-observation", "--enable-node-level-actions"},
		"":                         {"--require-real-driver", "--enable-node-level-actions"},
	} {
		t.Run(args, func(t *testing.T) {
			ds := &appsv1.DaemonSet{
				ObjectMeta: metav1.ObjectMeta{Name: "fleet-agent", Namespace: "kube-neuron", OwnerReferences: []metav1.OwnerReference{
					{APIVersion: "kubeneuron.io/v1alpha1", Kind: "KubeNeuron", Name: "fleet", UID: "uid-a", Controller: ptr.To(true)}}},
				Spec: appsv1.DaemonSetSpec{
					Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "agent"}},
					Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "agent", Args: want}}}},
				},
			}
			c := &Controller{}
			c.ConfigureInventoryDiagnostics(fake.NewClientset(ds), "kube-neuron", "fleet", "uid-a")
			got, err := c.inventoryDiagnostic(t.Context())
			if err != nil || got != args {
				t.Fatalf("diagnostic = %q, %v; want %q", got, err, args)
			}
		})
	}
}
