// SPDX-License-Identifier: Apache-2.0

package aws

import (
	"fmt"
	"slices"
	"strconv"
	"strings"

	sdk "github.com/aws/aws-sdk-go-v2/aws"
	at "github.com/aws/aws-sdk-go-v2/service/autoscaling/types"
	"github.com/kubeneuron/kubeneuron/pkg/cloud"
)

// planSuccessor records the baseline from an already verified EKS backing group.
func planSuccessor(s cloud.Successor, group at.AutoScalingGroup) (cloud.Successor, error) {
	if len(group.AvailabilityZones) != 1 || group.AvailabilityZones[0] == "" {
		return s, fmt.Errorf("%w: add-first requires a single availability zone", cloud.ErrAddFirstUnavailable)
	}
	if group.DesiredCapacity == nil || group.MaxSize == nil || *group.DesiredCapacity < 1 || *group.DesiredCapacity >= *group.MaxSize {
		return s, fmt.Errorf("%w: group needs spare capacity below maxSize", cloud.ErrAddFirstUnavailable)
	}
	ids := make([]string, 0, len(group.Instances))
	for _, instance := range group.Instances {
		id := sdk.ToString(instance.InstanceId)
		if id == "" || instance.LifecycleState != at.LifecycleStateInService || sdk.ToString(instance.AvailabilityZone) != group.AvailabilityZones[0] {
			return s, fmt.Errorf("%w: group membership is incomplete or changing", cloud.ErrAddFirstUnavailable)
		}
		ids = append(ids, id)
	}
	slices.Sort(ids)
	if len(slices.Compact(ids)) != int(*group.DesiredCapacity) || len(ids) != int(*group.DesiredCapacity) || !slices.Contains(ids, s.Instance) || sdk.ToString(group.AutoScalingGroupName) == "" || s.Operation == "" {
		return s, fmt.Errorf("%w: group baseline does not confirm the pinned instance and desired membership", cloud.ErrAddFirstUnavailable)
	}
	s.Pool = sdk.ToString(group.AutoScalingGroupName)
	s.State = map[string]string{"before": strconv.Itoa(len(ids)), "members": strings.Join(ids, ","), "zone": group.AvailabilityZones[0]}
	s.Phase = cloud.SuccessorPlanned
	return s, nil
}
