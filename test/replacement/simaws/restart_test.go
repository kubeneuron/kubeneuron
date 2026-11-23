// SPDX-License-Identifier: Apache-2.0

package simaws

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	et "github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

func TestLostResponseRestoresDeleteEffect(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	ec, scaling, _ := NewClients("training")
	if err := ec.EnableLostResponse(path); err != nil {
		t.Fatal(err)
	}
	ec.States["i-new"] = et.InstanceStateNameRunning
	scaling.group.Instances[0].InstanceId = aws.String("i-new")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	input := &ec2.TerminateInstancesInput{InstanceIds: []string{"i-new"}}
	if _, err := ec.TerminateInstances(ctx, input); !errors.Is(err, context.Canceled) {
		t.Fatalf("response: %v", err)
	}
	restarted, group, _ := NewClients("training")
	if err := restarted.EnableLostResponse(path); err != nil {
		t.Fatal(err)
	}
	if restarted.States["i-new"] != et.InstanceStateNameTerminated || aws.ToString(group.group.Instances[0].InstanceId) != "i-final" || restarted.snapshot.Calls != 1 || restarted.snapshot.Effects != 1 {
		t.Fatal("accepted delete missing after restart")
	}
	if _, err := restarted.TerminateInstances(context.Background(), input); err != nil {
		t.Fatal(err)
	}
	if restarted.snapshot.Calls != 2 || restarted.snapshot.Effects != 1 {
		t.Fatal("duplicate call not distinguished from a second effect")
	}
}
