// Package gpuhealth is a second GPU-fault detection source beside the kmsg
// XID watcher. It periodically polls DCGM through the mounted dcgmi binary
// (preferred) and falls back to nvidia-smi -q where dcgmi is absent — dcgmi is
// frequently missing on the stock EKS NVIDIA AMI, so its absence must degrade
// gracefully to observed-only, never crash.
//
// It is a DETECTION/OBSERVATION source only. It resolves faults into the same
// types.AgentEvent the kmsg path produces and never touches the executor or
// any destructive path. It also never fabricates events: DCGM's last-XID field
// is emitted level-triggered (on first sight and on change) so a fault the
// agent missed while down is recovered, while the nvidia-smi fallback's
// monotonic ECC/remap counters are baselined on the first poll and only
// re-emitted on a genuine increase.
//
// DCGM_FI_DEV_XID_ERRORS retains the last XID for the life of nv-hostengine, so
// "first sight" is made durable and boot-scoped (StatePath, mirroring the kmsg
// cursor): a (GPU, code) already emitted this boot is not re-emitted after a
// routine agent restart, which would otherwise re-open incidents for
// long-remediated faults on every DaemonSet rollout. A reboot clears the
// retained field and resets the record. When no durable record is available the
// source fails safe by baselining on first sight rather than re-emitting.
package gpuhealth

import (
	"context"
	"fmt"
	"log/slog"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/kubeneuron/kubeneuron/internal/metrics"
	"github.com/kubeneuron/kubeneuron/pkg/types"
)

const (
	// DefaultPollInterval is how often the source samples DCGM / nvidia-smi.
	// It is the slow path beside kmsg: kmsg reaches the controller seconds
	// after the kernel prints an XID, and this exists to catch what kmsg loses.
	DefaultPollInterval = 30 * time.Second
	// probeTimeout bounds one dcgmi / nvidia-smi invocation so a wedged driver
	// cannot stall the poll loop indefinitely.
	probeTimeout = 15 * time.Second
	// dcgmXIDFieldID is DCGM_FI_DEV_XID_ERRORS: the last XID error code the
	// host engine recorded for a GPU. dcgmi dmon reports it under the
	// "XIDERRORS" column.
	dcgmXIDFieldID = "230"
	// The nvidia-smi -q fallback's counter deltas are NOT XIDs, so they are no
	// longer dressed up as XID 48/64. They are emitted as vendor-neutral NVIDIA
	// faults through types.FaultSignal, and the detector maps (vendor, code) to
	// the same ProblemClass the old synthesized XID produced: "ecc-dbe" ->
	// ClassECCDBE (was XID 48), "row-remap-failure" -> ClassRowRemapFailure
	// (was XID 64). DCGM's last-XID field is a genuine XID and keeps using XID.
	faultVendorNVIDIA   = "nvidia"
	faultSourceSMI      = "nvidia-smi"
	codeECCDBE          = "ecc-dbe"
	codeRowRemapFailure = "row-remap-failure"
)

// runner abstracts subprocess execution for tests, matching the nvml/dcgm
// packages.
type runner func(ctx context.Context, name string, args ...string) ([]byte, error)

// Watcher polls a GPU health source and emits normalized XID events.
type Watcher struct {
	// NodeName stamps every emitted event.
	NodeName string
	// DCGMPath is the dcgmi binary; empty disables the DCGM path.
	DCGMPath string
	// DCGMEndpoint is this node's DCGM host engine, e.g. "10.0.1.7:5555".
	// Empty uses dcgmi's local default. As elsewhere, it must name this node.
	DCGMEndpoint string
	// NVIDIASMIPath is the nvidia-smi binary for the fallback path; empty
	// disables it.
	NVIDIASMIPath string
	// PollInterval defaults to DefaultPollInterval.
	PollInterval time.Duration
	// ResolveGPU maps a DCGM GPU index to inventory (for the UUID). It is
	// optional: a nil resolver, or a miss, leaves GPUUUID empty and keeps the
	// trustworthy index. nvidia-smi -q already carries the UUID inline.
	ResolveGPU func(ctx context.Context, index int) (types.GPUInfo, bool)
	// Logger records source selection and degradation; nil disables logging.
	Logger *slog.Logger

	// StatePath, when set, is a durable file recording which level-triggered
	// (GPU, code) observations have already been emitted this boot. It stops a
	// routine agent restart from re-emitting DCGM's retained last-XID and
	// re-opening incidents for long-remediated faults. Empty keeps the in-memory,
	// emit-on-first-sight behavior (used by unit tests and CPU-only installs).
	StatePath string
	// BootID binds StatePath to the current node boot. A boot-ID mismatch resets
	// the record, since a reboot clears nv-hostengine's retained XID field too.
	BootID string

	run runner

	mu        sync.Mutex
	seen      map[string]uint64
	baselined map[string]bool
	// stateLoaded guards the one-time load of the durable emitted-XID record.
	stateLoaded bool
	// levelState is the durable set of emitted (or baselined) level-series
	// values, persisted to StatePath. nil until the first poll in durable mode.
	levelState map[string]uint64
	// baselineFirstSight is set in durable mode when no usable record loaded
	// (missing/unreadable/cross-boot): a level series ALREADY nonzero at process
	// start is baselined on its first sight rather than re-emitted (unknown age),
	// the fail-safe posture against spamming.
	baselineFirstSight bool
	// firstPollDone marks that the first poll has completed. First-sight baselining
	// applies ONLY during that first poll: a series that was absent/zero at start
	// and becomes nonzero LATER is a genuine new fault and must emit, so the
	// suppression must not persist for the whole process lifetime.
	firstPollDone bool
	// persistFailLogged ensures a state-save failure is warned once, not per poll.
	persistFailLogged bool
	// degradedLogged ensures the "no usable health source" warning is emitted
	// once, not every poll.
	degradedLogged bool
	// unreadableDCGMLogged ensures an unparseable dmon layout is warned about
	// once. A layout this build cannot read does not change between polls, so
	// repeating it every tick would bury it in the noise it is warning about.
	unreadableDCGMLogged bool
}

// New builds a health watcher, auto-detecting which binaries are present. A
// binary that is not on the node is left disabled rather than treated as an
// error, so a CPU-only or dcgmi-less install degrades to observed-only.
func New(nodeName, dcgmPath, dcgmEndpoint, nvidiaSMIPath string) *Watcher {
	w := &Watcher{
		NodeName:     nodeName,
		DCGMEndpoint: strings.TrimSpace(dcgmEndpoint),
		PollInterval: DefaultPollInterval,
		run: func(ctx context.Context, name string, args ...string) ([]byte, error) {
			return exec.CommandContext(ctx, name, args...).CombinedOutput()
		},
	}
	if p := strings.TrimSpace(dcgmPath); p != "" {
		if _, err := exec.LookPath(p); err == nil {
			w.DCGMPath = p
		}
	}
	if p := strings.TrimSpace(nvidiaSMIPath); p != "" {
		if _, err := exec.LookPath(p); err == nil {
			w.NVIDIASMIPath = p
		}
	}
	return w
}

// Watch polls until ctx is done and streams normalized XID events. It never
// returns an error: an unusable source degrades to an empty, observed-only
// stream rather than failing the agent's detection wiring.
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
		// Poll once immediately: DCGM retains the last XID, so the first poll
		// after a restart is exactly when a missed fault is recovered.
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
	// xid is the genuine XID for the DCGM last-XID path; zero for a neutral
	// fault. fault is the vendor-neutral descriptor for the nvidia-smi
	// ECC/row-remap path; nil for an XID. Exactly one is set.
	xid   int
	fault *types.FaultSignal
	raw   string
	// key identifies the observation series for de-duplication over time.
	key string
	// value is the current series value; interpretation depends on level.
	value uint64
	// level marks a level-triggered series (DCGM last-XID): emit on first
	// sight and on any change to a nonzero. A non-level series is a monotonic
	// counter: baseline on first sight, emit only on an increase.
	level bool
}

// poll runs one probe and returns the events that survive filtering. DCGM is
// preferred; a DCGM failure (engine down, transient) falls back to nvidia-smi
// for this poll and is retried next tick.
func (w *Watcher) poll(ctx context.Context) []types.AgentEvent {
	var (
		candidates []candidate
		haveSource bool
	)
	source := "none"
	if w.DCGMPath != "" {
		if cs, err := w.pollDCGM(ctx); err != nil {
			if w.Logger != nil {
				w.Logger.Debug("DCGM health probe failed, falling back to nvidia-smi", "err", err)
			}
		} else {
			candidates = cs
			haveSource = true
			source = "dcgm"
		}
	}
	if !haveSource && w.NVIDIASMIPath != "" {
		if cs, err := w.pollSMI(ctx); err != nil {
			if w.Logger != nil {
				w.Logger.Debug("nvidia-smi health probe failed", "err", err)
			}
		} else {
			candidates = cs
			haveSource = true
			source = "nvidia-smi"
		}
	}
	// Publish which source served this poll. DCGM is preferred and nvidia-smi
	// is narrower, so silently degrading between them changes what this agent
	// can detect — an operator and the hardware harness both need to see it.
	for _, s := range []string{"dcgm", "nvidia-smi", "none"} {
		v := 0.0
		if s == source {
			v = 1
		}
		metrics.AgentHealthSource.WithLabelValues(s).Set(v)
	}
	if !haveSource {
		w.mu.Lock()
		if !w.degradedLogged && w.Logger != nil {
			w.Logger.Warn("GPU health second source has no usable dcgmi or nvidia-smi; observing only")
			w.degradedLogged = true
		}
		w.mu.Unlock()
		return nil
	}
	return w.filter(ctx, candidates)
}

// filter applies the per-series level/edge rules and resolves UUIDs.
func (w *Watcher) filter(ctx context.Context, candidates []candidate) []types.AgentEvent {
	w.mu.Lock()
	w.loadStateLocked()
	// First-sight baselining is scoped to the first poll only. A series absent or
	// zero at process start that later develops a fault (GPU clean at startup, XID
	// 79 two hours in) must EMIT, not be swallowed as "first sight".
	isFirstPoll := !w.firstPollDone
	w.firstPollDone = true
	var emit []candidate
	persistDirty := false
	for _, c := range candidates {
		last, ok := w.seen[c.key]
		w.seen[c.key] = c.value
		if c.level {
			// Durable mode with no usable record, and this is the FIRST poll: a
			// level series already nonzero at process start has an unknown age, so
			// baseline it rather than re-emit a possibly long-remediated retained
			// XID. Persist the baseline so a later restart suppresses it too. On
			// any later poll this branch is skipped, so an absent/zero->nonzero
			// transition falls through to the emit rule below.
			if w.baselineFirstSight && isFirstPoll && !w.baselined[c.key] {
				w.baselined[c.key] = true
				if w.recordLevelLocked(c) {
					persistDirty = true
				}
				continue
			}
			// Level-triggered: emit a nonzero value on first sight and whenever
			// it changes. This is what recovers a fault missed during downtime.
			// In durable mode a value already emitted-and-persisted this boot was
			// seeded into seen, so an unchanged retained XID is suppressed across
			// restarts instead of re-opening an incident.
			if c.value != 0 && (!ok || c.value != last) {
				emit = append(emit, c)
				if w.recordLevelLocked(c) {
					persistDirty = true
				}
			}
			continue
		}
		// Counter: the first observation only establishes the baseline, so a
		// large historical aggregate is never replayed on startup. A decrease
		// (a volatile counter reset by reboot/reset) re-baselines silently.
		if !w.baselined[c.key] {
			w.baselined[c.key] = true
			continue
		}
		if c.value > last {
			emit = append(emit, c)
		}
	}
	var snapshot map[string]uint64
	if persistDirty {
		snapshot = make(map[string]uint64, len(w.levelState))
		for k, v := range w.levelState {
			snapshot[k] = v
		}
	}
	w.mu.Unlock()

	if snapshot != nil {
		w.persistState(snapshot)
	}

	out := make([]types.AgentEvent, 0, len(emit))
	for _, c := range emit {
		uuid := c.uuid
		if uuid == "" && w.ResolveGPU != nil {
			if gpu, ok := w.ResolveGPU(ctx, c.index); ok {
				uuid = gpu.UUID
			}
		}
		out = append(out, types.AgentEvent{
			Node:      w.NodeName,
			GPUIndex:  c.index,
			GPUUUID:   uuid,
			XID:       c.xid,
			Fault:     c.fault,
			Raw:       c.raw,
			Timestamp: time.Now(),
		})
	}
	return out
}

// loadStateLocked initializes the in-memory maps and, in durable mode, seeds
// them from the persisted emitted-XID record exactly once. Called with mu held.
func (w *Watcher) loadStateLocked() {
	if w.stateLoaded {
		return
	}
	w.stateLoaded = true
	if w.seen == nil {
		w.seen = map[string]uint64{}
		w.baselined = map[string]bool{}
	}
	if w.StatePath == "" {
		// Persistence disabled: keep the in-memory emit-on-first-sight behavior.
		return
	}
	w.levelState = map[string]uint64{}
	rec, ok, err := loadHealthState(w.StatePath, w.BootID)
	if err != nil && w.Logger != nil {
		w.Logger.Warn("gpuhealth durable state unreadable; baselining to avoid re-emitting retained XIDs", "err", err)
	}
	if ok {
		// A usable record: treat every persisted (GPU, code) as already emitted
		// this boot so an unchanged retained XID is suppressed, while a genuinely
		// new code (a key/value not in the record) still emits.
		for k, v := range rec {
			w.seen[k] = v
			w.baselined[k] = true
			w.levelState[k] = v
		}
		return
	}
	// No usable record (missing/unreadable/cross-boot): baseline level series on
	// first sight so a retained XID is not re-emitted after a restart or reboot.
	w.baselineFirstSight = true
}

// recordLevelLocked notes a level series' current value in the durable set,
// reporting whether it changed (and so needs persisting). Called with mu held.
func (w *Watcher) recordLevelLocked(c candidate) bool {
	if w.levelState == nil {
		return false
	}
	if v, ok := w.levelState[c.key]; ok && v == c.value {
		return false
	}
	w.levelState[c.key] = c.value
	return true
}

// persistState durably writes the emitted-XID snapshot. A failure is warned
// once and never crashes the poll loop: the worst case is a re-emit on the next
// restart, which the controller still deduplicates on EventID.
func (w *Watcher) persistState(snapshot map[string]uint64) {
	if err := saveHealthState(w.StatePath, w.BootID, snapshot); err != nil {
		w.mu.Lock()
		if !w.persistFailLogged && w.Logger != nil {
			w.Logger.Warn("persisting gpuhealth durable state failed; a restart may re-emit a retained XID", "err", err)
			w.persistFailLogged = true
		}
		w.mu.Unlock()
	}
}

func (w *Watcher) pollDCGM(ctx context.Context) ([]candidate, error) {
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	args := []string{"dmon"}
	if w.DCGMEndpoint != "" {
		// dcgmi rejects --host before the subcommand (see internal/agent/dcgm):
		// it must follow "dmon".
		args = append(args, "--host", w.DCGMEndpoint)
	}
	args = append(args, "-e", dcgmXIDFieldID, "-c", "1")
	out, err := w.run(ctx, w.DCGMPath, args...)
	if err != nil {
		return nil, fmt.Errorf("dcgmi dmon: %w (output: %s)", err, firstLine(out))
	}
	candidates, unreadable, err := parseDCGMXID(string(out))
	if err != nil {
		return nil, err
	}
	if unreadable > 0 {
		w.mu.Lock()
		first := !w.unreadableDCGMLogged
		w.unreadableDCGMLogged = true
		w.mu.Unlock()
		if first && w.Logger != nil {
			w.Logger.Warn("dcgmi dmon printed rows this build cannot parse; "+
				"the DCGM detection source is degraded and may be reporting nothing",
				"unreadable_rows", unreadable, "first_line", firstLine(out))
		}
	}
	return candidates, nil
}

// dcgmRowRe matches a dmon data row: "GPU <index> <value>". The value column is
// the last XID error code, "N/A" when the engine has recorded none.
var dcgmRowRe = regexp.MustCompile(`^GPU\s+(\d+)\s+(\S+)`)

// parseDCGMXID reads `dcgmi dmon` output. The second return value is how many
// non-comment lines it could NOT read as a data row.
//
// That count is the whole point of the signature. Every unreadable line used
// to be silently skipped, so a DCGM release that changed the dmon layout would
// have produced zero candidates forever — a detection source going dark with
// nothing anywhere saying so, on a fleet that believes it is being watched. It
// also made the hardware harness's fallback assertion ("no gpuhealth parse
// warnings") vacuous: there was no code path that could emit one, so the phase
// passed without proving anything.
//
// Silence is still the behaviour for the FAULT path — an unreadable line must
// never become a fault. It is only the reporting that changes.
func parseDCGMXID(out string) ([]candidate, int, error) {
	var candidates []candidate
	unreadable := 0
	for _, line := range strings.Split(out, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		m := dcgmRowRe.FindStringSubmatch(trimmed)
		if m == nil {
			// dmon's header is TWO lines — "#Entity   XIDERRORS" then a bare
			// "Id" continuation — and only the first carries the comment
			// marker. A label line is the one thing here that is not a data
			// row and not a defect, and it is reliably digit-free: every real
			// row names an entity index. Counting the continuation as
			// unreadable would make this warn on every healthy fleet, which is
			// how a signal gets ignored.
			if strings.ContainsAny(trimmed, "0123456789") {
				unreadable++
			}
			continue
		}
		index, err := strconv.Atoi(m[1])
		if err != nil {
			unreadable++
			continue
		}
		value := strings.TrimSpace(m[2])
		if strings.EqualFold(value, "N/A") {
			// The engine has recorded no XID for this GPU. A health report,
			// not an unreadable line.
			continue
		}
		xid, err := strconv.Atoi(value)
		if err != nil {
			unreadable++
			continue
		}
		if xid <= 0 {
			continue
		}
		candidates = append(candidates, candidate{
			index: index,
			xid:   xid,
			raw:   fmt.Sprintf("DCGM DCGM_FI_DEV_XID_ERRORS: GPU %d reported XID %d", index, xid),
			key:   fmt.Sprintf("dcgm-xid-%d", index),
			value: uint64(xid),
			level: true,
		})
	}
	return candidates, unreadable, nil
}

func (w *Watcher) pollSMI(ctx context.Context) ([]candidate, error) {
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	out, err := w.run(ctx, w.NVIDIASMIPath, "-q")
	if err != nil {
		return nil, fmt.Errorf("nvidia-smi -q: %w (output: %s)", err, firstLine(out))
	}
	return parseSMIHealth(string(out)), nil
}

var (
	smiGPUHeaderRe = regexp.MustCompile(`^GPU\s+[0-9A-Fa-f]{4,8}:[0-9A-Fa-f]{2}:[0-9A-Fa-f]{2}\.[0-9A-Fa-f]+\s*$`)
	smiUUIDRe      = regexp.MustCompile(`(?i)^\s*GPU UUID\s*:\s*(\S+)`)
	smiKVRe        = regexp.MustCompile(`^\s*([^:]+?)\s*:\s*(\S.*?)\s*$`)
)

// parseSMIHealth extracts, per GPU block in nvidia-smi -q, a fresh-fault
// counter for uncorrectable ECC (volatile) and for row-remap failure. It is a
// deliberately narrow reading of a large, driver-version-dependent report:
// only these two well-understood, XID-mappable conditions are surfaced.
func parseSMIHealth(out string) []candidate {
	type block struct {
		index    int
		uuid     string
		ecc      uint64
		remap    uint64
		hasECC   bool
		hasRemap bool
	}
	var blocks []block
	var cur *block
	index := -1

	// Section tracking within a block: uncorrectable ECC totals are read only
	// from the Volatile subsection (resets on reboot => a fresh fault), never
	// the lifetime Aggregate counters.
	inECC, inVolatile, inRemap := false, false, false

	flush := func() {
		if cur != nil {
			blocks = append(blocks, *cur)
		}
	}
	for _, line := range strings.Split(out, "\n") {
		if smiGPUHeaderRe.MatchString(strings.TrimRight(line, " \t\r")) {
			flush()
			index++
			b := block{index: index}
			cur = &b
			inECC, inVolatile, inRemap = false, false, false
			continue
		}
		if cur == nil {
			continue
		}
		if m := smiUUIDRe.FindStringSubmatch(line); m != nil {
			cur.uuid = m[1]
			continue
		}
		trimmed := strings.TrimSpace(line)
		lower := strings.ToLower(trimmed)
		switch {
		case strings.HasPrefix(lower, "ecc errors"):
			inECC, inVolatile, inRemap = true, false, false
			continue
		case strings.HasPrefix(lower, "remapped rows"):
			inECC, inVolatile, inRemap = false, false, true
			continue
		case inECC && lower == "volatile":
			inVolatile = true
			continue
		case inECC && lower == "aggregate":
			inVolatile = false
			continue
		}
		m := smiKVRe.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		label := strings.ToLower(strings.TrimSpace(m[1]))
		value := strings.TrimSpace(m[2])
		switch {
		case inECC && inVolatile && (strings.Contains(label, "uncorrectable") || strings.Contains(label, "double bit")):
			if n, ok := parseCount(value); ok {
				cur.ecc += n
				cur.hasECC = true
			}
		case inRemap && strings.Contains(label, "failure occurred"):
			cur.hasRemap = true
			if strings.EqualFold(value, "yes") {
				cur.remap = 1
			}
		}
	}
	flush()

	var candidates []candidate
	for _, b := range blocks {
		if b.hasECC {
			candidates = append(candidates, candidate{
				index: b.index,
				uuid:  b.uuid,
				fault: &types.FaultSignal{
					Vendor: faultVendorNVIDIA,
					Source: faultSourceSMI,
					Code:   codeECCDBE,
					Attributes: map[string]string{
						"volatile_uncorrectable_ecc": strconv.FormatUint(b.ecc, 10),
					},
				},
				raw:   fmt.Sprintf("nvidia-smi -q: GPU %d volatile uncorrectable ECC errors=%d", b.index, b.ecc),
				key:   fmt.Sprintf("smi-ecc-%d", b.index),
				value: b.ecc,
			})
		}
		if b.hasRemap {
			candidates = append(candidates, candidate{
				index: b.index,
				uuid:  b.uuid,
				fault: &types.FaultSignal{
					Vendor:     faultVendorNVIDIA,
					Source:     faultSourceSMI,
					Code:       codeRowRemapFailure,
					Attributes: map[string]string{"remap_failure": "yes"},
				},
				raw:   fmt.Sprintf("nvidia-smi -q: GPU %d row-remap failure occurred", b.index),
				key:   fmt.Sprintf("smi-remap-%d", b.index),
				value: b.remap,
			})
		}
	}
	return candidates
}

// parseCount reads an nvidia-smi count field, tolerating "N/A" and thousands
// separators. It returns ok=false for unparseable values so they are ignored
// rather than counted as zero (which a later poll could misread as a decrease).
func parseCount(value string) (uint64, bool) {
	v := strings.TrimSpace(value)
	if v == "" || strings.EqualFold(v, "N/A") {
		return 0, false
	}
	v = strings.ReplaceAll(v, ",", "")
	n, err := strconv.ParseUint(v, 10, 64)
	if err != nil {
		return 0, false
	}
	return n, true
}

func firstLine(out []byte) string {
	line, _, _ := strings.Cut(strings.TrimSpace(string(out)), "\n")
	return line
}
