// SPDX-License-Identifier: Apache-2.0

package aws

import (
	"context"
	"errors"
	"testing"

	sdk "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/eks"
	et "github.com/aws/aws-sdk-go-v2/service/eks/types"
	"github.com/kubeneuron/kubeneuron/pkg/cloud"
)

type lostUpdateResponse struct {
	t      *testing.T
	tokens map[string]bool
}

func (f *lostUpdateResponse) UpdateNodegroupConfig(_ context.Context, in *eks.UpdateNodegroupConfigInput, _ ...func(*eks.Options)) (*eks.UpdateNodegroupConfigOutput, error) {
	f.t.Helper()
	cfg := in.ScalingConfig
	if sdk.ToString(in.ClusterName) != "cluster" || sdk.ToString(in.NodegroupName) != "workers" || cfg == nil || sdk.ToInt32(cfg.DesiredSize) != 3 || cfg.MinSize != nil || cfg.MaxSize != nil {
		f.t.Fatalf("unexpected scaling request: %+v", in)
	}
	token := sdk.ToString(in.ClientRequestToken)
	if token == "" {
		f.t.Fatal("missing idempotency token")
	}
	if !f.tokens[token] {
		f.tokens[token] = true
		return nil, errors.New("response lost")
	}
	return &eks.UpdateNodegroupConfigOutput{Update: &et.Update{Id: sdk.String("update-id")}}, nil
}

func TestRequestSuccessorReplaysLostResponse(t *testing.T) {
	client := &lostUpdateResponse{t: t, tokens: map[string]bool{}}
	plan := cloud.Successor{Operation: "op", Phase: cloud.SuccessorPlanned, State: map[string]string{"cluster": "cluster", "before": "2", "nodegroup": "workers"}}
	if _, err := requestSuccessor(t.Context(), client, "different-cluster", plan); !errors.Is(err, cloud.ErrNeedsOperator) || len(client.tokens) != 0 {
		t.Fatalf("cluster mismatch: %v", err)
	}
	if got, err := requestSuccessor(t.Context(), client, "cluster", plan); err == nil || got.Phase != cloud.SuccessorPlanned {
		t.Fatalf("first request: %+v %v", got, err)
	}
	got, err := requestSuccessor(t.Context(), client, "cluster", plan)
	if err != nil || got.Phase != cloud.SuccessorRequested || got.State["update"] != "update-id" || len(client.tokens) != 1 || plan.State["update"] != "" {
		t.Fatalf("replay: %+v %v", got, err)
	}
}
