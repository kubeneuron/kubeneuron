// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/kubeneuron/kubeneuron/internal/detect"
	"github.com/kubeneuron/kubeneuron/internal/nodehealth"
	"github.com/kubeneuron/kubeneuron/pkg/types"
)

func healthRowsOf(view types.AcceleratorHealthView, source string) []types.AcceleratorHealthRow {
	var out []types.AcceleratorHealthRow
	for _, r := range view.Rows {
		if r.Source == source {
			out = append(out, r)
		}
	}
	return out
}

// Before a source is read in the term its one row says so, and that it holds
// the steps that take a node out of service; once read, its rows are the
// node's signals.
func TestAcceleratorHealthNotReadThisTerm(t *testing.T) {
	c, _, plat, clock := healthFixture(t)
	ctx := context.Background()
	plat.set(neuronNode(clock, "True", neuronDMA))
	view := c.AcceleratorHealth(ctx, "trn-1")
	rows := healthRowsOf(view, "kubernetes-node")
	if len(rows) != 1 || rows[0].State != types.HealthRowNotReadThisTerm || rows[0].Node != "trn-1" ||
		!strings.Contains(rows[0].Why, "no step that takes a node out of service runs") {
		t.Fatalf("rows %+v", rows)
	}
	if refusal := c.healthRecoveryRefusal("trn-1", "platform.recycle_node"); refusal == "" {
		t.Fatal("the steps were not held")
	}

	plat.err = errors.New("nodes is forbidden: token abc123")
	c.pollNodeHealth(ctx)
	rows = healthRowsOf(c.AcceleratorHealth(ctx, ""), "kubernetes-node")
	if len(rows) != 1 || rows[0].State != types.HealthRowNotReadThisTerm || !strings.Contains(rows[0].Why, "failed") ||
		strings.Contains(rows[0].Why, "abc123") {
		t.Fatalf("rows after a failed read %+v", rows)
	}

	plat.err = nil
	c.pollNodeHealth(ctx)
	rows = healthRowsOf(c.AcceleratorHealth(ctx, "trn-1"), "kubernetes-node")
	if len(rows) != 2 || rows[0].Rule != "allocatable" || rows[0].State != string(nodehealth.StateHealthy) {
		t.Fatalf("rows after a read %+v", rows)
	}
	if r := rows[1]; r.State != string(nodehealth.StateFault) || r.Class != types.ClassAcceleratorFault ||
		r.Reason != neuronDMA || r.Profile != "aws-neuron" || r.Rule != "neuron-npd" || !r.ReadAt.Equal(clock.Now()) {
		t.Fatalf("fault row %+v", r)
	}

	// A later failed read keeps the rows, and says they are older.
	plat.err = errors.New("timeout")
	clock.Advance(time.Minute)
	c.pollNodeHealth(ctx)
	rows = healthRowsOf(c.AcceleratorHealth(ctx, "trn-1"), "kubernetes-node")
	if len(rows) != 3 || rows[0].Rule != "" || rows[0].State != string(nodehealth.StateUnknown) || !strings.Contains(rows[0].Why, "from the read before") ||
		rows[2].State != string(nodehealth.StateFault) {
		t.Fatalf("rows after a later failed read %+v", rows)
	}
}

// The node filter is an exact name, and rows are sorted by node.
func TestAcceleratorHealthNodeFilterAndOrder(t *testing.T) {
	c, _, plat, clock := healthFixture(t)
	ctx := context.Background()
	a, b := neuronNode(clock, "False", "NeuronHealthy"), neuronNode(clock, "True", neuronDMA)
	b.Node, b.UID = "trn-10", "uid-trn-10"
	plat.set(b, a)
	c.pollNodeHealth(ctx)
	view := c.AcceleratorHealth(ctx, "")
	if len(view.Rows) != 4 || view.Rows[0].Node != "trn-1" || view.Rows[1].Node != "trn-1" || view.Rows[2].Node != "trn-10" ||
		view.ProfileVersion != nodehealth.Version {
		t.Fatalf("rows %+v", view.Rows)
	}
	if view.Rows[1].State != string(nodehealth.StateHealthy) || view.Rows[3].State != string(nodehealth.StateFault) {
		t.Fatalf("rows %+v", view.Rows)
	}
	view = c.AcceleratorHealth(ctx, "trn-1")
	if len(view.Rows) != 3 || view.Rows[0].Source != agentHealthSource || view.Rows[1].Node != "trn-1" || view.Rows[2].Node != "trn-1" {
		t.Fatalf("filtered rows %+v", view.Rows)
	}
	if view = c.AcceleratorHealth(ctx, "trn"); len(view.Rows) != 1 || view.Rows[0].Source != agentHealthSource || view.Rows[0].Node != "trn" {
		t.Fatalf("a prefix matched: %+v", view.Rows)
	}
}

// Past the row limit the view is cut and says so.
func TestAcceleratorHealthTruncated(t *testing.T) {
	defer func(n int) { maxHealthRows = n }(maxHealthRows)
	maxHealthRows = 1
	c, _, plat, clock := healthFixture(t)
	ctx := context.Background()
	a, b := neuronNode(clock, "False", "NeuronHealthy"), neuronNode(clock, "False", "NeuronHealthy")
	b.Node = "trn-2"
	plat.set(a, b)
	c.pollNodeHealth(ctx)
	view := c.AcceleratorHealth(ctx, "")
	if !view.Truncated || len(view.Rows) != 1 || view.Rows[0].Node != "trn-1" {
		t.Fatalf("view %+v", view)
	}
}

// A DRA device that carries no taint, or that its driver does not watch, is
// shown as no-signal, not healthy.
func TestAcceleratorHealthDRANoSignal(t *testing.T) {
	c, plat, _ := draFixture(t, true)
	ctx := context.Background()
	quiet := gpuDevice()
	unwatched := gpuDevice(types.DRATaint{Key: "gpu.nvidia.com/unmonitored", Effect: "None"})
	unwatched.Device, unwatched.UUID = "gpu-1", ""
	plat.setDevices("gpu-1", quiet, unwatched)
	c.pollNodeHealth(ctx)
	rows := healthRowsOf(c.AcceleratorHealth(ctx, "gpu-1"), draHealthSourceName)
	if len(rows) != 2 {
		t.Fatalf("rows %+v", rows)
	}
	for _, r := range rows {
		if r.State != types.HealthRowNoSignal || r.Profile == "" {
			t.Fatalf("row %+v", r)
		}
	}
	if rows[0].Device != "gpu-0" || rows[0].Reason != nodehealth.DRANoHealthTaint {
		t.Fatalf("untainted %+v", rows[0])
	}
}

// The text of a row is KubeNeuron's: a cloud's error is not repeated, and
// what a node reports is bounded.
func TestAcceleratorHealthKeepsSourceTextOut(t *testing.T) {
	c, plat, clock := cloudHealthFixture(t)
	ctx := context.Background()
	plat.cloudErr = "permission-denied"
	c.pollNodeHealth(ctx)
	raw, err := json.Marshal(c.AcceleratorHealth(ctx, ""))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "scripted, request") {
		t.Fatalf("the cloud's error is in the view: %s", raw)
	}
	if !strings.Contains(string(raw), "permission-denied") {
		t.Fatalf("the reason of the failed read is not shown: %s", raw)
	}

	long := neuronNode(clock, "True", strings.Repeat("X", 1000)+"\nnext line")
	plat.set(long)
	c.pollNodeHealth(ctx)
	for _, r := range healthRowsOf(c.AcceleratorHealth(ctx, "trn-1"), "kubernetes-node") {
		for _, s := range []string{r.Reason, r.Why} {
			if len(s) > maxHealthText+len("…") || strings.Contains(s, "\n") {
				t.Fatalf("unbounded text %q", s)
			}
		}
	}
}

// What an agent reports about its sources: one it cannot read is unknown
// with its reason, one it can is available, never healthy; an agent that
// lists none gives no data. A reason not in the form of a code is dropped.
func TestAcceleratorHealthAgentSources(t *testing.T) {
	c, _, _, clock := healthFixture(t)
	ctx := context.Background()

	if rows := healthRowsOf(c.AcceleratorHealth(ctx, "trn-1"), agentHealthSource); len(rows) != 1 || rows[0].State != "unknown" {
		t.Fatalf("agent rows before first report %+v", rows)
	}
	sources := map[string][]types.AgentSignalSource{
		"trn-1": {
			{Node: "trn-1", Source: "nvidia-dcgm", Available: false, Reason: "dcgm-unreachable", ReadAt: clock.Now()},
			{Node: "trn-1", Source: "nvidia-kmsg", Available: true, ReadAt: clock.Now()},
			{Node: "trn-1", Source: "amd-kmsg", Available: false, Reason: "open /dev/kmsg: permission denied"},
		},
	}
	c.agentSignalSources = func(node string) []types.AgentSignalSource { return sources[node] }
	rows := healthRowsOf(c.AcceleratorHealth(ctx, "trn-1"), agentHealthSource)
	if len(rows) != 3 {
		t.Fatalf("rows %+v", rows)
	}
	byRule := map[string]types.AcceleratorHealthRow{}
	for _, r := range rows {
		byRule[r.Rule] = r
	}
	if r := byRule["nvidia-dcgm"]; r.State != string(nodehealth.StateUnknown) || r.Reason != "dcgm-unreachable" {
		t.Fatalf("dcgm %+v", r)
	}
	if r := byRule["nvidia-kmsg"]; r.State != types.HealthRowAvailable {
		t.Fatalf("kmsg %+v", r)
	}
	if r := byRule["amd-kmsg"]; r.State != string(nodehealth.StateUnknown) || r.Reason != "" {
		t.Fatalf("free-text reason kept %+v", r)
	}

	delete(sources, "trn-1")
	rows = healthRowsOf(c.AcceleratorHealth(ctx, "trn-1"), agentHealthSource)
	if len(rows) != 1 || rows[0].State != string(nodehealth.StateUnknown) || !strings.Contains(rows[0].Why, "no data") {
		t.Fatalf("an agent with no sources %+v", rows)
	}
	if rows := healthRowsOf(c.AcceleratorHealth(ctx, ""), agentHealthSource); len(rows) != 0 {
		t.Fatalf("no-data rows in the fleet view %+v", rows)
	}
}

// A source of another vendor than the node's accelerators is not the node's:
// an agent built for every vendor lists them all.
func TestAcceleratorHealthAgentSourcesOfTheNodesVendor(t *testing.T) {
	c, _, _, clock := healthFixture(t)
	ctx := context.Background()
	name := c.healthSources()[0].name()
	c.health.mu.Lock()
	c.health.read = map[string]bool{name: true}
	c.health.latest = map[string]nodehealth.NodeHealth{
		name + "/gpu-1": {Node: "gpu-1", Profiles: []string{"nvidia"}},
		name + "/mi-1":  {Node: "mi-1", Profiles: []string{"amd-instinct"}},
		name + "/trn-1": {Node: "trn-1", Profiles: []string{"aws-neuron"}},
	}
	c.health.mu.Unlock()
	all := func(node string) []types.AgentSignalSource {
		return []types.AgentSignalSource{
			{Node: node, Source: "nvidia-kmsg", Available: true, ReadAt: clock.Now()},
			{Node: node, Source: "amd-kmsg", Class: types.ClassPCIe, Available: true, ReadAt: clock.Now()},
			{Node: node, Source: detect.NeuronSysfsSource, Class: types.ClassAcceleratorFault, Available: true, ReadAt: clock.Now()},
		}
	}
	c.agentSignalSources = all
	for node, want := range map[string]string{"gpu-1": "nvidia-kmsg", "mi-1": "amd-kmsg", "trn-1": detect.NeuronSysfsSource} {
		rows := healthRowsOf(c.AcceleratorHealth(ctx, node), agentHealthSource)
		if len(rows) != 1 || rows[0].Rule != want {
			t.Fatalf("%s: rows %+v, want only %s", node, rows, want)
		}
	}
}

// An available kernel source says in WHY which kernel log the agent reads;
// an older agent that sends no via reads as before.
func TestAcceleratorHealthAgentKernelSourceVia(t *testing.T) {
	for _, tc := range []struct{ via, why string }{
		{"journal", "the agent reads this source from the node's journal; a fault it reads opens an incident, and none read is not health"},
		{"kmsg", "the agent reads this source from /dev/kmsg; a fault it reads opens an incident, and none read is not health"},
		{"", "the agent reads this source; a fault it reads opens an incident, and none read is not health"},
	} {
		rows := agentHealthRows("gpu-1", []types.AgentSignalSource{{Node: "gpu-1", Source: "nvidia-kmsg", Available: true, Via: tc.via}}, true)
		if len(rows) != 1 || rows[0].State != types.HealthRowAvailable || rows[0].Why != tc.why {
			t.Fatalf("via %q rows %+v, want why %q", tc.via, rows, tc.why)
		}
	}
	// An older agent's report has no via field at all.
	var old []types.AgentSignalSource
	if err := json.Unmarshal([]byte(`[{"node":"gpu-1","source":"amd-kmsg","class":"pcie","available":true,"read_at":"2026-11-20T00:00:00Z"}]`), &old); err != nil {
		t.Fatal(err)
	}
	if rows := agentHealthRows("gpu-1", old, true); len(rows) != 1 ||
		rows[0].Why != "the agent reads this source; a fault it reads opens an incident, and none read is not health" {
		t.Fatalf("older agent rows %+v", rows)
	}
}
