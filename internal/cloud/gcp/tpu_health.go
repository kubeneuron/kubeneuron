// SPDX-License-Identifier: Apache-2.0

package gcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"time"

	"google.golang.org/api/googleapi"
	monitoring "google.golang.org/api/monitoring/v3"

	"github.com/kubeneuron/kubeneuron/pkg/cloud"
)

// OptionTPUInfraHealth turns on the TPU health read from Cloud Monitoring
// when set to "true".
const OptionTPUInfraHealth = "tpuInfraHealth"

// The TPU health metric, BETA in Google Cloud: a gauge on gce_instance whose
// labels health_status (HEALTHY, UNHEALTHY, DEGRADED, UNKNOWN) and
// unhealthy_category (BAD_TPU_CHIP, NETWORKING_ISSUE, BAD_SC_CHIP, set only
// while UNHEALTHY) carry the state. Sampled every 60 s and visible up to 420 s
// later.
// https://docs.cloud.google.com/tpu/docs/troubleshooting/tpu-health and
// https://docs.cloud.google.com/monitoring/api/metrics_gcp_c (2026-11-18)
const (
	infraHealthMetric = "compute.googleapis.com/instance/tpu/infra_health"
	// infraHealthWindow is how far back a read asks for points: the
	// visibility delay, twice the sampling period and room for two points.
	infraHealthWindow = 15 * time.Minute
	// infraHealthMaxPages bounds one project's read. An answer longer than
	// this is incomplete, and an incomplete answer is a failed read.
	infraHealthMaxPages = 20
)

// monitoringAPI is the one Cloud Monitoring call this package makes.
type monitoringAPI interface {
	ListTimeSeries(ctx context.Context, project, filter string, start, end time.Time, pageToken string) (*monitoring.ListTimeSeriesResponse, error)
}

// restMonitoring is monitoringAPI over the Cloud Monitoring REST API.
type restMonitoring struct{ svc *monitoring.Service }

func (r restMonitoring) ListTimeSeries(ctx context.Context, project, filter string, start, end time.Time, pageToken string) (*monitoring.ListTimeSeriesResponse, error) {
	call := r.svc.Projects.TimeSeries.List("projects/" + project).
		Filter(filter).
		IntervalStartTime(start.UTC().Format(time.RFC3339)).
		IntervalEndTime(end.UTC().Format(time.RFC3339)).
		// FULL carries the instance's metadata, the only place its name is.
		View("FULL").
		Context(ctx)
	if pageToken != "" {
		call = call.PageToken(pageToken)
	}
	return call.Do()
}

// newSDKMonitoring is the Cloud Monitoring client, with Application Default
// Credentials. Without them every read fails with the cause.
func newSDKMonitoring(ctx context.Context) monitoringAPI {
	svc, err := monitoring.NewService(ctx)
	if err != nil {
		return unavailableMonitoring{fmt.Errorf("gcp: Cloud Monitoring client (are Application Default Credentials available to the controller pod?): %w", err)}
	}
	return restMonitoring{svc: svc}
}

// unavailableMonitoring stands in for the client when it could not be built.
type unavailableMonitoring struct{ err error }

func (u unavailableMonitoring) ListTimeSeries(context.Context, string, string, time.Time, time.Time, string) (*monitoring.ListTimeSeriesResponse, error) {
	return nil, u.err
}

var _ cloud.AcceleratorHealthReader = (*Provider)(nil)

// AcceleratorHealthEnabled reports whether spec.cloud.gcp turned the TPU
// health read on.
func (p *Provider) AcceleratorHealthEnabled() bool { return p.monitoring != nil }

// AcceleratorHealth reads the TPU health points of the instances, one
// Cloud Monitoring list per project. Of each series only the instance's zone
// and name, the two health labels and the points' times are read; the other
// metric and metadata labels are not kept, and the answer is never logged.
func (p *Provider) AcceleratorHealth(ctx context.Context, instanceIDs []string) (map[string][]cloud.HealthPoint, error) {
	if p.monitoring == nil {
		return nil, nil
	}
	wanted := map[string]map[string]bool{}
	for _, id := range instanceIDs {
		in, err := parseInstanceID(id)
		if err != nil {
			continue
		}
		if wanted[in.project] == nil {
			wanted[in.project] = map[string]bool{}
		}
		wanted[in.project][in.id()] = true
	}
	projects := make([]string, 0, len(wanted))
	for project := range wanted {
		projects = append(projects, project)
	}
	sort.Strings(projects)
	end := p.now()
	start := end.Add(-infraHealthWindow)
	filter := fmt.Sprintf(`metric.type = %q AND resource.type = "gce_instance"`, infraHealthMetric)
	out := map[string][]cloud.HealthPoint{}
	for _, project := range projects {
		token := ""
		for page := 0; ; page++ {
			if page == infraHealthMaxPages {
				return nil, &cloud.HealthReadError{Reason: cloud.HealthReadAPIError,
					Err: fmt.Errorf("gcp: Cloud Monitoring answered project %s with more than %d pages", project, infraHealthMaxPages)}
			}
			resp, err := p.monitoring.ListTimeSeries(ctx, project, filter, start, end, token)
			if err != nil {
				return nil, healthReadError(project, err)
			}
			for _, ts := range resp.TimeSeries {
				id, ok := seriesInstance(project, ts)
				if !ok || !wanted[project][id] {
					continue
				}
				out[id] = append(out[id], seriesPoints(ts)...)
			}
			if token = resp.NextPageToken; token == "" {
				break
			}
		}
	}
	for id, points := range out {
		sort.Slice(points, func(i, j int) bool { return points[i].At.Before(points[j].At) })
		out[id] = points
	}
	return out, nil
}

func healthReadError(project string, err error) error {
	reason := cloud.HealthReadAPIError
	var gerr *googleapi.Error
	if errors.As(err, &gerr) && (gerr.Code == http.StatusForbidden || gerr.Code == http.StatusUnauthorized) {
		reason = cloud.HealthReadPermissionDenied
	}
	return &cloud.HealthReadError{Reason: reason, Err: fmt.Errorf("gcp: Cloud Monitoring, project %s: %w", project, err)}
}

// seriesInstance is the instance ID of a gce_instance series: the zone from
// the resource labels and the name from the metadata's system labels.
func seriesInstance(project string, ts *monitoring.TimeSeries) (string, bool) {
	if ts.Resource == nil || ts.Resource.Type != "gce_instance" || ts.Metadata == nil || len(ts.Metadata.SystemLabels) == 0 {
		return "", false
	}
	var system struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(ts.Metadata.SystemLabels, &system); err != nil || system.Name == "" {
		return "", false
	}
	in := instance{project: project, zone: ts.Resource.Labels["zone"], name: system.Name}
	if _, err := parseInstanceID(in.id()); err != nil {
		return "", false
	}
	return in.id(), true
}

// seriesPoints are the series' points. Each health status is a series of its
// own: a point says the instance had that status at that time, and its value
// is not read.
func seriesPoints(ts *monitoring.TimeSeries) []cloud.HealthPoint {
	var status, category string
	if ts.Metric != nil {
		status = ts.Metric.Labels["health_status"]
		category = ts.Metric.Labels["unhealthy_category"]
	}
	var out []cloud.HealthPoint
	for _, pt := range ts.Points {
		if pt == nil || pt.Interval == nil {
			continue
		}
		at, err := time.Parse(time.RFC3339Nano, pt.Interval.EndTime)
		if err != nil {
			continue
		}
		out = append(out, cloud.HealthPoint{At: at, Status: status, Category: category})
	}
	return out
}
