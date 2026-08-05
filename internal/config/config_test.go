package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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

func TestDurationRoundTrip(t *testing.T) {
	d := Duration(90 * time.Minute)
	out, err := d.MarshalYAML()
	if err != nil || out != "1h30m0s" {
		t.Fatalf("MarshalYAML = %v, %v", out, err)
	}
}
