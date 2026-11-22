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

// successorCandidate compares a settled ASG to the saved baseline. The caller
// must independently establish attribution before adopting the returned instance.
func successorCandidate(s cloud.Successor, group at.AutoScalingGroup) (string, error) {
	zones := plannedZones(s)
	before, err := strconv.ParseInt(s.State["before"], 10, 32)
	if err != nil || before < 1 || group.DesiredCapacity == nil || int64(*group.DesiredCapacity) != before+1 || s.Pool == "" || sdk.ToString(group.AutoScalingGroupName) != s.Pool || s.State["asg_arn"] == "" || sdk.ToString(group.AutoScalingGroupARN) != s.State["asg_arn"] || !samePoolZones(zones, group.AvailabilityZones) {
		return "", fmt.Errorf("%w: scaled ASG identity, zone or desired capacity differs from the plan", cloud.ErrNeedsOperator)
	}
	old := map[string]bool{}
	for _, id := range strings.Split(s.State["members"], ",") {
		if id == "" || old[id] {
			return "", fmt.Errorf("%w: invalid saved ASG membership", cloud.ErrNeedsOperator)
		}
		old[id] = true
	}
	if !old[s.Instance] || int64(len(old)) != before || int64(len(group.Instances)) != before+1 {
		return "", fmt.Errorf("%w: incomplete ASG membership", cloud.ErrNeedsOperator)
	}
	var candidate string
	for _, member := range group.Instances {
		id := sdk.ToString(member.InstanceId)
		if id == "" || member.LifecycleState != at.LifecycleStateInService || !slices.Contains(zones, sdk.ToString(member.AvailabilityZone)) {
			return "", fmt.Errorf("%w: ASG membership is not settled", cloud.ErrNeedsOperator)
		}
		if _, exists := old[id]; exists {
			if !old[id] {
				return "", fmt.Errorf("%w: duplicate ASG member", cloud.ErrNeedsOperator)
			}
			old[id] = false
			continue
		}
		if candidate != "" {
			return "", fmt.Errorf("%w: multiple new ASG members", cloud.ErrNeedsOperator)
		}
		candidate = id
	}
	if candidate == "" {
		return "", fmt.Errorf("%w: missing new ASG member", cloud.ErrNeedsOperator)
	}
	return candidate, nil
}

func plannedZones(s cloud.Successor) []string {
	if saved := s.State["pool_zones"]; saved != "" {
		return strings.Split(saved, ",")
	}
	return []string{s.State["zone"]}
}

func samePoolZones(saved, current []string) bool {
	zones := slices.Clone(current)
	slices.Sort(zones)
	return !slices.Contains(saved, "") && slices.Equal(saved, zones)
}
