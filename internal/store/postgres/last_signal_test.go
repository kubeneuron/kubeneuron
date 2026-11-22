// SPDX-License-Identifier: Apache-2.0
package postgres

import (
	"context"
	"testing"
	"time"

	"github.com/kubeneuron/kubeneuron/pkg/types"
)

func TestLastSignalAtIsOnlyWhatTheWriterSets(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	opened := time.Now().Add(-time.Hour).Truncate(time.Millisecond)
	inc := &types.Incident{ID: "last-signal", Target: types.Target{Node: "worker"}, Class: types.ClassECCDBE, State: types.StateOpen, OpenedAt: opened, UpdatedAt: opened, StateChangedAt: opened}
	if err := s.CreateIncident(ctx, inc); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetIncident(ctx, inc.ID)
	if err != nil || !got.LastSignalAt.Equal(opened) {
		t.Fatalf("LastSignalAt = %v (%v), want the opening time %v", got.LastSignalAt, err, opened)
	}
	got.UpdatedAt = time.Now()
	if err := s.UpdateIncident(ctx, got); err != nil {
		t.Fatal(err)
	}
	if got, _ = s.GetIncident(ctx, inc.ID); !got.LastSignalAt.Equal(opened) {
		t.Fatalf("a write that is not a signal moved LastSignalAt to %v", got.LastSignalAt)
	}
	signal := opened.Add(30 * time.Minute)
	got.LastSignalAt = signal
	if err := s.UpdateIncident(ctx, got); err != nil {
		t.Fatal(err)
	}
	if got, _ = s.GetIncident(ctx, inc.ID); !got.LastSignalAt.Equal(signal) {
		t.Fatalf("LastSignalAt = %v, want %v", got.LastSignalAt, signal)
	}
	// A writer that does not know the last signal leaves it as it is.
	got.LastSignalAt = time.Time{}
	got.UpdatedAt = time.Now()
	if err := s.UpdateIncident(ctx, got); err != nil {
		t.Fatal(err)
	}
	if got, _ = s.GetIncident(ctx, inc.ID); !got.LastSignalAt.Equal(signal) {
		t.Fatalf("a zero LastSignalAt overwrote the column: %v, want %v", got.LastSignalAt, signal)
	}
}
