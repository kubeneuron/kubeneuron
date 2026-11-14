// SPDX-License-Identifier: Apache-2.0

package accelerator

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
)

type stubAdapter struct{ vendor Vendor }

func (s stubAdapter) Inventory(context.Context) (Inventory, error)        { return Inventory{}, nil }
func (s stubAdapter) Capabilities(context.Context) (CapabilitySet, error) { return nil, nil }
func (s stubAdapter) CheckHealth(context.Context, Target) (HealthReport, error) {
	return HealthReport{}, nil
}
func (s stubAdapter) Detect(context.Context) ([]Detection, error) { return nil, nil }
func (s stubAdapter) Vendor() Vendor                              { return s.vendor }

// withRegistry isolates a test from registrations made by other tests.
func withRegistry(t *testing.T) {
	t.Helper()
	saved := registry
	registry = map[Vendor]Factory{}
	t.Cleanup(func() { registry = saved })
}

func TestRegisterAndBuild(t *testing.T) {
	withRegistry(t)
	var got Config
	Register("acme", func(_ context.Context, cfg Config) (Adapter, error) {
		got = cfg
		return stubAdapter{vendor: "acme"}, nil
	})
	if !Registered("acme") {
		t.Fatal("acme should be registered")
	}
	adapter, err := Build(context.Background(), "acme", Config{NodeName: "gpu-1", Options: map[string]string{"k": "v"}})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if adapter.Vendor() != "acme" || got.NodeName != "gpu-1" || got.Options["k"] != "v" {
		t.Fatalf("factory did not receive config or adapter vendor is wrong: %+v %q", got, adapter.Vendor())
	}
	if vendors := Vendors(); len(vendors) != 1 || vendors[0] != "acme" {
		t.Fatalf("Vendors() = %v", vendors)
	}
}

func TestBuildFailsClosed(t *testing.T) {
	withRegistry(t)
	if _, err := Build(context.Background(), "missing", Config{}); err == nil || !strings.Contains(err.Error(), "registered: none") {
		t.Fatalf("unregistered vendor: err = %v", err)
	}

	factoryErr := errors.New("no runtime")
	Register("broken", func(context.Context, Config) (Adapter, error) { return nil, factoryErr })
	if _, err := Build(context.Background(), "broken", Config{}); !errors.Is(err, factoryErr) {
		t.Fatalf("factory error not returned: %v", err)
	}

	Register("nil", func(context.Context, Config) (Adapter, error) { return nil, nil })
	if _, err := Build(context.Background(), "nil", Config{}); err == nil {
		t.Fatal("nil adapter must fail closed")
	}

	Register("liar", func(context.Context, Config) (Adapter, error) { return stubAdapter{vendor: "other"}, nil })
	if _, err := Build(context.Background(), "liar", Config{}); err == nil {
		t.Fatal("adapter reporting another vendor must fail closed")
	}
}

func TestRegisterPanicsOnProgrammingErrors(t *testing.T) {
	withRegistry(t)
	ok := func(context.Context, Config) (Adapter, error) { return stubAdapter{vendor: "acme"}, nil }
	for name, register := range map[string]func(){
		"empty vendor":     func() { Register("", ok) },
		"malformed vendor": func() { Register("Acme Corp", ok) },
		"nil factory":      func() { Register("acme", nil) },
		"duplicate":        func() { Register("dup", ok); Register("dup", ok) },
	} {
		t.Run(name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Fatal("expected panic")
				}
			}()
			register()
		})
	}
}

func TestVendorValidAcceptsAnyWellFormedID(t *testing.T) {
	for _, vendor := range []Vendor{VendorNVIDIA, VendorAMD, VendorIntel, VendorGoogle, "aws-trainium", "acme", "a", "x9", Vendor(strings.Repeat("a", MaxVendorLength))} {
		if !vendor.Valid() {
			t.Errorf("Vendor(%q).Valid() = false, want true", vendor)
		}
	}
	for _, vendor := range []Vendor{"", "AMD", "-amd", "amd-", "am d", "amd_x", "amd.x", "ämd", Vendor(strings.Repeat("a", MaxVendorLength+1))} {
		if vendor.Valid() {
			t.Errorf("Vendor(%q).Valid() = true, want false", vendor)
		}
	}
}

func TestBuildPassesErrNotPresentThrough(t *testing.T) {
	withRegistry(t)
	Register("acme", func(context.Context, Config) (Adapter, error) {
		return nil, fmt.Errorf("no /dev/acme0: %w", ErrNotPresent)
	})
	_, err := Build(context.Background(), "acme", Config{NodeName: "gpu-1"})
	if !errors.Is(err, ErrNotPresent) {
		t.Fatalf("Build() error = %v, want ErrNotPresent", err)
	}
}
