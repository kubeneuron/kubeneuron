package gpucatalog

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// DerivedSignal is a kubeneuron_gpu_* series computed from canonical series
// (never from a dcgm-exporter series directly) by a recording rule in a
// gpu-derived-* group of CanonicalRulePath. The catalog entry is the
// contract the rule implements: Formula is the rule's expr verbatim, Inputs
// are the series the formula reads, and NoData states what the absence of
// a sample means. Absence is always honest: no formula synthesises a value
// (`or vector(0)`, `default`, `absent()`), so a derived series is missing
// exactly when an input its formula needs at that instant is missing or a
// guard (`> 0` on a divisor) excludes a sample. Which inputs are needed is
// the formula's business: most rows need every input, while the thermal
// headroom needs either the margin or both fallback inputs and is missing
// only when neither is reported. Each row's NoData states its own case.
// A derived signal is a component, never an aggregate health or error
// score: each series answers one question about one device. The one score
// in the table, the experimental shadow-only fail-slow peer score, keeps
// that boundary: it compares one device's clock ratio to its peers and sums
// no components into a health number.
type DerivedSignal struct {
	// Series is the derived series name (kubeneuron_gpu_*, Prometheus base
	// unit in the name, _total on counters).
	Series string `yaml:"series" json:"series"`
	// Kind is the metric type of the result.
	Kind Kind `yaml:"kind" json:"kind"`
	// Unit is the canonical unit of the result, from CanonicalUnits.
	Unit Unit `yaml:"unit" json:"unit"`
	// Labels are the fixed labels the recording rule adds, sorted by name.
	// Rows sharing a Series must have the same label names and pairwise
	// distinct values. Labels that an input carries (reason, type, domain,
	// class) flow through the formula instead and are listed in Carried.
	Labels []Label `yaml:"labels,omitempty" json:"labels,omitempty"`
	// Carried names the input labels the formula preserves on the result
	// (a rate over kubeneuron_gpu_throttle_seconds_total keeps reason).
	// Identity and placement labels always flow through and are not listed.
	Carried []string `yaml:"carried,omitempty" json:"carried,omitempty"`
	// Formula is the MetricsQL expression, exactly as the recording rule
	// states it. Empty only when Deferred is set.
	Formula string `yaml:"formula,omitempty" json:"formula,omitempty"`
	// Inputs are the catalog series (canonical kubeneuron_gpu_* or Tier T4
	// control-plane) the formula reads; every one must be cataloged.
	Inputs []string `yaml:"inputs,omitempty" json:"inputs,omitempty"`
	// NoData states what an absent sample means and when absence is
	// expected (an unsupported sensor, a zero divisor, a source the
	// hardware does not report).
	NoData string `yaml:"no_data" json:"no_data"`
	// Experimental marks a signal whose formula may change before it is
	// relied on by a controller path; only the Fail-slow Radar board and
	// the shadow-only ShadowGpuFailSlow alert read it, and neither acts.
	Experimental bool `yaml:"experimental,omitempty" json:"experimental,omitempty"`
	// Deferred, when set, is the reason this signal has no rule: the series
	// is intentionally No data and the catalog says why.
	Deferred string `yaml:"deferred,omitempty" json:"deferred,omitempty"`
	// Help is the one-line meaning of the series.
	Help string `yaml:"help" json:"help"`
	// Group is the gpu-derived-* recording group the rule lives in.
	Group string `yaml:"group,omitempty" json:"group,omitempty"`
}

// DerivedGroupPrefix names the recording groups that hold derived rules;
// gpu-canonical-* groups hold the normalisation rules only.
const DerivedGroupPrefix = "gpu-derived-"

// DerivedLabelNames is the closed set of fixed label names a derived rule
// may add, in addition to CanonicalLabelNames. `component` keeps the hard
// memory-fault signals apart without ever summing them into a score.
var DerivedLabelNames = []string{"class", "component"}

// Derived recording group names, in file order.
const (
	DerivedGroupThermal  = DerivedGroupPrefix + "thermal"
	DerivedGroupPower    = DerivedGroupPrefix + "power"
	DerivedGroupClocks   = DerivedGroupPrefix + "clocks"
	DerivedGroupMemory   = DerivedGroupPrefix + "memory"
	DerivedGroupLinks    = DerivedGroupPrefix + "links"
	DerivedGroupCapacity = DerivedGroupPrefix + "capacity"
	DerivedGroupFailSlow = DerivedGroupPrefix + "fail-slow"
)

// The fail-slow shadow signal (DerivedGroupFailSlow, experimental, recording
// only) scores 1 - own SM clock ratio / median SM clock ratio of the
// gpu_model peer group. A sample is recorded only when the score is at
// least 0.15, the device's maximum throttle share over 5m is at least 0.05
// and the peer group has at least 3 distinct devices. Input series are
// deduplicated to (node, gpu_uuid, gpu_model) first, so workload labels
// (pod, namespace, container) can neither inflate the peer count nor weight
// the median. The thresholds live in the formula and the no_data text only.

// DerivedRateWindow is the range every derived rate() uses; it must exceed
// the 30s canonical interval by a margin that survives a missed scrape.
const DerivedRateWindow = "5m"

// DerivedSignals is the shipped derived-signal table (v0.7 M3, slices 2-3).
// A row may read a derived series declared earlier in this table (the
// fail-slow score reads the clock and throttle ratios). Excluded on
// purpose: any aggregate health or error score (misleading by construction;
// the experimental fail-slow peer score is the only permitted score and it
// aggregates nothing, see above), anything reading XID 230, recovery action
// 1523 or the uncataloged NVLink byte rates 1011/1012. Fixed-label inputs
// that must not leak into a result are removed with label_del, which the
// pinned vmalert (v1.147.0) accepts.
var DerivedSignals = []DerivedSignal{
	{
		Series: "kubeneuron_gpu_thermal_headroom_celsius", Kind: KindGauge, Unit: UnitCelsius, Group: DerivedGroupThermal,
		Formula: `kubeneuron_gpu_thermal_margin_celsius or (label_del(kubeneuron_gpu_temperature_limit_celsius{limit="slowdown"}, "limit") - kubeneuron_gpu_temperature_celsius)`,
		Inputs:  []string{"kubeneuron_gpu_thermal_margin_celsius", "kubeneuron_gpu_temperature_limit_celsius", "kubeneuron_gpu_temperature_celsius"},
		NoData:  "absent when the GPU reports neither a thermal margin (153) nor both a slowdown threshold (158) and a temperature (150); the fallback is used only for devices with no margin sample, never summed with it.",
		Help:    "Degrees of headroom before thermal slowdown: the reported thermal margin, else slowdown threshold minus GPU temperature.",
	},
	{
		Series: "kubeneuron_gpu_memory_thermal_headroom_celsius", Kind: KindGauge, Unit: UnitCelsius, Group: DerivedGroupThermal,
		Formula: `kubeneuron_gpu_memory_temperature_limit_celsius - kubeneuron_gpu_memory_temperature_celsius`,
		Inputs:  []string{"kubeneuron_gpu_memory_temperature_limit_celsius", "kubeneuron_gpu_memory_temperature_celsius"},
		NoData:  "absent when the memory temperature (140) or its maximum operating temperature (151) is not reported; whether the Tesla T4 reports either is not established.",
		Help:    "Degrees of headroom before the memory's maximum operating temperature.",
	},
	{
		Series: "kubeneuron_gpu_power_ratio", Kind: KindGauge, Unit: UnitRatio, Group: DerivedGroupPower,
		Formula: `kubeneuron_gpu_power_watts / (kubeneuron_gpu_power_limit_watts > 0)`,
		Inputs:  []string{"kubeneuron_gpu_power_watts", "kubeneuron_gpu_power_limit_watts"},
		NoData:  "absent when power draw (155) or the enforced limit (164) is not reported, or the limit is 0 (no division by zero is recorded).",
		Help:    "Power draw as a fraction of the enforced power limit; above 1 means the limit is being exceeded at the sample instant.",
	},
	{
		Series: "kubeneuron_gpu_throttle_share_ratio", Kind: KindGauge, Unit: UnitRatio, Group: DerivedGroupPower, Carried: []string{"reason"},
		Formula: `rate(kubeneuron_gpu_throttle_seconds_total[5m])`,
		Inputs:  []string{"kubeneuron_gpu_throttle_seconds_total"},
		NoData:  "absent per reason when the violation-time counter for that reason is not reported or has fewer than two samples in the window; a reason the hardware never throttles on reads 0, not absent.",
		Help:    "Fraction of wall time spent throttled, per reason (power, thermal, sync_boost, board_limit, reliability), over the last 5m.",
	},
	{
		Series: "kubeneuron_gpu_clock_ratio", Kind: KindGauge, Unit: UnitRatio, Group: DerivedGroupClocks, Carried: []string{"domain"},
		Formula: `kubeneuron_gpu_clock_hertz / (kubeneuron_gpu_clock_max_hertz > 0)`,
		Inputs:  []string{"kubeneuron_gpu_clock_hertz", "kubeneuron_gpu_clock_max_hertz"},
		NoData:  "absent per domain (sm, memory) when the current or the maximum clock is not reported, or the maximum is 0.",
		Help:    "Current clock as a fraction of the maximum clock, per domain; the sm domain is the SM clock ratio.",
	},
	{
		Series: "kubeneuron_gpu_memory_used_ratio", Kind: KindGauge, Unit: UnitRatio, Group: DerivedGroupMemory,
		Formula: `kubeneuron_gpu_memory_used_bytes / (kubeneuron_gpu_memory_total_bytes > 0)`,
		Inputs:  []string{"kubeneuron_gpu_memory_used_bytes", "kubeneuron_gpu_memory_total_bytes"},
		NoData:  "absent when framebuffer used (252) or total (250) is not reported, or total is 0.",
		Help:    "Framebuffer memory in use as a fraction of total framebuffer memory.",
	},
	{
		Series: "kubeneuron_gpu_ecc_corrected_errors_per_second", Kind: KindGauge, Unit: UnitCount, Group: DerivedGroupMemory, Carried: []string{"scope"},
		Formula: `label_del(rate(kubeneuron_gpu_ecc_errors_total{bits="single"}[5m]), "bits")`,
		Inputs:  []string{"kubeneuron_gpu_ecc_errors_total"},
		NoData:  "absent per scope when the single-bit ECC counter for that scope (310 volatile, 312 aggregate) is not reported (ECC disabled or unsupported) or has fewer than two samples in the window; scope stays on the result so the two never collide.",
		Help:    "Corrected (single-bit) ECC errors per second over the last 5m, per scope (volatile, aggregate); a soft-error rate, not a fault.",
	},
	{
		Series: "kubeneuron_gpu_ecc_uncorrectable_errors_per_second", Kind: KindGauge, Unit: UnitCount, Group: DerivedGroupMemory, Carried: []string{"scope"},
		Formula: `label_del(rate(kubeneuron_gpu_ecc_errors_total{bits="double"}[5m]), "bits")`,
		Inputs:  []string{"kubeneuron_gpu_ecc_errors_total"},
		NoData:  "absent per scope when the double-bit ECC counter for that scope (311 volatile, 313 aggregate) is not reported or has fewer than two samples in the window; scope stays on the result so the two never collide.",
		Help:    "Uncorrectable (double-bit) ECC errors per second over the last 5m, per scope (volatile, aggregate).",
	},
	{
		Series: "kubeneuron_gpu_retired_pages_per_second", Kind: KindGauge, Unit: UnitCount, Group: DerivedGroupMemory, Carried: []string{"cause"},
		Formula: `rate(kubeneuron_gpu_retired_pages_total[5m])`,
		Inputs:  []string{"kubeneuron_gpu_retired_pages_total"},
		NoData:  "absent per cause (single_bit, double_bit) when that retired-page counter (390, 391) is not reported (page retirement is pre-Ampere; Ampere+ remaps rows instead) or has fewer than two samples in the window.",
		Help:    "Pages retired per second over the last 5m, per cause (single_bit, double_bit); pre-Ampere page retirement only.",
	},
	{
		Series: "kubeneuron_gpu_memory_hard_fault_active", Kind: KindGauge, Unit: UnitBool, Group: DerivedGroupMemory, Labels: []Label{L("component", "uncorrectable_ecc_volatile")},
		Formula: `label_del(increase(kubeneuron_gpu_ecc_errors_total{bits="double",scope="volatile"}[1h]), "bits", "scope") > bool 0`,
		Inputs:  []string{"kubeneuron_gpu_ecc_errors_total"},
		NoData:  "absent when the volatile double-bit ECC counter (311) is not reported (ECC disabled or unsupported) or has fewer than two samples in the 1h window; 0 when it is reported and did not grow in the last hour. Reads 311 only; the aggregate counter (313) is its own component, and the two are never combined.",
		Help:    "1 while the volatile (since last reset) uncorrectable ECC counter grew in the last hour. One component of hard memory fault; components are never summed.",
	},
	{
		Series: "kubeneuron_gpu_memory_hard_fault_active", Kind: KindGauge, Unit: UnitBool, Group: DerivedGroupMemory, Labels: []Label{L("component", "uncorrectable_ecc_aggregate")},
		Formula: `label_del(increase(kubeneuron_gpu_ecc_errors_total{bits="double",scope="aggregate"}[1h]), "bits", "scope") > bool 0`,
		Inputs:  []string{"kubeneuron_gpu_ecc_errors_total"},
		NoData:  "absent when the aggregate (lifetime) double-bit ECC counter (313) is not reported (ECC disabled or unsupported) or has fewer than two samples in the 1h window; 0 when it is reported and did not grow in the last hour. Reads 313 only; the volatile counter (311) is its own component, and the two are never combined.",
		Help:    "1 while the aggregate (lifetime, persists across resets) uncorrectable ECC counter grew in the last hour. One component of hard memory fault; components are never summed.",
	},
	{
		Series: "kubeneuron_gpu_memory_hard_fault_active", Kind: KindGauge, Unit: UnitBool, Group: DerivedGroupMemory, Labels: []Label{L("component", "uncorrectable_row_remap")},
		Formula: `label_del(increase(kubeneuron_gpu_remapped_rows_total{kind="uncorrectable"}[1h]), "kind") > bool 0`,
		Inputs:  []string{"kubeneuron_gpu_remapped_rows_total"},
		NoData:  "absent when the uncorrectable remapped-rows counter (393) is not reported (row remapping is Ampere+, not-applicable on the Tesla T4); 0 when reported and unchanged in the last hour.",
		Help:    "1 while at least one row was remapped for an uncorrectable error in the last hour.",
	},
	{
		Series: "kubeneuron_gpu_memory_hard_fault_active", Kind: KindGauge, Unit: UnitBool, Group: DerivedGroupMemory, Labels: []Label{L("component", "row_remap_failed")},
		Formula: `kubeneuron_gpu_row_remap_failed > bool 0`,
		Inputs:  []string{"kubeneuron_gpu_row_remap_failed"},
		NoData:  "absent when the row-remap failure flag (395) is not reported (Ampere+ only).",
		Help:    "1 while the GPU reports a failed row remap, which is unrecoverable without service.",
	},
	{
		Series: "kubeneuron_gpu_memory_hard_fault_active", Kind: KindGauge, Unit: UnitBool, Group: DerivedGroupMemory, Labels: []Label{L("component", "row_remap_pending")},
		Formula: `kubeneuron_gpu_row_remap_pending > bool 0`,
		Inputs:  []string{"kubeneuron_gpu_row_remap_pending"},
		NoData:  "absent when the row-remap pending flag (396) is not reported (Ampere+ only); 0 when reported and no remap awaits a reset.",
		Help:    "1 while a row remap is pending a GPU reset; the fault is recorded but not yet repaired.",
	},
	{
		Series: "kubeneuron_gpu_memory_hard_fault_active", Kind: KindGauge, Unit: UnitBool, Group: DerivedGroupMemory, Labels: []Label{L("component", "retired_page_pending")},
		Formula: `kubeneuron_gpu_retired_pages_pending > bool 0`,
		Inputs:  []string{"kubeneuron_gpu_retired_pages_pending"},
		NoData:  "absent when the retired-page pending flag (392) is not reported (pre-Ampere only); 0 when reported and no retirement awaits a reset.",
		Help:    "1 while a page retirement is pending a GPU reset (pre-Ampere).",
	},
	{
		Series: "kubeneuron_gpu_memory_hard_fault_active", Kind: KindGauge, Unit: UnitBool, Group: DerivedGroupMemory, Labels: []Label{L("component", "unrepairable")},
		Formula: `kubeneuron_gpu_memory_unrepairable > bool 0`,
		Inputs:  []string{"kubeneuron_gpu_memory_unrepairable"},
		NoData:  "absent when the unrepairable-memory flag (1507) is not reported.",
		Help:    "1 while the GPU reports unrepairable memory.",
	},
	{
		Series: "kubeneuron_gpu_pcie_replays_per_second", Kind: KindGauge, Unit: UnitCount, Group: DerivedGroupLinks,
		Formula: `rate(kubeneuron_gpu_pcie_replays_total[5m])`,
		Inputs:  []string{"kubeneuron_gpu_pcie_replays_total"},
		NoData:  "absent when the PCIe replay counter (202) is not reported or has fewer than two samples in the window.",
		Help:    "PCIe replay events per second over the last 5m.",
	},
	{
		Series: "kubeneuron_gpu_pcie_link_width_ratio", Kind: KindGauge, Unit: UnitRatio, Group: DerivedGroupLinks,
		Formula: `label_del(kubeneuron_gpu_pcie_link_width{kind="current"}, "kind") / (label_del(kubeneuron_gpu_pcie_link_width{kind="max"}, "kind") > 0)`,
		Inputs:  []string{"kubeneuron_gpu_pcie_link_width"},
		NoData:  "absent when the current (237) or maximum (236) link width is not reported, or the maximum is 0.",
		Help:    "Current PCIe link width as a fraction of the maximum width; below 1 means the link trained narrow.",
	},
	{
		Series: "kubeneuron_gpu_pcie_link_generation_ratio", Kind: KindGauge, Unit: UnitRatio, Group: DerivedGroupLinks,
		Formula: `label_del(kubeneuron_gpu_pcie_link_generation{kind="current"}, "kind") / (label_del(kubeneuron_gpu_pcie_link_generation{kind="max"}, "kind") > 0)`,
		Inputs:  []string{"kubeneuron_gpu_pcie_link_generation"},
		NoData:  "absent when the current (238) or maximum (235) link generation is not reported, or the maximum is 0. The current generation drops legitimately at idle on many boards; a low ratio under load is the signal.",
		Help:    "Current PCIe link generation as a fraction of the maximum generation.",
	},
	{
		Series: "kubeneuron_gpu_nvlink_errors_per_second", Kind: KindGauge, Unit: UnitCount, Group: DerivedGroupLinks, Carried: []string{"type"},
		Formula: `rate(kubeneuron_gpu_nvlink_errors_total[5m])`,
		Inputs:  []string{"kubeneuron_gpu_nvlink_errors_total"},
		NoData:  "absent per type (crc_flit, crc_data, replay, recovery) when that NVLink counter is not reported (no NVLink on the Tesla T4) or has fewer than two samples in the window.",
		Help:    "NVLink errors per second over the last 5m, per error type.",
	},
	{
		Series: "kubeneuron_gpu_recovered_capacity_hours_total", Kind: KindCounter, Unit: UnitHours, Group: DerivedGroupCapacity, Carried: []string{"class"},
		Formula: `label_del(kubeneuron_degraded_gpu_seconds_total{outcome="resolved"}, "outcome") / 3600`,
		Inputs:  []string{"kubeneuron_degraded_gpu_seconds_total"},
		NoData:  "absent until the controller has closed at least one incident as resolved for the class; a fleet with no resolved incident has no sample, not 0.",
		Help:    "GPU-hours returned to service by resolved incidents, per problem class (resolved degraded GPU-seconds / 3600).",
	},
	{
		Series: "kubeneuron_gpu_fail_slow_score_ratio", Kind: KindGauge, Unit: UnitRatio, Group: DerivedGroupFailSlow, Experimental: true,
		Formula: `(1 - max by (node, gpu_uuid, gpu_model) (kubeneuron_gpu_clock_ratio{domain="sm"}) / on (gpu_model) group_left() (median by (gpu_model) (max by (node, gpu_uuid, gpu_model) (kubeneuron_gpu_clock_ratio{domain="sm"})) > 0)) >= 0.15 and on (node, gpu_uuid, gpu_model) (max by (node, gpu_uuid, gpu_model) (kubeneuron_gpu_throttle_share_ratio) >= 0.05) and on (gpu_model) (count by (gpu_model) (max by (node, gpu_uuid, gpu_model) (kubeneuron_gpu_clock_ratio{domain="sm"})) >= 3)`,
		Inputs:  []string{"kubeneuron_gpu_clock_ratio", "kubeneuron_gpu_throttle_share_ratio"},
		NoData:  "shadow only: read by the Fail-slow Radar board and the shadow-only ShadowGpuFailSlow alert, which route to humans and trigger no remediation; no controller path consumes it. Absent unless all three gates hold at the sample instant: score = 1 - own SM clock ratio / median SM clock ratio of the gpu_model peer group is at least 0.15 (own clock at least 15% below the peer median); the device's maximum throttle share over 5m across reasons is at least 0.05; and the gpu_model has at least 3 distinct devices (node, gpu_uuid, gpu_model) reporting an SM clock ratio. Inputs are deduplicated to (node, gpu_uuid, gpu_model) before the median and the count, so pod, namespace and container labels do not appear on the result and cannot weight the peer group. Also absent when the peer median is 0 or either input is missing. A healthy, idle or singleton device has no sample, not 0.",
		Help:    "Fail-slow degradation score: how far the SM clock ratio sits below its gpu_model peer median (1 - own/peer_median), recorded only when the device is also throttled and has at least 3 peers.",
	},
	{
		Series: "kubeneuron_gpu_present", Kind: KindGauge, Unit: UnitBool,
		Deferred: "intentionally No data: a presence signal needs the node's allocatable nvidia.com/gpu from kube-state-metrics, which is not an established dependency of this project. Until it is, there is no honest source for a GPU that exists but reports nothing, and no rule records this series.",
		NoData:   "always absent in this version; see deferred.",
		Help:     "1 while the node advertises the GPU as allocatable. Not recorded.",
	},
}

var derivedRecordRE = regexp.MustCompile(`^kubeneuron_gpu_[a-z0-9_]+$`)

// derivedForbidden are expression fragments that would synthesise a sample
// or read a source the profile does not expose.
var derivedForbidden = []string{"or vector(", "default", "absent(", "DCGM_FI_DEV_XID_ERRORS", "DCGM_FI_DEV_GPU_RECOVERY_ACTION", "DCGM_FI_PROF_NVLINK", "DCGM_FI_"}

// DerivedSeries returns the distinct derived series names, sorted.
func (c Catalog) DerivedSeries() []string {
	seen := map[string]bool{}
	var out []string
	for _, d := range c.Derived {
		if !seen[d.Series] {
			seen[d.Series] = true
			out = append(out, d.Series)
		}
	}
	sort.Strings(out)
	return out
}

// LookupDerived returns every derived row that records a series, in
// catalog order.
func (c Catalog) LookupDerived(series string) []DerivedSignal {
	var out []DerivedSignal
	for _, d := range c.Derived {
		if d.Series == series {
			out = append(out, d)
		}
	}
	return out
}

// LabelSet renders the fixed labels as a selector fragment, like Canonical.
func (d DerivedSignal) LabelSet() string {
	return Canonical{Labels: d.Labels}.LabelSet()
}

func validateDerived(c Catalog, reserved map[string]bool) []error {
	var errs []error
	known := map[string]bool{}
	for _, s := range c.CanonicalSeries() {
		known[s] = true
	}
	for _, f := range c.Fields {
		if f.Tier == TierT4 {
			known[f.Series] = true
		}
	}
	allowed := map[string]bool{}
	for _, n := range append(append([]string{}, CanonicalLabelNames...), DerivedLabelNames...) {
		allowed[n] = true
	}
	units := map[Unit]bool{}
	for _, u := range CanonicalUnits {
		units[u] = true
	}
	shapes := map[string]*canonShape{}
	seenKey := map[string]bool{}
	// declaredDerived holds the derived series whose every row precedes the
	// current one, so an input may be a derived series only when it is
	// declared earlier in the table: table order is evaluation order, a
	// self-reference or a cycle cannot be expressed, and the result is
	// deterministic.
	declaredDerived := map[string]bool{}
	for i, d := range c.Derived {
		where := fmt.Sprintf("derived[%d] %s%s", i, d.Series, d.LabelSet())
		if !derivedRecordRE.MatchString(d.Series) {
			errs = append(errs, fmt.Errorf("%s: series must be %s* in snake case", where, canonicalGPUPrefix))
		}
		if reserved[d.Series] {
			errs = append(errs, fmt.Errorf("%s: records a reserved canonical name", where))
		}
		if known[d.Series] {
			errs = append(errs, fmt.Errorf("%s: collides with a canonical or control-plane series", where))
		}
		if d.Kind != KindGauge && d.Kind != KindCounter {
			errs = append(errs, fmt.Errorf("%s: kind must be gauge or counter, got %q", where, d.Kind))
		}
		if !units[d.Unit] {
			errs = append(errs, fmt.Errorf("%s: unit %q is not a canonical unit", where, d.Unit))
		}
		if (d.Kind == KindCounter) != strings.HasSuffix(d.Series, "_total") {
			errs = append(errs, fmt.Errorf("%s: _total suffix exactly on counters", where))
		}
		if suffix, ok := unitSuffix[d.Unit]; ok && !strings.HasSuffix(strings.TrimSuffix(d.Series, "_total"), suffix) && !strings.HasSuffix(d.Series, "_per_second") {
			errs = append(errs, fmt.Errorf("%s: unit %s requires the %s suffix", where, d.Unit, suffix))
		}
		for _, s := range []struct{ name, v string }{{"help", d.Help}, {"no_data", d.NoData}} {
			if strings.TrimSpace(s.v) == "" {
				errs = append(errs, fmt.Errorf("%s: %s is empty", where, s.name))
			}
		}
		key := d.Series + d.LabelSet()
		if seenKey[key] {
			errs = append(errs, fmt.Errorf("%s: recorded twice", where))
		}
		seenKey[key] = true
		for j, l := range d.Labels {
			if !allowed[l.Name] {
				errs = append(errs, fmt.Errorf("%s: label %q is not a canonical or derived label name", where, l.Name))
			}
			if j > 0 && d.Labels[j-1].Name >= l.Name {
				errs = append(errs, fmt.Errorf("%s: labels must be sorted by name without repeats", where))
			}
		}
		for _, l := range d.Carried {
			if !allowed[l] {
				errs = append(errs, fmt.Errorf("%s: carried label %q is not a canonical or derived label name", where, l))
			}
		}
		names := make([]string, 0, len(d.Labels))
		for _, l := range d.Labels {
			names = append(names, l.Name)
		}
		sig := strings.Join(names, ",") + "|" + strings.Join(d.Carried, ",")
		if sh, ok := shapes[d.Series]; ok {
			if sh.kind != d.Kind || sh.unit != d.Unit || sh.labelNames != sig {
				errs = append(errs, fmt.Errorf("%s: rows of one derived series must share kind, unit and label names", where))
			}
		} else {
			shapes[d.Series] = &canonShape{kind: d.Kind, unit: d.Unit, labelNames: sig}
		}

		if d.Deferred != "" {
			if d.Formula != "" || len(d.Inputs) > 0 || d.Group != "" {
				errs = append(errs, fmt.Errorf("%s: a deferred derived signal has no formula, inputs or group", where))
			}
			continue
		}
		if strings.TrimSpace(d.Formula) == "" {
			errs = append(errs, fmt.Errorf("%s: formula is empty and the signal is not deferred", where))
		}
		if !strings.HasPrefix(d.Group, DerivedGroupPrefix) {
			errs = append(errs, fmt.Errorf("%s: group must be %s*", where, DerivedGroupPrefix))
		}
		if len(d.Inputs) == 0 {
			errs = append(errs, fmt.Errorf("%s: inputs are empty", where))
		}
		for _, in := range d.Inputs {
			if in == d.Series {
				errs = append(errs, fmt.Errorf("%s: input %s is the series itself", where, in))
			} else if !known[in] && !declaredDerived[in] {
				errs = append(errs, fmt.Errorf("%s: input %s is neither a canonical, control-plane nor previously declared derived series", where, in))
			}
			if !strings.Contains(d.Formula, in) {
				errs = append(errs, fmt.Errorf("%s: input %s does not appear in the formula", where, in))
			}
		}
		for _, bad := range derivedForbidden {
			if strings.Contains(d.Formula, bad) {
				errs = append(errs, fmt.Errorf("%s: formula contains %q; derived rules read canonical series only and never synthesise a sample", where, bad))
			}
		}
		for _, opt := range []string{"cluster", "node_pool", "driver"} {
			if strings.Contains(d.Formula, opt) {
				errs = append(errs, fmt.Errorf("%s: formula mentions optional label %s", where, opt))
			}
		}
		declaredDerived[d.Series] = true
	}
	return errs
}
