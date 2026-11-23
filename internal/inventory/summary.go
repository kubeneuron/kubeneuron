// SPDX-License-Identifier: Apache-2.0
package inventory

import (
	"errors"
	"fmt"
	"slices"
	"strconv"
	"time"
)

const Interval = time.Minute
const Suffix = "-inventory-summary"

// FirstAlertAcceptedAt is the summary key the controller sets once, when it
// first accepts a metric alert. It is not part of the inventory schema, so
// republishing the inventory keeps it.
const FirstAlertAcceptedAt = "firstAlertAcceptedAt"

// schemaKeys are the keys Data may write; Merge replaces exactly these.
var schemaKeys = []string{"advertisedNodes", "schemaVersion", "observedAt", "reportingNodes", "gpuCount", "resetConfirmed", "diagnostic", "lastReportAt", "undeclaredEKSPool", "nvidiaNodes", "diagnosticNodes", "nodeLevelActions"}

const (
	ExitSMIMissing        = 78
	ExitDriverUnavailable = 79
)

func DiagnosticMessage(reason string) string {
	switch reason {
	case "SMIMissing":
		return "The managed agent could not find nvidia-smi in the configured host tools."
	case "DriverUnavailable":
		return "The managed agent found nvidia-smi but could not initialize the NVIDIA driver inventory."
	case "HostToolingNotConfigured":
		return "NVIDIA host tooling is unset for the managed agent."
	case "RuntimeDidNotExposeGPUs":
		return fmt.Sprintf(runtimeExposedNone, "")
	case "AgentRuntimeClassPodsNotCreated":
		return "No agent Pod could be created with spec.agent.runtimeClassName. Check that the RuntimeClass it names exists (kubectl get runtimeclass), or clear the field. Until then, on nodes without a running agent, faults that only the agent detects are not detected."
	case "AgentRuntimeClassNotStarting":
		return fmt.Sprintf(runtimeClassNotStarting, "some nodes")
	case "DeviceAccessDisabled":
		return "Agents do not inventory GPUs because spec.agent.deviceAccess is None. " + replacementCheck
	default:
		return ""
	}
}

const (
	runtimeClassNotStarting = "Agent Pods with spec.agent.runtimeClassName cannot start on %s. The usual cause is that the container runtime on those nodes has no handler for that RuntimeClass, for example on CPU nodes. Limit the agent to nodes that have the handler with spec.agent.nodeSelector, configure the handler, or clear the field. Until then faults that only the agent detects are not detected on those nodes."
	replacementCheck        = "Node replacement does not depend on this; after a replacement KubeNeuron checks node readiness and advertised capacity, not device health."
	runtimeExposedNone      = "Kubernetes advertises NVIDIA GPUs%s, but the agents see none. The usual cause is that the NVIDIA container runtime is not the default runtime on these nodes, so the devices were not exposed to the agent (see docs/operations.md). " + replacementCheck
)

// DiagnosticText is s's diagnostic as an operator reads it, with the number
// of nodes it is about where it names one.
func DiagnosticText(s Summary) string {
	if s.Diagnostic == "RuntimeDidNotExposeGPUs" && s.NVIDIANodes > 0 {
		return fmt.Sprintf(runtimeExposedNone, fmt.Sprintf(" on %d %s", s.NVIDIANodes, Plural(s.NVIDIANodes, "node", "nodes")))
	}
	if s.Diagnostic == "AgentRuntimeClassNotStarting" && s.DiagnosticNodes > 0 {
		return fmt.Sprintf(runtimeClassNotStarting, fmt.Sprintf("%d %s", s.DiagnosticNodes, Plural(s.DiagnosticNodes, "node", "nodes")))
	}
	return DiagnosticMessage(s.Diagnostic)
}

// AgentRuntimeClassDiagnostic is whether reason says agent Pods do not run
// with the RuntimeClass the owner named: true also while other agents report.
func AgentRuntimeClassDiagnostic(reason string) bool {
	return reason == "AgentRuntimeClassPodsNotCreated" || reason == "AgentRuntimeClassNotStarting"
}

// Plural selects the noun or phrase for a count.
func Plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// ActionMode is empty when the summary predates nodeLevelActions.
type ActionMode string

const (
	ActionsDisabled ActionMode = "false"
	ActionsEnabled  ActionMode = "true"
)

type Summary struct {
	UndeclaredEKSPool string
	Diagnostic        string
	AdvertisedNodes   int
	// NVIDIANodes are the nodes that advertise nvidia.com/gpu; 0 also when
	// the controller did not count them.
	NVIDIANodes int
	// DiagnosticNodes are the nodes the diagnostic is about, where it counts
	// them; 0 otherwise.
	DiagnosticNodes  int
	ObservedAt       time.Time
	LastReportAt     time.Time
	ReportingNodes   int
	GPUs             int
	NodeLevelActions ActionMode
	ResetConfirmed   bool
}

func (s Summary) Data() map[string]string {
	data := map[string]string{
		"advertisedNodes": strconv.Itoa(s.AdvertisedNodes),
		"schemaVersion":   "1",
		"observedAt":      s.ObservedAt.UTC().Format(time.RFC3339Nano),
		"reportingNodes":  strconv.Itoa(s.ReportingNodes),
		"gpuCount":        strconv.Itoa(s.GPUs),
		"resetConfirmed":  strconv.FormatBool(s.ResetConfirmed),
	}
	if s.NodeLevelActions != "" {
		data["nodeLevelActions"] = string(s.NodeLevelActions)
	}
	if s.UndeclaredEKSPool != "" {
		data["undeclaredEKSPool"] = s.UndeclaredEKSPool
	}
	if s.Diagnostic != "" {
		data["diagnostic"] = s.Diagnostic
	}
	if s.NVIDIANodes > 0 {
		data["nvidiaNodes"] = strconv.Itoa(s.NVIDIANodes)
	}
	if s.DiagnosticNodes > 0 {
		data["diagnosticNodes"] = strconv.Itoa(s.DiagnosticNodes)
	}
	if !s.LastReportAt.IsZero() {
		data["lastReportAt"] = s.LastReportAt.UTC().Format(time.RFC3339Nano)
	}
	return data
}

// Merge returns existing with the inventory keys replaced by s: a stale
// optional key is dropped and every key outside the schema is kept.
func Merge(existing map[string]string, s Summary) map[string]string {
	data := s.Data()
	for k, v := range existing {
		if !slices.Contains(schemaKeys, k) {
			data[k] = v
		}
	}
	return data
}

// FirstAlert returns when the controller first accepted a metric alert, if
// it has.
func FirstAlert(data map[string]string) (time.Time, bool) {
	at, err := time.Parse(time.RFC3339Nano, data[FirstAlertAcceptedAt])
	return at, err == nil && !at.IsZero()
}

func Parse(data map[string]string, now time.Time) (Summary, error) {
	invalid := errors.New("invalid inventory summary")
	s := Summary{UndeclaredEKSPool: data["undeclaredEKSPool"]}
	var err error
	s.Diagnostic = data["diagnostic"]
	if s.Diagnostic != "" && DiagnosticMessage(s.Diagnostic) == "" {
		return s, invalid
	}
	if data["schemaVersion"] != "1" {
		return s, invalid
	}
	s.ObservedAt, err = time.Parse(time.RFC3339Nano, data["observedAt"])
	if err != nil || s.ObservedAt.IsZero() || s.ObservedAt.After(now.Add(5*time.Second)) {
		return s, invalid
	}
	if last := data["lastReportAt"]; last != "" {
		s.LastReportAt, err = time.Parse(time.RFC3339Nano, last)
		if err != nil || s.LastReportAt.IsZero() || s.LastReportAt.After(s.ObservedAt) {
			return s, invalid
		}
	}
	advertised, err := strconv.ParseUint(data["advertisedNodes"], 10, 31)
	if err != nil {
		return s, invalid
	}
	s.AdvertisedNodes = int(advertised)
	if n := data["nvidiaNodes"]; n != "" {
		nvidia, err := strconv.ParseUint(n, 10, 31)
		if err != nil || int(nvidia) > s.AdvertisedNodes {
			return s, invalid
		}
		s.NVIDIANodes = int(nvidia)
	}
	if n := data["diagnosticNodes"]; n != "" {
		count, err := strconv.ParseUint(n, 10, 31)
		if err != nil || count == 0 {
			return s, invalid
		}
		s.DiagnosticNodes = int(count)
	}
	nodes, err := strconv.ParseUint(data["reportingNodes"], 10, 31)
	if err != nil {
		return s, invalid
	}
	gpus, err := strconv.ParseUint(data["gpuCount"], 10, 31)
	if err != nil || (nodes == 0 && gpus > 0) || (nodes > 0 && s.LastReportAt.IsZero()) {
		return s, invalid
	}
	s.ReportingNodes, s.GPUs = int(nodes), int(gpus)
	if raw, ok := data["nodeLevelActions"]; ok {
		s.NodeLevelActions = ActionMode(raw)
		if s.NodeLevelActions != ActionsEnabled && s.NodeLevelActions != ActionsDisabled {
			return s, invalid
		}
	}
	switch data["resetConfirmed"] {
	case "true":
		if gpus == 0 {
			return s, invalid
		}
		s.ResetConfirmed = true
	case "false":
	default:
		return s, invalid
	}
	return s, nil
}
