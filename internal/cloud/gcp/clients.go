// SPDX-License-Identifier: Apache-2.0

package gcp

import (
	"context"
	"fmt"

	compute "cloud.google.com/go/compute/apiv1"
	container "cloud.google.com/go/container/apiv1"
	resourcemanager "cloud.google.com/go/resourcemanager/apiv3"
)

// newSDKAPI builds the clients on Application Default Credentials: on GKE,
// the controller's Workload Identity Federation token from the metadata
// server. No key is read from the KubeNeuron resource or its Secrets.
func newSDKAPI(ctx context.Context, s settings) (*sdkAPI, error) {
	clusters, err := container.NewClusterManagerRESTClient(ctx)
	if err != nil {
		return nil, fmt.Errorf("gcp: GKE client (are Application Default Credentials available to the controller pod?): %w", err)
	}
	instances, err := compute.NewInstancesRESTClient(ctx)
	if err != nil {
		return nil, fmt.Errorf("gcp: Compute Engine client: %w", err)
	}
	groups, err := compute.NewInstanceGroupManagersRESTClient(ctx)
	if err != nil {
		return nil, fmt.Errorf("gcp: Compute Engine client: %w", err)
	}
	projects, err := resourcemanager.NewProjectsRESTClient(ctx)
	if err != nil {
		return nil, fmt.Errorf("gcp: Resource Manager client: %w", err)
	}
	return &sdkAPI{settings: s, clusters: clusters, instances: instances, groups: groups, projects: projects}, nil
}
