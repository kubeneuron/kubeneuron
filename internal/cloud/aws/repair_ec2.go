// SPDX-License-Identifier: Apache-2.0

package aws

import (
	"context"
	"errors"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

type instanceStatusReader interface {
	DescribeInstanceStatus(context.Context, *ec2.DescribeInstanceStatusInput, ...func(*ec2.Options)) (*ec2.DescribeInstanceStatusOutput, error)
}

// checkInstanceStatus checks the EC2 portion of the evidence for OpenRepair.
// A clear EC2 status alone cannot establish absence of managed node repair.
func checkInstanceStatus(ctx context.Context, client instanceStatusReader, instanceID string) error {
	if instanceID == "" {
		return errors.New("missing pinned EC2 instance ID")
	}
	out, err := client.DescribeInstanceStatus(ctx, &ec2.DescribeInstanceStatusInput{InstanceIds: []string{instanceID}, IncludeAllInstances: awssdk.Bool(true)})
	if err != nil {
		return err
	}
	if out == nil || len(out.InstanceStatuses) != 1 || awssdk.ToString(out.NextToken) != "" {
		return errors.New("incomplete EC2 instance status")
	}
	status := out.InstanceStatuses[0]
	if awssdk.ToString(status.InstanceId) != instanceID {
		return errors.New("EC2 status does not identify the pinned instance")
	}
	if status.InstanceState == nil || status.InstanceState.Name != ec2types.InstanceStateNameRunning {
		return errors.New("EC2 instance is not confirmed running")
	}
	if len(status.Events) > 0 {
		return errors.New("EC2 instance has scheduled events")
	}
	return nil
}
