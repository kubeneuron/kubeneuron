// SPDX-License-Identifier: Apache-2.0

package aws

import (
	"context"
	"fmt"
	"time"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/autoscaling"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/eks"
	"github.com/aws/aws-sdk-go-v2/service/sqs"

	"github.com/kubeneuron/kubeneuron/pkg/cloud"
)

func newManagedProvider(ctx context.Context, options cloud.Config) (*managedProvider, error) {
	region := options.Region
	var opts []func(*awsconfig.LoadOptions) error
	if region != "" {
		opts = append(opts, awsconfig.WithRegion(region))
	}
	cfg, err := awsconfig.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("aws: loading configuration: %w", err)
	}
	if cfg.Region == "" {
		return nil, fmt.Errorf("aws: no region configured; set AWS_REGION or the node's region label")
	}
	client := ec2.NewFromConfig(cfg)
	eksClient := eks.NewFromConfig(cfg)
	scalingClient := autoscaling.NewFromConfig(cfg)
	provider := &managedProvider{updater: eksClient, terminator: scalingClient, Recycler: &Recycler{api: client, pollInterval: 10 * time.Second}, repairReader: &repairReader{checkAutoscalerGroup: options.CheckAutoscalerGroup, cluster: options.Options["cluster"], eks: eksClient, scaling: scalingClient, ec2: client, updates: &updateCache{}}}
	if url := options.Options[OptionInterruptionQueue]; url != "" {
		provider.queue = newInterruptionQueue(sqs.NewFromConfig(cfg), url)
	}
	return provider, nil
}
