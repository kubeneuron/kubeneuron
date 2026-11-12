// SPDX-License-Identifier: LicenseRef-KubeNeuron

package nvidia

import (
	"context"
	"testing"

	"github.com/kubeneuron/kubeneuron/internal/agent/nvml"
	"github.com/kubeneuron/kubeneuron/pkg/accelerator"
)

func TestFactoryBuildsAdapterFromRegistryConfig(t *testing.T) {
	built, err := Factory(context.Background(), accelerator.Config{
		NodeName: "gpu-node-1",
		Options:  map[string]string{OptionPartitionTopology: string(PartitionTopologyNone)},
		Host:     &nvml.Fake{},
	})
	if err != nil {
		t.Fatalf("Factory() error = %v", err)
	}
	adapter, ok := built.(*Adapter)
	if !ok {
		t.Fatalf("Factory() returned %T, want *Adapter", built)
	}
	if adapter.cfg.PartitionTopology != PartitionTopologyNone || adapter.cfg.NodeName != "gpu-node-1" {
		t.Fatalf("config not carried through: %+v", adapter.cfg)
	}
}

func TestFactoryFailsClosed(t *testing.T) {
	for name, cfg := range map[string]accelerator.Config{
		"no host":          {NodeName: "gpu-node-1"},
		"foreign host":     {NodeName: "gpu-node-1", Host: "not a driver"},
		"no node name":     {Host: &nvml.Fake{}},
		"unknown topology": {NodeName: "gpu-node-1", Host: &nvml.Fake{}, Options: map[string]string{OptionPartitionTopology: "bogus"}},
	} {
		t.Run(name, func(t *testing.T) {
			if built, err := Factory(context.Background(), cfg); err == nil || built != nil {
				t.Fatalf("Factory() = %v, %v; want nil adapter and an error", built, err)
			}
		})
	}
}
