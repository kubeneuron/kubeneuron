// SPDX-License-Identifier: Apache-2.0

package aws

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	sdk "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/eks"
	et "github.com/aws/aws-sdk-go-v2/service/eks/types"
	"github.com/kubeneuron/kubeneuron/pkg/cloud"
)

const repairSettleWindow = 30 * time.Minute

type updateReader interface {
	ListUpdates(context.Context, *eks.ListUpdatesInput, ...func(*eks.Options)) (*eks.ListUpdatesOutput, error)
	DescribeUpdate(context.Context, *eks.DescribeUpdateInput, ...func(*eks.Options)) (*eks.DescribeUpdateOutput, error)
}

// updateCache keeps the terminal updates of the groups read: a terminal
// update does not change, so each is described once. Unfinished ones are
// described on every check.
type updateCache struct {
	mu      sync.Mutex
	updates map[string]*et.Update
}

func (c *updateCache) get(cluster, group, id string) *et.Update {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.updates[cluster+"/"+group+"/"+id]
}

func (c *updateCache) put(cluster, group string, u *et.Update) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.updates == nil {
		c.updates = map[string]*et.Update{}
	}
	c.updates[cluster+"/"+group+"/"+sdk.ToString(u.Id)] = u
}

// scalingOnly reports an update that changed only the group's size. With
// native repair off a size change starts no repair, so it opens no window;
// the Auto Scaling activity check covers the scaling it causes.
func scalingOnly(u *et.Update) bool {
	if u.Type != et.UpdateTypeConfigUpdate || len(u.Params) == 0 {
		return false
	}
	for _, p := range u.Params {
		switch p.Type {
		case et.UpdateParamTypeDesiredSize, et.UpdateParamTypeMinSize, et.UpdateParamTypeMaxSize:
		default:
			return false
		}
	}
	return true
}

func updateKind(u *et.Update) string {
	var params []string
	for _, p := range u.Params {
		params = append(params, string(p.Type))
	}
	if len(params) == 0 {
		return string(u.Type)
	}
	slices.Sort(params)
	return strings.Join(slices.Compact(params), ", ")
}

// clock is t in UTC, with its date when that is not now's date.
func clock(t, now time.Time) string {
	t, now = t.UTC(), now.UTC()
	if y, m, d := t.Date(); y != now.Year() || m != now.Month() || d != now.Day() {
		return t.Format("2006-01-02 15:04 UTC")
	}
	return t.Format("15:04 UTC")
}

// listUpdateIDs reads the group's whole update history.
func listUpdateIDs(ctx context.Context, client updateReader, cluster, group string) ([]string, error) {
	in := &eks.ListUpdatesInput{Name: &cluster, NodegroupName: &group}
	seen := map[string]bool{}
	var ids []string
	for {
		page, err := client.ListUpdates(ctx, in)
		if err != nil {
			return nil, &readError{"eks:ListUpdates", err}
		}
		if page == nil {
			return nil, errors.New("missing EKS update history")
		}
		for _, id := range page.UpdateIds {
			if id == "" {
				return nil, errors.New("missing EKS update identifier")
			}
			ids = append(ids, id)
		}
		token := sdk.ToString(page.NextToken)
		if token == "" {
			slices.Sort(ids)
			return ids, nil
		}
		if seen[token] {
			return nil, errors.New("incomplete EKS update pagination")
		}
		seen[token] = true
		in.NextToken = page.NextToken
	}
}

// checkUpdateHistory holds while an update of the group is unfinished, and for
// repairSettleWindow after one that changed more than its size: even a
// terminal update does not tell when work started under the previous policy
// completed. The group's modifiedAt is not used: EKS moves it without an
// update or an API call. It returns the update IDs it read.
func checkUpdateHistory(ctx context.Context, client updateReader, cache *updateCache, cluster, group string, now time.Time) ([]string, error) {
	if cluster == "" || group == "" {
		return nil, errors.New("explicit EKS cluster and nodegroup required")
	}
	ids, err := listUpdateIDs(ctx, client, cluster, group)
	if err != nil {
		return nil, err
	}
	var recent *et.Update
	for _, id := range ids {
		u := cache.get(cluster, group, id)
		if u == nil {
			detail, err := client.DescribeUpdate(ctx, &eks.DescribeUpdateInput{Name: &cluster, NodegroupName: &group, UpdateId: &id})
			if err != nil {
				return nil, &readError{"eks:DescribeUpdate", err}
			}
			if detail == nil || detail.Update == nil || sdk.ToString(detail.Update.Id) != id {
				return nil, errors.New("unconfirmed EKS update identity")
			}
			u = detail.Update
		}
		switch u.Status {
		case et.UpdateStatusSuccessful, et.UpdateStatusFailed, et.UpdateStatusCancelled:
		case et.UpdateStatusInProgress:
			return nil, &cloud.RecoveryHold{Code: cloud.HoldUpdateRunning,
				Reason: fmt.Sprintf("An EKS update of node group %s (%s) is still in progress.", group, updateKind(u)),
				Action: "KubeNeuron checks again every minute."}
		default:
			return nil, errors.New("EKS nodegroup update status is unknown")
		}
		if u.CreatedAt == nil || u.CreatedAt.IsZero() || u.CreatedAt.After(now) {
			return nil, errors.New("missing or invalid EKS update time")
		}
		cache.put(cluster, group, u)
		if now.Sub(*u.CreatedAt) < repairSettleWindow && !scalingOnly(u) && (recent == nil || u.CreatedAt.After(*recent.CreatedAt)) {
			recent = u
		}
	}
	if recent != nil {
		return nil, &cloud.RecoveryHold{Code: cloud.HoldRecentChange,
			Reason: fmt.Sprintf("Node group %s changed at %s (EKS update: %s).", group, clock(*recent.CreatedAt, now), updateKind(recent)),
			Action: fmt.Sprintf("KubeNeuron waits 30 minutes after a change and checks again at %s.", clock(recent.CreatedAt.Add(repairSettleWindow), now))}
	}
	return ids, nil
}
