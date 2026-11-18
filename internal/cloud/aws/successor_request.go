// SPDX-License-Identifier: Apache-2.0

package aws

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"maps"
	"math"
	"strconv"

	sdk "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/eks"
	et "github.com/aws/aws-sdk-go-v2/service/eks/types"
	"github.com/aws/smithy-go"
	"github.com/kubeneuron/kubeneuron/pkg/cloud"
)

type nodegroupUpdater interface {
	UpdateNodegroupConfig(context.Context, *eks.UpdateNodegroupConfigInput, ...func(*eks.Options)) (*eks.UpdateNodegroupConfigOutput, error)
}

// requestSuccessor replays the original absolute target and EKS token after a lost response.
func requestSuccessor(ctx context.Context, client nodegroupUpdater, cluster string, s cloud.Successor) (cloud.Successor, error) {
	before, err := strconv.ParseInt(s.State["before"], 10, 32)
	group := s.State["nodegroup"]
	if err != nil || before < 1 || before >= math.MaxInt32 || group == "" || cluster == "" || cluster != s.State["cluster"] || s.Operation == "" || s.Phase != cloud.SuccessorPlanned {
		return s, fmt.Errorf("%w: incomplete EKS scaling plan", cloud.ErrNeedsOperator)
	}
	token := fmt.Sprintf("%x", sha256.Sum256([]byte(s.Operation)))
	out, err := client.UpdateNodegroupConfig(ctx, &eks.UpdateNodegroupConfigInput{ClusterName: &cluster, NodegroupName: &group, ClientRequestToken: &token, ScalingConfig: &et.NodegroupScalingConfig{DesiredSize: sdk.Int32(int32(before + 1))}})
	if err != nil {
		var api smithy.APIError
		if errors.As(err, &api) && (api.ErrorCode() == "AccessDeniedException" || api.ErrorCode() == "AccessDenied") {
			return s, fmt.Errorf("%w: eks:UpdateNodegroupConfig: %w", cloud.ErrNeedsOperator, err)
		}
		return s, err
	}
	if out == nil || out.Update == nil || sdk.ToString(out.Update.Id) == "" {
		return s, fmt.Errorf("EKS returned no scaling update ID")
	}
	s.State = maps.Clone(s.State)
	s.State["update"] = *out.Update.Id
	s.Phase = cloud.SuccessorRequested
	return s, nil
}
