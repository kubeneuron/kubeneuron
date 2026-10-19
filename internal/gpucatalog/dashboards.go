package gpucatalog

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
)

// Generated Grafana dashboard paths, relative to the repository root. The
// legacy deploy/grafana/kubeneuron-dashboard.json is hand-maintained and is
// deliberately not in this list.
const (
	PathDashboardFleet       = "deploy/grafana/fleet-overview.json"
	PathDashboardHealth      = "deploy/grafana/health-errors.json"
	PathDashboardFailSlow    = "deploy/grafana/fail-slow-radar.json"
	PathDashboardRemediation = "deploy/grafana/remediation.json"
	PathDashboardPassport    = "deploy/grafana/gpu-passport.json"
)

// Generated dashboard UIDs. The legacy board links to the remediation UID,
// so it is exported for the test that pins the link.
const (
	UIDDashboardFleet       = "kubeneuron-fleet-overview"
	UIDDashboardHealth      = "kubeneuron-health-errors"
	UIDDashboardFailSlow    = "kubeneuron-fail-slow-radar"
	UIDDashboardRemediation = "kubeneuron-remediation"
	UIDDashboardPassport    = "kubeneuron-gpu-passport"
)

// DashboardSchemaVersion is the Grafana dashboard JSON schema version the
// generator targets.
const DashboardSchemaVersion = 39

// DashboardDatasourceUID is the templated datasource reference every panel
// and variable uses; the operator binds it on import.
const DashboardDatasourceUID = "${DS_PROMETHEUS}"

// ShadowAlertLabel is the matcher every ALERTS read carries: shadow rules
// carry kubeneuron_shadow="true" and nothing else is read from ALERTS.
const ShadowAlertLabel = `kubeneuron_shadow="true"`

// DashboardVariables is the fixed template-variable list, in order, that
// every generated board carries. Each is All/multi with allValue ".*", so a
// board on a cluster that never sets the label (cluster, node_pool are
// admin-set; namespace and pod exist only on attributed series) still
// renders: the selector degrades to label=~".*", which matches an absent
// label.
var DashboardVariables = []string{"cluster", "node_pool", "gpu_model", "node", "gpu_uuid", "namespace", "pod"}

// Scopes are the subsets of DashboardVariables an expression may filter
// by, chosen by what the series actually carries:
//
//   - GPUScopeVariables: every variable. Canonical kubeneuron_gpu_* series
//     and the derived series that keep their input labels carry the
//     identity and placement labels of the exporter sample.
//   - DeviceScopeVariables: node, gpu_uuid, gpu_model only. The fail-slow
//     peer score deduplicates its inputs to exactly these three (the rule
//     drops cluster, node_pool, namespace and pod on purpose), and a shadow
//     alert built on it carries nothing more. Reading ALERTS by a label the
//     rule removed would hide every firing alert as soon as a variable is
//     narrowed, so the shadow panels filter by this scope only.
//   - ControlScopeVariables: cluster only. Tier T4 control-plane series
//     are controller counters labelled by class, state, outcome and the
//     like; they carry no gpu_uuid, gpu_model, node_pool, node or workload
//     label, and cluster is the admin-set external label.
var (
	GPUScopeVariables     = DashboardVariables
	DeviceScopeVariables  = []string{"gpu_model", "node", "gpu_uuid"}
	ControlScopeVariables = []string{"cluster"}
)

// variableSource is the series whose label values populate a variable. The
// query is label_values(<series>, <label>), which returns an empty list, not
// an error, when the label is absent. Hardware variables read the GPU
// temperature, a T0 series every supported device reports; workload
// variables read the utilization ratio, which carries the attribution
// labels when the exporter attributes samples to pods.
var variableSource = map[string]string{
	"cluster":   "kubeneuron_gpu_temperature_celsius",
	"node_pool": "kubeneuron_gpu_temperature_celsius",
	"gpu_model": "kubeneuron_gpu_temperature_celsius",
	"node":      "kubeneuron_gpu_temperature_celsius",
	"gpu_uuid":  "kubeneuron_gpu_temperature_celsius",
	"namespace": "kubeneuron_gpu_utilization_ratio",
	"pod":       "kubeneuron_gpu_utilization_ratio",
}

// DashboardSpec is the shared declarative description of one board; the
// renderer turns it into Grafana JSON. Panels are laid out top to bottom in
// a 24-column grid from their declared width and height.
type DashboardSpec struct {
	Path        string
	UID         string
	Title       string
	Description string
	Tags        []string
	Panels      []PanelSpec
}

// PanelSpec is one panel. Targets are PromQL/MetricsQL expressions built
// from the scope selectors above so every expression is filtered by the
// board's variables and no expression repeats a selector by hand.
type PanelSpec struct {
	Title       string
	Type        string // timeseries, stat, table, state-timeline
	Description string
	Unit        string
	Width       int
	Height      int
	Targets     []TargetSpec
}

// TargetSpec is one Prometheus target of a panel.
type TargetSpec struct {
	Expr    string
	Legend  string
	Instant bool
	Format  string // table for table panels; empty means time_series
}

// scope builds the label matchers for a variable subset, followed by any
// extra matchers. Each variable is regex-matched against its value, and
// "All" resolves to ".*", which also matches an absent label.
func scope(vars []string, extra ...string) string {
	parts := make([]string, 0, len(vars)+len(extra))
	for _, v := range vars {
		parts = append(parts, fmt.Sprintf(`%s=~"$%s"`, v, v))
	}
	parts = append(parts, extra...)
	return strings.Join(parts, ",")
}

// Selector helpers: the braces included, extra matchers first so a fixed
// label reads before the variable list.
func gpuSel(extra ...string) string     { return "{" + scope(GPUScopeVariables, extra...) + "}" }
func deviceSel(extra ...string) string  { return "{" + scope(DeviceScopeVariables, extra...) + "}" }
func controlSel(extra ...string) string { return "{" + scope(ControlScopeVariables, extra...) + "}" }

// shadowAlerts is the only ALERTS selector the boards use: the shadow label
// and any extra matchers (alertstate) sit inside the one brace pair with the
// device scope.
func shadowAlerts(extra ...string) string {
	return "ALERTS" + deviceSel(append([]string{ShadowAlertLabel}, extra...)...)
}

// DashboardSpecs returns the five board specifications. Expressions read only
// canonical kubeneuron_gpu_* series, recorded derived series, registered
// kubeneuron_* control-plane series and ALERTS with the shadow label; none
// uses `or vector(0)` and none aggregates by a label an installation may
// lack.
func DashboardSpecs() []DashboardSpec {
	sel := gpuSel()
	ctl := controlSel()
	return []DashboardSpec{
		{
			Path: PathDashboardFleet, UID: UIDDashboardFleet, Title: "KubeNeuron / Fleet Reliability Overview",
			Description: "Is the fleet healthy, how much capacity is at risk and how much was returned? Reads canonical GPU series and KubeNeuron control-plane counters; a missing series renders as No data, never as zero.",
			Tags:        []string{"kubeneuron", "gpu", "fleet"},
			Panels: []PanelSpec{
				{Title: "GPUs reporting", Type: "stat", Unit: "short", Width: 6, Height: 5,
					Description: "Devices with a GPU temperature sample (T0, reported by every supported device) in the selection. kubeneuron_gpu_present is deferred and never recorded, so a GPU that exists but reports nothing is not counted.",
					Targets:     []TargetSpec{{Expr: "count(kubeneuron_gpu_temperature_celsius" + sel + ")", Legend: "reporting", Instant: true}}},
				{Title: "GPUs under incident", Type: "stat", Unit: "short", Width: 6, Height: 5,
					Description: "kubeneuron_degraded_gpus summed over class and owner. Absent on a standby replica.",
					Targets:     []TargetSpec{{Expr: "sum(kubeneuron_degraded_gpus" + ctl + ")", Legend: "degraded", Instant: true}}},
				{Title: "Capacity returned to service (24h)", Type: "stat", Unit: "h", Width: 6, Height: 5,
					Description: "GPU-hours recovered from resolved incidents over the last day.",
					Targets:     []TargetSpec{{Expr: "sum(increase(kubeneuron_gpu_recovered_capacity_hours_total" + ctl + "[24h]))", Legend: "recovered", Instant: true}}},
				{Title: "Mean GPU utilization", Type: "stat", Unit: "percentunit", Width: 6, Height: 5,
					Description: "Average SM utilization ratio across the selection.",
					Targets:     []TargetSpec{{Expr: "avg(kubeneuron_gpu_utilization_ratio" + sel + ")", Legend: "utilization", Instant: true}}},
				{Title: "Degraded GPUs by class", Type: "timeseries", Unit: "short", Width: 12, Height: 8,
					Description: "Accelerators under a non-terminal incident, by problem class.",
					Targets:     []TargetSpec{{Expr: "sum by (class) (kubeneuron_degraded_gpus" + ctl + ")", Legend: "{{class}}"}}},
				{Title: "Recovered capacity by class", Type: "timeseries", Unit: "h", Width: 12, Height: 8,
					Description: "Cumulative GPU-hours returned to service from resolved incidents.",
					Targets:     []TargetSpec{{Expr: "sum by (class) (kubeneuron_gpu_recovered_capacity_hours_total" + ctl + ")", Legend: "{{class}}"}}},
				{Title: "Power draw by model", Type: "timeseries", Unit: "watt", Width: 12, Height: 8,
					Description: "Board power summed per gpu_model.",
					Targets:     []TargetSpec{{Expr: "sum by (gpu_model) (kubeneuron_gpu_power_watts" + sel + ")", Legend: "{{gpu_model}}"}}},
				{Title: "Utilization by model", Type: "timeseries", Unit: "percentunit", Width: 12, Height: 8,
					Description: "Mean SM utilization per gpu_model.",
					Targets:     []TargetSpec{{Expr: "avg by (gpu_model) (kubeneuron_gpu_utilization_ratio" + sel + ")", Legend: "{{gpu_model}}"}}},
			},
		},
		{
			Path: PathDashboardHealth, UID: UIDDashboardHealth, Title: "KubeNeuron / Health & Errors",
			Description: "What is broken, where, and since when? ECC, retired-page and row-remap signals come from the canonical catalog. XID is shown from KubeNeuron incidents (class xid-*), not from a DCGM-derived XID rate, which the catalog does not record.",
			Tags:        []string{"kubeneuron", "gpu", "health"},
			Panels: []PanelSpec{
				{Title: "ECC uncorrectable errors/s", Type: "timeseries", Unit: "short", Width: 12, Height: 8,
					Description: "Derived double-bit ECC error rate per device.",
					Targets:     []TargetSpec{{Expr: "max by (node, gpu_uuid) (kubeneuron_gpu_ecc_uncorrectable_errors_per_second" + sel + ")", Legend: "{{node}} {{gpu_uuid}}"}}},
				{Title: "ECC corrected errors/s", Type: "timeseries", Unit: "short", Width: 12, Height: 8,
					Description: "Derived single-bit ECC error rate per device.",
					Targets:     []TargetSpec{{Expr: "max by (node, gpu_uuid) (kubeneuron_gpu_ecc_corrected_errors_per_second" + sel + ")", Legend: "{{node}} {{gpu_uuid}}"}}},
				{Title: "Retired pages/s", Type: "timeseries", Unit: "short", Width: 8, Height: 8,
					Description: "Derived retired-page rate per device.",
					Targets:     []TargetSpec{{Expr: "max by (node, gpu_uuid) (kubeneuron_gpu_retired_pages_per_second" + sel + ")", Legend: "{{node}} {{gpu_uuid}}"}}},
				{Title: "Memory hard fault active", Type: "state-timeline", Unit: "bool", Width: 8, Height: 8,
					Description: "Derived hard-fault indicator (volatile/aggregate double-bit ECC, uncorrectable remaps, remap failed/pending, retired pages pending, unrepairable memory), by component.",
					Targets:     []TargetSpec{{Expr: "max by (node, gpu_uuid, component) (kubeneuron_gpu_memory_hard_fault_active" + sel + ")", Legend: "{{node}} {{gpu_uuid}} {{component}}"}}},
				{Title: "Row remap failed / pending", Type: "timeseries", Unit: "bool", Width: 8, Height: 8,
					Description: "Canonical row-remap flags per device.",
					Targets: []TargetSpec{
						{Expr: "max by (node, gpu_uuid) (kubeneuron_gpu_row_remap_failed" + sel + ")", Legend: "failed {{node}} {{gpu_uuid}}"},
						{Expr: "max by (node, gpu_uuid) (kubeneuron_gpu_row_remap_pending" + sel + ")", Legend: "pending {{node}} {{gpu_uuid}}"},
					}},
				{Title: "XID incidents opened (from KubeNeuron incidents, not a DCGM XID rate)", Type: "timeseries", Unit: "short", Width: 12, Height: 8,
					Description: "Incidents the agent opened from kmsg/DCGM XID observations, by class. The catalog records no XID event counter, so no XID rate is derived.",
					Targets:     []TargetSpec{{Expr: "sum by (class) (increase(kubeneuron_incidents_opened_total" + controlSel(`class=~"xid-.*"`) + "[1h]))", Legend: "{{class}}"}}},
				{Title: "NVLink / PCIe errors", Type: "timeseries", Unit: "short", Width: 12, Height: 8,
					Description: "Derived NVLink error and PCIe replay rates per device.",
					Targets: []TargetSpec{
						{Expr: "max by (node, gpu_uuid) (kubeneuron_gpu_nvlink_errors_per_second" + sel + ")", Legend: "nvlink {{node}} {{gpu_uuid}}"},
						{Expr: "max by (node, gpu_uuid) (kubeneuron_gpu_pcie_replays_per_second" + sel + ")", Legend: "pcie {{node}} {{gpu_uuid}}"},
					}},
			},
		},
		{
			Path: PathDashboardFailSlow, UID: UIDDashboardFailSlow, Title: "KubeNeuron / Fail-slow Radar",
			Description: "Which GPUs are quietly slower than their peers? Experimental and shadow-only: the peer score triggers nothing, and the shadow alerts go to humans, never to the controller. A device with no sample is not scored, not healthy.",
			Tags:        []string{"kubeneuron", "gpu", "fail-slow", "shadow"},
			Panels: []PanelSpec{
				{Title: "Suspect devices (shadow alerts firing)", Type: "stat", Unit: "short", Width: 6, Height: 5,
					Description: "Count of firing shadow alerts in the selection.",
					Targets:     []TargetSpec{{Expr: "count(" + shadowAlerts(`alertstate="firing"`) + ")", Legend: "firing", Instant: true}}},
				{Title: "Devices with a peer score", Type: "stat", Unit: "short", Width: 6, Height: 5,
					Description: "Devices whose fail-slow peer score has a sample (all three gates hold).",
					Targets:     []TargetSpec{{Expr: "count(kubeneuron_gpu_fail_slow_score_ratio" + sel + ")", Legend: "scored", Instant: true}}},
				{Title: "Worst peer score", Type: "stat", Unit: "percentunit", Width: 12, Height: 5,
					Description: "Largest fail-slow peer score in the selection.",
					Targets:     []TargetSpec{{Expr: "max(kubeneuron_gpu_fail_slow_score_ratio" + sel + ")", Legend: "max", Instant: true}}},
				{Title: "Fail-slow peer score by device", Type: "timeseries", Unit: "percentunit", Width: 12, Height: 9,
					Description: "1 - own SM clock ratio / gpu_model peer median; present only while the device is also throttled and has at least 3 peers.",
					Targets:     []TargetSpec{{Expr: "kubeneuron_gpu_fail_slow_score_ratio" + sel, Legend: "{{node}} {{gpu_uuid}} ({{gpu_model}})"}}},
				{Title: "Shadow alerts", Type: "state-timeline", Unit: "none", Width: 12, Height: 9,
					Description: "vmalert ALERTS state for shadow rules (kubeneuron_shadow=\"true\"), by alert name and device.",
					Targets:     []TargetSpec{{Expr: "max by (alertname, alertstate, node, gpu_uuid) (" + shadowAlerts() + ")", Legend: "{{alertname}} {{alertstate}} {{node}} {{gpu_uuid}}"}}},
				{Title: "SM clock ratio by device", Type: "timeseries", Unit: "percentunit", Width: 12, Height: 8,
					Description: "Derived SM clock / max clock; the peer score's input.",
					Targets:     []TargetSpec{{Expr: "max by (node, gpu_uuid) (kubeneuron_gpu_clock_ratio" + gpuSel(`domain="sm"`) + ")", Legend: "{{node}} {{gpu_uuid}}"}}},
				{Title: "Throttle share by reason", Type: "timeseries", Unit: "percentunit", Width: 12, Height: 8,
					Description: "Derived share of time throttled over 5m, by reason.",
					Targets:     []TargetSpec{{Expr: "max by (node, gpu_uuid, reason) (kubeneuron_gpu_throttle_share_ratio" + sel + ")", Legend: "{{reason}} {{node}} {{gpu_uuid}}"}}},
			},
		},
		{
			Path: PathDashboardRemediation, UID: UIDDashboardRemediation, Title: "KubeNeuron / Remediation & Recovered Capacity",
			Description: "What did KubeNeuron do, did it work, and what came back? Control-plane counters plus the shadow alert panel; the legacy kubeneuron-dashboard.json keeps its own copies of the incident panels.",
			Tags:        []string{"kubeneuron", "remediation"},
			Panels: []PanelSpec{
				{Title: "Incidents opened (1h)", Type: "stat", Unit: "short", Width: 6, Height: 5,
					Targets: []TargetSpec{{Expr: "sum(increase(kubeneuron_incidents_opened_total" + ctl + "[1h]))", Legend: "opened", Instant: true}}},
				{Title: "Incidents recovered (1h)", Type: "stat", Unit: "short", Width: 6, Height: 5,
					Targets: []TargetSpec{{Expr: "sum(increase(kubeneuron_incidents_recovered_total" + ctl + "[1h]))", Legend: "recovered", Instant: true}}},
				{Title: "Actions pending", Type: "stat", Unit: "short", Width: 6, Height: 5,
					Description: "Dispatched agent actions not yet claimed; sustained non-zero means an unreachable agent.",
					Targets:     []TargetSpec{{Expr: "sum(kubeneuron_actions_pending" + ctl + ")", Legend: "pending", Instant: true}}},
				{Title: "Capacity returned (24h)", Type: "stat", Unit: "h", Width: 6, Height: 5,
					Targets: []TargetSpec{{Expr: "sum(increase(kubeneuron_gpu_recovered_capacity_hours_total" + ctl + "[24h]))", Legend: "recovered", Instant: true}}},
				{Title: "Incidents by state", Type: "timeseries", Unit: "short", Width: 12, Height: 8,
					Targets: []TargetSpec{{Expr: "sum by (state) (kubeneuron_incidents" + ctl + ")", Legend: "{{state}}"}}},
				{Title: "Playbook steps by outcome", Type: "timeseries", Unit: "short", Width: 12, Height: 8,
					Targets: []TargetSpec{{Expr: "sum by (outcome) (rate(kubeneuron_steps_total" + ctl + "[5m]))", Legend: "{{outcome}}"}}},
				{Title: "Recovered vs needed a human", Type: "timeseries", Unit: "short", Width: 12, Height: 8,
					Targets: []TargetSpec{{Expr: "sum by (class, unattended) (increase(kubeneuron_incidents_recovered_total" + ctl + "[1h]))", Legend: "{{class}} unattended={{unattended}}"}}},
				{Title: "Gate denials and deferred destructive steps", Type: "timeseries", Unit: "short", Width: 12, Height: 8,
					Targets: []TargetSpec{
						{Expr: "sum(rate(kubeneuron_gate_denials_total" + ctl + "[5m]))", Legend: "denied"},
						{Expr: "sum by (reason) (rate(kubeneuron_destructive_steps_deferred_total" + ctl + "[5m]))", Legend: "deferred {{reason}}"},
					}},
				{Title: "Shadow alerts (would have fired)", Type: "table", Unit: "none", Width: 24, Height: 9,
					Description: "vmalert ALERTS carrying kubeneuron_shadow=\"true\". These rules route to humans only and drive no remediation.",
					Targets:     []TargetSpec{{Expr: shadowAlerts(), Legend: "", Instant: true, Format: "table"}}},
			},
		},
		{
			Path: PathDashboardPassport, UID: UIDDashboardPassport, Title: "KubeNeuron / GPU Passport",
			Description: "The full history of one GPU: pick a gpu_uuid. Every panel reads canonical or derived catalog series; a field the device does not report is No data.",
			Tags:        []string{"kubeneuron", "gpu", "passport"},
			Panels: []PanelSpec{
				{Title: "Temperature", Type: "timeseries", Unit: "celsius", Width: 12, Height: 8,
					Targets: []TargetSpec{
						{Expr: "max by (gpu_uuid) (kubeneuron_gpu_temperature_celsius" + sel + ")", Legend: "gpu {{gpu_uuid}}"},
						{Expr: "max by (gpu_uuid) (kubeneuron_gpu_memory_temperature_celsius" + sel + ")", Legend: "memory {{gpu_uuid}}"},
					}},
				{Title: "Thermal headroom", Type: "timeseries", Unit: "celsius", Width: 12, Height: 8,
					Description: "Derived margin to the slowdown threshold for the die and the memory.",
					Targets: []TargetSpec{
						{Expr: "max by (gpu_uuid) (kubeneuron_gpu_thermal_headroom_celsius" + sel + ")", Legend: "gpu {{gpu_uuid}}"},
						{Expr: "max by (gpu_uuid) (kubeneuron_gpu_memory_thermal_headroom_celsius" + sel + ")", Legend: "memory {{gpu_uuid}}"},
					}},
				{Title: "Power", Type: "timeseries", Unit: "watt", Width: 12, Height: 8,
					Targets: []TargetSpec{
						{Expr: "max by (gpu_uuid) (kubeneuron_gpu_power_watts" + sel + ")", Legend: "draw {{gpu_uuid}}"},
						{Expr: "max by (gpu_uuid) (kubeneuron_gpu_power_limit_watts" + sel + ")", Legend: "limit {{gpu_uuid}}"},
					}},
				{Title: "Power ratio", Type: "timeseries", Unit: "percentunit", Width: 12, Height: 8,
					Targets: []TargetSpec{{Expr: "max by (gpu_uuid) (kubeneuron_gpu_power_ratio" + sel + ")", Legend: "{{gpu_uuid}}"}}},
				{Title: "Memory used", Type: "timeseries", Unit: "bytes", Width: 12, Height: 8,
					Targets: []TargetSpec{
						{Expr: "max by (gpu_uuid) (kubeneuron_gpu_memory_used_bytes" + sel + ")", Legend: "used {{gpu_uuid}}"},
						{Expr: "max by (gpu_uuid) (kubeneuron_gpu_memory_total_bytes" + sel + ")", Legend: "total {{gpu_uuid}}"},
					}},
				{Title: "Memory used ratio", Type: "timeseries", Unit: "percentunit", Width: 12, Height: 8,
					Targets: []TargetSpec{{Expr: "max by (gpu_uuid) (kubeneuron_gpu_memory_used_ratio" + sel + ")", Legend: "{{gpu_uuid}}"}}},
				{Title: "Clocks", Type: "timeseries", Unit: "hertz", Width: 12, Height: 8,
					Targets: []TargetSpec{
						{Expr: "max by (gpu_uuid, domain) (kubeneuron_gpu_clock_hertz" + sel + ")", Legend: "{{domain}} {{gpu_uuid}}"},
						{Expr: "max by (gpu_uuid, domain) (kubeneuron_gpu_clock_max_hertz" + sel + ")", Legend: "{{domain}} max {{gpu_uuid}}"},
					}},
				{Title: "Clock ratio and utilization", Type: "timeseries", Unit: "percentunit", Width: 12, Height: 8,
					Targets: []TargetSpec{
						{Expr: "max by (gpu_uuid, domain) (kubeneuron_gpu_clock_ratio" + sel + ")", Legend: "clock {{domain}} {{gpu_uuid}}"},
						{Expr: "max by (gpu_uuid) (kubeneuron_gpu_utilization_ratio" + sel + ")", Legend: "utilization {{gpu_uuid}}"},
					}},
			},
		},
	}
}

// Grafana JSON shape. Field order inside each struct is the encoder's output
// order, so the generated files are stable across runs.
type grafanaDashboard struct {
	Title         string            `json:"title"`
	UID           string            `json:"uid"`
	Description   string            `json:"description"`
	SchemaVersion int               `json:"schemaVersion"`
	Version       int               `json:"version"`
	Editable      bool              `json:"editable"`
	Tags          []string          `json:"tags"`
	Time          grafanaTime       `json:"time"`
	Refresh       string            `json:"refresh"`
	Templating    grafanaTemplating `json:"templating"`
	Panels        []grafanaPanel    `json:"panels"`
}

type grafanaTime struct {
	From string `json:"from"`
	To   string `json:"to"`
}

type grafanaTemplating struct {
	List []grafanaVariable `json:"list"`
}

type grafanaVariable struct {
	Name       string            `json:"name"`
	Label      string            `json:"label"`
	Type       string            `json:"type"`
	Datasource grafanaDatasource `json:"datasource"`
	Query      grafanaVarQuery   `json:"query"`
	Definition string            `json:"definition"`
	Refresh    int               `json:"refresh"`
	IncludeAll bool              `json:"includeAll"`
	Multi      bool              `json:"multi"`
	AllValue   string            `json:"allValue"`
	Current    grafanaVarCurrent `json:"current"`
	Sort       int               `json:"sort"`
}

type grafanaVarQuery struct {
	Query string `json:"query"`
	RefID string `json:"refId"`
}

type grafanaVarCurrent struct {
	Selected bool     `json:"selected"`
	Text     []string `json:"text"`
	Value    []string `json:"value"`
}

type grafanaDatasource struct {
	Type string `json:"type"`
	UID  string `json:"uid"`
}

type grafanaPanel struct {
	ID          int                `json:"id"`
	Title       string             `json:"title"`
	Type        string             `json:"type"`
	Description string             `json:"description,omitempty"`
	Datasource  grafanaDatasource  `json:"datasource"`
	GridPos     grafanaGridPos     `json:"gridPos"`
	FieldConfig grafanaFieldConfig `json:"fieldConfig"`
	Targets     []grafanaTarget    `json:"targets"`
}

type grafanaGridPos struct {
	H int `json:"h"`
	W int `json:"w"`
	X int `json:"x"`
	Y int `json:"y"`
}

type grafanaFieldConfig struct {
	Defaults  grafanaFieldDefaults `json:"defaults"`
	Overrides []struct{}           `json:"overrides"`
}

type grafanaFieldDefaults struct {
	Unit    string `json:"unit"`
	NoValue string `json:"noValue"`
}

type grafanaTarget struct {
	RefID        string            `json:"refId"`
	Datasource   grafanaDatasource `json:"datasource"`
	Expr         string            `json:"expr"`
	LegendFormat string            `json:"legendFormat"`
	Instant      bool              `json:"instant"`
	Range        bool              `json:"range"`
	Format       string            `json:"format"`
}

func renderDashboard(s DashboardSpec) ([]byte, error) {
	ds := grafanaDatasource{Type: "prometheus", UID: DashboardDatasourceUID}
	d := grafanaDashboard{
		Title:         s.Title,
		UID:           s.UID,
		Description:   header + ". " + s.Description,
		SchemaVersion: DashboardSchemaVersion,
		Version:       1,
		Editable:      true,
		Tags:          s.Tags,
		Time:          grafanaTime{From: "now-6h", To: "now"},
		Refresh:       "1m",
	}
	for _, v := range DashboardVariables {
		q := fmt.Sprintf("label_values(%s, %s)", variableSource[v], v)
		d.Templating.List = append(d.Templating.List, grafanaVariable{
			Name: v, Label: v, Type: "query", Datasource: ds,
			Query:      grafanaVarQuery{Query: q, RefID: "StandardVariableQuery"},
			Definition: q, Refresh: 2, IncludeAll: true, Multi: true, AllValue: ".*",
			Current: grafanaVarCurrent{Selected: true, Text: []string{"All"}, Value: []string{"$__all"}},
			Sort:    1,
		})
	}
	x, y, rowH, id := 0, 0, 0, 0
	for _, p := range s.Panels {
		if x+p.Width > 24 {
			x, y, rowH = 0, y+rowH, 0
		}
		id++
		gp := grafanaPanel{
			ID: id, Title: p.Title, Type: p.Type, Description: p.Description, Datasource: ds,
			GridPos:     grafanaGridPos{H: p.Height, W: p.Width, X: x, Y: y},
			FieldConfig: grafanaFieldConfig{Defaults: grafanaFieldDefaults{Unit: p.Unit, NoValue: "No data"}, Overrides: []struct{}{}},
		}
		for i, t := range p.Targets {
			format := t.Format
			if format == "" {
				format = "time_series"
			}
			gp.Targets = append(gp.Targets, grafanaTarget{
				RefID: string(rune('A' + i)), Datasource: ds, Expr: t.Expr, LegendFormat: t.Legend,
				Instant: t.Instant, Range: !t.Instant, Format: format,
			})
		}
		d.Panels = append(d.Panels, gp)
		x += p.Width
		if p.Height > rowH {
			rowH = p.Height
		}
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(d); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// renderDashboards renders every board keyed by path.
func renderDashboards() (map[string][]byte, error) {
	out := map[string][]byte{}
	for _, s := range DashboardSpecs() {
		b, err := renderDashboard(s)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", s.Path, err)
		}
		out[s.Path] = b
	}
	return out, nil
}

// DashboardPaths returns the generated dashboard paths in spec order.
func DashboardPaths() []string {
	var p []string
	for _, s := range DashboardSpecs() {
		p = append(p, s.Path)
	}
	return p
}
