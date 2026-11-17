// SPDX-License-Identifier: Apache-2.0

package aws

import (
	"testing"
	"time"
)

func TestRepairSettleWindow(t *testing.T) {
	now := time.Now()
	for _, tc := range []struct {
		name string
		age  time.Duration
		ok   bool
	}{
		{"recent", 30*time.Minute - time.Nanosecond, false},
		{"boundary", 30 * time.Minute, true},
		{"old", time.Hour, true},
		{"future", -time.Second, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stamp := now.Add(-tc.age)
			if err := settledSince(now, &stamp); (err == nil) != tc.ok {
				t.Fatalf("settledSince: %v", err)
			}
		})
	}
	if settledSince(now, nil) == nil {
		t.Fatal("missing timestamp accepted")
	}
}
