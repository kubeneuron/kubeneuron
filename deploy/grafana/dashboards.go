// SPDX-License-Identifier: Apache-2.0

// Package grafana carries the dashboard files a Managed Grafana provisions.
// They are KubeNeuron's own: five generated from the metric catalog and
// kubeneuron-dashboard.json, maintained by hand.
package grafana

import "embed"

// DatasourcePlaceholder is the datasource uid every dashboard names; the
// provisioning that loads a board binds it to a real datasource.
const DatasourcePlaceholder = "${DS_PROMETHEUS}"

// Files holds every dashboard, by file name.
//
//go:embed *.json
var Files embed.FS
