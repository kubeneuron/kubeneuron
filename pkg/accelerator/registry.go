// SPDX-License-Identifier: Apache-2.0

package accelerator

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"
)

// Config carries adapter-neutral construction settings from the agent to an
// adapter factory. An adapter that needs more reads Options or Host.
type Config struct {
	// NodeName is the Kubernetes node the adapter observes. Adapters must
	// refuse an empty node name: a device ID alone is not a safe target.
	NodeName string
	// DriverVersion and RuntimeVersion are configured fallbacks; an adapter
	// that can observe the real versions should prefer its observation.
	DriverVersion  string
	RuntimeVersion string
	// Options carries adapter-specific settings. Keys are defined by each
	// adapter; an adapter must reject a value it does not understand rather
	// than ignore it.
	Options map[string]string
	// Host is an optional handle to host tooling the agent has already
	// resolved, such as a driver client. Its concrete type is a contract
	// between the agent and one adapter; an adapter that receives a Host it
	// does not recognise must fail closed.
	Host any
	// Now supplies observation timestamps; nil means time.Now.
	Now func() time.Time
}

// Factory builds an Adapter from construction Config.
type Factory func(ctx context.Context, cfg Config) (Adapter, error)

var registry = map[Vendor]Factory{}

// Register makes an adapter buildable by vendor. Adapters are registered from
// one registration file in the KubeNeuron agent (see the LICENSE file), at
// init time. An invalid vendor, a nil factory, or a duplicate registration is
// a programming error and panics at startup.
func Register(vendor Vendor, factory Factory) {
	if !vendor.Valid() {
		panic("accelerator: adapter vendor must not be empty")
	}
	if factory == nil {
		panic("accelerator: adapter factory must not be nil")
	}
	if _, exists := registry[vendor]; exists {
		panic(fmt.Sprintf("accelerator: adapter %q already registered", vendor))
	}
	registry[vendor] = factory
}

// Build constructs the adapter registered for vendor. An unregistered vendor
// fails closed with a message listing the vendors that are available.
func Build(ctx context.Context, vendor Vendor, cfg Config) (Adapter, error) {
	factory, ok := registry[vendor]
	if !ok {
		return nil, fmt.Errorf("no accelerator adapter registered for vendor %q (registered: %s)", vendor, registeredList())
	}
	adapter, err := factory(ctx, cfg)
	if err != nil {
		return nil, err
	}
	if adapter == nil {
		return nil, fmt.Errorf("accelerator adapter for vendor %q returned no adapter", vendor)
	}
	if got := adapter.Vendor(); got != vendor {
		return nil, fmt.Errorf("accelerator adapter registered for vendor %q reports vendor %q", vendor, got)
	}
	return adapter, nil
}

// Registered reports whether an adapter is registered for vendor.
func Registered(vendor Vendor) bool {
	_, ok := registry[vendor]
	return ok
}

// Vendors returns the registered vendors in sorted order.
func Vendors() []Vendor {
	vendors := make([]Vendor, 0, len(registry))
	for vendor := range registry {
		vendors = append(vendors, vendor)
	}
	sort.Slice(vendors, func(i, j int) bool { return vendors[i] < vendors[j] })
	return vendors
}

func registeredList() string {
	vendors := Vendors()
	if len(vendors) == 0 {
		return "none"
	}
	names := make([]string, len(vendors))
	for i, vendor := range vendors {
		names[i] = string(vendor)
	}
	return strings.Join(names, ", ")
}
