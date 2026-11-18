// SPDX-License-Identifier: Apache-2.0

package kubernetes

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/kubeneuron/kubeneuron/pkg/cloud"
)

// queueRecycler is a provider with an interruption queue holding notices
// for the instances in held.
type queueRecycler struct {
	fakeRecycler
	held  []cloud.Interruption
	asked []cloud.NodeRef
	acked []string
}

func (q *queueRecycler) Interruptions(_ context.Context, nodes []cloud.NodeRef) ([]cloud.Interruption, error) {
	q.asked = nodes
	return q.held, nil
}
func (q *queueRecycler) ReceiveInterruptions(context.Context) (bool, error) { return true, nil }
func (q *queueRecycler) AckInterruption(_ context.Context, key string) error {
	q.acked = append(q.acked, key)
	return nil
}

func TestCloudInterruptionsNameTheNode(t *testing.T) {
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "ip-10-0-0-1", UID: "u1"},
		Spec:       corev1.NodeSpec{ProviderID: "cloud:///zone-a/i-0abc"},
	}
	rec := &queueRecycler{held: []cloud.Interruption{{InstanceID: "i-0abc", Kind: cloud.InterruptionSpotReclaim, Source: "s", Key: "k", Once: true}}}
	p := &Platform{client: fake.NewSimpleClientset(node), recycler: rec}
	if !p.CloudInterruptionQueueConfigured() {
		t.Fatal("a queue provider is not reported as configured")
	}
	got, err := p.CloudInterruptions(context.Background())
	if err != nil || len(got) != 1 || got[0].Node != "ip-10-0-0-1" || got[0].Key != "k" {
		t.Fatalf("notices %+v, %v; want one for ip-10-0-0-1", got, err)
	}
	if len(rec.asked) != 1 || rec.asked[0].Identity.InstanceID != "i-0abc" {
		t.Fatalf("provider asked about %+v", rec.asked)
	}
	if err := p.AckCloudInterruption(context.Background(), "k"); err != nil || len(rec.acked) != 1 {
		t.Fatalf("ack: %v, %v", rec.acked, err)
	}

	plain := &Platform{client: fake.NewSimpleClientset(node), recycler: &fakeRecycler{}}
	if plain.CloudInterruptionQueueConfigured() {
		t.Fatal("a provider without a queue is reported as configured")
	}
	if got, err := plain.CloudInterruptions(context.Background()); got != nil || err != nil {
		t.Fatalf("without a queue: %+v, %v", got, err)
	}
}
