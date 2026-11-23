// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"cmp"
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/kubeneuron/kubeneuron/internal/recoverymode"
	"github.com/kubeneuron/kubeneuron/internal/store/sqlite"
	"github.com/kubeneuron/kubeneuron/pkg/types"
)

// The reasons an agent with nvidia-smi from the container runtime sent from
// a T4 instance on EKS, before and after the replacement.
var eksT4Reasons = []string{
	"physical-device reset unavailable: GPU 0 cannot be reset: the kernel exposes no PCI reset for device 0000:00:1e.0; on a virtualized instance the hypervisor withholds it and no GPU reset can succeed",
	"DCGM runtime version probe failed: dcgmi --version: fork/exec /usr/bin/dcgmi: no such file or directory",
}

// eksT4Report posts that report's shape for node: degraded, a driver
// version and no runtime version, and no driver health time.
func eksT4Report(t *testing.T, st *sqlite.Store, node, uid string, devices int, edit func(*types.AgentAcceleratorReport)) {
	t.Helper()
	report := types.AgentAcceleratorReport{
		Node: node, NodeUID: uid, Vendor: types.AcceleratorVendorNVIDIA, ObservedAt: time.Now(),
		DriverVersion: "580.178.04", TopologySafety: types.AcceleratorTopologyVerifiedUnpartitioned,
		Capabilities: []types.AgentAcceleratorCapability{}, Readiness: types.AcceleratorReadinessDegraded,
		ReadinessReasons: append([]string(nil), eksT4Reasons...),
	}
	for i := 0; i < devices; i++ {
		report.Devices = append(report.Devices, types.AgentAcceleratorDevice{
			ID: fmt.Sprintf("GPU-%s-%d", node, i), Kind: types.AcceleratorDevicePhysical, Family: types.AcceleratorFamilyGPU, Model: "Tesla T4",
		})
	}
	if edit != nil {
		edit(&report)
	}
	if err := st.UpsertAcceleratorReport(context.Background(), &report); err != nil {
		t.Fatal(err)
	}
}

// A typical install has no DCGM and no runtime profile, and a cloud GPU
// cannot be reset: the old node's agent reported its devices degraded, and
// the new node's report of the same depth stands for them. Fewer devices, no
// devices or a not-ready report go to a person, each with what it says.
func TestReplacementWithAgentsThatCannotAttestTheRuntime(t *testing.T) {
	const (
		inventoryOnly = "Accelerator inventory observed on the new node; DCGM not available to the agent, device health not checked."
		driverProbe   = "Accelerator inventory and driver probe observed; DCGM not available to the agent, device health not checked."
		notAttested   = "Accelerator inventory observed on the new node; runtime not attested, device health not checked."
	)
	for _, tc := range []struct {
		name string
		// old and new are how many devices the old and the new node's
		// agents report.
		old, new int
		edit     func(*types.AgentAcceleratorReport)
		resolved bool
		want     string
	}{
		{name: "no profile", old: 1, new: 1, resolved: true, want: inventoryOnly},
		{name: "a profile that does not match", old: 1, new: 1, resolved: true, want: inventoryOnly,
			edit: func(r *types.AgentAcceleratorReport) {
				r.ProfileDigest, r.ProfileUID, r.ProfileGeneration = "t4-rehearsal", "t4-rehearsal-uid", 1
			}},
		{name: "the agent's own words for no dcgmi", old: 1, new: 1, resolved: true, want: inventoryOnly,
			edit: func(r *types.AgentAcceleratorReport) {
				r.ReadinessReasons[1] = "DCGM is not available to the agent; device health is not checked"
			}},
		{name: "a driver probe", old: 1, new: 1, resolved: true, want: driverProbe,
			edit: func(r *types.AgentAcceleratorReport) {
				at := time.Now()
				r.Devices[0].DriverHealthyAt = &at
			}},
		{name: "DCGM answers, runtime not attested", old: 1, new: 1, resolved: true, want: notAttested,
			edit: func(r *types.AgentAcceleratorReport) {
				r.RuntimeVersion = "dcgm-4.2.3"
				r.ReadinessReasons = []string{eksT4Reasons[0], "NVIDIA runtime profile is missing driver_version or runtime_version"}
			}},
		// The agent's own words when DCGM answered and did not agree: they
		// name DCGM, and DCGM was available.
		{name: "DCGM answers, GPU count differs", old: 1, new: 1, resolved: true, want: notAttested,
			edit: func(r *types.AgentAcceleratorReport) {
				r.ReadinessReasons = []string{eksT4Reasons[0], "DCGM discovery found 2 GPUs, nvidia-smi inventory found 1"}
			}},
		{name: "DCGM answers, version differs from the profile", old: 1, new: 1, resolved: true, want: notAttested,
			edit: func(r *types.AgentAcceleratorReport) {
				r.RuntimeVersion = "dcgm-9.9"
				r.ReadinessReasons = []string{eksT4Reasons[0],
					`locally attested NVIDIA runtime version "dcgm-9.9" from /usr/bin/dcgmi does not match the controller profile "gpu-operator-v24.9.2"`}
			}},
		{name: "DCGM answers, not attested by matching probes", old: 1, new: 1, resolved: true, want: notAttested,
			edit: func(r *types.AgentAcceleratorReport) {
				r.ReadinessReasons = []string{"NVIDIA runtime is not attested by matching local DCGM version and discovery probes"}
			}},
		{name: "DCGM discovery probe failed", old: 1, new: 1, resolved: true, want: inventoryOnly,
			edit: func(r *types.AgentAcceleratorReport) {
				r.ReadinessReasons = []string{eksT4Reasons[0], "DCGM discovery probe failed: dcgmi discovery -l: exit status 255"}
			}},
		{name: "DCGM attestation unavailable", old: 1, new: 1, resolved: true, want: inventoryOnly,
			edit: func(r *types.AgentAcceleratorReport) {
				r.ReadinessReasons = []string{"NVIDIA runtime is not attested by matching local DCGM version and discovery probes", "DCGM runtime attestation unavailable"}
			}},
		{name: "fewer devices", old: 2, new: 1, want: "New node %s reports 1 of 2 accelerator devices."},
		{name: "no devices", old: 1, new: 0, want: "New node %s reports no accelerator devices.",
			edit: func(r *types.AgentAcceleratorReport) {
				r.Readiness, r.ReadinessReasons = types.AcceleratorReadinessNotReady, []string{"nvidia-smi failed"}
			}},
		{name: "not ready", old: 1, new: 1, want: "New node %s reports its accelerators not ready.",
			edit: func(r *types.AgentAcceleratorReport) {
				r.Readiness, r.ReadinessReasons = types.AcceleratorReadinessNotReady, []string{"node health probe failed: driver not loaded"}
			}},
	} {
		for _, strategy := range []recoverymode.Strategy{"", recoverymode.RemoveFirst} {
			t.Run(tc.name+"/"+cmp.Or(string(strategy), "add-first"), func(t *testing.T) {
				p := &poolPlatform{successorPlatform: newSuccessorPlatform()}
				c, st, _ := successorFixture(t, recoverymode.Automatic, p.successorPlatform, strategy)
				c.platform = p
				registerSuccessorAgent(t, st, 8)
				registerOldNode(t, st, 8, types.AcceleratorVendorNVIDIA)
				eksT4Report(t, st, "n1", "uid-n1", tc.old, nil)
				id := openVendorIncident(t, c, "n1", types.AcceleratorVendorNVIDIA)
				inc := untilState(t, c, st, id, func(inc *types.Incident) bool { return inc.State.Halted() || inc.State == types.StateVerifying })
				entries := trail(t, st, id)
				if inc.State != types.StateVerifying || !auditHasParam(entries, replacedDevicesAction, "device_count", fmt.Sprint(tc.old)) {
					t.Fatalf("state = %s, want VERIFYING with the old node's %d devices on record%s", inc.State, tc.old, dump(entries))
				}
				node := successorNodeName
				if strategy == recoverymode.RemoveFirst {
					node = poolNodeName
					registerAgentAs(t, st, poolNodeName, "uid-"+poolNodeName)
				}
				eksT4Report(t, st, node, "uid-"+node, tc.new, tc.edit)
				want := tc.want
				if !tc.resolved {
					want = fmt.Sprintf(tc.want, node)
				}
				ok, reason, _ := c.verifyRecovery(context.Background(), mustIncident(t, st, id))
				if ok != tc.resolved || ok && !hasSuffix(reason, want) || !ok && reason != want {
					t.Fatalf("verify = %v %q, want %v %q%s", ok, reason, tc.resolved, want, dump(trail(t, st, id)))
				}
				if !tc.resolved {
					return
				}
				inc = untilState(t, c, st, id, func(inc *types.Incident) bool { return inc.State != types.StateVerifying })
				if resolved := resolveEntry(trail(t, st, id)); inc.State != types.StateResolved || resolved == nil || !hasSuffix(resolved.Result, " "+want) {
					t.Fatalf("state = %s, resolve entry %+v; want RESOLVED saying %q", inc.State, resolved, want)
				}
			})
		}
	}
}

func hasSuffix(s, suffix string) bool {
	return len(s) >= len(suffix) && s[len(s)-len(suffix):] == suffix
}
