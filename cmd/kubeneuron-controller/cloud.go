// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"os"

	"github.com/kubeneuron/kubeneuron/pkg/cloud"
	// Blank import registers every cloud provider. Adding a cloud is a new
	// provider package plus one import line in internal/cloud/providers.
	"github.com/kubeneuron/kubeneuron/internal/cloud/azure"
	_ "github.com/kubeneuron/kubeneuron/internal/cloud/providers"
	"github.com/kubeneuron/kubeneuron/internal/platform"
)

// newInstanceRecycler builds the cloud provider that recycles or replaces the
// instance behind a node, by name, from the registry. An unsupported provider
// fails closed with a clear message rather than silently disabling recycling.
func newInstanceRecycler(ctx context.Context, provider, region string) (platform.InstanceRecycler, error) {
	cfg := cloud.Config{Region: region}
	switch provider {
	case "aws":
		cfg.Options = map[string]string{"cluster": os.Getenv("KUBENEURON_EKS_CLUSTER")}
	case "azure":
		cfg.Options = map[string]string{
			azure.OptionSubscription:  os.Getenv("KUBENEURON_AKS_SUBSCRIPTION_ID"),
			azure.OptionResourceGroup: os.Getenv("KUBENEURON_AKS_RESOURCE_GROUP"),
			azure.OptionCluster:       os.Getenv("KUBENEURON_AKS_CLUSTER"),
		}
	}
	p, err := cloud.Build(ctx, provider, cfg)
	if err != nil {
		return nil, err
	}
	return p, nil
}
