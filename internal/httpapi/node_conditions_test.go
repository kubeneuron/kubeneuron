// SPDX-License-Identifier: Apache-2.0

package httpapi

import (
	"github.com/kubeneuron/kubeneuron/pkg/types"
	"testing"
)

func TestLatestNodeConditions(t *testing.T) {
	trail := []*types.AuditEntry{{Params: map[string]string{"node_conditions": `{"observed_at":"2026-01-01T00:00:00Z","node_uid":"worker"}`}}, {}}
	trail = append(trail, &types.AuditEntry{Action: "open", Params: map[string]string{"node_conditions_unavailable": "untrusted"}})
	if snap, reason := latestNodeConditions(trail); snap == nil || snap.NodeUID != "worker" || reason != "" {
		t.Fatal(snap, reason)
	}
	for key, value := range map[string]string{"node_conditions_unavailable": "NodeReadFailed", "node_conditions": "broken"} {
		next := append(trail, &types.AuditEntry{Params: map[string]string{key: value}})
		if snap, reason := latestNodeConditions(next); snap != nil || reason == "" {
			t.Fatal(snap, reason)
		}
	}
}
