// SPDX-License-Identifier: Apache-2.0

package httpapi

import (
	"testing"
	"time"

	"github.com/kubeneuron/kubeneuron/pkg/types"
)

func TestAgentSourceAcceptsAnUnreadableJournal(t *testing.T) {
	now := time.Now()
	for _, tc := range []struct {
		row  types.AgentSignalSource
		want bool
	}{
		{types.AgentSignalSource{Source: "nvidia-kmsg", Class: types.ClassXIDApp, Reason: "journal-unreadable", ReadAt: now}, true},
		{types.AgentSignalSource{Source: "amd-kmsg", Class: types.ClassPCIe, Reason: "journal-unreadable", ReadAt: now}, true},
		{types.AgentSignalSource{Source: "nvidia-kmsg", Class: types.ClassXIDApp, Reason: "kernel-log-unavailable", ReadAt: now}, true},
		{types.AgentSignalSource{Source: "nvidia-dcgm", Class: types.ClassXIDApp, Reason: "journal-unreadable", ReadAt: now}, false},
		{types.AgentSignalSource{Source: "nvidia-kmsg", Class: types.ClassXIDApp, Available: true, Reason: "journal-unreadable", ReadAt: now}, false},
		// Only the controller says why a node has no mount.
		{types.AgentSignalSource{Source: "nvidia-kmsg", Class: types.ClassXIDApp, Reason: "os-image-not-recognized", ReadAt: now}, false},
	} {
		if got := validAgentSource(tc.row); got != tc.want {
			t.Errorf("%+v: valid = %t, want %t", tc.row, got, tc.want)
		}
	}
}
