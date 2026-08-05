// Package nvidia adapts the NVIDIA agent driver to the vendor-neutral
// accelerator contract. It intentionally works with the existing narrow
// nvml.GPUDriver interface: that interface inventories physical GPUs and has
// no MIG discovery API, so this adapter never invents MIG partitions.
package nvidia

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/kubeneuron/kubeneuron/internal/accelerator"
	"github.com/kubeneuron/kubeneuron/internal/agent/nvml"
	"github.com/kubeneuron/kubeneuron/internal/detect"
	"github.com/kubeneuron/kubeneuron/pkg/types"
)

// PartitionTopology records the partition-safety result supplied by a
// runtime-profile preflight. GPUDriver exposes physical GPU inventory only;
// when a driver additionally implements the optional topology probe below,
// the adapter uses that current evidence instead of a configured assertion.
//
// Unknown is the safe default. In that state the adapter reports physical
// devices for observation, but does not claim that a physical reset is safe.
// None may be used only after an external runtime preflight has verified that
// every NVIDIA GPU is unpartitioned. MIG and Other explicitly deny physical
// device reset until a future adapter can enumerate and protect all affected
// partitions. The production nvidia-smi driver supplies current MIG-mode
// evidence, so a manually configured None cannot override it.
type PartitionTopology string

const (
	PartitionTopologyUnknown PartitionTopology = "unknown"
	PartitionTopologyNone    PartitionTopology = "none"
	PartitionTopologyMIG     PartitionTopology = "mig"
	PartitionTopologyOther   PartitionTopology = "other"
)

func (p PartitionTopology) valid() bool {
	switch p {
	case PartitionTopologyUnknown, PartitionTopologyNone, PartitionTopologyMIG, PartitionTopologyOther:
		return true
	default:
		return false
	}
}

// Config identifies the local NVIDIA runtime. RuntimeVersion is descriptive
// profile metadata until a separately reviewed runtime probe attests it; it
// is never guessed from an image tag. DriverVersion is a fallback only for
// drivers without the optional current-version probe; the production
// nvidia-smi driver supplies its actual version on every preflight.
type Config struct {
	NodeName       string
	DriverVersion  string
	RuntimeVersion string

	// PartitionTopology must be None before the adapter declares a physical
	// GPU reset capability. The default is Unknown and fails closed.
	PartitionTopology PartitionTopology

	// Now supplies observation timestamps. It is injectable for deterministic
	// tests; nil uses time.Now.
	Now func() time.Time
}

// PreflightReadiness describes whether this adapter has enough current,
// observation-only evidence to expose a node for the indicated level of
// automation. It deliberately distinguishes a healthy runtime that is safe
// only to observe from one that is eligible for its declared physical-device
// actions.
type PreflightReadiness string

const (
	// PreflightEligible means inventory and node liveness succeeded and the
	// adapter's topology-safe physical-device reset capability is available.
	PreflightEligible PreflightReadiness = "eligible"
	// PreflightObservedOnly means the node can be inventoried and health
	// checked, but the adapter must not offer physical-device reset. Unknown
	// or partitioned topology intentionally lands here rather than becoming
	// eligible through a generic capability declaration.
	PreflightObservedOnly PreflightReadiness = "observed-only"
	// PreflightBlocked means a required inventory, capability, or node health
	// probe did not produce evidence that can safely be used for automation.
	PreflightBlocked PreflightReadiness = "blocked"
)

// PreflightReport is an observation-only snapshot of the local NVIDIA
// runtime. Reasons explain why the status is not Eligible; probe failures are
// captured here rather than being silently converted into an optimistic
// capability. This is not a GPU Operator or DCGM diagnostic report.
//
// Capabilities is retained for status/reporting. Callers deciding whether to
// execute an action must use Allows: reset eligibility is deliberately kept
// private so a caller cannot turn an unknown or MIG topology into a
// resettable one by adding a capability to this public slice.
type PreflightReport struct {
	Readiness    PreflightReadiness        `json:"readiness"`
	Reasons      []string                  `json:"reasons,omitempty"`
	Topology     PartitionTopology         `json:"partition_topology"`
	Inventory    accelerator.Inventory     `json:"inventory"`
	Capabilities accelerator.CapabilitySet `json:"capabilities,omitempty"`
	NodeHealth   accelerator.HealthReport  `json:"node_health"`

	nodeName                    string
	physicalDeviceIDs           map[string]struct{}
	physicalDeviceResetEligible bool
}

// Ready reports whether this snapshot is eligible for all actions that the
// adapter has declared safe. Observed-only nodes still expose their
// read-only capabilities through Allows.
func (r PreflightReport) Ready() bool { return r.Readiness == PreflightEligible }

// Allows is the capability gate for this preflight snapshot. It is
// intentionally stricter than CapabilitySet.SupportsTarget for a physical
// reset: reset is allowed only when this particular preflight established an
// explicitly unpartitioned topology. Therefore an external or stale
// capability value cannot make Unknown, MIG, or another partition topology
// resettable.
func (r PreflightReport) Allows(action accelerator.Action, target accelerator.Target) bool {
	if err := target.Validate(); err != nil {
		return false
	}
	if r.Readiness == PreflightBlocked || r.nodeName == "" ||
		target.NodeName != r.nodeName || target.Vendor != accelerator.VendorNVIDIA {
		return false
	}
	if target.Scope == accelerator.ScopePartition {
		// GPUDriver has no partition inventory. A preflight must never claim
		// it can authorize a target it could not observe.
		return false
	}
	if target.Scope == accelerator.ScopePhysicalDevice {
		if _, observed := r.physicalDeviceIDs[target.DeviceID]; !observed {
			return false
		}
	}
	if action == accelerator.ActionResetDevice {
		return r.physicalDeviceResetEligible &&
			target.Scope == accelerator.ScopePhysicalDevice &&
			r.Capabilities.SupportsTarget(action, target)
	}
	return r.Capabilities.SupportsTarget(action, target)
}

// Adapter implements accelerator.Adapter for the existing NVIDIA agent
// runtime. It does not execute actions itself; capabilities state only the
// operations that the GPUDriver makes safe and available to the shared
// action path.
type Adapter struct {
	cfg    Config
	driver nvml.GPUDriver
	now    func() time.Time
}

// partitionTopologyProber is an optional narrow extension implemented by
// drivers that can query current partition mode. It deliberately returns a
// string so the low-level nvml package does not import this adapter package.
// Drivers without it retain the configured fail-closed topology assertion.
type partitionTopologyProber interface {
	PartitionTopology(context.Context) (string, error)
}

// driverVersionProber is an optional current-driver attestation. It is kept
// separate from GPUDriver so existing non-NVIDIA and test drivers do not gain
// an accidental runtime-identity contract.
type driverVersionProber interface {
	DriverVersion(context.Context) (string, error)
}

// resetCapabilityProber reports whether the platform can reset a GPU's PCI
// function at all. Optional for the same reason as the probes above.
type resetCapabilityProber interface {
	ResetCapability(ctx context.Context, index int) (nvml.ResetCapability, error)
}

var _ accelerator.Adapter = (*Adapter)(nil)
var _ accelerator.DetectionWatcher = (*Adapter)(nil)

// New constructs a NVIDIA adapter. It requires the node identity because a
// device UUID alone is not sufficient to form a safe accelerator target.
func New(cfg Config, driver nvml.GPUDriver) (*Adapter, error) {
	if strings.TrimSpace(cfg.NodeName) == "" {
		return nil, fmt.Errorf("nvidia adapter: node name is required")
	}
	if driver == nil {
		return nil, fmt.Errorf("nvidia adapter: GPU driver is required")
	}
	if cfg.PartitionTopology == "" {
		cfg.PartitionTopology = PartitionTopologyUnknown
	}
	if !cfg.PartitionTopology.valid() {
		return nil, fmt.Errorf("nvidia adapter: unsupported partition topology %q", cfg.PartitionTopology)
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	return &Adapter{cfg: cfg, driver: driver, now: now}, nil
}

// Vendor identifies this adapter's runtime owner.
func (*Adapter) Vendor() accelerator.Vendor { return accelerator.VendorNVIDIA }

// Inventory translates GPUDriver's physical NVIDIA inventory into the common
// model. The current GPUDriver has no MIG/partition enumeration; reporting a
// partition here would therefore be unsafe and is deliberately unsupported.
func (a *Adapter) Inventory(ctx context.Context) (accelerator.Inventory, error) {
	topology, err := a.partitionTopology(ctx)
	if err != nil {
		return accelerator.Inventory{}, err
	}
	driverVersion, err := a.driverVersion(ctx)
	if err != nil {
		return accelerator.Inventory{}, err
	}
	return a.inventory(ctx, topology, driverVersion)
}

func (a *Adapter) inventory(ctx context.Context, topology PartitionTopology, driverVersion string) (accelerator.Inventory, error) {
	gpus, err := a.driver.ListGPUs(ctx)
	if err != nil {
		return accelerator.Inventory{}, fmt.Errorf("nvidia adapter: list GPUs: %w", err)
	}

	devices := make([]accelerator.Device, 0, len(gpus))
	seen := make(map[string]struct{}, len(gpus))
	for _, gpu := range gpus {
		id := strings.TrimSpace(gpu.UUID)
		if id == "" {
			return accelerator.Inventory{}, fmt.Errorf("nvidia adapter: GPU index %d has no stable UUID", gpu.Index)
		}
		if _, duplicate := seen[id]; duplicate {
			return accelerator.Inventory{}, fmt.Errorf("nvidia adapter: duplicate GPU UUID %q", id)
		}
		seen[id] = struct{}{}
		devices = append(devices, accelerator.Device{
			ID:     id,
			Kind:   accelerator.DevicePhysical,
			Family: accelerator.FamilyGPU,
			Model:  strings.TrimSpace(gpu.Model),
			Attributes: map[string]string{
				"nvidia.com/gpu-index":          strconv.Itoa(gpu.Index),
				"nvidia.com/partition-topology": string(topology),
			},
		})
	}

	inventory := accelerator.Inventory{
		NodeName:       a.cfg.NodeName,
		Vendor:         accelerator.VendorNVIDIA,
		DriverVersion:  driverVersion,
		RuntimeVersion: a.cfg.RuntimeVersion,
		ObservedAt:     a.now(),
		Devices:        devices,
	}
	if err := inventory.Validate(); err != nil {
		return accelerator.Inventory{}, fmt.Errorf("nvidia adapter: invalid physical inventory: %w", err)
	}
	return inventory, nil
}

// Capabilities returns only operations represented by the current agent
// runtime. Kubernetes workload evacuation/quarantine, reboot, and diagnostic
// collection are intentionally absent because GPUDriver cannot perform them.
//
// A physical reset is declared only for a preflight-verified unpartitioned
// runtime. It is never inferred for a MIG/unknown topology and is never
// offered for a partition target.
func (a *Adapter) Capabilities(ctx context.Context) (accelerator.CapabilitySet, error) {
	topology, err := a.partitionTopology(ctx)
	if err != nil {
		return nil, err
	}
	return a.capabilities(ctx, topology)
}

func (a *Adapter) capabilities(ctx context.Context, topology PartitionTopology) (accelerator.CapabilitySet, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	capabilities := accelerator.CapabilitySet{
		{
			Action: accelerator.ActionVerifyHealth,
			Scopes: []accelerator.TargetScope{
				accelerator.ScopeNode,
				accelerator.ScopePhysicalDevice,
			},
		},
	}
	if topology == PartitionTopologyNone && a.platformCanReset(ctx) {
		capabilities = append(capabilities, accelerator.ActionCapability{
			Action: accelerator.ActionResetDevice,
			Scopes: []accelerator.TargetScope{accelerator.ScopePhysicalDevice},
		})
	}
	if err := capabilities.Validate(); err != nil {
		return nil, fmt.Errorf("nvidia adapter: invalid capabilities: %w", err)
	}
	return capabilities, nil
}

// platformCanReset reports whether the machine can actually reset a GPU.
//
// An unpartitioned topology and a healthy driver are not enough: on a
// virtualized instance the hypervisor withholds the PCI reset entirely, and the
// only symptom is a reset that fails after a playbook has already cordoned and
// drained the node. Declaring the capability only when the kernel exposes a
// reset turns that into an honest ladder that ends in reboot or replacement.
//
// A driver that cannot answer keeps the previous behaviour rather than losing
// the capability: the fake driver and any future driver without the probe must
// not be silently downgraded. The reset gate and the node-side preflight both
// still apply.
func (a *Adapter) platformCanReset(ctx context.Context) bool {
	_, ok := a.resetCapability(ctx)
	return ok
}

// resetCapability returns an operator-facing explanation alongside the verdict.
func (a *Adapter) resetCapability(ctx context.Context) (string, bool) {
	prober, ok := a.driver.(resetCapabilityProber)
	if !ok {
		return "", true
	}
	devices, err := a.driver.ListGPUs(ctx)
	if err != nil {
		return fmt.Sprintf("PCI reset probe could not enumerate GPUs: %v", err), false
	}
	if len(devices) == 0 {
		return "PCI reset probe found no GPUs", false
	}
	for _, device := range devices {
		capability, err := prober.ResetCapability(ctx, device.Index)
		if err != nil {
			return fmt.Sprintf("PCI reset probe failed for GPU %d: %v", device.Index, err), false
		}
		if !capability.Supported {
			// One device without a reset is enough: the machine as a whole
			// cannot be trusted to perform one, and per-device capability is
			// not something the report models.
			return fmt.Sprintf("GPU %d cannot be reset: %s", device.Index, capability.Detail), false
		}
	}
	return "", true
}

// Preflight collects the current NVIDIA inventory, declared capabilities, and
// a node-scoped liveness check into one fail-closed observation snapshot. It
// does not execute an action, modify the node, or infer GPU Operator/DCGM
// health. Individual probe failures are retained as reasons so callers can
// report a useful blocked status while treating the result as ineligible.
//
// All three observations are attempted even when an earlier one fails. A
// partial report is more useful to an operator and does not weaken the gate:
// any failed required probe leaves the result Blocked.
func (a *Adapter) Preflight(ctx context.Context) PreflightReport {
	topology, topologyErr := a.partitionTopology(ctx)
	driverVersion, driverVersionErr := a.driverVersion(ctx)
	report := PreflightReport{
		Readiness: PreflightBlocked,
		Topology:  topology,
		nodeName:  a.cfg.NodeName,
	}
	if topologyErr != nil {
		report.Reasons = append(report.Reasons, fmt.Sprintf("partition topology probe failed: %v", topologyErr))
	}
	if driverVersionErr != nil {
		report.Reasons = append(report.Reasons, fmt.Sprintf("driver version probe failed: %v", driverVersionErr))
	}

	inventory, inventoryErr := a.inventory(ctx, topology, driverVersion)
	if inventoryErr != nil {
		report.Reasons = append(report.Reasons, fmt.Sprintf("inventory probe failed: %v", inventoryErr))
	} else {
		report.Inventory = inventory
		if err := validatePreflightInventory(inventory, a.cfg.NodeName); err != nil {
			report.Reasons = append(report.Reasons, err.Error())
		} else {
			report.physicalDeviceIDs = physicalDeviceIDs(inventory)
		}
	}

	capabilities, capabilitiesErr := a.capabilities(ctx, topology)
	if capabilitiesErr != nil {
		report.Reasons = append(report.Reasons, fmt.Sprintf("capability probe failed: %v", capabilitiesErr))
	} else {
		report.Capabilities = capabilities
		if err := capabilities.Validate(); err != nil {
			report.Reasons = append(report.Reasons, fmt.Sprintf("invalid capability report: %v", err))
		}
	}

	nodeTarget := accelerator.Target{
		NodeName: a.cfg.NodeName,
		Vendor:   accelerator.VendorNVIDIA,
		Scope:    accelerator.ScopeNode,
	}
	nodeHealth, healthErr := a.CheckHealth(ctx, nodeTarget)
	if healthErr != nil {
		report.Reasons = append(report.Reasons, fmt.Sprintf("node health probe failed: %v", healthErr))
	} else {
		report.NodeHealth = nodeHealth
		if err := validateNodeHealth(nodeHealth, nodeTarget); err != nil {
			report.Reasons = append(report.Reasons, err.Error())
		}
	}

	if len(report.Reasons) != 0 {
		return report
	}

	// This must remain an independent topology check, not merely a capability
	// lookup. Current driver evidence, when available, wins over the configured
	// assertion; unknown/MIG/other topology can never become resettable if a
	// caller later appends a reset capability to the report.
	if topology != PartitionTopologyNone {
		report.Readiness = PreflightObservedOnly
		report.Reasons = append(report.Reasons, fmt.Sprintf(
			"physical-device reset unavailable: partition topology %q is not explicitly unpartitioned",
			topology,
		))
		return report
	}
	if !capabilities.Supports(accelerator.ActionResetDevice, accelerator.ScopePhysicalDevice) {
		report.Readiness = PreflightObservedOnly
		reason := "physical-device reset unavailable: adapter did not declare reset capability"
		// Say why. "Capability not declared" sends an operator looking for a
		// configuration mistake; "the hypervisor exposes no PCI reset" tells
		// them the machine is the wrong shape and no setting will help.
		if detail, _ := a.resetCapability(ctx); detail != "" {
			reason = "physical-device reset unavailable: " + detail
		}
		report.Reasons = append(report.Reasons, reason)
		return report
	}

	report.Readiness = PreflightEligible
	report.physicalDeviceResetEligible = true
	return report
}

func (a *Adapter) partitionTopology(ctx context.Context) (PartitionTopology, error) {
	probe, ok := a.driver.(partitionTopologyProber)
	if !ok {
		return a.cfg.PartitionTopology, nil
	}
	raw, err := probe.PartitionTopology(ctx)
	if err != nil {
		return PartitionTopologyUnknown, err
	}
	topology := PartitionTopology(strings.ToLower(strings.TrimSpace(raw)))
	if !topology.valid() {
		return PartitionTopologyUnknown, fmt.Errorf("unsupported partition topology %q", raw)
	}
	return topology, nil
}

func (a *Adapter) driverVersion(ctx context.Context) (string, error) {
	probe, ok := a.driver.(driverVersionProber)
	if !ok {
		return a.cfg.DriverVersion, nil
	}
	version, err := probe.DriverVersion(ctx)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(version) == "" || strings.TrimSpace(version) != version {
		return "", fmt.Errorf("invalid driver version %q", version)
	}
	return version, nil
}

func validatePreflightInventory(inventory accelerator.Inventory, nodeName string) error {
	if err := inventory.Validate(); err != nil {
		return fmt.Errorf("invalid inventory report: %w", err)
	}
	if inventory.NodeName != nodeName {
		return fmt.Errorf("inventory report belongs to node %q, not local node %q", inventory.NodeName, nodeName)
	}
	if inventory.Vendor != accelerator.VendorNVIDIA {
		return fmt.Errorf("inventory report vendor %q is not nvidia", inventory.Vendor)
	}
	if inventory.ObservedAt.IsZero() {
		return fmt.Errorf("inventory report has no observation time")
	}
	if len(inventory.Devices) == 0 {
		return fmt.Errorf("inventory report contains no NVIDIA GPUs")
	}
	for _, device := range inventory.Devices {
		if device.Family != accelerator.FamilyGPU || device.Kind != accelerator.DevicePhysical {
			return fmt.Errorf("inventory report contains unsupported NVIDIA device topology for %q", device.ID)
		}
	}
	return nil
}

func validateNodeHealth(report accelerator.HealthReport, target accelerator.Target) error {
	if report.Target != target {
		return fmt.Errorf("node health report target does not match preflight node")
	}
	if report.CheckedAt.IsZero() {
		return fmt.Errorf("node health report has no check time")
	}
	if report.Status != accelerator.HealthHealthy {
		return fmt.Errorf("node health is %q", report.Status)
	}
	return nil
}

func physicalDeviceIDs(inventory accelerator.Inventory) map[string]struct{} {
	ids := make(map[string]struct{}, len(inventory.Devices))
	for _, device := range inventory.Devices {
		if device.Kind == accelerator.DevicePhysical {
			ids[device.ID] = struct{}{}
		}
	}
	return ids
}

// CheckHealth reports NVIDIA driver liveness. A physical target additionally
// verifies that its stable UUID is still in the physical GPU inventory. This
// is not a DCGM diagnostic and must not be interpreted as one.
func (a *Adapter) CheckHealth(ctx context.Context, target accelerator.Target) (accelerator.HealthReport, error) {
	if err := a.validateTarget(target); err != nil {
		return accelerator.HealthReport{}, err
	}
	report := accelerator.HealthReport{
		Target:    target,
		CheckedAt: a.now(),
		Evidence: map[string]string{
			"probe": "nvidia-gpu-driver-liveness",
		},
	}
	if err := a.driver.Healthy(ctx); err != nil {
		if ctx.Err() != nil {
			return report, ctx.Err()
		}
		// GPUDriver reports a failed bounded liveness probe as an error. The
		// probe completed, so surface a negative health result rather than
		// pretending the device remains healthy.
		report.Status = accelerator.HealthUnhealthy
		report.Evidence["driver_error"] = err.Error()
		return report, nil
	}

	if target.Scope == accelerator.ScopeNode {
		report.Status = accelerator.HealthHealthy
		return report, nil
	}

	gpus, err := a.driver.ListGPUs(ctx)
	if err != nil {
		return report, fmt.Errorf("nvidia adapter: verify physical GPU %q: %w", target.DeviceID, err)
	}
	for _, gpu := range gpus {
		if gpu.UUID == target.DeviceID {
			report.Status = accelerator.HealthHealthy
			report.Evidence["physical_device_present"] = "true"
			return report, nil
		}
	}
	report.Status = accelerator.HealthUnhealthy
	report.Evidence["physical_device_present"] = "false"
	return report, nil
}

// Detect has no polling source in the existing GPUDriver. NVIDIA XID events
// are supplied through WatchDetections when the runtime implements its event
// stream; the existing SMI driver correctly reports that it has no stream and
// relies on the agent's kmsg path instead.
func (*Adapter) Detect(ctx context.Context) ([]accelerator.Detection, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return nil, nil
}

// WatchDetections converts NVIDIA-agent XID events from GPUDriver's optional
// event source to normalized detections. Events for another node are dropped:
// accepting them would create a target the local adapter does not own.
func (a *Adapter) WatchDetections(ctx context.Context) (<-chan accelerator.Detection, error) {
	events, err := a.driver.WatchEvents(ctx)
	if err != nil {
		return nil, fmt.Errorf("nvidia adapter: watch events: %w", err)
	}
	if events == nil {
		return nil, fmt.Errorf("nvidia adapter: watch events returned a nil stream")
	}

	detections := make(chan accelerator.Detection, 64)
	go func() {
		defer close(detections)
		for {
			select {
			case <-ctx.Done():
				return
			case event, open := <-events:
				if !open {
					return
				}
				detection, ok := a.DetectionFromAgentEvent(event)
				if !ok {
					continue
				}
				select {
				case detections <- detection:
				case <-ctx.Done():
					return
				}
			}
		}
	}()
	return detections, nil
}

// DetectionFromAgentEvent converts an existing NVIDIA agent XID event when
// it belongs to this adapter's node. A present GPU UUID is a physical target:
// GPUDriver's AgentEvent contract resolves it from ListGPUs, which exposes
// physical devices only. A missing UUID remains node-scoped rather than
// guessing an ordinal or fabricating a partition identity.
func (a *Adapter) DetectionFromAgentEvent(event types.AgentEvent) (accelerator.Detection, bool) {
	if event.Node != a.cfg.NodeName {
		return accelerator.Detection{}, false
	}
	return DetectionFromAgentEvent(event)
}

// DetectionFromAgentEvent maps an existing agent XID event into the common
// detection vocabulary. Unknown XIDs and malformed events are ignored, as in
// the existing detector: the adapter must not manufacture a failure class.
func DetectionFromAgentEvent(event types.AgentEvent) (accelerator.Detection, bool) {
	if strings.TrimSpace(event.Node) == "" || event.XID <= 0 || event.Timestamp.IsZero() {
		return accelerator.Detection{}, false
	}
	info, known := detect.ClassifyXID(event.XID)
	if !known {
		return accelerator.Detection{}, false
	}
	class, mapped := normalizedClass(info.Class)
	if !mapped {
		return accelerator.Detection{}, false
	}

	target := accelerator.Target{
		NodeName: event.Node,
		Vendor:   accelerator.VendorNVIDIA,
		Scope:    accelerator.ScopeNode,
	}
	if id := strings.TrimSpace(event.GPUUUID); id != "" {
		target.DeviceID = id
		target.Scope = accelerator.ScopePhysicalDevice
	}
	evidence := map[string]string{
		"vendor_event":         "nvidia-xid",
		"xid":                  strconv.Itoa(event.XID),
		"xid_name":             info.Name,
		"legacy_problem_class": string(info.Class),
		"gpu_index":            strconv.Itoa(event.GPUIndex),
	}
	if event.Raw != "" {
		evidence["raw"] = event.Raw
	}
	return accelerator.Detection{
		ID:         event.EventID,
		Target:     target,
		Class:      class,
		Severity:   accelerator.Severity(info.Severity),
		ObservedAt: event.Timestamp,
		Evidence:   evidence,
	}, true
}

func (a *Adapter) validateTarget(target accelerator.Target) error {
	if err := target.Validate(); err != nil {
		return err
	}
	if target.Vendor != accelerator.VendorNVIDIA {
		return fmt.Errorf("nvidia adapter: target vendor %q is not nvidia", target.Vendor)
	}
	if target.NodeName != a.cfg.NodeName {
		return fmt.Errorf("nvidia adapter: target node %q is not local node %q", target.NodeName, a.cfg.NodeName)
	}
	if target.Scope == accelerator.ScopePartition {
		return fmt.Errorf("nvidia adapter: partition health is unsupported without MIG inventory")
	}
	return nil
}

func normalizedClass(class types.ProblemClass) (accelerator.DetectionClass, bool) {
	switch class {
	case types.ClassECCDBE, types.ClassECCSBERate, types.ClassECCContained,
		types.ClassRowRemapOK, types.ClassRowRemapFailure, types.ClassRowRemapBudget:
		return accelerator.DetectionMemory, true
	case types.ClassNVLink, types.ClassPCIe:
		return accelerator.DetectionInterconnect, true
	case types.ClassFellOffBus, types.ClassGPULost:
		return accelerator.DetectionDeviceUnavailable, true
	case types.ClassGSPError, types.ClassDriverHang:
		return accelerator.DetectionDriver, true
	case types.ClassThermal:
		return accelerator.DetectionThermal, true
	case types.ClassPower:
		return accelerator.DetectionPower, true
	case types.ClassXIDApp, types.ClassDiagFailure:
		// The portable model has no application-only class. An XID still is
		// a driver diagnostic observation, and retains its original class in
		// evidence so policy never loses that distinction.
		return accelerator.DetectionDiagnostic, true
	default:
		return "", false
	}
}
