// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"encoding/json"
	"testing"

	"github.com/kubeneuron/kubeneuron/internal/cloud/capacityreason"
	"github.com/kubeneuron/kubeneuron/pkg/cloud"
	"github.com/kubeneuron/kubeneuron/pkg/types"
)

func TestGrowCapacityRecordCompatibility(t *testing.T) {
	const oldJSON = `{"version":1,"operation":"attempt-1","state":{"grow_failed":"capacity"}}`
	var rec types.RemoveFirstRecord
	if err := json.Unmarshal([]byte(oldJSON), &rec); err != nil {
		t.Fatal(err)
	}
	if capacityreason.Known(rec.CapacityReason) != capacityreason.Unspecified {
		t.Fatal(rec.CapacityReason)
	}
	for _, reason := range []string{"", string(capacityreason.Quota)} {
		rec.CapacityReason = reason
		data, err := json.Marshal(rec)
		if err != nil {
			t.Fatal(err)
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(data, &fields); err != nil {
			t.Fatal(err)
		}
		if _, ok := fields["capacity_reason"]; ok != (reason != "") {
			t.Fatalf("field presence: %s", data)
		}
		var old struct {
			cloud.RemoveFirstPlan
			Version int `json:"version"`
		}
		if err := json.Unmarshal(data, &old); err != nil {
			t.Fatal(err)
		}
		if old.Version != 1 || old.Operation != "attempt-1" || old.State["grow_failed"] != "capacity" {
			t.Fatalf("old reader: %+v", old)
		}
		var current types.RemoveFirstRecord
		if err := json.Unmarshal(data, &current); err != nil || current.CapacityReason != reason {
			t.Fatalf("current reader: %q, %v", current.CapacityReason, err)
		}
	}
}
