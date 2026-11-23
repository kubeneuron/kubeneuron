// SPDX-License-Identifier: Apache-2.0

// Package simaws implements local AWS clients for the kind replacement rehearsal.
package simaws

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"

	"github.com/aws/aws-sdk-go-v2/aws"
	at "github.com/aws/aws-sdk-go-v2/service/autoscaling/types"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	et "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/smithy-go"
)

// Instances holds the EC2 view of one simulated Auto Scaling group.
type Instances struct {
	mu      sync.Mutex
	Group   string
	States  map[string]et.InstanceStateName
	scaling *Scaling
}

func (c *Instances) instance(ids []string) (et.Instance, error) {
	if len(ids) != 1 {
		return et.Instance{}, errors.New("rehearsal expects exactly one pinned instance")
	}
	state, ok := c.States[ids[0]]
	if !ok {
		return et.Instance{}, fmt.Errorf("unknown simulated instance %q", ids[0])
	}
	return et.Instance{InstanceId: aws.String(ids[0]), State: &et.InstanceState{Name: state},
		Tags: []et.Tag{{Key: aws.String("aws:autoscaling:groupName"), Value: aws.String(c.Group)}}}, nil
}

func (c *Instances) DescribeInstances(_ context.Context, in *ec2.DescribeInstancesInput, _ ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	i, err := c.instance(in.InstanceIds)
	if err != nil {
		return nil, err
	}
	return &ec2.DescribeInstancesOutput{Reservations: []et.Reservation{{Instances: []et.Instance{i}}}}, nil
}

func (c *Instances) DescribeInstanceStatus(_ context.Context, in *ec2.DescribeInstanceStatusInput, _ ...func(*ec2.Options)) (*ec2.DescribeInstanceStatusOutput, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	i, err := c.instance(in.InstanceIds)
	if err != nil {
		return nil, err
	}
	return &ec2.DescribeInstanceStatusOutput{InstanceStatuses: []et.InstanceStatus{{InstanceId: i.InstanceId, InstanceState: i.State}}}, nil
}

func (c *Instances) TerminateInstances(_ context.Context, in *ec2.TerminateInstancesInput, _ ...func(*ec2.Options)) (*ec2.TerminateInstancesOutput, error) {
	c.scaling.mu.Lock()
	defer c.scaling.mu.Unlock()
	c.mu.Lock()
	defer c.mu.Unlock()
	i, err := c.instance(in.InstanceIds)
	if err != nil {
		return nil, err
	}
	if aws.ToBool(in.DryRun) {
		return nil, &smithy.GenericAPIError{Code: "DryRunOperation", Message: "simulated permission granted"}
	}
	if i.State.Name != et.InstanceStateNameTerminated {
		// With unchanged desired capacity the group supplies the final worker.
		members := c.scaling.group.Instances
		idx := slices.IndexFunc(members, func(m at.Instance) bool { return aws.ToString(m.InstanceId) == *i.InstanceId })
		if idx < 0 || *i.InstanceId != "i-new" {
			return nil, fmt.Errorf("unexpected remove-first target %s", *i.InstanceId)
		}
		members[idx].InstanceId = aws.String("i-final")
		c.States["i-final"] = et.InstanceStateNameRunning
		c.States[*i.InstanceId] = et.InstanceStateNameTerminated
	}
	return &ec2.TerminateInstancesOutput{TerminatingInstances: []et.InstanceStateChange{{InstanceId: i.InstanceId, PreviousState: i.State, CurrentState: &et.InstanceState{Name: et.InstanceStateNameTerminated}}}}, nil
}

func (*Instances) StartInstances(context.Context, *ec2.StartInstancesInput, ...func(*ec2.Options)) (*ec2.StartInstancesOutput, error) {
	return nil, errors.New("unexpected StartInstances during node replacement")
}

func (*Instances) StopInstances(context.Context, *ec2.StopInstancesInput, ...func(*ec2.Options)) (*ec2.StopInstancesOutput, error) {
	return nil, errors.New("unexpected StopInstances during node replacement")
}
