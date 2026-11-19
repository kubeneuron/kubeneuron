// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"io"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/kubeneuron/kubeneuron/internal/detect"
	"github.com/kubeneuron/kubeneuron/internal/playbook"
	"github.com/kubeneuron/kubeneuron/internal/store"
	"github.com/kubeneuron/kubeneuron/internal/store/sqlite"
	"github.com/kubeneuron/kubeneuron/pkg/types"
)

func neuronFixture(t *testing.T, enabled bool) (*Controller, store.Store) {
	t.Helper()
	st, err := sqlite.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	c := New(st, st, nil, nil, nil, nil, nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	rc := *c.runtime.Load()
	rc.Health = HealthPolicy{AWSNeuronSysfs: enabled}
	c.runtime.Store(&rc)
	return c, st
}

func neuronEvent(id, code string, attrs map[string]string) types.AgentEvent {
	return types.AgentEvent{EventID: id, Node: "trn-1", Timestamp: time.Now(),
		Fault: &types.FaultSignal{Vendor: "aws", Source: detect.NeuronSysfsSource, Code: code, Attributes: attrs}}
}

func sendAgentEvent(t *testing.T, c *Controller, ev types.AgentEvent) {
	t.Helper()
	if err := c.HandleAgentEvent(httptest.NewRequest("POST", "/api/v1/events", nil), ev); err != nil {
		t.Fatal(err)
	}
	c.drainEventOutbox(context.Background())
}

func incidentsOf(t *testing.T, st store.Store) []*types.Incident {
	t.Helper()
	incidents, err := st.ListIncidents(context.Background(), store.IncidentFilter{})
	if err != nil {
		t.Fatal(err)
	}
	return incidents
}

func TestNeuronSysfsFaultIsNotActedOnWhileOff(t *testing.T) {
	c, st := neuronFixture(t, false)
	sendAgentEvent(t, c, neuronEvent("n-1", "hbm-ecc-uncorrectable", map[string]string{"device": "neuron3"}))
	// A source name spelled otherwise does not pass the gate: it is on the
	// vendor.
	sendAgentEvent(t, c, neuronEvent("n-2", "unhealthy-component", map[string]string{"device": "neuron1", "component": "hbm"}))
	ev := neuronEvent("n-3", "sram-ecc-uncorrectable", map[string]string{"device": "../../etc"})
	ev.Fault.Source = "kmsg"
	sendAgentEvent(t, c, ev)

	if got := incidentsOf(t, st); len(got) != 0 {
		t.Fatalf("a fault of a source that is off opened incidents: %+v", got)
	}
	rows := healthRowsOf(c.AcceleratorHealth(context.Background(), ""), detect.NeuronSysfsSource)
	if len(rows) != 3 {
		t.Fatalf("rows %+v, want the three faults not acted on", rows)
	}
	for _, r := range rows {
		if r.State != "fault" || r.Class != types.ClassAcceleratorFault || !strings.HasPrefix(r.Why, "not acted on: enable spec.safety.acceleratorHealth.awsNeuronSysfs.enabled") {
			t.Fatalf("row %+v", r)
		}
	}
	// Sorted by rule; a device not in the driver's form is not repeated.
	if rows[0].Device != "neuron3" || rows[1].Device != "" || rows[2].Device != "neuron1" || rows[2].Reason != "hbm" {
		t.Fatalf("devices and components %+v", rows)
	}

	// Once the installation opts in, the faults read before are not shown
	// as waiting for it.
	rc := *c.runtime.Load()
	rc.Health.AWSNeuronSysfs = true
	c.runtime.Store(&rc)
	if rows := healthRowsOf(c.AcceleratorHealth(context.Background(), ""), detect.NeuronSysfsSource); len(rows) != 0 {
		t.Fatalf("rows after opting in %+v", rows)
	}
}

func TestNeuronSysfsFaultOpensANodeIncidentWhileOn(t *testing.T) {
	c, st := neuronFixture(t, true)
	ev := neuronEvent("n-1", "hbm-ecc-uncorrectable", map[string]string{"device": "neuron3"})
	ev.GPUIndex = 3
	sendAgentEvent(t, c, ev)
	got := incidentsOf(t, st)
	if len(got) != 1 {
		t.Fatalf("incidents %+v, want one", got)
	}
	inc := got[0]
	if inc.Class != types.ClassAcceleratorFault || inc.Target.IsDeviceScoped() || inc.Target.Node != "trn-1" {
		t.Fatalf("incident %+v, want the node's accelerator-fault", inc)
	}
	if rows := healthRowsOf(c.AcceleratorHealth(context.Background(), ""), detect.NeuronSysfsSource); len(rows) != 0 {
		t.Fatalf("a fault acted on is also shown as not acted on: %+v", rows)
	}
}

// A fault of the sysfs read names the vendor aws, so a policy scoped to aws
// answers it; the same fault read from a Node condition names none and gets
// the policy for any vendor.
func TestNeuronSysfsFaultSelectsTheAWSPolicy(t *testing.T) {
	c, _ := neuronFixture(t, true)
	sig, ok := c.agentEventSignal(context.Background(), neuronEvent("n-1", "sram-ecc-uncorrectable", map[string]string{"device": "neuron0"}))
	if !ok || sig.Vendor() != types.AcceleratorVendorAWS {
		t.Fatalf("signal %+v ok %v, want vendor aws", sig, ok)
	}
	if sig.Evidence["attr_device"] != "neuron0" || sig.Evidence["source"] != detect.NeuronSysfsSource {
		t.Fatalf("evidence %v", sig.Evidence)
	}
	book := &playbook.Playbook{Name: "observe", Steps: []playbook.Step{{Name: "notify", Action: "notify.ticket"}}}
	engine, err := playbook.NewEngine(map[string]*playbook.Playbook{"observe": book}, []playbook.Policy{
		{Name: "any", Class: types.ClassAcceleratorFault, Playbook: "observe"},
		{Name: "aws", Class: types.ClassAcceleratorFault, Vendor: types.AcceleratorVendorAWS, Playbook: "observe"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if pol, _ := engine.PolicyFor(sig.Class, sig.Vendor()); pol.Name != "aws" {
		t.Fatalf("sysfs fault got policy %q, want aws", pol.Name)
	}
	condition := types.Signal{Source: types.SourceNodeHealth, Class: types.ClassAcceleratorFault, Evidence: map[string]string{"vendor": "aws"}}
	if pol, _ := engine.PolicyFor(condition.Class, condition.Vendor()); pol.Name != "any" {
		t.Fatalf("node condition fault got policy %q, want any", pol.Name)
	}
}

func TestNeuronSysfsHeldFaultsAreBounded(t *testing.T) {
	c, _ := neuronFixture(t, false)
	ctx := context.Background()
	start := time.Now().Add(-time.Hour)
	for i := 0; i <= neuronHeldMax; i++ {
		ev := neuronEvent("", "hbm-ecc-uncorrectable", nil)
		ev.Node = "trn-" + strings.Repeat("x", i%3) + string(rune('a'+i%26)) + "-" + time.Duration(i).String()
		ev.Timestamp = start.Add(time.Duration(i) * time.Second)
		if _, ok := c.agentEventSignal(ctx, ev); ok {
			t.Fatal("a fault of a source that is off gave a signal")
		}
	}
	rows := c.neuronHeldRows(ctx, func(string) bool { return true }, time.Now())
	if len(rows) != neuronHeldMax {
		t.Fatalf("%d rows, want %d", len(rows), neuronHeldMax)
	}
	for _, r := range rows {
		if r.Since.Equal(start) {
			t.Fatal("the oldest fault was kept past the bound")
		}
	}
	if rows := c.neuronHeldRows(ctx, func(string) bool { return true }, time.Now().Add(neuronHeldFor+time.Hour)); len(rows) != 0 {
		t.Fatalf("%d rows a day later, want none", len(rows))
	}
}

// The agent sends a counter's rise once, so a restart or a new leader reads
// the faults not acted on back from the event archive instead of showing a
// broken device as quiet.
func TestNeuronSysfsHeldFaultsOutliveANewTerm(t *testing.T) {
	c, _ := neuronFixture(t, false)
	sendAgentEvent(t, c, neuronEvent("n-1", "hbm-ecc-uncorrectable", map[string]string{"device": "neuron3"}))
	old := neuronEvent("n-2", "sram-ecc-uncorrectable", map[string]string{"device": "neuron4"})
	old.Timestamp = time.Now().Add(-neuronHeldFor - time.Hour)
	sendAgentEvent(t, c, old)
	// Another vendor's fault is not this source's.
	amd := types.AgentEvent{EventID: "a-1", Node: "mi-1", Timestamp: time.Now(),
		Fault: &types.FaultSignal{Vendor: "amd", Source: "amd-smi", Code: "ecc-uncorrectable"}}
	sendAgentEvent(t, c, amd)

	c.health.reset()
	rows := healthRowsOf(c.AcceleratorHealth(context.Background(), ""), detect.NeuronSysfsSource)
	if len(rows) != 1 || rows[0].Device != "neuron3" || rows[0].Rule != "hbm-ecc-uncorrectable" || rows[0].State != "fault" {
		t.Fatalf("rows after a new term %+v, want the fault of the last day", rows)
	}
	// Read back once a term: a second view does not add another row.
	if again := healthRowsOf(c.AcceleratorHealth(context.Background(), ""), detect.NeuronSysfsSource); len(again) != 1 {
		t.Fatalf("second view %+v", again)
	}
}
