// SPDX-License-Identifier: Apache-2.0

package aws

import (
	"context"
	"errors"
	"slices"
	"strings"
	"time"

	sdk "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/autoscaling"
	at "github.com/aws/aws-sdk-go-v2/service/autoscaling/types"
)

type membershipReader interface {
	DescribeAutoScalingInstances(context.Context, *autoscaling.DescribeAutoScalingInstancesInput, ...func(*autoscaling.Options)) (*autoscaling.DescribeAutoScalingInstancesOutput, error)
	DescribeAutoScalingGroups(context.Context, *autoscaling.DescribeAutoScalingGroupsInput, ...func(*autoscaling.Options)) (*autoscaling.DescribeAutoScalingGroupsOutput, error)
}

// confirmedGroup requires agreement between instance discovery, the EKS
// nodegroup's backing group names, and the group's current membership list.
func confirmedGroup(ctx context.Context, client membershipReader, id string, groups []string) (string, error) {
	if id == "" || len(groups) == 0 {
		return "", errors.New("missing instance or EKS backing groups")
	}
	members, err := client.DescribeAutoScalingInstances(ctx, &autoscaling.DescribeAutoScalingInstancesInput{InstanceIds: []string{id}})
	if err != nil {
		return "", &readError{"autoscaling:DescribeAutoScalingInstances", err}
	}
	if members == nil || len(members.AutoScalingInstances) != 1 || sdk.ToString(members.NextToken) != "" {
		return "", errors.New("incomplete Auto Scaling instance membership")
	}
	instance := members.AutoScalingInstances[0]
	group := sdk.ToString(instance.AutoScalingGroupName)
	if group == "" || !slices.Contains(groups, group) || sdk.ToString(instance.InstanceId) != id || sdk.ToString(instance.LifecycleState) != "InService" || !strings.EqualFold(sdk.ToString(instance.HealthStatus), "Healthy") {
		return "", errors.New("instance is not a healthy member of the configured nodegroup")
	}
	out, err := client.DescribeAutoScalingGroups(ctx, &autoscaling.DescribeAutoScalingGroupsInput{AutoScalingGroupNames: []string{group}, IncludeInstances: sdk.Bool(true)})
	if err != nil {
		return "", &readError{"autoscaling:DescribeAutoScalingGroups", err}
	}
	if out == nil || len(out.AutoScalingGroups) != 1 || sdk.ToString(out.NextToken) != "" || sdk.ToString(out.AutoScalingGroups[0].AutoScalingGroupName) != group {
		return "", errors.New("incomplete Auto Scaling group response")
	}
	count := 0
	for _, member := range out.AutoScalingGroups[0].Instances {
		if sdk.ToString(member.InstanceId) == id {
			if member.LifecycleState != at.LifecycleStateInService || !strings.EqualFold(sdk.ToString(member.HealthStatus), "Healthy") {
				return "", errors.New("instance health or lifecycle changed during membership check")
			}
			count++
		}
	}
	if count != 1 {
		return "", errors.New("group membership does not confirm pinned instance")
	}
	return group, nil
}

// errActivityRunning is an Auto Scaling activity that has not finished.
var errActivityRunning = errors.New("unfinished or unknown Auto Scaling group activity")

type activityReader interface {
	DescribeScalingActivities(context.Context, *autoscaling.DescribeScalingActivitiesInput, ...func(*autoscaling.Options)) (*autoscaling.DescribeScalingActivitiesOutput, error)
}

// Auto Scaling returns the newest activities first. Stop after a full page
// of terminal activities whose completion times precede the lookback window.
func checkGroupActivities(ctx context.Context, client activityReader, group string) error {
	if group == "" {
		return errors.New("missing Auto Scaling group")
	}
	in := &autoscaling.DescribeScalingActivitiesInput{AutoScalingGroupName: sdk.String(group)}
	cutoff := time.Now().Add(-24 * time.Hour)
	seen := map[string]bool{}
	for {
		out, err := client.DescribeScalingActivities(ctx, in)
		if err != nil {
			return &readError{"autoscaling:DescribeScalingActivities", err}
		}
		if out == nil {
			return errors.New("missing Auto Scaling activity response")
		}
		oldPage := len(out.Activities) > 0
		for _, a := range out.Activities {
			if sdk.ToString(a.AutoScalingGroupName) != group {
				return errors.New("unconfirmed activity scope")
			}
			switch a.StatusCode {
			case at.ScalingActivityStatusCodeSuccessful, at.ScalingActivityStatusCodeCancelled, at.ScalingActivityStatusCodeFailed:
				if a.EndTime == nil || a.EndTime.IsZero() || !a.EndTime.Before(cutoff) {
					oldPage = false
				}
			default:
				return errActivityRunning
			}
		}
		token := sdk.ToString(out.NextToken)
		if token == "" {
			return nil
		}
		if seen[token] {
			return errors.New("incomplete Auto Scaling activity pagination")
		}
		if oldPage {
			return nil
		}
		seen[token] = true
		in.NextToken = out.NextToken
	}
}
