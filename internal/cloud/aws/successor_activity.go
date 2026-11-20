// SPDX-License-Identifier: Apache-2.0

package aws

import (
	"context"
	"fmt"
	"github.com/aws/aws-sdk-go-v2/service/autoscaling"
	"regexp"
	"strconv"
	"strings"
	"time"

	sdk "github.com/aws/aws-sdk-go-v2/aws"
	at "github.com/aws/aws-sdk-go-v2/service/autoscaling/types"
	"github.com/kubeneuron/kubeneuron/pkg/cloud"
)

const successorClockSkew = time.Minute

// successorLaunchGrace is how long a raised desired capacity may go without
// an ASG launch activity. The ASG starts the launch some seconds after EKS
// reports the size update done (14 s in a live EKS run); only a longer
// silence leaves the request to the operator.
const successorLaunchGrace = 5 * time.Minute

var launchCapacityFailure = regexp.MustCompile(`\b(InsufficientInstanceCapacity|InstanceLimitExceeded|VcpuLimitExceeded)\b`)

// AWS shows free-form Cause and Description examples at
// https://docs.aws.amazon.com/autoscaling/ec2/APIReference/API_DescribeScalingActivities.html .
// The exact wording matched here is a fixture assumption pending live EKS validation.
var desiredTransition = regexp.MustCompile(`\bdesired capacity from ([0-9]+) to ([0-9]+)\b`)

// confirmSuccessorLaunch expects the complete activity window for the pinned ASG.
// Unknown cause text or competing activity leaves attribution to the operator.
// With no candidate instance yet, an empty window within successorLaunchGrace
// of the request is a launch the ASG has not started: nil, read again later.
func confirmSuccessorLaunch(s cloud.Successor, candidate string, activities []at.Activity, now time.Time) error {
	before, err := strconv.ParseInt(s.State["before"], 10, 32)
	if err != nil || before < 1 || s.RequestedAt.IsZero() || s.State["asg_arn"] == "" {
		return fmt.Errorf("%w: incomplete launch evidence", cloud.ErrNeedsOperator)
	}
	cutoff := s.RequestedAt.Add(-successorClockSkew)
	matched := false
	capacityFailure := false
	for _, a := range activities {
		if a.StartTime == nil || a.StartTime.IsZero() {
			return fmt.Errorf("%w: undated ASG activity", cloud.ErrNeedsOperator)
		}
		if a.StartTime.Before(cutoff) {
			continue
		}
		transitions := desiredTransition.FindAllStringSubmatch(sdk.ToString(a.Cause), -1)
		if matched || sdk.ToString(a.AutoScalingGroupARN) != s.State["asg_arn"] || len(transitions) != 1 || transitions[0][1] != strconv.FormatInt(before, 10) || transitions[0][2] != strconv.FormatInt(before+1, 10) {
			return fmt.Errorf("%w: competing or unconfirmed ASG launch activity", cloud.ErrNeedsOperator)
		}
		if candidate == "" {
			if !launchDescription(sdk.ToString(a.Description)) {
				return fmt.Errorf("%w: ASG activity is not a launch", cloud.ErrNeedsOperator)
			}
			// the instance is not an InService member yet: a launch under way, or done
			// before the group lists the instance InService, is read again later
			if launchUnderWay(a.StatusCode) {
				matched = true
				continue
			}
			capacityFailure = a.StatusCode == at.ScalingActivityStatusCodeFailed && launchCapacityFailure.MatchString(sdk.ToString(a.StatusMessage))
			if !capacityFailure {
				return fmt.Errorf("%w: unconfirmed ASG capacity failure", cloud.ErrNeedsOperator)
			}
		} else if a.StatusCode != at.ScalingActivityStatusCodeSuccessful || sdk.ToString(a.Description) != "Launching a new EC2 instance: "+candidate {
			return fmt.Errorf("%w: ASG activity does not confirm candidate", cloud.ErrNeedsOperator)
		}
		matched = true
	}
	if capacityFailure {
		return fmt.Errorf("%w: ASG failed to launch the additional instance", cloud.ErrReplacementCapacity)
	}
	if !matched {
		if candidate == "" && now.Sub(s.RequestedAt) < successorLaunchGrace {
			return nil
		}
		return fmt.Errorf("%w: no matching ASG launch activity", cloud.ErrNeedsOperator)
	}
	return nil
}

// launchDescription is an ASG launch activity's Description: AWS names the
// instance once it has one ("Launching a new EC2 instance: i-...").
func launchDescription(d string) bool {
	return d == "Launching a new EC2 instance" || strings.HasPrefix(d, "Launching a new EC2 instance: i-")
}

// launchUnderWay is a launch activity that started an instance or is still
// starting one.
func launchUnderWay(code at.ScalingActivityStatusCode) bool {
	switch code {
	case at.ScalingActivityStatusCodeWaitingForInstanceId, at.ScalingActivityStatusCodePreInService,
		at.ScalingActivityStatusCodeInProgress, at.ScalingActivityStatusCodeMidLifecycleAction,
		at.ScalingActivityStatusCodeWaitingForInstanceWarmup, at.ScalingActivityStatusCodeSuccessful:
		return true
	}
	return false
}

// checkSuccessorActivities reads newest-first ASG history until it covers the
// operation window. A truncated history cannot establish a unique launch.
func checkSuccessorActivities(ctx context.Context, client activityReader, s cloud.Successor, candidate string) error {
	if s.Pool == "" || s.RequestedAt.IsZero() {
		return fmt.Errorf("%w: missing ASG activity scope", cloud.ErrNeedsOperator)
	}
	in := &autoscaling.DescribeScalingActivitiesInput{AutoScalingGroupName: &s.Pool, MaxRecords: sdk.Int32(100)}
	seen := map[string]bool{}
	cutoff := s.RequestedAt.Add(-successorClockSkew)
	var recent []at.Activity
	for range 20 {
		page, err := client.DescribeScalingActivities(ctx, in)
		if err != nil {
			return err
		}
		if page == nil {
			return fmt.Errorf("%w: missing ASG activity page", cloud.ErrNeedsOperator)
		}
		olderPage := len(page.Activities) > 0
		for _, a := range page.Activities {
			if a.StartTime == nil || a.StartTime.IsZero() || sdk.ToString(a.AutoScalingGroupName) != s.Pool {
				return fmt.Errorf("%w: incomplete ASG activity scope or time", cloud.ErrNeedsOperator)
			}
			if !a.StartTime.Before(cutoff) {
				olderPage = false
				recent = append(recent, a)
			}
		}
		token := sdk.ToString(page.NextToken)
		if token == "" || olderPage {
			return confirmSuccessorLaunch(s, candidate, recent, time.Now())
		}
		if seen[token] {
			return fmt.Errorf("%w: repeated ASG activity page token", cloud.ErrNeedsOperator)
		}
		seen[token] = true
		in.NextToken = page.NextToken
	}
	return fmt.Errorf("%w: activity history too long", cloud.ErrNeedsOperator)
}
