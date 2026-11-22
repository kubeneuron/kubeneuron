// SPDX-License-Identifier: Apache-2.0

package checkpoint

import (
	"errors"
	"regexp"
)

var trainingCheckpointID = regexp.MustCompile(`^[A-Za-z0-9._-]{1,128}$`)

// TrainingReport carries application facts. The receiver derives the worker
// identity from its authenticated pod and assigns the receipt time.
type TrainingReport struct {
	Kind         string `json:"kind"`
	CheckpointID string `json:"checkpoint_id,omitempty"`
	Step         *int64 `json:"step,omitempty"`
}

func (r TrainingReport) Validate() error {
	if r.Step != nil && *r.Step < 0 {
		return errors.New("step must be nonnegative")
	}
	switch r.Kind {
	case "checkpoint", "resume":
		if !trainingCheckpointID.MatchString(r.CheckpointID) || r.CheckpointID == "." || r.CheckpointID == ".." {
			return errors.New("invalid checkpoint identifier")
		}
	case "progress":
		if r.Step == nil || r.CheckpointID != "" {
			return errors.New("progress requires a step and no checkpoint identifier")
		}
	default:
		return errors.New("unknown training report kind")
	}
	return nil
}
