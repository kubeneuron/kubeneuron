// SPDX-License-Identifier: Apache-2.0

package nodehealth

import (
	"fmt"
	"time"

	"github.com/kubeneuron/kubeneuron/pkg/types"
)

// RuleGCPInfraHealth reads the TPU health metric of Cloud Monitoring,
// compute.googleapis.com/instance/tpu/infra_health (BETA in Google Cloud),
// for a GKE TPU node's instance. The values are those of
// https://docs.cloud.google.com/tpu/docs/troubleshooting/tpu-health and
// https://docs.cloud.google.com/monitoring/api/metrics_gcp_c (2026-11-18).
// The metric names no chip: a fault is the node's.
const RuleGCPInfraHealth = "gcp-infra-health"

const (
	// cloudFreshness is how old the newest point may be: the metric is
	// sampled every 60 s and visible up to 420 s later; ten minutes allows
	// one more sample.
	cloudFreshness = 10 * time.Minute
	// cloudConfirmPoints is how many consecutive points must report a fault
	// before it counts: two samples, not two reads of one sample.
	cloudConfirmPoints = 2
)

// infraHealthFaults are the unhealthy categories that blame the node's own
// hardware.
var infraHealthFaults = map[string]bool{"BAD_TPU_CHIP": true, "BAD_SC_CHIP": true}

// EvaluateCloud reads what the cloud's monitoring reports about one TPU
// node's instance: one observation of the google-tpu profile. A point older
// than the Node is the instance's predecessor of the same name and is not
// read. No point, a stale point, a failed read or a status or category the
// table does not list is unknown, never healthy. Only a fresh HEALTHY point
// is healthy.
func EvaluateCloud(snap types.NodeHealthSnapshot) NodeHealth {
	const profile = "google-tpu"
	o := Observation{Profile: profile, Rule: RuleGCPInfraHealth, Key: RuleGCPInfraHealth, Reads: 1}
	var points []types.CloudHealthPoint
	for _, pt := range snap.Cloud {
		if snap.CreatedAt.IsZero() || pt.At.After(snap.CreatedAt) {
			points = append(points, pt)
		}
	}
	switch {
	case snap.CloudError != "":
		o.State, o.Why = StateUnknown, "the Cloud Monitoring read failed: "+snap.CloudError
	case len(points) == 0:
		o.State, o.Why = StateUnknown, "no-series: Cloud Monitoring holds no recent infra_health point for the node's instance"
	case snap.ReadAt.Sub(points[len(points)-1].At) > cloudFreshness:
		latest := points[len(points)-1]
		o.State = StateUnknown
		o.Why = fmt.Sprintf("stale: the newest infra_health point is %s old, more than %s",
			snap.ReadAt.Sub(latest.At).Truncate(time.Second), cloudFreshness)
	default:
		evaluateSamples(&o, samples(points))
	}
	out := NodeHealth{Node: snap.Node, Profiles: []string{profile}, State: StateHealthy, Observations: []Observation{o}}
	switch o.State {
	case StateFault, StateUnknown:
		out.State = o.State
	}
	out.Slice = multiHostSlice(snap)
	return out
}

func evaluateSamples(o *Observation, s []sample) {
	latest := s[len(s)-1]
	o.Reason = latest.Status
	if latest.conflict {
		o.State, o.Why = StateUnknown, "Cloud Monitoring reports two health statuses at the newest sample time"
		return
	}
	switch latest.Status {
	case "HEALTHY":
		o.State = StateHealthy
	case "DEGRADED":
		o.State, o.Reason = StateIgnored, "degraded"
		o.Why = "the instance is degraded: performance, not a fault of the node"
	case "UNHEALTHY":
		switch {
		case latest.Category == "NETWORKING_ISSUE":
			o.State, o.Reason = StateIgnored, "networking-issue"
			o.Why = "a networking issue does not place the fault on this node"
		case infraHealthFaults[latest.Category]:
			o.Reason = latest.Category
			n, since := sameRun(s)
			if n < cloudConfirmPoints {
				o.State = StateUnknown
				o.Why = fmt.Sprintf("%s on %d sample; a fault needs %d in a row", latest.Category, n, cloudConfirmPoints)
				return
			}
			o.State, o.Class = StateFault, types.ClassAcceleratorFault
			o.Key += "=UNHEALTHY/" + latest.Category
			o.Since = since
		default:
			o.State = StateUnknown
			o.Why = fmt.Sprintf("unhealthy category %q is not in profile table %s", latest.Category, Version)
		}
	default:
		o.State = StateUnknown
		o.Why = fmt.Sprintf("health status %q", latest.Status)
	}
}

// sample is the status at one sample time. conflict is set when the cloud
// reports two statuses at that time.
type sample struct {
	types.CloudHealthPoint
	conflict bool
}

// samples collapses points, oldest first, to one per sample time.
func samples(points []types.CloudHealthPoint) []sample {
	var out []sample
	for _, pt := range points {
		if n := len(out); n > 0 && out[n-1].At.Equal(pt.At) {
			if out[n-1].Status != pt.Status || out[n-1].Category != pt.Category {
				out[n-1].conflict = true
			}
			continue
		}
		out = append(out, sample{CloudHealthPoint: pt})
	}
	return out
}

// sameRun is how many of the newest samples, back from the newest, report
// the newest one's status and category, and when the first of them was.
func sameRun(s []sample) (int, time.Time) {
	latest := s[len(s)-1]
	n, since := 0, latest.At
	for i := len(s) - 1; i >= 0; i-- {
		if s[i].conflict || s[i].Status != latest.Status || s[i].Category != latest.Category {
			break
		}
		n, since = n+1, s[i].At
	}
	return n, since
}
