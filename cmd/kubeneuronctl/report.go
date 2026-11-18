// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/kubeneuron/kubeneuron/pkg/types"
)

// cmdReport prints the capacity-owner view of a window: how much accelerator
// capacity was degraded, how much came back, how much came back without a
// human, which classes cost the most, and what is still open.
//
// The numbers come from the controller's incident store (GET
// /api/v1/report/recovery), not from Prometheus. The store is the ground
// truth the outcome metrics are derived from, so the report is exact,
// survives a metrics counter reset, and works on a fresh install with no
// monitoring stack attached — and the server does the aggregation, so the
// definition of "recovered" lives in one place rather than in every client.
// The Prometheus series remain the right source for the dashboard, where the
// question is a shape over time rather than a total.
func cmdReport() *cobra.Command {
	c := &cobra.Command{
		Use:   "report",
		Short: "Show degraded GPU-hours, recovery times and optional savings estimates",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cl, err := newClient(cmd)
			if err != nil {
				return err
			}
			since, _ := cmd.Flags().GetString("since")
			window, err := parseWindow(since)
			if err != nil {
				return err
			}
			// The server anchors the window to its own clock — the one that
			// stamped the incident rows — so a skewed workstation clock
			// cannot shift the boundary. It echoes the resolved bounds back.
			var report types.RecoveryReport
			if err := cl.do("GET", "/api/v1/report/recovery?window="+window.String(), nil, &report); err != nil {
				return err
			}
			if asJSON, _ := cmd.Flags().GetBool("json"); asJSON {
				enc := json.NewEncoder(cmd.OutOrStdout())
				enc.SetIndent("", "  ")
				return enc.Encode(report)
			}
			return printRecoveryReport(cmd, &report, strings.TrimSpace(since))
		},
	}
	c.Flags().String("since", "7d", "window length, e.g. 24h, 7d, 30d")
	c.Flags().Bool("json", false, "emit the report as JSON")
	return c
}

// parseWindow accepts Go durations plus the day and week suffixes an operator
// actually types. time.ParseDuration stops at hours, and "--since 720h" is a
// month nobody wants to compute in their head.
func parseWindow(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, fmt.Errorf("--since is empty")
	}
	unit := time.Duration(0)
	switch s[len(s)-1] {
	case 'd':
		unit = 24 * time.Hour
	case 'w':
		unit = 7 * 24 * time.Hour
	}
	if unit > 0 {
		count, err := strconv.ParseFloat(s[:len(s)-1], 64)
		if err != nil || count <= 0 {
			return 0, fmt.Errorf("--since %q: want a positive count, e.g. 30d", s)
		}
		return time.Duration(count * float64(unit)), nil
	}
	window, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("--since %q: want a duration such as 24h, 7d, 4w", s)
	}
	if window <= 0 {
		return 0, fmt.Errorf("--since %q must be positive", s)
	}
	return window, nil
}

func printRecoveryReport(cmd *cobra.Command, r *types.RecoveryReport, since string) error {
	out := cmd.OutOrStdout()
	_, _ = fmt.Fprintf(out, "window:    %s .. %s (%s)\n",
		r.From.UTC().Format(time.RFC3339), r.To.UTC().Format(time.RFC3339), since)
	if r.Incidents == 0 && r.DryRunExcluded == 0 && r.Simulated == nil {
		_, err := fmt.Fprintln(out, "No incidents in this window.")
		if err == nil {
			printAnnouncedInterruptions(cmd, r.AnnouncedInterruptions)
		}
		return err
	}
	// The GPU-hour lines count GPUs only, and add resources together: they
	// are printed only for a window of one GPU resource. Any other window has
	// its hours in the table per resource and nowhere else.
	gpus := gpuHoursText(r.Incidents, r.AcceleratorHours)
	if gpus {
		_, _ = fmt.Fprintf(out, "Recovered GPU-hours: %s\n", gpuHours(r.RecoveredGPUHours))
	}
	_, _ = fmt.Fprintf(out, "incidents: %d in window, %d still open\n\n", r.Incidents, len(r.Open))
	if err := printAcceleratorHours(out, r.AcceleratorHours, ""); err != nil {
		return err
	}

	if estimate := r.SavingsEstimate; !gpus {
		// The saved hours of each resource are in its row above.
	} else if estimate != nil {
		_, _ = fmt.Fprintf(out, "GPU-hours saved (estimate): %s (manual baseline %s)\n", gpuHours(estimate.GPUHoursSaved), baseline(estimate.ManualBaselineSeconds))
	} else {
		_, _ = fmt.Fprintln(out, "Set spec.manualRecoveryBaseline to estimate GPU-hours saved.")
	}
	w := tabwriter.NewWriter(out, 2, 4, 2, ' ', 0)
	if gpus {
		_, _ = fmt.Fprintf(w, "degraded GPU-hours\t%s\n", gpuHours(r.DegradedGPUHours))
	}
	_, _ = fmt.Fprintf(w, "incidents recovered\t%d of %d\t%s\n",
		r.Recovered, r.Incidents, share(float64(r.Recovered), float64(r.Incidents)))
	_, _ = fmt.Fprintf(w, "  without a human\t%d of %d\t%s of recovered\n",
		r.RecoveredUnattended, r.Recovered, share(float64(r.RecoveredUnattended), float64(r.Recovered)))
	// Printed directly under "recovered" on purpose: this is the number that
	// used to be inside it, and the pair only makes sense read together.
	if gpus {
		_, _ = fmt.Fprintf(w, "closed without verified recovery\t%d of %d\t%s\t(%s degraded)\n",
			r.ObservedOnly, r.Incidents, share(float64(r.ObservedOnly), float64(r.Incidents)),
			gpuHours(r.ObservedOnlyGPUHours))
		_, _ = fmt.Fprintf(w, "target changed, recovery unverified\t%d\t(%s degraded)\n", r.Superseded, gpuHours(r.SupersededGPUHours))
	} else {
		_, _ = fmt.Fprintf(w, "closed without verified recovery\t%d of %d\t%s\n",
			r.ObservedOnly, r.Incidents, share(float64(r.ObservedOnly), float64(r.Incidents)))
		_, _ = fmt.Fprintf(w, "target changed, recovery unverified\t%d\n", r.Superseded)
	}
	_, _ = fmt.Fprintf(w, "Time to recover (n=%d)\tp50 %s\tp90 %s\tmean %s\n",
		r.MTTR.Samples, seconds(r.MTTR.P50Seconds), seconds(r.MTTR.P90Seconds), seconds(r.MTTR.MeanSeconds))
	if err := w.Flush(); err != nil {
		return err
	}

	if err := printReportIncidents(cmd, r.IncidentDetails, r.Incidents, "TIME TO RECOVER", gpus); err != nil {
		return err
	}
	if len(r.Classes) > 0 && !gpus {
		_, _ = fmt.Fprintln(out, "\nproblem classes:")
		w := tabwriter.NewWriter(out, 2, 4, 2, ' ', 0)
		_, _ = fmt.Fprintln(w, "CLASS\tINCIDENTS\tNOTHING DONE\tUNATTENDED\tMTTR P50\tMTTR P90")
		for _, class := range r.Classes {
			_, _ = fmt.Fprintf(w, "%s\t%d\t%d\t%d\t%s\t%s\n", class.Class, class.Incidents, class.ObservedOnly,
				class.RecoveredUnattended, seconds(class.MTTR.P50Seconds), seconds(class.MTTR.P90Seconds))
		}
		if err := w.Flush(); err != nil {
			return err
		}
	} else if len(r.Classes) > 0 {
		_, _ = fmt.Fprintln(out, "\ntop problem classes by degraded GPU-hours:")
		w := tabwriter.NewWriter(out, 2, 4, 2, ' ', 0)
		_, _ = fmt.Fprintln(w, "CLASS\tINCIDENTS\tDEGRADED\tDEGRADED IN REMEDIATED INCIDENTS\tNOTHING DONE\tUNATTENDED\tMTTR P50\tMTTR P90")
		for _, class := range r.Classes {
			_, _ = fmt.Fprintf(w, "%s\t%d\t%s\t%s\t%d\t%d\t%s\t%s\n",
				class.Class, class.Incidents, gpuHours(class.DegradedGPUHours),
				gpuHours(class.RecoveredGPUHours), class.ObservedOnly, class.RecoveredUnattended,
				seconds(class.MTTR.P50Seconds), seconds(class.MTTR.P90Seconds))
		}
		if err := w.Flush(); err != nil {
			return err
		}
	}

	if len(r.Open) > 0 && !gpus {
		_, _ = fmt.Fprintf(out, "\nstill open (%d), oldest first:\n", len(r.Open))
		w := tabwriter.NewWriter(out, 2, 4, 2, ' ', 0)
		_, _ = fmt.Fprintln(w, "ID\tSTATE\tCLASS\tNODE\tAGE")
		for _, inc := range r.Open {
			_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n",
				inc.ID, inc.State, inc.Class, inc.Node, r.To.Sub(inc.OpenedAt).Round(time.Second))
		}
		if err := w.Flush(); err != nil {
			return err
		}
	} else if len(r.Open) > 0 {
		_, _ = fmt.Fprintf(out, "\nstill open (%d), oldest first:\n", len(r.Open))
		w := tabwriter.NewWriter(out, 2, 4, 2, ' ', 0)
		_, _ = fmt.Fprintln(w, "ID\tSTATE\tCLASS\tNODE\tGPU\tAGE\tDEGRADED")
		for _, inc := range r.Open {
			_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
				inc.ID, inc.State, inc.Class, inc.Node, inc.GPUUUID,
				r.To.Sub(inc.OpenedAt).Round(time.Second), gpuHours(inc.DegradedGPUHours))
		}
		if err := w.Flush(); err != nil {
			return err
		}
	}

	// The legend is not decoration: every one of these numbers has a plausible
	// wrong reading, and a capacity report that has to be interpreted is a
	// report that will be misquoted.
	if !gpus {
		_, _ = fmt.Fprint(out, "\nlegend:\n"+
			"  recovered     = an audited remediation followed by a verified resolution.\n"+
			"                  Overlapping recovery intervals on one device count once.\n"+
			"  unverified    = closed without that evidence, including observation and manual closure.\n"+
			"  unattended    = recovered without ever asking for an approval (no human decision).\n"+
			"  hours         = degraded device-time per resource in its own unit, clipped to the\n"+
			"                  window, not lost time: a degraded device may still have been serving.\n"+
			"                  The devices recorded when the incident opened; node-hours where\n"+
			"                  no device count was recorded.\n"+
			"  MTTR          = full open-to-resolved time of incidents that RECOVERED in the\n"+
			"                  window; an incident nothing repaired contributes no repair time.\n")
	} else {
		_, _ = fmt.Fprint(out, "\nlegend:\n"+
			"  recovered     = an audited remediation followed by a verified resolution.\n"+
			"                  Overlapping recovery intervals on one GPU count once in the fleet total.\n"+
			"  unverified    = closed without that evidence, including observation and manual closure.\n"+
			"  unattended    = recovered without ever asking for an approval (no human decision).\n"+
			"  GPU-hours     = degraded GPU-time clipped to the window, not lost GPU-time:\n"+
			"                  a degraded GPU may still have been serving.\n"+
			"                  1 GPU for a GPU-scoped incident, the node's registered inventory\n"+
			"                  for a node-scoped one.\n"+
			"  MTTR          = full open-to-resolved time of incidents that RECOVERED in the\n"+
			"                  window; an incident nothing repaired contributes no repair time.\n")
	}
	if r.AssumedSingleGPU > 0 && gpus {
		_, _ = fmt.Fprintf(out, "\nnote: %d node-scoped incident(s) hit nodes with no registered GPU inventory\n"+
			"and were charged 1 GPU each — the real degraded GPU-hours are higher.\n", r.AssumedSingleGPU)
	}
	printAnnouncedInterruptions(cmd, r.AnnouncedInterruptions)
	if r.DryRunExcluded > 0 {
		// Say this loudly. On a fresh install every incident is dry-run, so a
		// silent zero would read as "nothing happened" when the truth is
		// "everything was watched and nothing was executed, by design".
		_, _ = fmt.Fprintf(out, "\nnote: %s excluded because their installation was in dry-run.\n"+
			"Dry-run executes nothing, so it recovers nothing; these are not counted as capacity\n"+
			"returned to service. Switch to executionMode: Enabled (confined) to measure real recovery.\n",
			incidents(r.DryRunExcluded))
	}
	if sim := r.Simulated; sim != nil {
		// Printed AFTER the note above and under its own heading, never
		// interleaved with the real numbers. A reader skimming must not be
		// able to carry one of these away as a measurement.
		simGPUs := gpuHoursText(sim.Incidents, sim.AcceleratorHours)
		_, _ = fmt.Fprintf(out, "\nSIMULATED — what dry-run WOULD have done (nothing was executed)\n"+
			"  incidents             %d\n"+
			"  would recover         %d (%d without asking a human)\n"+
			"  no ladder to run      %d   ← closed with no policy bound to the class\n",
			sim.Incidents,
			sim.WouldRecover, sim.WouldRecoverUnattended,
			sim.ObservedOnly)
		if simGPUs {
			_, _ = fmt.Fprintf(out, "  degraded GPU-hours    %s   ← real: the hardware was degraded this long\n"+
				"  GPU-hours that would have been returned (estimate)  %s (assumes recovery at simulated resolution)\n",
				gpuHours(sim.DegradedGPUHours), gpuHours(sim.WouldRecoverGPUHours))
		}
		_, _ = fmt.Fprintf(out, "  ladder decision time  p50 %s  p90 %s\n", seconds(sim.MTTR.P50Seconds), seconds(sim.MTTR.P90Seconds))
		if err := printReportIncidents(cmd, sim.IncidentDetails, sim.Incidents, "SIMULATED DECISION TIME", simGPUs); err != nil {
			return err
		}
		if len(sim.Groups) > 0 || sim.DeviceCountUnknown != nil {
			_, _ = fmt.Fprintln(out)
		}
		if err := printAcceleratorHours(out, sim.AcceleratorHours, "  "); err != nil {
			return err
		}
		if estimate := sim.WouldHaveSaved; estimate != nil && simGPUs {
			_, _ = fmt.Fprintf(out, "  GPU-hours that would have been saved (estimate): %s (manual baseline %s; assumes simulated decision time equals recovery time)\n", gpuHours(estimate.GPUHoursSaved), baseline(estimate.ManualBaselineSeconds))
		}
		if sim.AnnouncedInterruptions > 0 {
			_, _ = fmt.Fprintf(out, "  %d dry-run interruption(s) announced by the cloud are left out: the nodes were not degraded.\n", sim.AnnouncedInterruptions)
		}
		_, _ = fmt.Fprint(out, "\nThese are decisions, not repairs: every one of them reached RESOLVED against a\n"+
			"synthetic success, so they measure what the policy would have chosen — which is\n"+
			"what a dry-run pilot is for — and not what came back.\n")
	}
	return nil
}

// printAnnouncedInterruptions says how many announced cloud interruptions
// the numbers leave out, so a window that held only those does not read as
// a quiet one.
func printAnnouncedInterruptions(cmd *cobra.Command, n int) {
	if n == 0 {
		return
	}
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "\nnote: %d interruption(s) announced by the cloud are left out: the nodes were\n"+
		"not degraded, and nothing was recovered.\n", n)
}

// gpuHoursText reports whether a section prints the GPU-hour lines: only
// for incidents of one GPU resource, or from a controller that does not count
// per resource.
func gpuHoursText(incidents int, h types.AcceleratorHours) bool {
	if incidents == 0 || h.DeviceCountUnknown != nil {
		return false
	}
	if len(h.Groups) == 0 {
		return true
	}
	return len(h.Groups) == 1 && h.Groups[0].Group == types.AcceleratorGroupGPU && len(h.Groups[0].Resources) == 1
}

func printReportIncidents(cmd *cobra.Command, rows []types.RecoveryIncident, total int, timeLabel string, gpus bool) error {
	if len(rows) == 0 {
		return nil
	}
	w := tabwriter.NewWriter(cmd.OutOrStdout(), 2, 4, 2, ' ', 0)
	if !gpus {
		if len(rows) < total {
			_, _ = fmt.Fprintf(w, "\nShowing %d of %d incidents\n", len(rows), total)
		}
		_, _ = fmt.Fprintf(w, "\nINCIDENT\tNODE\tVENDOR\tSTATE\t%s\n", timeLabel)
		for _, row := range rows {
			_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", row.ID, row.Node, reportVendor(row), row.State, reportDuration(row))
		}
		return w.Flush()
	}
	if len(rows) < total {
		_, _ = fmt.Fprintf(w, "\nShowing %d of %d incidents (highest degraded GPU-hours)\n", len(rows), total)
	}
	_, _ = fmt.Fprintf(w, "\nINCIDENT\tNODE\tVENDOR\tSTATE\tDEGRADED GPU-HOURS\t%s\n", timeLabel)
	for _, row := range rows {
		_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n", row.ID, row.Node, reportVendor(row), row.State, gpuHours(row.DegradedGPUHours), reportDuration(row))
	}
	return w.Flush()
}

func reportVendor(row types.RecoveryIncident) string {
	if row.Vendor == "" {
		return "unknown"
	}
	return string(row.Vendor)
}

func reportDuration(row types.RecoveryIncident) string {
	if row.RecoverySeconds == nil {
		return "—"
	}
	return (time.Duration(*row.RecoverySeconds * float64(time.Second))).Round(time.Second).String()
}

// printAcceleratorHours prints the window per accelerator resource, one
// block per group in its own unit, never a sum across resources; then the
// node-hours of incidents whose device count is unknown, and the total cost
// estimate when every resource shown has a rate.
func printAcceleratorHours(out io.Writer, h types.AcceleratorHours, indent string) error {
	if len(h.Groups) == 0 && h.DeviceCountUnknown == nil {
		return nil
	}
	w := tabwriter.NewWriter(out, 2, 4, 2, ' ', 0)
	for _, g := range h.Groups {
		_, _ = fmt.Fprintf(w, "%s%s — %s\n", indent, acceleratorGroupTitle(g.Group), g.Unit)
		_, _ = fmt.Fprintf(w, "%s  RESOURCE\tINCIDENTS\tDEGRADED\tRECOVERED\tSAVED (ESTIMATE)\tCOST EQUIVALENT (ESTIMATE)\n", indent)
		for _, r := range g.Resources {
			saved, cost := "Manual recovery baseline not set", "-"
			if e := r.SavedEstimate; e != nil {
				saved = fmt.Sprintf("%s %s, manual baseline %s", gpuHours(e.Hours), g.Unit, baseline(e.ManualBaselineSeconds))
				cost = "rate not set"
				if c := e.Cost; c != nil {
					cost = fmt.Sprintf("%s %s at %s %s per %s", money(c.Amount), c.Currency, money(c.RatePerHour), c.Currency, strings.TrimSuffix(g.Unit, "s"))
				}
			}
			_, _ = fmt.Fprintf(w, "%s  %s\t%d\t%s\t%s\t%s\t%s\n", indent, r.Resource, r.Incidents,
				gpuHours(r.DegradedHours), gpuHours(r.RecoveredHours), saved, cost)
		}
	}
	if u := h.DeviceCountUnknown; u != nil {
		_, _ = fmt.Fprintf(w, "%sDevice count unknown — node-hours\n", indent)
		_, _ = fmt.Fprintf(w, "%s  degraded\t%s\t(%s; no device count was recorded, so no device-hours and no cost)\n",
			indent, gpuHours(u.DegradedNodeHours), incidents(u.Incidents))
		_, _ = fmt.Fprintf(w, "%s  recovered\t%s\n", indent, gpuHours(u.RecoveredNodeHours))
	}
	if err := w.Flush(); err != nil {
		return err
	}
	if len(h.Groups) > 1 || len(h.Groups) == 1 && len(h.Groups[0].Resources) > 1 {
		_, _ = fmt.Fprintf(out, "%sHours are not added across resources: each counts its own devices.\n", indent)
	}
	if t := h.SavedCostTotal; t != nil {
		if t.Amount != nil {
			_, _ = fmt.Fprintf(out, "%sTotal cost equivalent (estimate): %s %s\n", indent, money(*t.Amount), t.Currency)
		} else {
			_, _ = fmt.Fprintf(out, "%sTotal cost equivalent (estimate) unavailable. Rate not set for: %s.\n", indent, strings.Join(t.RateNotSetFor, ", "))
		}
	}
	_, err := fmt.Fprintln(out)
	return err
}

func acceleratorGroupTitle(group string) string {
	switch group {
	case types.AcceleratorGroupGPU:
		return "GPU"
	case types.AcceleratorGroupTPU:
		return "TPU"
	case types.AcceleratorGroupAWSNeuron:
		return "AWS Neuron"
	}
	return group
}

func money(v float64) string { return strconv.FormatFloat(v, 'f', 2, 64) }

func gpuHours(v float64) string { return strconv.FormatFloat(v, 'f', 1, 64) }

// baseline is a manual baseline without its zero minutes and seconds: 4h, not
// 4h0m0s.
func baseline(v float64) string {
	s := seconds(v)
	if strings.HasSuffix(s, "m0s") {
		s = strings.TrimSuffix(s, "0s")
	}
	if strings.HasSuffix(s, "h0m") {
		s = strings.TrimSuffix(s, "0m")
	}
	return s
}

func incidents(n int) string {
	if n == 1 {
		return "1 incident"
	}
	return fmt.Sprintf("%d incidents", n)
}

func seconds(v float64) string {
	if v <= 0 {
		return "-"
	}
	return (time.Duration(v * float64(time.Second))).Round(time.Second).String()
}

// share prints a percentage, or "n/a" when the denominator is zero: printing
// "0.0%" for "nothing happened" invites the reader to see a failure.
func share(part, total float64) string {
	if total <= 0 {
		return "n/a"
	}
	return strconv.FormatFloat(100*part/total, 'f', 1, 64) + "%"
}
