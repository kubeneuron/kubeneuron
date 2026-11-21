// SPDX-License-Identifier: Apache-2.0

package aws

import (
	"context"
	"errors"
	"fmt"

	sdk "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/smithy-go"

	"github.com/kubeneuron/kubeneuron/pkg/cloud"
)

// CheckReplacePermission asks EC2 to authorize the exact request used by
// Replace. A successful dry run is reported as an AWS API error.
func (r *Recycler) CheckReplacePermission(ctx context.Context, id string) error {
	if id == "" {
		return errors.New("missing pinned instance ID")
	}
	_, err := r.api.TerminateInstances(ctx, &ec2.TerminateInstancesInput{InstanceIds: []string{id}, DryRun: sdk.Bool(true)})
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) {
		switch apiErr.ErrorCode() {
		case "DryRunOperation":
			return nil
		case "UnauthorizedOperation":
			// EC2's message can carry an encoded authorization failure with
			// the role's ARN: it stays in Cause, for the controller log.
			return &cloud.RecoveryHold{Code: cloud.HoldPermissionMissing, Cause: fmt.Errorf("ec2:TerminateInstances denied for %s: %w", id, err),
				Reason:     "The controller's IAM role may not call ec2:TerminateInstances on the node's instance.",
				Action:     "Allow ec2:TerminateInstances in the controller's IAM role (see docs/cloud-aws.md). KubeNeuron checks again every minute.",
				PoolReason: "the controller lacks ec2:TerminateInstances",
				PoolAction: "allow ec2:TerminateInstances in the controller's IAM role"}
		}
	}
	if err != nil {
		answer := "the call failed"
		if apiErr != nil {
			answer = "EC2 answered " + apiErr.ErrorCode()
		}
		return &cloud.RecoveryHold{Code: cloud.HoldReadFailed, Cause: fmt.Errorf("EC2 TerminateInstances DryRun failed for %s: %w", id, err),
			Reason:     "KubeNeuron could not check ec2:TerminateInstances with a dry run: " + answer + ".",
			Action:     "KubeNeuron checks again every minute.",
			PoolReason: "could not check permissions: " + answer}
	}
	return errors.New("EC2 TerminateInstances DryRun returned no authorization result")
}
