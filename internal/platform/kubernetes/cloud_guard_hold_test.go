// SPDX-License-Identifier: Apache-2.0
package kubernetes

import (
	"context"
	"errors"
	"testing"

	"github.com/kubeneuron/kubeneuron/pkg/cloud"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

// holdRecycler answers the repair contract with a provider's hold.
type holdRecycler struct {
	repairRecycler
	policy    cloud.RepairPolicy
	policyErr error
	openErr   error
	addFirst  error
	opened    int
}

func (r *holdRecycler) RepairPolicy(context.Context, cloud.NodeRef) (cloud.RepairPolicy, error) {
	return r.policy, r.policyErr
}
func (r *holdRecycler) OpenRepair(context.Context, cloud.NodeRef) (cloud.RepairObservation, error) {
	r.opened++
	return cloud.RepairObservation{Known: r.openErr == nil}, r.openErr
}
func (r *holdRecycler) CheckAddFirst(context.Context, string) error { return r.addFirst }

func TestRecoveryHoldReachesTheOperator(t *testing.T) {
	raw := errors.New("AccessDeniedException: User: arn:aws:sts::123456789012:assumed-role/x")
	recent := &cloud.RecoveryHold{Code: cloud.HoldRecentChange, Reason: "Node group gpu changed at 15:30 UTC (EKS update: NodeRepairEnabled).", Action: "KubeNeuron waits 30 minutes after a change and checks again at 16:00 UTC."}
	denied := &cloud.RecoveryHold{Code: cloud.HoldReadFailed, Reason: "KubeNeuron could not read node group gpu: eks:DescribeNodegroup was denied.", Action: "Allow eks:DescribeNodegroup in the controller's IAM role. KubeNeuron retries every minute.", Cause: raw}
	on := cloud.RepairPolicy{Mode: cloud.RepairOn, Reason: "EKS node auto repair is enabled for node group gpu, so KubeNeuron does not replace its nodes.", Action: "To let KubeNeuron replace them: aws eks update-nodegroup-config --cluster-name c --nodegroup-name gpu --node-repair-config enabled=false"}
	off := cloud.RepairPolicy{Mode: cloud.RepairOff}
	for _, tc := range []struct {
		name      string
		rec       holdRecycler
		reason    string
		pool      string // the hold code AssessAddFirstPool returns; empty: none
		poolOpens int
	}{
		{"repair on", holdRecycler{policy: on}, on.Reason + " " + on.Action, cloud.HoldNativeRepair, 0},
		// a transient hold holds the incident, not the pool's add-first
		{"recent change", holdRecycler{policy: off, openErr: recent}, recent.Message(), "", 1},
		{"policy read denied", holdRecycler{policyErr: denied}, denied.Message(), cloud.HoldReadFailed, 0},
		{"open-repair read denied", holdRecycler{policy: off, openErr: denied}, denied.Message(), cloud.HoldReadFailed, 1},
		{"untyped provider error", holdRecycler{policy: off, openErr: raw}, "open-repair query failed", "", 1},
		{"pool refused before repair is read", holdRecycler{policy: on, addFirst: &cloud.PoolRefusal{Code: cloud.PoolAutoscaled}}, on.Reason + " " + on.Action, "", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			n := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "worker", UID: "uid"}, Spec: corev1.NodeSpec{ProviderID: "cloud:///zone/i-old"}, Status: corev1.NodeStatus{NodeInfo: corev1.NodeSystemInfo{BootID: "boot"}}}
			rec := tc.rec
			p := &Platform{client: fake.NewSimpleClientset(n), recycler: &rec}
			ref, err := p.CloudNode(ctx, n.Name)
			if err != nil {
				t.Fatal(err)
			}
			v, _ := p.RecoveryEligibility(ctx, ref.Identity)
			if v.ReplaceAllowed || v.Reason != tc.reason {
				t.Fatalf("eligibility %+v, want reason %q", v, tc.reason)
			}
			rec.opened = 0
			err = p.AssessAddFirstPool(ctx, ref.Identity)
			var hold *cloud.RecoveryHold
			switch {
			case tc.pool == "" && errors.As(err, &hold):
				t.Fatalf("unexpected hold %v", err)
			case tc.pool != "" && (!errors.As(err, &hold) || hold.Code != tc.pool):
				t.Fatalf("pool assessment: want %s, got %v", tc.pool, err)
			case rec.opened != tc.poolOpens:
				t.Fatalf("OpenRepair calls %d", rec.opened)
			}
		})
	}
}
