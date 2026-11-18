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

// scalingUpdateComplete checks the update whose ID was returned for this
// operation. Even a successful update does not identify an EC2 instance.
func (p *managedProvider) scalingUpdateComplete(ctx context.Context, s cloud.Successor) (bool, error) {
	id, group := s.State["update"], s.State["nodegroup"]
	if s.Phase != cloud.SuccessorRequested || id == "" || group == "" || p.cluster == "" || p.cluster != s.State["cluster"] {
		return false, fmt.Errorf("%w: incomplete EKS update identity", cloud.ErrNeedsOperator)
	}
	out, err := p.eks.DescribeUpdate(ctx, &eks.DescribeUpdateInput{Name: &p.cluster, NodegroupName: &group, UpdateId: &id})
	if err != nil {
		return false, err
	}
	if out == nil || out.Update == nil || sdk.ToString(out.Update.Id) != id {
		return false, fmt.Errorf("%w: EKS did not confirm the recorded update ID", cloud.ErrNeedsOperator)
	}
	switch out.Update.Status {
	case et.UpdateStatusInProgress:
		return false, nil
	case et.UpdateStatusSuccessful:
		return true, nil
	case et.UpdateStatusFailed:
		for _, detail := range out.Update.Errors {
			switch detail.ErrorCode {
			case et.ErrorCodeEniLimitReached, et.ErrorCodeIpNotAvailable, et.ErrorCodeInsufficientFreeAddresses:
				return false, fmt.Errorf("%w: EKS update %s: %s", cloud.ErrReplacementCapacity, id, detail.ErrorCode)
			}
		}
	}
	return false, fmt.Errorf("%w: EKS update %s has status %q", cloud.ErrNeedsOperator, id, out.Update.Status)
}
