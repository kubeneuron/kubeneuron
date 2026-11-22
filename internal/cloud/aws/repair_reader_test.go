// SPDX-License-Identifier: Apache-2.0

package aws

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	sdk "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/autoscaling"
	at "github.com/aws/aws-sdk-go-v2/service/autoscaling/types"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ct "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/eks"
	et "github.com/aws/aws-sdk-go-v2/service/eks/types"
	"github.com/aws/smithy-go"
	"github.com/kubeneuron/kubeneuron/pkg/cloud"
)

type groupHistory struct {
	updateReader
	group      et.Nodegroup
	reads      int
	lists      int
	flip       string
	describeEr error
}

func (h *groupHistory) DescribeNodegroup(_ context.Context, in *eks.DescribeNodegroupInput, _ ...func(*eks.Options)) (*eks.DescribeNodegroupOutput, error) {
	h.reads++
	if h.describeEr != nil {
		return nil, h.describeEr
	}
	group := h.group
	if h.reads == 2 {
		switch h.flip {
		case "repair":
			group.NodeRepairConfig = &et.NodeRepairConfig{Enabled: sdk.Bool(true)}
		case "modifiedAt":
			// seen live: EKS moves modifiedAt with no update and no API call
			group.ModifiedAt = sdk.Time(time.Now())
		case "launch template":
			group.LaunchTemplate = &et.LaunchTemplateSpecification{Id: sdk.String("lt"), Version: sdk.String("2")}
		}
	}
	return &eks.DescribeNodegroupOutput{Nodegroup: &group}, nil
}
func (h *groupHistory) ListUpdates(context.Context, *eks.ListUpdatesInput, ...func(*eks.Options)) (*eks.ListUpdatesOutput, error) {
	h.lists++
	if h.flip == "new update" && h.lists == 2 {
		return &eks.ListUpdatesOutput{UpdateIds: []string{"new"}}, nil
	}
	return &eks.ListUpdatesOutput{}, nil
}

type quietASG struct{ membershipFixture }

func (quietASG) DescribeScalingActivities(context.Context, *autoscaling.DescribeScalingActivitiesInput, ...func(*autoscaling.Options)) (*autoscaling.DescribeScalingActivitiesOutput, error) {
	return &autoscaling.DescribeScalingActivitiesOutput{}, nil
}

func repairFixture(t *testing.T, config *et.NodeRepairConfig, flip string) (repairReader, *groupHistory, cloud.NodeRef) {
	group := et.Nodegroup{ClusterName: sdk.String("training"), NodegroupName: sdk.String("gpu"), Status: et.NodegroupStatusActive, ModifiedAt: sdk.Time(time.Now().Add(-time.Minute)), NodeRepairConfig: config, Resources: &et.NodegroupResources{AutoScalingGroups: []et.AutoScalingGroup{{Name: sdk.String("workers")}}}}
	history := &groupHistory{group: group, flip: flip}
	asg := at.AutoScalingGroup{AutoScalingGroupName: sdk.String("workers"), Instances: []at.Instance{{InstanceId: sdk.String("i-pinned"), HealthStatus: sdk.String("Healthy"), LifecycleState: at.LifecycleStateInService}}}
	status := ct.InstanceStatus{InstanceId: sdk.String("i-pinned"), InstanceState: &ct.InstanceState{Name: ct.InstanceStateNameRunning}}
	r := repairReader{cluster: "training", eks: history, scaling: quietASG{membershipFixture{t, &autoscaling.DescribeAutoScalingGroupsOutput{AutoScalingGroups: []at.AutoScalingGroup{asg}}}}, ec2: statusFixture{t, &ec2.DescribeInstanceStatusOutput{InstanceStatuses: []ct.InstanceStatus{status}}}}
	ref := cloud.NodeRef{Labels: map[string]string{eksNodegroupLabel: "gpu"}, Identity: cloud.NodeIdentity{Node: "n", NodeUID: "uid", BootID: "boot", InstanceID: "i-pinned", ProviderID: "aws:///az/i-pinned"}}
	return r, history, ref
}

const repairOnMessage = "EKS node auto repair is enabled for node group gpu, so KubeNeuron does not replace its nodes. Leave repairs of this group to EKS, or turn EKS node auto repair off for this group: aws eks update-nodegroup-config --cluster-name training --nodegroup-name gpu --node-repair-config enabled=false. EKS then no longer repairs nodes in this group. KubeNeuron checks again every minute."

func TestOpenRepairRechecksManagedNodegroup(t *testing.T) {
	for _, tc := range []struct {
		name   string
		config *et.NodeRepairConfig
		flip   string
		hold   string // empty: Known
		reads  int
	}{
		{"settled", &et.NodeRepairConfig{Enabled: sdk.Bool(false)}, "", "", 2},
		// eksctl's default group: EKS reports nodeRepairConfig null, and repair is off by default
		{"no repair configuration", nil, "", "", 2},
		{"repair configuration without enabled", &et.NodeRepairConfig{}, "", "", 2},
		{"modifiedAt moves on its own", nil, "modifiedAt", "", 2},
		{"repair enabled during observation", nil, "repair", cloud.HoldChangedDuringRun, 2},
		{"launch template changed during observation", nil, "launch template", cloud.HoldChangedDuringRun, 2},
		{"update started during observation", nil, "new update", cloud.HoldChangedDuringRun, 2},
		{"repair initially enabled", &et.NodeRepairConfig{Enabled: sdk.Bool(true)}, "", cloud.HoldNativeRepair, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, history, ref := repairFixture(t, tc.config, tc.flip)
			obs, err := r.OpenRepair(t.Context(), ref)
			var hold *cloud.RecoveryHold
			switch {
			case tc.hold == "" && (err != nil || !obs.Known):
				t.Fatalf("observation %+v, error %v", obs, err)
			case tc.hold != "" && (!errors.As(err, &hold) || hold.Code != tc.hold || obs.Known):
				t.Fatalf("want hold %s: observation %+v, error %v", tc.hold, obs, err)
			case history.reads != tc.reads:
				t.Fatalf("reads %d", history.reads)
			}
			if tc.hold == cloud.HoldNativeRepair && hold.Message() != repairOnMessage {
				t.Fatalf("message %q", hold.Message())
			}
		})
	}
}

func TestRepairPolicyReadsMissingConfigurationAsOff(t *testing.T) {
	for _, tc := range []struct {
		name   string
		config *et.NodeRepairConfig
		mode   cloud.RepairMode
	}{
		{"null", nil, cloud.RepairOff},
		{"enabled unset", &et.NodeRepairConfig{}, cloud.RepairOff},
		{"disabled", &et.NodeRepairConfig{Enabled: sdk.Bool(false)}, cloud.RepairOff},
		{"enabled", &et.NodeRepairConfig{Enabled: sdk.Bool(true)}, cloud.RepairOn},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, _, ref := repairFixture(t, tc.config, "")
			policy, err := r.RepairPolicy(t.Context(), ref)
			if err != nil || policy.Mode != tc.mode {
				t.Fatalf("policy %+v, error %v", policy, err)
			}
			if tc.mode == cloud.RepairOn && policy.Message() != repairOnMessage {
				t.Fatalf("message %q", policy.Message())
			}
		})
	}
}

func TestRepairReadFailuresNameTheCall(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want string
	}{
		{"denied", &smithy.GenericAPIError{Code: "AccessDeniedException", Message: "User: arn:aws:sts::123456789012:assumed-role/x is not authorized"},
			"KubeNeuron could not read node group gpu: eks:DescribeNodegroup was denied. Allow eks:DescribeNodegroup in the controller's IAM role. KubeNeuron retries every minute."},
		{"throttled", &smithy.GenericAPIError{Code: "ThrottlingException"},
			"KubeNeuron could not read node group gpu: eks:DescribeNodegroup failed. KubeNeuron retries every minute."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, history, ref := repairFixture(t, nil, "")
			history.describeEr = tc.err
			for _, call := range []func() error{
				func() error { _, err := r.RepairPolicy(t.Context(), ref); return err },
				func() error { _, err := r.OpenRepair(t.Context(), ref); return err },
			} {
				var hold *cloud.RecoveryHold
				err := call()
				if !errors.As(err, &hold) || hold.Code != cloud.HoldReadFailed || hold.Message() != tc.want {
					t.Fatalf("got %v", err)
				}
				// the raw provider error is for the log, never the operator text
				if strings.Contains(hold.Message(), "arn:") || !errors.Is(err, tc.err) {
					t.Fatalf("raw error leaked or lost: %q", hold.Message())
				}
			}
		})
	}
}

func TestNodegroupDiscoveryRequiresNodeLabel(t *testing.T) {
	r := repairReader{cluster: "training"}
	ref := cloud.NodeRef{Identity: cloud.NodeIdentity{Node: "n", NodeUID: "uid", BootID: "boot", InstanceID: "i-pinned", ProviderID: "aws:///az/i-pinned"}}
	policy, err := r.RepairPolicy(t.Context(), ref)
	var hold *cloud.RecoveryHold
	if !errors.As(err, &hold) || hold.Code != cloud.HoldUnconfirmed || policy.Mode != cloud.RepairUnknown {
		t.Fatalf("missing label: policy %+v, error %v", policy, err)
	}
	// the package's own error text goes to the log, not the operator
	want := "KubeNeuron could not confirm that EKS is not repairing the node's EKS node group: EKS, Auto Scaling or EC2 did not confirm the node's group and state. KubeNeuron checks again every minute."
	if hold.Message() != want || hold.Cause == nil {
		t.Fatalf("message %q", hold.Message())
	}
}
