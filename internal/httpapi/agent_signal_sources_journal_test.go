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
		// Only an available kernel source says which kernel log it reads.
		{types.AgentSignalSource{Source: "nvidia-kmsg", Class: types.ClassXIDApp, Available: true, Via: "journal", ReadAt: now}, true},
		{types.AgentSignalSource{Source: "amd-kmsg", Class: types.ClassPCIe, Available: true, Via: "kmsg", ReadAt: now}, true},
		{types.AgentSignalSource{Source: "nvidia-kmsg", Class: types.ClassXIDApp, Available: true, ReadAt: now}, true},
		{types.AgentSignalSource{Source: "nvidia-kmsg", Class: types.ClassXIDApp, Available: true, Via: "dmesg", ReadAt: now}, false},
		{types.AgentSignalSource{Source: "nvidia-kmsg", Class: types.ClassXIDApp, Reason: "journal-unreadable", Via: "journal", ReadAt: now}, false},
		{types.AgentSignalSource{Source: "nvidia-dcgm", Class: types.ClassXIDApp, Available: true, Via: "kmsg", ReadAt: now}, false},
		{types.AgentSignalSource{Source: "aws-neuron-sysfs", Class: types.ClassAcceleratorFault, Available: true, Via: "journal", ReadAt: now}, false},
	} {
		if got := validAgentSource(tc.row); got != tc.want {
			t.Errorf("%+v: valid = %t, want %t", tc.row, got, tc.want)
		}
	}
}
