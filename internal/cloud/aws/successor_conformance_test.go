// SPDX-License-Identifier: Apache-2.0

package aws

import (
	"context"
	"errors"
	"testing"
	"time"

	sdk "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/autoscaling"
	at "github.com/aws/aws-sdk-go-v2/service/autoscaling/types"
	"github.com/aws/aws-sdk-go-v2/service/eks"
	et "github.com/aws/aws-sdk-go-v2/service/eks/types"
	"github.com/kubeneuron/kubeneuron/internal/cloud/cloudtest"
	"github.com/kubeneuron/kubeneuron/pkg/cloud"
)

type successorLab struct {
	observedASG
	scalingUpdateFixture
	terminateFixture
	hold, lose bool
	token      string
	requests   int
}

func (f *successorLab) HoldNextRequest() { f.hold = true }
func (f *successorLab) LoseNextAnswer()  { f.lose = true }
func (f *successorLab) Requests() int    { return f.requests }
func (f *successorLab) Size() int {
	return int(*f.membershipFixture.group.AutoScalingGroups[0].DesiredCapacity)
}
func (f *successorLab) Deliver() {
	g := &f.membershipFixture.group.AutoScalingGroups[0]
	member := g.Instances[0]
	member.InstanceId = sdk.String("i-new")
	g.Instances = append(g.Instances, member)
	f.scalingUpdateFixture.out.Update.Status = et.UpdateStatusSuccessful
	f.launchActivityPages[""].Activities = []at.Activity{{StartTime: f.scalingUpdateFixture.out.Update.CreatedAt, AutoScalingGroupName: g.AutoScalingGroupName, AutoScalingGroupARN: g.AutoScalingGroupARN, Description: sdk.String("Launching a new EC2 instance: i-new"), Cause: sdk.String("changing the desired capacity from 1 to 2"), StatusCode: at.ScalingActivityStatusCodeSuccessful}}
}

func (f *successorLab) UpdateNodegroupConfig(_ context.Context, in *eks.UpdateNodegroupConfigInput, _ ...func(*eks.Options)) (*eks.UpdateNodegroupConfigOutput, error) {
	token := sdk.ToString(in.ClientRequestToken)
	if token == "" || sdk.ToInt32(in.ScalingConfig.DesiredSize) != 2 {
		f.t.Fatalf("unexpected scaling request: %+v", in)
	}
	if token != f.token {
		f.token = token
		f.requests++
		f.membershipFixture.group.AutoScalingGroups[0].DesiredCapacity = sdk.Int32(2)
		f.scalingUpdateFixture.out.Update = &et.Update{Id: sdk.String("update-1"), CreatedAt: sdk.Time(time.Now()), Type: et.UpdateTypeConfigUpdate, Status: et.UpdateStatusInProgress, Params: []et.UpdateParam{{Type: et.UpdateParamTypeDesiredSize, Value: sdk.String("2")}}}
		if !f.hold {
			f.Deliver()
		}
	}
	if f.lose {
		f.lose = false
		return nil, errors.New("lost update response")
	}
	return &eks.UpdateNodegroupConfigOutput{Update: f.scalingUpdateFixture.out.Update}, nil
}

func (f *successorLab) TerminateInstanceInAutoScalingGroup(ctx context.Context, in *autoscaling.TerminateInstanceInAutoScalingGroupInput, opts ...func(*autoscaling.Options)) (*autoscaling.TerminateInstanceInAutoScalingGroupOutput, error) {
	out, err := f.terminateFixture.TerminateInstanceInAutoScalingGroup(ctx, in, opts...)
	if err == nil {
		g := &f.membershipFixture.group.AutoScalingGroups[0]
		g.Instances = g.Instances[1:]
		g.DesiredCapacity = sdk.Int32(1)
	}
	return out, err
}

func newSuccessorLab(t *testing.T) (cloud.SuccessorProvider, cloudtest.SuccessorCloud) {
	group := at.AutoScalingGroup{
		AutoScalingGroupName: sdk.String("workers"), AutoScalingGroupARN: sdk.String("asg-1"),
		DesiredCapacity: sdk.Int32(1), MaxSize: sdk.Int32(2), AvailabilityZones: []string{"az"},
		Instances: []at.Instance{{InstanceId: sdk.String("i-pinned"), AvailabilityZone: sdk.String("az"), LifecycleState: at.LifecycleStateInService}},
		Tags:      []at.TagDescription{{Key: sdk.String("eks:nodegroup-name"), Value: sdk.String("gpu")}},
	}
	ng := et.Nodegroup{
		ClusterName: sdk.String("training"), NodegroupName: sdk.String("gpu"), NodegroupArn: sdk.String("ng"),
		Tags: map[string]string{fixedSizeTag: "true"}, Status: et.NodegroupStatusActive, Resources: &et.NodegroupResources{AutoScalingGroups: []et.AutoScalingGroup{{Name: group.AutoScalingGroupName}}},
	}
	f := &successorLab{
		observedASG:          observedASG{membershipFixture{t, &autoscaling.DescribeAutoScalingGroupsOutput{AutoScalingGroups: []at.AutoScalingGroup{group}}}, launchActivityPages{"": {}}},
		scalingUpdateFixture: scalingUpdateFixture{out: &eks.DescribeUpdateOutput{}, groupHistory: groupHistory{group: ng}},
		terminateFixture:     terminateFixture{t: t, expectedID: "i-pinned", out: &autoscaling.TerminateInstanceInAutoScalingGroupOutput{Activity: &at.Activity{ActivityId: sdk.String("term")}}},
	}
	p := &managedProvider{repairReader: &repairReader{cluster: "training", eks: f, scaling: f}, updater: f, terminator: f}
	return p, f
}

func TestEKSSuccessorConformance(t *testing.T) {
	cloudtest.SuccessorConformance(t, cloudtest.SuccessorCase{Offline: &managedProvider{}, Instance: "i-pinned", New: newSuccessorLab})
}

// A lost response can precede visibility in either control plane. The
// controller has already persisted RequestedAt when it sends the request.
func TestReleaseAfterLostAnswerWithStaleCloudReads(t *testing.T) {
	provider, simulated := newSuccessorLab(t)
	f := simulated.(*successorLab)
	plan, err := provider.PrepareSuccessor(t.Context(), cloud.Successor{Instance: "i-pinned", Operation: "stale-reads"})
	if err != nil {
		t.Fatal(err)
	}
	plan.RequestedAt = time.Now().UTC()
	f.HoldNextRequest()
	f.LoseNextAnswer()
	if _, err := provider.PrepareSuccessor(t.Context(), plan); err == nil {
		t.Fatal("expected lost response")
	}
	// ASG still serves its old snapshot. EKS ListUpdates on groupHistory
	// likewise returns no IDs, even though the request has been accepted.
	f.membershipFixture.group.AutoScalingGroups[0].DesiredCapacity = sdk.Int32(1)
	if err := provider.ReleaseSuccessor(t.Context(), plan); !errors.Is(err, cloud.ErrNeedsOperator) {
		t.Fatalf("release: %v", err)
	}
	if f.Requests() != 1 || f.calls != 0 {
		t.Fatalf("requests=%d terminations=%d", f.Requests(), f.calls)
	}
}
