// SPDX-License-Identifier: Apache-2.0

package aws

import (
	"context"
	"fmt"
	"strings"

	"github.com/kubeneuron/kubeneuron/pkg/cloud"
)

// providerName is this provider's registry key and matches spec.cloud.provider.
const providerName = "aws"

// capabilities declares what EC2 can do: stop/start reinitializes the instance
// in place (RecycleNode) and terminate hands it to the autoscaler (ReplaceNode).
// The operator uses these provider-wide capabilities to compile playbooks.
// The runtime managedProvider narrows them to replacement for EKS managed nodes.
var capabilities = cloud.Capabilities{ReinitializeInPlace: true, Replace: true}

// Recycler is the AWS cloud.Provider.
var _ cloud.Provider = (*Recycler)(nil)

type managedProvider struct {
	*Recycler
	*repairReader
	// queue is the interruption queue, when one is configured.
	queue *interruptionQueue
}

var _ cloud.RepairAwareProvider = (*managedProvider)(nil)

// An EKS managed group can replace a stopped instance during a recycle.
func (*managedProvider) Capabilities() cloud.Capabilities { return cloud.Capabilities{Replace: true} }

func init() {
	cloud.Register(providerName, capabilities, func(ctx context.Context, cfg cloud.Config) (cloud.Provider, error) {
		r, err := newManagedProvider(ctx, cfg)
		if err != nil {
			return nil, err
		}
		if r.queue != nil {
			return &queueProvider{managedProvider: r, interruptionQueue: r.queue}, nil
		}
		return r, nil
	})
}

// Name identifies this provider in the registry and the CR.
func (r *Recycler) Name() string { return providerName }

// Capabilities reports that AWS supports both recycle (stop/start) and replace
// (terminate).
func (r *Recycler) Capabilities() cloud.Capabilities { return capabilities }

// InstanceID extracts the EC2 instance ID from a Kubernetes node providerID.
// The parsing lives here, in the aws provider, so the generic Kubernetes
// platform stays free of any "aws://" scheme knowledge.
func (r *Recycler) InstanceID(providerID string) (string, error) {
	return instanceIDFromProviderID(providerID)
}

// instanceIDFromProviderID extracts the EC2 instance ID from a Kubernetes
// providerID. The AWS cloud provider sets it as "aws:///<az>/<instance-id>";
// the instance ID is the final path segment and begins with "i-". A foreign or
// malformed providerID fails closed so a step never recycles the wrong instance.
func instanceIDFromProviderID(providerID string) (string, error) {
	if providerID == "" {
		return "", fmt.Errorf("has no providerID; the node is not backed by a cloud instance")
	}
	if !strings.HasPrefix(providerID, "aws://") {
		return "", fmt.Errorf("providerID %q is not an AWS instance", providerID)
	}
	segments := strings.Split(providerID, "/")
	id := segments[len(segments)-1]
	if !strings.HasPrefix(id, "i-") {
		return "", fmt.Errorf("providerID %q has no recognizable instance ID", providerID)
	}
	return id, nil
}
