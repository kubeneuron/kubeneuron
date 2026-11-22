// SPDX-License-Identifier: Apache-2.0

package checkpoint

import (
	"strings"
	"testing"
)

func TestTrainingReport(t *testing.T) {
	zero, negative := int64(0), int64(-1)
	for _, tc := range []struct {
		report TrainingReport
		valid  bool
	}{
		{TrainingReport{Kind: "checkpoint", CheckpointID: strings.Repeat("a", 128)}, true},
		{TrainingReport{Kind: "resume", CheckpointID: "epoch_1.part-2"}, true},
		{TrainingReport{Kind: "progress", Step: &zero}, true},
		{TrainingReport{Kind: "progress"}, false},
		{TrainingReport{Kind: "progress", Step: &negative}, false},
		{TrainingReport{Kind: "progress", Step: &zero, CheckpointID: "epoch"}, false},
		{TrainingReport{Kind: "running"}, false},
	} {
		if got := tc.report.Validate() == nil; got != tc.valid {
			t.Errorf("%+v valid=%v", tc.report, got)
		}
	}
	for _, id := range []string{"", ".", "..", "../epoch", "https://example.org/file", "dir/file", "dir\\file", "epoch\n", strings.Repeat("a", 129)} {
		if (TrainingReport{Kind: "checkpoint", CheckpointID: id}).Validate() == nil {
			t.Errorf("accepted identifier %q", id)
		}
	}
}
