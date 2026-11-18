// SPDX-License-Identifier: Apache-2.0

package aws

import (
	"context"
	"errors"
	"testing"

	sdk "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/autoscaling"
	at "github.com/aws/aws-sdk-go-v2/service/autoscaling/types"
	"github.com/aws/smithy-go"
	"github.com/kubeneuron/kubeneuron/pkg/cloud"
)

type terminateFixture struct {
	t          *testing.T
	calls      int
	expectedID string
	out        *autoscaling.TerminateInstanceInAutoScalingGroupOutput
	err        error
}

func (f *terminateFixture) TerminateInstanceInAutoScalingGroup(_ context.Context, in *autoscaling.TerminateInstanceInAutoScalingGroupInput, options ...func(*autoscaling.Options)) (*autoscaling.TerminateInstanceInAutoScalingGroupOutput, error) {
	f.calls++
	var cfg autoscaling.Options
	for _, apply := range options {
		apply(&cfg)
	}
	if sdk.ToString(in.InstanceId) != f.expectedID || !sdk.ToBool(in.ShouldDecrementDesiredCapacity) || cfg.Retryer == nil || cfg.Retryer.MaxAttempts() != 1 {
		f.t.Fatalf("unexpected termination request or retry policy")
	}
	return f.out, f.err
}
func TestTerminatePinned(t *testing.T) {
	for _, tc := range []struct {
		name string
		out  *autoscaling.TerminateInstanceInAutoScalingGroupOutput
		err  error
		ok   bool
	}{
		{"accepted", &autoscaling.TerminateInstanceInAutoScalingGroupOutput{Activity: &at.Activity{ActivityId: sdk.String("act")}}, nil, true},
		{"no activity", nil, nil, false}, {"lost response", nil, errors.New("connection closed"), false}, {"denied", nil, &smithy.GenericAPIError{Code: "AccessDenied"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := &terminateFixture{t: t, expectedID: "i-old", out: tc.out, err: tc.err}
			err := terminatePinned(t.Context(), client, "i-old")
			if client.calls != 1 || tc.ok && err != nil || !tc.ok && !errors.Is(err, cloud.ErrNeedsOperator) {
				t.Fatalf("calls=%d err=%v", client.calls, err)
			}
		})
	}
}
