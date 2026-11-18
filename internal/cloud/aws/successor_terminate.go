// SPDX-License-Identifier: Apache-2.0

package aws

import (
	"context"
	"errors"
	"fmt"

	sdk "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/autoscaling"
	"github.com/aws/smithy-go"
	"github.com/kubeneuron/kubeneuron/pkg/cloud"
)

type instanceTerminator interface {
	TerminateInstanceInAutoScalingGroup(context.Context, *autoscaling.TerminateInstanceInAutoScalingGroupInput, ...func(*autoscaling.Options)) (*autoscaling.TerminateInstanceInAutoScalingGroupOutput, error)
}

// terminatePinned sends a single decrement for an instance already verified by
// the caller. The API has no client token to replay an ambiguous response.
func terminatePinned(ctx context.Context, client instanceTerminator, instance string) error {
	if instance == "" {
		return fmt.Errorf("%w: missing pinned EC2 instance", cloud.ErrNeedsOperator)
	}
	out, err := client.TerminateInstanceInAutoScalingGroup(ctx, &autoscaling.TerminateInstanceInAutoScalingGroupInput{InstanceId: &instance, ShouldDecrementDesiredCapacity: sdk.Bool(true)}, func(o *autoscaling.Options) { o.Retryer = sdk.NopRetryer{} })
	if err != nil {
		var api smithy.APIError
		if errors.As(err, &api) && (api.ErrorCode() == "AccessDenied" || api.ErrorCode() == "AccessDeniedException") {
			return fmt.Errorf("%w: autoscaling:TerminateInstanceInAutoScalingGroup denied; group may retain one extra instance: %w", cloud.ErrNeedsOperator, err)
		}
		return fmt.Errorf("%w: termination of %s has an unconfirmed outcome: %w", cloud.ErrNeedsOperator, instance, err)
	}
	if out == nil || out.Activity == nil || sdk.ToString(out.Activity.ActivityId) == "" {
		return fmt.Errorf("%w: missing activity ID for termination of %s", cloud.ErrNeedsOperator, instance)
	}
	return nil
}

// ReplaceWithSuccessor revalidates the persisted successor before removing the
// old instance. The caller has completed its workload handoff before this call.
func (p *managedProvider) ReplaceWithSuccessor(ctx context.Context, s cloud.Successor) error {
	if err := p.confirmCreatedSuccessor(ctx, s); err != nil {
		return err
	}
	return terminatePinned(ctx, p.terminator, s.Instance)
}

// releaseCreatedSuccessor removes the extra instance when the handoff is abandoned.
func (p *managedProvider) releaseCreatedSuccessor(ctx context.Context, s cloud.Successor) error {
	if err := p.confirmCreatedSuccessor(ctx, s); err != nil {
		return err
	}
	id, err := p.InstanceID(s.ProviderID)
	if err != nil || id == s.Instance {
		return fmt.Errorf("%w: invalid successor identity for release", cloud.ErrNeedsOperator)
	}
	return terminatePinned(ctx, p.terminator, id)
}

func (p *managedProvider) confirmCreatedSuccessor(ctx context.Context, s cloud.Successor) error {
	if s.Phase != cloud.SuccessorCreated || s.Instance == "" || s.ProviderID == "" {
		return fmt.Errorf("%w: replacement requires a pinned instance and a created successor", cloud.ErrNeedsOperator)
	}
	requested := s
	requested.Phase = cloud.SuccessorRequested
	requested.ProviderID = ""
	current, err := p.observeSuccessor(ctx, requested)
	if err != nil {
		return err
	}
	if current.Phase != cloud.SuccessorCreated || current.ProviderID != s.ProviderID {
		return fmt.Errorf("%w: current ASG launch does not confirm the persisted successor", cloud.ErrNeedsOperator)
	}
	return nil
}
