// Package amdhealth is the AMD sibling of internal/agent/gpuhealth: a
// polling, observation-only detection source that turns amd-smi (preferred) or
// rocm-smi (fallback) output into the same vendor-neutral types.AgentEvent the
// rest of the pipeline already understands. It never touches the executor and
// never enables an action.
//
// UNVALIDATED AGAINST REAL HARDWARE. Every parser here, and every fixture under
// testdata/, was written without access to an AMD accelerator: the field names,
// the JSON shapes, and the rocm-smi key spellings are plausible reconstructions,
// not captures. That is why the parsers refuse rather than guess — a field this
// package cannot find yields NO fault and a log line naming what was missing,
// so the honest failure mode on real hardware is "this source saw nothing",
// never "this source invented a fault on a healthy GPU". Re-verify the fixtures
// against a real node before trusting a silent poll as evidence of health.
//
// It mirrors gpuhealth's safety posture deliberately:
//
//   - Real-binary evidence only. New() enables a tool only if it resolves on
//     PATH; there is no fake AMD driver and no simulated output.
//   - A missing or unparseable tool degrades to observed-only, logged once, and
//     is retried on the next tick rather than crashing the agent.
//   - Monotonic counters (ECC, retired pages, XGMI errors) are baselined on the
//     first poll so a long-standing historical aggregate is never replayed as a
//     fresh fault at startup, and only a genuine increase emits.
//   - Ambiguity refuses: a temperature with no operator-configured critical
//     threshold and no explicit throttle flag is observed, never promoted to a
//     thermal fault, because the critical temperature is SKU-specific and
//     inferring one would be fabrication.
//
// Unlike gpuhealth it keeps NO durable cross-restart state, and that is a
// decision rather than an omission: gpuhealth needs one because DCGM retains its
// last-XID field for the life of nv-hostengine, so a restart re-reads a fault
// that may be long remediated. No series here has that property — counters are
// baselined in memory and device presence is compared only against what THIS
// process has seen — so a restart can only under-report (the safe direction),
// never re-open a remediated incident.
package amdhealth

import (
	"context"
	"fmt"
	"log/slog"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/kubeneuron/kubeneuron/pkg/types"
)

const (
	// DefaultPollInterval is how often the source samples amd-smi / rocm-smi.
	// It matches gpuhealth's cadence: this is the slow path, and a shorter
	// interval buys nothing but subprocess load on a node under stress.
	DefaultPollInterval = 30 * time.Second
	// probeTimeout bounds one amd-smi / rocm-smi invocation so a wedged driver
	// cannot stall the poll loop indefinitely.
	probeTimeout = 15 * time.Second

	// FaultVendor and the source names are the provenance stamped on every
	// emitted FaultSignal. They are what makes an AMD detection distinguishable
	// downstream without a second event type.
	FaultVendor     = "amd"
	SourceAMDSMI    = "amd-smi"
	SourceROCmSMI   = "rocm-smi"
	CodeECCUncorr   = "ecc-uncorrectable"
	CodeECCCorrRate = "ecc-correctable-rate"
	CodePageRetire  = "page-retirement"
	// CodePageRetireExhausted is the peer of NVIDIA's XID 64: not "a page was
	// retired" (the mechanism working) but "the device has retired so many
	// pages that its spare budget is gone". It is a level, not an event — the
	// GPU is out of spares right now — so it is emitted as a state series.
	CodePageRetireExhausted = "page-retirement-exhausted"
	CodeXGMILink            = "xgmi-link-error"
	CodeThermal             = "thermal-throttle"
	CodeGPULost             = "gpu-lost"

	// defaultCorrectableRateMinDelta is how many NEW correctable ECC errors a
	// GPU must accumulate since the last report before this source raises the
	// rate fault again. Corrected errors are routine on HBM: emitting one event
	// per increment would post thousands of events a day per GPU and bury the
	// uncorrectable faults that matter. The number is a conservative first pass
	// chosen WITHOUT AMD field data — operators with real fleets should set
	// CorrectableRateMinDelta from their own observed baseline.
	defaultCorrectableRateMinDelta = 128

	// defaultXGMILinkMinDelta is how many NEW XGMI link errors must accumulate
	// before the fabric fault reports again. XGMI link errors are classified
	// CRITICAL, and the counter increments on corrected link retries as well as
	// on real degradation: with no delta a single retry under load pages
	// somebody about a healthy fabric. Like the correctable-ECC delta this
	// number was chosen WITHOUT AMD field data — it is set low because a
	// degrading link climbs fast, and operators with real fleets should set
	// XGMILinkMinDelta from their own observed baseline.
	defaultXGMILinkMinDelta = 4
)

// runner abstracts subprocess execution for tests, matching gpuhealth and the
// nvml/dcgm packages.
type runner func(ctx context.Context, name string, args ...string) ([]byte, error)

// Watcher polls an AMD GPU health tool and emits neutral fault events.
type Watcher struct {
	// NodeName stamps every emitted event.
	NodeName string
	// AMDSMIPath is the amd-smi binary; empty disables the preferred path.
	AMDSMIPath string
	// ROCmSMIPath is the rocm-smi binary used when amd-smi is absent or fails;
	// empty disables the fallback.
	ROCmSMIPath string
	// PollInterval defaults to DefaultPollInterval.
	PollInterval time.Duration
	// ThermalCriticalC, when > 0, is the hotspot temperature at or above which
	// an observed temperature becomes a thermal fault. It is deliberately
	// unset by default: the critical temperature is SKU-specific, so with no
	// operator-supplied value a temperature is observed and never promoted.
	ThermalCriticalC float64
	// CorrectableRateMinDelta overrides defaultCorrectableRateMinDelta.
	CorrectableRateMinDelta uint64
	// XGMILinkMinDelta overrides defaultXGMILinkMinDelta.
	XGMILinkMinDelta uint64
	// BadPageThreshold, when > 0, is the retired-page count at or above which
	// the device is reported as having exhausted its spare memory — a
	// replace-the-GPU condition rather than the routine retirement below it.
	// It is deliberately unset by default, for the same reason as
	// ThermalCriticalC: the bad-page budget differs by SKU and HBM stack, so
	// with no operator-supplied value KubeNeuron reports retirements and never
	// claims the device is out of spares.
	BadPageThreshold uint64
	// Logger records source selection, degradation, and every refusal; nil
	// disables logging, which is only appropriate in tests.
	Logger *slog.Logger

	run runner

	mu sync.Mutex
	// seen is the last observed value per series key; baselined marks a counter
	// whose first (historical) value has been absorbed.
	seen      map[string]uint64
	baselined map[string]bool
	// emittedAt is the counter value at the last emission for a series with a
	// minimum delta, so the delta is measured against what was REPORTED rather
	// than against the previous poll — a slow trickle must still eventually
	// report, and a fast burst must not report on every tick.
	emittedAt map[string]uint64
	// lastKnown remembers each index's identity while it is present, so a GPU
	// that disappears can still be named in the event that reports its loss.
	lastKnown map[int]deviceIdentity
	// loggedOnce suppresses repeat log lines for a condition that persists
	// across every poll (a missing tool, an unthresholded temperature).
	loggedOnce map[string]bool
}

// deviceIdentity is the last identity a tool reported for a GPU index, and
// which tool reported it. The source is part of the record because presence
// may only be compared within one tool's own view of the node.
type deviceIdentity struct {
	uuid   string
	bdf    string
	source string
}

// New builds an AMD health watcher, enabling only the tools that actually
// resolve on this node. A tool that is not installed is left disabled rather
// than treated as an error: an NVIDIA-only or CPU-only node simply has no AMD
// source. Enabled reports whether anything was found, so the caller can refuse
// to wire a source that would never observe anything.
func New(nodeName, amdSMIPath, rocmSMIPath string) *Watcher {
	w := &Watcher{
		NodeName:     nodeName,
		PollInterval: DefaultPollInterval,
		run: func(ctx context.Context, name string, args ...string) ([]byte, error) {
			return exec.CommandContext(ctx, name, args...).CombinedOutput()
		},
	}
	if p := strings.TrimSpace(amdSMIPath); p != "" {
		if resolved, err := exec.LookPath(p); err == nil {
			w.AMDSMIPath = resolved
		}
	}
	if p := strings.TrimSpace(rocmSMIPath); p != "" {
		if resolved, err := exec.LookPath(p); err == nil {
			w.ROCmSMIPath = resolved
		}
	}
	return w
}

// Enabled reports whether at least one AMD tool is present. A watcher that is
// not enabled would poll nothing forever, so the agent declines to wire it and
// says so once at startup instead of running a permanently silent source that
// looks like coverage.
func (w *Watcher) Enabled() bool {
	return w.AMDSMIPath != "" || w.ROCmSMIPath != ""
}

// Watch polls until ctx is done and streams neutral AMD fault events. It never
// returns an error: an unusable tool degrades to an empty, observed-only stream
// rather than failing the agent's detection wiring.
func (w *Watcher) Watch(ctx context.Context) (<-chan types.AgentEvent, error) {
	if w.run == nil {
		w.run = func(ctx context.Context, name string, args ...string) ([]byte, error) {
			return exec.CommandContext(ctx, name, args...).CombinedOutput()
		}
	}
	interval := w.PollInterval
	if interval <= 0 {
		interval = DefaultPollInterval
	}
	ch := make(chan types.AgentEvent, 64)
	go func() {
		defer close(ch)
		tick := time.NewTicker(interval)
		defer tick.Stop()
		// The first poll only establishes baselines (see filter), so it costs
		// one subprocess and buys a correct starting point for every counter.
		w.pollAndEmit(ctx, ch)
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
				w.pollAndEmit(ctx, ch)
			}
		}
	}()
	return ch, nil
}

func (w *Watcher) pollAndEmit(ctx context.Context, ch chan<- types.AgentEvent) {
	for _, ev := range w.poll(ctx) {
		select {
		case ch <- ev:
		case <-ctx.Done():
			return
		}
	}
}

// candidate is one observation before edge/level filtering.
type candidate struct {
	index int
	uuid  string
	bdf   string
	code  string
	// source names the tool that observed it, so an operator reading an
	// incident knows whether the evidence came from amd-smi or the narrower
	// rocm-smi fallback.
	source string
	attrs  map[string]string
	raw    string
	// key identifies the observation series over time. It is keyed by code and
	// GPU index, NOT by tool: amd-smi and rocm-smi enumerate the same devices,
	// so a fallback poll must continue the same counter series rather than
	// starting a second one that re-baselines and hides an increase.
	key   string
	value uint64
	// minDelta is the increase a counter must accumulate since its last
	// EMISSION before it reports again. Zero means any increase reports.
	minDelta uint64
	// level marks a state series (device presence): it reports on the
	// transition into the faulted value and stays quiet while it persists.
	level bool
}

// poll runs one probe and returns the events that survive filtering. amd-smi is
// preferred; a failure falls back to rocm-smi for this poll and is retried on
// the next tick. Two tools failing is degradation, not an error: the source goes
// observed-only and says so once.
func (w *Watcher) poll(ctx context.Context) []types.AgentEvent {
	var (
		samples    []sample
		source     string
		haveSource bool
	)
	if w.AMDSMIPath != "" {
		if s, err := w.pollAMDSMI(ctx); err != nil {
			w.logOnce("amd-smi-failed", slog.LevelWarn,
				"amd-smi health probe failed; falling back to rocm-smi for this poll", "err", err)
		} else {
			samples, source, haveSource = s, SourceAMDSMI, true
		}
	}
	if !haveSource && w.ROCmSMIPath != "" {
		if s, err := w.pollROCmSMI(ctx); err != nil {
			w.logOnce("rocm-smi-failed", slog.LevelWarn, "rocm-smi health probe failed", "err", err)
		} else {
			samples, source, haveSource = s, SourceROCmSMI, true
		}
	}
	if !haveSource {
		w.logOnce("no-source", slog.LevelWarn,
			"AMD detection source has no usable amd-smi or rocm-smi; observing only")
		return nil
	}
	return w.filter(w.candidates(samples, source))
}

// candidates turns parsed samples into observation series, and is where the
// refusals live: a sample that does not carry a field yields no candidate for
// it, so an unrecognized tool version produces silence rather than zeros that a
// later poll would misread as a fault.
func (w *Watcher) candidates(samples []sample, source string) []candidate {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.initLocked()

	if len(samples) == 0 {
		if len(w.lastKnown) > 0 {
			// Zero devices where devices used to be is a statement about the
			// TOOL, not about any one GPU: a wedged driver, a container that
			// lost /dev/kfd, a permissions change. Attributing per-GPU loss
			// here would open an incident on every accelerator in the node at
			// once, so refuse and make the refusal visible.
			w.logOnceLocked("empty-inventory", slog.LevelWarn,
				"AMD tool reported no GPUs where some were previously present; refusing to attribute per-GPU loss",
				"source", source, "previously_known", len(w.lastKnown))
		}
		return nil
	}

	var out []candidate
	present := map[int]bool{}
	for _, s := range samples {
		present[s.index] = true
		w.forgetReplacedDeviceLocked(s)
		w.lastKnown[s.index] = deviceIdentity{uuid: s.uuid, bdf: s.bdf, source: source}
		out = append(out, w.deviceCandidates(s, source)...)
		// A present device clears its own loss series, so a device that
		// disappears again later reports again instead of being suppressed as
		// an unchanged level value.
		out = append(out, candidate{
			index: s.index, uuid: s.uuid, bdf: s.bdf, code: CodeGPULost, source: source,
			key: seriesKey(CodeGPULost, s.index), value: 0, level: true,
		})
	}
	// Losses are reported in index order so a multi-GPU failure produces a
	// deterministic event sequence, which the tests can pin.
	var lost []int
	for index := range w.lastKnown {
		if !present[index] {
			lost = append(lost, index)
		}
	}
	sort.Ints(lost)
	for _, index := range lost {
		id := w.lastKnown[index]
		if id.source != source {
			// The device was last seen by the OTHER tool. amd-smi and rocm-smi
			// need not enumerate the same devices or agree on indices, so an
			// absence across a tool switch is evidence about the tools, not
			// about the hardware. Presence is only ever compared within one
			// tool's own view.
			w.logOnceLocked("cross-tool-presence", slog.LevelInfo,
				"AMD device was last seen by a different tool; not treating its absence as a lost GPU",
				"gpu", index, "last_seen_source", id.source, "current_source", source)
			continue
		}
		out = append(out, candidate{
			index: index, uuid: id.uuid, bdf: id.bdf, code: CodeGPULost, source: source,
			attrs: map[string]string{"last_known_bdf": id.bdf},
			raw: fmt.Sprintf("%s: GPU %d (%s) is no longer reported by the tool that previously listed it",
				source, index, identityText(id)),
			key: seriesKey(CodeGPULost, index), value: 1, level: true,
		})
	}
	return out
}

// deviceCandidates builds the per-device series from one sample. Called with mu
// held (it consults the thermal-refusal log-once set).
func (w *Watcher) deviceCandidates(s sample, source string) []candidate {
	var out []candidate
	add := func(code string, value uint64, minDelta uint64, attrs map[string]string, raw string) {
		out = append(out, candidate{
			index: s.index, uuid: s.uuid, bdf: s.bdf, code: code, source: source,
			attrs: attrs, raw: raw, key: seriesKey(code, s.index), value: value, minDelta: minDelta,
		})
	}
	if s.eccUncorrectable != nil {
		add(CodeECCUncorr, *s.eccUncorrectable, 0,
			map[string]string{"uncorrectable_ecc_errors": strconv.FormatUint(*s.eccUncorrectable, 10)},
			fmt.Sprintf("%s: GPU %d uncorrectable ECC errors=%d", source, s.index, *s.eccUncorrectable))
	}
	if s.eccCorrectable != nil {
		add(CodeECCCorrRate, *s.eccCorrectable, w.correctableMinDeltaLocked(),
			map[string]string{"correctable_ecc_errors": strconv.FormatUint(*s.eccCorrectable, 10)},
			fmt.Sprintf("%s: GPU %d correctable ECC errors=%d", source, s.index, *s.eccCorrectable))
	}
	if s.retiredPages != nil {
		add(CodePageRetire, *s.retiredPages, 0,
			map[string]string{"retired_pages": strconv.FormatUint(*s.retiredPages, 10)},
			fmt.Sprintf("%s: GPU %d retired page count=%d", source, s.index, *s.retiredPages))
		if w.BadPageThreshold > 0 {
			// A LEVEL series, not a counter. "Out of spare pages" is a
			// condition the device is in right now, so it must survive the
			// startup baseline that (correctly) swallows the retirement
			// history: a GPU already past its budget when the agent starts is
			// still a GPU that needs replacing.
			exhausted := uint64(0)
			if *s.retiredPages >= w.BadPageThreshold {
				exhausted = 1
			}
			out = append(out, candidate{
				index: s.index, uuid: s.uuid, bdf: s.bdf, code: CodePageRetireExhausted,
				source: source, level: true,
				key: seriesKey(CodePageRetireExhausted, s.index), value: exhausted,
				attrs: map[string]string{
					"retired_pages": strconv.FormatUint(*s.retiredPages, 10),
					"threshold":     strconv.FormatUint(w.BadPageThreshold, 10),
				},
				raw: fmt.Sprintf("%s: GPU %d retired page count=%d at or above the configured bad-page threshold %d",
					source, s.index, *s.retiredPages, w.BadPageThreshold),
			})
		}
	}
	if s.xgmiErrors != nil {
		add(CodeXGMILink, *s.xgmiErrors, w.xgmiMinDeltaLocked(),
			map[string]string{"xgmi_link_errors": strconv.FormatUint(*s.xgmiErrors, 10)},
			fmt.Sprintf("%s: GPU %d XGMI link errors=%d", source, s.index, *s.xgmiErrors))
	}
	if c := w.thermalCandidateLocked(s, source); c != nil {
		out = append(out, *c)
	}
	return out
}

// thermalCandidateLocked decides whether this sample is a thermal fault. The
// tool's own throttle flag is authoritative when present. A raw temperature is
// only a fault against an operator-configured critical threshold: the number at
// which a given SKU is in trouble is not knowable from the reading alone, and
// picking one here would manufacture faults on hardware nobody has tested.
// Called with mu held.
func (w *Watcher) thermalCandidateLocked(s sample, source string) *candidate {
	active := false
	var raw string
	attrs := map[string]string{}
	switch {
	case s.thermalThrottled != nil:
		active = *s.thermalThrottled
		attrs["throttle_status"] = s.thermalThrottleStatus
		raw = fmt.Sprintf("%s: GPU %d thermal throttle status=%q", source, s.index, s.thermalThrottleStatus)
		if s.hotspotC != nil {
			attrs["hotspot_c"] = strconv.FormatFloat(*s.hotspotC, 'f', -1, 64)
		}
	case s.hotspotC != nil && w.ThermalCriticalC > 0:
		active = *s.hotspotC >= w.ThermalCriticalC
		attrs["hotspot_c"] = strconv.FormatFloat(*s.hotspotC, 'f', -1, 64)
		attrs["threshold_c"] = strconv.FormatFloat(w.ThermalCriticalC, 'f', -1, 64)
		raw = fmt.Sprintf("%s: GPU %d hotspot %.1fC at or above the configured critical %.1fC",
			source, s.index, *s.hotspotC, w.ThermalCriticalC)
	case s.hotspotC != nil:
		w.logOnceLocked("thermal-unthresholded", slog.LevelInfo,
			"AMD hotspot temperature observed but no throttle flag and no thermal critical threshold configured; observing only",
			"source", source)
		return nil
	default:
		return nil
	}
	value := uint64(0)
	if active {
		value = 1
	}
	return &candidate{
		index: s.index, uuid: s.uuid, bdf: s.bdf, code: CodeThermal, source: source,
		attrs: attrs, raw: raw, key: seriesKey(CodeThermal, s.index), value: value, level: true,
	}
}

// filter applies the per-series baseline/level rules and builds the events.
func (w *Watcher) filter(candidates []candidate) []types.AgentEvent {
	w.mu.Lock()
	w.initLocked()
	var emit []candidate
	for _, c := range candidates {
		last, ok := w.seen[c.key]
		w.seen[c.key] = c.value
		if c.level {
			// A state series reports on entry into the faulted value and stays
			// quiet while it persists, so a GPU that stays lost does not
			// re-open an incident on every tick. Unlike a counter it is NOT
			// baselined on the first poll: a throttle that is active right now
			// is a current condition, not an aggregate of unknown age, and
			// swallowing it at startup would hide a fault that is happening.
			if c.value != 0 && (!ok || c.value != last) {
				emit = append(emit, c)
			}
			continue
		}
		// A counter's first observation establishes the baseline: the value is
		// a lifetime aggregate of unknown age, and replaying it at startup
		// would open incidents for faults remediated months ago.
		if !w.baselined[c.key] {
			w.baselined[c.key] = true
			w.emittedAt[c.key] = c.value
			continue
		}
		if c.value < last {
			// A counter that went backwards was reset (reboot, tool restart).
			// Re-baseline silently: the alternative is treating the next tick's
			// normal value as an enormous increase.
			w.emittedAt[c.key] = c.value
			continue
		}
		reported := w.emittedAt[c.key]
		if c.value <= reported {
			continue
		}
		required := c.minDelta
		if required == 0 {
			required = 1
		}
		if c.value-reported < required {
			// Below the reporting delta: real, remembered, and still
			// accumulating toward the next report rather than discarded.
			continue
		}
		emit = append(emit, c)
		w.emittedAt[c.key] = c.value
	}
	w.mu.Unlock()

	now := time.Now()
	out := make([]types.AgentEvent, 0, len(emit))
	for _, c := range emit {
		attrs := map[string]string{}
		for k, v := range c.attrs {
			if v != "" {
				attrs[k] = v
			}
		}
		out = append(out, types.AgentEvent{
			Node:     w.NodeName,
			GPUIndex: c.index,
			GPUUUID:  c.uuid,
			PCIAddr:  c.bdf,
			// XID stays zero: nothing here is an NVIDIA XID, and the ingest
			// path rejects an event that claims both identities.
			Fault: &types.FaultSignal{
				Vendor:     FaultVendor,
				Source:     c.source,
				Code:       c.code,
				Attributes: attrs,
			},
			Raw:       c.raw,
			Timestamp: now,
		})
	}
	return out
}

func (w *Watcher) initLocked() {
	if w.seen == nil {
		w.seen = map[string]uint64{}
		w.baselined = map[string]bool{}
		w.emittedAt = map[string]uint64{}
		w.lastKnown = map[int]deviceIdentity{}
		w.loggedOnce = map[string]bool{}
	}
}

func (w *Watcher) correctableMinDeltaLocked() uint64 {
	if w.CorrectableRateMinDelta > 0 {
		return w.CorrectableRateMinDelta
	}
	return defaultCorrectableRateMinDelta
}

func (w *Watcher) xgmiMinDeltaLocked() uint64 {
	if w.XGMILinkMinDelta > 0 {
		return w.XGMILinkMinDelta
	}
	return defaultXGMILinkMinDelta
}

// logOnce emits a persistent condition's log line exactly once per watcher.
// Every refusal in this package is logged: a source that silently declines to
// report is indistinguishable from a healthy fleet, which is the failure this
// project cares most about.
func (w *Watcher) logOnce(key string, level slog.Level, msg string, args ...any) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.initLocked()
	w.logOnceLocked(key, level, msg, args...)
}

// logOnceLocked is logOnce with mu already held.
func (w *Watcher) logOnceLocked(key string, level slog.Level, msg string, args ...any) {
	if w.Logger == nil || w.loggedOnce[key] {
		return
	}
	w.loggedOnce[key] = true
	w.Logger.Log(context.Background(), level, msg, args...)
}

func seriesKey(code string, index int) string {
	return code + "|" + strconv.Itoa(index)
}

// codesWithSeries lists every code that keeps per-index state, so replacing a
// device can clear all of it. Missing an entry here leaves one counter series
// attributed to hardware that is no longer in the machine.
var codesWithSeries = []string{
	CodeECCUncorr, CodeECCCorrRate, CodePageRetire, CodePageRetireExhausted,
	CodeXGMILink, CodeThermal, CodeGPULost,
}

// forgetReplacedDeviceLocked drops every series belonging to an index whose
// device has been physically swapped.
//
// Series are keyed by GPU index rather than UUID on purpose: the rocm-smi
// fallback reports no UUID at all, so a UUID-keyed series would re-baseline —
// and hide a real increase — every time amd-smi failed over. The cost of index
// keying is that an RMA'd GPU inherits its predecessor's counter history: the
// replacement's lifetime totals are compared against a dead device's, which
// either replays its whole history as one enormous delta or silently absorbs a
// real fault as a "counter went backwards" reset.
//
// So identity is checked rather than assumed. Only a CHANGE between two
// non-empty UUIDs counts: an absent UUID is the fallback tool being narrow, not
// evidence that the hardware changed.
func (w *Watcher) forgetReplacedDeviceLocked(s sample) {
	prev, ok := w.lastKnown[s.index]
	if !ok || prev.uuid == "" || s.uuid == "" || prev.uuid == s.uuid {
		return
	}
	for _, code := range codesWithSeries {
		key := seriesKey(code, s.index)
		delete(w.seen, key)
		delete(w.baselined, key)
		delete(w.emittedAt, key)
	}
	if w.Logger != nil {
		w.Logger.Info("AMD GPU index now reports a different device; starting its counter series over",
			"index", s.index, "previous_uuid", prev.uuid, "uuid", s.uuid)
	}
}

// identityText names a device for an operator without pretending to an identity
// the tool never reported.
func identityText(id deviceIdentity) string {
	switch {
	case id.uuid != "" && id.bdf != "":
		return id.uuid + " at " + id.bdf
	case id.uuid != "":
		return id.uuid
	case id.bdf != "":
		return id.bdf
	default:
		return "no reported identity"
	}
}

func firstLine(out []byte) string {
	line, _, _ := strings.Cut(strings.TrimSpace(string(out)), "\n")
	return line
}
