// SPDX-License-Identifier: Apache-2.0

package types

import "time"

// AgentSignalSource is whether the agent on a node can read one of its
// accelerator signal sources. Available is not health: it says the source is
// read, not that it reports no fault.
type AgentSignalSource struct {
	Node      string       `json:"node"`
	Source    string       `json:"source"`
	Class     ProblemClass `json:"class,omitempty"`
	Available bool         `json:"available"`
	Reason    string       `json:"reason,omitempty"`
	ReadAt    time.Time    `json:"read_at,omitzero"`
}
