// SPDX-License-Identifier: Apache-2.0

package sqlite

import (
	"testing"

	"github.com/kubeneuron/kubeneuron/pkg/types"
)

func TestTrainingWorkersKeepFirstObservation(t *testing.T) {
	s, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	ctx := t.Context()
	for _, id := range []string{"occupied", "empty"} {
		impactIncident(t, s, id)
		if _, err := s.sqlDB.ExecContext(ctx, "UPDATE incidents SET node=? WHERE id=?", id, id); err != nil {
			t.Fatal(err)
		}
		if got, err := s.TrainingWorkers(ctx, id); err != nil || got != nil {
			t.Fatalf("unobserved=%v err=%v", got, err)
		}
	}
	first := []types.TrainingWorker{{Namespace: "ml", Pod: "trainer", PodUID: "uid"}}
	if err := s.SaveTrainingWorkers(ctx, "occupied", first); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"occupied", "empty"} {
		if err := s.SaveTrainingWorkers(ctx, id, []types.TrainingWorker{}); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.TrainingWorkers(ctx, "occupied")
	if err != nil || len(got) != 1 || got[0].PodUID != "uid" {
		t.Fatalf("occupied=%v err=%v", got, err)
	}
	got, err = s.TrainingWorkers(ctx, "empty")
	if err != nil || got == nil || len(got) != 0 {
		t.Fatalf("empty=%v err=%v", got, err)
	}
}
