package types

import (
	"strings"
	"testing"
	"time"
)

func TestAgentAcceleratorReportValidateAcceptsSafeObservation(t *testing.T) {
	report := validAgentAcceleratorReport()
	if err := report.Validate(); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}
}

func TestAgentAcceleratorReportValidateFailsClosed(t *testing.T) {
	for name, mutate := range map[string]func(*AgentAcceleratorReport){
		"unknown vendor": func(report *AgentAcceleratorReport) {
			report.Vendor = "unrecognized"
		},
		"node UID whitespace": func(report *AgentAcceleratorReport) {
			report.NodeUID = " node-uid "
		},
		"missing observed time": func(report *AgentAcceleratorReport) {
			report.ObservedAt = time.Time{}
		},
		"unknown topology": func(report *AgentAcceleratorReport) {
			report.TopologySafety = "maybe-safe"
		},
		"ready lacks runtime version": func(report *AgentAcceleratorReport) {
			report.RuntimeVersion = ""
		},
		"partition lacks physical parent": func(report *AgentAcceleratorReport) {
			report.Devices = append(report.Devices, AgentAcceleratorDevice{
				ID: "MIG-a", Kind: AcceleratorDevicePartition, Family: AcceleratorFamilyGPU,
			})
		},
		"unsafe physical reset": func(report *AgentAcceleratorReport) {
			report.TopologySafety = AcceleratorTopologyUnknown
		},
		"duplicate declared scope": func(report *AgentAcceleratorReport) {
			report.Capabilities[0].Scopes = append(report.Capabilities[0].Scopes, AcceleratorScopePhysicalDevice)
		},
		"profile generation without UID": func(report *AgentAcceleratorReport) {
			report.ProfileGeneration = 1
		},
		"profile UID without generation": func(report *AgentAcceleratorReport) {
			report.ProfileUID = "profile-uid"
		},
	} {
		t.Run(name, func(t *testing.T) {
			report := validAgentAcceleratorReport()
			mutate(&report)
			if err := report.Validate(); err == nil {
				t.Fatal("Validate() succeeded, want fail-closed rejection")
			}
		})
	}
}

func TestAgentAcceleratorObservationProfileValidatePinsDigest(t *testing.T) {
	profile := AgentAcceleratorObservationProfile{
		Vendor:            AcceleratorVendorNVIDIA,
		ProfileDigest:     "sha256:" + strings.Repeat("a", 64),
		ProfileUID:        "profile-uid-a",
		ProfileGeneration: 1,
		RuntimeVersion:    "dcgm-4.1",
	}
	if err := profile.Validate(); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}
	profile.ProfileDigest = "sha256:not-a-reviewable-digest"
	if err := profile.Validate(); err == nil {
		t.Fatal("Validate() accepted malformed profile digest")
	}
}

func validAgentAcceleratorReport() AgentAcceleratorReport {
	return AgentAcceleratorReport{
		Node:           "node-a",
		Vendor:         AcceleratorVendorNVIDIA,
		ObservedAt:     time.Date(2026, time.July, 24, 12, 0, 0, 0, time.UTC),
		DriverVersion:  "570.42",
		RuntimeVersion: "dcgm-4.1",
		TopologySafety: AcceleratorTopologyVerifiedUnpartitioned,
		Devices: []AgentAcceleratorDevice{{
			ID: "GPU-a", Kind: AcceleratorDevicePhysical, Family: AcceleratorFamilyGPU, Model: "H100",
		}},
		Capabilities: []AgentAcceleratorCapability{{
			Action: AcceleratorActionResetDevice, Scopes: []AcceleratorTargetScope{AcceleratorScopePhysicalDevice},
		}, {
			Action: AcceleratorActionVerifyHealth, Scopes: []AcceleratorTargetScope{AcceleratorScopeNode, AcceleratorScopePhysicalDevice},
		}},
		Readiness:     AcceleratorReadinessReady,
		ProfileDigest: "sha256:4c5bbf8f",
	}
}
