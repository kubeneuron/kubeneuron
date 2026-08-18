package detect

import (
	"net"
	"strconv"
	"time"

	"github.com/kubeneuron/kubeneuron/pkg/types"
)

// AlertmanagerWebhook is the payload Alertmanager POSTs to
// /api/v1/webhooks/alertmanager (webhook receiver format, version "4").
type AlertmanagerWebhook struct {
	Version           string            `json:"version"`
	GroupKey          string            `json:"groupKey"`
	Status            string            `json:"status"` // "firing" | "resolved"
	Receiver          string            `json:"receiver"`
	GroupLabels       map[string]string `json:"groupLabels"`
	CommonLabels      map[string]string `json:"commonLabels"`
	CommonAnnotations map[string]string `json:"commonAnnotations"`
	Alerts            []Alert           `json:"alerts"`
}

// Alert is a single alert inside an Alertmanager webhook payload.
type Alert struct {
	Status       string            `json:"status"`
	Labels       map[string]string `json:"labels"`
	Annotations  map[string]string `json:"annotations"`
	StartsAt     time.Time         `json:"startsAt"`
	EndsAt       time.Time         `json:"endsAt"`
	Fingerprint  string            `json:"fingerprint"`
	GeneratorURL string            `json:"generatorURL"`
}

// alertClassMap maps vmalert rule names (configs/vmalert/gpu-rules.yaml) to
// problem classes. Rules and this map must stay in sync; the loader test
// cross-checks them.
var alertClassMap = map[string]types.ProblemClass{
	"GpuDoubleBitEccVolatile":  types.ClassECCDBE,
	"GpuDoubleBitEccAggregate": types.ClassECCDBE,
	"GpuRowRemapPending":       types.ClassRowRemapOK,
	"GpuRowRemapFailure":       types.ClassRowRemapFailure,
	"GpuRemappedRowsHigh":      types.ClassRowRemapBudget,
	"GpuThermalThrottle":       types.ClassThermal,
	"GpuTempCritical":          types.ClassThermal,
	"GpuPowerBrake":            types.ClassPower,
	"GpuNvlinkCrcErrors":       types.ClassNVLink,
	"GpuPcieReplayHigh":        types.ClassPCIe,
	"GpuExporterDown":          types.ClassDriverHang,
	"KubeNeuronAgentDown":      types.ClassAgentDown,
	// GpuLost/GpuDriverHang/GpuDcgmDiagFailed return together with the agent
	// series that back them; classes ClassGPULost/ClassDriverHang/
	// ClassDiagFailure stay reachable through the XID paths meanwhile.
}

// AlertClasses returns every problem class the metric-alert path can produce.
// It is the third detection source beside XIDTable and FaultTable, and it
// exists so a coverage audit can ask "what can this build emit?" without
// hard-coding a list that goes stale the next time a rule is mapped.
func AlertClasses() []types.ProblemClass {
	out := make([]types.ProblemClass, 0, len(alertClassMap))
	seen := make(map[types.ProblemClass]bool, len(alertClassMap))
	for _, class := range alertClassMap {
		if !seen[class] {
			seen[class] = true
			out = append(out, class)
		}
	}
	return out
}

// SignalFromAlert converts a firing alert into a Signal. Resolved alerts and
// unknown alert names return ok=false (unknown names are still worth logging
// by the caller — they usually mean rules and code drifted apart).
func SignalFromAlert(a Alert) (types.Signal, bool) {
	if a.Status != "firing" {
		return types.Signal{}, false
	}
	class, ok := alertClassMap[a.Labels["alertname"]]
	if !ok {
		return types.Signal{}, false
	}
	return signalFromAlertWithClass(a, class, types.SeverityWarning)
}

// signalFromAlertWithClass builds the signal once the class is decided; the
// alert's own severity label wins over the supplied default.
func signalFromAlertWithClass(a Alert, class types.ProblemClass, defaultSeverity types.Severity) (types.Signal, bool) {
	name := a.Labels["alertname"]

	// The alert's own severity label may override the default, but only when it
	// names a severity we actually know. An unknown or spoofed value (e.g. a
	// hand-crafted "emergency" posted at the webhook) must not become an
	// authoritative severity that could drive escalation: fall back to the
	// validated default instead of trusting arbitrary caller-supplied text.
	sev := defaultSeverity
	if s := a.Labels["severity"]; s != "" {
		if candidate := types.Severity(s); validSeverity(candidate) {
			sev = candidate
		}
	}

	gpuIndex := 0
	if s := a.Labels["gpu"]; s != "" {
		if n, err := strconv.Atoi(s); err == nil {
			gpuIndex = n
		}
	}

	evidence := map[string]string{"alertname": name}
	for k, v := range a.Annotations {
		evidence[k] = v
	}

	return types.Signal{
		Target: types.Target{
			// dcgm-exporter labels: Hostname/node via relabeling, UUID, gpu
			// index. The instance label is host:port; normalize it to a bare
			// host so alert-path targets match agent-registered node names —
			// otherwise the two ingestion paths open two incidents for one
			// node.
			Node:     firstNonEmpty(a.Labels["node"], a.Labels["Hostname"], hostFromInstance(a.Labels["instance"])),
			GPUUUID:  a.Labels["UUID"],
			GPUIndex: gpuIndex,
		},
		Class:      class,
		Severity:   sev,
		Source:     types.SourceAlertmanager,
		Evidence:   evidence,
		ObservedAt: a.StartsAt,
	}, true
}

// validSeverity reports whether s is one of the severities the pipeline
// recognises. It mirrors the set validated for declarative overrides, so an
// alert label can never smuggle in a severity the config plane would reject.
func validSeverity(s types.Severity) bool {
	switch s {
	case types.SeverityInfo, types.SeverityWarning, types.SeverityCritical:
		return true
	default:
		return false
	}
}

// KnownAlertNames returns the alert names this package can map, for
// cross-checking against the shipped vmalert rules.
func KnownAlertNames() []string {
	out := make([]string, 0, len(alertClassMap))
	for k := range alertClassMap {
		out = append(out, k)
	}
	return out
}

// hostFromInstance strips the port from a Prometheus instance label
// (host:port). Bare hosts pass through unchanged.
func hostFromInstance(instance string) string {
	if host, _, err := net.SplitHostPort(instance); err == nil {
		return host
	}
	return instance
}

func firstNonEmpty(ss ...string) string {
	for _, s := range ss {
		if s != "" {
			return s
		}
	}
	return ""
}
