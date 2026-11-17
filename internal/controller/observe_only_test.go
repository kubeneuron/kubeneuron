// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/kubeneuron/kubeneuron/internal/detect"
	"github.com/kubeneuron/kubeneuron/internal/playbook"
	"github.com/kubeneuron/kubeneuron/internal/recoverymode"
	"github.com/kubeneuron/kubeneuron/internal/safety"
	"github.com/kubeneuron/kubeneuron/internal/store"
	"github.com/kubeneuron/kubeneuron/internal/store/sqlite"
	"github.com/kubeneuron/kubeneuron/pkg/types"
)

// No data from a node is not evidence that its accelerator failed. An
// Automatic policy that binds agent-down or exporter-down straight to the
// replacement ladder still runs no step: the platform below has no cordon,
// drain or cloud call to make (any would panic on the nil Platform), and the
// gate is live, so nothing is simulated either.
func TestObserveOnlyClassesNeverStartRecovery(t *testing.T) {
	for _, class := range []types.ProblemClass{types.ClassAgentDown, types.ClassExporterDown} {
		t.Run(string(class), func(t *testing.T) {
			st, err := sqlite.Open(":memory:")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = st.Close() })
			ctx := context.Background()
			for i := 1; i <= 4; i++ {
				if err := st.UpsertNode(ctx, &types.Node{Name: nodeName(i), Labels: map[string]string{"pool": "train"}}); err != nil {
					t.Fatal(err)
				}
			}
			book := &playbook.Playbook{Name: "cloud-replace", Target: "node", Steps: []playbook.Step{
				{Name: "cordon", Action: "platform.cordon"},
				{Name: "drain", Action: "platform.drain", Approval: "required"},
				{Name: "replace", Action: "platform.replace_node", Approval: "required"},
			}}
			engine, err := playbook.NewEngine(map[string]*playbook.Playbook{"cloud-replace": book}, []playbook.Policy{{
				Name: "user-binding", Class: class, Playbook: "cloud-replace", Mode: recoverymode.Automatic,
				Automatic: autoSettings(), ReplacementStrategy: recoverymode.RemoveFirst,
			}})
			if err != nil {
				t.Fatal(err)
			}
			gate := safety.NewGate(safety.Limits{MaxConcurrentRemediations: 8, MaxConcurrentReboots: 8})
			notifier := &recordingNotifier{}
			c := New(st, st, engine, gate, safety.NewFlapDetector(100, time.Hour), &autoCloudPlatform{}, nil, notifier,
				slog.New(slog.NewTextHandler(io.Discard, nil)))

			for i := 0; i < 5; i++ { // past any observe threshold
				if err := c.ingest(ctx, signal(class, "n1", "")); err != nil {
					t.Fatal(err)
				}
			}
			incidents, err := st.ListIncidents(ctx, store.IncidentFilter{Node: "n1"})
			if err != nil || len(incidents) != 1 {
				t.Fatalf("incidents = %v, %v", incidents, err)
			}
			id := incidents[0].ID
			if incidents[0].Playbook != "cloud-replace" {
				t.Fatalf("playbook %q: the user's binding must still be recorded", incidents[0].Playbook)
			}
			for i := 0; i < 20; i++ {
				c.reconcile(ctx)
			}
			inc, err := st.GetIncident(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			if inc.State != types.StateObserving || inc.StepIndex != 0 || inc.RemediationSlotHeld {
				t.Fatalf("incident = %s step %d slot %v, want OBSERVING with no step", inc.State, inc.StepIndex, inc.RemediationSlotHeld)
			}
			if len(notifier.approvals) != 0 {
				t.Fatalf("approval requests %v", notifier.approvals)
			}
			reason, _ := detect.ObserveOnly(class)
			trail, err := st.AuditTrail(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			recorded := false
			for _, e := range trail {
				if e.Action == "observe-only" && e.Result == reason {
					recorded = true
				}
				if e.Action == "cordon" || e.Action == "drain" || e.Action == "replace" || strings.HasPrefix(e.Action, "platform.") {
					t.Errorf("audit has a step: %+v", e)
				}
			}
			if !recorded {
				t.Fatalf("audit %+v: want an observe-only row with %q", trail, reason)
			}
		})
	}
}
