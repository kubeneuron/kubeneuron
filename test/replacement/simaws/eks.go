// SPDX-License-Identifier: Apache-2.0

package simaws

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/eks"
	et "github.com/aws/aws-sdk-go-v2/service/eks/types"
)

// EKS serves a single managed node group. Grow applies the requested absolute
// size to its backing Auto Scaling group before the update becomes visible.
type EKS struct {
	Group      et.Nodegroup
	Grow       func(int32, time.Time) error
	mu         sync.Mutex
	updates    map[string]*et.Update
	hold, lose bool
	launch     func(time.Time)
	pending    *et.Update
	scaling    *Scaling
}

func (e *EKS) group(cluster, name *string) error {
	if aws.ToString(cluster) != aws.ToString(e.Group.ClusterName) || aws.ToString(name) != aws.ToString(e.Group.NodegroupName) {
		return errors.New("unknown simulated EKS node group")
	}
	return nil
}

func (e *EKS) DescribeNodegroup(_ context.Context, in *eks.DescribeNodegroupInput, _ ...func(*eks.Options)) (*eks.DescribeNodegroupOutput, error) {
	if err := e.group(in.ClusterName, in.NodegroupName); err != nil {
		return nil, err
	}
	return &eks.DescribeNodegroupOutput{Nodegroup: &e.Group}, nil
}

func (e *EKS) UpdateNodegroupConfig(_ context.Context, in *eks.UpdateNodegroupConfigInput, _ ...func(*eks.Options)) (*eks.UpdateNodegroupConfigOutput, error) {
	if err := e.group(in.ClusterName, in.NodegroupName); err != nil {
		return nil, err
	}
	token := aws.ToString(in.ClientRequestToken)
	if token == "" || in.ScalingConfig == nil || in.ScalingConfig.DesiredSize == nil {
		return nil, errors.New("expected a token and desired size")
	}
	size := *in.ScalingConfig.DesiredSize
	value := fmt.Sprint(size)
	e.mu.Lock()
	defer e.mu.Unlock()
	if previous := e.updates[token]; previous != nil {
		if aws.ToString(previous.Params[0].Value) != value {
			return nil, errors.New("scale target changed for existing token")
		}
		return &eks.UpdateNodegroupConfigOutput{Update: previous}, nil
	}
	now := time.Now()
	if err := e.Grow(size, now); err != nil {
		return nil, err
	}
	update := &et.Update{Id: aws.String(token), CreatedAt: &now, Status: et.UpdateStatusSuccessful, Type: et.UpdateTypeConfigUpdate, Params: []et.UpdateParam{{Type: et.UpdateParamTypeDesiredSize, Value: &value}}}
	if e.updates == nil {
		e.updates = make(map[string]*et.Update)
	}
	if e.hold {
		update.Status = et.UpdateStatusInProgress
		e.pending = update
		e.hold = false
	} else {
		e.launch(now)
	}
	e.updates[token] = update
	if e.lose {
		e.lose = false
		return nil, errors.New("simulated lost EKS response")
	}
	return &eks.UpdateNodegroupConfigOutput{Update: update}, nil
}

func (e *EKS) ListUpdates(_ context.Context, in *eks.ListUpdatesInput, _ ...func(*eks.Options)) (*eks.ListUpdatesOutput, error) {
	if err := e.group(in.Name, in.NodegroupName); err != nil {
		return nil, err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	var ids []string
	for id := range e.updates {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	return &eks.ListUpdatesOutput{UpdateIds: ids}, nil
}

func (e *EKS) DescribeUpdate(_ context.Context, in *eks.DescribeUpdateInput, _ ...func(*eks.Options)) (*eks.DescribeUpdateOutput, error) {
	if err := e.group(in.Name, in.NodegroupName); err != nil {
		return nil, err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	u := e.updates[aws.ToString(in.UpdateId)]
	if u == nil {
		return nil, errors.New("unknown EKS update")
	}
	return &eks.DescribeUpdateOutput{Update: u}, nil
}

func (e *EKS) HoldNextRequest() { e.mu.Lock(); defer e.mu.Unlock(); e.hold = true }
func (e *EKS) LoseNextAnswer()  { e.mu.Lock(); defer e.mu.Unlock(); e.lose = true }
func (e *EKS) Requests() int    { e.mu.Lock(); defer e.mu.Unlock(); return len(e.updates) }
func (e *EKS) Size() int {
	e.scaling.mu.Lock()
	defer e.scaling.mu.Unlock()
	return int(aws.ToInt32(e.scaling.group.DesiredCapacity))
}
func (e *EKS) Deliver() {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.pending != nil {
		e.launch(*e.pending.CreatedAt)
		update := *e.pending
		update.Status = et.UpdateStatusSuccessful
		e.updates[*update.Id] = &update
		e.pending = nil
	}
}
