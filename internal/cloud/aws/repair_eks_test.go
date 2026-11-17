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
)

func TestRepairSettleWindow(t *testing.T) {
	now := time.Now()
	for _, tc := range []struct {
		name string
		age  time.Duration
		ok   bool
	}{
		{"recent", 30*time.Minute - time.Nanosecond, false},
		{"boundary", 30 * time.Minute, true},
		{"old", time.Hour, true},
		{"future", -time.Second, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stamp := now.Add(-tc.age)
			if err := settledSince(now, &stamp); (err == nil) != tc.ok {
				t.Fatalf("settledSince: %v", err)
			}
		})
	}
	if settledSince(now, nil) == nil {
		t.Fatal("missing timestamp accepted")
	}
}

type updatePages struct {
	t       *testing.T
	outcome string
	calls   int
	old     time.Time
}

func (p *updatePages) ListUpdates(_ context.Context, in *eks.ListUpdatesInput, _ ...func(*eks.Options)) (*eks.ListUpdatesOutput, error) {
	p.t.Helper()
	p.calls++
	if sdk.ToString(in.Name) != "cluster" || sdk.ToString(in.NodegroupName) != "gpu" {
		p.t.Fatal("unexpected scope")
	}
	if p.calls == 1 {
		return &eks.ListUpdatesOutput{NextToken: sdk.String("more")}, nil
	}
	if p.calls != 2 || sdk.ToString(in.NextToken) != "more" {
		p.t.Fatal("unexpected pagination")
	}
	switch p.outcome {
	case "denied":
		return nil, errors.New("AccessDenied")
	case "repeat":
		return &eks.ListUpdatesOutput{NextToken: sdk.String("more")}, nil
	default:
		return &eks.ListUpdatesOutput{UpdateIds: []string{"change"}}, nil
	}
}
func (p *updatePages) DescribeUpdate(_ context.Context, in *eks.DescribeUpdateInput, _ ...func(*eks.Options)) (*eks.DescribeUpdateOutput, error) {
	p.t.Helper()
	if sdk.ToString(in.UpdateId) != "change" || sdk.ToString(in.Name) != "cluster" || sdk.ToString(in.NodegroupName) != "gpu" {
		p.t.Fatal("unexpected describe scope")
	}
	update := et.Update{Id: in.UpdateId, CreatedAt: &p.old, Status: et.UpdateStatusSuccessful}
	switch p.outcome {
	case "throttled":
		return nil, errors.New("ThrottlingException")
	case "in progress":
		update.Status = et.UpdateStatusInProgress
	case "wrong id":
		update.Id = sdk.String("other")
	}
	return &eks.DescribeUpdateOutput{Update: &update}, nil
}
func TestEKSUpdateHistoryReadsSecondPage(t *testing.T) {
	now := time.Now()
	for _, outcome := range []string{"clear", "denied", "repeat", "throttled", "in progress", "wrong id"} {
		t.Run(outcome, func(t *testing.T) {
			p := &updatePages{t: t, outcome: outcome, old: now.Add(-time.Hour)}
			err := checkUpdateHistory(t.Context(), p, "cluster", "gpu", &p.old, now)
			if (err == nil) != (outcome == "clear") || p.calls != 2 {
				t.Fatalf("pages=%d err=%v", p.calls, err)
			}
		})
	}
}
