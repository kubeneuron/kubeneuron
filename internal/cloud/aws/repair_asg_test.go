// SPDX-License-Identifier: Apache-2.0

package aws

import (
	"context"
	"slices"
	"testing"

	sdk "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/autoscaling"
	at "github.com/aws/aws-sdk-go-v2/service/autoscaling/types"
)

type membershipFixture struct {
	t     *testing.T
	group *autoscaling.DescribeAutoScalingGroupsOutput
}

func (f membershipFixture) DescribeAutoScalingInstances(_ context.Context, in *autoscaling.DescribeAutoScalingInstancesInput, _ ...func(*autoscaling.Options)) (*autoscaling.DescribeAutoScalingInstancesOutput, error) {
	f.t.Helper()
	if !slices.Equal(in.InstanceIds, []string{"i-pinned"}) {
		f.t.Fatal(in.InstanceIds)
	}
	return &autoscaling.DescribeAutoScalingInstancesOutput{AutoScalingInstances: []at.AutoScalingInstanceDetails{{InstanceId: sdk.String("i-pinned"), AutoScalingGroupName: sdk.String("workers"), HealthStatus: sdk.String("HEALTHY"), LifecycleState: sdk.String("InService")}}}, nil
}
func (f membershipFixture) DescribeAutoScalingGroups(_ context.Context, in *autoscaling.DescribeAutoScalingGroupsInput, _ ...func(*autoscaling.Options)) (*autoscaling.DescribeAutoScalingGroupsOutput, error) {
	f.t.Helper()
	if !slices.Equal(in.AutoScalingGroupNames, []string{"workers"}) || !sdk.ToBool(in.IncludeInstances) {
		f.t.Fatal(in)
	}
	return f.group, nil
}

func TestConfirmedGroup(t *testing.T) {
	for _, tc := range []struct {
		name, backedGroup, returnedID string
		state                         at.LifecycleState
		ok                            bool
	}{
		{"confirmed", "workers", "i-pinned", at.LifecycleStateInService, true},
		{"foreign group", "another", "i-pinned", at.LifecycleStateInService, false},
		{"replaced member", "workers", "i-other", at.LifecycleStateInService, false},
		{"terminating member", "workers", "i-pinned", at.LifecycleStateTerminating, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			group := at.AutoScalingGroup{AutoScalingGroupName: sdk.String("workers"), Instances: []at.Instance{{InstanceId: sdk.String(tc.returnedID), LifecycleState: tc.state, HealthStatus: sdk.String("Healthy")}}}
			got, err := confirmedGroup(t.Context(), membershipFixture{t, &autoscaling.DescribeAutoScalingGroupsOutput{AutoScalingGroups: []at.AutoScalingGroup{group}}}, "i-pinned", []string{tc.backedGroup})
			if (err == nil) != tc.ok || (tc.ok && got != "workers") {
				t.Fatalf("confirmedGroup returned %q, %v", got, err)
			}
		})
	}
}
