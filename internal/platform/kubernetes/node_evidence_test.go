// SPDX-License-Identifier: Apache-2.0

package kubernetes

import (
	"errors"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/kubeneuron/kubeneuron/internal/evidence"
	"github.com/kubeneuron/kubeneuron/pkg/types"
)

func TestNodeEvidenceReadsResourcesAndPods(t *testing.T) {
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "worker"}, Status: corev1.NodeStatus{
		Capacity:    corev1.ResourceList{"nvidia.com/gpu": resource.MustParse("8"), "vpc.amazonaws.com/efa": resource.MustParse("4")},
		Allocatable: corev1.ResourceList{"nvidia.com/gpu": resource.MustParse("7"), "vpc.amazonaws.com/efa": resource.MustParse("4")},
	}}
	ready := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "dp", Namespace: "gpu-operator", Labels: map[string]string{"app": "nvidia-device-plugin-daemonset"}},
		Spec:   corev1.PodSpec{NodeName: "worker"},
		Status: corev1.PodStatus{Phase: corev1.PodRunning, Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}}}
	client := fake.NewSimpleClientset(node, ready)
	p := &Platform{client: client}
	n, err := p.NodeEvidence(t.Context(), "worker")
	if err != nil {
		t.Fatal(err)
	}
	if n.Capacity["nvidia.com/gpu"] != 8 || n.Allocatable["nvidia.com/gpu"] != 7 || len(n.Pods) != 1 || !n.Pods[0].Running || !n.Pods[0].Ready {
		t.Fatalf("%+v", n)
	}
	if sw := evidence.Software(n); sw.State != types.EvidenceDegraded {
		t.Fatalf("software %+v", sw)
	}
	if net := evidence.Network(n); net.State != types.EvidencePresent {
		t.Fatalf("network %+v", net)
	}
	for _, a := range client.Actions() {
		if a.GetVerb() != "get" && a.GetVerb() != "list" {
			t.Fatalf("evidence wrote to the API: %v", a)
		}
	}

	// A failed pod list keeps the resources and says so.
	client.PrependReactor("list", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("forbidden")
	})
	n, err = p.NodeEvidence(t.Context(), "worker")
	if err != nil || n.PodsErr == nil || n.Capacity["nvidia.com/gpu"] != 8 {
		t.Fatalf("%+v %v", n, err)
	}
	if _, err := p.NodeEvidence(t.Context(), "absent"); err == nil {
		t.Fatal("missing node returned evidence")
	}
}
