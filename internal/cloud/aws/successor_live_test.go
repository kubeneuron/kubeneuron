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

// liveLaunchCause is the Cause of the add-first launch activity of a live EKS
// run (managed node group, desired 1 to 2), verbatim but for the times.
const liveLaunchCause = "At 2026-11-20T23:30:23Z a user request update of AutoScalingGroup constraints to min: 1, max: 2, desired: 2 changing the desired capacity from 1 to 2.  At 2026-11-20T23:30:37Z an instance was started in response to a difference between desired and actual capacity, increasing the capacity from 1 to 2."

func TestLiveLaunchCauseHasOneDesiredTransition(t *testing.T) {
	got := desiredTransition.FindAllStringSubmatch(liveLaunchCause, -1)
	if len(got) != 1 || got[0][1] != "1" || got[0][2] != "2" {
		t.Fatalf("transitions in the live cause: %q", got)
	}
}

// The order seen on EKS: the size update is done and the ASG desired is 2,
// with no launch activity for 16 s; then the instance is Pending under a
// launch in progress, the activity succeeds before the group lists the
// instance InService, and only then is the successor named.
func TestObserveSuccessorLiveLaunchOrder(t *testing.T) {
	requested := time.Now().Add(-15 * time.Second)
	s := cloud.Successor{Phase: cloud.SuccessorRequested, Instance: "i-old", Pool: "workers", RequestedAt: requested,
		State: map[string]string{"cluster": "training", "nodegroup": "gpu", "update": "update-1", "before": "1", "members": "i-old", "zone": "az", "asg_arn": "arn", "nodegroup_arn": "ng"}}
	eksReader := &scalingUpdateFixture{out: &eks.DescribeUpdateOutput{Update: &et.Update{Id: sdk.String("update-1"), Status: et.UpdateStatusSuccessful}},
		groupHistory: groupHistory{group: et.Nodegroup{NodegroupArn: sdk.String("ng"), Resources: &et.NodegroupResources{AutoScalingGroups: []et.AutoScalingGroup{{Name: &s.Pool}}}}}}
	member := func(id string, state at.LifecycleState) at.Instance {
		return at.Instance{InstanceId: sdk.String(id), AvailabilityZone: sdk.String("az"), LifecycleState: state}
	}
	started := requested.Add(16 * time.Second)
	activity := at.Activity{AutoScalingGroupName: &s.Pool, AutoScalingGroupARN: sdk.String("arn"), StartTime: &started,
		Description: sdk.String("Launching a new EC2 instance: i-new"), Cause: sdk.String(liveLaunchCause)}

	for _, step := range []struct {
		name       string
		instances  []at.Instance
		activities []at.Activity
		phase      cloud.SuccessorPhase
	}{
		{"desired raised, no activity yet", []at.Instance{member("i-old", at.LifecycleStateInService)}, nil, cloud.SuccessorRequested},
		{"instance pending, launch in progress", []at.Instance{member("i-old", at.LifecycleStateInService), member("i-new", at.LifecycleStatePending)},
			[]at.Activity{withStatus(activity, at.ScalingActivityStatusCodeInProgress)}, cloud.SuccessorRequested},
		{"launch done, instance still pending", []at.Instance{member("i-old", at.LifecycleStateInService), member("i-new", at.LifecycleStatePending)},
			[]at.Activity{withStatus(activity, at.ScalingActivityStatusCodeSuccessful)}, cloud.SuccessorRequested},
		{"instance in service", []at.Instance{member("i-old", at.LifecycleStateInService), member("i-new", at.LifecycleStateInService)},
			[]at.Activity{withStatus(activity, at.ScalingActivityStatusCodeSuccessful)}, cloud.SuccessorCreated},
	} {
		t.Run(step.name, func(t *testing.T) {
			g := at.AutoScalingGroup{AutoScalingGroupName: &s.Pool, AutoScalingGroupARN: sdk.String("arn"), DesiredCapacity: sdk.Int32(2), AvailabilityZones: []string{"az"}, Instances: step.instances}
			p := managedProvider{repairReader: &repairReader{cluster: "training", eks: eksReader}}
			p.scaling = observedASG{membershipFixture{t, &autoscaling.DescribeAutoScalingGroupsOutput{AutoScalingGroups: []at.AutoScalingGroup{g}}},
				launchActivityPages{"": {Activities: step.activities}}}
			got, err := p.PrepareSuccessor(t.Context(), s)
			if err != nil || got.Phase != step.phase {
				t.Fatalf("got %+v, %v; want phase %q", got, err, step.phase)
			}
			if step.phase == cloud.SuccessorCreated && got.ProviderID != "aws:///az/i-new" {
				t.Fatalf("successor %q", got.ProviderID)
			}
		})
	}

	// what still goes to the operator at once: a pending instance with a cancelled launch,
	// with a launch another size change caused, or with a launch of another instance
	g := at.AutoScalingGroup{AutoScalingGroupName: &s.Pool, AutoScalingGroupARN: sdk.String("arn"), DesiredCapacity: sdk.Int32(2), AvailabilityZones: []string{"az"},
		Instances: []at.Instance{member("i-old", at.LifecycleStateInService), member("i-new", at.LifecycleStatePending)}}
	other := withStatus(activity, at.ScalingActivityStatusCodeInProgress)
	other.Cause = sdk.String("At 2026-11-20T23:30:23Z a user request explicitly set group desired capacity changing the desired capacity from 1 to 3.")
	elsewhere := withStatus(activity, at.ScalingActivityStatusCodeInProgress)
	elsewhere.Description = sdk.String("Launching a new EC2 instance: i-other")
	for name, a := range map[string]at.Activity{"cancelled": withStatus(activity, at.ScalingActivityStatusCodeCancelled), "other change": other, "another instance": elsewhere} {
		p := managedProvider{repairReader: &repairReader{cluster: "training", eks: eksReader}}
		p.scaling = observedASG{membershipFixture{t, &autoscaling.DescribeAutoScalingGroupsOutput{AutoScalingGroups: []at.AutoScalingGroup{g}}},
			launchActivityPages{"": {Activities: []at.Activity{a}}}}
		if _, err := p.PrepareSuccessor(t.Context(), s); !errors.Is(err, cloud.ErrNeedsOperator) {
			t.Fatalf("%s: %v", name, err)
		}
	}
}

func withStatus(a at.Activity, code at.ScalingActivityStatusCode) at.Activity {
	a.StatusCode = code
	return a
}
