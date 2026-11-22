// SPDX-License-Identifier: Apache-2.0
package postgres

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/kubeneuron/kubeneuron/pkg/types"
)

// The devices an incident covered when it opened are kept as written, and
// an update does not change them.
func TestIncidentAcceleratorsSurvive(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	want := []types.IncidentAccelerator{{Resource: "google.com/tpu", Unit: types.DeviceUnitTPUChip, Count: 4}}
	inc := &types.Incident{ID: "acc", Target: types.Target{Node: "tpu-1"}, Class: types.ClassFellOffBus, State: types.StateOpen,
		OpenedAt: now, UpdatedAt: now, StateChangedAt: now, Accelerators: want}
	bare := &types.Incident{ID: "bare", Target: types.Target{Node: "n"}, Class: types.ClassFellOffBus, State: types.StateOpen,
		OpenedAt: now, UpdatedAt: now, StateChangedAt: now}
	for _, i := range []*types.Incident{inc, bare} {
		if err := s.CreateIncident(ctx, i); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.GetIncident(ctx, inc.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.Accelerators, want) {
		t.Fatalf("accelerators = %+v, want %+v", got.Accelerators, want)
	}
	got.State, got.Accelerators = types.StateEvaluating, nil
	if err := s.UpdateIncident(ctx, got); err != nil {
		t.Fatal(err)
	}
	if got, err = s.GetIncident(ctx, inc.ID); err != nil || !reflect.DeepEqual(got.Accelerators, want) {
		t.Fatalf("after an update: %+v, %v", got.Accelerators, err)
	}
	if got, err = s.GetIncident(ctx, bare.ID); err != nil || got.Accelerators != nil {
		t.Fatalf("an incident without a record: %+v, %v", got.Accelerators, err)
	}
}
