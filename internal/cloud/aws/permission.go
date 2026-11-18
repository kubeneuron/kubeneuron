// SPDX-License-Identifier: Apache-2.0

package aws

import (
	"context"
	"errors"

	sdk "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/smithy-go"
)

// checkReplacePermission asks EC2 to authorize the exact request used by
// Replace. A successful dry run is reported as an AWS API error.
func (r *Recycler) checkReplacePermission(ctx context.Context, id string) error {
	if id == "" {
		return errors.New("missing pinned instance ID")
	}
	_, err := r.api.TerminateInstances(ctx, &ec2.TerminateInstancesInput{InstanceIds: []string{id}, DryRun: sdk.Bool(true)})
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) && apiErr.ErrorCode() == "DryRunOperation" {
		return nil
	}
	return errors.New("EC2 terminate permission unconfirmed")
}
