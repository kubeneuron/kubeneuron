// Package cloud is the provider-neutral seam for node recycle/replace
// remediation. It exists so no workflow, controller, or platform code ever
// names a specific cloud: each Provider parses its own node providerID scheme,
// drives its own instances, and declares which safe primitives it actually has.
//
// Providers register themselves by name. The controller builds one by name and
// the operator queries a provider's declared capabilities by name (without
// credentials, so a playbook can be gated at compile time). Adding a cloud is
// therefore a new provider package plus one registry line — never an edit here.
//
// The package imports only the standard library, so operator, controller, and
// platform can all consume the seam without pulling any cloud SDK; the concrete
// providers (and their SDKs) are linked only by the binaries that wire them.
package cloud

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
)

// ErrRecycleNotViable marks a definitive, per-instance verdict that
// reinitialize-in-place cannot work for this exact instance even though the
// provider declares the capability — e.g. an autoscaling-group member whose
// group would see the stopped instance as unhealthy and terminate it
// mid-recycle. Callers use errors.Is to distinguish this verdict (escalate to
// Replace now) from a transient lookup failure (retry).
var ErrRecycleNotViable = errors.New("recycle is not viable for this instance")

// Capabilities declares which node-remediation primitives a provider can
// perform safely. A provider that lacks a primitive must declare it false so
// the operator rejects a playbook that would depend on it, rather than the
// controller discovering the gap in the middle of an incident.
type Capabilities struct {
	// ReinitializeInPlace is true when the provider can stop and start the same
	// instance so its GPU passthrough is torn down and re-established without the
	// node moving — the RecycleNode primitive. A provider whose only "restart" is
	// a soft reboot must leave this false: a reboot does not reinitialize the
	// passthrough, so it would report a recycle that fixed nothing.
	ReinitializeInPlace bool
	// Replace is true when the provider can terminate the instance for an
	// autoscaler to replace it with a fresh one — the ReplaceNode primitive.
	Replace bool
}

// Provider is the provider-neutral cloud seam. Each implementation owns its own
// providerID scheme and its own instance operations; the rest of the system
// depends only on this interface.
type Provider interface {
	// Name is the registry key, matching spec.cloud.provider (e.g. "aws").
	Name() string
	// InstanceID extracts the cloud instance ID from a Kubernetes node
	// providerID written in this provider's own scheme. An unparseable or
	// foreign providerID fails closed with an error.
	InstanceID(providerID string) (string, error)
	// CheckRecycle reports whether Recycle can work for THIS instance, beyond
	// the provider's static capability declaration: capabilities are
	// provider-scoped, but viability is instance-scoped (an autoscaling-group
	// member cannot be stop/started under its group's health check). A
	// definitive no wraps ErrRecycleNotViable so the caller can escalate to
	// Replace immediately instead of discovering the gap by timeout; any other
	// error is a transient lookup failure.
	CheckRecycle(ctx context.Context, instanceID string) error
	// Recycle reinitializes the instance in place (e.g. stop then start),
	// bounded by the context deadline.
	Recycle(ctx context.Context, instanceID string) error
	// Replace terminates the instance for the autoscaler to replace.
	Replace(ctx context.Context, instanceID string) error
	// Capabilities declares which of the above primitives this provider can
	// perform safely. It must match the static declaration registered with the
	// provider's factory.
	Capabilities() Capabilities
}

// Config carries provider-agnostic construction settings. Region is a
// near-universal cloud concept; a provider that needs more reads Options.
type Config struct {
	// Region is the provider region (empty uses the provider's ambient default,
	// e.g. AWS_REGION for AWS).
	Region string
	// Options carries provider-specific settings for providers that need more
	// than a region. It is empty for AWS.
	Options map[string]string
}

// Factory builds a live Provider from construction Config.
type Factory func(ctx context.Context, cfg Config) (Provider, error)

// registration is a provider's factory plus its static capability declaration.
// The capabilities live here, without credentials, so the operator can gate a
// playbook at compile time without ever constructing a live client.
type registration struct {
	factory      Factory
	capabilities Capabilities
}

var registry = map[string]registration{}

// Register makes a provider buildable and its capabilities queryable by name.
// Providers call it from an init function. An empty name, a nil factory, or a
// duplicate registration is a programming error and panics at startup.
func Register(name string, capabilities Capabilities, factory Factory) {
	if name == "" {
		panic("cloud: provider name must not be empty")
	}
	if factory == nil {
		panic("cloud: provider factory must not be nil")
	}
	if _, exists := registry[name]; exists {
		panic(fmt.Sprintf("cloud: provider %q already registered", name))
	}
	registry[name] = registration{factory: factory, capabilities: capabilities}
}

// Build constructs the named provider. An unregistered name fails closed with a
// message listing the providers that are available.
func Build(ctx context.Context, name string, cfg Config) (Provider, error) {
	reg, ok := registry[name]
	if !ok {
		return nil, fmt.Errorf("unsupported cloud provider %q (supported: %s)", name, supportedList())
	}
	return reg.factory(ctx, cfg)
}

// DeclaredCapabilities returns a provider's static capability declaration
// without constructing it. The bool is false for an unregistered provider, so a
// caller can fail closed on an unknown cloud.
func DeclaredCapabilities(name string) (Capabilities, bool) {
	reg, ok := registry[name]
	return reg.capabilities, ok
}

// Registered reports whether a provider name is known.
func Registered(name string) bool {
	_, ok := registry[name]
	return ok
}

func supportedList() string {
	names := make([]string, 0, len(registry))
	for name := range registry {
		names = append(names, name)
	}
	sort.Strings(names)
	if len(names) == 0 {
		return "none"
	}
	return strings.Join(names, ", ")
}
