package sqlite

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/kubeneuron/kubeneuron/internal/store"
	"github.com/kubeneuron/kubeneuron/pkg/types"
)

// This file covers the device-identity half of an incident's key: the PCI
// address an incident is opened with when nothing can resolve a GPU UUID, and
// the promotion of such an incident onto the UUID a later, precise signal
// carries.
//
// Both behaviours exist because of one operational failure. A kernel fault
// that knocks a GPU off the bus can name the device only by bus address, so
// the incident it opens has an empty GPU UUID — and an empty GPU UUID is read
// downstream as a PERMANENT infeasibility (errResetTargetUnattributed). By the
// time the ladder reaches the reset rung it has already cordoned the node and
// drained every tenant job off it, so an incident that cannot be attributed is
// a machine emptied of paying work and handed to a human.

func unattributedIncident(id, node, pci string, class types.ProblemClass, opened time.Time) *types.Incident {
	return &types.Incident{
		ID:             id,
		Target:         types.Target{Node: node, GPUIndex: -1, PCIAddr: pci},
		Class:          class,
		State:          types.StateOpen,
		SignalSeen:     1,
		OpenedAt:       opened,
		UpdatedAt:      opened,
		StateChangedAt: opened,
	}
}

// TestTwoUnattributedGPUsOnOneNodeKeepSeparateIncidents is the regression test
// for the incident key being unable to tell two broken devices apart. Both GPUs
// fall off the same node's bus with the same problem class, so both incidents
// carry an empty GPU UUID and differ ONLY in their bus address. Keyed without
// that address they collide: the second GPU's fault is folded into the first
// GPU's incident, and the fleet never records — let alone remediates — that a
// second device failed.
func TestTwoUnattributedGPUsOnOneNodeKeepSeparateIncidents(t *testing.T) {
	s, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	ctx := context.Background()
	now := time.Now()

	first := unattributedIncident("inc-3b", "node-a", "0000:3b:00", types.ClassFellOffBus, now)
	if err := s.CreateIncident(ctx, first); err != nil {
		t.Fatalf("opening the incident for the GPU at 0000:3b:00: %v", err)
	}
	second := unattributedIncident("inc-86", "node-a", "0000:86:00", types.ClassFellOffBus, now.Add(time.Second))
	if err := s.CreateIncident(ctx, second); err != nil {
		t.Fatalf("opening a second incident for the DIFFERENT GPU at 0000:86:00 was refused (%v): two distinct "+
			"devices that fell off one node's bus are being treated as one failure, so the second GPU is never "+
			"remediated and never reported broken", err)
	}

	// Each address must find its OWN incident, or a later signal for one device
	// drives the other device's remediation.
	for _, tc := range []struct{ pci, want string }{
		{"0000:3b:00", "inc-3b"},
		{"0000:86:00", "inc-86"},
		// The same slots as another source spells them.
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

// TestPreciseSignalPromotesTheIncidentItsKernelFaultOpened is the regression
// test for the precise signal being discarded. The incident opened from the
// kernel fault can be addressed only by bus address; when the vendor tool
// resolves that same address to a real UUID, the store must hand back that
// incident as a promotion candidate and let it be promoted in place.
func TestPreciseSignalPromotesTheIncidentItsKernelFaultOpened(t *testing.T) {
	s, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	ctx := context.Background()
	now := time.Now()

	inc := unattributedIncident("inc-fob", "node-a", "0000:3b:00", types.ClassFellOffBus, now)
	if err := s.CreateIncident(ctx, inc); err != nil {
		t.Fatal(err)
	}

	// The precise signal: same slot, spelled the way nvidia-smi prints it, now
	// carrying the device's real identity.
	precise := types.Target{Node: "node-a", GPUUUID: "GPU-abc", GPUIndex: 3, PCIAddr: "00000000:3B:00.0"}

	var promoted *types.Incident
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
		promoted = candidate
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
	if promoted.Target.GPUUUID != "GPU-abc" {
		t.Fatal("PromoteIncidentTarget left the caller's in-memory incident unattributed, so the very transaction " +
			"that identified the device would go on to write the old identity back")
	}

	// From here the incident is reachable by BOTH identities: by UUID for the
	// vendor tool, and still by bus address for the kernel, whose next fault
	// line for that slot must join this incident rather than open another.
	byUUID, err := s.GetOpenIncident(ctx, types.Target{Node: "node-a", GPUUUID: "GPU-abc"}, types.ClassFellOffBus)
	if err != nil || byUUID.ID != inc.ID {
		t.Fatalf("after promotion, lookup by UUID = %v, %v; want %s", byUUID, err, inc.ID)
	}
	byPCI, err := s.GetOpenIncident(ctx, types.Target{Node: "node-a", PCIAddr: "0000:3b:00"}, types.ClassFellOffBus)
	if err != nil || byPCI.ID != inc.ID {
		t.Fatalf("after promotion, a further kernel fault at 0000:3b:00 found %v, %v; want %s: it would otherwise "+
			"open a second incident and cordon the already-cordoned node again", byPCI, err, inc.ID)
	}
}

// TestPromotionHappensExactlyOnceUnderConcurrentPreciseSignals pins the
// atomicity rule. Two precise signals for one device can be ingested at the
// same moment (the kmsg watcher and the vendor poll both resolve it, or the
// event outbox replays). Promotion is a change of an incident's IDENTITY while
// a playbook may be running against it, so it must happen exactly once: the
// second attempt must be refused with a conflict the caller retries, never
// silently applied on top of the first.
func TestPromotionHappensExactlyOnceUnderConcurrentPreciseSignals(t *testing.T) {
	s, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	ctx := context.Background()

	inc := unattributedIncident("inc-race", "node-a", "0000:3b:00", types.ClassFellOffBus, time.Now())
	if err := s.CreateIncident(ctx, inc); err != nil {
		t.Fatal(err)
	}

	// Two writers each read the row before either has written: the same
	// snapshot, the same version.
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
	if stored.Target.GPUUUID != "GPU-abc" || stored.Target.GPUIndex != 3 {
		t.Fatalf("the incident ended up targeted at %+v, want the identity the first promotion committed "+
			"(GPU-abc, index 3)", stored.Target)
	}
}

// TestPromotionIsRefusedOnceTheIncidentIsTerminal keeps automation from
// re-identifying an incident it has already finished with. A resolved or
// expired incident has had its slots released and its report written; moving
// its target afterwards would rewrite history and could re-open the
// (node, uuid, class) slot a live incident holds.
func TestPromotionIsRefusedOnceTheIncidentIsTerminal(t *testing.T) {
	s, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	ctx := context.Background()

	inc := unattributedIncident("inc-done", "node-a", "0000:3b:00", types.ClassFellOffBus, time.Now())
	if err := s.CreateIncident(ctx, inc); err != nil {
		t.Fatal(err)
	}
	inc.State = types.StateResolved
	resolved := time.Now()
	inc.ResolvedAt = &resolved
	if err := s.UpdateIncident(ctx, inc); err != nil {
		t.Fatal(err)
	}
	err = s.PromoteIncidentTarget(ctx, inc,
		types.Target{Node: "node-a", GPUUUID: "GPU-abc", GPUIndex: 3, PCIAddr: "0000:3b:00"})
	if !errors.Is(err, store.ErrConflict) {
		t.Fatalf("promoting a RESOLVED incident returned %v, want ErrConflict: automation has finished with this "+
			"incident, and re-targeting it now can collide with the open incident of the device it names", err)
	}
}

// TestAttributedSignalLookupIsUnchanged is the do-no-harm test. Everything
// above changes how an UNATTRIBUTED incident is addressed; a signal that
// arrives with a GPU UUID must behave exactly as it always has, including
// keeping at most one open incident per (node, GPU, class) and never being
// redirected by a bus address.
func TestAttributedSignalLookupIsUnchanged(t *testing.T) {
	s, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	ctx := context.Background()
	now := time.Now()

	inc := testIncident("inc-attributed", now) // node-a / GPU-1 / ecc-dbe
	if err := s.CreateIncident(ctx, inc); err != nil {
		t.Fatal(err)
	}
	// A second open incident for the same attributed device and class is still
	// refused: that guarantee is what stops two ladders driving one GPU.
	dup := testIncident("inc-attributed-2", now)
	if err := s.CreateIncident(ctx, dup); err == nil {
		t.Fatal("a second open incident for (node-a, GPU-1, ecc-dbe) was accepted: two playbooks can now reset " +
			"the same GPU at the same time")
	}
	// The UUID decides, whatever bus address happens to ride along.
	got, err := s.GetOpenIncident(ctx,
		types.Target{Node: "node-a", GPUUUID: "GPU-1", PCIAddr: "0000:99:00"}, types.ClassECCDBE)
	if err != nil || got.ID != inc.ID {
		t.Fatalf("an attributed signal found %v, %v; want %s: a signal that names its own device must never be "+
			"redirected by a bus address", got, err, inc.ID)
	}
	// And a DIFFERENT attributed device on the same node is its own incident,
	// even when it shares the bus address of no incident at all.
	other, err := s.GetOpenIncident(ctx, types.Target{Node: "node-a", GPUUUID: "GPU-2"}, types.ClassECCDBE)
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("a signal for GPU-2 found %v, %v; want ErrNotFound", other, err)
	}
}

// TestUnattributedIncidentWithoutABusAddressKeepsTheOldRule covers the rows
// that predate the pci_addr column and the signals that name no device at all
// (an alert, a manual operator trigger). They must still find each other, or a
// rolling upgrade opens a duplicate incident for every fault already in flight
// and cordons those nodes twice.
func TestUnattributedIncidentWithoutABusAddressKeepsTheOldRule(t *testing.T) {
	s, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	ctx := context.Background()
	now := time.Now()

	legacy := unattributedIncident("inc-legacy", "node-a", "", types.ClassFellOffBus, now)
	if err := s.CreateIncident(ctx, legacy); err != nil {
		t.Fatal(err)
	}
	nodeScoped, err := s.GetOpenIncident(ctx, types.Target{Node: "node-a"}, types.ClassFellOffBus)
	if err != nil || nodeScoped.ID != legacy.ID {
		t.Fatalf("a node-scoped signal found %v, %v; want %s", nodeScoped, err, legacy.ID)
	}
	withPCI, err := s.GetOpenIncident(ctx, types.Target{Node: "node-a", PCIAddr: "0000:3b:00"}, types.ClassFellOffBus)
	if err != nil || withPCI.ID != legacy.ID {
		t.Fatalf("a bus-addressed signal found %v, %v; want the already-open device-less incident %s: opening a "+
			"second incident for a fault already being remediated cordons the same node twice",
			withPCI, err, legacy.ID)
	}
}
