package main

import (
	"context"

	"github.com/kubeneuron/kubeneuron/internal/cloud"
	// Blank import registers every cloud provider (aws today). Adding a cloud is
	// a new provider package plus one import line in internal/cloud/providers.
	_ "github.com/kubeneuron/kubeneuron/internal/cloud/providers"
	"github.com/kubeneuron/kubeneuron/internal/platform"
)

// newInstanceRecycler builds the cloud provider that recycles or replaces the
// instance behind a node, by name, from the registry. An unsupported provider
// fails closed with a clear message rather than silently disabling recycling.
func newInstanceRecycler(ctx context.Context, provider, region string) (platform.InstanceRecycler, error) {
	p, err := cloud.Build(ctx, provider, cloud.Config{Region: region})
	if err != nil {
		return nil, err
	}
	return p, nil
}
