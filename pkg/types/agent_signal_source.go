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
	// Via is the kernel log an available kernel source is read from,
	// "journal" or "kmsg"; empty for other sources and older agents.
	Via string `json:"via,omitempty"`
}
