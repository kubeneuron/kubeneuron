// SPDX-License-Identifier: Apache-2.0
package simaws_test

import (
	"context"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	et "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	provider "github.com/kubeneuron/kubeneuron/internal/cloud/aws"
	"github.com/kubeneuron/kubeneuron/test/replacement/simaws"
	"testing"
)

func TestPermissionProbeLeavesPinnedInstanceRunning(t *testing.T) {
	instances, _, _ := simaws.NewClients("training")
	if err := provider.NewWithAPI(instances).CheckReplacePermission(context.Background(), "i-pinned"); err != nil {
		t.Fatal(err)
	}
	out, err := instances.DescribeInstanceStatus(context.Background(), &ec2.DescribeInstanceStatusInput{InstanceIds: []string{"i-pinned"}, IncludeAllInstances: aws.Bool(true)})
	if err != nil {
		t.Fatal(err)
	}
	if state := out.InstanceStatuses[0].InstanceState.Name; state != et.InstanceStateNameRunning {
		t.Fatalf("after permission probe: state = %s", state)
	}
}
