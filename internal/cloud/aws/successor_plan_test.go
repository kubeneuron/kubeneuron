// SPDX-License-Identifier: Apache-2.0

package aws

import (
	"errors"
	"testing"

	sdk "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/autoscaling"
	at "github.com/aws/aws-sdk-go-v2/service/autoscaling/types"
	et "github.com/aws/aws-sdk-go-v2/service/eks/types"
	"github.com/kubeneuron/kubeneuron/pkg/cloud"
)

func TestPlanSuccessor(t *testing.T) {
	group := at.AutoScalingGroup{AutoScalingGroupName: sdk.String("workers"), AvailabilityZones: []string{"zone-a"}, DesiredCapacity: sdk.Int32(2), MaxSize: sdk.Int32(3)}
	for _, id := range []string{"i-old", "i-a"} {
		group.Instances = append(group.Instances, at.Instance{InstanceId: sdk.String(id), AvailabilityZone: sdk.String("zone-a"), LifecycleState: at.LifecycleStateInService})
	}
	s := cloud.Successor{Instance: "i-old", Operation: "op"}
	plan, err := planSuccessor(s, group)
	if err != nil || plan.State["members"] != "i-a,i-old" || plan.State["before"] != "2" || plan.Phase != cloud.SuccessorPlanned {
		t.Fatalf("plan=%+v err=%v", plan, err)
	}
	group.AvailabilityZones = append(group.AvailabilityZones, "zone-b")
	if _, err := planSuccessor(s, group); !errors.Is(err, cloud.ErrAddFirstUnavailable) {
		t.Fatal(err)
	}
	group.AvailabilityZones = group.AvailabilityZones[:1]
	group.MaxSize = sdk.Int32(2)
	if _, err := planSuccessor(s, group); !errors.Is(err, cloud.ErrAddFirstUnavailable) {
		t.Fatal(err)
	}
}

func TestCheckAddFirstRequiresEKSBackingGroup(t *testing.T) {
	for _, tc := range []struct {
		name, cluster, backing string
		eligible               bool
	}{
		{"confirmed", "training", "workers", true},
		{"foreign cluster", "other", "workers", false},
		{"tagged self-managed ASG", "training", "different-asg", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			group := at.AutoScalingGroup{
				AutoScalingGroupARN: sdk.String("asg-arn"), AutoScalingGroupName: sdk.String("workers"), DesiredCapacity: sdk.Int32(1), MaxSize: sdk.Int32(2),
				AvailabilityZones: []string{"zone-a"},
				Instances:         []at.Instance{{InstanceId: sdk.String("i-pinned"), AvailabilityZone: sdk.String("zone-a"), LifecycleState: at.LifecycleStateInService}},
				Tags:              []at.TagDescription{{Key: sdk.String("eks:nodegroup-name"), Value: sdk.String("gpu")}, {Key: sdk.String("k8s.io/cluster-autoscaler/enabled"), Value: sdk.String("true")}},
			}
			eksGroup := et.Nodegroup{NodegroupArn: sdk.String("group-arn"), ClusterName: sdk.String(tc.cluster), NodegroupName: sdk.String("gpu"), Status: et.NodegroupStatusActive,
				Resources: &et.NodegroupResources{AutoScalingGroups: []et.AutoScalingGroup{{Name: sdk.String(tc.backing)}}}}
			p := managedProvider{repairReader: &repairReader{cluster: "training", eks: &groupHistory{group: eksGroup},
				scaling: quietASG{membershipFixture{t, &autoscaling.DescribeAutoScalingGroupsOutput{AutoScalingGroups: []at.AutoScalingGroup{group}}}}}}
			err := p.CheckAddFirst(t.Context(), "i-pinned")
			if tc.eligible {
				if err != nil {
					t.Fatal(err)
				}
				plan, err := p.PrepareSuccessor(t.Context(), cloud.Successor{Instance: "i-pinned", Operation: "op"})
				if err != nil || plan.State["asg_arn"] != "asg-arn" || plan.State["nodegroup_arn"] != "group-arn" || plan.State["cluster"] != "training" {
					t.Fatalf("resource identities: %+v, %v", plan, err)
				}
			}
			if !tc.eligible && !errors.Is(err, cloud.ErrAddFirstUnavailable) {
				t.Fatalf("expected unavailable, got %v", err)
			}
		})
	}
}

func TestVerifySuccessorBaseline(t *testing.T) {
	member := func(id string) at.Instance {
		return at.Instance{InstanceId: sdk.String(id), AvailabilityZone: sdk.String("zone-a"), LifecycleState: at.LifecycleStateInService}
	}
	group := at.AutoScalingGroup{
		AutoScalingGroupName: sdk.String("workers"), AutoScalingGroupARN: sdk.String("asg-1"),
		DesiredCapacity: sdk.Int32(2), MaxSize: sdk.Int32(3), AvailabilityZones: []string{"zone-a"},
		Instances: []at.Instance{member("i-pinned"), member("i-peer")},
		Tags:      []at.TagDescription{{Key: sdk.String("eks:nodegroup-name"), Value: sdk.String("gpu")}},
	}
	out := &autoscaling.DescribeAutoScalingGroupsOutput{AutoScalingGroups: []at.AutoScalingGroup{group}}
	eksReader := &groupHistory{group: et.Nodegroup{NodegroupArn: sdk.String("ng-1"), ClusterName: sdk.String("training"), NodegroupName: sdk.String("gpu"), Status: et.NodegroupStatusActive, Resources: &et.NodegroupResources{AutoScalingGroups: []et.AutoScalingGroup{{Name: sdk.String("workers")}}}}}
	p := managedProvider{repairReader: &repairReader{cluster: "training", eks: eksReader, scaling: quietASG{membershipFixture{t, out}}}}
	plan, err := p.PrepareSuccessor(t.Context(), cloud.Successor{Operation: "op", Instance: "i-pinned"})
	if err != nil {
		t.Fatal(err)
	}
	if err = p.verifySuccessorBaseline(t.Context(), plan); err != nil {
		t.Fatal(err)
	}
	for _, change := range []string{"asg replaced", "peer replaced", "nodegroup replaced", "headroom lost"} {
		t.Run(change, func(t *testing.T) {
			g := group
			eksReader.group.NodegroupArn = sdk.String("ng-1")
			switch change {
			case "asg replaced":
				g.AutoScalingGroupARN = sdk.String("asg-2")
			case "peer replaced":
				g.Instances = []at.Instance{member("i-pinned"), member("i-other")}
			case "nodegroup replaced":
				eksReader.group.NodegroupArn = sdk.String("ng-2")
			case "headroom lost":
				g.MaxSize = sdk.Int32(2)
			}
			out.AutoScalingGroups[0] = g
			if _, err := p.PrepareSuccessor(t.Context(), plan); !errors.Is(err, cloud.ErrNeedsOperator) {
				t.Fatalf("got %v", err)
			}
		})
	}
}
