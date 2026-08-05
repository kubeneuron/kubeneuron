package config

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/kubeneuron/kubeneuron/pkg/types"
)

const profileDigest = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func TestAcceleratorRuntimeProfileValidatesAndGatesNVIDIAReset(t *testing.T) {
	now := time.Date(2026, time.July, 24, 12, 0, 0, 0, time.UTC)
	profile := validNVIDIAProfile()
	if err := profile.Validate(); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}
	if !profile.MatchesLabels(map[string]string{"accelerator": "nvidia", "pool": "a100"}) {
		t.Fatal("MatchesLabels() = false, want true")
	}
	if profile.MatchesLabels(map[string]string{"accelerator": "nvidia"}) {
		t.Fatal("MatchesLabels() = true with a missing required label")
	}
	if !profile.Allows(types.AcceleratorActionResetDevice, types.AcceleratorScopePhysicalDevice) {
		t.Fatal("Allows(reset-device, physical-device) = false")
	}
	if !profile.RequiresVerifiedUnpartitionedTopology(types.AcceleratorActionResetDevice, types.AcceleratorScopePhysicalDevice) {
		t.Fatal("physical reset must require verified-unpartitioned topology")
	}
	if profile.Allows(types.AcceleratorActionRebootNode, types.AcceleratorScopeNode) {
		t.Fatal("unlisted action must not be allowed")
	}

	report := readyNVIDIAReport(now.Add(-5 * time.Minute))
	if err := profile.CheckAction(now, report, types.AcceleratorActionResetDevice, types.AcceleratorScopePhysicalDevice); err != nil {
		t.Fatalf("CheckAction() error = %v", err)
	}
}

func TestAcceleratorRuntimeProfileRejectsUnsafeOrAmbiguousConfiguration(t *testing.T) {
	for name, mutate := range map[string]func(*AcceleratorRuntimeProfile){
		"empty selector":          func(p *AcceleratorRuntimeProfile) { p.NodeSelector = nil },
		"wrong vendor":            func(p *AcceleratorRuntimeProfile) { p.Vendor = types.AcceleratorVendorAMD },
		"unreviewed digest":       func(p *AcceleratorRuntimeProfile) { p.ProfileDigest = "latest" },
		"missing driver version":  func(p *AcceleratorRuntimeProfile) { p.DriverVersion = "" },
		"missing runtime version": func(p *AcceleratorRuntimeProfile) { p.RuntimeVersion = "" },
		"missing profile UID":     func(p *AcceleratorRuntimeProfile) { p.ProfileUID = "" },
		"zero profile generation": func(p *AcceleratorRuntimeProfile) { p.ProfileGeneration = 0 },
		"unbounded report":        func(p *AcceleratorRuntimeProfile) { p.MaxReportAge = 0 },
		"physical reset missing topology precondition": func(p *AcceleratorRuntimeProfile) {
			p.AllowedActions[0].RequireVerifiedUnpartitionedTopology = false
		},
		"partition reset": func(p *AcceleratorRuntimeProfile) {
			p.AllowedActions[0].Scopes = []types.AcceleratorTargetScope{types.AcceleratorScopePartition}
		},
		"topology precondition on non reset": func(p *AcceleratorRuntimeProfile) {
			p.AllowedActions = []AcceleratorActionPolicy{{
				Action:                               types.AcceleratorActionCollectDiagnostics,
				Scopes:                               []types.AcceleratorTargetScope{types.AcceleratorScopeNode},
				RequireVerifiedUnpartitionedTopology: true,
			}}
		},
	} {
		t.Run(name, func(t *testing.T) {
			profile := validNVIDIAProfile()
			mutate(&profile)
			if err := profile.Validate(); err == nil {
				t.Fatal("Validate() error = nil, want rejection")
			}
		})
	}
}

func TestConfigAcceleratorRuntimeProfilesDefaultDenyAndAmbiguity(t *testing.T) {
	var empty Config
	if _, err := empty.ResolveAcceleratorRuntimeProfile(map[string]string{"accelerator": "nvidia", "pool": "a100"}, types.AcceleratorVendorNVIDIA); !errors.Is(err, ErrNoAcceleratorRuntimeProfile) {
		t.Fatalf("default ResolveAcceleratorRuntimeProfile() error = %v, want ErrNoAcceleratorRuntimeProfile", err)
	}

	first := validNVIDIAProfile()
	second := validNVIDIAProfile()
	second.Name = "nvidia-a100-secondary"
	config := Config{
		Policies:            []Policy{{Match: Match{Class: types.ClassECCDBE}, Playbook: "drain-and-reset"}},
		AcceleratorProfiles: []AcceleratorRuntimeProfile{first, second},
	}
	if err := config.Validate(); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}
	if _, err := config.ResolveAcceleratorRuntimeProfile(map[string]string{"accelerator": "nvidia", "pool": "a100"}, types.AcceleratorVendorNVIDIA); !errors.Is(err, ErrAmbiguousAcceleratorRuntimeProfile) {
		t.Fatalf("overlapping ResolveAcceleratorRuntimeProfile() error = %v, want ErrAmbiguousAcceleratorRuntimeProfile", err)
	}

	second.NodeSelector = map[string]string{"accelerator": "nvidia", "pool": "h100"}
	config.AcceleratorProfiles[1] = second
	got, err := config.ResolveAcceleratorRuntimeProfile(map[string]string{"accelerator": "nvidia", "pool": "a100"}, types.AcceleratorVendorNVIDIA)
	if err != nil {
		t.Fatalf("ResolveAcceleratorRuntimeProfile() error = %v", err)
	}
	if got.Name != first.Name {
		t.Fatalf("resolved profile = %q, want %q", got.Name, first.Name)
	}
}

func TestAcceleratorRuntimeProfileCheckActionFailsClosedOnReportDrift(t *testing.T) {
	now := time.Date(2026, time.July, 24, 12, 0, 0, 0, time.UTC)
	profile := validNVIDIAProfile()
	for name, tc := range map[string]struct {
		mutate func(*types.AgentAcceleratorReport)
		want   string
	}{
		"stale": {
			mutate: func(r *types.AgentAcceleratorReport) { r.ObservedAt = now.Add(-11 * time.Minute) },
			want:   "older than max_report_age",
		},
		"different profile": {
			mutate: func(r *types.AgentAcceleratorReport) {
				r.ProfileDigest = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
			},
			want: "digest does not match",
		},
		"different profile generation": {
			mutate: func(r *types.AgentAcceleratorReport) { r.ProfileGeneration = 2 },
			want:   "identity does not match",
		},
		"different driver version": {
			mutate: func(r *types.AgentAcceleratorReport) { r.DriverVersion = "550.54.15" },
			want:   "driver version does not match",
		},
		"different runtime version": {
			mutate: func(r *types.AgentAcceleratorReport) { r.RuntimeVersion = "dcgm-3.3.0" },
			want:   "does not satisfy the pinned",
		},
		"missing declared capability": {
			mutate: func(r *types.AgentAcceleratorReport) { r.Capabilities = nil },
			want:   "does not declare action",
		},
	} {
		t.Run(name, func(t *testing.T) {
			report := readyNVIDIAReport(now.Add(-5 * time.Minute))
			tc.mutate(&report)
			err := profile.CheckAction(now, report, types.AcceleratorActionResetDevice, types.AcceleratorScopePhysicalDevice)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("CheckAction() error = %v, want substring %q", err, tc.want)
			}
		})
	}
}

func TestConfigLoadsAcceleratorRuntimeProfiles(t *testing.T) {
	config := `
policies:
  - match: { class: ecc-dbe }
    playbook: drain-and-reset
accelerator_profiles:
  - name: nvidia-a100
    node_selector:
      accelerator: nvidia
      pool: a100
    vendor: nvidia
    profile_digest: sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef
    driver_version: 570.1
    runtime_version: dcgm-4.1
    profile_uid: profile-uid-a100
    profile_generation: 1
    max_report_age: 10m
    allowed_actions:
      - action: reset-device
        scopes: [physical-device]
        require_verified_unpartitioned_topology: true
`
	c, err := Load(writeConfig(t, config))
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if len(c.AcceleratorProfiles) != 1 {
		t.Fatalf("loaded profiles = %d, want 1", len(c.AcceleratorProfiles))
	}
}

func validNVIDIAProfile() AcceleratorRuntimeProfile {
	return AcceleratorRuntimeProfile{
		Name:              "nvidia-a100",
		NodeSelector:      map[string]string{"accelerator": "nvidia", "pool": "a100"},
		Vendor:            types.AcceleratorVendorNVIDIA,
		ProfileDigest:     profileDigest,
		DriverVersion:     "570.1",
		RuntimeVersion:    "dcgm-4.1",
		ProfileUID:        "profile-uid-a100",
		ProfileGeneration: 1,
		MaxReportAge:      Duration(10 * time.Minute),
		AllowedActions: []AcceleratorActionPolicy{{
			Action: types.AcceleratorActionResetDevice,
			Scopes: []types.AcceleratorTargetScope{
				types.AcceleratorScopePhysicalDevice,
			},
			RequireVerifiedUnpartitionedTopology: true,
		}},
	}
}

func readyNVIDIAReport(observedAt time.Time) types.AgentAcceleratorReport {
	return types.AgentAcceleratorReport{
		Node:           "gpu-node-1",
		Vendor:         types.AcceleratorVendorNVIDIA,
		ObservedAt:     observedAt,
		DriverVersion:  "570.1",
		RuntimeVersion: "dcgm-4.1",
		TopologySafety: types.AcceleratorTopologyVerifiedUnpartitioned,
		Devices: []types.AgentAcceleratorDevice{{
			ID: "GPU-a", Kind: types.AcceleratorDevicePhysical, Family: types.AcceleratorFamilyGPU,
		}},
		Capabilities: []types.AgentAcceleratorCapability{{
			Action: types.AcceleratorActionResetDevice,
			Scopes: []types.AcceleratorTargetScope{types.AcceleratorScopePhysicalDevice},
		}},
		Readiness:         types.AcceleratorReadinessReady,
		ProfileDigest:     profileDigest,
		ProfileUID:        "profile-uid-a100",
		ProfileGeneration: 1,
	}
}
