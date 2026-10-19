package gpucatalog

import (
	"encoding/csv"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// The shipped dcgm-exporter profile (ShippedProfilePath, the GPU Operator
// values file) is the reliability profile of v0.7: it lists exactly the
// catalog fields ProfileEligible admits, under the series name the pinned
// exporter uses for each field id. The profile is written by hand, not
// generated, so this file gives the tests one parser and one eligibility
// rule to pin the two against each other in both directions.
//
// Listing a series in the profile is an observability change only: it
// configures the exporter to emit the series and nothing else. No rule,
// detector threshold, playbook or controller behaviour reads a series
// because the profile lists it; ConsumedBy records readers separately.

// ProfileEligible reports whether a field belongs in the shipped exporter
// profile: a DCGM field (tier T0 or T1) that feeds a canonical series and is
// not deferred. The sticky last-XID gauge (230, no canonical series), the
// P-state (190, no canonical series) and the recovery action (1523,
// deferred) are therefore excluded by rule, not by list.
func ProfileEligible(f Field) bool {
	return (f.Tier == TierT0 || f.Tier == TierT1) && f.Canonical != nil && f.Deferred == ""
}

// ProfileFields returns the catalog fields the shipped profile must list,
// in catalog order.
func (c Catalog) ProfileFields() []Field {
	var out []Field
	for _, f := range c.Fields {
		if ProfileEligible(f) {
			out = append(out, f)
		}
	}
	return out
}

// LegacyProfileSeries is the nine-series profile v0.6.1 shipped, in the
// order that release listed them. It is the documented rollback target of
// the reliability profile and exactly the set of profiled series the shipped
// alert rules read; the tests pin both facts.
var LegacyProfileSeries = []string{
	"DCGM_FI_DEV_ECC_DBE_VOL_TOTAL",
	"DCGM_FI_DEV_ECC_DBE_AGG_TOTAL",
	"DCGM_FI_DEV_ROW_REMAP_PENDING",
	"DCGM_FI_DEV_ROW_REMAP_FAILURE",
	"DCGM_FI_DEV_UNCORRECTABLE_REMAPPED_ROWS",
	"DCGM_FI_DEV_NVLINK_CRC_FLIT_ERROR_COUNT_TOTAL",
	"DCGM_FI_DEV_PCIE_REPLAY_COUNTER",
	"DCGM_FI_DEV_CLOCK_THROTTLE_REASONS",
	"DCGM_FI_DEV_GPU_TEMP",
}

// ProfileRow is one data row of a dcgm-exporter metrics CSV: the series the
// exporter publishes, its Prometheus type and the help text.
type ProfileRow struct {
	Series string
	Kind   Kind
	Help   string
}

// ParseProfileCSV parses dcgm-exporter metrics-config text the way the
// pinned exporter (4.5.3-4.8.2, internal/pkg/counters) does: encoding/csv
// with '#' comment lines, every field trimmed of spaces, and exactly three
// fields per record. A help text containing a comma is therefore a parse
// error here, as it is in the exporter.
func ParseProfileCSV(text string) ([]ProfileRow, error) {
	r := csv.NewReader(strings.NewReader(text))
	r.Comment = '#'
	r.FieldsPerRecord = -1
	records, err := r.ReadAll()
	if err != nil {
		return nil, fmt.Errorf("metrics csv: %w", err)
	}
	var rows []ProfileRow
	for i, rec := range records {
		if len(rec) == 0 {
			continue
		}
		for j := range rec {
			rec[j] = strings.Trim(rec[j], " ")
		}
		if len(rec) != 3 {
			return nil, fmt.Errorf("metrics csv record %d (%q): expected 3 fields, got %d", i, rec, len(rec))
		}
		if rec[0] == "" || rec[1] == "" || rec[2] == "" {
			return nil, fmt.Errorf("metrics csv record %d (%q): empty field", i, rec)
		}
		rows = append(rows, ProfileRow{Series: rec[0], Kind: Kind(rec[1]), Help: rec[2]})
	}
	return rows, nil
}

// ShippedProfileConfigMapName is the ConfigMap the GPU Operator chart
// creates from dcgmExporter.config and points ClusterPolicy at.
const ShippedProfileConfigMapName = "kubeneuron-dcgm-metrics"

// ShippedProfileCSV reads ShippedProfilePath under root and returns the raw
// dcgmExporter.config.data text, after checking that the chart is told to
// create the ConfigMap under ShippedProfileConfigMapName.
func ShippedProfileCSV(root string) (string, error) {
	raw, err := os.ReadFile(filepath.Join(root, ShippedProfilePath))
	if err != nil {
		return "", err
	}
	var values struct {
		DCGMExporter struct {
			Config struct {
				Name   string `yaml:"name"`
				Create bool   `yaml:"create"`
				Data   string `yaml:"data"`
			} `yaml:"config"`
		} `yaml:"dcgmExporter"`
	}
	if err := yaml.Unmarshal(raw, &values); err != nil {
		return "", fmt.Errorf("%s: %w", ShippedProfilePath, err)
	}
	cfg := values.DCGMExporter.Config
	switch {
	case cfg.Name != ShippedProfileConfigMapName:
		return "", fmt.Errorf("%s: dcgmExporter.config.name is %q, want %q", ShippedProfilePath, cfg.Name, ShippedProfileConfigMapName)
	case !cfg.Create:
		return "", fmt.Errorf("%s: dcgmExporter.config.create must be true for the chart to create the ConfigMap", ShippedProfilePath)
	case strings.TrimSpace(cfg.Data) == "":
		return "", errors.New(ShippedProfilePath + ": dcgmExporter.config.data is empty")
	}
	return cfg.Data, nil
}

// ShippedProfile returns the parsed rows of the shipped exporter profile.
func ShippedProfile(root string) ([]ProfileRow, error) {
	text, err := ShippedProfileCSV(root)
	if err != nil {
		return nil, err
	}
	return ParseProfileCSV(text)
}
