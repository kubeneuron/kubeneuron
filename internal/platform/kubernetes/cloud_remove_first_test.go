// SPDX-License-Identifier: Apache-2.0
package kubernetes

import (
	"context"
	"errors"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/kubeneuron/kubeneuron/pkg/cloud"
)

type plannedRecycler struct {
	repairRecycler
	continued []cloud.RemoveFirstPhase
}

func (r *plannedRecycler) PlanRemoveFirst(_ context.Context, id, op string) (cloud.RemoveFirstPlan, error) {
	return cloud.RemoveFirstPlan{Operation: op, Instance: id, Phase: cloud.RemoveFirstPlanned}, nil
}

func (r *plannedRecycler) ContinueRemoveFirst(_ context.Context, plan cloud.RemoveFirstPlan) (cloud.RemoveFirstPlan, error) {
	r.continued = append(r.continued, plan.Phase)
	return plan, nil
}

// The step that commits to the delete repeats the guard against the pin;
// the steps after it act on a node that may be gone and do not.
func TestRemoveFirstGuardsOnlyTheCommit(t *testing.T) {
	ctx := context.Background()
	n := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "worker", UID: "uid-old"}, Spec: corev1.NodeSpec{ProviderID: "cloud:///zone/i-old"}, Status: corev1.NodeStatus{NodeInfo: corev1.NodeSystemInfo{BootID: "boot-old"}}}
	rec := &plannedRecycler{repairRecycler: repairRecycler{mode: cloud.RepairOff, repair: cloud.RepairObservation{Known: true}}}
	p := &Platform{client: fake.NewSimpleClientset(n), recycler: rec}
	ref, err := p.CloudNode(ctx, n.Name)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := p.PlanRemoveFirst(ctx, ref.Identity, "op")
	if err != nil || plan.Instance != "i-old" {
		t.Fatalf("plan %+v, %v", plan, err)
	}
	rec.repair.Active = true
	if _, err := p.ContinueRemoveFirst(ctx, ref.Identity, plan); err == nil || len(rec.continued) != 0 {
		t.Fatalf("committed under an active cloud repair: %v, %v", err, rec.continued)
	}
	plan.Phase = cloud.RemoveFirstDeleting
	if _, err := p.ContinueRemoveFirst(ctx, ref.Identity, plan); err != nil || len(rec.continued) != 1 {
		t.Fatalf("a committed plan was not continued: %v, %v", err, rec.continued)
	}
	other := ref.Identity
	other.InstanceID = "i-other"
	if _, err := p.ContinueRemoveFirst(ctx, other, plan); err == nil {
		t.Fatal("a plan of another instance was continued")
	}
	// A provider without plans says so, and the caller deletes only.
	if _, err := (&Platform{client: fake.NewSimpleClientset(n), recycler: &rec.repairRecycler}).PlanRemoveFirst(ctx, ref.Identity, "op"); !errors.Is(err, cloud.ErrRemoveFirstUnplanned) {
		t.Fatalf("unplanned provider: %v", err)
	}
}
