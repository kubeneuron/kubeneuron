package executor

import (
	"strings"
	"testing"

	"github.com/kubeneuron/kubeneuron/internal/agent/nvml"
)

func stateExecutor(t *testing.T) *Executor {
	t.Helper()
	return NewWithOptions(&nvml.Fake{}, Options{AcceleratorHostStatePath: t.TempDir() + "/accelerator-host-state.json"})
}

// TestSaveAcceleratorHostStatePreservesSnapshotAcrossRetries is the regression
// for a partially-completed quiesce that could never be retried. Attempt 1
// snapshots the PRE-mutation state ({service active, pm enabled}) and then makes
// its mutations; a retry recomputes the now-partially-mutated live state
// ({service inactive}) and saved it against the same key. The old code compared
// the two and refused every retry with "conflicts with an earlier quiesce". The
// stored snapshot is the thing restore must put back, so it must be preserved,
// not overwritten or compared against the live state.
func TestSaveAcceleratorHostStatePreservesSnapshotAcrossRetries(t *testing.T) {
	e := stateExecutor(t)

	preMutation := acceleratorHostState{
		GPUIndex:              0,
		GPUUUID:               "GPU-abc",
		PersistenceKnown:      true,
		PersistenceWasEnabled: true,
		ServiceWasActive:      true,
	}
	if err := e.saveAcceleratorHostState("node-a", preMutation); err != nil {
		t.Fatalf("first save = %v", err)
	}

	// The retry computes a state that reflects the mutations attempt 1 already
	// made: the daemon is now inactive and persistence is now disabled.
	postPartialMutation := acceleratorHostState{
		GPUIndex:              0,
		GPUUUID:               "GPU-abc",
		PersistenceKnown:      true,
		PersistenceWasEnabled: false,
		ServiceWasActive:      false,
	}
	if err := e.saveAcceleratorHostState("node-a", postPartialMutation); err != nil {
		t.Fatalf("retry save must succeed, got %v", err)
	}

	// Restore reads the ORIGINAL pre-mutation snapshot, not the retry's live state.
	got, ok, err := e.loadAcceleratorHostState("node-a")
	if err != nil || !ok {
		t.Fatalf("load = %+v, %v, ok=%t", got, err, ok)
	}
	if got != preMutation {
		t.Fatalf("stored snapshot = %+v, want the pre-mutation snapshot %+v preserved", got, preMutation)
	}
}

// TestSaveAcceleratorHostStateConflictsOnlyOnDifferentDevice proves the retry
// path still refuses a key that has been reused for a genuinely different
// physical GPU, which restore could not honor.
func TestSaveAcceleratorHostStateConflictsOnlyOnDifferentDevice(t *testing.T) {
	e := stateExecutor(t)

	if err := e.saveAcceleratorHostState("node-a", acceleratorHostState{GPUIndex: 0, GPUUUID: "GPU-abc"}); err != nil {
		t.Fatalf("first save = %v", err)
	}
	err := e.saveAcceleratorHostState("node-a", acceleratorHostState{GPUIndex: 1, GPUUUID: "GPU-xyz"})
	if err == nil || !strings.Contains(err.Error(), "conflicts with an earlier quiesce") {
		t.Fatalf("save for a different device = %v, want a conflict", err)
	}
}
