// SPDX-License-Identifier: Apache-2.0

package controller

import "context"

// Include escalation targets so the first cordon/drain cannot run before the
// cloud recovery path's eligibility is known.
func (c *Controller) cloudRecoveryLadder(ctx context.Context, name string) bool {
	engine := c.runtimeConfig(ctx).Engine
	if engine == nil {
		return false
	}
	seen := map[string]bool{}
	for name != "" && !seen[name] {
		seen[name] = true
		book, ok := engine.Playbook(name)
		if !ok {
			return false
		}
		for _, step := range book.Steps {
			if step.Action == "platform.recycle_node" || step.Action == "platform.replace_node" {
				return true
			}
		}
		name = book.OnFailure.EscalateTo
	}
	return false
}
