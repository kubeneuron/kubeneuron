// SPDX-License-Identifier: Apache-2.0

package aws

import (
	"context"

	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"

	"github.com/kubeneuron/kubeneuron/internal/cloud/removefirst"
)

// ObserveRemoveFirstDelete reads the EC2 instance pinned before termination.
// An empty or incomplete EC2 read cannot establish that the instance died.
func (r *Recycler) ObserveRemoveFirstDelete(ctx context.Context, instanceID string) (removefirst.Observation, error) {
	state, found, err := r.instanceState(ctx, instanceID)
	if err != nil || !found {
		return removefirst.Unknown, err
	}
	switch state {
	case ec2types.InstanceStateNameTerminated:
		return removefirst.Deleted, nil
	case ec2types.InstanceStateNameShuttingDown:
		return removefirst.Deleting, nil
	case ec2types.InstanceStateNameRunning, ec2types.InstanceStateNameStopped, ec2types.InstanceStateNameStopping, ec2types.InstanceStateNamePending:
		return removefirst.SameMachine, nil
	default:
		return removefirst.Unknown, nil
	}
}
