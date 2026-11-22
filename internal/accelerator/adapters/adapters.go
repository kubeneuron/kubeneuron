// SPDX-License-Identifier: Apache-2.0

// Package adapters is the accelerator adapter registration file. It lists
// every accelerator adapter built into the KubeNeuron agent and contains
// nothing else. Adding an adapter is one import and one accelerator.Register
// line here.
package adapters

import (
	"github.com/kubeneuron/kubeneuron/internal/accelerator/nvidia"
	"github.com/kubeneuron/kubeneuron/pkg/accelerator"
)

func init() {
	accelerator.Register(accelerator.VendorNVIDIA, nvidia.Factory)
}
