// SPDX-License-Identifier: Apache-2.0

package aws

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	sdk "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/eks"
	et "github.com/aws/aws-sdk-go-v2/service/eks/types"
	"github.com/aws/smithy-go"
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

// readError names the AWS call whose read failed.
type readError struct {
	call string
	err  error
}

func (e *readError) Error() string { return e.call + ": " + e.err.Error() }
func (e *readError) Unwrap() error { return e.err }

// repairReader has only read clients. Execution is enabled separately once
// action-specific authorization has been checked.
type repairReader struct {
	checkAutoscalerGroup func(context.Context, string) (bool, error)
	cluster              string
	eks                  nodegroupReader
	scaling              scalingReader
	ec2                  instanceStatusReader
	updates              *updateCache
}

// repairEnabled reads the group's native repair setting. An absent
// nodeRepairConfig, or one without enabled, is off: the EKS API reference
// documents node auto repair as disabled by default, and a group created
// without the setting (eksctl's default) reports null.
func repairEnabled(ng *et.Nodegroup) bool {
	return ng.NodeRepairConfig != nil && sdk.ToBool(ng.NodeRepairConfig.Enabled)
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
		return nil, "", &readError{"eks:DescribeNodegroup", err}
	}
	if out == nil || out.Nodegroup == nil {
		return nil, "", errors.New("missing EKS nodegroup")
	}
	ng := out.Nodegroup
	if sdk.ToString(ng.ClusterName) != r.cluster || sdk.ToString(ng.NodegroupName) != nodegroup || ng.Status != et.NodegroupStatusActive {
		return nil, "", errors.New("active EKS nodegroup identity is unconfirmed")
	}
	if ng.Resources == nil {
		return nil, "", errors.New("missing EKS backing groups")
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

// repairOn is the hold while EKS node auto repair is enabled for group. It
// holds every replacement, add-first or not.
func (r *repairReader) repairOn(group string) *cloud.RecoveryHold {
	command := fmt.Sprintf("aws eks update-nodegroup-config --cluster-name %s --nodegroup-name %s --node-repair-config enabled=false", r.cluster, group)
	return &cloud.RecoveryHold{Code: cloud.HoldNativeRepair,
		Reason:     fmt.Sprintf("EKS node auto repair is enabled for node group %s, so KubeNeuron does not replace its nodes.", group),
		Action:     fmt.Sprintf("Leave repairs of this group to EKS, or turn EKS node auto repair off for this group: %s. EKS then no longer repairs nodes in this group. KubeNeuron checks again every minute.", command),
		PoolReason: "EKS node auto repair is enabled for this node group",
		PoolAction: fmt.Sprintf("leave repairs to EKS, or turn it off with %s; EKS then no longer repairs nodes in this group", command)}
}

// hold gives an error on the repair path its operator-facing text. The raw
// provider error stays in Cause, for the controller log only.
func (r *repairReader) hold(ref cloud.NodeRef, err error) error {
	var hold *cloud.RecoveryHold
	if err == nil || errors.As(err, &hold) {
		return err
	}
	group := "node group " + ref.Labels[eksNodegroupLabel]
	if ref.Labels[eksNodegroupLabel] == "" {
		group = "the node's EKS node group"
	}
	var read *readError
	if errors.As(err, &read) {
		var api smithy.APIError
		if errors.As(err, &api) && slices.Contains([]string{"AccessDenied", "AccessDeniedException", "UnauthorizedOperation", "UnauthorizedException"}, api.ErrorCode()) {
			return &cloud.RecoveryHold{Code: cloud.HoldReadFailed, Cause: err,
				Reason:     fmt.Sprintf("KubeNeuron could not read %s: %s was denied.", group, read.call),
				Action:     fmt.Sprintf("Allow %s in the controller's IAM role. KubeNeuron retries every minute.", read.call),
				PoolReason: fmt.Sprintf("could not read the node group: %s was denied", read.call),
				PoolAction: fmt.Sprintf("allow %s in the controller's IAM role", read.call)}
		}
		return &cloud.RecoveryHold{Code: cloud.HoldReadFailed, Cause: err,
			Reason:     fmt.Sprintf("KubeNeuron could not read %s: %s failed.", group, read.call),
			Action:     "KubeNeuron retries every minute.",
			PoolReason: fmt.Sprintf("could not read the node group: %s failed", read.call)}
	}
	// Every other error on this path is one of this package's own checks; its
	// text stays in Cause, for the controller log.
	return &cloud.RecoveryHold{Code: cloud.HoldUnconfirmed, Cause: err,
		Reason: fmt.Sprintf("KubeNeuron could not confirm that EKS is not repairing %s: EKS, Auto Scaling or EC2 did not confirm the node's group and state.", group),
		Action: "KubeNeuron checks again every minute."}
}

func (r *repairReader) RepairPolicy(ctx context.Context, ref cloud.NodeRef) (cloud.RepairPolicy, error) {
	ng, _, err := r.managedGroup(ctx, ref)
	if err != nil {
		return cloud.RepairPolicy{Mode: cloud.RepairUnknown}, r.hold(ref, err)
	}
	if repairEnabled(ng) {
		hold := r.repairOn(sdk.ToString(ng.NodegroupName))
		return cloud.RepairPolicy{Mode: cloud.RepairOn, Reason: hold.Reason, Action: hold.Action, Pool: hold}, nil
	}
	reason := "EKS native node repair disabled; stop/start recycle unavailable in a managed nodegroup"
	if ng.NodeRepairConfig == nil {
		reason = "EKS native node repair not configured (off by default); stop/start recycle unavailable in a managed nodegroup"
	}
	return cloud.RepairPolicy{Mode: cloud.RepairOff, Reason: reason}, nil
}

// groupState is what must not change while OpenRepair observes the group.
// modifiedAt is not part of it: EKS moves it on its own.
func groupState(ng *et.Nodegroup, updates []string) string {
	var groups []string
	for _, g := range ng.Resources.AutoScalingGroups {
		groups = append(groups, sdk.ToString(g.Name))
	}
	slices.Sort(groups)
	template := ""
	if lt := ng.LaunchTemplate; lt != nil {
		template = sdk.ToString(lt.Id) + "/" + sdk.ToString(lt.Name) + "/" + sdk.ToString(lt.Version)
	}
	return fmt.Sprint(repairEnabled(ng), ng.Status, sdk.ToString(ng.Version), sdk.ToString(ng.ReleaseVersion), template, groups, updates)
}

func (r *repairReader) OpenRepair(ctx context.Context, ref cloud.NodeRef) (cloud.RepairObservation, error) {
	obs, err := r.openRepair(ctx, ref)
	return obs, r.hold(ref, err)
}

func (r *repairReader) openRepair(ctx context.Context, ref cloud.NodeRef) (cloud.RepairObservation, error) {
	ng, group, err := r.managedGroup(ctx, ref)
	if err != nil {
		return cloud.RepairObservation{}, err
	}
	name := sdk.ToString(ng.NodegroupName)
	if repairEnabled(ng) {
		return cloud.RepairObservation{}, r.repairOn(name)
	}
	updates, err := checkUpdateHistory(ctx, r.eks, r.updates, r.cluster, name, time.Now())
	if err != nil {
		return cloud.RepairObservation{}, err
	}
	if err = checkGroupActivities(ctx, r.scaling, group); errors.Is(err, errActivityRunning) {
		return cloud.RepairObservation{}, &cloud.RecoveryHold{Code: cloud.HoldActivityRunning,
			Reason: fmt.Sprintf("The Auto Scaling group of node group %s has an unfinished activity.", name),
			Action: "KubeNeuron checks again every minute."}
	} else if err != nil {
		return cloud.RepairObservation{}, err
	}
	if err = checkInstanceStatus(ctx, r.ec2, ref.Identity.InstanceID); err != nil {
		return cloud.RepairObservation{}, err
	}
	latest, currentGroup, err := r.managedGroup(ctx, ref)
	if err != nil {
		return cloud.RepairObservation{}, err
	}
	latestUpdates, err := listUpdateIDs(ctx, r.eks, r.cluster, name)
	if err != nil {
		return cloud.RepairObservation{}, err
	}
	if currentGroup != group || groupState(latest, latestUpdates) != groupState(ng, updates) {
		return cloud.RepairObservation{}, &cloud.RecoveryHold{Code: cloud.HoldChangedDuringRun,
			Reason: fmt.Sprintf("Node group %s changed while KubeNeuron was checking it.", name),
			Action: "KubeNeuron checks again every minute."}
	}
	return cloud.RepairObservation{Known: true, Reason: "EKS repair disabled and settled; ASG and EC2 checks clear"}, nil
}
