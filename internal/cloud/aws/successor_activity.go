// SPDX-License-Identifier: Apache-2.0

package aws

import (
	"context"
	"fmt"
	"github.com/aws/aws-sdk-go-v2/service/autoscaling"
	"regexp"
	"strconv"
	"time"

	sdk "github.com/aws/aws-sdk-go-v2/aws"
	at "github.com/aws/aws-sdk-go-v2/service/autoscaling/types"
	"github.com/kubeneuron/kubeneuron/pkg/cloud"
)

var desiredTransition = regexp.MustCompile(`\bdesired capacity from ([0-9]+) to ([0-9]+)\b`)

// confirmSuccessorLaunch expects the complete activity window for the pinned ASG.
// Unknown cause text or competing activity leaves attribution to the operator.
func confirmSuccessorLaunch(s cloud.Successor, candidate string, activities []at.Activity) error {
	before, err := strconv.ParseInt(s.State["before"], 10, 32)
	if err != nil || before < 1 || s.RequestedAt.IsZero() || candidate == "" || s.State["asg_arn"] == "" {
		return fmt.Errorf("%w: incomplete launch evidence", cloud.ErrNeedsOperator)
	}
	cutoff := s.RequestedAt.Add(-time.Minute)
	matched := false
	for _, a := range activities {
		if a.StartTime == nil || a.StartTime.IsZero() {
			return fmt.Errorf("%w: undated ASG activity", cloud.ErrNeedsOperator)
		}
		if a.StartTime.Before(cutoff) {
			continue
		}
		transitions := desiredTransition.FindAllStringSubmatch(sdk.ToString(a.Cause), -1)
		if matched || sdk.ToString(a.AutoScalingGroupARN) != s.State["asg_arn"] || a.StatusCode != at.ScalingActivityStatusCodeSuccessful || sdk.ToString(a.Description) != "Launching a new EC2 instance: "+candidate || len(transitions) != 1 || transitions[0][1] != strconv.FormatInt(before, 10) || transitions[0][2] != strconv.FormatInt(before+1, 10) {
			return fmt.Errorf("%w: competing or unconfirmed ASG launch activity", cloud.ErrNeedsOperator)
		}
		matched = true
	}
	if !matched {
		return fmt.Errorf("%w: no matching ASG launch activity", cloud.ErrNeedsOperator)
	}
	return nil
}

// checkSuccessorActivities reads newest-first ASG history until it covers the
// operation window. A truncated history cannot establish a unique launch.
func checkSuccessorActivities(ctx context.Context, client activityReader, s cloud.Successor, candidate string) error {
	if s.Pool == "" || s.RequestedAt.IsZero() {
		return fmt.Errorf("%w: missing ASG activity scope", cloud.ErrNeedsOperator)
	}
	in := &autoscaling.DescribeScalingActivitiesInput{AutoScalingGroupName: &s.Pool, MaxRecords: sdk.Int32(100)}
	seen := map[string]bool{}
	cutoff := s.RequestedAt.Add(-time.Minute)
	var recent []at.Activity
	for {
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
			return confirmSuccessorLaunch(s, candidate, recent)
		}
		if seen[token] {
			return fmt.Errorf("%w: repeated ASG activity page token", cloud.ErrNeedsOperator)
		}
		seen[token] = true
		in.NextToken = page.NextToken
	}
}
