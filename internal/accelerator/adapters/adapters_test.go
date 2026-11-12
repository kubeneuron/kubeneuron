// SPDX-License-Identifier: LicenseRef-KubeNeuron

package adapters

import (
	"testing"

	"github.com/kubeneuron/kubeneuron/pkg/accelerator"
)

func TestNVIDIAAdapterIsRegistered(t *testing.T) {
	if !accelerator.Registered(accelerator.VendorNVIDIA) {
		t.Fatalf("NVIDIA adapter is not registered; registered: %v", accelerator.Vendors())
	}
}
