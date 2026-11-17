// SPDX-License-Identifier: Apache-2.0
package controller

import (
	"context"
	"github.com/kubeneuron/kubeneuron/internal/recoverymode"
	"github.com/kubeneuron/kubeneuron/internal/safety"
	"strings"
	"testing"
	"time"

	"github.com/kubeneuron/kubeneuron/pkg/cloud"
	"github.com/kubeneuron/kubeneuron/pkg/types"
)

func TestRecycleInventoryMatches(t *testing.T) {
	now := time.Now()
	pin := cloud.NodeIdentity{Node: "n", NodeUID: "uid", ProviderID: "p", InstanceID: "i", BootID: "new"}
	completed := now.Add(-time.Minute)
	for _, tc := range []struct {
		name, boot, uid, gpu string
		observed             time.Time
		want                 bool
	}{
		{"current device", "new", "uid", "gpu", now, true},
		{"legacy", "", "uid", "gpu", now, false},
		{"delayed old boot", "old", "uid", "gpu", now, false},
		{"reused name", "new", "other", "gpu", now, false},
		{"before recycle", "new", "uid", "gpu", completed, false},
		{"future", "new", "uid", "gpu", now.Add(time.Second), false},
		{"different device", "new", "uid", "other", now, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := &types.AgentAcceleratorReport{Node: "n", NodeUID: tc.uid, BootID: tc.boot, ObservedAt: tc.observed, Readiness: types.AcceleratorReadinessReady,
				Devices: []types.AgentAcceleratorDevice{{ID: "gpu", Kind: types.AcceleratorDevicePhysical, Family: types.AcceleratorFamilyGPU}}}
			if got := recycleInventoryMatches(r, pin, tc.gpu, completed, now); got != tc.want {
				t.Fatalf("matches = %t, want %t", got, tc.want)
			}
		})
	}
}

func TestRecycleVerificationAfterControllerRestart(t *testing.T) {
	for _, name := range []string{"verified-recycle", "subsequent-replacement", "mixed-degradation"} {
		replacing := name == "subsequent-replacement"
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			p := &autoCloudPlatform{}
			first, st := stackTestController(t, p)
			ref, err := p.CloudNode(ctx, "node-a")
			if err != nil {
				t.Fatal(err)
			}
			old := ref.Identity
			old.BootID = "previous"
			now := time.Now().UTC()
			inc := resetIncident()
			inc.CloudIdentity = &old
			inc.StateChangedAt = now.Add(-2 * time.Minute)
			inc.UpdatedAt = inc.StateChangedAt
			if err := st.CreateIncident(ctx, inc); err != nil {
				t.Fatal(err)
			}
			if err := first.repinAfterRecycle(ctx, inc, cloud.RecycleResult{Before: old, After: ref.Identity, CompletedAt: now.Add(-time.Minute)}); err != nil {
				t.Fatal(err)
			}
			inc.State = types.StateVerifying
			if err := st.UpdateIncident(ctx, inc); err != nil {
				t.Fatal(err)
			}
			if err := st.AppendAudit(ctx, &types.AuditEntry{IncidentID: inc.ID, Time: now, Result: auditStepOutcomeResult("platform.recycle_node", false, true)}); err != nil {
				t.Fatal(err)
			}
			if replacing {
				if err := st.AppendAudit(ctx, &types.AuditEntry{IncidentID: inc.ID, Time: now, Result: auditStepStartResult("platform.replace_node")}); err != nil {
					t.Fatal(err)
				}
			}
			report := readyNVIDIAResetReport(now, "profile")
			report.Readiness = types.AcceleratorReadinessDegraded
			report.ReadinessReasons = []string{"DCGM unavailable"}
			report.RuntimeVersion = ""
			if name == "mixed-degradation" {
				report.ReadinessReasons = append(report.ReadinessReasons, "additional inventory fault")
			} else {
				report.Devices[0].DriverHealthyAt = &now
			}
			report.BootID = ref.Identity.BootID
			report.NodeUID = ref.Identity.NodeUID
			if err := st.UpsertAcceleratorReport(ctx, &report); err != nil {
				t.Fatal(err)
			}
			c := New(st, st, modeEngine(t, recoverymode.Approval, nil), safety.NewGate(safety.Limits{}), nil, p, nil, &recordingNotifier{}, first.log)
			c.SetTimings(time.Millisecond, 0)
			reloaded, err := st.GetIncident(ctx, inc.ID)
			if err != nil {
				t.Fatal(err)
			}
			if err := c.advanceVerifying(ctx, reloaded); err != nil {
				t.Fatal(err)
			}
			got, err := st.GetIncident(ctx, inc.ID)
			if err != nil {
				t.Fatal(err)
			}
			want := types.StateResolved
			if replacing || name == "mixed-degradation" {
				want = types.StateVerifying
			}
			if want == types.StateResolved {
				trail, err := st.AuditTrail(ctx, inc.ID)
				if err != nil || len(trail) == 0 || !strings.Contains(trail[len(trail)-1].Result, "DCGM not present, device health not checked") {
					t.Fatalf("missing verification depth in resolution audit: %v, %v", trail, err)
				}
			}
			if got.State != want {
				t.Fatalf("got %s, want %s", got.State, want)
			}
		})
	}
}

func TestDegradedInventoryRequiresFreshDriverHealth(t *testing.T) {
	now := time.Now().UTC()
	completed := now.Add(-time.Minute)
	future := now.Add(time.Minute)
	pin := cloud.NodeIdentity{Node: "node-a", NodeUID: "uid", ProviderID: "p", InstanceID: "i", BootID: "new"}
	report := readyNVIDIAResetReport(now, "profile")
	report.NodeUID, report.BootID = pin.NodeUID, pin.BootID
	report.Readiness = types.AcceleratorReadinessDegraded
	for _, tc := range []struct {
		name   string
		health *time.Time
		want   bool
	}{
		{"missing", nil, false}, {"prior boot", &completed, false}, {"future", &future, false}, {"current", &now, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			report.Devices[0].DriverHealthyAt = tc.health
			if got := recycleInventoryMatches(&report, pin, report.Devices[0].ID, completed, now); got != tc.want {
				t.Fatalf("matches = %t, want %t", got, tc.want)
			}
		})
	}
}
