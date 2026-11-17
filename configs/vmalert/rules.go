// SPDX-License-Identifier: Apache-2.0

// Package vmalert carries the rule files the operator ships to a managed
// vmalert.
package vmalert

import _ "embed"

// Recording is gpu-recording-rules.yaml: the canonical and derived
// kubeneuron_gpu_* series the dashboards read. It holds recording rules only.
//
//go:embed gpu-recording-rules.yaml
var Recording string
