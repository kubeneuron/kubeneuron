// SPDX-License-Identifier: Apache-2.0

package aws

import (
	"context"
	"errors"
	"time"

	sdk "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/eks"
	et "github.com/aws/aws-sdk-go-v2/service/eks/types"
)

const repairSettleWindow = 30 * time.Minute

type updateReader interface {
	ListUpdates(context.Context, *eks.ListUpdatesInput, ...func(*eks.Options)) (*eks.ListUpdatesOutput, error)
	DescribeUpdate(context.Context, *eks.DescribeUpdateInput, ...func(*eks.Options)) (*eks.DescribeUpdateOutput, error)
}

// A recent change of any kind is a conservative veto: even a terminal update
// does not indicate when work started under the previous policy completed.
func settledSince(now time.Time, stamp *time.Time) error {
	if stamp == nil || stamp.IsZero() || stamp.After(now) {
		return errors.New("missing or invalid EKS change time")
	}
	if now.Sub(*stamp) < repairSettleWindow {
		return errors.New("repair configuration changed recently")
	}
	return nil
}

func checkUpdateHistory(ctx context.Context, client updateReader, cluster, group string, modified *time.Time, now time.Time) error {
	if cluster == "" || group == "" {
		return errors.New("explicit EKS cluster and nodegroup required")
	}
	if err := settledSince(now, modified); err != nil {
		return err
	}
	in := &eks.ListUpdatesInput{Name: &cluster, NodegroupName: &group}
	seen := map[string]bool{}
	for {
		page, err := client.ListUpdates(ctx, in)
		if err != nil {
			return err
		}
		if page == nil {
			return errors.New("missing EKS update history")
		}
		for _, id := range page.UpdateIds {
			if id == "" {
				return errors.New("missing EKS update identifier")
			}
			detail, err := client.DescribeUpdate(ctx, &eks.DescribeUpdateInput{Name: &cluster, NodegroupName: &group, UpdateId: &id})
			if err != nil {
				return err
			}
			if detail == nil || detail.Update == nil || sdk.ToString(detail.Update.Id) != id {
				return errors.New("unconfirmed EKS update identity")
			}
			if err := settledSince(now, detail.Update.CreatedAt); err != nil {
				return err
			}
			switch detail.Update.Status {
			case et.UpdateStatusSuccessful, et.UpdateStatusFailed, et.UpdateStatusCancelled:
			default:
				return errors.New("EKS nodegroup update is unfinished or unknown")
			}
		}
		token := sdk.ToString(page.NextToken)
		if token == "" {
			return nil
		}
		if seen[token] {
			return errors.New("incomplete EKS update pagination")
		}
		seen[token] = true
		in.NextToken = page.NextToken
	}
}
