// SPDX-License-Identifier: Apache-2.0

package aws

import (
	"errors"
	"strings"
	"testing"
	"time"

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
	if _, err := planSuccessor(s, group); !errors.Is(err, cloud.ErrAddFirstUnavailable) || !strings.Contains(err.Error(), "node group spans several zones") {
		t.Fatal(err)
	}
	group.AvailabilityZones = group.AvailabilityZones[:1]
	group.MaxSize = sdk.Int32(2)
	if _, err := planSuccessor(s, group); !errors.Is(err, cloud.ErrAddFirstUnavailable) || !strings.Contains(err.Error(), "no room below maxSize") {
		t.Fatal(err)
	}
}

func TestCheckAddFirstRequiresEKSBackingGroup(t *testing.T) {
	for _, tc := range []struct {
		name, cluster, backing string
		eligible               bool
		autoscalerTag          string
	}{
		{"confirmed", "training", "workers", true, ""},
		{"autoscaler enabled", "training", "workers", false, "k8s.io/cluster-autoscaler/enabled"},
		{"autoscaler cluster", "training", "workers", false, "k8s.io/cluster-autoscaler/training"},
		{"foreign cluster", "other", "workers", false, ""},
		{"tagged self-managed ASG", "training", "different-asg", false, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			group := at.AutoScalingGroup{
				AutoScalingGroupARN: sdk.String("asg-arn"), AutoScalingGroupName: sdk.String("workers"), DesiredCapacity: sdk.Int32(1), MaxSize: sdk.Int32(2),
				AvailabilityZones: []string{"zone-a"},
				Instances:         []at.Instance{{InstanceId: sdk.String("i-pinned"), AvailabilityZone: sdk.String("zone-a"), LifecycleState: at.LifecycleStateInService}},
				Tags:              []at.TagDescription{{Key: sdk.String("eks:nodegroup-name"), Value: sdk.String("gpu")}},
			}
			if tc.autoscalerTag != "" {
				group.Tags = append(group.Tags, at.TagDescription{Key: sdk.String(tc.autoscalerTag)})
			}
			eksGroup := et.Nodegroup{NodegroupArn: sdk.String("group-arn"), ClusterName: sdk.String(tc.cluster), NodegroupName: sdk.String("gpu"), Status: et.NodegroupStatusActive,
				Resources: &et.NodegroupResources{AutoScalingGroups: []et.AutoScalingGroup{{Name: sdk.String(tc.backing)}}}}
			p := managedProvider{repairReader: &repairReader{cluster: "training", eks: &groupHistory{group: eksGroup},
				scaling: quietASG{membershipFixture{t, &autoscaling.DescribeAutoScalingGroupsOutput{AutoScalingGroups: []at.AutoScalingGroup{group}}}}}}
			err := p.CheckAddFirst(t.Context(), "i-pinned")
			if tc.autoscalerTag != "" && (err == nil || !strings.Contains(err.Error(), "node group is managed by Cluster Autoscaler")) {
				t.Fatalf("autoscaler refusal: %v", err)
			}
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
	plan.RequestedAt = time.Unix(1030, 0) // Controller clock leads the EKS update time by 30 seconds.
	if err = p.verifySuccessorBaseline(t.Context(), plan); err != nil {
		t.Fatal(err)
	}
	t.Run("accepted request after membership changes", func(t *testing.T) {
		client := &lostUpdateResponse{t: t, tokens: map[string]bool{}, cluster: "training", group: "gpu"}
		first := managedProvider{repairReader: p.repairReader, updater: client}
		if got, err := first.PrepareSuccessor(t.Context(), plan); err == nil || got.Phase != cloud.SuccessorPlanned {
			t.Fatalf("lost response: %+v, %v", got, err)
		}
		grown := group
		grown.DesiredCapacity = sdk.Int32(3)
		grown.Instances = append(grown.Instances, member("i-new"))
		out.AutoScalingGroups[0] = grown
		defer func() { out.AutoScalingGroups[0] = group }()

		restarted := managedProvider{repairReader: p.repairReader, updater: client}
		got, err := restarted.PrepareSuccessor(t.Context(), plan)
		if err != nil || got.Phase != cloud.SuccessorRequested || got.State["update"] != "update-id" || len(client.tokens) != 1 {
			t.Fatalf("retry after restart: %+v, %v", got, err)
		}
		client.replayErr = &et.ResourceInUseException{}
		if _, err := restarted.PrepareSuccessor(t.Context(), plan); !errors.Is(err, cloud.ErrNeedsOperator) {
			t.Fatalf("foreign update: %v", err)
		}

	})

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
