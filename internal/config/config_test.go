package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/kubeneuron/kubeneuron/internal/checkpoint"
	"github.com/kubeneuron/kubeneuron/pkg/types"
)

func writeConfig(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "policies.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadShippedConfig(t *testing.T) {
	c, err := Load("../../configs/policies.yaml")
	if err != nil {
		t.Fatalf("shipped config must load: %v", err)
	}
	if !c.Safety.DryRun {
		t.Fatal("shipped config must default to dry-run")
	}
	if c.Safety.MaxConcurrentRemediations <= 0 || c.Safety.MaxConcurrentReboots <= 0 {
		t.Fatal("shipped config must set positive concurrency limits")
	}
}

func TestLoadAppliesDefaults(t *testing.T) {
	path := writeConfig(t, `
policies:
  - match: { class: ecc-dbe }
    playbook: drain-and-reset
`)
	c, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if c.Safety.MaxConcurrentRemediations != 2 || c.Safety.MaxConcurrentReboots != 1 {
		t.Fatalf("concurrency defaults = %d/%d, want 2/1",
			c.Safety.MaxConcurrentRemediations, c.Safety.MaxConcurrentReboots)
	}
	if c.Approvals.TTL.Std() != 12*time.Hour {
		t.Fatalf("approval TTL default = %v, want 12h", c.Approvals.TTL.Std())
	}
	if c.Policies[0].Match.Class != types.ClassECCDBE {
		t.Fatalf("class = %s", c.Policies[0].Match.Class)
	}
}

func TestLoadRejectsInvalidConfig(t *testing.T) {
	for name, tc := range map[string]struct {
		content string
		wantErr string
	}{
		"no policies":      {"safety: { dry_run: true }", "at least one policy"},
		"missing class":    {"policies:\n  - playbook: rma", "match.class is required"},
		"missing playbook": {"policies:\n  - match: { class: ecc-dbe }", "playbook is required"},
		"bad duration":     {"safety: { verify_quiet_window: nonsense }\npolicies:\n  - match: { class: x }\n    playbook: y", "invalid duration"},
		"bad yaml":         {"policies: [", "yaml"},
		// A typo'd taint effect must fail the load, not fall back to something
		// that works: the whole point of the field is that the operator gets
		// the scheduling effect they asked for.
		"bad taint effect": {"safety:\n  taint_degraded_nodes: { enabled: true, effect: NoExecute }\npolicies:\n  - match: { class: x }\n    playbook: y", "taint_degraded_nodes.effect"},
		// An enabled checkpoint policy fails closed on every bound it could
		// not enforce as written. A wait that quietly became longer, or an
		// allowlist that quietly became "everyone", is exactly what the
		// feature promises cannot happen.
		"checkpoint no namespaces":      {checkpointConfig("{ enabled: true }"), "namespaces must name"},
		"checkpoint blank namespace":    {checkpointConfig(`{ enabled: true, namespaces: ["a", " "] }`), "blank entry"},
		"checkpoint blank skip class":   {checkpointConfig(`{ enabled: true, namespaces: [a], skip_classes: [""] }`), "skip_classes must not"},
		"checkpoint negative default":   {checkpointConfig(`{ enabled: true, namespaces: [a], default_wait: -1m }`), "default_wait must be positive"},
		"checkpoint negative max":       {checkpointConfig(`{ enabled: true, namespaces: [a], max_wait: -1m }`), "max_wait must be positive"},
		"checkpoint max above ceiling":  {checkpointConfig(`{ enabled: true, namespaces: [a], max_wait: 31m }`), "exceeds the 30m0s ceiling"},
		"checkpoint default above max":  {checkpointConfig(`{ enabled: true, namespaces: [a], default_wait: 10m, max_wait: 5m }`), "default_wait 10m0s exceeds max_wait"},
		"checkpoint malformed duration": {checkpointConfig(`{ enabled: true, namespaces: [a], max_wait: soon }`), "invalid duration"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := Load(writeConfig(t, tc.content))
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err = %v, want substring %q", err, tc.wantErr)
			}
		})
	}
}

// The weak effect is what an operator who only said "enabled" gets. A stronger
// one has to be typed out.
func TestTaintEffectDefaultsToPreferNoSchedule(t *testing.T) {
	cfg, err := Load(writeConfig(t,
		"safety:\n  taint_degraded_nodes: { enabled: true }\npolicies:\n  - match: { class: x }\n    playbook: y"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Safety.TaintDegradedNodes == nil || cfg.Safety.TaintDegradedNodes.Effect != TaintEffectPreferNoSchedule {
		t.Fatalf("compiled taint = %+v, want the weak default effect", cfg.Safety.TaintDegradedNodes)
	}
}

// checkpointConfig wraps one checkpoint_coordination block in a loadable file.
func checkpointConfig(block string) string {
	return "safety:\n  checkpoint_coordination: " + block + "\npolicies:\n  - match: { class: x }\n    playbook: y"
}

func TestCheckpointCoordinationDefaultsAndDisabled(t *testing.T) {
	cfg, err := Load(writeConfig(t, checkpointConfig(`{ enabled: true, namespaces: [training], skip_classes: [fell-off-bus] }`)))
	if err != nil {
		t.Fatal(err)
	}
	cc := cfg.Safety.CheckpointCoordination
	if cc == nil || !cc.Enabled {
		t.Fatalf("compiled checkpoint policy = %+v, want enabled", cc)
	}
	if cc.DefaultWait != CheckpointDefaultWait || cc.MaxWait != CheckpointDefaultMaxWait {
		t.Fatalf("waits = %v/%v, want the 5m/15m defaults", cc.DefaultWait.Std(), cc.MaxWait.Std())
	}
	if len(cc.Namespaces) != 1 || cc.Namespaces[0] != "training" {
		t.Fatalf("namespaces = %v", cc.Namespaces)
	}
	if len(cc.SkipClasses) != 1 || cc.SkipClasses[0] != types.ClassFellOffBus {
		t.Fatalf("explicit skip classes = %v, want exactly what was written", cc.SkipClasses)
	}

	// An absent skip list defaults to the device-dead classes; an explicit
	// empty list is the explicit decision to coordinate for every class, and
	// the two must not collapse into each other.
	cfg, err = Load(writeConfig(t, checkpointConfig(`{ enabled: true, namespaces: [training] }`)))
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Safety.CheckpointCoordination.SkipClasses; !reflect.DeepEqual(got, checkpoint.DefaultSkipClasses()) {
		t.Fatalf("absent skip_classes = %v, want the defaults %v", got, checkpoint.DefaultSkipClasses())
	}
	cfg, err = Load(writeConfig(t, checkpointConfig(`{ enabled: true, namespaces: [training], skip_classes: [] }`)))
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Safety.CheckpointCoordination.SkipClasses; got == nil || len(got) != 0 {
		t.Fatalf("explicit empty skip_classes = %v, want an empty list that skips nothing", got)
	}
	// The round trip keeps the key even when empty, so a controller reloading
	// what the operator wrote cannot re-default an explicit "skip none".
	out, err := yaml.Marshal(cfg.Safety)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "skip_classes: []") {
		t.Fatalf("marshalled safety block drops the empty skip list:\n%s", out)
	}

	// Explicit values are carried through, including the ceiling itself.
	cfg, err = Load(writeConfig(t, checkpointConfig(`{ enabled: true, namespaces: [a], default_wait: 30m, max_wait: 30m }`)))
	if err != nil {
		t.Fatal(err)
	}
	if cc := cfg.Safety.CheckpointCoordination; cc.DefaultWait.Std() != 30*time.Minute || cc.MaxWait.Std() != 30*time.Minute {
		t.Fatalf("waits = %+v, want 30m/30m", cc)
	}

	// A disabled block is inert: nothing in it is validated or defaulted,
	// because nothing in it is read.
	cfg, err = Load(writeConfig(t, checkpointConfig(`{ enabled: false, max_wait: 2h }`)))
	if err != nil {
		t.Fatalf("a disabled block must load: %v", err)
	}
	if cc := cfg.Safety.CheckpointCoordination; cc == nil || cc.Enabled || cc.DefaultWait != 0 {
		t.Fatalf("disabled block = %+v, want untouched", cc)
	}

	// The absent key is the common case and stays nil.
	cfg, err = Load(writeConfig(t, "policies:\n  - match: { class: x }\n    playbook: y"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Safety.CheckpointCoordination != nil {
		t.Fatalf("absent key compiled to %+v, want nil", cfg.Safety.CheckpointCoordination)
	}
}

func TestDurationRoundTrip(t *testing.T) {
	d := Duration(90 * time.Minute)
	out, err := d.MarshalYAML()
	if err != nil || out != "1h30m0s" {
		t.Fatalf("MarshalYAML = %v, %v", out, err)
	}
}
