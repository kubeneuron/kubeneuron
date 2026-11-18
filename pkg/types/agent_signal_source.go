// SPDX-License-Identifier: Apache-2.0

package types

import "time"

// AgentSignalSource describes whether an agent can read a signal for a class.
// Available describes the source and makes no assertion about device health.
type AgentSignalSource struct {
	Node      string       `json:"node"`
	Source    string       `json:"source"`
	Class     ProblemClass `json:"class"`
	Available bool         `json:"available"`
	Reason    string       `json:"reason,omitempty"`
	ReadAt    time.Time    `json:"read_at"`
}
