package amdhealth

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"regexp"
	"strconv"
	"strings"
)

// sample is one GPU's health reading, normalized away from whichever tool
// produced it. Every metric is a POINTER because "absent" and "zero" must stay
// distinguishable: a field the tool did not report yields no fault series at
// all, while a reported zero is genuine evidence of health that anchors the
// counter baseline. Collapsing the two would let an unrecognized tool version
// look like a healthy GPU forever.
type sample struct {
	index int
	uuid  string
	bdf   string

	eccUncorrectable *uint64
	eccCorrectable   *uint64
	retiredPages     *uint64
	xgmiErrors       *uint64
	hotspotC         *float64
	// thermalThrottled is the tool's own verdict when it reports one; it
	// outranks any temperature comparison.
	thermalThrottled      *bool
	thermalThrottleStatus string
}

// amdSMIDevice is the subset of `amd-smi metric --json` this parser accepts.
//
// SYNTHETIC SCHEMA. These field names were reconstructed from amd-smi's
// documented vocabulary without a machine to check them against, so treat this
// struct as "the shape the parser will accept" rather than "the shape amd-smi
// emits". Unknown fields are ignored and missing fields simply produce no
// series, so a wrong guess costs coverage, never a false fault. Every pointer
// and every amdNumber below is optional for that reason.
type amdSMIDevice struct {
	GPU  *int   `json:"gpu"`
	BDF  string `json:"bdf"`
	UUID string `json:"uuid"`
	ECC  *struct {
		TotalCorrectableCount   amdNumber `json:"total_correctable_count"`
		TotalUncorrectableCount amdNumber `json:"total_uncorrectable_count"`
	} `json:"ecc"`
	RAS *struct {
		RetiredPageCount amdNumber `json:"retired_page_count"`
	} `json:"ras"`
	XGMI *struct {
		LinkErrorCount amdNumber `json:"link_error_count"`
	} `json:"xgmi"`
	Temperature *struct {
		Hotspot amdNumber `json:"hotspot"`
	} `json:"temperature"`
	Throttle *struct {
		ThermalThrottleStatus string `json:"thermal_throttle_status"`
	} `json:"throttle"`
}

// amdNumber reads the several shapes a numeric metric arrives in: a bare
// number, a quoted number, or the {"value": N, "unit": "C"} envelope amd-smi
// uses for physical quantities. "N/A", null, and anything unparseable are
// recorded as ABSENT rather than zero — an unreadable counter must not become a
// zero that the next poll reads as a large increase.
type amdNumber struct {
	set bool
	val float64
}

func (n *amdNumber) UnmarshalJSON(data []byte) error {
	trimmed := strings.TrimSpace(string(data))
	if trimmed == "" || trimmed == "null" {
		return nil
	}
	if trimmed[0] == '{' {
		var envelope struct {
			Value json.RawMessage `json:"value"`
		}
		if err := json.Unmarshal(data, &envelope); err != nil || len(envelope.Value) == 0 {
			return nil
		}
		return n.UnmarshalJSON(envelope.Value)
	}
	var quoted string
	if trimmed[0] == '"' {
		if err := json.Unmarshal(data, &quoted); err != nil {
			return nil
		}
	} else {
		quoted = trimmed
	}
	quoted = strings.TrimSpace(strings.TrimSuffix(quoted, "%"))
	f, err := strconv.ParseFloat(quoted, 64)
	if err != nil {
		return nil
	}
	n.set, n.val = true, f
	return nil
}

// count returns the value as a non-negative counter, or nil when it is absent
// or not a sane counter. A negative or non-finite reading is a parse the tool
// and this code disagree about, so it is discarded rather than coerced.
func (n amdNumber) count() *uint64 {
	if !n.set || n.val < 0 || math.IsNaN(n.val) || math.IsInf(n.val, 0) {
		return nil
	}
	v := uint64(n.val)
	return &v
}

func (n amdNumber) temperature() *float64 {
	if !n.set || math.IsNaN(n.val) || math.IsInf(n.val, 0) {
		return nil
	}
	v := n.val
	return &v
}

func (w *Watcher) pollAMDSMI(ctx context.Context) ([]sample, error) {
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	// --json is requested explicitly: the human-readable table is formatted for
	// eyes and reflows between releases, while a schema drift in the JSON shows
	// up as a missing field, which this parser turns into silence rather than a
	// misread number.
	out, err := w.run(ctx, w.AMDSMIPath, "metric", "--json")
	if err != nil {
		return nil, fmt.Errorf("amd-smi metric --json: %w (output: %s)", err, firstLine(out))
	}
	return w.parseAMDSMI(out)
}

// parseAMDSMI converts amd-smi JSON into samples. A device record without a GPU
// index is dropped with a log line: every downstream identity (the dedup key,
// the incident target) is per device, so an unindexed record could only be
// attributed by guessing.
func (w *Watcher) parseAMDSMI(out []byte) ([]sample, error) {
	var devices []amdSMIDevice
	if err := json.Unmarshal(out, &devices); err != nil {
		return nil, fmt.Errorf("decode amd-smi JSON: %w", err)
	}
	var samples []sample
	for _, d := range devices {
		if d.GPU == nil {
			w.logOnce("amd-smi-unindexed", slog.LevelWarn,
				"amd-smi reported a device record without a gpu index; refusing to attribute its metrics")
			continue
		}
		s := sample{
			index: *d.GPU,
			uuid:  strings.TrimSpace(d.UUID),
			bdf:   normalizeBDF(d.BDF),
		}
		if d.ECC != nil {
			s.eccUncorrectable = d.ECC.TotalUncorrectableCount.count()
			s.eccCorrectable = d.ECC.TotalCorrectableCount.count()
			// A section that exists but whose counters cannot be read is the
			// most dangerous state this source can be in: it looks like a
			// healthy GPU forever. Name it, once, so the blindness is
			// discoverable without a hardware autopsy.
			w.warnUnreadableSection("ecc", s.index, s.eccUncorrectable == nil && s.eccCorrectable == nil)
		}
		if d.RAS != nil {
			s.retiredPages = d.RAS.RetiredPageCount.count()
			w.warnUnreadableSection("ras", s.index, s.retiredPages == nil)
		}
		if d.XGMI != nil {
			s.xgmiErrors = d.XGMI.LinkErrorCount.count()
			w.warnUnreadableSection("xgmi", s.index, s.xgmiErrors == nil)
		}
		if d.Temperature != nil {
			s.hotspotC = d.Temperature.Hotspot.temperature()
			w.warnUnreadableSection("temperature", s.index, s.hotspotC == nil)
		}
		if d.Throttle != nil {
			if throttled, ok := throttleStatus(d.Throttle.ThermalThrottleStatus); ok {
				s.thermalThrottled = &throttled
				s.thermalThrottleStatus = strings.TrimSpace(d.Throttle.ThermalThrottleStatus)
			} else if strings.TrimSpace(d.Throttle.ThermalThrottleStatus) != "" {
				// A status string this code does not recognize is exactly the
				// ambiguity that must not become a verdict in either direction.
				w.logOnce("throttle-unknown", slog.LevelWarn,
					"amd-smi reported an unrecognized thermal throttle status; ignoring it rather than guessing",
					"status", strings.TrimSpace(d.Throttle.ThermalThrottleStatus))
			}
		}
		if s.isEmpty() {
			w.logOnce("amd-smi-no-fields", slog.LevelWarn,
				"amd-smi output carried no field this parser recognizes; observing only (the fixtures are synthetic and may not match this amd-smi version)",
				"gpu", s.index)
		}
		samples = append(samples, s)
	}
	if len(samples) == 0 && len(devices) > 0 {
		return nil, fmt.Errorf("amd-smi reported %d devices, none of them attributable", len(devices))
	}
	return samples, nil
}

// warnUnreadableSection reports, once per section, that the tool printed a
// health section this parser could not read a value out of. It is the
// difference between "this GPU is fine" and "this source is blind to that
// GPU", which no metric downstream can distinguish on its own.
func (w *Watcher) warnUnreadableSection(section string, index int, unreadable bool) {
	if !unreadable {
		return
	}
	w.logOnce("unreadable-"+section, slog.LevelWarn,
		"AMD tool reported a health section whose values this parser could not read; that series is NOT monitored",
		"section", section, "gpu", index)
}

// isEmpty reports whether a sample carries no health field at all — a device
// this source can see but says nothing about.
func (s sample) isEmpty() bool {
	return s.eccUncorrectable == nil && s.eccCorrectable == nil && s.retiredPages == nil &&
		s.xgmiErrors == nil && s.hotspotC == nil && s.thermalThrottled == nil
}

// throttleStatus maps the throttle strings amd-smi is expected to print to a
// verdict. ok is false for anything else, so an unrecognized status neither
// raises nor clears a thermal fault.
func throttleStatus(status string) (throttled bool, ok bool) {
	switch strings.ToUpper(strings.TrimSpace(status)) {
	case "ACTIVE", "THROTTLED", "TRUE", "YES":
		return true, true
	case "INACTIVE", "NOT THROTTLED", "NONE", "FALSE", "NO", "N/A":
		return false, true
	default:
		return false, false
	}
}

func (w *Watcher) pollROCmSMI(ctx context.Context) ([]sample, error) {
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	out, err := w.run(ctx, w.ROCmSMIPath, "--showretiredpages", "--showtemp", "--json")
	if err != nil {
		return nil, fmt.Errorf("rocm-smi --showretiredpages --showtemp --json: %w (output: %s)", err, firstLine(out))
	}
	return w.parseROCmSMI(out)
}

var rocmCardRe = regexp.MustCompile(`^card(\d+)$`)

// parseROCmSMI reads the deliberately narrow rocm-smi fallback: retired pages
// and temperature only, matching the flags the poll passes. rocm-smi's JSON keys
// are long human-readable labels that vary by ROCm release, so keys are matched
// by substring rather than exact spelling, and a key that matches nothing is
// ignored.
//
// It deliberately does NOT set a UUID. rocm-smi's "Unique ID" is a different
// identifier from the UUID amd-smi reports, and letting one GPU carry two
// identities depending on which tool answered would fragment incident identity
// across a fallback. The event stays unattributed-but-addressed: index and PCI
// address are still carried, which is what the agent's dedup uses.
func (w *Watcher) parseROCmSMI(out []byte) ([]sample, error) {
	var cards map[string]map[string]json.RawMessage
	if err := json.Unmarshal(out, &cards); err != nil {
		return nil, fmt.Errorf("decode rocm-smi JSON: %w", err)
	}
	var samples []sample
	for card, fields := range cards {
		m := rocmCardRe.FindStringSubmatch(strings.ToLower(strings.TrimSpace(card)))
		if m == nil {
			// "system" and other non-card sections appear in the same object.
			continue
		}
		index, err := strconv.Atoi(m[1])
		if err != nil {
			continue
		}
		s := sample{index: index}
		for key, raw := range fields {
			label := strings.ToLower(key)
			value := rocmScalar(raw)
			switch {
			case strings.Contains(label, "pci bus"):
				s.bdf = normalizeBDF(value)
			case strings.Contains(label, "retired page"):
				n, ok := parseCount(value)
				if ok {
					s.retiredPages = &n
				}
				w.warnUnreadableSection("rocm-retired-pages", index, !ok)
			case strings.Contains(label, "temperature") &&
				(strings.Contains(label, "junction") || strings.Contains(label, "hotspot")):
				if f, err := strconv.ParseFloat(strings.TrimSpace(value), 64); err == nil {
					s.hotspotC = &f
				}
			}
		}
		if s.isEmpty() {
			w.logOnce("rocm-smi-no-fields", slog.LevelWarn,
				"rocm-smi output carried no field this parser recognizes; observing only (the fixtures are synthetic and may not match this rocm-smi version)",
				"card", card)
		}
		samples = append(samples, s)
	}
	return samples, nil
}

// rocmScalar renders a rocm-smi field value as text. rocm-smi quotes almost
// everything, but a release that emits a bare number must not be lost.
func rocmScalar(raw json.RawMessage) string {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return ""
	}
	if trimmed[0] == '"' {
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return ""
		}
		return strings.TrimSpace(s)
	}
	return trimmed
}

// parseCount reads a counter field, tolerating "N/A" and thousands separators.
// ok is false for unparseable values so they are ignored rather than counted as
// zero, which a later poll would misread as a fresh increase.
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

// normalizeBDF lowercases a PCI address so the same device compares equal
// whichever tool printed it; AMD tools disagree on case. The value is otherwise
// left exactly as reported: rewriting an address the agent cannot verify would
// invent device identity.
func normalizeBDF(bdf string) string {
	return strings.ToLower(strings.TrimSpace(bdf))
}
