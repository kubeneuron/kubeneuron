// Package gpucatalog is the canonical, versioned catalog of the GPU
// observability signals KubeNeuron knows about.
//
// Five tiers with fixed meanings; three are assigned in the MVP:
//
//   - T0 baseline GPU telemetry: temperature, utilisation, memory-copy
//     utilisation, power, energy, framebuffer, clocks, PCIe link state and
//     PCIe byte rates.
//   - T1 GPU reliability telemetry: XID, ECC, retired pages, row remap,
//     NVLink, PCIe replay, clock-event (throttle) reasons, recovery actions.
//   - T2 DCGM effective-work / profiling telemetry (DCGM_FI_PROF_* other than
//     the baseline PCIe byte rates): deferred, no MVP entries.
//   - T3 serving / Triton telemetry (nv_inference_*): deferred, no MVP entries.
//   - T4 KubeNeuron control-plane telemetry: incident state, actions, gates,
//     recovered capacity and agent health, published by the controller and
//     agent themselves (kubeneuron_* series).
//
// T0/T1 entries are keyed by DCGM numeric field ID; T4 entries have no DCGM
// field and are keyed by their series name. Hardware (Tesla T4, A100, H100)
// is a separate support/evidence dimension and never a tier; in particular
// tier "T4" is the control plane and has nothing to do with the Tesla T4.
//
// Every entry has two sides:
//
//   - The SOURCE side: the DCGM field id and its canonical DCGM name, the
//     legacy names the same id has carried, the series name the pinned
//     dcgm-exporter would publish it under, whether the shipped exporter
//     profile actually lists that series, and the raw unit the source emits.
//     Source is honest about emission: only a field the shipped profile
//     lists has source dcgm-exporter; every other DCGM field has source
//     dcgm-field, meaning it is cataloged by id from NVIDIA documentation
//     and no exporter is asserted to emit it.
//   - The CANONICAL side: the normalised KubeNeuron series the field feeds
//     (kubeneuron_gpu_* for DCGM fields, Prometheus base units in the name,
//     _total on counters), the canonical unit, the factor from raw to
//     canonical, and the finite fixed labels that let several DCGM fields
//     merge into one canonical series (for example SM and memory clocks into
//     kubeneuron_gpu_clock_hertz{domain}).
//
// The canonical side is the CONTRACT the shipped recording rules
// (CanonicalRulePath, mirrored in DeployedRulePath) implement: each
// kubeneuron_gpu_* series is recorded from its source series with the stated
// factor and fixed labels, present when the source is scraped and absent
// otherwise. Recording a series proves nothing about hardware: the support
// cells are unchanged by it. Control-plane (T4) entries are the exception: their canonical series is
// the existing kubeneuron_* series itself, registered by internal/metrics.
//
// The Go tables in this file are the source of truth. config/gpu-metrics/
// and docs/reference-gpu-metrics.md are generated from them by
// hack/gpucatalog (Makefile: generate-gpucatalog / verify-gpucatalog) and
// must never be edited by hand.
//
// The catalog DESCRIBES signals. It drives no alert rule, no detector
// threshold, no controller decision and no remediation. DCGM profiling
// fields (T2), Triton / inference-serving metrics (T3), vGPU fields and any
// recovery-action automation keyed to catalog entries are deferred; see
// Deferred.
package gpucatalog

import (
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// Version is the catalog schema+content version. Bump the minor on added
// entries or added schema fields, the major on removed or re-keyed entries.
//
// v1.2.0 added the canonical side of every entry (series, kind, unit,
// factor, fixed labels) and the reserved canonical names; no key changed.
//
// v1.3.0 added the remaining Appendix A throttle-time fields (242, 243, 245)
// and the unrepairable-memory flag (1507), and split the DCGM source value:
// dcgm-exporter now means "listed in the shipped exporter profile" and the
// new dcgm-field value marks a field that is merely cataloged. No key changed.
//
// v1.4.0 made exporter exposure and rule consumption independent claims:
// exporter_exposed alone records that the shipped profile lists the series,
// and consumed_by lists only rule files and components that read it, never
// the profile itself. No key changed.
//
// v1.5.0 (v0.7 M2) expanded the shipped exporter profile to the reliability
// profile: exporter_exposed is now true for exactly the T0/T1 fields with a
// canonical mapping that are not deferred (ProfileEligible), and Validate
// enforces that equality. No consumer was added: the new profile entries were
// read by no rule until the recording-rule phase. No key changed.
//
// v1.6.0 (v0.7 M3, slice 1) added the canonical recording rules
// (CanonicalRuleConsumerPaths) as the consumer of every profiled field: each
// kubeneuron_gpu_* series is now recorded from its source series with the
// catalog's factor and fixed labels, wherever the source exists. The nine
// v0.6.1 series keep their legacy raw-alert consumers
// (LegacyRuleConsumerPaths) as well. Producing a series is not validating
// it: no hardware cell changed. No key changed.
//
// v1.7.0 (v0.7 M3, slice 2) added the derived-signal schema (DerivedSignal,
// Catalog.Derived): kubeneuron_gpu_* series computed from canonical series
// by the gpu-derived-* recording groups, each with its formula, inputs,
// unit, no-data semantics and an experimental flag, including the
// experimental fail-slow score, which only the shadow layer reads.
// Nothing reads a DCGM series directly, nothing synthesises a sample, and
// no aggregate health or error score is defined: the experimental fail-slow
// peer score is the only score, and nothing acts on it. GPU presence is
// not recorded. No key changed.
const Version = "v1.7.0"

// CanonicalContract is the statement every generated artifact carries about
// what a canonical mapping means. It is data, not prose, so the tests can
// assert the artifacts say it.
const CanonicalContract = "A canonical mapping is the contract the shipped recording rules implement: " +
	"it states the kubeneuron_gpu_* series, unit and fixed labels the rules derive from the source field. " +
	"Since v1.6.0 the canonical recording rules (configs/vmalert/gpu-recording-rules.yaml, mirrored in the deployed VMRule) record every mapped series from its source series, so a canonical series exists exactly when its source series is scraped; " +
	"an absent source yields no canonical sample, never a synthesised zero. A recorded series is not a validated one: the rules add no hardware evidence, and every support cell stands as the catalog states it. " +
	"The source side is likewise a description: a DCGM field is emitted by the pinned dcgm-exporter only when the shipped profile lists its series (profiled = yes, source = dcgm-exporter); " +
	"every other DCGM field has source = dcgm-field, which means it is cataloged by id from NVIDIA documentation and no exporter is asserted to emit it."

// Tier is the signal class.
type Tier string

const (
	TierT0 Tier = "T0" // baseline GPU telemetry
	TierT1 Tier = "T1" // GPU reliability telemetry
	TierT2 Tier = "T2" // DCGM effective-work / profiling (DCGM_FI_PROF_*) — deferred, may not be assigned
	TierT3 Tier = "T3" // serving / Triton (nv_inference_*) — deferred, may not be assigned
	TierT4 Tier = "T4" // KubeNeuron control-plane telemetry (never the Tesla T4 GPU)
)

// DeferredTiers are defined but may carry no MVP entry.
var DeferredTiers = []Tier{TierT2, TierT3}

// BaselinePROFFields are the only DCGM_FI_PROF_* fields admitted into the
// MVP: the PCIe byte-rate fields, which are baseline link-traffic telemetry
// (T0) rather than profiling. Every other DCGM_FI_PROF_* field is tier T2
// and deferred.
var BaselinePROFFields = map[int]string{
	1009: "DCGM_FI_PROF_PCIE_TX_BYTES",
	1010: "DCGM_FI_PROF_PCIE_RX_BYTES",
}

// Source is who emits the series, or, for a DCGM field nothing is shown to
// emit, where the catalog entry comes from.
type Source string

const (
	// SourceDCGMExporter: the shipped dcgm-exporter profile
	// (ShippedProfilePath) lists the series, so the pinned exporter is
	// configured to emit it. Exactly the ExporterExposed fields.
	SourceDCGMExporter Source = "dcgm-exporter"
	// SourceDCGMField: a DCGM field cataloged by numeric id and semantics
	// from NVIDIA documentation (dcgm_fields.h). No exporter is asserted to
	// emit it; it reaches a consumer only through a profile change or a
	// direct hostengine read such as the agent's dcgmi poll of field 230.
	// Every DCGM entry outside the shipped profile carries this source.
	SourceDCGMField  Source = "dcgm-field"
	SourceController Source = "kubeneuron-controller"
	SourceAgent      Source = "kubeneuron-agent"
)

// DCGMSources are the source values a T0/T1 entry may carry.
var DCGMSources = []Source{SourceDCGMExporter, SourceDCGMField}

// IsDCGM reports whether s is one of DCGMSources.
func (s Source) IsDCGM() bool { return s == SourceDCGMExporter || s == SourceDCGMField }

// Unit names a unit. CanonicalUnits is the closed set a consumer may rely
// on; raw units are whatever the source emits.
type Unit string

const (
	// Canonical units (Prometheus base units plus the dimensionless kinds).
	UnitCelsius     Unit = "celsius"
	UnitWatts       Unit = "watts"
	UnitJoules      Unit = "joules"
	UnitBytes       Unit = "bytes"
	UnitBytesPerSec Unit = "bytes_per_second" // a rate the source reports directly (PCIe byte rates); canonical, not raw-only
	UnitSeconds     Unit = "seconds"
	UnitHours       Unit = "hours" // derived only: GPU-hours of capacity (seconds / 3600)
	UnitHertz       Unit = "hertz"
	UnitRatio       Unit = "ratio"
	UnitCount       Unit = "count"
	UnitBool        Unit = "boolean"
	UnitCode        Unit = "code"
	UnitBitmask     Unit = "bitmask"

	// Raw-only units, never canonical.
	UnitMillijoules  Unit = "millijoules"
	UnitMegahertz    Unit = "megahertz"
	UnitMicroseconds Unit = "microseconds"
	UnitNanoseconds  Unit = "nanoseconds"
	UnitPercent      Unit = "percent"
	UnitMiB          Unit = "mebibytes"
)

// CanonicalUnits is the only set Canonical.Unit may use.
var CanonicalUnits = []Unit{UnitCelsius, UnitWatts, UnitJoules, UnitBytes, UnitBytesPerSec, UnitSeconds, UnitHours, UnitHertz, UnitRatio, UnitCount, UnitBool, UnitCode, UnitBitmask}

// unitSuffix is the name suffix a canonical series must carry for its unit
// (before _total on a counter). Dimensionless units carry no suffix.
var unitSuffix = map[Unit]string{
	UnitCelsius:     "_celsius",
	UnitWatts:       "_watts",
	UnitJoules:      "_joules",
	UnitBytes:       "_bytes",
	UnitBytesPerSec: "_bytes_per_second",
	UnitSeconds:     "_seconds",
	UnitHours:       "_hours",
	UnitHertz:       "_hertz",
	UnitRatio:       "_ratio",
}

// Kind is the Prometheus metric type.
type Kind string

const (
	KindGauge     Kind = "gauge"
	KindCounter   Kind = "counter"
	KindHistogram Kind = "histogram"
)

// Measure is the raw unit the source emits. Factor is always 1: raw is the
// reference the canonical factor is expressed against.
type Measure struct {
	Unit   Unit    `yaml:"unit" json:"unit"`
	Factor float64 `yaml:"factor,omitempty" json:"factor,omitempty"`
}

// Label is one fixed label on a canonical series. Fixed labels are how
// several DCGM field rows merge into one canonical metric: the row for
// DCGM_FI_DEV_SM_CLOCK carries domain="sm", the row for
// DCGM_FI_DEV_MEM_CLOCK carries domain="memory", and both feed
// kubeneuron_gpu_clock_hertz. Values are constants chosen here, never read
// from the source, so the label set is finite by construction.
type Label struct {
	Name  string `yaml:"name" json:"name"`
	Value string `yaml:"value" json:"value"`
}

// Canonical is the normalised KubeNeuron series a source field feeds. See
// CanonicalContract: this is what the shipped recording rules record from
// the source series when it exists, with no claim of hardware support.
type Canonical struct {
	// Series is the canonical Prometheus series name: kubeneuron_gpu_* with
	// the Prometheus base unit in the name and _total on counters.
	Series string `yaml:"series" json:"series"`
	// Kind is the canonical metric type. It must equal the source kind: a
	// gauge never becomes a counter by renaming, which is exactly why the
	// sticky last-XID gauge has no canonical counter (see ReservedCanonical).
	Kind Kind `yaml:"kind" json:"kind"`
	// Unit is the canonical unit, from CanonicalUnits.
	Unit Unit `yaml:"unit" json:"unit"`
	// Factor converts raw to canonical: canonical = raw * Factor. It is 1
	// when the raw unit already is canonical and 0 (omitted) when the
	// mapping is a decode rather than a scalar conversion (ValueLabels set).
	Factor float64 `yaml:"factor,omitempty" json:"factor,omitempty"`
	// Labels are the fixed labels this source row contributes, sorted by
	// name. Rows sharing a Series must have the same label names and
	// pairwise distinct label values.
	Labels []Label `yaml:"labels,omitempty" json:"labels,omitempty"`
	// ValueLabels are labels whose values the recording rule derives from
	// the raw value instead of fixing here (a bitmask decoded into one
	// series per bit). A mapping with ValueLabels is a decode: Factor is 0.
	ValueLabels []string `yaml:"value_labels,omitempty" json:"value_labels,omitempty"`
	// Note records where the mapping comes from when it is not a row of
	// Appendix A of the feature brief, or how a decode is meant to work.
	Note string `yaml:"note,omitempty" json:"note,omitempty"`
}

// Hardware names a GPU product line in the support/evidence dimension.
type Hardware string

const (
	HardwareT4   Hardware = "tesla-t4" // Turing; the only hardware a recorded run of this project has touched
	HardwareA100 Hardware = "a100"     // Ampere; declared from DCGM docs only
	HardwareH100 Hardware = "h100"     // Hopper; declared from DCGM docs only
)

// Support is the per-hardware status of one field.
type Support string

const (
	// SupportValidated: a recorded run of this project observed the live
	// value on that hardware. Field.Evidence must cite the run. No v0.7 run
	// exists; every validated cell today cites v0.6.0-era evidence.
	SupportValidated Support = "validated"
	// SupportDeclared: documented by NVIDIA for that hardware and, where
	// noted, enabled in the exporter profile — but never asserted by a run
	// of this project. Treat as unvalidated.
	SupportDeclared Support = "declared"
	// SupportNotApplicable: the hardware provably lacks the feature. Used
	// only where the NVIDIA documentation establishes it (NVLink and row
	// remapping absent on Turing, page retirement replaced on Ampere+). Where
	// a source does not settle the question the cell is SupportUnknown, never
	// an inferred not-applicable.
	SupportNotApplicable Support = "not-applicable"
	// SupportUnknown: not assessed or not establishable from a cited source.
	SupportUnknown Support = "unknown"
)

// Field is one catalog entry: a source series and the canonical series it
// feeds.
type Field struct {
	// Key is the stable catalog key: "dcgm:<field id>" for DCGM fields,
	// "kubeneuron:<series>" for control-plane series.
	Key string `yaml:"key" json:"key"`
	// DCGMFieldID is the dcgm_fields.h numeric id (what `dcgmi dmon -e`
	// takes). Zero for non-DCGM entries.
	DCGMFieldID int `yaml:"dcgm_field_id,omitempty" json:"dcgm_field_id,omitempty"`
	// DCGMField is the canonical DCGM name of the id: the C constant in the
	// DCGM 4.x headers. Empty for non-DCGM entries.
	DCGMField string `yaml:"dcgm_field,omitempty" json:"dcgm_field,omitempty"`
	// Aliases are the legacy DCGM names the same field id has carried. The
	// pinned dcgm-exporter's default CSV still uses them for several fields.
	Aliases []string `yaml:"aliases,omitempty" json:"aliases,omitempty"`
	// Series is the Prometheus series name the field appears under when it
	// is emitted: for DCGM fields, the name the pinned dcgm-exporter uses
	// for the id in its metrics CSV (a legacy alias where the field was
	// renamed), which is tracked separately from DCGMField for that reason.
	// Series says what name to expect, not that anything emits it: only
	// ExporterExposed asserts emission.
	Series string `yaml:"series" json:"series"`
	// SeriesAliases are other exporter series the same field id can appear
	// under (for example the current DCGM name on an exporter build that
	// accepts it). A consumer matching either name sees the same field.
	SeriesAliases []string `yaml:"series_aliases,omitempty" json:"series_aliases,omitempty"`
	// ExporterExposed is true only when Series is listed in the shipped
	// dcgm-exporter metrics profile (ShippedProfilePath), so the pinned
	// exporter is configured to emit it. Every other DCGM entry is cataloged
	// by id and semantics from NVIDIA documentation; whether an exporter
	// would emit it is not asserted. The test cross-checks this flag against
	// the shipped profile file so it cannot drift. Since v1.5.0 the shipped
	// profile is the reliability profile: Validate requires this flag to be
	// true for exactly the fields ProfileEligible admits.
	ExporterExposed bool `yaml:"exporter_exposed" json:"exporter_exposed"`
	// Source is SourceDCGMExporter exactly when ExporterExposed is true,
	// SourceDCGMField for every other DCGM entry, and a kubeneuron component
	// for T4; Validate enforces the correspondence.
	Source Source `yaml:"source" json:"source"`
	Kind   Kind   `yaml:"kind" json:"kind"`
	Tier   Tier   `yaml:"tier" json:"tier"`
	// Raw is the unit the source emits.
	Raw Measure `yaml:"raw" json:"raw"`
	// Canonical is the normalised series this field feeds, or nil when the
	// field has no canonical mapping; then CanonicalDeferred says why. For
	// T4 entries Canonical names the existing kubeneuron_* series itself.
	Canonical *Canonical `yaml:"canonical,omitempty" json:"canonical,omitempty"`
	// CanonicalDeferred is the reason a DCGM field feeds no canonical series
	// in this version. Required exactly when Canonical is nil.
	CanonicalDeferred string `yaml:"canonical_deferred,omitempty" json:"canonical_deferred,omitempty"`
	Help              string `yaml:"help" json:"help"`
	// Support is the per-hardware status. Required and total for DCGM
	// fields; omitted for control-plane series, which do not depend on GPU
	// hardware.
	// In YAML it is rendered as an ordered list (see render.go), so the
	// map itself is not encoded.
	Support map[Hardware]Support `yaml:"-" json:"support,omitempty"`
	// Evidence cites the recorded run behind every validated cell. It must
	// point at a public source: a path in this source tree or a tagged
	// release note, never a private or transient artifact.
	Evidence string `yaml:"evidence,omitempty" json:"evidence,omitempty"`
	// Deferred marks an entry cataloged for reference whose use is out of
	// scope for this version (no profile, no rule, no action). It stays in
	// the table so the field id and semantics are pinned, nothing more.
	Deferred string `yaml:"deferred,omitempty" json:"deferred,omitempty"`
	// ConsumedBy lists the shipped consumers that READ the series: rule files
	// (RuleConsumerPaths: the canonical recording rules, the legacy raw
	// alerts and the deployed VMRule) and kubeneuron components. It is a
	// claim independent of ExporterExposed: the exporter profile is never a
	// consumer, and a profiled field may have no consumer at all (the
	// profile can list a series before any rule reads it). Since v1.6.0
	// every profiled field is read by the canonical recording rules; the
	// nine v0.6.1 series are read by the legacy raw alerts as well.
	ConsumedBy []string `yaml:"consumed_by,omitempty" json:"consumed_by,omitempty"`
}

// XIDBucket is a coarse, bounded label value for XID codes. Buckets exist so
// a series can carry `xid_bucket` without one series per raw code, and so a
// label value never names an action: a bucket says which subsystem
// reported the code, nothing about what to do.
type XIDBucket struct {
	Name  string `yaml:"name" json:"name"`
	Codes []int  `yaml:"codes" json:"codes"`
	Help  string `yaml:"help" json:"help"`
}

// Deferral records scope considered and intentionally left out.
type Deferral struct {
	Area   string `yaml:"area" json:"area"`
	Reason string `yaml:"reason" json:"reason"`
}

// Reservation is a canonical series name the brief defines that no
// cataloged source may claim yet, with the reason. Validate rejects any
// field mapped to a reserved name.
type Reservation struct {
	Series string `yaml:"series" json:"series"`
	Reason string `yaml:"reason" json:"reason"`
}

// Catalog is the whole document the generator renders.
type Catalog struct {
	Version           string        `yaml:"version" json:"version"`
	CanonicalContract string        `yaml:"canonical_contract" json:"canonical_contract"`
	Fields            []Field       `yaml:"fields" json:"fields"`
	ReservedCanonical []Reservation `yaml:"reserved_canonical" json:"reserved_canonical"`
	XIDBuckets        []XIDBucket   `yaml:"xid_buckets" json:"xid_buckets"`
	Deferred          []Deferral    `yaml:"deferred" json:"deferred"`
	// Derived lists the derived signals the gpu-derived-* recording groups
	// compute from canonical series, plus the signals intentionally left
	// unrecorded (Deferred set).
	Derived []DerivedSignal `yaml:"derived" json:"derived"`
}

// XIDBucketOther is the catch-all bucket for any code not listed.
const XIDBucketOther = "other"

// Canonical series names the brief defines but this version does not map,
// so the reservation is visible to both the validator and a reader.
const (
	// CanonicalXIDEventsTotal is the XID event counter the brief's Health &
	// Errors board needs. The only XID field in the catalog, 230, is a sticky
	// last-value gauge; turning it into this counter would invent events.
	CanonicalXIDEventsTotal = "kubeneuron_gpu_xid_errors_total"
	// CanonicalRecoveryAction is the brief's name for field 1523, which this
	// version catalogs for id and semantics only.
	CanonicalRecoveryAction = "kubeneuron_gpu_recovery_action"
)

// ReservedCanonical lists canonical names no field may claim in this
// version. Both are explicit deferrals, not oversights.
var ReservedCanonical = []Reservation{
	{Series: CanonicalXIDEventsTotal, Reason: "needs an event counter source: DCGM_EXP_XID_ERRORS_TOTAL once confirmed to label each XID code, or a kernel-log (kmsg) watcher. DCGM_FI_DEV_XID_ERRORS (230) keeps only the last code for the hostengine's lifetime, so a repeat is invisible and a rate over it would count nothing or count one event forever. The gauge therefore maps to no canonical series; this counter stays reserved until a counter source is cataloged."},
	{Series: CanonicalRecoveryAction, Reason: "DCGM_FI_DEV_GPU_RECOVERY_ACTION (1523) is cataloged only: not enabled in any profile, not shown to be exposed by the pinned dcgm-exporter, unvalidated on every hardware and attached to no action. A canonical series for it is assigned when the field is first enabled, together with the enumeration of its values."},
}

var allHardware = []Hardware{HardwareT4, HardwareA100, HardwareH100}

// AllHardware returns the support-matrix columns in canonical order.
func AllHardware() []Hardware { return append([]Hardware(nil), allHardware...) }

// CanonicalLabelNames is the closed set of label names a canonical mapping
// may fix or derive. Identity and placement labels (cluster, node, gpu_uuid,
// pod, ...) are carried through from the source series by the recording
// rule and are never fixed in the catalog; a label that varies per device
// or per workload is not finite and may not appear here.
var CanonicalLabelNames = []string{"bits", "cause", "direction", "domain", "kind", "limit", "reason", "scope", "type"}

// Deferred is the scope this version explicitly does not cover.
var Deferred = []Deferral{
	{Area: "Tier T2: DCGM effective-work / profiling fields (DCGM_FI_PROF_*)", Reason: "require DCGM profiling support and exclusive access to hardware counters; not part of the MVP baseline. The only exception is the baseline PCIe byte-rate fields DCGM_FI_PROF_PCIE_TX_BYTES (1009) and DCGM_FI_PROF_PCIE_RX_BYTES (1010), which are link-traffic telemetry and sit in T0."},
	{Area: "Tier T3: Triton / inference-serving metrics (nv_inference_*)", Reason: "come from a different exporter with per-model cardinality; they need their own catalog once a serving stack exists in a reference environment. The kubeneuron_serving_* names in Appendix A of the brief are not cataloged here."},
	{Area: "vGPU fields (DCGM_FI_DEV_VGPU_*)", Reason: "no vGPU host in any validated environment; semantics differ per hypervisor."},
	{Area: "MIG-instance scoping of listed fields", Reason: "the only hardware this project has run on (Tesla T4) is MIG-incapable; instance-level labels stay unvalidated until an A100/H100 run exists."},
	{Area: "Recovery-action automation keyed to catalog entries", Reason: "the catalog describes signals only. Alert rules, detector thresholds, playbooks and controller behaviour are unchanged by it; wiring a field or XID bucket to an action is a separate, reviewed change. DCGM_FI_DEV_GPU_RECOVERY_ACTION (1523) is cataloged for its id and semantics only: it is not enabled in any profile, not shown to be exposed by the pinned dcgm-exporter, attached to no action, and mapped to no canonical series (kubeneuron_gpu_recovery_action is reserved)."},
	{Area: "XID event counter (kubeneuron_gpu_xid_errors_total)", Reason: "the brief derives XID rate from a counter per code. The catalog's only XID field, DCGM_FI_DEV_XID_ERRORS (230), is a sticky last-value gauge and is deliberately not mapped to that counter; the name is reserved until DCGM_EXP_XID_ERRORS_TOTAL or a kernel-log watcher is cataloged as the source."},
	{Area: "Alerts, consumers of the experimental fail-slow score, an aggregate health or error score and hardware evidence for the canonical and derived series", Reason: "the shipped recording rules (configs/vmalert/gpu-recording-rules.yaml and the deployed VMRule) normalise (gpu-canonical-* groups: each kubeneuron_gpu_* series is its source series times the catalog factor with the fixed labels, produced when the source exists and absent otherwise) and derive (gpu-derived-* groups: headroom, ratios, rates and hard-fault components computed from canonical series or from derived series declared earlier in the list, listed under derived). The only alerts reading them are the shadow-only ShadowGpu* rules (configs/vmalert/gpu-shadow-rules.yaml), which route to humans and trigger no remediation; the experimental fail-slow score (kubeneuron_gpu_fail_slow_score_ratio) is recorded from the derived ratios and read only by the Fail-slow Radar board and the shadow-only ShadowGpuFailSlow alert; no aggregate health or error score is defined because one number would hide which component moved, and recording a series validates nothing: every support cell stands as declared, validated (v0.6.0-era Tesla T4 evidence) or unknown exactly as before. Appendix A rows whose DCGM fields are not cataloged (NVLink byte rates 1011, 1012, which are DCGM_FI_PROF_* and tier T2; the exporter-computed DCGM_EXP_GPU_HEALTH_STATUS, which has no DCGM field id) have no mapping and no rule until their fields are added."},
	{Area: "v0.7 hardware validation", Reason: "no v0.7 run has been recorded. Every validated cell cites v0.6.0-era evidence from the Tesla T4 stand; every other cell is declared from NVIDIA documentation or unknown."},
}

// XIDBuckets mirrors internal/detect's classification table; the test
// asserts the two never drift.
var XIDBuckets = []XIDBucket{
	{Name: "application", Codes: []int{13, 31, 43, 46}, Help: "Engine, page-fault and channel errors that are almost always a workload bug."},
	{Name: "ecc", Codes: []int{48, 63, 64, 92, 94, 95}, Help: "ECC and row-remap events, correctable and uncorrectable alike."},
	{Name: "nvlink", Codes: []int{74}, Help: "NVLink fabric errors."},
	{Name: "bus", Codes: []int{79}, Help: "PCIe bus loss."},
	{Name: "gsp", Codes: []int{119, 120}, Help: "GPU System Processor RPC timeouts and faults."},
	{Name: XIDBucketOther, Codes: nil, Help: "Any XID not listed above. Counted, never classified."},
}

// ShippedProfilePath is the GPU Operator values file whose dcgm-metrics CSV
// is the only exporter profile this repository ships. ExporterExposed is
// defined against it.
const ShippedProfilePath = "deploy/kubernetes/dependencies/gpu-operator/values.yaml"

// Rule files are consumers by role. A field listing a path in ConsumedBy
// claims that file references the field's Series; the test greps the files
// so the claim cannot drift. Rule consumption is a claim separate from
// exporter exposure: readBy() and recordedBy() make it, profiled() does
// not, and ShippedProfilePath is never a consumer.
const (
	// LegacyRulePath is the raw-alert file that reads the nine v0.6.1
	// series directly; it was never extended to the reliability profile.
	LegacyRulePath = "configs/vmalert/gpu-rules.yaml"
	// CanonicalRulePath holds the recording rules that derive every
	// kubeneuron_gpu_* series from its profiled source (v0.7 M3).
	CanonicalRulePath = "configs/vmalert/gpu-recording-rules.yaml"
	// DeployedRulePath is the Kubernetes manifest carrying both: the legacy
	// alert VMRule and the mirror of the canonical recording groups.
	DeployedRulePath = "deploy/kubernetes/dependencies/observability/rules.yaml"
)

// LegacyRuleConsumerPaths are the files whose raw alerts read a v0.6.1
// series by its source name. Only the nine legacy fields list them.
var LegacyRuleConsumerPaths = []string{LegacyRulePath, DeployedRulePath}

// CanonicalRuleConsumerPaths are the files that record a canonical series
// from the field's source series. Every profiled field lists them.
var CanonicalRuleConsumerPaths = []string{CanonicalRulePath, DeployedRulePath}

// RuleConsumerPaths is the de-duplicated union of every rule file a field
// may list, in the order above; the consumer-file test walks it.
var RuleConsumerPaths = unionPaths(LegacyRuleConsumerPaths, CanonicalRuleConsumerPaths)

func unionPaths(sets ...[]string) []string {
	var out []string
	seen := map[string]bool{}
	for _, set := range sets {
		for _, p := range set {
			if !seen[p] {
				seen[p] = true
				out = append(out, p)
			}
		}
	}
	return out
}

var (
	allDeclared = sup(SupportDeclared, SupportDeclared, SupportDeclared)
	allUnknown  = sup(SupportUnknown, SupportUnknown, SupportUnknown)
	// amperePlus: NVLink and row remapping are documented Ampere+ features
	// absent from Turing; the Tesla T4 cell is therefore not-applicable from
	// the source, not inferred.
	amperePlus = sup(SupportNotApplicable, SupportDeclared, SupportDeclared)
	preAmpere  = sup(SupportDeclared, SupportNotApplicable, SupportNotApplicable)
	// t4Unknown: documented generically by DCGM, but whether the Tesla T4
	// reports a value (for example a memory temperature sensor on GDDR6) is
	// not established by any source this catalog cites.
	t4Unknown = sup(SupportUnknown, SupportDeclared, SupportDeclared)
)

func sup(t4, a100, h100 Support) map[Hardware]Support {
	return map[Hardware]Support{HardwareT4: t4, HardwareA100: a100, HardwareH100: h100}
}

func raw(u Unit) Measure { return Measure{Unit: u, Factor: 1} }

// dcgm builds a DCGM field entry whose exporter series equals the field
// name. Its source is SourceDCGMField: it is NOT marked exporter-exposed and
// does not claim the exporter as its source; only profiled() does both, for
// the fields the shipped exporter profile lists. It carries no canonical
// mapping until canon(), decoded() or unmapped() gives it one.
func dcgm(id int, name string, kind Kind, tier Tier, rawUnit Unit, help string, support map[Hardware]Support) Field {
	return Field{Key: fmt.Sprintf("dcgm:%d", id), DCGMFieldID: id, DCGMField: name, Series: name, Source: SourceDCGMField,
		Kind: kind, Tier: tier, Raw: raw(rawUnit), Help: help, Support: support}
}

// legacy records that the field id was renamed in DCGM and that the pinned
// dcgm-exporter's metrics CSV still spells it the old way: the legacy name
// is the series the exporter would publish, the current DCGM name is a
// series alias on exporter builds that accept it. It says nothing about
// whether the shipped profile or the shipped rules use the field: profiled()
// claims the former, readBy() the latter, each on its own.
func legacy(f Field, oldName string) Field {
	f.Series = oldName
	f.Aliases = []string{oldName}
	f.SeriesAliases = []string{f.DCGMField}
	return f
}

// profiled marks a field listed in the shipped exporter profile
// (ShippedProfilePath), so the pinned exporter is configured to emit it. It
// is the only constructor that assigns SourceDCGMExporter, and it is the
// only thing exporter exposure depends on: it claims no consumer. Whether a
// rule reads the series is readBy()'s separate claim, so a profile change
// never has to pretend a rule exists. The reliability profile (v0.7 M2)
// applies it to every ProfileEligible field and to nothing else; Validate
// rejects a table where the two disagree.
func profiled(f Field) Field {
	f.ExporterExposed = true
	f.Source = SourceDCGMExporter
	return f
}

// readBy records the shipped rule files whose expressions read the field's
// series. Each file is an explicit claim the test greps for the series; a
// file already listed is not repeated. It says nothing about exporter
// exposure: a field may be read by a rule without being profiled (then the
// rule reads a series nothing emits, which the catalog records honestly)
// and profiled without being read.
func readBy(f Field, ruleFiles ...string) Field {
	return withConsumers(f, unionPaths(f.ConsumedBy, ruleFiles)...)
}

// recordedBy marks a field the shipped exporter profile lists AND the
// canonical recording rules read: profiled() for the exposure claim, then
// readBy() with CanonicalRuleConsumerPaths for the consumer claim. Every
// ProfileEligible field uses it since v1.6.0, because the recording rules
// derive one canonical series from each of them. It claims nothing about
// the legacy raw alerts: a field they also read adds LegacyRuleConsumerPaths
// with a separate readBy().
func recordedBy(f Field) Field {
	return readBy(profiled(f), CanonicalRuleConsumerPaths...)
}

// deferred marks a cataloged entry whose use is out of scope: no profile
// enables it, nothing acts on it.
func deferred(f Field, reason string) Field {
	f.Deferred = reason
	return f
}

// L builds a fixed label.
func L(name, value string) Label { return Label{Name: name, Value: value} }

// canon attaches the canonical series a DCGM field feeds. The canonical kind
// is the source kind; factor converts raw to the canonical unit.
func canon(f Field, series string, unit Unit, factor float64, labels ...Label) Field {
	f.Canonical = &Canonical{Series: series, Kind: f.Kind, Unit: unit, Factor: factor, Labels: labels}
	return f
}

// noted adds the provenance note to an existing canonical mapping.
func noted(f Field, note string) Field {
	f.Canonical.Note = note
	return f
}

// decoded attaches a canonical mapping that is a decode, not a scalar
// conversion: the recording rule derives valueLabel from the raw value.
func decoded(f Field, series string, unit Unit, valueLabel, note string) Field {
	f.Canonical = &Canonical{Series: series, Kind: f.Kind, Unit: unit, ValueLabels: []string{valueLabel}, Note: note}
	return f
}

// unmapped records that a DCGM field feeds no canonical series, and why.
func unmapped(f Field, reason string) Field {
	f.CanonicalDeferred = reason
	return f
}

// cp builds a control-plane (T4) entry. Its canonical series is the existing
// series itself.
func cp(series string, src Source, kind Kind, u Unit, help string) Field {
	return Field{Key: "kubeneuron:" + series, Series: series, Source: src, Kind: kind, Tier: TierT4, Raw: raw(u),
		Canonical: &Canonical{Series: series, Kind: kind, Unit: u, Factor: 1}, Help: help}
}

func withConsumers(f Field, c ...string) Field { f.ConsumedBy = c; return f }

// Conversion factors from raw to canonical units.
const (
	factorMHz   = 1e6     // megahertz -> hertz
	factorMJ    = 1e-3    // millijoules -> joules
	factorPct   = 1e-2    // percent -> ratio
	factorMiB   = 1 << 20 // mebibytes -> bytes
	factorNanos = 1e-9    // nanoseconds -> seconds
)

// Fields is the catalog body. DCGM entries ascend by field id, then
// control-plane entries ascend by series name; Validate enforces it.
//
// Canonical mappings follow Appendix A of the feature brief (DCGM table).
// Rows not in Appendix A say so in a note; fields the brief maps but this
// version does not are unmapped() with the reason.
var Fields = []Field{
	// --- T0 baseline -----------------------------------------------------
	canon(recordedBy(dcgm(100, "DCGM_FI_DEV_SM_CLOCK", KindGauge, TierT0, UnitMegahertz, "Current SM clock.", allDeclared)),
		"kubeneuron_gpu_clock_hertz", UnitHertz, factorMHz, L("domain", "sm")),
	canon(recordedBy(dcgm(101, "DCGM_FI_DEV_MEM_CLOCK", KindGauge, TierT0, UnitMegahertz, "Current memory clock.", allDeclared)),
		"kubeneuron_gpu_clock_hertz", UnitHertz, factorMHz, L("domain", "memory")),
	// --- T1 reliability --------------------------------------------------
	// Field 112 was renamed in DCGM; the shipped exporter profile and rules
	// still use the legacy name, so that is the series the pinned exporter
	// publishes today (see legacy()). The canonical form is one boolean
	// series per decoded reason bit, not the bitmask.
	decoded(readBy(recordedBy(legacy(dcgm(112, "DCGM_FI_DEV_CLOCKS_EVENT_REASONS", KindGauge, TierT1, UnitBitmask,
		"Active clock-event (throttle) reason bitmask. 0x08 HW slowdown, 0x20 SW thermal, 0x40 HW thermal, 0x80 HW power brake.", allDeclared),
		"DCGM_FI_DEV_CLOCK_THROTTLE_REASONS")), LegacyRuleConsumerPaths...),
		"kubeneuron_gpu_clock_event_active", UnitBool, "reason",
		"the recording rule decodes each bit of the bitmask into one series with reason set to the bit's name (for example hw_slowdown, sw_thermal, hw_thermal, hw_power_brake); value 1 while the bit is set"),
	canon(recordedBy(dcgm(113, "DCGM_FI_DEV_MAX_SM_CLOCK", KindGauge, TierT0, UnitMegahertz, "Maximum supported SM clock.", allDeclared)),
		"kubeneuron_gpu_clock_max_hertz", UnitHertz, factorMHz, L("domain", "sm")),
	canon(recordedBy(dcgm(114, "DCGM_FI_DEV_MAX_MEM_CLOCK", KindGauge, TierT0, UnitMegahertz, "Maximum supported memory clock.", allDeclared)),
		"kubeneuron_gpu_clock_max_hertz", UnitHertz, factorMHz, L("domain", "memory")),
	canon(recordedBy(legacy(dcgm(140, "DCGM_FI_DEV_MEMORY_TEMP_CELSIUS", KindGauge, TierT0, UnitCelsius, "Memory temperature. Whether the Tesla T4 reports a value is not established by any cited source.", t4Unknown), "DCGM_FI_DEV_MEMORY_TEMP")),
		"kubeneuron_gpu_memory_temperature_celsius", UnitCelsius, 1),
	// No public source-tree document records a scrape of this field, so the
	// Tesla T4 cell is declared (documented and in the shipped profile), not
	// validated, until a v0.7 run is recorded.
	canon(readBy(recordedBy(legacy(dcgm(150, "DCGM_FI_DEV_GPU_TEMP_CELSIUS", KindGauge, TierT0, UnitCelsius, "GPU die temperature.", allDeclared), "DCGM_FI_DEV_GPU_TEMP")), LegacyRuleConsumerPaths...),
		"kubeneuron_gpu_temperature_celsius", UnitCelsius, 1),
	canon(recordedBy(legacy(dcgm(151, "DCGM_FI_DEV_MEMORY_MAX_OP_TEMP_CELSIUS", KindGauge, TierT0, UnitCelsius, "Maximum operating temperature of the memory. Whether the Tesla T4 reports a value is not established by any cited source.", t4Unknown), "DCGM_FI_DEV_MEM_MAX_OP_TEMP")),
		"kubeneuron_gpu_memory_temperature_limit_celsius", UnitCelsius, 1),
	canon(recordedBy(legacy(dcgm(153, "DCGM_FI_DEV_GPU_TEMP_MARGIN_CELSIUS", KindGauge, TierT0, UnitCelsius, "Thermal margin: distance from the current GPU temperature to the nearest slowdown threshold.", allDeclared), "DCGM_FI_DEV_GPU_TEMP_LIMIT")),
		"kubeneuron_gpu_thermal_margin_celsius", UnitCelsius, 1),
	canon(recordedBy(legacy(dcgm(155, "DCGM_FI_DEV_BOARD_POWER_WATTS", KindGauge, TierT0, UnitWatts, "Board power draw.", allDeclared), "DCGM_FI_DEV_POWER_USAGE")),
		"kubeneuron_gpu_power_watts", UnitWatts, 1),
	canon(recordedBy(dcgm(156, "DCGM_FI_DEV_TOTAL_ENERGY_CONSUMPTION", KindCounter, TierT0, UnitMillijoules, "Energy consumed since driver load.", allDeclared)),
		"kubeneuron_gpu_energy_joules_total", UnitJoules, factorMJ),
	canon(recordedBy(legacy(dcgm(158, "DCGM_FI_DEV_GPU_TEMP_SLOWDOWN_CELSIUS", KindGauge, TierT0, UnitCelsius, "Slowdown temperature threshold.", allDeclared), "DCGM_FI_DEV_SLOWDOWN_TEMP")),
		"kubeneuron_gpu_temperature_limit_celsius", UnitCelsius, 1, L("limit", "slowdown")),
	canon(recordedBy(legacy(dcgm(159, "DCGM_FI_DEV_GPU_TEMP_SHUTDOWN_CELSIUS", KindGauge, TierT0, UnitCelsius, "Shutdown temperature threshold.", allDeclared), "DCGM_FI_DEV_SHUTDOWN_TEMP")),
		"kubeneuron_gpu_temperature_limit_celsius", UnitCelsius, 1, L("limit", "shutdown")),
	canon(recordedBy(dcgm(164, "DCGM_FI_DEV_ENFORCED_POWER_LIMIT", KindGauge, TierT0, UnitWatts, "Power limit currently enforced by the driver.", allDeclared)),
		"kubeneuron_gpu_power_limit_watts", UnitWatts, 1),
	unmapped(dcgm(190, "DCGM_FI_DEV_PSTATE", KindGauge, TierT0, UnitCode, "Performance state (0 = max performance, 15 = idle).", allDeclared),
		"not a row of Appendix A; no canonical series is assigned until a dashboard or rule needs the P-state"),
	canon(readBy(recordedBy(legacy(dcgm(202, "DCGM_FI_DEV_PCIE_REPLAY_TOTAL", KindCounter, TierT1, UnitCount, "PCIe link replays (retries).", allDeclared), "DCGM_FI_DEV_PCIE_REPLAY_COUNTER")), LegacyRuleConsumerPaths...),
		"kubeneuron_gpu_pcie_replays_total", UnitCount, 1),
	canon(recordedBy(legacy(dcgm(203, "DCGM_FI_DEV_GPU_UTIL_RATIO", KindGauge, TierT0, UnitPercent, "Coarse GPU utilisation: fraction of time any kernel ran.", allDeclared), "DCGM_FI_DEV_GPU_UTIL")),
		"kubeneuron_gpu_utilization_ratio", UnitRatio, factorPct),
	canon(recordedBy(dcgm(204, "DCGM_FI_DEV_MEM_COPY_UTIL", KindGauge, TierT0, UnitPercent, "Memory copy-engine utilisation.", allDeclared)),
		"kubeneuron_gpu_memory_copy_utilization_ratio", UnitRatio, factorPct),
	func() Field {
		// Read by the agent through dcgmi by numeric id, not through the
		// exporter: the shipped exporter profile does not list it.
		f := legacy(dcgm(230, "DCGM_FI_DEV_XID_ERROR", KindGauge, TierT1, UnitCode,
			"Last XID code recorded by nv-hostengine. Sticky for the engine's lifetime: a non-zero value is not a new fault.", sup(SupportValidated, SupportDeclared, SupportDeclared)),
			"DCGM_FI_DEV_XID_ERRORS")
		f.Evidence = "v0.6.0-era run, not v0.7: the agent gpuhealth source (internal/agent/gpuhealth, dcgmi dmon -e 230) read this field on the Tesla T4 stand during the hw-e2e test-dcgm phase; see the v0.6.0 section of CHANGELOG.md. The XID was injected, not a natural fault. No v0.7 validation has been performed."
		f = unmapped(f, "a sticky last-value gauge cannot feed "+CanonicalXIDEventsTotal+": a rate over it sees no repeats and keeps the last code forever. The counter is reserved until an event-counter source (DCGM_EXP_XID_ERRORS_TOTAL or a kernel-log watcher) is cataloged; see reserved_canonical")
		return withConsumers(f, "internal/agent/gpuhealth")
	}(),
	canon(recordedBy(dcgm(235, "DCGM_FI_DEV_PCIE_MAX_LINK_GEN", KindGauge, TierT0, UnitCount, "Maximum PCIe link generation the device supports.", allDeclared)),
		"kubeneuron_gpu_pcie_link_generation", UnitCount, 1, L("kind", "max")),
	canon(recordedBy(dcgm(236, "DCGM_FI_DEV_PCIE_MAX_LINK_WIDTH", KindGauge, TierT0, UnitCount, "Maximum PCIe link width (lanes) the device supports.", allDeclared)),
		"kubeneuron_gpu_pcie_link_width", UnitCount, 1, L("kind", "max")),
	canon(recordedBy(dcgm(237, "DCGM_FI_DEV_PCIE_LINK_GEN", KindGauge, TierT0, UnitCount, "Current PCIe link generation.", allDeclared)),
		"kubeneuron_gpu_pcie_link_generation", UnitCount, 1, L("kind", "current")),
	canon(recordedBy(dcgm(238, "DCGM_FI_DEV_PCIE_LINK_WIDTH", KindGauge, TierT0, UnitCount, "Current PCIe link width (lanes).", allDeclared)),
		"kubeneuron_gpu_pcie_link_width", UnitCount, 1, L("kind", "current")),
	canon(recordedBy(dcgm(240, "DCGM_FI_DEV_POWER_VIOLATION", KindCounter, TierT1, UnitNanoseconds, "Time the GPU was power-capped (raw nanoseconds).", allDeclared)),
		"kubeneuron_gpu_throttle_seconds_total", UnitSeconds, factorNanos, L("reason", "power")),
	canon(recordedBy(dcgm(241, "DCGM_FI_DEV_THERMAL_VIOLATION", KindCounter, TierT1, UnitNanoseconds, "Time the GPU was thermally capped (raw nanoseconds).", allDeclared)),
		"kubeneuron_gpu_throttle_seconds_total", UnitSeconds, factorNanos, L("reason", "thermal")),
	// 242, 243 and 245 complete the Appendix A throttle-time row. dcgm_fields.h
	// documents all of them as cumulative violation time in nanoseconds, like
	// 240 and 241. The exporter's stock default CSV lists them commented out;
	// the reliability profile this repository ships enables all five, none of
	// which has been observed on any hardware by a recorded run.
	canon(recordedBy(dcgm(242, "DCGM_FI_DEV_SYNC_BOOST_VIOLATION", KindCounter, TierT1, UnitNanoseconds, "Time the GPU was capped by a sync-boost group constraint (raw nanoseconds).", allDeclared)),
		"kubeneuron_gpu_throttle_seconds_total", UnitSeconds, factorNanos, L("reason", "sync_boost")),
	canon(recordedBy(dcgm(243, "DCGM_FI_DEV_BOARD_LIMIT_VIOLATION", KindCounter, TierT1, UnitNanoseconds, "Time the GPU was capped by a board power or current limit (raw nanoseconds).", allDeclared)),
		"kubeneuron_gpu_throttle_seconds_total", UnitSeconds, factorNanos, L("reason", "board_limit")),
	canon(recordedBy(dcgm(245, "DCGM_FI_DEV_RELIABILITY_VIOLATION", KindCounter, TierT1, UnitNanoseconds, "Time the GPU was capped by a reliability (long-term operating) constraint (raw nanoseconds).", allDeclared)),
		"kubeneuron_gpu_throttle_seconds_total", UnitSeconds, factorNanos, L("reason", "reliability")),
	noted(canon(recordedBy(dcgm(250, "DCGM_FI_DEV_FB_TOTAL", KindGauge, TierT0, UnitMiB, "Framebuffer total.", allDeclared)),
		"kubeneuron_gpu_memory_total_bytes", UnitBytes, factorMiB),
		"not a row of Appendix A; named like its used/free siblings"),
	canon(recordedBy(dcgm(251, "DCGM_FI_DEV_FB_FREE", KindGauge, TierT0, UnitMiB, "Framebuffer free.", allDeclared)),
		"kubeneuron_gpu_memory_free_bytes", UnitBytes, factorMiB),
	canon(recordedBy(dcgm(252, "DCGM_FI_DEV_FB_USED", KindGauge, TierT0, UnitMiB, "Framebuffer used.", allDeclared)),
		"kubeneuron_gpu_memory_used_bytes", UnitBytes, factorMiB),
	canon(recordedBy(dcgm(310, "DCGM_FI_DEV_ECC_SBE_VOL_TOTAL", KindCounter, TierT1, UnitCount, "Volatile single-bit ECC errors (corrected; trend only).", allDeclared)),
		"kubeneuron_gpu_ecc_errors_total", UnitCount, 1, L("bits", "single"), L("scope", "volatile")),
	canon(readBy(recordedBy(dcgm(311, "DCGM_FI_DEV_ECC_DBE_VOL_TOTAL", KindCounter, TierT1, UnitCount, "Volatile double-bit ECC errors since driver load.", allDeclared)), LegacyRuleConsumerPaths...),
		"kubeneuron_gpu_ecc_errors_total", UnitCount, 1, L("bits", "double"), L("scope", "volatile")),
	noted(canon(recordedBy(dcgm(312, "DCGM_FI_DEV_ECC_SBE_AGG_TOTAL", KindCounter, TierT1, UnitCount, "Aggregate (lifetime) single-bit ECC errors.", allDeclared)),
		"kubeneuron_gpu_ecc_errors_total", UnitCount, 1, L("bits", "single"), L("scope", "aggregate")),
		"Appendix A maps only the volatile counters with {bits}; the scope label distinguishes the lifetime aggregate"),
	noted(canon(readBy(recordedBy(dcgm(313, "DCGM_FI_DEV_ECC_DBE_AGG_TOTAL", KindCounter, TierT1, UnitCount, "Aggregate (lifetime) double-bit ECC errors.", allDeclared)), LegacyRuleConsumerPaths...),
		"kubeneuron_gpu_ecc_errors_total", UnitCount, 1, L("bits", "double"), L("scope", "aggregate")),
		"Appendix A maps only the volatile counters with {bits}; the scope label distinguishes the lifetime aggregate"),
	canon(recordedBy(dcgm(390, "DCGM_FI_DEV_RETIRED_SBE", KindCounter, TierT1, UnitCount, "Pages retired for single-bit errors (pre-Ampere retirement).", preAmpere)),
		"kubeneuron_gpu_retired_pages_total", UnitCount, 1, L("cause", "single_bit")),
	canon(recordedBy(dcgm(391, "DCGM_FI_DEV_RETIRED_DBE", KindCounter, TierT1, UnitCount, "Pages retired for double-bit errors (pre-Ampere retirement).", preAmpere)),
		"kubeneuron_gpu_retired_pages_total", UnitCount, 1, L("cause", "double_bit")),
	canon(recordedBy(dcgm(392, "DCGM_FI_DEV_RETIRED_PENDING", KindGauge, TierT1, UnitBool, "Page retirement pending a reset (pre-Ampere).", preAmpere)),
		"kubeneuron_gpu_retired_pages_pending", UnitBool, 1),
	canon(readBy(recordedBy(dcgm(393, "DCGM_FI_DEV_UNCORRECTABLE_REMAPPED_ROWS", KindCounter, TierT1, UnitCount, "Rows remapped for uncorrectable errors (Ampere+ row remapping).", amperePlus)), LegacyRuleConsumerPaths...),
		"kubeneuron_gpu_remapped_rows_total", UnitCount, 1, L("kind", "uncorrectable")),
	canon(recordedBy(dcgm(394, "DCGM_FI_DEV_CORRECTABLE_REMAPPED_ROWS", KindCounter, TierT1, UnitCount, "Rows remapped for correctable errors (Ampere+).", amperePlus)),
		"kubeneuron_gpu_remapped_rows_total", UnitCount, 1, L("kind", "correctable")),
	canon(readBy(recordedBy(dcgm(395, "DCGM_FI_DEV_ROW_REMAP_FAILURE", KindGauge, TierT1, UnitBool, "Row remapping failed (Ampere+).", amperePlus)), LegacyRuleConsumerPaths...),
		"kubeneuron_gpu_row_remap_failed", UnitBool, 1),
	canon(readBy(recordedBy(dcgm(396, "DCGM_FI_DEV_ROW_REMAP_PENDING", KindGauge, TierT1, UnitBool, "Row remap pending a reset (Ampere+).", amperePlus)), LegacyRuleConsumerPaths...),
		"kubeneuron_gpu_row_remap_pending", UnitBool, 1),
	canon(readBy(recordedBy(dcgm(409, "DCGM_FI_DEV_NVLINK_CRC_FLIT_ERROR_COUNT_TOTAL", KindCounter, TierT1, UnitCount, "NVLink flow-control CRC errors, all links.", amperePlus)), LegacyRuleConsumerPaths...),
		"kubeneuron_gpu_nvlink_errors_total", UnitCount, 1, L("type", "crc_flit")),
	canon(recordedBy(dcgm(419, "DCGM_FI_DEV_NVLINK_CRC_DATA_ERROR_COUNT_TOTAL", KindCounter, TierT1, UnitCount, "NVLink data CRC errors, all links.", amperePlus)),
		"kubeneuron_gpu_nvlink_errors_total", UnitCount, 1, L("type", "crc_data")),
	canon(recordedBy(dcgm(429, "DCGM_FI_DEV_NVLINK_REPLAY_ERROR_COUNT_TOTAL", KindCounter, TierT1, UnitCount, "NVLink replay errors, all links.", amperePlus)),
		"kubeneuron_gpu_nvlink_errors_total", UnitCount, 1, L("type", "replay")),
	canon(recordedBy(dcgm(439, "DCGM_FI_DEV_NVLINK_RECOVERY_ERROR_COUNT_TOTAL", KindCounter, TierT1, UnitCount, "NVLink recovery errors, all links.", amperePlus)),
		"kubeneuron_gpu_nvlink_errors_total", UnitCount, 1, L("type", "recovery")),
	// --- T0 baseline PCIe byte rates: the only DCGM_FI_PROF_* fields in the
	// MVP. NVIDIA documents 1009/1010 as the PCIe bandwidth averaged over
	// the sample interval, in bytes per second: a rate, hence a gauge, with
	// bytes_per_second as both the raw and the canonical unit.
	canon(recordedBy(dcgm(1009, BaselinePROFFields[1009], KindGauge, TierT0, UnitBytesPerSec, "PCIe transmit bandwidth, header and payload, averaged over the sample interval in bytes per second. A rate, not a cumulative byte total.", allDeclared)),
		"kubeneuron_gpu_pcie_bytes_per_second", UnitBytesPerSec, 1, L("direction", "tx")),
	canon(recordedBy(dcgm(1010, BaselinePROFFields[1010], KindGauge, TierT0, UnitBytesPerSec, "PCIe receive bandwidth, header and payload, averaged over the sample interval in bytes per second. A rate, not a cumulative byte total.", allDeclared)),
		"kubeneuron_gpu_pcie_bytes_per_second", UnitBytesPerSec, 1, L("direction", "rx")),
	// --- T1 memory beyond repair ------------------------------------------
	// dcgm_fields.h: "Unrepairable memory flag indicating if memory has
	// unrepairable errors, 1=yes, 0=no". A flag, so a boolean gauge; which
	// hardware reports it is not established by any source this catalog
	// cites, so every cell is unknown rather than declared. The reliability
	// profile lists it so it is read wherever the GPU and DCGM build report
	// it; nothing acts on it.
	// Field 1507 is DCGM_FI_DEV_MEMORY_UNREPAIRABLE in dcgm_fields.h, but the
	// go-dcgm the pinned dcgm-exporter (4.5.3-4.8.2) is built against knows
	// the id only as DCGM_FI_DEV_MEMORY_UNREPAIRABLE_FLAG (later go-dcgm
	// deprecates that spelling in favour of the header name). The exporter
	// resolves CSV names through that table, so the header name is unknown
	// to it: the shipped profile and the rules must spell it _FLAG (see
	// legacy()). The 2026-10-18 G4dn run listed the header name and the
	// exporter served nothing.
	canon(recordedBy(legacy(dcgm(1507, "DCGM_FI_DEV_MEMORY_UNREPAIRABLE", KindGauge, TierT1, UnitBool, "Memory has unrepairable errors (1 = yes, 0 = no): row remapping or page retirement can no longer repair it. Cataloged from dcgm_fields.h; listed in the shipped profile under the pinned exporter's DCGM_FI_DEV_MEMORY_UNREPAIRABLE_FLAG spelling, observed on no hardware by this project.", allUnknown),
		"DCGM_FI_DEV_MEMORY_UNREPAIRABLE_FLAG")),
		"kubeneuron_gpu_memory_unrepairable", UnitBool, 1),
	// --- T1 cataloged-only ------------------------------------------------
	unmapped(deferred(dcgm(1523, "DCGM_FI_DEV_GPU_RECOVERY_ACTION", KindGauge, TierT1, UnitCode,
		"Recovery action the driver requests for the GPU (DCGM 4.x enumeration). Cataloged for its id and semantics only.", allUnknown),
		"not enabled in any profile, not shown to be exposed by the pinned dcgm-exporter, unvalidated on every hardware, and attached to no action"),
		"cataloged only; "+CanonicalRecoveryAction+" is reserved until the field is enabled and its value enumeration is cataloged; see reserved_canonical"),
	// --- T4 control plane (existing kubeneuron_* series; see internal/metrics)
	cp("kubeneuron_actions_pending", SourceController, KindGauge, UnitCount, "Agent actions dispatched and not yet claimed; sustained non-zero means an unreachable or unregistered agent."),
	cp("kubeneuron_agent_detections_deduplicated_total", SourceAgent, KindCounter, UnitCount, "Detections suppressed by agent-side deduplication, by the source that was dropped."),
	cp("kubeneuron_agent_detections_total", SourceAgent, KindCounter, UnitCount, "GPU fault detections observed by the agent, by detection source (kmsg, kmsg-amd, gpuhealth, amdhealth)."),
	cp("kubeneuron_agent_health_source", SourceAgent, KindGauge, UnitBool, "Which second-source GPU health probe (dcgm, nvidia-smi, none) served the last poll; 1 for the active source."),
	cp("kubeneuron_checkpoint_requests_total", SourceController, KindCounter, UnitCount, "Checkpoint-coordination outcomes ahead of a drain or GPU-workload eviction, by outcome."),
	cp("kubeneuron_checkpoint_wait_seconds", SourceController, KindHistogram, UnitSeconds, "Seconds a workload with a checkpoint request in force was given before its outcome settled."),
	cp("kubeneuron_dead_lettered_total", SourceController, KindCounter, UnitCount, "Queued actions or agent events that exhausted their attempt budget, by queue."),
	cp("kubeneuron_decision_stale_evidence_total", SourceController, KindCounter, UnitCount, "Decisions held for stale or missing evidence, by adapter and reason."),
	cp("kubeneuron_degraded_gpu_seconds_total", SourceController, KindCounter, UnitSeconds, "GPU-seconds charged when an incident reached a terminal state, by class and outcome; the resolved share is capacity returned to service."),
	cp("kubeneuron_degraded_gpus", SourceController, KindGauge, UnitCount, "Accelerators currently under a non-terminal incident, by problem class and owner (automation or human). Scrape-time collector; absent on a standby replica."),
	cp("kubeneuron_destructive_steps_deferred_total", SourceController, KindCounter, UnitCount, "Destructive playbook steps that did not run, by the guard that stopped them."),
	cp("kubeneuron_escalations_total", SourceController, KindCounter, UnitCount, "Ladder escalations after step or verification failures."),
	cp("kubeneuron_gate_denials_total", SourceController, KindCounter, UnitCount, "Steps denied by the safety gate (pause, cooldown, concurrency, capability)."),
	cp("kubeneuron_incident_duration_seconds", SourceController, KindHistogram, UnitSeconds, "Open-to-halted wall time of incidents, by class and outcome."),
	cp("kubeneuron_incidents", SourceController, KindGauge, UnitCount, "Incidents currently in each state. Scrape-time collector; absent on a standby replica."),
	cp("kubeneuron_incidents_opened_total", SourceController, KindCounter, UnitCount, "Incidents opened, by problem class."),
	cp("kubeneuron_incidents_recovered_total", SourceController, KindCounter, UnitCount, "Incidents that reached RESOLVED, by class and whether a human was needed; dry-run incidents excluded."),
	cp("kubeneuron_signals_dropped_total", SourceController, KindCounter, UnitCount, "Signals lost to ingest-queue overflow."),
	cp("kubeneuron_signals_total", SourceController, KindCounter, UnitCount, "Normalised signals entering the pipeline, by source and class."),
	cp("kubeneuron_stack_restore_failures_total", SourceController, KindCounter, UnitCount, "Failed accelerator-stack restores by the janitor."),
	cp("kubeneuron_steps_total", SourceController, KindCounter, UnitCount, "Playbook steps executed, by outcome (ok, failed, dry_run)."),
	cp("kubeneuron_workloads_evicted_total", SourceController, KindCounter, UnitCount, "GPU workloads moved off a node ahead of a destructive step, by problem class."),
}

// Default returns the complete catalog, validated. It panics on an invalid
// table because that is a programming error the test catches first.
func Default() Catalog {
	c := Catalog{Version: Version, CanonicalContract: CanonicalContract, Fields: Fields, ReservedCanonical: ReservedCanonical, XIDBuckets: XIDBuckets, Deferred: Deferred, Derived: DerivedSignals}
	if err := c.Validate(); err != nil {
		panic("gpucatalog: built-in catalog invalid: " + err.Error())
	}
	return c
}

// BucketForXID returns the bucket name for a code, XIDBucketOther when none
// lists it.
func BucketForXID(code int) string {
	for _, b := range XIDBuckets {
		for _, c := range b.Codes {
			if c == code {
				return b.Name
			}
		}
	}
	return XIDBucketOther
}

// LookupDCGM returns the field with the given DCGM field id.
func (c Catalog) LookupDCGM(id int) (Field, bool) {
	for _, f := range c.Fields {
		if f.DCGMFieldID == id && id != 0 {
			return f, true
		}
	}
	return Field{}, false
}

// LookupSeries returns the field published under a source series name.
func (c Catalog) LookupSeries(series string) (Field, bool) {
	for _, f := range c.Fields {
		if f.Series == series {
			return f, true
		}
	}
	return Field{}, false
}

// LookupCanonical returns every field that feeds a canonical series, in
// catalog order.
func (c Catalog) LookupCanonical(series string) []Field {
	var out []Field
	for _, f := range c.Fields {
		if f.Canonical != nil && f.Canonical.Series == series {
			out = append(out, f)
		}
	}
	return out
}

// CanonicalSeries returns the distinct canonical series names, sorted.
func (c Catalog) CanonicalSeries() []string {
	seen := map[string]bool{}
	var out []string
	for _, f := range c.Fields {
		if f.Canonical != nil && !seen[f.Canonical.Series] {
			seen[f.Canonical.Series] = true
			out = append(out, f.Canonical.Series)
		}
	}
	sort.Strings(out)
	return out
}

// LabelSet renders fixed labels as a Prometheus selector fragment, for
// example {domain="sm"}; empty when there are none.
func (k Canonical) LabelSet() string {
	if len(k.Labels) == 0 {
		return ""
	}
	parts := make([]string, 0, len(k.Labels))
	for _, l := range k.Labels {
		parts = append(parts, fmt.Sprintf("%s=%q", l.Name, l.Value))
	}
	return "{" + strings.Join(parts, ",") + "}"
}

// Validate enforces the structural invariants every version must keep.
func (c Catalog) Validate() error {
	var errs []error
	if c.Version == "" {
		errs = append(errs, errors.New("version is empty"))
	}
	if c.CanonicalContract != CanonicalContract {
		errs = append(errs, errors.New("canonical contract statement missing or altered"))
	}
	reserved := map[string]bool{}
	for _, r := range c.ReservedCanonical {
		if r.Series == "" || strings.TrimSpace(r.Reason) == "" {
			errs = append(errs, fmt.Errorf("reserved canonical %q: series and reason are required", r.Series))
		}
		reserved[r.Series] = true
	}
	for _, must := range []string{CanonicalXIDEventsTotal, CanonicalRecoveryAction} {
		if !reserved[must] {
			errs = append(errs, fmt.Errorf("reserved canonical list must keep %s", must))
		}
	}
	seenKey := map[string]bool{}
	seenSeries := map[string]bool{}
	seenDCGM := map[int]bool{}
	shapes := map[string]*canonShape{}
	var prevID int
	var prevSeries string
	seenCP := false
	for i, f := range c.Fields {
		where := fmt.Sprintf("field[%d] %s", i, f.Key)
		if seenKey[f.Key] {
			errs = append(errs, fmt.Errorf("%s: duplicate key", where))
		}
		seenKey[f.Key] = true
		if f.Series == "" {
			errs = append(errs, fmt.Errorf("%s: series is empty", where))
		}
		if seenSeries[f.Series] {
			errs = append(errs, fmt.Errorf("%s: duplicate series %q", where, f.Series))
		}
		seenSeries[f.Series] = true
		if f.Kind != KindGauge && f.Kind != KindCounter && f.Kind != KindHistogram {
			errs = append(errs, fmt.Errorf("%s: unknown kind %q", where, f.Kind))
		}
		if strings.TrimSpace(f.Help) == "" {
			errs = append(errs, fmt.Errorf("%s: help is empty", where))
		}
		errs = append(errs, validateRaw(where, f)...)
		errs = append(errs, validateCanonical(where, f, reserved, shapes)...)

		switch f.Tier {
		case TierT0, TierT1:
			if !f.Source.IsDCGM() {
				errs = append(errs, fmt.Errorf("%s: tier %s must have a DCGM source (%v)", where, f.Tier, DCGMSources))
			}
			if f.ExporterExposed && f.Source != SourceDCGMExporter {
				errs = append(errs, fmt.Errorf("%s: a field the shipped profile lists must have source %s", where, SourceDCGMExporter))
			}
			if !f.ExporterExposed && f.Source == SourceDCGMExporter {
				errs = append(errs, fmt.Errorf("%s: source %s is only for fields %s lists; a merely cataloged field is source %s", where, SourceDCGMExporter, ShippedProfilePath, SourceDCGMField))
			}
			if f.ExporterExposed != ProfileEligible(f) {
				errs = append(errs, fmt.Errorf("%s: exporter_exposed=%v but the reliability profile lists exactly the T0/T1 fields with a canonical mapping that are not deferred (eligible=%v); change the catalog and %s together", where, f.ExporterExposed, ProfileEligible(f), ShippedProfilePath))
			}
			if f.DCGMFieldID <= 0 || !strings.HasPrefix(f.DCGMField, "DCGM_FI_") {
				errs = append(errs, fmt.Errorf("%s: tier %s requires a positive DCGM field id and a DCGM_FI_ name", where, f.Tier))
			}
			errs = append(errs, validatePROF(where, f)...)
			if strings.HasPrefix(f.DCGMField, "DCGM_FI_DEV_VGPU_") {
				errs = append(errs, fmt.Errorf("%s: vGPU fields are deferred", where))
			}
			for _, a := range f.SeriesAliases {
				if a == f.Series || seenSeries[a] {
					errs = append(errs, fmt.Errorf("%s: series alias %q collides with a series", where, a))
				}
				seenSeries[a] = true
			}
			if f.Deferred != "" && (f.ExporterExposed || len(f.ConsumedBy) > 0 || f.Canonical != nil) {
				errs = append(errs, fmt.Errorf("%s: a deferred entry may not claim exporter exposure, consumers or a canonical series", where))
			}
			if hasConsumer(f, ShippedProfilePath) {
				errs = append(errs, fmt.Errorf("%s: %s is not a consumer; exporter exposure is exporter_exposed, not a consumed_by entry", where, ShippedProfilePath))
			}
			if f.Key != fmt.Sprintf("dcgm:%d", f.DCGMFieldID) {
				errs = append(errs, fmt.Errorf("%s: key must be dcgm:<field id>", where))
			}
			if seenDCGM[f.DCGMFieldID] {
				errs = append(errs, fmt.Errorf("%s: duplicate DCGM field id", where))
			}
			seenDCGM[f.DCGMFieldID] = true
			if seenCP {
				errs = append(errs, fmt.Errorf("%s: DCGM fields must precede control-plane fields", where))
			}
			if f.DCGMFieldID <= prevID {
				errs = append(errs, fmt.Errorf("%s: DCGM field ids must ascend", where))
			}
			prevID = f.DCGMFieldID
			errs = append(errs, validateSupport(where, f)...)
		case TierT4:
			if f.Source != SourceController && f.Source != SourceAgent {
				errs = append(errs, fmt.Errorf("%s: tier T4 must come from a kubeneuron component, never a DCGM source", where))
			}
			if f.DCGMFieldID != 0 || f.DCGMField != "" || f.Support != nil || len(f.Aliases) > 0 || len(f.SeriesAliases) > 0 || f.ExporterExposed || f.Deferred != "" {
				errs = append(errs, fmt.Errorf("%s: tier T4 carries no DCGM field, alias, exporter flag or hardware support", where))
			}
			if strings.Contains(strings.ToLower(f.Help), "tesla") {
				errs = append(errs, fmt.Errorf("%s: tier T4 is the control plane, never the Tesla T4", where))
			}
			if !strings.HasPrefix(f.Series, "kubeneuron_") || f.Key != "kubeneuron:"+f.Series {
				errs = append(errs, fmt.Errorf("%s: tier T4 series must be kubeneuron_* with key kubeneuron:<series>", where))
			}
			if strings.HasPrefix(f.Series, canonicalGPUPrefix) {
				errs = append(errs, fmt.Errorf("%s: %s* is reserved for normalised GPU series; a control-plane series may not use it", where, canonicalGPUPrefix))
			}
			if seenCP && f.Series <= prevSeries {
				errs = append(errs, fmt.Errorf("%s: control-plane series must ascend", where))
			}
			seenCP = true
			prevSeries = f.Series
		case TierT2, TierT3:
			errs = append(errs, fmt.Errorf("%s: tier %s is deferred and may not be assigned", where, f.Tier))
		default:
			errs = append(errs, fmt.Errorf("%s: unknown tier %q", where, f.Tier))
		}
	}
	errs = append(errs, validateBuckets(c.XIDBuckets)...)
	errs = append(errs, validateDerived(c, reserved)...)
	if len(c.Deferred) == 0 {
		errs = append(errs, errors.New("deferred scope list is empty; the catalog must state what it does not cover"))
	}
	for _, tier := range DeferredTiers {
		found := false
		for _, d := range c.Deferred {
			if strings.Contains(d.Area, string(tier)) {
				found = true
			}
		}
		if !found {
			errs = append(errs, fmt.Errorf("deferred scope must name tier %s", tier))
		}
	}
	return errors.Join(errs...)
}

// validatePROF admits a DCGM_FI_PROF_* field only when it is one of the
// baseline PCIe byte-rate fields, in T0, with byte-per-second semantics.
func validatePROF(where string, f Field) []error {
	baseline, isBaseline := BaselinePROFFields[f.DCGMFieldID]
	switch {
	case isBaseline && f.DCGMField != baseline:
		return []error{fmt.Errorf("%s: field id %d must be named %s", where, f.DCGMFieldID, baseline)}
	case isBaseline:
		var errs []error
		if f.Tier != TierT0 {
			errs = append(errs, fmt.Errorf("%s: baseline PCIe byte rate must be tier T0", where))
		}
		if f.Raw.Unit != UnitBytesPerSec || f.Canonical == nil || f.Canonical.Unit != UnitBytesPerSec || f.Canonical.Factor != 1 {
			errs = append(errs, fmt.Errorf("%s: baseline PCIe byte rate must be bytes per second raw and canonical", where))
		}
		if f.Kind != KindGauge {
			errs = append(errs, fmt.Errorf("%s: baseline PCIe byte rate is an interval average and must be a gauge", where))
		}
		return errs
	case strings.HasPrefix(f.DCGMField, "DCGM_FI_PROF_"):
		return []error{fmt.Errorf("%s: profiling field %s is tier T2 and deferred", where, f.DCGMField)}
	}
	for _, name := range BaselinePROFFields {
		if f.DCGMField == name {
			return []error{fmt.Errorf("%s: %s carries the wrong field id %d", where, name, f.DCGMFieldID)}
		}
	}
	return nil
}

func validateRaw(where string, f Field) []error {
	var errs []error
	if f.Raw.Unit == "" {
		errs = append(errs, fmt.Errorf("%s: raw unit is empty", where))
	}
	if f.Raw.Factor != 1 {
		errs = append(errs, fmt.Errorf("%s: raw factor must be 1 (it is the reference)", where))
	}
	return errs
}

const canonicalGPUPrefix = "kubeneuron_gpu_"

var (
	metricNameRE = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)
	labelTokenRE = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)
)

// canonShape is what every row feeding one canonical series must agree on.
type canonShape struct {
	kind        Kind
	unit        Unit
	labelNames  string
	valueLabels string
	firstWhere  string
	rows        map[string]string // label set -> where
}

// validateCanonical checks the canonical side of one field and records it
// against the other rows feeding the same series.
func validateCanonical(where string, f Field, reserved map[string]bool, shapes map[string]*canonShape) []error {
	var errs []error
	k := f.Canonical
	isDCGM := f.Tier == TierT0 || f.Tier == TierT1
	if k == nil {
		if isDCGM && strings.TrimSpace(f.CanonicalDeferred) == "" {
			errs = append(errs, fmt.Errorf("%s: a DCGM field without a canonical series must say why (canonical_deferred)", where))
		}
		if f.Tier == TierT4 {
			errs = append(errs, fmt.Errorf("%s: a control-plane series is its own canonical series and must carry one", where))
		}
		return errs
	}
	if f.CanonicalDeferred != "" {
		errs = append(errs, fmt.Errorf("%s: canonical mapping and canonical_deferred are mutually exclusive", where))
	}
	if !metricNameRE.MatchString(k.Series) {
		errs = append(errs, fmt.Errorf("%s: canonical series %q is not a lower-case Prometheus metric name", where, k.Series))
	}
	if reserved[k.Series] {
		errs = append(errs, fmt.Errorf("%s: canonical series %s is reserved and may not be claimed", where, k.Series))
	}
	if isDCGM && !strings.HasPrefix(k.Series, canonicalGPUPrefix) {
		errs = append(errs, fmt.Errorf("%s: canonical series %q must start with %s", where, k.Series, canonicalGPUPrefix))
	}
	if f.Tier == TierT4 {
		if k.Series != f.Series || len(k.Labels) > 0 || len(k.ValueLabels) > 0 || k.Unit != f.Raw.Unit || k.Factor != 1 || k.Note != "" {
			errs = append(errs, fmt.Errorf("%s: a control-plane canonical series must be the series itself, unlabelled, in its own unit", where))
		}
	}
	if k.Kind != f.Kind {
		errs = append(errs, fmt.Errorf("%s: canonical kind %s differs from source kind %s; a mapping renames and rescales, it never changes the metric type", where, k.Kind, f.Kind))
	}
	hasTotal := strings.HasSuffix(k.Series, "_total")
	if (k.Kind == KindCounter) != hasTotal {
		errs = append(errs, fmt.Errorf("%s: canonical series %q must end in _total exactly when it is a counter (kind %s)", where, k.Series, k.Kind))
	}
	if !isCanonical(k.Unit) {
		errs = append(errs, fmt.Errorf("%s: canonical unit %q is not in the canonical set", where, k.Unit))
	}
	base := strings.TrimSuffix(k.Series, "_total")
	if suffix, ok := unitSuffix[k.Unit]; ok && !strings.HasSuffix(base, suffix) {
		errs = append(errs, fmt.Errorf("%s: canonical series %q must carry the unit suffix %s before any _total", where, k.Series, suffix))
	}
	for u, suffix := range unitSuffix {
		if u != k.Unit && strings.HasSuffix(base, suffix) {
			errs = append(errs, fmt.Errorf("%s: canonical series %q ends in %s but its unit is %s", where, k.Series, suffix, k.Unit))
		}
	}
	switch {
	case len(k.ValueLabels) > 0:
		if k.Factor != 0 {
			errs = append(errs, fmt.Errorf("%s: a decode mapping (value_labels) carries no scalar factor", where))
		}
		if strings.TrimSpace(k.Note) == "" {
			errs = append(errs, fmt.Errorf("%s: a decode mapping must explain the decode in its note", where))
		}
	default:
		if k.Factor <= 0 {
			errs = append(errs, fmt.Errorf("%s: canonical factor must be positive", where))
		}
		if f.Raw.Unit == k.Unit && k.Factor != 1 {
			errs = append(errs, fmt.Errorf("%s: same raw and canonical unit but factor %v", where, k.Factor))
		}
		if f.Raw.Unit != k.Unit && k.Factor == 1 {
			errs = append(errs, fmt.Errorf("%s: raw %s differs from canonical %s but factor is 1", where, f.Raw.Unit, k.Unit))
		}
	}
	names := make([]string, 0, len(k.Labels))
	for i, l := range k.Labels {
		if !isCanonicalLabel(l.Name) {
			errs = append(errs, fmt.Errorf("%s: label %q is not in the finite canonical label set %v", where, l.Name, CanonicalLabelNames))
		}
		if !labelTokenRE.MatchString(l.Value) {
			errs = append(errs, fmt.Errorf("%s: label value %q must be a lower-case token", where, l.Value))
		}
		if i > 0 && l.Name <= k.Labels[i-1].Name {
			errs = append(errs, fmt.Errorf("%s: labels must ascend by name without repeats", where))
		}
		names = append(names, l.Name)
	}
	for _, v := range k.ValueLabels {
		if !isCanonicalLabel(v) {
			errs = append(errs, fmt.Errorf("%s: value label %q is not in the finite canonical label set %v", where, v, CanonicalLabelNames))
		}
		for _, n := range names {
			if n == v {
				errs = append(errs, fmt.Errorf("%s: label %q is both fixed and derived", where, v))
			}
		}
	}
	shape, seen := shapes[k.Series]
	if !seen {
		shapes[k.Series] = &canonShape{kind: k.Kind, unit: k.Unit, labelNames: strings.Join(names, ","), valueLabels: strings.Join(k.ValueLabels, ","), firstWhere: where, rows: map[string]string{k.LabelSet(): where}}
		return errs
	}
	if shape.kind != k.Kind || shape.unit != k.Unit {
		errs = append(errs, fmt.Errorf("%s: canonical series %s is %s %s at %s but %s %s here", where, k.Series, shape.kind, shape.unit, shape.firstWhere, k.Kind, k.Unit))
	}
	if shape.labelNames != strings.Join(names, ",") || shape.valueLabels != strings.Join(k.ValueLabels, ",") {
		errs = append(errs, fmt.Errorf("%s: canonical series %s has label names {%s} at %s but {%s} here", where, k.Series, shape.labelNames, shape.firstWhere, strings.Join(names, ",")))
	}
	if prev, dup := shape.rows[k.LabelSet()]; dup {
		errs = append(errs, fmt.Errorf("%s: canonical series %s%s collides with %s; two source rows cannot feed one labelled series", where, k.Series, k.LabelSet(), prev))
	}
	shape.rows[k.LabelSet()] = where
	return errs
}

func validateSupport(where string, f Field) []error {
	var errs []error
	validated := false
	for _, hw := range allHardware {
		s, ok := f.Support[hw]
		if !ok {
			errs = append(errs, fmt.Errorf("%s: support matrix missing %s", where, hw))
			continue
		}
		switch s {
		case SupportValidated:
			validated = true
		case SupportDeclared, SupportNotApplicable, SupportUnknown:
		default:
			errs = append(errs, fmt.Errorf("%s: unknown support status %q for %s", where, s, hw))
		}
	}
	for hw := range f.Support {
		if !hasHardware(hw) {
			errs = append(errs, fmt.Errorf("%s: unknown hardware %q", where, hw))
		}
	}
	if validated && strings.TrimSpace(f.Evidence) == "" {
		errs = append(errs, fmt.Errorf("%s: validated support requires evidence", where))
	}
	if validated && f.Deferred != "" {
		errs = append(errs, fmt.Errorf("%s: a deferred entry cannot be validated", where))
	}
	if !validated && f.Evidence != "" {
		errs = append(errs, fmt.Errorf("%s: evidence given but nothing is validated", where))
	}
	if f.Evidence != "" {
		errs = append(errs, validateEvidence(where, f.Evidence)...)
	}
	return errs
}

// privateEvidenceMarkers are path shapes that point outside the source tree
// (a developer's home, a scratch directory, a bucket, a URL): evidence that a
// reader of the repository cannot audit.
var privateEvidenceMarkers = []string{"/home/", "/tmp/", "/Users/", "s3://", "gs://", "http://", "https://", "~/"}

// validateEvidence requires evidence to be auditable from the source tree
// and to state that it is not a v0.7 validation, since no v0.7 run exists.
func validateEvidence(where, evidence string) []error {
	var errs []error
	for _, m := range privateEvidenceMarkers {
		if strings.Contains(evidence, m) {
			errs = append(errs, fmt.Errorf("%s: evidence references a private or transient path (%q); cite a source-tree file or release note", where, m))
		}
	}
	if !strings.Contains(evidence, "not v0.7") {
		errs = append(errs, fmt.Errorf("%s: evidence must state that it is not a v0.7 validation", where))
	}
	return errs
}

func validateBuckets(buckets []XIDBucket) []error {
	var errs []error
	seenCode := map[int]string{}
	seenName := map[string]bool{}
	other := false
	for _, b := range buckets {
		if b.Name == "" || strings.ToLower(b.Name) != b.Name || strings.ContainsAny(b.Name, " /") {
			errs = append(errs, fmt.Errorf("xid bucket %q: name must be a lowercase label-safe token", b.Name))
		}
		if seenName[b.Name] {
			errs = append(errs, fmt.Errorf("xid bucket %q: duplicate name", b.Name))
		}
		seenName[b.Name] = true
		if b.Name == XIDBucketOther {
			other = true
			if len(b.Codes) != 0 {
				errs = append(errs, fmt.Errorf("xid bucket %q: catch-all must list no codes", b.Name))
			}
		}
		if !sort.IntsAreSorted(b.Codes) {
			errs = append(errs, fmt.Errorf("xid bucket %q: codes must be ascending", b.Name))
		}
		for _, code := range b.Codes {
			if prev, dup := seenCode[code]; dup {
				errs = append(errs, fmt.Errorf("xid bucket %q: code %d already in %q", b.Name, code, prev))
			}
			seenCode[code] = b.Name
		}
	}
	if !other {
		errs = append(errs, fmt.Errorf("xid bucket %q is required", XIDBucketOther))
	}
	return errs
}

func hasConsumer(f Field, c string) bool {
	for _, x := range f.ConsumedBy {
		if x == c {
			return true
		}
	}
	return false
}

func isCanonical(u Unit) bool {
	for _, c := range CanonicalUnits {
		if c == u {
			return true
		}
	}
	return false
}

func isCanonicalLabel(name string) bool {
	for _, n := range CanonicalLabelNames {
		if n == name {
			return true
		}
	}
	return false
}

func hasHardware(hw Hardware) bool {
	for _, x := range allHardware {
		if x == hw {
			return true
		}
	}
	return false
}
