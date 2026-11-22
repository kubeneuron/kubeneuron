// SPDX-License-Identifier: Apache-2.0

package kubernetes

import (
	"context"
	"os"
	"testing"

	"github.com/kubeneuron/kubeneuron/internal/nodehealth"
	"github.com/kubeneuron/kubeneuron/pkg/types"
)

// TestDRAHealthLive reads the device taints a live API server keeps. It runs
// only under hack/kind-dra-health.sh, which creates the slice; elsewhere it
// is skipped.
func TestDRAHealthLive(t *testing.T) {
	kubeconfig, node := os.Getenv("KUBENEURON_DRA_LIVE_KUBECONFIG"), os.Getenv("KUBENEURON_DRA_LIVE_NODE")
	if kubeconfig == "" || node == "" {
		t.Skip("no live cluster: run hack/kind-dra-health.sh")
	}
	p, err := New(kubeconfig)
	if err != nil {
		t.Fatal(err)
	}
	snaps, err := p.DRAHealth(context.Background())
	if err != nil {
		t.Fatalf("DRAHealth: %v", err)
	}
	var snap *types.NodeHealthSnapshot
	for i := range snaps {
		if snaps[i].Node == node {
			snap = &snaps[i]
		}
	}
	if snap == nil || len(snap.DRADevices) != 2 {
		t.Fatalf("snapshots %+v: want %s with two devices", snaps, node)
	}
	for _, d := range snap.DRADevices {
		t.Logf("device %s/%s/%s uuid=%q taints=%+v", d.Driver, d.Pool, d.Device, d.UUID, d.Taints)
	}
	tainted := snap.DRADevices[0]
	if len(tainted.Taints) != 1 || tainted.Taints[0].TimeAdded.IsZero() {
		t.Fatalf("device %+v: the API server must keep the taint and set timeAdded", tainted)
	}
	h, states := nodehealth.EvaluateDRA(*snap)
	if len(h.Observations) != 1 || h.Observations[0].State != nodehealth.StateFault || h.Observations[0].DeviceUUID == "" {
		t.Fatalf("observations %+v", h.Observations)
	}
	if states[nodehealth.DRADeviceKey(snap.DRADevices[1])] != nodehealth.DRANoHealthTaint {
		t.Fatalf("untainted device read as %q", states[nodehealth.DRADeviceKey(snap.DRADevices[1])])
	}
}
