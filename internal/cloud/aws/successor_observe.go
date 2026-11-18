// SPDX-License-Identifier: Apache-2.0

package aws

import (
	"context"
	"fmt"

	sdk "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/autoscaling"
	"github.com/aws/aws-sdk-go-v2/service/eks"
	"github.com/kubeneuron/kubeneuron/pkg/cloud"
)

// observeSuccessor performs the read-only transition after EKS accepted the request.
func (p *managedProvider) observeSuccessor(ctx context.Context, s cloud.Successor) (cloud.Successor, error) {
	complete, err := p.scalingUpdateComplete(ctx, s)
	if err != nil || !complete {
		return s, err
	}
	group := s.State["nodegroup"]
	ng, err := p.eks.DescribeNodegroup(ctx, &eks.DescribeNodegroupInput{ClusterName: &p.cluster, NodegroupName: &group})
	if err != nil {
		return s, err
	}
	if ng == nil || ng.Nodegroup == nil || s.State["nodegroup_arn"] == "" || sdk.ToString(ng.Nodegroup.NodegroupArn) != s.State["nodegroup_arn"] || ng.Nodegroup.Resources == nil || len(ng.Nodegroup.Resources.AutoScalingGroups) != 1 || sdk.ToString(ng.Nodegroup.Resources.AutoScalingGroups[0].Name) != s.Pool || s.Pool == "" {
		return s, fmt.Errorf("%w: EKS no longer confirms the planned nodegroup and backing ASG", cloud.ErrNeedsOperator)
	}
	groups, err := p.scaling.DescribeAutoScalingGroups(ctx, &autoscaling.DescribeAutoScalingGroupsInput{AutoScalingGroupNames: []string{s.Pool}, IncludeInstances: sdk.Bool(true)})
	if err != nil {
		return s, err
	}
	if groups == nil || len(groups.AutoScalingGroups) != 1 || sdk.ToString(groups.NextToken) != "" {
		return s, fmt.Errorf("%w: incomplete ASG response", cloud.ErrNeedsOperator)
	}
	candidate, err := successorCandidate(s, groups.AutoScalingGroups[0])
	if err != nil {
		return s, err
	}
	if err := checkSuccessorActivities(ctx, p.scaling, s, candidate); err != nil {
		return s, err
	}
	s.ProviderID = "aws:///" + s.State["zone"] + "/" + candidate
	s.Phase = cloud.SuccessorCreated
	return s, nil
}

// PrepareSuccessor advances one persisted phase of the EKS add-first operation.
func (p *managedProvider) PrepareSuccessor(ctx context.Context, s cloud.Successor) (cloud.Successor, error) {
	switch s.Phase {
	case "":
		return p.readSuccessorPlan(ctx, s)
	case cloud.SuccessorPlanned:
		if err := p.verifySuccessorBaseline(ctx, s); err != nil {
			return s, err
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
