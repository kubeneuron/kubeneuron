// SPDX-License-Identifier: Apache-2.0

package aws

import (
	"context"
	"errors"
	"time"

	sdk "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/eks"
	et "github.com/aws/aws-sdk-go-v2/service/eks/types"
	"github.com/kubeneuron/kubeneuron/pkg/cloud"
)

const eksNodegroupLabel = "eks.amazonaws.com/nodegroup"

type nodegroupReader interface {
	updateReader
	DescribeNodegroup(context.Context, *eks.DescribeNodegroupInput, ...func(*eks.Options)) (*eks.DescribeNodegroupOutput, error)
}
type scalingReader interface {
	membershipReader
	activityReader
}

// repairReader has only read clients. Execution is enabled separately once
// action-specific authorization has been checked.
type repairReader struct {
	checkAutoscalerGroup func(context.Context, string) (bool, error)
	cluster              string
	eks                  nodegroupReader
	scaling              scalingReader
	ec2                  instanceStatusReader
}

func (r *repairReader) managedGroup(ctx context.Context, ref cloud.NodeRef) (*et.Nodegroup, string, error) {
	if err := ref.Identity.Validate(); err != nil {
		return nil, "", err
	}
	id, err := instanceIDFromProviderID(ref.Identity.ProviderID)
	if err != nil || id != ref.Identity.InstanceID {
		return nil, "", errors.New("pinned EC2 identity is inconsistent")
	}
	nodegroup := ref.Labels[eksNodegroupLabel]
	if r.cluster == "" || nodegroup == "" {
		return nil, "", errors.New("EKS cluster name and nodegroup label required")
	}
	out, err := r.eks.DescribeNodegroup(ctx, &eks.DescribeNodegroupInput{ClusterName: &r.cluster, NodegroupName: &nodegroup})
	if err != nil {
		return nil, "", err
	}
	if out == nil || out.Nodegroup == nil {
		return nil, "", errors.New("missing EKS nodegroup")
	}
	ng := out.Nodegroup
	if sdk.ToString(ng.ClusterName) != r.cluster || sdk.ToString(ng.NodegroupName) != nodegroup || ng.Status != et.NodegroupStatusActive {
		return nil, "", errors.New("active EKS nodegroup identity is unconfirmed")
	}
	if ng.Resources == nil || ng.NodeRepairConfig == nil || ng.NodeRepairConfig.Enabled == nil {
		return nil, "", errors.New("missing EKS repair configuration or backing groups")
	}
	var names []string
	for _, g := range ng.Resources.AutoScalingGroups {
		name := sdk.ToString(g.Name)
		if name == "" {
			return nil, "", errors.New("missing EKS backing group name")
		}
		names = append(names, name)
	}
	group, err := confirmedGroup(ctx, r.scaling, id, names)
	return ng, group, err
}

func (r *repairReader) RepairPolicy(ctx context.Context, ref cloud.NodeRef) (cloud.RepairPolicy, error) {
	ng, _, err := r.managedGroup(ctx, ref)
	if err != nil {
		return cloud.RepairPolicy{Mode: cloud.RepairUnknown}, err
	}
	if *ng.NodeRepairConfig.Enabled {
		return cloud.RepairPolicy{Mode: cloud.RepairOn, Reason: "EKS native node repair enabled"}, nil
	}
	return cloud.RepairPolicy{Mode: cloud.RepairOff, Reason: "EKS native node repair disabled; stop/start recycle unavailable in a managed nodegroup"}, nil
}

func (r *repairReader) OpenRepair(ctx context.Context, ref cloud.NodeRef) (cloud.RepairObservation, error) {
	ng, group, err := r.managedGroup(ctx, ref)
	if err != nil {
		return cloud.RepairObservation{}, err
	}
	if *ng.NodeRepairConfig.Enabled {
		return cloud.RepairObservation{Reason: "EKS native node repair enabled"}, nil
	}
	if err = checkUpdateHistory(ctx, r.eks, r.cluster, sdk.ToString(ng.NodegroupName), ng.ModifiedAt, time.Now()); err != nil {
		return cloud.RepairObservation{}, err
	}
	if err = checkGroupActivities(ctx, r.scaling, group); err != nil {
		return cloud.RepairObservation{}, err
	}
	if err = checkInstanceStatus(ctx, r.ec2, ref.Identity.InstanceID); err != nil {
		return cloud.RepairObservation{}, err
	}
	latest, currentGroup, err := r.managedGroup(ctx, ref)
	if err != nil {
		return cloud.RepairObservation{}, err
	}
	if *latest.NodeRepairConfig.Enabled || currentGroup != group || latest.ModifiedAt == nil || !latest.ModifiedAt.Equal(*ng.ModifiedAt) {
		return cloud.RepairObservation{Reason: "EKS policy or backing group changed during observation"}, nil
	}
	return cloud.RepairObservation{Known: true, Reason: "EKS repair disabled and settled; ASG and EC2 checks clear"}, nil
}
