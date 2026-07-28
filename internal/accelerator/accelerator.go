// Package accelerator contains the vendor-neutral contract between the
// control plane and accelerator-specific adapters. It deliberately does not
// replace the existing NVIDIA wire types yet: adapters translate this model
// at the integration boundary.
package accelerator

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// Vendor identifies the manufacturer or provider that owns an accelerator
// runtime. The type remains open so an adapter can support a new vendor
// without a core-package change.
type Vendor string

const (
	VendorNVIDIA Vendor = "nvidia"
	VendorAMD    Vendor = "amd"
	VendorIntel  Vendor = "intel"
	VendorGoogle Vendor = "google"
)

// Valid reports whether v can identify an adapter. It intentionally permits
// vendors beyond the built-in constants for future adapters.
func (v Vendor) Valid() bool { return strings.TrimSpace(string(v)) != "" }

// Family identifies the broad accelerator architecture exposed by a device.
type Family string

const (
	FamilyGPU Family = "gpu"
	FamilyTPU Family = "tpu"
)

// Valid reports whether f is one of the accelerator families currently
// understood by the common model.
func (f Family) Valid() bool {
	return f == FamilyGPU || f == FamilyTPU
}

// DeviceKind distinguishes hardware that can be independently recovered from
// a logical portion of that hardware. A partition must never be assumed to be
// resettable just because its parent device is resettable.
type DeviceKind string

const (
	DevicePhysical  DeviceKind = "physical"
	DevicePartition DeviceKind = "partition"
)

// Valid reports whether k is a supported device kind.
func (k DeviceKind) Valid() bool {
	return k == DevicePhysical || k == DevicePartition
}

// TargetScope describes which part of a node an action or health check
// applies to.
type TargetScope string

const (
	ScopeNode           TargetScope = "node"
	ScopePhysicalDevice TargetScope = "physical-device"
	ScopePartition      TargetScope = "partition"
)

// Valid reports whether s is a supported action target scope.
func (s TargetScope) Valid() bool {
	return s == ScopeNode || s == ScopePhysicalDevice || s == ScopePartition
}

// scopeForDeviceKind converts an inventory topology kind to its target scope.
func scopeForDeviceKind(kind DeviceKind) TargetScope {
	if kind == DevicePartition {
		return ScopePartition
	}
	return ScopePhysicalDevice
}

// Device is one physical accelerator or a logical partition provided by it.
// ID must be stable within the adapter's vendor runtime (for example, a GPU
// UUID or a TPU slice identifier), rather than an ephemeral ordinal.
type Device struct {
	ID               string            `json:"id"`
	Kind             DeviceKind        `json:"kind"`
	Family           Family            `json:"family"`
	Model            string            `json:"model,omitempty"`
	ParentID         string            `json:"parent_id,omitempty"`
	PartitionProfile string            `json:"partition_profile,omitempty"`
	Attributes       map[string]string `json:"attributes,omitempty"`
}

// Inventory is the adapter-owned accelerator view of a single node for one
// vendor. A node with, for example, NVIDIA GPUs and Google TPUs has one
// Inventory per adapter/vendor rather than a mixed topology.
type Inventory struct {
	NodeName       string    `json:"node_name"`
	Vendor         Vendor    `json:"vendor"`
	DriverVersion  string    `json:"driver_version,omitempty"`
	RuntimeVersion string    `json:"runtime_version,omitempty"`
	ObservedAt     time.Time `json:"observed_at"`
	Devices        []Device  `json:"devices"`
}

// Validate checks the topology invariants needed to safely distinguish a
// physical device from a partition. It does not require at least one device:
// a valid inventory can report that an adapter is installed but currently sees
// no attached hardware.
func (i Inventory) Validate() error {
	if strings.TrimSpace(i.NodeName) == "" {
		return fmt.Errorf("accelerator inventory: node name is required")
	}
	if !i.Vendor.Valid() {
		return fmt.Errorf("accelerator inventory: vendor is required")
	}

	devices := make(map[string]Device, len(i.Devices))
	for _, device := range i.Devices {
		if strings.TrimSpace(device.ID) == "" {
			return fmt.Errorf("accelerator inventory: device ID is required")
		}
		if _, exists := devices[device.ID]; exists {
			return fmt.Errorf("accelerator inventory: duplicate device ID %q", device.ID)
		}
		if !device.Kind.Valid() {
			return fmt.Errorf("accelerator inventory: device %q has invalid kind %q", device.ID, device.Kind)
		}
		if !device.Family.Valid() {
			return fmt.Errorf("accelerator inventory: device %q has invalid family %q", device.ID, device.Family)
		}
		switch device.Kind {
		case DevicePhysical:
			if device.ParentID != "" {
				return fmt.Errorf("accelerator inventory: physical device %q cannot have a parent", device.ID)
			}
		case DevicePartition:
			if strings.TrimSpace(device.ParentID) == "" {
				return fmt.Errorf("accelerator inventory: partition %q requires a physical parent", device.ID)
			}
		}
		devices[device.ID] = device
	}

	for _, device := range i.Devices {
		if device.Kind != DevicePartition {
			continue
		}
		parent, exists := devices[device.ParentID]
		if !exists {
			return fmt.Errorf("accelerator inventory: partition %q references missing parent %q", device.ID, device.ParentID)
		}
		if parent.Kind != DevicePhysical {
			return fmt.Errorf("accelerator inventory: partition %q parent %q is not physical", device.ID, device.ParentID)
		}
		if parent.Family != device.Family {
			return fmt.Errorf("accelerator inventory: partition %q family %q does not match parent %q family %q", device.ID, device.Family, parent.ID, parent.Family)
		}
	}
	return nil
}

// Device returns the device with id from this inventory.
func (i Inventory) Device(id string) (Device, bool) {
	for _, device := range i.Devices {
		if device.ID == id {
			return device, true
		}
	}
	return Device{}, false
}

// Target is a vendor-neutral node, physical-device, or partition target.
// DeviceID is empty only for node-scoped targets.
type Target struct {
	NodeName string      `json:"node_name"`
	Vendor   Vendor      `json:"vendor"`
	DeviceID string      `json:"device_id,omitempty"`
	Scope    TargetScope `json:"scope"`
}

// Validate checks that target has the identity required for its scope.
func (t Target) Validate() error {
	if strings.TrimSpace(t.NodeName) == "" {
		return fmt.Errorf("accelerator target: node name is required")
	}
	if !t.Vendor.Valid() {
		return fmt.Errorf("accelerator target: vendor is required")
	}
	if !t.Scope.Valid() {
		return fmt.Errorf("accelerator target: invalid scope %q", t.Scope)
	}
	if t.Scope == ScopeNode && t.DeviceID != "" {
		return fmt.Errorf("accelerator target: node scope cannot include a device ID")
	}
	if t.Scope != ScopeNode && strings.TrimSpace(t.DeviceID) == "" {
		return fmt.Errorf("accelerator target: %s scope requires a device ID", t.Scope)
	}
	return nil
}

// TargetFor returns a target tied to this inventory. An empty device ID
// denotes the whole node. The inventory is validated first so callers cannot
// accidentally obtain a partition target from a malformed topology.
func (i Inventory) TargetFor(deviceID string) (Target, error) {
	if err := i.Validate(); err != nil {
		return Target{}, err
	}
	if deviceID == "" {
		return Target{NodeName: i.NodeName, Vendor: i.Vendor, Scope: ScopeNode}, nil
	}
	device, found := i.Device(deviceID)
	if !found {
		return Target{}, fmt.Errorf("accelerator inventory: device %q not found on node %q", deviceID, i.NodeName)
	}
	return Target{
		NodeName: i.NodeName,
		Vendor:   i.Vendor,
		DeviceID: device.ID,
		Scope:    scopeForDeviceKind(device.Kind),
	}, nil
}

// Action is a semantic recovery or verification operation. Adapters declare
// the scopes they support; the core must not infer that a reset is available
// for every vendor or for a partition just because it is available for a
// physical device.
type Action string

const (
	ActionEvacuateWorkloads  Action = "evacuate-workloads"
	ActionQuarantineNode     Action = "quarantine-node"
	ActionResetDevice        Action = "reset-device"
	ActionRestartRuntime     Action = "restart-runtime"
	ActionRebootNode         Action = "reboot-node"
	ActionCollectDiagnostics Action = "collect-diagnostics"
	ActionVerifyHealth       Action = "verify-health"
	ActionReplaceNode        Action = "replace-node"
)

// Valid reports whether action is a semantic action currently known to the
// common model.
func (a Action) Valid() bool {
	switch a {
	case ActionEvacuateWorkloads, ActionQuarantineNode, ActionResetDevice,
		ActionRestartRuntime, ActionRebootNode, ActionCollectDiagnostics,
		ActionVerifyHealth, ActionReplaceNode:
		return true
	default:
		return false
	}
}

// ActionCapability declares that an adapter can carry out Action for one or
// more target scopes. A capability is declarative only; it does not authorize
// execution and does not bypass the controller's safety gates.
type ActionCapability struct {
	Action Action        `json:"action"`
	Scopes []TargetScope `json:"scopes"`
}

// CapabilitySet is the collection of action support declared by one adapter
// on one node/runtime profile.
type CapabilitySet []ActionCapability

// Validate checks for malformed or ambiguous capability declarations.
func (set CapabilitySet) Validate() error {
	seen := make(map[Action]map[TargetScope]struct{})
	for _, capability := range set {
		if !capability.Action.Valid() {
			return fmt.Errorf("accelerator capabilities: invalid action %q", capability.Action)
		}
		if len(capability.Scopes) == 0 {
			return fmt.Errorf("accelerator capabilities: action %q has no scopes", capability.Action)
		}
		if seen[capability.Action] == nil {
			seen[capability.Action] = make(map[TargetScope]struct{})
		}
		for _, scope := range capability.Scopes {
			if !scope.Valid() {
				return fmt.Errorf("accelerator capabilities: action %q has invalid scope %q", capability.Action, scope)
			}
			if _, duplicate := seen[capability.Action][scope]; duplicate {
				return fmt.Errorf("accelerator capabilities: duplicate %q capability for %q", capability.Action, scope)
			}
			seen[capability.Action][scope] = struct{}{}
		}
	}
	return nil
}

// Supports reports whether action is declared for scope.
func (set CapabilitySet) Supports(action Action, scope TargetScope) bool {
	for _, capability := range set {
		if capability.Action != action {
			continue
		}
		for _, supported := range capability.Scopes {
			if supported == scope {
				return true
			}
		}
	}
	return false
}

// SupportsTarget reports whether action is declared for target's scope.
func (set CapabilitySet) SupportsTarget(action Action, target Target) bool {
	return set.Supports(action, target.Scope)
}

// HealthStatus is the portable result of a point-in-time health probe.
type HealthStatus string

const (
	HealthUnknown   HealthStatus = "unknown"
	HealthHealthy   HealthStatus = "healthy"
	HealthDegraded  HealthStatus = "degraded"
	HealthUnhealthy HealthStatus = "unhealthy"
)

// HealthReport contains adapter evidence from a health check. A returned
// error means the probe itself could not be completed; it is distinct from an
// unhealthy result.
type HealthReport struct {
	Target    Target            `json:"target"`
	Status    HealthStatus      `json:"status"`
	CheckedAt time.Time         `json:"checked_at"`
	Evidence  map[string]string `json:"evidence,omitempty"`
}

// DetectionClass is a normalized, vendor-neutral failure category. Adapters
// retain raw vendor codes in Detection.Evidence and map them to this class.
type DetectionClass string

const (
	DetectionDeviceUnavailable DetectionClass = "device-unavailable"
	DetectionMemory            DetectionClass = "memory"
	DetectionInterconnect      DetectionClass = "interconnect"
	DetectionThermal           DetectionClass = "thermal"
	DetectionPower             DetectionClass = "power"
	DetectionDriver            DetectionClass = "driver"
	DetectionDiagnostic        DetectionClass = "diagnostic"
)

// Severity communicates the urgency assigned by an adapter's detector.
type Severity string

const (
	SeverityInfo     Severity = "info"
	SeverityWarning  Severity = "warning"
	SeverityCritical Severity = "critical"
)

// Detection is one normalized observation from an adapter. ID must be stable
// across a retried delivery when the source supplies an event identity.
type Detection struct {
	ID         string            `json:"id,omitempty"`
	Target     Target            `json:"target"`
	Class      DetectionClass    `json:"class"`
	Severity   Severity          `json:"severity"`
	ObservedAt time.Time         `json:"observed_at"`
	Evidence   map[string]string `json:"evidence,omitempty"`
}

// InventoryProvider reports adapter-owned inventory.
type InventoryProvider interface {
	Inventory(context.Context) (Inventory, error)
}

// CapabilityProvider reports the actions that are safe and implemented for
// the adapter's current runtime profile.
type CapabilityProvider interface {
	Capabilities(context.Context) (CapabilitySet, error)
}

// HealthChecker performs a bounded health probe for a target.
type HealthChecker interface {
	CheckHealth(context.Context, Target) (HealthReport, error)
}

// Detector performs a bounded detection pass. It is suitable for polling
// sources such as exporter metrics or provider APIs.
type Detector interface {
	Detect(context.Context) ([]Detection, error)
}

// DetectionWatcher is optional for adapters with an event stream. It is kept
// separate from Detector so polling-only adapters are not forced to create a
// synthetic stream.
type DetectionWatcher interface {
	WatchDetections(context.Context) (<-chan Detection, error)
}

// Adapter is the minimum contract an accelerator integration exposes to the
// core. Event streaming is optional through DetectionWatcher.
type Adapter interface {
	InventoryProvider
	CapabilityProvider
	HealthChecker
	Detector

	Vendor() Vendor
}
