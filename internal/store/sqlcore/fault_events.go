// SPDX-License-Identifier: Apache-2.0

package sqlcore

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/kubeneuron/kubeneuron/pkg/types"
)

// RecentFaultEvents returns at most limit archived agent events whose fault
// names vendor, captured at or after since, newest first. The vendor is
// matched on the archived fault and checked again after decoding, so a row
// whose JSON only looks alike is not returned.
func (c *Core) RecentFaultEvents(ctx context.Context, vendor string, since time.Time, limit int) ([]types.AgentEvent, error) {
	if limit <= 0 || vendor == "" || strings.ContainsAny(vendor, `"%_\`) {
		return nil, nil
	}
	// json.Marshal writes FaultSignal's fields in declaration order, Vendor
	// first, so the prefix selects the vendor's rows on both engines.
	prefix := `{"vendor":"` + vendor + `",%`
	rows, err := c.SQL.QueryContext(ctx, c.rebind(`
		SELECT event_id, node, gpu_index, gpu_uuid, raw, timestamp, fault_json, pci_addr
		FROM events
		WHERE fault_json LIKE ? AND timestamp >= ?
		ORDER BY id DESC
		LIMIT ?`), prefix, ts(since), limit)
	if err != nil {
		return nil, fmt.Errorf("list fault events: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []types.AgentEvent
	for rows.Next() {
		var ev types.AgentEvent
		var at, faultJSON string
		if err := rows.Scan(&ev.EventID, &ev.Node, &ev.GPUIndex, &ev.GPUUUID, &ev.Raw, &at, &faultJSON, &ev.PCIAddr); err != nil {
			return nil, fmt.Errorf("scan fault event: %w", err)
		}
		var fault types.FaultSignal
		if err := json.Unmarshal([]byte(faultJSON), &fault); err != nil || fault.Vendor != vendor {
			continue
		}
		ev.Fault, ev.Timestamp = &fault, parseTS(at)
		out = append(out, ev)
	}
	return out, rows.Err()
}
