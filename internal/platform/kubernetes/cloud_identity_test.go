// SPDX-License-Identifier: Apache-2.0
package kubernetes

import (
	"context"
	"errors"
	"github.com/kubeneuron/kubeneuron/internal/platform"
	"github.com/kubeneuron/kubeneuron/pkg/cloud"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
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

// Only the Kubernetes API's NotFound for the live read says that no Node of
// the name exists; a read that fails otherwise, or no cloud provider to read
// it with, says nothing about the Node.
func TestCloudNodeTellsANodeThatDoesNotExist(t *testing.T) {
	ctx := context.Background()
	p := &Platform{client: fake.NewSimpleClientset(), recycler: &fakeRecycler{}}
	_, err := p.CloudNode(ctx, "worker")
	if !errors.Is(err, platform.ErrNodeNotFound) || !apierrors.IsNotFound(err) || err.Error() != `nodes "worker" not found` {
		t.Fatalf("read of a missing Node failed with %v", err)
	}

	nodes := schema.GroupResource{Resource: "nodes"}
	for name, readErr := range map[string]error{
		"timeout":   apierrors.NewTimeoutError("the read took too long", 2),
		"forbidden": apierrors.NewForbidden(nodes, "worker", errors.New("no access")),
		"transport": errors.New("connection refused"),
	} {
		t.Run(name, func(t *testing.T) {
			client := fake.NewSimpleClientset()
			client.PrependReactor("get", "nodes", func(k8stesting.Action) (bool, runtime.Object, error) {
				return true, nil, readErr
			})
			p := &Platform{client: client, recycler: &fakeRecycler{}}
			if _, err := p.CloudNode(ctx, "worker"); err == nil || errors.Is(err, platform.ErrNodeNotFound) {
				t.Fatalf("read failed with %v, want an error that says nothing about the Node", err)
			}
		})
	}

	unconfigured := &Platform{client: fake.NewSimpleClientset()}
	if _, err := unconfigured.CloudNode(ctx, "worker"); err == nil || errors.Is(err, platform.ErrNodeNotFound) {
		t.Fatalf("read without a cloud provider failed with %v", err)
	}
}

func TestMatchCloudNodeUID(t *testing.T) {
	for _, tc := range []struct {
		name, uid, providerID string
		changed, valid        bool
	}{
		{"matched", "old", "cloud:///zone/instance", false, true},
		{"pending", "old", "", false, false},
		{"recreated", "new", "", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			n := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "worker", UID: types.UID(tc.uid)}, Spec: corev1.NodeSpec{ProviderID: tc.providerID}, Status: corev1.NodeStatus{NodeInfo: corev1.NodeSystemInfo{BootID: "current"}}}
			client := fake.NewSimpleClientset(n)
			p := &Platform{client: client, recycler: &fakeRecycler{}}
			pin, err := p.MatchCloudNodeUID(context.Background(), n.Name, "old")
			if (err == nil) != tc.valid || errors.Is(err, cloud.ErrIdentityChanged) != tc.changed || (tc.valid && pin.BootID != "current") {
				t.Fatalf("pin=%+v err=%v", pin, err)
			}
			if actions := client.Actions(); len(actions) != 1 || !actions[0].Matches("get", "nodes") {
				t.Fatalf("actions=%v", actions)
			}
		})
	}
}
