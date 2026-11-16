// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"testing"

	"github.com/kubeneuron/kubeneuron/pkg/cloud"
	"github.com/kubeneuron/kubeneuron/pkg/types"
)

func TestCloudEligibilityDoesNotAuthorizeMissingOrForeignPin(t *testing.T) {
	c := &Controller{}
	inc := &types.Incident{Target: types.Target{Node: "worker"}}
	v, err := c.cloudRecoveryEligibility(context.Background(), inc)
	if err != nil || v.ReplaceAllowed || v.RecycleAllowed || v.Reason == "" {
		t.Fatalf("legacy incident: verdict=%+v err=%v", v, err)
	}
	inc.CloudIdentity = &cloud.NodeIdentity{Node: "other-worker", NodeUID: "uid", ProviderID: "provider", InstanceID: "instance", BootID: "boot"}
	v, err = c.cloudRecoveryEligibility(context.Background(), inc)
	if err == nil || v.ReplaceAllowed || v.RecycleAllowed {
		t.Fatalf("foreign pin: verdict=%+v err=%v", v, err)
	}
}
