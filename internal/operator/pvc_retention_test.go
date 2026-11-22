// SPDX-License-Identifier: Apache-2.0

package operator

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestRetainedStateSurvivesReconcileAndIsAdoptedOnReinstall(t *testing.T) {
	ctx := context.Background()
	scheme := newOperatorScheme(t)
	root := testKubeNeuron()
	root.UID = types.UID("original-installation")
	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	r := &KubeNeuronReconciler{Client: c, Scheme: scheme}
	wanted, err := controllerPVC(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.ensurePersistentVolumeClaim(ctx, root, wanted); err != nil {
		t.Fatal(err)
	}
	var claim corev1.PersistentVolumeClaim
	key := client.ObjectKeyFromObject(wanted)
	if err := c.Get(ctx, key, &claim); err != nil {
		t.Fatal(err)
	}
	claim.Spec.VolumeName = "bound-volume"
	claim.Annotations[retainedForNameAnnotation] = root.Name
	claim.Annotations[retainedForUIDAnnotation] = string(root.UID)
	if err := c.Update(ctx, &claim); err != nil {
		t.Fatal(err)
	}
	// Repeated reconciles, including ones holding the old root, cannot reattach.
	for i := 0; i < 3; i++ {
		if err := r.ensurePersistentVolumeClaim(ctx, root, wanted); err != nil {
			t.Fatal(err)
		}
		if err := c.Get(ctx, key, &claim); err != nil {
			t.Fatal(err)
		}
		if len(claim.OwnerReferences) != 0 || claim.Annotations[retainedForUIDAnnotation] != string(root.UID) || claim.Spec.VolumeName != "bound-volume" {
			t.Fatalf("retention lost: %#v", claim)
		}
	}
	root.UID = types.UID("replacement-installation")
	if err := r.ensurePersistentVolumeClaim(ctx, root, wanted); err != nil {
		t.Fatal(err)
	}
	if err := c.Get(ctx, key, &claim); err != nil {
		t.Fatal(err)
	}
	if !metav1.IsControlledBy(&claim, root) || claim.Spec.VolumeName != "bound-volume" || claim.Annotations[retainedForUIDAnnotation] != "" {
		t.Fatalf("reinstall did not adopt retained state: %#v", claim)
	}
}

func TestRetainedStateNeverAdoptsForeignOrUnmarkedClaims(t *testing.T) {
	for _, tc := range []struct {
		name, markerName, markerUID string
		foreignOwner                bool
	}{
		{name: "unmarked"},
		{name: "different installation", markerName: "another", markerUID: "old"},
		{name: "missing UID", markerName: "fleet"},
		{name: "foreign owner", markerName: "fleet", markerUID: "old", foreignOwner: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			scheme := newOperatorScheme(t)
			root := testKubeNeuron()
			root.Name = "fleet"
			root.UID = "new"
			claim, err := controllerPVC(root)
			if err != nil {
				t.Fatal(err)
			}
			claim.Annotations[retainedForNameAnnotation] = tc.markerName
			claim.Annotations[retainedForUIDAnnotation] = tc.markerUID
			if tc.foreignOwner {
				claim.OwnerReferences = []metav1.OwnerReference{{APIVersion: "v1", Kind: "ConfigMap", Name: "foreign", UID: "foreign"}}
			}
			c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(claim).Build()
			r := &KubeNeuronReconciler{Client: c, Scheme: scheme}
			if err := r.ensurePersistentVolumeClaim(ctx, root, claim); err == nil {
				t.Fatal("adopted foreign or unmarked state")
			}
		})
	}
}
