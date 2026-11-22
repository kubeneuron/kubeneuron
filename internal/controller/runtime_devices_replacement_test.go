// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kubeneuron/kubeneuron/internal/agent/nvml"
	"github.com/kubeneuron/kubeneuron/internal/recoverymode"
	"github.com/kubeneuron/kubeneuron/internal/store/sqlite"
	"github.com/kubeneuron/kubeneuron/pkg/types"
)

// registerRuntimeAgent registers node as an agent started with
// --nvidia-runtime-devices registers it when the runtime gave it no GPU: the
// GPUs its driver lists, and no NVIDIA report, which such an agent does not
// send.
func registerRuntimeAgent(t *testing.T, st *sqlite.Store, node, uid string, seen time.Time) {
	t.Helper()
	driver, err := nvml.RuntimeInjected(filepath.Join(t.TempDir(), "nvidia-smi"))
	if err == nil {
		t.Fatal("RuntimeInjected found an nvidia-smi in an empty directory")
	}
	gpus, err := driver.ListGPUs(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertAgentRegistration(context.Background(), &types.Node{Name: node, UID: uid, AgentLastSeen: seen,
		Labels: map[string]string{"pool": "train"}, GPUs: gpus}); err != nil {
		t.Fatal(err)
	}
}

// Agents the NVIDIA runtime gave no GPU, on the old node and the new one: the
// replacement after an NVIDIA fault resolves on readiness and capacity.
func TestReplacementWithRuntimeAgentsThatSeeNoGPU(t *testing.T) {
	for _, tc := range []struct {
		name     string
		strategy recoverymode.Strategy
		node     string
	}{
		{"add-first", "", successorNodeName},
		{"remove-first", recoverymode.RemoveFirst, poolNodeName},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := &poolPlatform{successorPlatform: newSuccessorPlatform()}
			c, st, _ := successorFixture(t, recoverymode.Automatic, p.successorPlatform, tc.strategy)
			c.platform = p
			registerRuntimeAgent(t, st, successorNodeName, "uid-"+successorNodeName, time.Now().Add(time.Second))
			registerRuntimeAgent(t, st, "n1", "uid-n1", time.Now())
			id := openVendorIncident(t, c, "n1", types.AcceleratorVendorNVIDIA)
			inc := untilState(t, c, st, id, func(inc *types.Incident) bool { return inc.State.Halted() || inc.State == types.StateVerifying })
			if inc.State != types.StateVerifying && inc.State != types.StateResolved {
				t.Fatalf("state = %s after the replacement%s", inc.State, dump(trail(t, st, id)))
			}
			if tc.node == poolNodeName {
				registerRuntimeAgent(t, st, poolNodeName, "uid-"+poolNodeName, time.Now())
			}
			inc = untilState(t, c, st, id, func(inc *types.Incident) bool { return inc.State != types.StateVerifying })
			entries := trail(t, st, id)
			if !auditHasParam(entries, replacedDevicesAction, "devices", "false") {
				t.Fatalf("no %s row says the old node never reported%s", replacedDevicesAction, dump(entries))
			}
			resolved := resolveEntry(entries)
			if inc.State != types.StateResolved || resolved == nil || resolved.Params["replacement_node"] != tc.node ||
				!strings.HasSuffix(resolved.Result, " Verification used Kubernetes node readiness and advertised accelerator capacity.") {
				t.Fatalf("state = %s, resolve %+v; want RESOLVED on %s by readiness and capacity%s", inc.State, resolved, tc.node, dump(entries))
			}
			reports, err := st.ListAcceleratorReports(context.Background(), "")
			if err != nil || len(reports) != 0 {
				t.Fatalf("accelerator reports %v (%v), want none", reports, err)
			}
		})
	}
}
