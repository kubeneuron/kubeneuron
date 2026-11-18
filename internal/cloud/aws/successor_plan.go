// SPDX-License-Identifier: Apache-2.0

package aws

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"

	sdk "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/autoscaling"
	at "github.com/aws/aws-sdk-go-v2/service/autoscaling/types"
	"github.com/aws/aws-sdk-go-v2/service/eks"
	et "github.com/aws/aws-sdk-go-v2/service/eks/types"
	"github.com/kubeneuron/kubeneuron/pkg/cloud"
)

// planSuccessor records the baseline from an already verified EKS backing group.
func planSuccessor(s cloud.Successor, group at.AutoScalingGroup) (cloud.Successor, error) {
	if len(group.AvailabilityZones) != 1 || group.AvailabilityZones[0] == "" {
		return s, fmt.Errorf("%w: add-first requires a single availability zone", cloud.ErrAddFirstUnavailable)
	}
	if group.DesiredCapacity == nil || group.MaxSize == nil || *group.DesiredCapacity < 1 || *group.DesiredCapacity >= *group.MaxSize {
		return s, fmt.Errorf("%w: group needs spare capacity below maxSize", cloud.ErrAddFirstUnavailable)
	}
	ids := make([]string, 0, len(group.Instances))
	for _, instance := range group.Instances {
		id := sdk.ToString(instance.InstanceId)
		if id == "" || instance.LifecycleState != at.LifecycleStateInService || sdk.ToString(instance.AvailabilityZone) != group.AvailabilityZones[0] {
			return s, fmt.Errorf("%w: group membership is incomplete or changing", cloud.ErrAddFirstUnavailable)
		}
		ids = append(ids, id)
	}
	slices.Sort(ids)
	if len(slices.Compact(ids)) != int(*group.DesiredCapacity) || len(ids) != int(*group.DesiredCapacity) || !slices.Contains(ids, s.Instance) || sdk.ToString(group.AutoScalingGroupName) == "" || s.Operation == "" {
		return s, fmt.Errorf("%w: group baseline does not confirm the pinned instance and desired membership", cloud.ErrAddFirstUnavailable)
	}
	s.Pool = sdk.ToString(group.AutoScalingGroupName)
	s.State = map[string]string{"before": strconv.Itoa(len(ids)), "members": strings.Join(ids, ","), "zone": group.AvailabilityZones[0]}
	s.Phase = cloud.SuccessorPlanned
	return s, nil
}

// CheckAddFirst checks the same read-only baseline used when preparing a plan.
func (p *managedProvider) CheckAddFirst(ctx context.Context, instanceID string) error {
	_, err := p.readSuccessorPlan(ctx, cloud.Successor{Instance: instanceID, Operation: "preflight"})
	return err
}

func (p *managedProvider) readSuccessorPlan(ctx context.Context, s cloud.Successor) (cloud.Successor, error) {
	if s.Instance == "" || p.cluster == "" {
		return s, fmt.Errorf("%w: missing pinned instance or configured cluster", cloud.ErrAddFirstUnavailable)
	}
	membership, err := p.scaling.DescribeAutoScalingInstances(ctx, &autoscaling.DescribeAutoScalingInstancesInput{InstanceIds: []string{s.Instance}})
	if err != nil {
		return s, err
	}
	if membership == nil || sdk.ToString(membership.NextToken) != "" || len(membership.AutoScalingInstances) != 1 || sdk.ToString(membership.AutoScalingInstances[0].InstanceId) != s.Instance {
		return s, fmt.Errorf("%w: no confirmed ASG for pinned instance", cloud.ErrAddFirstUnavailable)
	}
	name := sdk.ToString(membership.AutoScalingInstances[0].AutoScalingGroupName)
	if name == "" {
		return s, fmt.Errorf("%w: missing ASG name", cloud.ErrAddFirstUnavailable)
	}
	groups, err := p.scaling.DescribeAutoScalingGroups(ctx, &autoscaling.DescribeAutoScalingGroupsInput{AutoScalingGroupNames: []string{name}, IncludeInstances: sdk.Bool(true)})
	if err != nil {
		return s, err
	}
	if groups == nil || len(groups.AutoScalingGroups) != 1 || sdk.ToString(groups.NextToken) != "" || sdk.ToString(groups.AutoScalingGroups[0].AutoScalingGroupName) != name {
		return s, fmt.Errorf("%w: ASG identity is unconfirmed", cloud.ErrAddFirstUnavailable)
	}
	group := groups.AutoScalingGroups[0]
	var nodegroup string
	for _, tag := range group.Tags {
		if sdk.ToString(tag.Key) == "eks:nodegroup-name" {
			nodegroup = sdk.ToString(tag.Value)
		}
	}
	if nodegroup == "" {
		return s, fmt.Errorf("%w: missing EKS managed nodegroup identity", cloud.ErrAddFirstUnavailable)
	}
	// The tag is a discovery hint. Only EKS can confirm the backing ASG.
	out, err := p.eks.DescribeNodegroup(ctx, &eks.DescribeNodegroupInput{ClusterName: &p.cluster, NodegroupName: &nodegroup})
	if err != nil {
		return s, err
	}
	if out == nil || out.Nodegroup == nil {
		return s, fmt.Errorf("%w: missing EKS nodegroup", cloud.ErrAddFirstUnavailable)
	}
	ng := out.Nodegroup
	if sdk.ToString(ng.ClusterName) != p.cluster || sdk.ToString(ng.NodegroupName) != nodegroup || ng.Status != et.NodegroupStatusActive || ng.Resources == nil || len(ng.Resources.AutoScalingGroups) != 1 || sdk.ToString(ng.Resources.AutoScalingGroups[0].Name) != name {
		return s, fmt.Errorf("%w: EKS must confirm exactly one backing ASG for the active nodegroup", cloud.ErrAddFirstUnavailable)
	}
	// Names can be reused after deletion. Pin both resource generations before
	// handing a plan to the controller for persistence.
	asgARN := sdk.ToString(group.AutoScalingGroupARN)
	nodegroupARN := sdk.ToString(ng.NodegroupArn)
	if asgARN == "" || nodegroupARN == "" {
		return s, fmt.Errorf("%w: EKS or ASG resource identity is missing", cloud.ErrAddFirstUnavailable)
	}
	plan, err := planSuccessor(s, group)
	if err == nil {
		plan.State["nodegroup"] = nodegroup
		plan.State["cluster"] = p.cluster
		plan.State["asg_arn"] = asgARN
		plan.State["nodegroup_arn"] = nodegroupARN
	}
	return plan, err
}
