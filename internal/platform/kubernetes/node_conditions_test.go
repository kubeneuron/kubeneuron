// SPDX-License-Identifier: Apache-2.0

package kubernetes

import (
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
	"testing"
)

func TestNodeConditionsContext(t *testing.T) {
	n := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "worker", UID: "uid"}, Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionFalse}, {Type: corev1.NodeDiskPressure, Status: corev1.ConditionTrue}}}}
	client := fake.NewSimpleClientset(n)
	p := &Platform{client: client}
	snap, err := p.NodeConditions(t.Context(), "worker")
	if err != nil {
		t.Fatal(err)
	}
	if snap.NodeUID != "uid" || snap.ObservedAt.IsZero() || len(snap.Conditions) != 5 {
		t.Fatalf("snapshot %+v", snap)
	}
	for i, want := range []string{"False", "Unknown", "True", "Unknown", "Unknown"} {
		if snap.Conditions[i].Status != want {
			t.Errorf("condition %+v want %s", snap.Conditions[i], want)
		}
	}
	if acts := client.Actions(); len(acts) != 1 || acts[0].GetVerb() != "get" {
		t.Fatalf("actions %v", acts)
	}
	if snap, err := p.NodeConditions(t.Context(), "absent"); snap != nil || !apierrors.IsNotFound(err) {
		t.Fatal("missing node returned an observation")
	}
}
