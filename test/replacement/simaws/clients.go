// SPDX-License-Identifier: Apache-2.0

package simaws

import (
	"fmt"
	"github.com/aws/aws-sdk-go-v2/aws"
	at "github.com/aws/aws-sdk-go-v2/service/autoscaling/types"
	ct "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	et "github.com/aws/aws-sdk-go-v2/service/eks/types"
	"log"
	"time"
)

func NewClients(cluster string) (*Instances, *Scaling, *EKS) {
	return newClients(cluster, []string{"az"}, "az")
}

// NewTwoZoneClients places the original in zone A and launches its successor in zone B.
func NewTwoZoneClients(cluster string) (*Instances, *Scaling, *EKS) {
	ec, scaling, eks := newClients(cluster, []string{"us-east-1a", "us-east-1b"}, "us-east-1b")
	log.Print("zone rehearsal: simulator ready")
	scaling.traceMutations = true
	eks.traceMutations = true
	return ec, scaling, eks
}

func newClients(cluster string, zones []string, launchZone string) (*Instances, *Scaling, *EKS) {
	group := aws.String("workers")
	zone := aws.String(zones[0])
	old := at.Instance{InstanceId: aws.String("i-pinned"), AvailabilityZone: zone, LifecycleState: at.LifecycleStateInService, HealthStatus: aws.String("Healthy")}
	ec := &Instances{Group: *group, States: map[string]ct.InstanceStateName{"i-pinned": ct.InstanceStateNameRunning}}
	scaling := &Scaling{group: at.AutoScalingGroup{AutoScalingGroupName: group, AutoScalingGroupARN: aws.String("asg-1"), AvailabilityZones: zones, DesiredCapacity: aws.Int32(1), MaxSize: aws.Int32(2), Instances: []at.Instance{old}, Tags: []at.TagDescription{{Key: aws.String("eks:nodegroup-name"), Value: aws.String("gpu")}}}}
	ec.scaling = scaling
	eks := &EKS{scaling: scaling, Group: et.Nodegroup{ClusterName: aws.String(cluster), NodegroupName: aws.String("gpu"), NodegroupArn: aws.String("ng"), Status: et.NodegroupStatusActive, Tags: map[string]string{"kubeneuron.io/fixed-size": "true"}, Resources: &et.NodegroupResources{AutoScalingGroups: []et.AutoScalingGroup{{Name: group}}}}}
	eks.Grow = func(size int32, now time.Time) error {
		scaling.mu.Lock()
		defer scaling.mu.Unlock()
		if size != 2 || len(scaling.group.Instances) != 1 {
			return fmt.Errorf("unexpected simulated target size %d", size)
		}
		scaling.group.DesiredCapacity = aws.Int32(size)
		return nil
	}
	eks.launch = func(now time.Time) {
		scaling.mu.Lock()
		defer scaling.mu.Unlock()
		ec.mu.Lock()
		defer ec.mu.Unlock()
		member := old
		member.InstanceId = aws.String("i-new")
		member.AvailabilityZone = aws.String(launchZone)
		ec.States[*member.InstanceId] = ct.InstanceStateNameRunning
		scaling.group.Instances = append(scaling.group.Instances, member)
		activity := at.Activity{ActivityId: aws.String("launch-i-new"), StartTime: &now, EndTime: &now, AutoScalingGroupName: group, AutoScalingGroupARN: scaling.group.AutoScalingGroupARN, Description: aws.String("Launching a new EC2 instance: i-new"), Cause: aws.String("changing the desired capacity from 1 to 2"), StatusCode: at.ScalingActivityStatusCodeSuccessful}
		scaling.activities = append([]at.Activity{activity}, scaling.activities...)
	}
	return ec, scaling, eks
}
