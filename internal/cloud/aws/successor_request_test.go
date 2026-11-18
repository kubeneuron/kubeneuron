// SPDX-License-Identifier: Apache-2.0

package aws

import (
	"context"
	"errors"
	"testing"
	"time"

	sdk "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/eks"
	et "github.com/aws/aws-sdk-go-v2/service/eks/types"
	"github.com/kubeneuron/kubeneuron/pkg/cloud"
)

type lostUpdateResponse struct {
	t              *testing.T
	tokens         map[string]bool
	cluster, group string
	replayErr      error
}

func (f *lostUpdateResponse) UpdateNodegroupConfig(_ context.Context, in *eks.UpdateNodegroupConfigInput, _ ...func(*eks.Options)) (*eks.UpdateNodegroupConfigOutput, error) {
	f.t.Helper()
	cfg := in.ScalingConfig
	if sdk.ToString(in.ClusterName) != f.cluster || sdk.ToString(in.NodegroupName) != f.group || cfg == nil || sdk.ToInt32(cfg.DesiredSize) != 3 || cfg.MinSize != nil || cfg.MaxSize != nil {
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
	if f.replayErr != nil {
		return nil, f.replayErr
	}
	return &eks.UpdateNodegroupConfigOutput{Update: &et.Update{Id: sdk.String("update-id"), Type: et.UpdateTypeConfigUpdate, Params: []et.UpdateParam{{Type: et.UpdateParamTypeDesiredSize, Value: sdk.String("3")}}, CreatedAt: sdk.Time(time.Unix(1000, 0))}}, nil
}

func TestRequestSuccessorReplaysLostResponse(t *testing.T) {
	client := &lostUpdateResponse{t: t, tokens: map[string]bool{}, cluster: "cluster", group: "workers"}
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
	if !got.RequestedAt.Equal(time.Unix(1000, 0)) {
		t.Fatalf("activity window starts at %v", got.RequestedAt)
	}
	plan.RequestedAt = time.Unix(990, 0)
	got, err = requestSuccessor(t.Context(), client, "cluster", plan)
	if err != nil || !got.RequestedAt.Equal(plan.RequestedAt) {
		t.Fatalf("lost persisted request time: %+v %v", got, err)
	}

}

type scalingUpdateFixture struct {
	groupHistory
	out *eks.DescribeUpdateOutput
}

func (f scalingUpdateFixture) DescribeUpdate(_ context.Context, in *eks.DescribeUpdateInput, _ ...func(*eks.Options)) (*eks.DescribeUpdateOutput, error) {
	if sdk.ToString(in.Name) != "training" || sdk.ToString(in.NodegroupName) != "gpu" || sdk.ToString(in.UpdateId) != "update-1" {
		return nil, errors.New("unexpected EKS lookup")
	}
	return f.out, nil
}
func TestScalingUpdateComplete(t *testing.T) {
	s := cloud.Successor{Phase: cloud.SuccessorRequested, State: map[string]string{"cluster": "training", "nodegroup": "gpu", "update": "update-1"}}
	for _, tc := range []struct {
		name, id string
		status   et.UpdateStatus
		code     et.ErrorCode
		complete bool
		wantErr  error
	}{
		{"pending", "update-1", et.UpdateStatusInProgress, "", false, nil},
		{"success", "update-1", et.UpdateStatusSuccessful, "", true, nil},
		{"foreign success", "other", et.UpdateStatusSuccessful, "", false, cloud.ErrNeedsOperator},
		{"cancelled", "update-1", et.UpdateStatusCancelled, "", false, cloud.ErrNeedsOperator},
		{"capacity", "update-1", et.UpdateStatusFailed, et.ErrorCodeInsufficientFreeAddresses, false, cloud.ErrReplacementCapacity},
		{"creation failure", "update-1", et.UpdateStatusFailed, et.ErrorCodeNodeCreationFailure, false, cloud.ErrNeedsOperator},
	} {
		t.Run(tc.name, func(t *testing.T) {
			update := et.Update{Id: sdk.String(tc.id), Status: tc.status, Errors: []et.ErrorDetail{{ErrorCode: tc.code}}}
			p := managedProvider{repairReader: &repairReader{cluster: "training", eks: &scalingUpdateFixture{out: &eks.DescribeUpdateOutput{Update: &update}}}}
			done, err := p.scalingUpdateComplete(t.Context(), s)
			if done != tc.complete || !errors.Is(err, tc.wantErr) {
				t.Fatalf("complete=%t err=%v", done, err)
			}
		})
	}
}

func TestRecordedScalingUpdateChecksReportedTarget(t *testing.T) {
	s := cloud.Successor{Phase: cloud.SuccessorRequested, State: map[string]string{"cluster": "training", "nodegroup": "gpu", "before": "2", "update": "update-1"}}
	for _, tc := range []struct {
		param et.UpdateParamType
		value string
		valid bool
	}{
		{et.UpdateParamTypeDesiredSize, "3", true},
		{et.UpdateParamTypeDesiredSize, "4", false},
		{et.UpdateParamTypeMaxSize, "3", false},
		{et.UpdateParamTypeMinSize, "2", false},
	} {
		update := et.Update{Id: sdk.String("update-1"), Type: et.UpdateTypeConfigUpdate, Status: et.UpdateStatusSuccessful, Params: []et.UpdateParam{{Type: tc.param, Value: &tc.value}}}
		p := managedProvider{repairReader: &repairReader{cluster: "training", eks: &scalingUpdateFixture{out: &eks.DescribeUpdateOutput{Update: &update}}}}
		complete, err := p.scalingUpdateComplete(t.Context(), s)
		if complete != tc.valid || (tc.valid && err != nil) || (!tc.valid && !errors.Is(err, cloud.ErrNeedsOperator)) {
			t.Errorf("%s=%s: complete=%v err=%v", tc.param, tc.value, complete, err)
		}
	}
}
