// SPDX-License-Identifier: Apache-2.0
package inventory

import (
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
