package main

import (
	"bytes"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"
)

// The spec the kind harness runs on its first (labelled) node: seven Tesla T4
// peers with exactly one slow device (gpu 6) and exactly one device
// reporting unrepairable memory (gpu 2, the ShadowGpuMemoryBeyondRepair
// input), plus one slow A10G whose peer group stays below three even with
// the second node's healthy A10G. Must stay identical to the -spec argument
// in test/integration/gpu-observability/fixture.yaml.tmpl; the harness's
// --verify mode checks that.
const harnessSpecA = "Tesla T4:7:6:2;NVIDIA A10G:1:0"

var sampleLine = regexp.MustCompile(`^([A-Z_]+)\{([^}]*)\} (\S+)$`)

type parsedSample struct {
	name   string
	labels map[string]string
	value  string
}

func parseExposition(t *testing.T, text string) []parsedSample {
	t.Helper()
	var out []parsedSample
	for _, line := range strings.Split(strings.TrimSpace(text), "\n") {
		if strings.HasPrefix(line, "#") {
			continue
		}
		m := sampleLine.FindStringSubmatch(line)
		if m == nil {
			t.Fatalf("unparseable sample line %q", line)
		}
		labels := map[string]string{}
		for _, kv := range strings.Split(m[2], ",") {
			k, v, ok := strings.Cut(kv, "=")
			if !ok {
				t.Fatalf("bad label %q in %q", kv, line)
			}
			labels[k] = strings.Trim(v, `"`)
		}
		out = append(out, parsedSample{m[1], labels, m[3]})
	}
	return out
}

func render(t *testing.T, elapsed time.Duration) []parsedSample {
	t.Helper()
	devices, err := ParseSpec(harnessSpecA)
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := Render(&buf, "fixture-node", devices, elapsed); err != nil {
		t.Fatal(err)
	}
	return parseExposition(t, buf.String())
}

func TestSpecShape(t *testing.T) {
	devices, err := ParseSpec(harnessSpecA)
	if err != nil {
		t.Fatal(err)
	}
	if len(devices) != 8 {
		t.Fatalf("want 8 devices, got %d", len(devices))
	}
	t4, slowT4 := 0, 0
	for _, d := range devices {
		if d.Model == "Tesla T4" {
			t4++
			if d.Slow {
				slowT4++
			}
		}
	}
	if t4 < 7 || slowT4 != 1 {
		t.Fatalf("want >=7 Tesla T4 with exactly one slow, got %d/%d", t4, slowT4)
	}
	if !devices[7].Slow || devices[7].Model != "NVIDIA A10G" {
		t.Fatalf("device 7 should be the slow A10G: %+v", devices[7])
	}
	unrepairable := 0
	for _, d := range devices {
		if d.Unrepairable {
			unrepairable++
			if d.Index != 2 || d.Model != "Tesla T4" || d.Slow {
				t.Fatalf("unrepairable device should be the healthy Tesla T4 gpu 2: %+v", d)
			}
		}
	}
	if unrepairable != 1 {
		t.Fatalf("want exactly one unrepairable device, got %d", unrepairable)
	}
	three, err := ParseSpec("Tesla T4:2:-")
	if err != nil || three[0].Unrepairable || three[1].Unrepairable {
		t.Fatalf("three-field spec should parse with no unrepairable device: %v %+v", err, three)
	}
	uuids := map[string]bool{}
	for _, d := range devices {
		if uuids[d.UUID()] {
			t.Fatalf("duplicate UUID %s", d.UUID())
		}
		uuids[d.UUID()] = true
	}
	for _, bad := range []string{"", "Tesla T4", "Tesla T4:0:-", "Tesla T4:2:2", "Tesla T4:2:-:2", "Tesla T4:2:-:x", "Tesla T4:2:-:-:-", "a:b:c:d"} {
		if _, err := ParseSpec(bad); err == nil {
			t.Errorf("ParseSpec(%q) should fail", bad)
		}
	}
}

func TestExpositionLabelsAndValues(t *testing.T) {
	samples := render(t, 0)
	byName := map[string][]parsedSample{}
	for _, s := range samples {
		byName[s.name] = append(byName[s.name], s)
	}
	for _, want := range []string{"gpu", "UUID", "pci_bus_id", "device", "modelName", "Hostname", "DCGM_FI_DRIVER_VERSION", "pod", "namespace", "container"} {
		if _, ok := samples[0].labels[want]; !ok {
			t.Errorf("label %s missing from %q", want, samples[0])
		}
	}
	clocks := byName["DCGM_FI_DEV_SM_CLOCK"]
	if len(clocks) != 8 {
		t.Fatalf("want 8 SM clock samples, got %d", len(clocks))
	}
	slow := 0
	for _, c := range clocks {
		if c.labels["modelName"] == "Tesla T4" && c.value != "1590" {
			slow++
			if c.value != "1000" || c.labels["gpu"] != "6" {
				t.Errorf("unexpected slow T4 sample %+v", c)
			}
		}
	}
	if slow != 1 {
		t.Fatalf("want exactly one slow T4, got %d", slow)
	}
	for _, u := range byName["DCGM_FI_DEV_FB_USED"] {
		if u.labels["gpu"] == "0" && u.value != "12345" {
			t.Errorf("gpu 0 FB_USED = %s, want 12345 (-> 12944670720 bytes)", u.value)
		}
	}
	// 1507 is present on every device: 1 on gpu 2 only, a literal 0 elsewhere.
	unrepairable := byName["DCGM_FI_DEV_MEMORY_UNREPAIRABLE_FLAG"]
	if len(unrepairable) != 8 {
		t.Fatalf("want 8 MEMORY_UNREPAIRABLE samples, got %d", len(unrepairable))
	}
	for _, u := range unrepairable {
		want := "0"
		if u.labels["gpu"] == "2" {
			want = "1"
		}
		if u.value != want {
			t.Errorf("gpu %s MEMORY_UNREPAIRABLE = %s, want %s", u.labels["gpu"], u.value, want)
		}
	}
	for _, r := range byName["DCGM_FI_DEV_CLOCK_THROTTLE_REASONS"] {
		switch r.labels["gpu"] {
		case "6", "7":
			if r.value != "40" {
				t.Errorf("slow gpu %s throttle reasons %s, want 40", r.labels["gpu"], r.value)
			}
		default:
			if r.value != "1" {
				t.Errorf("healthy gpu %s throttle reasons %s, want 1", r.labels["gpu"], r.value)
			}
		}
	}
	// Workload labels: held devices carry pod/namespace/container, the last
	// device of each model is idle with empty values.
	for _, u := range byName["DCGM_FI_DEV_GPU_TEMP"] {
		held := u.labels["pod"] != ""
		switch u.labels["gpu"] {
		case "6", "7":
			if held {
				t.Errorf("gpu %s should be idle, got pod=%q", u.labels["gpu"], u.labels["pod"])
			}
		default:
			if !held || u.labels["namespace"] != "ml-fixture" || u.labels["container"] != "trainer" {
				t.Errorf("gpu %s should be held by a workload: %+v", u.labels["gpu"], u.labels)
			}
		}
	}
}

func TestCountersGrowOnlyWhereIntended(t *testing.T) {
	at0 := render(t, 0)
	at10 := render(t, 10*time.Second)
	if len(at0) != len(at10) {
		t.Fatalf("sample count changed over time: %d vs %d", len(at0), len(at10))
	}
	for i := range at0 {
		a, b := at0[i], at10[i]
		if a.name != b.name || a.labels["gpu"] != b.labels["gpu"] {
			t.Fatalf("exposition order is not stable at %d", i)
		}
		switch a.name {
		case "DCGM_FI_DEV_POWER_VIOLATION":
			slow := a.labels["gpu"] == "6" || a.labels["gpu"] == "7"
			if slow && b.value != "1200000000" {
				t.Errorf("slow gpu %s power violation after 10s = %s, want 1200000000", a.labels["gpu"], b.value)
			}
			if !slow && (a.value != "0" || b.value != "0") {
				t.Errorf("healthy gpu %s power violation moved: %s -> %s", a.labels["gpu"], a.value, b.value)
			}
		case "DCGM_FI_DEV_TOTAL_ENERGY_CONSUMPTION":
			if a.value == b.value {
				t.Errorf("energy counter for gpu %s did not grow", a.labels["gpu"])
			}
		default:
			if a.value != b.value {
				t.Errorf("%s for gpu %s should be constant: %s -> %s", a.name, a.labels["gpu"], a.value, b.value)
			}
		}
	}
}

// Every DCGM_FI_* source the recording rules read is either emitted by the
// fixture or on its deliberate omission list; nothing is left out by
// accident, and nothing on the omission list is emitted.
func TestCoversRecordingRuleSources(t *testing.T) {
	rules, err := os.ReadFile("../../configs/vmalert/gpu-recording-rules.yaml")
	if err != nil {
		t.Fatal(err)
	}
	emitted := EmittedFields()
	seen := map[string]bool{}
	for _, m := range regexp.MustCompile(`DCGM_FI_[A-Z0-9_]+`).FindAllString(string(rules), -1) {
		if seen[m] {
			continue
		}
		seen[m] = true
		isEmitted := slices.Contains(emitted, m)
		isOmitted := slices.Contains(OmittedFields, m)
		switch {
		case isEmitted && isOmitted:
			t.Errorf("%s is both emitted and on the omission list", m)
		case !isEmitted && !isOmitted:
			t.Errorf("recording rules read %s but the fixture neither emits nor deliberately omits it", m)
		}
	}
	for _, o := range OmittedFields {
		if !seen[o] {
			t.Errorf("omitted field %s is not read by any recording rule; the omission proves nothing", o)
		}
		if strings.Contains(o, "NVLINK") && !strings.HasPrefix(o, "DCGM_FI_DEV_NVLINK_") {
			t.Errorf("unexpected NVLink name %s", o)
		}
	}
	text := new(bytes.Buffer)
	devices, _ := ParseSpec(harnessSpecA)
	if err := Render(text, "n", devices, 0); err != nil {
		t.Fatal(err)
	}
	for _, o := range OmittedFields {
		if strings.Contains(text.String(), o) {
			t.Errorf("omitted field %s appears in the exposition", o)
		}
	}
	if strings.Contains(text.String(), "NVLINK") {
		t.Error("exposition contains an NVLink field")
	}
}
