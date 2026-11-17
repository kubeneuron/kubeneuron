// SPDX-License-Identifier: Apache-2.0

package aws

import (
	"context"
	"testing"
	"time"

	sdk "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/autoscaling"
	at "github.com/aws/aws-sdk-go-v2/service/autoscaling/types"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ct "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/eks"
	et "github.com/aws/aws-sdk-go-v2/service/eks/types"
	"github.com/kubeneuron/kubeneuron/pkg/cloud"
)

type groupHistory struct {
	updateReader
	group et.Nodegroup
	reads int
	flip  bool
}

func (h *groupHistory) DescribeNodegroup(_ context.Context, in *eks.DescribeNodegroupInput, _ ...func(*eks.Options)) (*eks.DescribeNodegroupOutput, error) {
	h.reads++
	group := h.group
	if h.flip && h.reads == 2 {
		group.NodeRepairConfig = &et.NodeRepairConfig{Enabled: sdk.Bool(true)}
	}
	return &eks.DescribeNodegroupOutput{Nodegroup: &group}, nil
}
func (*groupHistory) ListUpdates(context.Context, *eks.ListUpdatesInput, ...func(*eks.Options)) (*eks.ListUpdatesOutput, error) {
	return &eks.ListUpdatesOutput{}, nil
}

type quietASG struct{ membershipFixture }

func (quietASG) DescribeScalingActivities(context.Context, *autoscaling.DescribeScalingActivitiesInput, ...func(*autoscaling.Options)) (*autoscaling.DescribeScalingActivitiesOutput, error) {
	return &autoscaling.DescribeScalingActivitiesOutput{}, nil
}

func TestOpenRepairRechecksManagedNodegroup(t *testing.T) {
	for _, tc := range []struct {
		name        string
		enabled     *bool
		flip, known bool
		reads       int
	}{
		{"settled", sdk.Bool(false), false, true, 2},
		{"repair enabled during observation", sdk.Bool(false), true, false, 2},
		{"repair initially enabled", sdk.Bool(true), false, false, 1},
		{"missing policy", nil, false, false, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			group := et.Nodegroup{ClusterName: sdk.String("training"), NodegroupName: sdk.String("gpu"), Status: et.NodegroupStatusActive, ModifiedAt: sdk.Time(time.Now().Add(-time.Hour)), NodeRepairConfig: &et.NodeRepairConfig{Enabled: tc.enabled}, Resources: &et.NodegroupResources{AutoScalingGroups: []et.AutoScalingGroup{{Name: sdk.String("workers")}}}}
			history := &groupHistory{group: group, flip: tc.flip}
			asg := at.AutoScalingGroup{AutoScalingGroupName: sdk.String("workers"), Instances: []at.Instance{{InstanceId: sdk.String("i-pinned"), HealthStatus: sdk.String("Healthy"), LifecycleState: at.LifecycleStateInService}}}
			status := ct.InstanceStatus{InstanceId: sdk.String("i-pinned"), InstanceState: &ct.InstanceState{Name: ct.InstanceStateNameRunning}}
			r := repairReader{cluster: "training", nodegroup: "gpu", eks: history, scaling: quietASG{membershipFixture{t, &autoscaling.DescribeAutoScalingGroupsOutput{AutoScalingGroups: []at.AutoScalingGroup{asg}}}}, ec2: statusFixture{t, &ec2.DescribeInstanceStatusOutput{InstanceStatuses: []ct.InstanceStatus{status}}}}
			ref := cloud.NodeRef{Identity: cloud.NodeIdentity{Node: "n", NodeUID: "uid", BootID: "boot", InstanceID: "i-pinned", ProviderID: "aws:///az/i-pinned"}}
			obs, err := r.OpenRepair(t.Context(), ref)
			if (err != nil) != (tc.enabled == nil) || obs.Known != tc.known || history.reads != tc.reads {
				t.Fatalf("observation %+v, error %v, reads %d", obs, err, history.reads)
			}
		})
	}
}
