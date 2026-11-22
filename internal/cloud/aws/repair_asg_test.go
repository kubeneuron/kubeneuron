// SPDX-License-Identifier: Apache-2.0

package aws

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

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

type activityPages struct {
	t       *testing.T
	first   []at.Activity
	outcome string
	calls   int
}

func (p *activityPages) DescribeScalingActivities(_ context.Context, in *autoscaling.DescribeScalingActivitiesInput, _ ...func(*autoscaling.Options)) (*autoscaling.DescribeScalingActivitiesOutput, error) {
	p.t.Helper()
	p.calls++
	if sdk.ToString(in.AutoScalingGroupName) != "workers" {
		p.t.Fatal("wrong group")
	}
	if p.calls == 1 {
		return &autoscaling.DescribeScalingActivitiesOutput{Activities: p.first, NextToken: sdk.String("next")}, nil
	}
	if p.calls != 2 || sdk.ToString(in.NextToken) != "next" {
		p.t.Fatal("unexpected pagination")
	}
	switch p.outcome {
	case "denied":
		return nil, errors.New("AccessDenied")
	case "repeated":
		return &autoscaling.DescribeScalingActivitiesOutput{NextToken: sdk.String("next")}, nil
	case "in progress":
		return &autoscaling.DescribeScalingActivitiesOutput{Activities: []at.Activity{{AutoScalingGroupName: in.AutoScalingGroupName, StatusCode: at.ScalingActivityStatusCodeInProgress}}}, nil
	default:
		return &autoscaling.DescribeScalingActivitiesOutput{}, nil
	}
}

func TestCheckGroupActivitiesReadsSecondPage(t *testing.T) {
	for _, outcome := range []string{"clear", "denied", "repeated", "in progress"} {
		t.Run(outcome, func(t *testing.T) {
			p := &activityPages{t: t, outcome: outcome}
			err := checkGroupActivities(t.Context(), p, "workers")
			if (err == nil) != (outcome == "clear") || p.calls != 2 {
				t.Fatalf("pages=%d err=%v", p.calls, err)
			}
		})
	}
}

func TestASGHistoryCutoff(t *testing.T) {
	old := at.Activity{AutoScalingGroupName: sdk.String("workers"), StatusCode: at.ScalingActivityStatusCodeSuccessful, EndTime: sdk.Time(time.Now().Add(-25 * time.Hour))}
	recent := old
	recent.EndTime = sdk.Time(time.Now().Add(-time.Hour))
	undated := old
	undated.EndTime = nil
	active := old
	active.StatusCode = at.ScalingActivityStatusCodeInProgress
	for _, tc := range []struct {
		name  string
		page  []at.Activity
		calls int
		clear bool
	}{
		{"old terminal page", []at.Activity{old, old}, 1, true},
		{"mixed ages", []at.Activity{recent, old}, 2, true},
		{"missing end time", []at.Activity{old, undated}, 2, true},
		{"unfinished on old page", []at.Activity{old, active}, 1, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := &activityPages{t: t, first: tc.page}
			err := checkGroupActivities(t.Context(), p, "workers")
			if p.calls != tc.calls || (err == nil) != tc.clear {
				t.Fatalf("calls=%d err=%v", p.calls, err)
			}
		})
	}
}
