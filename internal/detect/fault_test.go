package detect

import (
	"testing"
	"time"

	"github.com/kubeneuron/kubeneuron/pkg/types"
)

// TestNeutralFaultClassifiesToSameClassAsFormerXID pins the R6 invariant: the
// nvidia-smi fallback's migrated faults classify to exactly the ProblemClass
// their former synthesized XID produced (XID 48 -> ClassECCDBE, XID 64 ->
// ClassRowRemapFailure), so removing the synthesized XID changed the encoding,
// not the outcome.
func TestNeutralFaultClassifiesToSameClassAsFormerXID(t *testing.T) {
	cases := []struct {
		code      string
		wantClass types.ProblemClass
		formerXID int
	}{
		{"ecc-dbe", types.ClassECCDBE, 48},
		{"row-remap-failure", types.ClassRowRemapFailure, 64},
	}
	for _, tc := range cases {
		xidInfo, xidOK := ClassifyXID(tc.formerXID)
		if !xidOK {
			t.Fatalf("former XID %d must still classify", tc.formerXID)
		}
		faultInfo, faultOK := ClassifyFault("nvidia", tc.code)
		if !faultOK {
			t.Fatalf("neutral fault %q must classify", tc.code)
		}
		if faultInfo.Class != tc.wantClass || faultInfo.Class != xidInfo.Class {
			t.Fatalf("fault %q class = %s, XID %d class = %s, want %s",
				tc.code, faultInfo.Class, tc.formerXID, xidInfo.Class, tc.wantClass)
		}
		if faultInfo.Severity != xidInfo.Severity {
			t.Fatalf("fault %q severity = %s, XID %d severity = %s (must match)",
				tc.code, faultInfo.Severity, tc.formerXID, xidInfo.Severity)
		}
	}
}

func TestSignalFromFaultEnvelope(t *testing.T) {
	ev := types.AgentEvent{
		Node: "node07", GPUIndex: 2, GPUUUID: "GPU-xyz",
		Fault: &types.FaultSignal{
			Vendor: "nvidia", Source: "nvidia-smi", Code: "ecc-dbe",
			Attributes: map[string]string{"volatile_uncorrectable_ecc": "2"},
		},
		Raw: "nvidia-smi -q: GPU 2 volatile uncorrectable ECC errors=2", Timestamp: time.Now(),
	}
	sig, ok := SignalFromAgentEvent(ev)
	if !ok {
		t.Fatal("neutral NVIDIA ecc-dbe fault must produce a signal")
	}
	if sig.Class != types.ClassECCDBE || sig.Severity != types.SeverityCritical {
		t.Fatalf("signal = %+v, want ClassECCDBE/critical", sig)
	}
	if sig.Target.Node != "node07" || sig.Target.GPUUUID != "GPU-xyz" {
		t.Fatalf("target = %+v, want node07/GPU-xyz", sig.Target)
	}
	if sig.Evidence["code"] != "ecc-dbe" || sig.Evidence["vendor"] != "nvidia" || sig.Evidence["source"] != "nvidia-smi" {
		t.Fatalf("evidence = %+v, want vendor/source/code recorded", sig.Evidence)
	}
	if sig.Evidence["attr_volatile_uncorrectable_ecc"] != "2" {
		t.Fatalf("evidence = %+v, want fault attributes surfaced", sig.Evidence)
	}
}

// TestUnclassifiedFaultIsNotActionable confirms an unknown vendor/code is
// recorded but opens no incident, mirroring an unknown XID.
func TestUnclassifiedFaultIsNotActionable(t *testing.T) {
	ev := types.AgentEvent{Node: "n1", Fault: &types.FaultSignal{Vendor: "acme", Source: "acme-tool", Code: "mystery"}}
	if _, ok := SignalFromAgentEvent(ev); ok {
		t.Fatal("unknown neutral fault must not be actionable")
	}
	if _, ok := FaultClass(ev); ok {
		t.Fatal("unknown neutral fault must not resolve a ProblemClass")
	}
}

// TestFaultClassUnifiesXIDAndNeutralFault is the cross-source identity the agent
// dedup keys on: a genuine XID and the neutral fault for the same condition must
// resolve to the same ProblemClass.
func TestFaultClassUnifiesXIDAndNeutralFault(t *testing.T) {
	xidEv := types.AgentEvent{Node: "n1", GPUIndex: 0, GPUUUID: "GPU-a", XID: 48}
	faultEv := types.AgentEvent{Node: "n1", GPUIndex: 0, GPUUUID: "GPU-a",
		Fault: &types.FaultSignal{Vendor: "nvidia", Source: "nvidia-smi", Code: "ecc-dbe"}}
	xidClass, ok1 := FaultClass(xidEv)
	faultClass, ok2 := FaultClass(faultEv)
	if !ok1 || !ok2 || xidClass != faultClass {
		t.Fatalf("XID 48 class = %v (%v), ecc-dbe class = %v (%v), want equal", xidClass, ok1, faultClass, ok2)
	}
}
