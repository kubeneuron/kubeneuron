// SPDX-License-Identifier: Apache-2.0

package aws

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	sdk "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/autoscaling"
	at "github.com/aws/aws-sdk-go-v2/service/autoscaling/types"
	"github.com/aws/aws-sdk-go-v2/service/eks"
	"github.com/kubeneuron/kubeneuron/pkg/cloud"
)

// observeSuccessor performs the read-only transition after EKS accepted the request.
func (p *managedProvider) observeSuccessor(ctx context.Context, s cloud.Successor) (cloud.Successor, error) {
	complete, err := p.scalingUpdateComplete(ctx, s)
	if err != nil || !complete {
		return s, err
	}
	group, err := p.successorGroup(ctx, s)
	if err != nil {
		return s, err
	}
	members := strings.Split(s.State["members"], ",")
	if group.DesiredCapacity != nil && int(*group.DesiredCapacity) == len(members)+1 && s.State["before"] == strconv.Itoa(len(members)) && len(group.Instances) == len(members) {
		baseline := group
		baseline.DesiredCapacity = sdk.Int32(int32(len(members)))
		if settledMembership(s, baseline, members) {
			return s, checkSuccessorActivities(ctx, p.scaling, s, "", "")
		}
	}
	if pending, ok := launchingMember(s, group, members); ok {
		return s, checkSuccessorActivities(ctx, p.scaling, s, "", pending)
	}
	candidate, err := successorCandidate(s, group)
	if err != nil {
		return s, err
	}
	if err := checkSuccessorActivities(ctx, p.scaling, s, candidate, ""); err != nil {
		return s, err
	}
	s.ProviderID = "aws:///" + s.State["zone"] + "/" + candidate
	s.Phase = cloud.SuccessorCreated
	return s, nil
}

// PrepareSuccessor advances one persisted phase of the EKS add-first operation.
func (p *managedProvider) PrepareSuccessor(ctx context.Context, s cloud.Successor) (cloud.Successor, error) {
	if s.UnrequestedPlan() {
		return s, fmt.Errorf("%w: planned successor has no RequestedAt", cloud.ErrNeedsOperator)
	}
	switch s.Phase {
	case "":
		return p.readSuccessorPlan(ctx, s)
	case cloud.SuccessorPlanned:
		if err := p.verifySuccessorBaseline(ctx, s); err != nil {
			if !errors.Is(err, cloud.ErrNeedsOperator) {
				return s, err
			}
			group, readErr := p.successorGroup(ctx, s)
			if readErr != nil {
				return s, readErr
			}
			before, parseErr := strconv.ParseInt(s.State["before"], 10, 32)
			if parseErr != nil || group.DesiredCapacity == nil || int64(*group.DesiredCapacity) != before+1 || sdk.ToString(group.AutoScalingGroupARN) != s.State["asg_arn"] || s.State["asg_arn"] == "" {
				return s, err
			}
			next, replayErr := sendScaleRequest(ctx, p.updater, p.cluster, s, true)
			if replayErr != nil {
				return s, fmt.Errorf("%w: EKS nodegroup %s ASG %s desired was %d, now %d: %v", cloud.ErrNeedsOperator, s.State["nodegroup"], s.Pool, before, *group.DesiredCapacity, replayErr)
			}
			return next, nil
		}
		return requestSuccessor(ctx, p.updater, p.cluster, s)
	case cloud.SuccessorRequested:
		return p.observeSuccessor(ctx, s)
	case cloud.SuccessorCreated:
		return s, nil
	default:
		return s, fmt.Errorf("%w: unknown EKS successor phase %q", cloud.ErrNeedsOperator, s.Phase)
	}
}

func (p *managedProvider) successorGroup(ctx context.Context, s cloud.Successor) (at.AutoScalingGroup, error) {
	if s.State["cluster"] != p.cluster || p.cluster == "" {
		return at.AutoScalingGroup{}, fmt.Errorf("%w: EKS cluster changed", cloud.ErrNeedsOperator)
	}
	group := s.State["nodegroup"]
	ng, err := p.eks.DescribeNodegroup(ctx, &eks.DescribeNodegroupInput{ClusterName: &p.cluster, NodegroupName: &group})
	if err != nil {
		return at.AutoScalingGroup{}, err
	}
	if ng == nil || ng.Nodegroup == nil || s.State["nodegroup_arn"] == "" || sdk.ToString(ng.Nodegroup.NodegroupArn) != s.State["nodegroup_arn"] || ng.Nodegroup.Resources == nil || len(ng.Nodegroup.Resources.AutoScalingGroups) != 1 || sdk.ToString(ng.Nodegroup.Resources.AutoScalingGroups[0].Name) != s.Pool || s.Pool == "" {
		return at.AutoScalingGroup{}, fmt.Errorf("%w: EKS no longer confirms the planned nodegroup and backing ASG", cloud.ErrNeedsOperator)
	}
	groups, err := p.scaling.DescribeAutoScalingGroups(ctx, &autoscaling.DescribeAutoScalingGroupsInput{AutoScalingGroupNames: []string{s.Pool}, IncludeInstances: sdk.Bool(true)})
	if err != nil {
		return at.AutoScalingGroup{}, err
	}
	if groups == nil || len(groups.AutoScalingGroups) != 1 || sdk.ToString(groups.NextToken) != "" {
		return at.AutoScalingGroup{}, fmt.Errorf("%w: incomplete ASG response", cloud.ErrNeedsOperator)
	}
	return groups.AutoScalingGroups[0], nil
}

// launchingMember is the raised group with every saved member InService and
// one new instance in the plan's zone that is not InService yet; it returns
// that instance.
func launchingMember(s cloud.Successor, g at.AutoScalingGroup, members []string) (string, bool) {
	zone := s.State["zone"]
	if g.DesiredCapacity == nil || int(*g.DesiredCapacity) != len(members)+1 || s.State["before"] != strconv.Itoa(len(members)) || len(g.Instances) != len(members)+1 || zone == "" {
		return "", false
	}
	pending := ""
	for _, member := range g.Instances {
		id := sdk.ToString(member.InstanceId)
		if sdk.ToString(member.AvailabilityZone) != zone || id == "" {
			return "", false
		}
		if slices.Contains(members, id) {
			if member.LifecycleState != at.LifecycleStateInService {
				return "", false
			}
			continue
		}
		switch member.LifecycleState {
		case at.LifecycleStatePending, at.LifecycleStatePendingWait, at.LifecycleStatePendingProceed:
			if pending != "" {
				return "", false
			}
			pending = id
		default:
			return "", false
		}
	}
	return pending, pending != ""
}
