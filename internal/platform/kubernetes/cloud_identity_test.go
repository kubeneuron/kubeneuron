// SPDX-License-Identifier: Apache-2.0
package kubernetes

import (
	"context"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
	"testing"
)

func TestCloudNodeReadsLiveIdentity(t *testing.T) {
	ctx := context.Background()
	n := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "worker", UID: "old-uid"}, Spec: corev1.NodeSpec{ProviderID: "cloud:///zone/old-instance"}, Status: corev1.NodeStatus{NodeInfo: corev1.NodeSystemInfo{BootID: "old-boot"}}}
	client := fake.NewSimpleClientset(n)
	p := &Platform{client: client, recycler: &fakeRecycler{}}
	before, err := p.CloudNode(ctx, n.Name)
	if err != nil {
		t.Fatal(err)
	}
	n.UID = "new-uid"
	n.Spec.ProviderID = "cloud:///zone/new-instance"
	n.Status.NodeInfo.BootID = "new-boot"
	if _, err := client.CoreV1().Nodes().Update(ctx, n, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	after, err := p.CloudNode(ctx, n.Name)
	if err != nil {
		t.Fatal(err)
	}
	if err := before.Identity.Match(after.Identity); err == nil {
		t.Fatal("stale identity matched replacement")
	}
	if after.Identity.InstanceID != "new-instance" {
		t.Fatal("identity was read from stale cache")
	}
	n.Status.NodeInfo.BootID = ""
	if _, err := client.CoreV1().Nodes().Update(ctx, n, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := p.CloudNode(ctx, n.Name); err == nil {
		t.Fatal("missing boot accepted")
	}
}
