// SPDX-License-Identifier: Apache-2.0

package gcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/api/googleapi"
	monitoring "google.golang.org/api/monitoring/v3"
	"google.golang.org/api/option"

	"github.com/kubeneuron/kubeneuron/pkg/cloud"
)

var healthNow = time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)

// fakeMonitoring serves scripted pages per project.
type fakeMonitoring struct {
	mu      sync.Mutex
	pages   map[string][]*monitoring.ListTimeSeriesResponse
	err     error
	calls   []string
	filters []string
}

func (f *fakeMonitoring) ListTimeSeries(_ context.Context, project, filter string, start, end time.Time, token string) (*monitoring.ListTimeSeriesResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, project+"#"+token)
	f.filters = append(f.filters, filter)
	if f.err != nil {
		return nil, f.err
	}
	if !end.Equal(healthNow) || end.Sub(start) != infraHealthWindow {
		return nil, fmt.Errorf("interval %v..%v", start, end)
	}
	pages := f.pages[project]
	i := 0
	if token != "" {
		if _, err := fmt.Sscanf(token, "page-%d", &i); err != nil {
			return nil, err
		}
	}
	if i >= len(pages) {
		return &monitoring.ListTimeSeriesResponse{}, nil
	}
	resp := *pages[i]
	if i+1 < len(pages) {
		resp.NextPageToken = fmt.Sprintf("page-%d", i+1)
	}
	return &resp, nil
}

func series(zone, name, status, category string, ago ...time.Duration) *monitoring.TimeSeries {
	labels := map[string]string{"health_status": status, "machine_id": "m-1", "reservation_id": "r-1"}
	if category != "" {
		labels["unhealthy_category"] = category
	}
	system, _ := json.Marshal(map[string]any{"name": name, "machine_image": "secret-image"})
	ts := &monitoring.TimeSeries{
		Metric:   &monitoring.Metric{Type: infraHealthMetric, Labels: labels},
		Resource: &monitoring.MonitoredResource{Type: "gce_instance", Labels: map[string]string{"zone": zone, "instance_id": "123"}},
		Metadata: &monitoring.MonitoredResourceMetadata{SystemLabels: system, UserLabels: map[string]string{"team": "x"}},
	}
	for _, a := range ago {
		at := healthNow.Add(-a).Format(time.RFC3339)
		ts.Points = append(ts.Points, &monitoring.Point{Interval: &monitoring.TimeInterval{StartTime: at, EndTime: at}, Value: &monitoring.TypedValue{Int64Value: googleapi.Int64(1)}})
	}
	return ts
}

func healthProvider(api monitoringAPI) *Provider {
	p := newWithAPI(settings{project: "proj-a", location: "us-central2", cluster: "c"}, nil)
	p.monitoring = api
	p.now = func() time.Time { return healthNow }
	return p
}

func TestAcceleratorHealthReadsTheInstancesPoints(t *testing.T) {
	fake := &fakeMonitoring{pages: map[string][]*monitoring.ListTimeSeriesResponse{
		"proj-a": {
			{TimeSeries: []*monitoring.TimeSeries{
				series("us-central2-b", "tpu-vm-1", "UNHEALTHY", "BAD_TPU_CHIP", time.Minute),
				// Another instance of the project: not asked for.
				series("us-central2-b", "other-vm", "UNHEALTHY", "BAD_TPU_CHIP", time.Minute),
			}},
			{TimeSeries: []*monitoring.TimeSeries{
				series("us-central2-b", "tpu-vm-1", "HEALTHY", "", 3*time.Minute, 2*time.Minute),
				// The same name in another zone is another instance.
				series("us-central2-a", "tpu-vm-1", "HEALTHY", "", time.Minute),
			}},
		},
		"proj-b": {{TimeSeries: []*monitoring.TimeSeries{series("europe-west4-a", "tpu-vm-2", "DEGRADED", "", time.Minute)}}},
	}}
	got, err := healthProvider(fake).AcceleratorHealth(context.Background(), []string{
		"proj-a/us-central2-b/tpu-vm-1", "proj-b/europe-west4-a/tpu-vm-2", "proj-a/us-central2-b/silent-vm", "not an id",
	})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string][]cloud.HealthPoint{
		"proj-a/us-central2-b/tpu-vm-1": {
			{At: healthNow.Add(-3 * time.Minute), Status: "HEALTHY"},
			{At: healthNow.Add(-2 * time.Minute), Status: "HEALTHY"},
			{At: healthNow.Add(-time.Minute), Status: "UNHEALTHY", Category: "BAD_TPU_CHIP"},
		},
		"proj-b/europe-west4-a/tpu-vm-2": {{At: healthNow.Add(-time.Minute), Status: "DEGRADED"}},
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("got %v\nwant %v", got, want)
	}
	if strings.Join(fake.calls, ",") != "proj-a#,proj-a#page-1,proj-b#" {
		t.Errorf("calls %v: one list per project, every page", fake.calls)
	}
	for _, f := range fake.filters {
		if f != `metric.type = "compute.googleapis.com/instance/tpu/infra_health" AND resource.type = "gce_instance"` {
			t.Errorf("filter %q", f)
		}
	}
}

func TestAcceleratorHealthFailures(t *testing.T) {
	ids := []string{"proj-a/us-central2-b/tpu-vm-1"}
	cases := map[string]struct {
		api    monitoringAPI
		reason string
	}{
		"forbidden":    {&fakeMonitoring{err: &googleapi.Error{Code: http.StatusForbidden, Message: "Permission monitoring.timeSeries.list denied"}}, cloud.HealthReadPermissionDenied},
		"unauthorized": {&fakeMonitoring{err: &googleapi.Error{Code: http.StatusUnauthorized}}, cloud.HealthReadPermissionDenied},
		"unavailable":  {&fakeMonitoring{err: &googleapi.Error{Code: http.StatusServiceUnavailable}}, cloud.HealthReadAPIError},
		"no client":    {unavailableMonitoring{errors.New("no credentials")}, cloud.HealthReadAPIError},
		"too many pages": {func() monitoringAPI {
			pages := make([]*monitoring.ListTimeSeriesResponse, infraHealthMaxPages+1)
			for i := range pages {
				pages[i] = &monitoring.ListTimeSeriesResponse{TimeSeries: []*monitoring.TimeSeries{series("us-central2-b", "tpu-vm-1", "HEALTHY", "", time.Minute)}}
			}
			return &fakeMonitoring{pages: map[string][]*monitoring.ListTimeSeriesResponse{"proj-a": pages}}
		}(), cloud.HealthReadAPIError},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			got, err := healthProvider(tc.api).AcceleratorHealth(context.Background(), ids)
			var herr *cloud.HealthReadError
			if !errors.As(err, &herr) || herr.Reason != tc.reason {
				t.Fatalf("err %v, want a %s read error", err, tc.reason)
			}
			if got != nil {
				t.Errorf("a failed read returned points %v; part of the answer is not the whole of it", got)
			}
		})
	}
}

func TestAcceleratorHealthOff(t *testing.T) {
	p := newWithAPI(settings{project: "proj-a", location: "us-central2", cluster: "c"}, nil)
	if p.AcceleratorHealthEnabled() {
		t.Fatal("the read is on without the option")
	}
	if got, err := p.AcceleratorHealth(context.Background(), []string{"proj-a/us-central2-b/tpu-vm-1"}); got != nil || err != nil {
		t.Fatalf("%v, %v", got, err)
	}
	s, err := settingsFrom(cloud.Config{Options: map[string]string{OptionProject: "proj-a", OptionLocation: "l", OptionCluster: "c"}})
	if err != nil || s.tpuInfraHealth {
		t.Fatalf("default settings %+v, %v: the read must be off", s, err)
	}
	s, _ = settingsFrom(cloud.Config{Options: map[string]string{OptionProject: "proj-a", OptionLocation: "l", OptionCluster: "c", OptionTPUInfraHealth: "true"}})
	if !s.tpuInfraHealth {
		t.Fatal("the option does not turn the read on")
	}
}

// The REST client asks for the documented metric over the window, with the
// metadata, and follows the page token.
func TestRESTMonitoringRequest(t *testing.T) {
	var mu sync.Mutex
	var queries []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		queries = append(queries, r.URL.Path+"?"+r.URL.RawQuery)
		mu.Unlock()
		resp := monitoring.ListTimeSeriesResponse{TimeSeries: []*monitoring.TimeSeries{series("us-central2-b", "tpu-vm-1", "HEALTHY", "", time.Minute)}}
		if r.URL.Query().Get("pageToken") == "" {
			resp.NextPageToken = "next"
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()
	svc, err := monitoring.NewService(context.Background(), option.WithEndpoint(srv.URL), option.WithoutAuthentication(), option.WithHTTPClient(srv.Client()))
	if err != nil {
		t.Fatal(err)
	}
	got, err := healthProvider(restMonitoring{svc: svc}).AcceleratorHealth(context.Background(), []string{"proj-a/us-central2-b/tpu-vm-1"})
	if err != nil {
		t.Fatal(err)
	}
	if n := len(got["proj-a/us-central2-b/tpu-vm-1"]); n != 2 {
		t.Errorf("%d points, want one from each page", n)
	}
	if len(queries) != 2 {
		t.Fatalf("queries %v", queries)
	}
	for _, want := range []string{"/v3/projects/proj-a/timeSeries", "view=FULL", "interval.startTime=2026-11-18T11%3A45%3A00Z",
		"interval.endTime=2026-11-18T12%3A00%3A00Z", "filter=metric.type+%3D+%22compute.googleapis.com%2Finstance%2Ftpu%2Finfra_health%22"} {
		if !strings.Contains(queries[0], want) {
			t.Errorf("query %q lacks %q", queries[0], want)
		}
	}
	if !strings.Contains(queries[1], "pageToken=next") {
		t.Errorf("second query %q does not follow the page token", queries[1])
	}
}
