// SPDX-License-Identifier: Apache-2.0

package kubernetes

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/kubeneuron/kubeneuron/pkg/cloud"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

type eventsRecycler struct {
	repairRecycler
	sources []string
}

func (r *eventsRecycler) NodeEventSources() []string { return r.sources }

func nodeEvent(name, node, source, reason string, at time.Time) *corev1.Event {
	return &corev1.Event{
		ObjectMeta:     metav1.ObjectMeta{Name: name, Namespace: "default"},
		InvolvedObject: corev1.ObjectReference{Kind: "Node", Name: node},
		Source:         corev1.EventSource{Component: source},
		Reason:         reason,
		Message:        "pod default/secret-workload evicted",
		LastTimestamp:  metav1.NewTime(at),
	}
}

func TestCloudNodeCarriesRecentEventsOfNamedSources(t *testing.T) {
	ctx := context.Background()
	now := time.Now()
	n := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "worker", UID: "uid"}, Spec: corev1.NodeSpec{ProviderID: "cloud:///zone/i-1"}, Status: corev1.NodeStatus{NodeInfo: corev1.NodeSystemInfo{BootID: "boot"}}}
	client := fake.NewSimpleClientset(n,
		nodeEvent("repair", "worker", "native-repair", "NodeRebootStart", now.Add(-10*time.Minute)),
		nodeEvent("old", "worker", "native-repair", "NodeRebootEnd", now.Add(-2*cloud.NodeEventWindow)),
		nodeEvent("kubelet", "worker", "kubelet", "NodeReady", now),
		nodeEvent("untimed", "worker", "native-repair", "NodeReimageStart", time.Time{}),
	)
	// The fake client ignores field selectors; the other node's event proves
	// the selector is sent, not that it is applied.
	var selector string
	client.PrependReactor("list", "events", func(a k8stesting.Action) (bool, runtime.Object, error) {
		selector = a.(k8stesting.ListAction).GetListRestrictions().Fields.String()
		return false, nil, nil
	})
	p := &Platform{client: client, recycler: &eventsRecycler{sources: []string{"native-repair"}}}
	ref, err := p.CloudNode(ctx, n.Name)
	if err != nil {
		t.Fatal(err)
	}
	reasons := map[string]bool{}
	for _, e := range ref.Events {
		if e.Source != "native-repair" {
			t.Fatalf("event from another source: %+v", e)
		}
		reasons[e.Reason] = true
	}
	// The untimed event is kept as possibly recent, the old one is not.
	if !ref.EventsKnown || len(ref.Events) != 2 || !reasons["NodeRebootStart"] || !reasons["NodeReimageStart"] {
		t.Fatalf("events = %+v known=%t", ref.Events, ref.EventsKnown)
	}
	if selector != "involvedObject.kind=Node,involvedObject.name=worker" {
		t.Fatalf("field selector = %q", selector)
	}
}

func TestCloudNodeEventsUnknownWhenUnreadable(t *testing.T) {
	for _, scenario := range []string{"denied", "truncated", "not-needed"} {
		t.Run(scenario, func(t *testing.T) {
			n := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "worker", UID: "uid"}, Spec: corev1.NodeSpec{ProviderID: "cloud:///zone/i-1"}, Status: corev1.NodeStatus{NodeInfo: corev1.NodeSystemInfo{BootID: "boot"}}}
			client := fake.NewSimpleClientset(n)
			listed := false
			client.PrependReactor("list", "events", func(k8stesting.Action) (bool, runtime.Object, error) {
				listed = true
				if scenario == "denied" {
					return true, nil, errors.New("forbidden")
				}
				return true, &corev1.EventList{ListMeta: metav1.ListMeta{Continue: "more"}}, nil
			})
			rec := &eventsRecycler{sources: []string{"native-repair"}}
			if scenario == "not-needed" {
				rec.sources = nil
			}
			p := &Platform{client: client, recycler: rec}
			ref, err := p.CloudNode(context.Background(), n.Name)
			if err != nil {
				t.Fatal(err)
			}
			if ref.EventsKnown || ref.Events != nil {
				t.Fatalf("events = %+v known=%t, want unknown", ref.Events, ref.EventsKnown)
			}
			if scenario == "not-needed" && listed {
				t.Fatal("events were listed for a provider that names no source")
			}
		})
	}
}
