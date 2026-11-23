// SPDX-License-Identifier: Apache-2.0

package azure

import (
	"fmt"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/arm"
	armruntime "github.com/Azure/azure-sdk-for-go/sdk/azcore/arm/runtime"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/runtime"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	aks "github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/containerservice/armcontainerservice/v9"
)

func newSDKAPI(s settings) (*sdkAPI, error) {
	// Only the workload identity: no client secret or certificate is ever read,
	// so no credential can sit in the KubeNeuron resource or its Secrets.
	cred, err := azidentity.NewWorkloadIdentityCredential(nil)
	if err != nil {
		return nil, fmt.Errorf("azure: workload identity unavailable (is the AKS workload identity webhook enabled for the controller pod?): %w", err)
	}
	opts := &arm.ClientOptions{}
	factory, err := aks.NewClientFactory(s.subscription, cred, opts)
	if err != nil {
		return nil, fmt.Errorf("azure: AKS clients: %w", err)
	}
	pl, err := armruntime.NewPipeline("kubeneuron-azure", "v1", cred, runtime.PipelineOptions{}, opts)
	if err != nil {
		return nil, fmt.Errorf("azure: authorization client: %w", err)
	}
	return &sdkAPI{
		subscription: s.subscription, resourceGroup: s.resourceGroup, cluster: s.cluster,
		pools: factory.NewAgentPoolsClient(), machines: factory.NewMachinesClient(), clusters: factory.NewManagedClustersClient(),
		pipeline: pl, endpoint: "https://management.azure.com", pollFrequency: 15 * time.Second,
	}, nil
}
