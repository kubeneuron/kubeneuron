// SPDX-License-Identifier: Apache-2.0

package postgres

import (
	"context"
	"testing"
	"time"

	"github.com/kubeneuron/kubeneuron/pkg/types"
)

func TestRecentFaultEvents(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	write := func(id, vendor string, at time.Time) {
		t.Helper()
		ev := &types.AgentEvent{EventID: id, Node: "trn-1", Timestamp: at}
		if vendor != "" {
			ev.Fault = &types.FaultSignal{Vendor: vendor, Source: "src", Code: "c", Attributes: map[string]string{"device": "neuron1"}}
		}
		if _, err := s.WriteEvent(ctx, ev); err != nil {
			t.Fatal(err)
		}
	}
	write("old", "aws", now.Add(-48*time.Hour))
	write("aws-1", "aws", now.Add(-time.Hour))
	write("aws-2", "aws", now.Add(-time.Minute))
	write("amd", "amd", now)
	write("aws-like", "aws-trainium", now)
	write("xid", "", now)

	got, err := s.RecentFaultEvents(ctx, "aws", now.Add(-24*time.Hour), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].EventID != "aws-2" || got[1].EventID != "aws-1" {
		t.Fatalf("events %+v, want aws-2 then aws-1", got)
	}
	if got[0].Fault == nil || got[0].Fault.Attributes["device"] != "neuron1" || !got[0].Timestamp.Equal(now.Add(-time.Minute)) {
		t.Fatalf("event %+v did not survive the archive", got[0])
	}
	if got, _ := s.RecentFaultEvents(ctx, "aws", now.Add(-24*time.Hour), 1); len(got) != 1 || got[0].EventID != "aws-2" {
		t.Fatalf("limit 1: %+v", got)
	}
	if got, _ := s.RecentFaultEvents(ctx, `a%`, now.Add(-24*time.Hour), 10); len(got) != 0 {
		t.Fatalf("a pattern in the vendor matched %+v", got)
	}
}
