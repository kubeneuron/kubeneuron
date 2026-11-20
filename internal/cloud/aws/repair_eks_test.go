// SPDX-License-Identifier: Apache-2.0

package aws

import (
	"context"
	"errors"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	sdk "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/eks"
	et "github.com/aws/aws-sdk-go-v2/service/eks/types"
	"github.com/aws/smithy-go"
	"github.com/kubeneuron/kubeneuron/pkg/cloud"
)

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
	update := et.Update{Id: in.UpdateId, CreatedAt: &p.old, Status: et.UpdateStatusSuccessful, Type: et.UpdateTypeConfigUpdate,
		Params: []et.UpdateParam{{Type: et.UpdateParamTypeNodeRepairEnabled, Value: sdk.String("false")}}}
	switch p.outcome {
	case "throttled":
		return nil, errors.New("ThrottlingException")
	case "in progress":
		update.Status = et.UpdateStatusInProgress
	case "unknown status":
		update.Status = "Pending"
	case "wrong id":
		update.Id = sdk.String("other")
	}
	return &eks.DescribeUpdateOutput{Update: &update}, nil
}
func TestEKSUpdateHistoryReadsSecondPage(t *testing.T) {
	now := time.Now()
	for _, outcome := range []string{"clear", "denied", "repeat", "throttled", "in progress", "unknown status", "wrong id"} {
		t.Run(outcome, func(t *testing.T) {
			p := &updatePages{t: t, outcome: outcome, old: now.Add(-time.Hour)}
			ids, err := checkUpdateHistory(t.Context(), p, nil, "cluster", "gpu", now)
			if (err == nil) != (outcome == "clear") || p.calls != 2 {
				t.Fatalf("pages=%d err=%v", p.calls, err)
			}
			if outcome == "clear" && (len(ids) != 1 || ids[0] != "change") {
				t.Fatalf("ids %v", ids)
			}
		})
	}
}

// oneUpdate is a group history of a single update.
type oneUpdate struct{ update et.Update }

func (o oneUpdate) ListUpdates(context.Context, *eks.ListUpdatesInput, ...func(*eks.Options)) (*eks.ListUpdatesOutput, error) {
	return &eks.ListUpdatesOutput{UpdateIds: []string{sdk.ToString(o.update.Id)}}, nil
}
func (o oneUpdate) DescribeUpdate(context.Context, *eks.DescribeUpdateInput, ...func(*eks.Options)) (*eks.DescribeUpdateOutput, error) {
	u := o.update
	return &eks.DescribeUpdateOutput{Update: &u}, nil
}

func TestEKSUpdateHistoryWindow(t *testing.T) {
	now := time.Date(2026, 11, 20, 15, 40, 0, 0, time.UTC)
	repair := []et.UpdateParam{{Type: et.UpdateParamTypeNodeRepairEnabled, Value: sdk.String("false")}}
	size := []et.UpdateParam{{Type: et.UpdateParamTypeDesiredSize, Value: sdk.String("2")}}
	for _, tc := range []struct {
		name   string
		age    time.Duration
		status et.UpdateStatus
		params []et.UpdateParam
		code   string // empty: settled
		text   string
	}{
		{"repair change 10 minutes ago", 10 * time.Minute, et.UpdateStatusSuccessful, repair, cloud.HoldRecentChange,
			"Node group gpu changed at 15:30 UTC (EKS update: NodeRepairEnabled). KubeNeuron waits 30 minutes after a change and checks again at 16:00 UTC."},
		{"repair change just inside the window", 30*time.Minute - time.Second, et.UpdateStatusSuccessful, repair, cloud.HoldRecentChange, ""},
		{"repair change at the window's end", 30 * time.Minute, et.UpdateStatusSuccessful, repair, "", ""},
		{"failed change still opens the window", time.Minute, et.UpdateStatusFailed, repair, cloud.HoldRecentChange, ""},
		// add-first's own scale-out must not hold the next replacement in the pool
		{"our own size change a minute ago", time.Minute, et.UpdateStatusSuccessful, size, "", ""},
		{"size change with another parameter", time.Minute, et.UpdateStatusSuccessful, append(append([]et.UpdateParam{}, size...), repair...), cloud.HoldRecentChange, ""},
		{"size change still running", time.Minute, et.UpdateStatusInProgress, size, cloud.HoldUpdateRunning,
			"An EKS update of node group gpu (DesiredSize) is still in progress. KubeNeuron checks again every minute."},
		{"old change", time.Hour, et.UpdateStatusSuccessful, repair, "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			created := now.Add(-tc.age)
			u := et.Update{Id: sdk.String("u1"), CreatedAt: &created, Status: tc.status, Type: et.UpdateTypeConfigUpdate, Params: tc.params}
			_, err := checkUpdateHistory(t.Context(), oneUpdate{u}, nil, "cluster", "gpu", now)
			var hold *cloud.RecoveryHold
			if tc.code == "" {
				if err != nil {
					t.Fatalf("settled group held: %v", err)
				}
				return
			}
			if !errors.As(err, &hold) || hold.Code != tc.code {
				t.Fatalf("want hold %s, got %v", tc.code, err)
			}
			if tc.text != "" && hold.Message() != tc.text {
				t.Fatalf("message:\n got %q\nwant %q", hold.Message(), tc.text)
			}
			if strings.Contains(hold.Message(), "u1") {
				t.Fatal("the message names the update id")
			}
		})
	}
	future := now.Add(time.Minute)
	if _, err := checkUpdateHistory(t.Context(), oneUpdate{et.Update{Id: sdk.String("u1"), CreatedAt: &future, Status: et.UpdateStatusSuccessful}}, nil, "cluster", "gpu", now); err == nil {
		t.Fatal("an update from the future was accepted")
	}
}

// history is a group's update history that counts the updates it describes.
type history struct {
	updates   map[string]et.Update
	described map[string]int
	throttle  bool
}

func (h *history) ListUpdates(_ context.Context, in *eks.ListUpdatesInput, _ ...func(*eks.Options)) (*eks.ListUpdatesOutput, error) {
	var ids []string
	for id := range h.updates {
		ids = append(ids, id)
	}
	// one update per page
	i := 0
	if in.NextToken != nil {
		i = int(sdk.ToString(in.NextToken)[0] - '0')
	}
	slices.Sort(ids)
	out := &eks.ListUpdatesOutput{UpdateIds: ids[i : i+1]}
	if i+1 < len(ids) {
		out.NextToken = sdk.String(string(rune('0' + i + 1)))
	}
	return out, nil
}
func (h *history) DescribeUpdate(_ context.Context, in *eks.DescribeUpdateInput, _ ...func(*eks.Options)) (*eks.DescribeUpdateOutput, error) {
	if h.throttle {
		return nil, &smithy.GenericAPIError{Code: "ThrottlingException", Message: "Rate exceeded"}
	}
	id := sdk.ToString(in.UpdateId)
	h.described[id]++
	u := h.updates[id]
	return &eks.DescribeUpdateOutput{Update: &u}, nil
}

func TestEKSUpdateCacheDescribesOnlyNewOrUnfinished(t *testing.T) {
	now := time.Date(2026, 11, 20, 15, 40, 0, 0, time.UTC)
	old := now.Add(-time.Hour)
	size := []et.UpdateParam{{Type: et.UpdateParamTypeDesiredSize, Value: sdk.String("3")}}
	h := &history{described: map[string]int{}, updates: map[string]et.Update{
		"a": {Id: sdk.String("a"), CreatedAt: &old, Status: et.UpdateStatusSuccessful, Type: et.UpdateTypeVersionUpdate},
		"b": {Id: sdk.String("b"), CreatedAt: &old, Status: et.UpdateStatusFailed, Type: et.UpdateTypeConfigUpdate, Params: size},
		"c": {Id: sdk.String("c"), CreatedAt: &old, Status: et.UpdateStatusInProgress, Type: et.UpdateTypeConfigUpdate, Params: size},
	}}
	var cache updateCache
	check := func() error {
		_, err := checkUpdateHistory(t.Context(), h, &cache, "cluster", "gpu", now)
		return err
	}
	var hold *cloud.RecoveryHold
	if err := check(); !errors.As(err, &hold) || hold.Code != cloud.HoldUpdateRunning {
		t.Fatalf("first check: %v", err)
	}
	c := h.updates["c"]
	c.Status = et.UpdateStatusSuccessful
	h.updates["c"] = c
	if err := check(); err != nil {
		t.Fatalf("second check: %v", err)
	}
	h.updates["d"] = et.Update{Id: sdk.String("d"), CreatedAt: &old, Status: et.UpdateStatusSuccessful, Type: et.UpdateTypeConfigUpdate, Params: size}
	if err := check(); err != nil {
		t.Fatalf("third check: %v", err)
	}
	// a and b were terminal when first read; c until its second read; d is new
	if want := map[string]int{"a": 1, "b": 1, "c": 2, "d": 1}; !maps.Equal(h.described, want) {
		t.Fatalf("described %v, want %v", h.described, want)
	}
}

func TestEKSUpdateThrottlingIsAFailedRead(t *testing.T) {
	r, _, ref := repairFixture(t, nil, "")
	old := time.Now().Add(-time.Hour)
	r.eks = &groupWithHistory{r.eks.(*groupHistory), &history{throttle: true, updates: map[string]et.Update{"a": {Id: sdk.String("a"), CreatedAt: &old}}}}
	_, err := r.OpenRepair(t.Context(), ref)
	var hold *cloud.RecoveryHold
	want := "KubeNeuron could not read node group gpu: eks:DescribeUpdate failed. KubeNeuron retries every minute."
	if !errors.As(err, &hold) || hold.Code != cloud.HoldReadFailed || hold.Message() != want || strings.Contains(hold.Message(), "Rate exceeded") {
		t.Fatalf("got %v", err)
	}
}

// groupWithHistory is a node group with a given update history.
type groupWithHistory struct {
	*groupHistory
	*history
}

func (g *groupWithHistory) ListUpdates(ctx context.Context, in *eks.ListUpdatesInput, o ...func(*eks.Options)) (*eks.ListUpdatesOutput, error) {
	return g.history.ListUpdates(ctx, in, o...)
}
func (g *groupWithHistory) DescribeUpdate(ctx context.Context, in *eks.DescribeUpdateInput, o ...func(*eks.Options)) (*eks.DescribeUpdateOutput, error) {
	return g.history.DescribeUpdate(ctx, in, o...)
}

func TestEKSUpdateHoldAcrossMidnightNamesTheDate(t *testing.T) {
	now := time.Date(2026, 11, 20, 23, 50, 0, 0, time.UTC)
	created := now.Add(-10 * time.Minute)
	u := et.Update{Id: sdk.String("u1"), CreatedAt: &created, Status: et.UpdateStatusSuccessful, Type: et.UpdateTypeVersionUpdate,
		Params: []et.UpdateParam{{Type: et.UpdateParamTypeReleaseVersion, Value: sdk.String("1.33")}}}
	_, err := checkUpdateHistory(t.Context(), oneUpdate{u}, nil, "cluster", "gpu", now)
	want := "Node group gpu changed at 23:40 UTC (EKS update: ReleaseVersion). KubeNeuron waits 30 minutes after a change and checks again at 2026-11-21 00:10 UTC."
	var hold *cloud.RecoveryHold
	if !errors.As(err, &hold) || hold.Message() != want {
		t.Fatalf("got %v", err)
	}
}
