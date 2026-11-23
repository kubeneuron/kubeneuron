// SPDX-License-Identifier: Apache-2.0

package simaws

import (
	"slices"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/autoscaling"
	"github.com/aws/aws-sdk-go-v2/service/eks"
	et "github.com/aws/aws-sdk-go-v2/service/eks/types"
)

func TestTwoZoneGroupLaunchesAcrossZones(t *testing.T) {
	_, scaling, group := NewTwoZoneClients("training")
	read := func() []string {
		t.Helper()
		result, err := scaling.DescribeAutoScalingGroups(t.Context(), &autoscaling.DescribeAutoScalingGroupsInput{AutoScalingGroupNames: []string{"workers"}})
		if err != nil {
			t.Fatal(err)
		}
		g := result.AutoScalingGroups[0]
		if !slices.Equal(g.AvailabilityZones, []string{"us-east-1a", "us-east-1b"}) {
			t.Fatal(g.AvailabilityZones)
		}
		var members []string
		for _, m := range g.Instances {
			members = append(members, aws.ToString(m.InstanceId)+"/"+aws.ToString(m.AvailabilityZone))
		}
		return members
	}
	if got := read(); !slices.Equal(got, []string{"i-pinned/us-east-1a"}) {
		t.Fatal(got)
	}
	_, err := group.UpdateNodegroupConfig(t.Context(), &eks.UpdateNodegroupConfigInput{ClusterName: aws.String("training"), NodegroupName: aws.String("gpu"), ClientRequestToken: aws.String("cross-zone"), ScalingConfig: &et.NodegroupScalingConfig{DesiredSize: aws.Int32(2)}})
	if err != nil {
		t.Fatal(err)
	}
	if got := read(); !slices.Equal(got, []string{"i-pinned/us-east-1a", "i-new/us-east-1b"}) {
		t.Fatal(got)
	}
}
