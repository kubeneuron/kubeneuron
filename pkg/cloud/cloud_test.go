// SPDX-License-Identifier: Apache-2.0

package cloud

import (
	"context"
	"errors"
	"strings"
	"testing"
)

type stubProvider struct {
	name         string
	capabilities Capabilities
}

func (s stubProvider) Name() string                                 { return s.name }
func (s stubProvider) InstanceID(providerID string) (string, error) { return providerID, nil }
func (s stubProvider) CheckRecycle(context.Context, string) error   { return nil }
func (s stubProvider) Recycle(context.Context, string) error        { return nil }
func (s stubProvider) Replace(context.Context, string) error        { return nil }
func (s stubProvider) Capabilities() Capabilities                   { return s.capabilities }

// withRegistry isolates a test from registrations made by other tests.
func withRegistry(t *testing.T) {
	t.Helper()
	saved := registry
	registry = map[string]registration{}
	t.Cleanup(func() { registry = saved })
}

func TestRegisterBuildAndDeclaredCapabilities(t *testing.T) {
	withRegistry(t)
	declared := Capabilities{ReinitializeInPlace: true, Replace: true}
	var got Config
	Register("acme-cloud", declared, func(_ context.Context, cfg Config) (Provider, error) {
		got = cfg
		return stubProvider{name: "acme-cloud", capabilities: declared}, nil
	})

	if !Registered("acme-cloud") {
		t.Fatal("acme-cloud should be registered")
	}
	capabilities, ok := DeclaredCapabilities("acme-cloud")
	if !ok || capabilities != declared {
		t.Fatalf("DeclaredCapabilities = %+v, %v; want %+v without building the provider", capabilities, ok, declared)
	}
	if got.Region != "" {
		t.Fatal("DeclaredCapabilities must not construct the provider")
	}

	provider, err := Build(context.Background(), "acme-cloud", Config{Region: "eu-west-1", Options: map[string]string{"k": "v"}})
	if err != nil {
		t.Fatalf("Build() = %v", err)
	}
	if provider.Name() != "acme-cloud" || provider.Capabilities() != declared {
		t.Fatalf("Build() returned %+v", provider)
	}
	if got.Region != "eu-west-1" || got.Options["k"] != "v" {
		t.Fatalf("factory received %+v, want the Build config", got)
	}
}

func TestBuildPassesFactoryErrorsThrough(t *testing.T) {
	withRegistry(t)
	boom := errors.New("no credentials")
	Register("acme-cloud", Capabilities{}, func(context.Context, Config) (Provider, error) { return nil, boom })
	if _, err := Build(context.Background(), "acme-cloud", Config{}); !errors.Is(err, boom) {
		t.Fatalf("Build() error = %v, want the factory error", err)
	}
}

func TestLookupOfAnUnregisteredProviderFailsClosed(t *testing.T) {
	withRegistry(t)
	if Registered("ghost") {
		t.Fatal("ghost must not be registered")
	}
	if capabilities, ok := DeclaredCapabilities("ghost"); ok || capabilities != (Capabilities{}) {
		t.Fatalf("DeclaredCapabilities(ghost) = %+v, %v; want no capabilities and false", capabilities, ok)
	}
	_, err := Build(context.Background(), "ghost", Config{})
	if err == nil || !strings.Contains(err.Error(), `unsupported cloud provider "ghost" (supported: none)`) {
		t.Fatalf("Build(ghost) error = %v", err)
	}

	Register("zeta", Capabilities{}, func(context.Context, Config) (Provider, error) { return stubProvider{name: "zeta"}, nil })
	Register("alpha", Capabilities{}, func(context.Context, Config) (Provider, error) { return stubProvider{name: "alpha"}, nil })
	_, err = Build(context.Background(), "ghost", Config{})
	if err == nil || !strings.Contains(err.Error(), "(supported: alpha, zeta)") {
		t.Fatalf("Build(ghost) error = %v, want the sorted list of registered providers", err)
	}
}

func TestRegisterPanicsOnProgrammingErrors(t *testing.T) {
	withRegistry(t)
	ok := func(context.Context, Config) (Provider, error) { return stubProvider{name: "acme"}, nil }
	for name, register := range map[string]func(){
		"empty name":  func() { Register("", Capabilities{}, ok) },
		"nil factory": func() { Register("acme", Capabilities{}, nil) },
		"duplicate":   func() { Register("dup", Capabilities{}, ok); Register("dup", Capabilities{Replace: true}, ok) },
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
	// The first registration of a duplicated name is kept unchanged.
	if capabilities, _ := DeclaredCapabilities("dup"); capabilities.Replace {
		t.Fatal("a rejected duplicate registration must not overwrite the first one")
	}
}
