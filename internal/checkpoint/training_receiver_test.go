// SPDX-License-Identifier: Apache-2.0

package checkpoint

import (
	"testing"
	"time"
)

func TestTrainingReceiverLimitsEachPod(t *testing.T) {
	h := new(TrainingReceiver)
	now := time.Now()
	for range 60 {
		if !h.allow("first", now) {
			t.Fatal("initial budget exhausted early")
		}
	}
	if h.allow("first", now) || !h.allow("second", now) || !h.allow("first", now.Add(time.Minute)) {
		t.Fatal("incorrect pod budget or window rollover")
	}
}
