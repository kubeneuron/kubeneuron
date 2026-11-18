// SPDX-License-Identifier: Apache-2.0

package aws

import (
	"errors"
	"testing"
	"time"

	sdk "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/autoscaling"
	at "github.com/aws/aws-sdk-go-v2/service/autoscaling/types"
	"github.com/aws/aws-sdk-go-v2/service/eks"
	et "github.com/aws/aws-sdk-go-v2/service/eks/types"
	"github.com/kubeneuron/kubeneuron/pkg/cloud"
)

type observedASG struct {
	membershipFixture
	launchActivityPages
}

func TestObserveSuccessor(t *testing.T) {
	now := time.Now()
	s := cloud.Successor{Phase: cloud.SuccessorRequested, Instance: "i-old", Pool: "workers", RequestedAt: now, State: map[string]string{"cluster": "training", "nodegroup": "gpu", "update": "update-1", "before": "1", "members": "i-old", "zone": "az", "asg_arn": "arn", "nodegroup_arn": "ng"}}
	update := &et.Update{Id: sdk.String("update-1"), Status: et.UpdateStatusInProgress}
	eksReader := &scalingUpdateFixture{out: &eks.DescribeUpdateOutput{Update: update}, groupHistory: groupHistory{group: et.Nodegroup{NodegroupArn: sdk.String("ng"), Resources: &et.NodegroupResources{AutoScalingGroups: []et.AutoScalingGroup{{Name: &s.Pool}}}}}}
	p := managedProvider{repairReader: &repairReader{cluster: "training", eks: eksReader}}
	got, err := p.observeSuccessor(t.Context(), s)
	if err != nil || got.Phase != cloud.SuccessorRequested {
		t.Fatalf("pending: %+v %v", got, err)
	}
	update.Status = et.UpdateStatusSuccessful
	g := at.AutoScalingGroup{AutoScalingGroupName: &s.Pool, AutoScalingGroupARN: sdk.String("arn"), DesiredCapacity: sdk.Int32(2), AvailabilityZones: []string{"az"}}
	for _, id := range []string{"i-old", "i-new"} {
		g.Instances = append(g.Instances, at.Instance{InstanceId: sdk.String(id), AvailabilityZone: sdk.String("az"), LifecycleState: at.LifecycleStateInService})
	}
	pages := launchActivityPages{"": {}}
	p.scaling = observedASG{membershipFixture{t, &autoscaling.DescribeAutoScalingGroupsOutput{AutoScalingGroups: []at.AutoScalingGroup{g}}}, pages}
	got, err = p.observeSuccessor(t.Context(), s)
	if !errors.Is(err, cloud.ErrNeedsOperator) || got.ProviderID != "" {
		t.Fatalf("missing launch: %+v %v", got, err)
	}
	pages[""].Activities = []at.Activity{{AutoScalingGroupName: &s.Pool, AutoScalingGroupARN: sdk.String("arn"), StartTime: &now, Description: sdk.String("Launching a new EC2 instance: i-new"), Cause: sdk.String("changing the desired capacity from 1 to 2."), StatusCode: at.ScalingActivityStatusCodeSuccessful}}
	got, err = p.observeSuccessor(t.Context(), s)
	if err != nil || got.Phase != cloud.SuccessorCreated || got.ProviderID != "aws:///az/i-new" {
		t.Fatalf("created: %+v %v", got, err)
	}
}
