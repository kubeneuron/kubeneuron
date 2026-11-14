// SPDX-License-Identifier: Apache-2.0

package types

import (
	"regexp"
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
		"malformed vendor": func(report *AgentAcceleratorReport) {
			report.Vendor = "Not A Vendor"
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

func TestAcceleratorVendorValidIsOpenButBounded(t *testing.T) {
	for _, vendor := range []AcceleratorVendor{AcceleratorVendorNVIDIA, AcceleratorVendorAMD, AcceleratorVendorIntel, AcceleratorVendorGoogle, "acme", "aws-trainium", "x"} {
		if !vendor.Valid() {
			t.Errorf("%q.Valid() = false, want true", vendor)
		}
	}
	for _, vendor := range []AcceleratorVendor{"", "NVIDIA", "nvidia ", "-amd", "amd-", "a_b", "a.b", AcceleratorVendor(strings.Repeat("a", MaxAcceleratorVendorLength+1))} {
		if vendor.Valid() {
			t.Errorf("%q.Valid() = true, want false", vendor)
		}
	}
	pattern := regexp.MustCompile(AcceleratorVendorPattern)
	for _, vendor := range []string{"acme", "aws-trainium", "x", "NVIDIA", "-amd", "a_b", strings.Repeat("a", 32), strings.Repeat("a", 33)} {
		if pattern.MatchString(vendor) != AcceleratorVendor(vendor).Valid() {
			t.Errorf("AcceleratorVendorPattern and Valid disagree on %q", vendor)
		}
	}
}

func TestOnlyAnAgentSignalNamesAVendor(t *testing.T) {
	evidence := map[string]string{"vendor": "acme"}
	if got := (Signal{Source: SourceAgentEvent, Evidence: evidence}).Vendor(); got != "acme" {
		t.Fatalf("agent signal vendor = %q, want acme", got)
	}
	for _, source := range []SignalSource{SourceAlertmanager, SourceManual, ""} {
		if got := (Signal{Source: source, Evidence: evidence}).Vendor(); got != "" {
			t.Fatalf("%q signal vendor = %q: an alert annotation must not choose the vendor", source, got)
		}
	}
	if got := (Signal{Source: SourceAgentEvent, Evidence: map[string]string{"vendor": "Not Valid"}}).Vendor(); got != "" {
		t.Fatalf("malformed vendor = %q, want empty", got)
	}
}
