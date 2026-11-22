// SPDX-License-Identifier: Apache-2.0
package inventory

import (
	"errors"
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
var schemaKeys = []string{"advertisedNodes", "schemaVersion", "observedAt", "reportingNodes", "gpuCount", "resetConfirmed", "diagnostic", "lastReportAt", "undeclaredEKSPool"}

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
	default:
		return ""
	}
}

type Summary struct {
	UndeclaredEKSPool string
	Diagnostic        string
	AdvertisedNodes   int
	ObservedAt        time.Time
	LastReportAt      time.Time
	ReportingNodes    int
	GPUs              int
	ResetConfirmed    bool
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
	if s.UndeclaredEKSPool != "" {
		data["undeclaredEKSPool"] = s.UndeclaredEKSPool
	}
	if s.Diagnostic != "" {
		data["diagnostic"] = s.Diagnostic
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
	nodes, err := strconv.ParseUint(data["reportingNodes"], 10, 31)
	if err != nil {
		return s, invalid
	}
	gpus, err := strconv.ParseUint(data["gpuCount"], 10, 31)
	if err != nil || (nodes == 0 && gpus > 0) || (nodes > 0 && s.LastReportAt.IsZero()) {
		return s, invalid
	}
	s.ReportingNodes, s.GPUs = int(nodes), int(gpus)
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
