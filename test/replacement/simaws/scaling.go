// SPDX-License-Identifier: Apache-2.0

package simaws

import (
	"context"
	"errors"
	"slices"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/autoscaling"
	at "github.com/aws/aws-sdk-go-v2/service/autoscaling/types"
)

// Scaling records membership and launch activities for a single backing group.
type Scaling struct {
	mu         sync.Mutex
	group      at.AutoScalingGroup
	activities []at.Activity
}

func (s *Scaling) DescribeAutoScalingInstances(_ context.Context, in *autoscaling.DescribeAutoScalingInstancesInput, _ ...func(*autoscaling.Options)) (*autoscaling.DescribeAutoScalingInstancesOutput, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := &autoscaling.DescribeAutoScalingInstancesOutput{}
	for _, instance := range s.group.Instances {
		if slices.Contains(in.InstanceIds, aws.ToString(instance.InstanceId)) {
			out.AutoScalingInstances = append(out.AutoScalingInstances, at.AutoScalingInstanceDetails{
				InstanceId: instance.InstanceId, AutoScalingGroupName: s.group.AutoScalingGroupName, AvailabilityZone: instance.AvailabilityZone, HealthStatus: instance.HealthStatus, LifecycleState: aws.String(string(instance.LifecycleState)),
			})
		}
	}
	return out, nil
}

func (s *Scaling) DescribeAutoScalingGroups(_ context.Context, in *autoscaling.DescribeAutoScalingGroupsInput, _ ...func(*autoscaling.Options)) (*autoscaling.DescribeAutoScalingGroupsOutput, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := &autoscaling.DescribeAutoScalingGroupsOutput{}
	if slices.Contains(in.AutoScalingGroupNames, aws.ToString(s.group.AutoScalingGroupName)) {
		group := s.group
		group.Instances = slices.Clone(group.Instances)
		out.AutoScalingGroups = []at.AutoScalingGroup{group}
	}
	return out, nil
}

func (s *Scaling) DescribeScalingActivities(_ context.Context, in *autoscaling.DescribeScalingActivitiesInput, _ ...func(*autoscaling.Options)) (*autoscaling.DescribeScalingActivitiesOutput, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if aws.ToString(in.AutoScalingGroupName) != aws.ToString(s.group.AutoScalingGroupName) {
		return nil, errors.New("unknown simulated Auto Scaling group")
	}
	return &autoscaling.DescribeScalingActivitiesOutput{Activities: slices.Clone(s.activities)}, nil
}

func (s *Scaling) TerminateInstanceInAutoScalingGroup(_ context.Context, in *autoscaling.TerminateInstanceInAutoScalingGroupInput, _ ...func(*autoscaling.Options)) (*autoscaling.TerminateInstanceInAutoScalingGroupOutput, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !aws.ToBool(in.ShouldDecrementDesiredCapacity) || aws.ToInt32(s.group.DesiredCapacity) < 2 {
		return nil, errors.New("expected a decrement after the successor joined")
	}
	id := aws.ToString(in.InstanceId)
	index := slices.IndexFunc(s.group.Instances, func(i at.Instance) bool { return aws.ToString(i.InstanceId) == id })
	if index < 0 {
		return nil, errors.New("instance is not a simulated group member")
	}
	now := time.Now()
	activity := at.Activity{ActivityId: aws.String("terminate-" + id), AutoScalingGroupName: s.group.AutoScalingGroupName, AutoScalingGroupARN: s.group.AutoScalingGroupARN, StartTime: &now, EndTime: &now, StatusCode: at.ScalingActivityStatusCodeSuccessful}
	s.group.Instances = slices.Delete(s.group.Instances, index, index+1)
	s.group.DesiredCapacity = aws.Int32(*s.group.DesiredCapacity - 1)
	s.activities = append([]at.Activity{activity}, s.activities...)
	return &autoscaling.TerminateInstanceInAutoScalingGroupOutput{Activity: &activity}, nil
}
