// SPDX-License-Identifier: Apache-2.0

package azure

import (
	"context"
	"strings"

	"github.com/kubeneuron/kubeneuron/internal/cloud/removefirst"
	"github.com/kubeneuron/kubeneuron/pkg/cloud"
)

// ObserveRemoveFirstDelete uses the pool and machine saved before deletion.
// A listed ARM path can be reused. The platform must independently check the
// original Kubernetes Node UID and providerID before permitting another delete.
func (p *Provider) ObserveRemoveFirstDelete(ctx context.Context, instanceID, poolName, machineName string) (removefirst.Observation, error) {
	if poolName == "" || machineName == "" {
		return removefirst.Unknown, nil
	}
	target, members, err := p.resolve(ctx, instanceID)
	if err != nil {
		return removefirst.Unknown, err
	}
	if !strings.EqualFold(target.pool, poolName) {
		return removefirst.IdentityChanged, nil
	}
	result := removefirst.Deleted
	for _, member := range members {
		name := strings.EqualFold(member.Name, machineName)
		path := strings.EqualFold(member.ResourceID, instanceID)
		if name != path {
			return removefirst.IdentityChanged, nil
		}
		if name {
			result = removefirst.Listed
		}
	}
	return result, nil
}

// DeleteListedMachine rechecks membership immediately before sending the
// name-based delete. Its caller must first check the live Node identity.
func (p *Provider) DeleteListedMachine(ctx context.Context, instanceID, poolName, machineName string) error {
	status, err := p.ObserveRemoveFirstDelete(ctx, instanceID, poolName, machineName)
	if err != nil {
		return err
	}
	if status != removefirst.Listed {
		return cloud.ErrIdentityChanged
	}
	return p.api.DeleteMachines(ctx, poolName, []string{machineName})
}
