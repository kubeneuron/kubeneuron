// SPDX-License-Identifier: LicenseRef-KubeNeuron

// Package adapters is the adapter registration file named in the LICENSE file.
// It lists every accelerator adapter built into the KubeNeuron agent and
// contains nothing else. Adding an adapter is one import and one
// accelerator.Register line here; under the KubeNeuron License (Section 2.9)
// those additions are the "registration code" an Adapter Build may contain.
package adapters

import (
	"github.com/kubeneuron/kubeneuron/internal/accelerator/nvidia"
	"github.com/kubeneuron/kubeneuron/pkg/accelerator"
)

func init() {
	accelerator.Register(accelerator.VendorNVIDIA, nvidia.Factory)
}
