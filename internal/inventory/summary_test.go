// SPDX-License-Identifier: Apache-2.0
package inventory

import (
	"strings"
	"testing"
	"time"
)

func TestParseInventorySummary(t *testing.T) {
	now := time.Now().UTC()
	want := Summary{ObservedAt: now, LastReportAt: now, ReportingNodes: 2, GPUs: 4, ResetConfirmed: true, AdvertisedNodes: 2}
	data := want.Data()
	data["extra"] = "ignored"
	got, err := Parse(data, now)
	if err != nil || got != want {
		t.Fatalf("round trip: %+v, %v", got, err)
	}
	for key, value := range map[string]string{"schemaVersion": "2", "observedAt": now.Add(time.Hour).Format(time.RFC3339Nano), "lastReportAt": "", "gpuCount": "2147483648", "advertisedNodes": "-1", "reportingNodes": "0", "resetConfirmed": "TRUE"} {
		data := want.Data()
		data[key] = value
		if _, err := Parse(data, now); err == nil {
			t.Errorf("accepted %s=%q", key, value)
		}
	}
}

func TestNVIDIANodesAndDiagnosticText(t *testing.T) {
	now := time.Now().UTC()
	want := Summary{ObservedAt: now, LastReportAt: now, ReportingNodes: 2, AdvertisedNodes: 3, NVIDIANodes: 2, Diagnostic: "RuntimeDidNotExposeGPUs"}
	got, err := Parse(want.Data(), now)
	if err != nil || got != want {
		t.Fatalf("round trip: %+v, %v", got, err)
	}
	data := want.Data()
	data["nvidiaNodes"] = "4"
	if _, err := Parse(data, now); err == nil {
		t.Error("accepted more NVIDIA nodes than advertised ones")
	}
	if text := DiagnosticText(want); !strings.HasPrefix(text, "Kubernetes advertises NVIDIA GPUs on 2 nodes, but the agents see none.") ||
		!strings.HasSuffix(text, "not device health.") {
		t.Fatalf("text = %q", text)
	}
	want.NVIDIANodes = 1
	if text := DiagnosticText(want); !strings.HasPrefix(text, "Kubernetes advertises NVIDIA GPUs on 1 node, but") {
		t.Fatalf("text = %q", text)
	}
	if DiagnosticText(Summary{Diagnostic: "DeviceAccessDisabled"}) != "Agents do not inventory GPUs because spec.agent.deviceAccess is None. Node replacement does not depend on this; after a replacement KubeNeuron checks node readiness and advertised capacity, not device health." {
		t.Fatal(DiagnosticText(Summary{Diagnostic: "DeviceAccessDisabled"}))
	}
}
