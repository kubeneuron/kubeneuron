# Replacement capacity error codes

Use this table after the provider has identified a replacement capacity failure. A generic launch failure, a networking limit or an unknown code cannot establish the cause for a machine type. Auto Scaling activity messages are free text. Use a structured EC2 or EKS code when available.

| Provider | Codes | Reason | Source |
| --- | --- | --- | --- |
| AWS | `InsufficientInstanceCapacity` | Unavailable | [Provider reference](https://docs.aws.amazon.com/ec2/latest/devguide/errors-overview.html) |
| AWS | `InstanceLimitExceeded`, `VcpuLimitExceeded` | Quota | [Provider reference](https://docs.aws.amazon.com/ec2/latest/devguide/errors-overview.html) |
| AWS | `AsgInstanceLaunchFailures` | Unspecified | [Provider reference](https://docs.aws.amazon.com/eks/latest/APIReference/API_Issue.html) |
| AWS | `InsufficientFreeAddresses` | NetworkAddresses | [Provider reference](https://docs.aws.amazon.com/eks/latest/APIReference/API_Issue.html) |
| AZURE | `AllocationFailed`, `ZonalAllocationFailed`, `OverconstrainedAllocationRequest`, `OverconstrainedZonalAllocationRequest` | Unavailable | [Provider reference](https://learn.microsoft.com/en-us/troubleshoot/azure/azure-kubernetes/availability-performance/cluster-node-virtual-machine-failed-state) |
| AZURE | `QuotaExceeded`, `InsufficientVCPUQuota` | Quota | [Provider reference](https://learn.microsoft.com/troubleshoot/azure/azure-kubernetes/create-upgrade-delete/quota-exceeded-during-creation-upgrade) |
| AZURE | `ResourceGroupQuotaExceeded` | Quota | [Provider reference](https://learn.microsoft.com/en-us/troubleshoot/azure/azure-kubernetes/availability-performance/cluster-node-virtual-machine-failed-state) |
| AZURE | `AKSCapacityError`, `AksCapacityHeavyUsage` | Unavailable | [Provider reference](https://learn.microsoft.com/en-us/troubleshoot/azure/azure-kubernetes/error-codes/aks-error-code-page) |
| AZURE | `SkuNotAvailable` | NotAvailableToSubscription | [Provider reference](https://learn.microsoft.com/en-us/troubleshoot/azure/azure-kubernetes/error-codes/aks-error-code-page) |
| AZURE | `SubnetIsFull` | NetworkAddresses | [Provider reference](https://learn.microsoft.com/en-us/troubleshoot/azure/azure-kubernetes/error-codes/aks-error-code-page) |
| AZURE | `InsufficientSubnetSize` | NetworkAddresses | [Provider reference](https://learn.microsoft.com/en-us/troubleshoot/azure/azure-kubernetes/error-codes/aks-error-code-page) |
| AZURE | `PublicIPCountLimitReached` | NetworkAddresses | [Provider reference](https://learn.microsoft.com/en-us/troubleshoot/azure/azure-kubernetes/error-codes/aks-error-code-page) |
| GCP | `RESOURCE_POOL_EXHAUSTED`, `ZONE_RESOURCE_POOL_EXHAUSTED`, `ZONE_RESOURCE_POOL_EXHAUSTED_WITH_DETAILS`, `REGION_RESOURCE_POOL_EXHAUSTED_WITH_DETAILS`, `POOL_CAPACITY_INSUFFICIENT` | Unavailable | [Provider reference](https://cloud.google.com/compute/docs/reference/rest/v1/errors) |
| GCP | `QUOTA_EXCEEDED` | Quota | [Provider reference](https://cloud.google.com/compute/docs/reference/rest/v1/errors) |
| GCP | `scale.up.error.out.of.resources` | Unavailable | [Provider reference](https://cloud.google.com/kubernetes-engine/docs/troubleshooting/cluster-autoscaler-scale-up) |
| GCP | `scale.up.error.quota.exceeded` | Quota | [Provider reference](https://cloud.google.com/kubernetes-engine/docs/troubleshooting/cluster-autoscaler-scale-up) |
| GCP | `GCE_STOCKOUT` | Unavailable | [Provider reference](https://cloud.google.com/kubernetes-engine/docs/reference/rest/v1/StatusCondition) |
| GCP | `GCE_QUOTA_EXCEEDED` | Quota | [Provider reference](https://cloud.google.com/kubernetes-engine/docs/reference/rest/v1/StatusCondition) |
