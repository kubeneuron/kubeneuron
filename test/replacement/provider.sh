#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Settings shared by manifest rendering and node identity checks.
# CLOUD_CONFIG is JSON data passed as one quoted argument to jq.
# shellcheck disable=SC2089,SC2090
case ${REPLACEMENT_PROVIDER:-aws} in
  aws)
    POOL_LABEL=eks.amazonaws.com/nodegroup
    INITIAL_ID=aws:///az/i-pinned
    CLOUD_CONFIG='{"provider":"aws","aws":{"clusterName":"training","region":"us-east-1"}}'
    ;;
  gke)
    POOL_LABEL=cloud.google.com/gke-nodepool
    INITIAL_ID="gce://ml-project/us-central1-a/${CLUSTER_NAME:?}-worker"
    CLOUD_CONFIG='{"provider":"gcp","gcp":{"project":"ml-project","location":"us-central1","clusterName":"training"}}'
    ;;
  aks)
    POOL_LABEL=kubernetes.azure.com/agentpool
    INITIAL_ID=azure:///subscriptions/11111111-2222-3333-4444-555555555555/resourceGroups/MC_ml_training_eastus/providers/Microsoft.Compute/virtualMachineScaleSets/aks-gpu-12345678-vmss/virtualMachines/0
    CLOUD_CONFIG='{"provider":"azure","azure":{"subscriptionID":"11111111-2222-3333-4444-555555555555","resourceGroup":"ml","clusterName":"training","clientID":"aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"}}'
    ;;
  *) echo "Unknown replacement provider: $REPLACEMENT_PROVIDER" >&2; exit 1 ;;
esac
case ${ZONE_SCENARIO:-} in
  "") ;;
  AnyPoolZone|SameZone)
    if [[ ${REPLACEMENT_PROVIDER:-aws} != aws ]]; then
      echo "ZONE_SCENARIO requires the AWS simulator" >&2; exit 1
    fi
    INITIAL_ID=aws:///us-east-1a/i-pinned
    CLOUD_CONFIG=$(jq -c '.aws.clusterName = "training-two-zones"' <<<"$CLOUD_CONFIG")
    ;;
  *) echo "Unknown ZONE_SCENARIO: $ZONE_SCENARIO" >&2; exit 1 ;;
esac
export INITIAL_ID POOL_LABEL CLOUD_CONFIG
