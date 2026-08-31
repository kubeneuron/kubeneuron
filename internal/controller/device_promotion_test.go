package controller

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/kubeneuron/kubeneuron/internal/detect"
	"github.com/kubeneuron/kubeneuron/internal/store"
	"github.com/kubeneuron/kubeneuron/pkg/types"
)

// agentEventSignal builds the signal the controller really ingests, through the
// same detect builder the event API uses, so these tests cannot pass on an
// input the real parser would have rejected.
func agentEventSignal(t *testing.T, ev types.AgentEvent) types.Signal {
	t.Helper()
	sig, ok := detect.SignalFromAgentEvent(ev)
	if !ok {
		t.Fatalf("the agent event %+v is not actionable, so this test would prove nothing", ev)
	}
	return sig
}

// TestPreciseSignalPromotesTheUnattributedIncidentItsKernelFaultOpened is the
// end-to-end regression test for the reported defect, walked exactly as it
// happens in production.
//
// XID 79 knocks a GPU off the bus. nvidia-smi no longer lists the device, so
// the agent's kmsg path can report only the bus address, and the controller
// opens an incident with an empty GPU UUID. Two seconds later the DCGM poll
// resolves that same address to GPU-abc.
//
// Before the fix, the precise signal could only open a second incident or be
// thrown away, and the first incident stayed unattributed for life. An empty
// GPU UUID is read by the reset preflight as errResetTargetUnattributed — a
// permanent infeasibility, which is the correct reading of a UUID that can
// never arrive and exactly the wrong outcome when the UUID did arrive. By then
// the fell-off-bus ladder has cordoned the node and drained every tenant job
// off it, so the incident is handed to a human with a machine emptied of paying
// work behind it.
func TestPreciseSignalPromotesTheUnattributedIncidentItsKernelFaultOpened(t *testing.T) {
	c, st := newIngestTestController(t)
	ctx := context.Background()

	// The kernel fault: an address, no device identity.
	kernel := agentEventSignal(t, types.AgentEvent{
		Node: "n1", GPUIndex: -1, GPUUUID: "", PCIAddr: "0000:3b:00", XID: 79,
		Raw: "NVRM: Xid (PCI:0000:3b:00): 79", Timestamp: time.Now(),
	})
	if err := c.ingest(ctx, kernel); err != nil {
		t.Fatal(err)
	}
	incidents, err := st.ListIncidents(ctx, store.IncidentFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(incidents) != 1 || incidents[0].Target.GPUUUID != "" {
		t.Fatalf("after the kernel fault: incidents = %+v, want exactly one, unattributed", incidents)
	}
	openedID := incidents[0].ID

	// The DCGM poll, seconds later: the same slot, now with a device identity.
	// It spells the address the way nvidia-smi does, which is not how the
	// kernel spelled it.
	precise := agentEventSignal(t, types.AgentEvent{
		Node: "n1", GPUIndex: 3, GPUUUID: "GPU-abc", PCIAddr: "00000000:3B:00.0", XID: 79,
		Raw: "DCGM_FI_DEV_XID_ERRORS=79", Timestamp: time.Now(),
	})
	if err := c.ingest(ctx, precise); err != nil {
		t.Fatal(err)
	}

	incidents, err = st.ListIncidents(ctx, store.IncidentFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(incidents) != 1 {
		t.Fatalf("incidents = %d, want 1: the precise observation of a fault already being remediated opened a "+
			"SECOND incident, so two ladders now cordon and drain the same node for one broken GPU", len(incidents))
	}
	inc := incidents[0]
	if inc.ID != openedID {
		t.Fatalf("the surviving incident is %s, want the original %s", inc.ID, openedID)
	}
	if inc.Target.GPUUUID != "GPU-abc" || inc.Target.GPUIndex != 3 {
		t.Fatalf("the incident is still targeted at %+v: the device that failed was identified as GPU-abc two "+
			"seconds after the fault, and dropping that identity means the reset is refused as permanently "+
			"infeasible and the node — cordoned and drained by then — is parked for a human", inc.Target)
	}
	if inc.Target.PCIAddr != "0000:3b:00" {
		t.Fatalf("the incident lost its bus address (%q): the next kernel fault at that slot would no longer "+
			"find it and would cordon the already-cordoned node again", inc.Target.PCIAddr)
	}
	if inc.SignalSeen != 2 {
		t.Fatalf("SignalSeen = %d, want 2: the promoting observation must also be counted as a signal against "+
			"this incident, or the evidence trail loses the very report that identified the device", inc.SignalSeen)
	}

	// The promotion has to be legible to the human who reads the incident,
	// because it changes what every later step of the ladder acts on.
	trail, err := st.AuditTrail(ctx, inc.ID)
	if err != nil {
		t.Fatal(err)
	}
	var promotion *types.AuditEntry
	for _, e := range trail {
		if e.Action == "promote-target" {
			promotion = e
		}
	}
	if promotion == nil {
		t.Fatalf("the audit trail (%d entries) records no promotion: an incident silently changed which physical "+
			"device it is about, and the trail is the only evidence an operator has", len(trail))
	}
	if !strings.Contains(promotion.Result, "GPU-abc") || !strings.Contains(promotion.Result, "0000:3b:00") {
		t.Fatalf("the promotion audit entry reads %q, want it to name both the bus address it was opened with "+
			"and the device it was identified as", promotion.Result)
	}
}

// TestTwoUnattributedGPUsOnOneNodeGetTheirOwnIncidents is the second half of
// the reported defect. Two GPUs on one node fall off the bus with the same
// problem class within moments of each other. Both are unattributed, so before
// the bus address became part of the incident key they merged into ONE
// incident: the second GPU's failure was never remediated, never counted, and
// never visible to anyone reading the incident list.
func TestTwoUnattributedGPUsOnOneNodeGetTheirOwnIncidents(t *testing.T) {
	c, st := newIngestTestController(t)
	ctx := context.Background()

	for _, pci := range []string{"0000:3b:00", "0000:86:00"} {
		if err := c.ingest(ctx, agentEventSignal(t, types.AgentEvent{
			Node: "n1", GPUIndex: -1, PCIAddr: pci, XID: 79,
			Raw: "NVRM: Xid (PCI:" + pci + "): 79", Timestamp: time.Now(),
		})); err != nil {
			t.Fatal(err)
		}
	}
	incidents, err := st.ListIncidents(ctx, store.IncidentFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(incidents) != 2 {
		t.Fatalf("incidents = %d, want 2: two DIFFERENT GPUs failed on this node and were merged into one "+
			"incident, so one of them is never remediated and never reported broken", len(incidents))
	}
	byAddr := map[string]bool{}
	for _, inc := range incidents {
		byAddr[inc.Target.PCIAddr] = true
	}
	if !byAddr["0000:3b:00"] || !byAddr["0000:86:00"] {
		t.Fatalf("the two incidents are addressed %v, want one per failing device", byAddr)
	}

	// Each device is then promoted onto its OWN identity: a precise signal must
	// never land on the neighbour's incident.
	if err := c.ingest(ctx, agentEventSignal(t, types.AgentEvent{
		Node: "n1", GPUIndex: 4, GPUUUID: "GPU-86", PCIAddr: "0000:86:00", XID: 79, Timestamp: time.Now(),
	})); err != nil {
		t.Fatal(err)
	}
	incidents, err = st.ListIncidents(ctx, store.IncidentFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(incidents) != 2 {
		t.Fatalf("incidents = %d, want 2 after the promotion", len(incidents))
	}
	for _, inc := range incidents {
		switch inc.Target.PCIAddr {
		case "0000:86:00":
			if inc.Target.GPUUUID != "GPU-86" {
				t.Fatalf("the incident for 0000:86:00 is targeted at %q, want GPU-86", inc.Target.GPUUUID)
			}
		case "0000:3b:00":
			if inc.Target.GPUUUID != "" {
				t.Fatalf("the incident for the OTHER GPU (0000:3b:00) was targeted at %q by a signal that named "+
					"the device at 0000:86:00: a reset would now be issued against a GPU nobody diagnosed",
					inc.Target.GPUUUID)
			}
		}
	}
}

// TestAttributedSignalIngestIsUnchanged is the do-no-harm test for the path
// that already worked: a signal that arrives WITH a GPU UUID must open and
// join incidents exactly as before, and must never be redirected onto another
// incident by a bus address that happens to ride along.
func TestAttributedSignalIngestIsUnchanged(t *testing.T) {
	c, st := newIngestTestController(t)
	ctx := context.Background()

	first := agentEventSignal(t, types.AgentEvent{
		Node: "n1", GPUIndex: 3, GPUUUID: "GPU-abc", XID: 79, Timestamp: time.Now(),
	})
	if err := c.ingest(ctx, first); err != nil {
		t.Fatal(err)
	}
	// The same device seen again, this time by a source that also knows the
	// bus address. It joins the incident it already has.
	second := agentEventSignal(t, types.AgentEvent{
		Node: "n1", GPUIndex: 3, GPUUUID: "GPU-abc", PCIAddr: "0000:3b:00", XID: 79, Timestamp: time.Now(),
	})
	if err := c.ingest(ctx, second); err != nil {
		t.Fatal(err)
	}
	// A DIFFERENT device on the same node keeps its own incident.
	third := agentEventSignal(t, types.AgentEvent{
		Node: "n1", GPUIndex: 4, GPUUUID: "GPU-def", PCIAddr: "0000:86:00", XID: 79, Timestamp: time.Now(),
	})
	if err := c.ingest(ctx, third); err != nil {
		t.Fatal(err)
	}

	incidents, err := st.ListIncidents(ctx, store.IncidentFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(incidents) != 2 {
		t.Fatalf("incidents = %d, want 2 (one per attributed device)", len(incidents))
	}
	for _, inc := range incidents {
		if inc.Target.GPUUUID == "GPU-abc" && inc.SignalSeen != 2 {
			t.Fatalf("GPU-abc's incident saw %d signals, want 2", inc.SignalSeen)
		}
		trail, err := st.AuditTrail(ctx, inc.ID)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range trail {
			if e.Action == "promote-target" {
				t.Fatalf("incident %s was promoted: a signal that already names its own device must never have "+
					"its identity rewritten, and a promotion here means the target moved under a running ladder",
					inc.ID)
			}
		}
	}
}

// TestPromotionKeepsTheSafetyGateSlotUnderItsStablePCIKey covers the atomicity
// consequence of changing an incident's identity mid-remediation.
//
// PCIAddr is present before and after promotion, so it gives the gate one
// immutable physical identity. Re-keying an in-memory reservation after the
// promotion commits has an unavoidable race with a terminal transition: the
// terminal release can happen before the re-key and the late code then creates
// a fresh slot for an incident that is already over.
func TestPromotionKeepsTheSafetyGateSlotUnderItsStablePCIKey(t *testing.T) {
	c, st := newIngestTestController(t)
	ctx := context.Background()

	if err := c.ingest(ctx, agentEventSignal(t, types.AgentEvent{
		Node: "n1", GPUIndex: -1, PCIAddr: "0000:3b:00", XID: 79, Timestamp: time.Now(),
	})); err != nil {
		t.Fatal(err)
	}
	incidents, err := st.ListIncidents(ctx, store.IncidentFilter{})
	if err != nil {
		t.Fatal(err)
	}
	inc := incidents[0]

	// The ladder admits its first destructive step under the PCI-keyed
	// remediation slot and records that durably.
	unattributed := inc.Target
	if err := c.gate.Allow(unattributed, types.ActionIdleCheck); err != nil {
		t.Fatalf("admitting the first step: %v", err)
	}
	inc.RemediationSlotHeld = true
	if err := st.UpdateIncident(ctx, inc); err != nil {
		t.Fatal(err)
	}

	// Now the precise signal lands, mid-remediation.
	if err := c.ingest(ctx, agentEventSignal(t, types.AgentEvent{
		Node: "n1", GPUIndex: 3, GPUUUID: "GPU-abc", PCIAddr: "0000:3b:00", XID: 79, Timestamp: time.Now(),
	})); err != nil {
		t.Fatal(err)
	}
	promoted, err := st.GetIncident(ctx, inc.ID)
	if err != nil {
		t.Fatal(err)
	}
	if promoted.Target.GPUUUID != "GPU-abc" {
		t.Fatalf("the incident was not promoted: %+v", promoted.Target)
	}

	// The incident terminalizes THROUGH THE CONTROLLER, from the incident as a
	// step goroutine would still be holding it — pre-promotion.
	//
	// This used to call c.gate.ReleaseRemediation(promoted.Target) directly,
	// which is the one thing the production path could not do: the goroutine
	// that terminalizes carries the incident as it was when its step began, so
	// it released the OLD key and left the promoted reservation held forever.
	// The test handed the release the answer and so could not see that.
	stale := *inc
	if err := c.transition(ctx, &stale, types.StateResolved, "system", "verify", "", nil); err != nil {
		t.Fatal(err)
	}

	// The leak is invisible from the gate's API, so it is observed the way the
	// fleet observes it: the cap is 2, and with a stale slot still held only
	// ONE healthy node can start a remediation instead of two.
	if err := c.gate.Allow(types.Target{Node: "n2"}, types.ActionIdleCheck); err != nil {
		t.Fatalf("a healthy node was refused its first remediation slot: %v", err)
	}
	if err := c.gate.Allow(types.Target{Node: "n3"}, types.ActionIdleCheck); err != nil {
		t.Fatalf("a second healthy node was refused a remediation slot (%v) although the only incident holding "+
			"one had already released it: the promotion changed the incident's identity without moving the "+
			"reservation it was admitted under, so the release missed and n1 counts against "+
			"MaxConcurrentRemediations for the lifetime of the process — capacity the fleet never gets back and "+
			"no incident is visibly responsible for", err)
	}
}
