// SPDX-License-Identifier: Apache-2.0

package kubernetes

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
	corev1listers "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"

	"github.com/kubeneuron/kubeneuron/internal/interruption"
	"github.com/kubeneuron/kubeneuron/pkg/cloud"
)

func TestNodeInterruptionsReadsEveryNode(t *testing.T) {
	spot := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "spot"},
		Spec: corev1.NodeSpec{ProviderID: "aws:///us-east-1a/i-1", Taints: []corev1.Taint{
			{Key: "aws-node-termination-handler/spot-itn", Value: "e1", Effect: corev1.TaintEffectNoSchedule},
		}},
	}
	quiet := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "quiet"}}
	p := &Platform{client: fake.NewSimpleClientset(spot, quiet)}
	got, err := p.NodeInterruptions(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Node != "spot" || got[0].Kind != cloud.InterruptionSpotReclaim ||
		got[0].Source != interruption.SourceNTHSpot || got[0].InstanceID != "aws:///us-east-1a/i-1" {
		t.Fatalf("%+v", got)
	}
}

// The node cache is published only once it has synced. An empty cache, as
// one published early would be, must not read as "no notices": that would
// close every interruption incident and release its cordon after a restart.
func TestNodeInterruptionsDoesNotTrustAnEmptyCache(t *testing.T) {
	spot := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "spot"},
		Spec: corev1.NodeSpec{Taints: []corev1.Taint{
			{Key: "aws-node-termination-handler/spot-itn", Value: "e1", Effect: corev1.TaintEffectNoSchedule},
		}},
	}
	p := &Platform{client: fake.NewSimpleClientset(spot)}
	empty := corev1listers.NewNodeLister(cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{}))
	p.nodeLister.Store(&empty)
	got, err := p.NodeInterruptions(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Node != "spot" {
		t.Fatalf("an empty cache hid the notice: %+v", got)
	}
}

// Before the cache syncs, StartNodeCache publishes nothing and the notices
// come from a live List.
func TestNodeInterruptionsBeforeTheCacheSyncs(t *testing.T) {
	spot := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "spot"},
		Spec: corev1.NodeSpec{Taints: []corev1.Taint{
			{Key: "aws-node-termination-handler/spot-itn", Value: "e1", Effect: corev1.TaintEffectNoSchedule},
		}},
	}
	p := &Platform{client: fake.NewSimpleClientset(spot)}
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // the informer never syncs
	p.StartNodeCache(ctx)
	if p.nodeLister.Load() != nil {
		t.Fatal("a cache that never synced was published")
	}
	got, err := p.NodeInterruptions(context.Background())
	if err != nil || len(got) != 1 {
		t.Fatalf("notices %+v, err %v; want the live one", got, err)
	}
}
