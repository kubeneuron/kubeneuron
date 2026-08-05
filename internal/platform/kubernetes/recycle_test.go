package kubernetes

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

// fakeRecycler stands in for a cloud provider. It owns the providerID parsing,
// exactly as a real provider does: no cloud-specific scheme lives in this
// package, so the fake resolves the instance ID as the final path segment of a
// deliberately provider-neutral providerID.
type fakeRecycler struct {
	recycled, replaced string
	checkErr           error
	checked            string
}

func (f *fakeRecycler) InstanceID(providerID string) (string, error) {
	segments := strings.Split(providerID, "/")
	return segments[len(segments)-1], nil
}
func (f *fakeRecycler) CheckRecycle(_ context.Context, id string) error {
	f.checked = id
	return f.checkErr
}
func (f *fakeRecycler) Recycle(_ context.Context, id string) error { f.recycled = id; return nil }
func (f *fakeRecycler) Replace(_ context.Context, id string) error { f.replaced = id; return nil }

func TestRecycleNodeResolvesInstanceAndCalls(t *testing.T) {
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "ip-10-0-0-1"},
		Spec:       corev1.NodeSpec{ProviderID: "cloud:///zone-a/i-0abc"},
	}
	rec := &fakeRecycler{}
	p := &Platform{client: fake.NewSimpleClientset(node), recycler: rec}

	if err := p.RecycleNode(context.Background(), "ip-10-0-0-1"); err != nil {
		t.Fatal(err)
	}
	if rec.recycled != "i-0abc" {
		t.Fatalf("recycled %q, want the node's instance ID", rec.recycled)
	}
	if err := p.ReplaceNode(context.Background(), "ip-10-0-0-1"); err != nil {
		t.Fatal(err)
	}
	if rec.replaced != "i-0abc" {
		t.Fatalf("replaced %q, want the node's instance ID", rec.replaced)
	}
}

// Without a cloud provider the platform must fail closed, never silently
// "succeed" on a node whose GPU was never cleared.
func TestRecycleWithoutCloudProviderFailsClosed(t *testing.T) {
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "n1"},
		Spec:       corev1.NodeSpec{ProviderID: "cloud:///z/i-0abc"},
	}
	p := &Platform{client: fake.NewSimpleClientset(node)}
	if p.CloudRecyclingConfigured() {
		t.Fatal("no recycler was set; must report unconfigured")
	}
	if err := p.RecycleNode(context.Background(), "n1"); err == nil {
		t.Fatal("RecycleNode must fail when no cloud provider is configured")
	}
}
