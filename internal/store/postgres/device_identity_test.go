package postgres

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/kubeneuron/kubeneuron/internal/store"
	"github.com/kubeneuron/kubeneuron/pkg/types"
)

// The PostgreSQL half of the device-identity conformance suite (the SQLite
// original is internal/store/sqlite/device_identity_test.go). It is mirrored
// rather than shared because the parts that can differ between the engines are
// exactly the parts under test: the two PARTIAL unique indexes that let two
// unattributed GPUs on one node keep two incidents, and the conditional UPDATE
// that promotes an incident onto a device exactly once. A promotion rule that
// held on SQLite and not on Postgres would mean a multi-writer HA install
// silently kept the defect: a node cordoned, drained and parked for a human
// with "reset target unattributed" for a device the vendor tool had named.

func unattributedIncident(id, node, pci string, class types.ProblemClass) *types.Incident {
	now := time.Now()
	return &types.Incident{
		ID:             id,
		Target:         types.Target{Node: node, GPUIndex: -1, PCIAddr: pci},
		Class:          class,
		State:          types.StateOpen,
		SignalSeen:     1,
		OpenedAt:       now,
		UpdatedAt:      now,
		StateChangedAt: now,
	}
}

func TestPostgresTwoUnattributedGPUsOnOneNodeKeepSeparateIncidents(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	if err := s.CreateIncident(ctx, unattributedIncident("inc-3b", "node-a", "0000:3b:00", types.ClassFellOffBus)); err != nil {
		t.Fatalf("opening the incident for the GPU at 0000:3b:00: %v", err)
	}
	if err := s.CreateIncident(ctx, unattributedIncident("inc-86", "node-a", "0000:86:00", types.ClassFellOffBus)); err != nil {
		t.Fatalf("opening a second incident for the DIFFERENT GPU at 0000:86:00 was refused (%v): two distinct "+
			"devices that fell off one node's bus are being treated as one failure, so the second GPU is never "+
			"remediated and never reported broken", err)
	}
	for _, tc := range []struct{ pci, want string }{
		{"0000:3b:00", "inc-3b"},
		{"00000000:3B:00.0", "inc-3b"},
		{"86:00", "inc-86"},
	} {
		got, err := s.GetOpenIncident(ctx, types.Target{Node: "node-a", PCIAddr: tc.pci}, types.ClassFellOffBus)
		if err != nil {
			t.Fatalf("looking up the open incident for %s: %v", tc.pci, err)
		}
		if got.ID != tc.want {
			t.Fatalf("a signal from the GPU at %s joined incident %s, want %s: the fault of one device is being "+
				"attached to another device's incident, and every remediation decision that follows names the "+
				"wrong GPU", tc.pci, got.ID, tc.want)
		}
	}
}

func TestPostgresPreciseSignalPromotesTheIncidentItsKernelFaultOpened(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	inc := unattributedIncident("inc-fob", "node-a", "0000:3b:00", types.ClassFellOffBus)
	if err := s.CreateIncident(ctx, inc); err != nil {
		t.Fatal(err)
	}
	precise := types.Target{Node: "node-a", GPUUUID: "GPU-abc", GPUIndex: 3, PCIAddr: "00000000:3B:00.0"}

	if err := s.WithTx(ctx, func(tx store.Tx) error {
		candidate, err := tx.GetOpenIncident(ctx, precise, types.ClassFellOffBus)
		if err != nil {
			return err
		}
		if candidate.ID != inc.ID {
			t.Fatalf("the precise signal found incident %q, want %q: it did not recognize the incident already "+
				"open for its own bus address, so it opens a SECOND incident and the first one stays "+
				"unattributed forever", candidate.ID, inc.ID)
		}
		return tx.PromoteIncidentTarget(ctx, candidate, precise)
	}); err != nil {
		t.Fatalf("promoting the incident onto the device the vendor tool named: %v", err)
	}

	stored, err := s.GetIncident(ctx, inc.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Target.GPUUUID != "GPU-abc" || stored.Target.GPUIndex != 3 {
		t.Fatalf("after the precise signal the incident is still targeted at %+v: the node has been cordoned and "+
			"drained by now, and an empty GPU UUID makes the reset permanently infeasible, so this node is parked "+
			"for a human although the exact device was identified seconds after the fault", stored.Target)
	}
	if stored.Target.PCIAddr != "0000:3b:00" {
		t.Fatalf("promotion dropped the bus address (%q): a later kernel fault at that address would no longer "+
			"find this incident and would cordon the same node a second time", stored.Target.PCIAddr)
	}
	byPCI, err := s.GetOpenIncident(ctx, types.Target{Node: "node-a", PCIAddr: "0000:3b:00"}, types.ClassFellOffBus)
	if err != nil || byPCI.ID != inc.ID {
		t.Fatalf("after promotion, a further kernel fault at 0000:3b:00 found %v, %v; want %s", byPCI, err, inc.ID)
	}
}

func TestPostgresPromotionHappensExactlyOnceUnderConcurrentPreciseSignals(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	inc := unattributedIncident("inc-race", "node-a", "0000:3b:00", types.ClassFellOffBus)
	if err := s.CreateIncident(ctx, inc); err != nil {
		t.Fatal(err)
	}
	firstSnapshot, err := s.GetIncident(ctx, inc.ID)
	if err != nil {
		t.Fatal(err)
	}
	secondSnapshot, err := s.GetIncident(ctx, inc.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.PromoteIncidentTarget(ctx, firstSnapshot,
		types.Target{Node: "node-a", GPUUUID: "GPU-abc", GPUIndex: 3, PCIAddr: "0000:3b:00"}); err != nil {
		t.Fatalf("the first promotion must succeed: %v", err)
	}
	err = s.PromoteIncidentTarget(ctx, secondSnapshot,
		types.Target{Node: "node-a", GPUUUID: "GPU-other", GPUIndex: 4, PCIAddr: "0000:3b:00"})
	if !errors.Is(err, store.ErrConflict) {
		t.Fatalf("the second promotion returned %v, want ErrConflict: a promotion that overwrites an identity "+
			"another writer already decided can point a running reset at a different device than the one the "+
			"incident was reasoned about with", err)
	}
	stored, err := s.GetIncident(ctx, inc.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Target.GPUUUID != "GPU-abc" {
		t.Fatalf("the incident ended up targeted at %+v, want the identity the first promotion committed", stored.Target)
	}
}

// TestPostgresPromotionOntoAnAlreadyBusyDeviceIsRefusedByTheIndex pins the last
// guard, which is the database's and not the promotion function's. If an open
// incident of this class already exists for the resolved UUID, promoting a
// second one onto it would put two ladders on one GPU. The partial unique index
// must refuse it, so the transaction aborts loudly and is retried rather than
// committing a second open incident for one device.
func TestPostgresPromotionOntoAnAlreadyBusyDeviceIsRefusedByTheIndex(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	existing := &types.Incident{
		ID: "inc-known", Target: types.Target{Node: "node-a", GPUUUID: "GPU-abc", GPUIndex: 3},
		Class: types.ClassFellOffBus, State: types.StateOpen, SignalSeen: 1,
		OpenedAt: time.Now(), UpdatedAt: time.Now(), StateChangedAt: time.Now(),
	}
	if err := s.CreateIncident(ctx, existing); err != nil {
		t.Fatal(err)
	}
	orphan := unattributedIncident("inc-orphan", "node-a", "0000:3b:00", types.ClassFellOffBus)
	if err := s.CreateIncident(ctx, orphan); err != nil {
		t.Fatal(err)
	}
	err := s.PromoteIncidentTarget(ctx, orphan,
		types.Target{Node: "node-a", GPUUUID: "GPU-abc", GPUIndex: 3, PCIAddr: "0000:3b:00"})
	if err == nil {
		t.Fatal("promoting onto a GPU that already has an open incident of this class was accepted: two " +
			"incidents now drive one device, and each of them can independently cordon, drain and reset it")
	}
	if errors.Is(err, store.ErrNotFound) {
		t.Fatalf("promotion failed with %v, want a constraint failure: the caller must be able to retry", err)
	}
}
