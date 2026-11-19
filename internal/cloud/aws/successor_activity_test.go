// SPDX-License-Identifier: Apache-2.0

package aws

import (
	"context"
	"errors"
	"github.com/aws/aws-sdk-go-v2/service/autoscaling"
	"strconv"
	"strings"
	"testing"
	"time"

	sdk "github.com/aws/aws-sdk-go-v2/aws"
	at "github.com/aws/aws-sdk-go-v2/service/autoscaling/types"
	"github.com/kubeneuron/kubeneuron/pkg/cloud"
)

func TestConfirmSuccessorLaunch(t *testing.T) {
	now := time.Now()
	s := cloud.Successor{RequestedAt: now, State: map[string]string{"before": "2", "asg_arn": "arn"}}
	launch := at.Activity{StartTime: &now, AutoScalingGroupARN: sdk.String("arn"), StatusCode: at.ScalingActivityStatusCodeSuccessful, Description: sdk.String("Launching a new EC2 instance: i-new"), Cause: sdk.String("changing the desired capacity from 2 to 3.")}
	stale := launch
	stale.StartTime = sdk.Time(now.Add(-time.Minute - time.Nanosecond))
	wrong := launch
	wrong.Cause = sdk.String("changing the desired capacity from 2 to 30.")
	for _, tc := range []struct {
		name       string
		activities []at.Activity
		ok         bool
	}{
		{"matching launch", []at.Activity{launch, stale}, true}, {"outside window", []at.Activity{stale}, false}, {"wrong transition", []at.Activity{wrong}, false}, {"competing activity", []at.Activity{launch, wrong}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := confirmSuccessorLaunch(s, "i-new", tc.activities)
			if tc.ok && err != nil || !tc.ok && !errors.Is(err, cloud.ErrNeedsOperator) {
				t.Fatal(err)
			}
		})
	}
}

type launchActivityPages map[string]*autoscaling.DescribeScalingActivitiesOutput

func (p launchActivityPages) DescribeScalingActivities(_ context.Context, in *autoscaling.DescribeScalingActivitiesInput, _ ...func(*autoscaling.Options)) (*autoscaling.DescribeScalingActivitiesOutput, error) {
	if sdk.ToString(in.AutoScalingGroupName) != "workers" {
		return nil, errors.New("unexpected group")
	}
	return p[sdk.ToString(in.NextToken)], nil
}

func TestCheckSuccessorActivityPages(t *testing.T) {
	now := time.Now()
	s := cloud.Successor{Pool: "workers", RequestedAt: now, State: map[string]string{"before": "1", "asg_arn": "arn"}}
	launch := at.Activity{StartTime: &now, AutoScalingGroupName: &s.Pool, AutoScalingGroupARN: sdk.String("arn"), StatusCode: at.ScalingActivityStatusCodeSuccessful, Description: sdk.String("Launching a new EC2 instance: i-new"), Cause: sdk.String("changing the desired capacity from 1 to 2.")}
	pages := launchActivityPages{"": {Activities: []at.Activity{launch}, NextToken: sdk.String("p2")}, "p2": {}}
	if err := checkSuccessorActivities(t.Context(), pages, s, "i-new"); err != nil {
		t.Fatal(err)
	}
	pages["p2"].Activities = []at.Activity{launch}
	if err := checkSuccessorActivities(t.Context(), pages, s, "i-new"); !errors.Is(err, cloud.ErrNeedsOperator) {
		t.Fatalf("competing launch: %v", err)
	}
	pages["p2"] = &autoscaling.DescribeScalingActivitiesOutput{NextToken: sdk.String("p2")}
	if err := checkSuccessorActivities(t.Context(), pages, s, "i-new"); !errors.Is(err, cloud.ErrNeedsOperator) {
		t.Fatalf("repeated token: %v", err)
	}

	token := ""
	for n := 1; n <= 20; n++ {
		next := strconv.Itoa(n)
		pages[token] = &autoscaling.DescribeScalingActivitiesOutput{NextToken: &next}
		token = next
	}
	pages[token] = &autoscaling.DescribeScalingActivitiesOutput{Activities: []at.Activity{launch}}
	if err := checkSuccessorActivities(t.Context(), pages, s, "i-new"); !errors.Is(err, cloud.ErrNeedsOperator) || !strings.Contains(err.Error(), "activity history too long") {
		t.Fatalf("21 pages: %v", err)
	}
}
