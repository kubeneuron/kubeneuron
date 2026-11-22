// SPDX-License-Identifier: Apache-2.0

package aws

import (
	"context"
	awssdk "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"testing"
)

type statusFixture struct {
	t   *testing.T
	out *ec2.DescribeInstanceStatusOutput
}

func (f statusFixture) DescribeInstanceStatus(_ context.Context, in *ec2.DescribeInstanceStatusInput, _ ...func(*ec2.Options)) (*ec2.DescribeInstanceStatusOutput, error) {
	f.t.Helper()
	if len(in.InstanceIds) != 1 || in.InstanceIds[0] != "i-pinned" || !awssdk.ToBool(in.IncludeAllInstances) {
		f.t.Fatalf("unexpected status request: %+v", in)
	}
	return f.out, nil
}

func TestPinnedEC2Status(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*ec2.DescribeInstanceStatusOutput)
	}{
		{"clear", nil},
		{"missing", func(o *ec2.DescribeInstanceStatusOutput) { o.InstanceStatuses = nil }},
		{"another instance", func(o *ec2.DescribeInstanceStatusOutput) { o.InstanceStatuses[0].InstanceId = awssdk.String("i-other") }},
		{"unknown state", func(o *ec2.DescribeInstanceStatusOutput) { o.InstanceStatuses[0].InstanceState = nil }},
		{"stopping", func(o *ec2.DescribeInstanceStatusOutput) {
			o.InstanceStatuses[0].InstanceState.Name = ec2types.InstanceStateNameStopping
		}},
		{"scheduled event", func(o *ec2.DescribeInstanceStatusOutput) {
			o.InstanceStatuses[0].Events = []ec2types.InstanceStatusEvent{{}}
		}},
		{"partial response", func(o *ec2.DescribeInstanceStatusOutput) { o.NextToken = awssdk.String("more") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o := &ec2.DescribeInstanceStatusOutput{InstanceStatuses: []ec2types.InstanceStatus{{InstanceId: awssdk.String("i-pinned"), InstanceState: &ec2types.InstanceState{Name: ec2types.InstanceStateNameRunning}}}}
			if tc.change != nil {
				tc.change(o)
			}
			err := checkInstanceStatus(t.Context(), statusFixture{t, o}, "i-pinned")
			if (err != nil) != (tc.change != nil) {
				t.Fatalf("unexpected status verdict: %v", err)
			}
		})
	}
}
