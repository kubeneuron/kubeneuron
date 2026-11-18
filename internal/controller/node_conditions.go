// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"encoding/json"
	"maps"
	"time"

	"github.com/kubeneuron/kubeneuron/pkg/types"
)

type nodeConditionReader interface {
	NodeConditions(context.Context, string) (*types.NodeConditionSnapshot, error)
}

// decisionNodeContext attaches observations to the audit only. Bound the
// lookup outside the store transaction so a slow API cannot hold its locks.
func (c *Controller) decisionNodeContext(ctx context.Context, node string, params map[string]string) map[string]string {
	reader, ok := c.platform.(nodeConditionReader)
	if !ok || node == "" {
		return params
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	result := maps.Clone(params)
	if result == nil {
		result = make(map[string]string)
	}
	snapshot, err := reader.NodeConditions(ctx, node)
	if err == nil && snapshot != nil {
		if data, err := json.Marshal(snapshot); err == nil {
			result["node_conditions"] = string(data)
			return result
		}
	}
	result["node_conditions_unavailable"] = "NodeReadFailed"
	return result
}
