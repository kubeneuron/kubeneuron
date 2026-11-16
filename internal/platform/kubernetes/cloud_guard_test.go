// SPDX-License-Identifier: Apache-2.0
package kubernetes

import (
	"context"
	"github.com/kubeneuron/kubeneuron/pkg/cloud"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
	"testing"
)

type repairRecycler struct {
	fakeRecycler
	mode       cloud.RepairMode
	repair     cloud.RepairObservation
	afterQuery func()
}

func (*repairRecycler) Name() string                     { return "test" }
func (*repairRecycler) Capabilities() cloud.Capabilities { return cloud.Capabilities{Replace: true} }
func (r *repairRecycler) RepairPolicy(context.Context, cloud.NodeRef) (cloud.RepairPolicy, error) {
	return cloud.RepairPolicy{Mode: r.mode}, nil
}
func (r *repairRecycler) OpenRepair(context.Context, cloud.NodeRef) (cloud.RepairObservation, error) {
	if r.afterQuery != nil {
		r.afterQuery()
	}
	return r.repair, nil
}

func TestPinnedReplacementRefusesChangedOrUncertainTarget(t *testing.T) {
	for _, scenario := range []string{"allowed", "repair-on", "repair-unknown", "repair-active", "replaced", "identity-changed-during-query", "veto-not-established"} {
		t.Run(scenario, func(t *testing.T) {
			ctx := context.Background()
			n := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "worker", UID: "uid-old"}, Spec: corev1.NodeSpec{ProviderID: "cloud:///zone/i-old"}, Status: corev1.NodeStatus{NodeInfo: corev1.NodeSystemInfo{BootID: "boot-old"}}}
			client := fake.NewSimpleClientset(n)
			rec := &repairRecycler{mode: cloud.RepairOff, repair: cloud.RepairObservation{Known: true}}
			p := &Platform{client: client, recycler: rec}
			ref, err := p.CloudNode(ctx, n.Name)
			if err != nil {
				t.Fatal(err)
			}
			switch scenario {
			case "repair-on":
				rec.mode = cloud.RepairOn
			case "repair-unknown":
				rec.repair.Known = false
			case "repair-active":
				rec.repair.Active = true
			case "replaced":
				rec.repair.Replaced = true
			case "veto-not-established":
				rec.mode = cloud.RepairVeto
			case "identity-changed-during-query":
				rec.afterQuery = func() {
					n.Spec.ProviderID = "cloud:///zone/i-new"
					if _, err := client.CoreV1().Nodes().Update(ctx, n, metav1.UpdateOptions{}); err != nil {
						t.Fatal(err)
					}
				}
			}
			err = p.ReplacePinnedNode(ctx, ref.Identity)
			if scenario == "allowed" {
				if err != nil || rec.replaced != "i-old" {
					t.Fatalf("replace=%s err=%v", rec.replaced, err)
				}
			} else {
				if err == nil || rec.replaced != "" {
					t.Fatalf("unsafe replacement=%s err=%v", rec.replaced, err)
				}
			}
		})
	}
}
