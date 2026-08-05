package detect

import (
	"sort"
	"strings"
	"testing"

	"github.com/kubeneuron/kubeneuron/pkg/types"
)

// TestEveryXIDReachableClassIsFaultReachable is the §2.3 audit, and it is the
// deliverable — more than the rows it guards. The XID catalog is rich and the
// neutral catalog started with two rows, so a class could be actionable when an
// NVIDIA kernel line reported it and silently unreachable when any other source
// described the same condition. A vendor-neutral catalog whose classes can only
// be entered through the NVIDIA encoding is neutral in form only.
//
// This fails the build the moment a new XID introduces a class with no neutral
// (vendor, code) row — which is exactly when the gap is cheap to close.
func TestEveryXIDReachableClassIsFaultReachable(t *testing.T) {
	faultClasses := map[types.ProblemClass][]FaultInfo{}
	for _, f := range FaultTable() {
		faultClasses[f.Class] = append(faultClasses[f.Class], f)
	}

	var missing []string
	for _, x := range XIDTable() {
		if len(faultClasses[x.Class]) == 0 {
			missing = append(missing, string(x.Class)+" (XID "+x.Name+")")
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		t.Fatalf("classes reachable by XID but not by any neutral (vendor, code) fault: %s\n"+
			"add a row to faultTable in internal/detect/fault.go — a class only NVIDIA's "+
			"encoding can enter is not vendor-neutral", strings.Join(missing, ", "))
	}
}

// TestNeutralFaultSeverityMatchesXIDSeverityPerClass stops the coverage rows
// from being cosmetic. A neutral row could satisfy the reachability audit above
// while quietly downgrading an uncontained ECC error to a warning, so the same
// physical fault would page when NVIDIA's kernel printed it and not when a
// second source observed it. Severity is per class in the XID catalog, so the
// neutral rows must agree with it.
func TestNeutralFaultSeverityMatchesXIDSeverityPerClass(t *testing.T) {
	xidSeverity := map[types.ProblemClass]types.Severity{}
	for _, x := range XIDTable() {
		if prev, ok := xidSeverity[x.Class]; ok && prev != x.Severity {
			t.Fatalf("XID catalog itself disagrees on severity for class %s (%s vs %s); "+
				"this test's premise is broken, fix the catalog or the test", x.Class, prev, x.Severity)
		}
		xidSeverity[x.Class] = x.Severity
	}
	for _, f := range FaultTable() {
		want, constrained := xidSeverity[f.Class]
		if !constrained {
			// A class no XID reaches (thermal, pcie, gpu-lost) has no XID
			// severity to agree with; the fault row is the only definition.
			continue
		}
		if f.Severity != want {
			t.Fatalf("neutral fault %s/%s classifies to %s at severity %s, but the XID encoding of "+
				"that class is %s: one physical fault must not change urgency with its encoding",
				f.Vendor, f.Code, f.Class, f.Severity, want)
		}
	}
}

// TestFaultTableRowsAreSelfConsistent guards the hand-maintained map: a key
// that disagrees with its value's Vendor/Code makes ClassifyFault lookups
// succeed under one spelling and silently miss under the other.
func TestFaultTableRowsAreSelfConsistent(t *testing.T) {
	for key, info := range faultTable {
		if key.Vendor != info.Vendor || key.Code != info.Code {
			t.Errorf("faultTable key %+v does not match its value %+v", key, info)
		}
		if info.Name == "" || info.Class == "" || info.Severity == "" {
			t.Errorf("faultTable row %+v is incomplete", info)
		}
		if got, ok := ClassifyFault(key.Vendor, key.Code); !ok || got.Class != info.Class {
			t.Errorf("ClassifyFault(%q, %q) = %+v, %v; want the table row", key.Vendor, key.Code, got, ok)
		}
	}
}

// TestAMDFaultsClassifyWithoutNewProblemClasses pins the constraint the AMD
// source was built under: every AMD code lands in a class the NVIDIA-era
// catalog already had. A new class here would mean new policy bindings in
// configs/policies.yaml and a new playbook per vendor — the fork this design
// exists to avoid.
func TestAMDFaultsClassifyWithoutNewProblemClasses(t *testing.T) {
	// The vocabulary as pkg/types declared it before any AMD source existed.
	// It is spelled out rather than derived so that inventing a class to fit an
	// AMD condition fails here instead of quietly widening the set — every
	// entry costs a policy binding in configs/policies.yaml and a playbook.
	declared := map[types.ProblemClass]bool{
		types.ClassXIDApp: true, types.ClassECCDBE: true, types.ClassECCSBERate: true,
		types.ClassECCContained: true, types.ClassRowRemapOK: true, types.ClassRowRemapFailure: true,
		types.ClassRowRemapBudget: true, types.ClassNVLink: true, types.ClassFellOffBus: true,
		types.ClassGSPError: true, types.ClassThermal: true, types.ClassPower: true,
		types.ClassPCIe: true, types.ClassDriverHang: true, types.ClassGPULost: true,
		types.ClassDiagFailure: true, types.ClassAgentDown: true,
	}
	for _, f := range FaultTable() {
		if f.Vendor != "amd" {
			continue
		}
		if !declared[f.Class] {
			t.Errorf("AMD fault %q introduces problem class %q, which is not part of the pre-AMD "+
				"vocabulary; AMD codes must reuse the existing classes", f.Code, f.Class)
		}
	}
}
