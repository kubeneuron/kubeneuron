package main

import (
	"encoding/json"
	"fmt"
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
		Short: "Recovery report for a window: GPU-hours degraded, recovered, and unattended",
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
	_, _ = fmt.Fprintf(out, "incidents: %d in window, %d still open\n\n", r.Incidents, len(r.Open))

	w := tabwriter.NewWriter(out, 2, 4, 2, ' ', 0)
	_, _ = fmt.Fprintf(w, "degraded GPU-hours\t%s\n", gpuHours(r.DegradedGPUHours))
	_, _ = fmt.Fprintf(w, "recovered GPU-hours\t%s\t%s of degraded\n",
		gpuHours(r.RecoveredGPUHours), share(r.RecoveredGPUHours, r.DegradedGPUHours))
	_, _ = fmt.Fprintf(w, "incidents recovered\t%d of %d\t%s\n",
		r.Recovered, r.Incidents, share(float64(r.Recovered), float64(r.Incidents)))
	_, _ = fmt.Fprintf(w, "  without a human\t%d of %d\t%s of recovered\n",
		r.RecoveredUnattended, r.Recovered, share(float64(r.RecoveredUnattended), float64(r.Recovered)))
	// Printed directly under "recovered" on purpose: this is the number that
	// used to be inside it, and the pair only makes sense read together.
	_, _ = fmt.Fprintf(w, "closed, nothing done\t%d of %d\t%s\t(%s degraded)\n",
		r.ObservedOnly, r.Incidents, share(float64(r.ObservedOnly), float64(r.Incidents)),
		gpuHours(r.ObservedOnlyGPUHours))
	_, _ = fmt.Fprintf(w, "MTTR (recovered, n=%d)\tp50 %s\tp90 %s\tmean %s\n",
		r.MTTR.Samples, seconds(r.MTTR.P50Seconds), seconds(r.MTTR.P90Seconds), seconds(r.MTTR.MeanSeconds))
	if err := w.Flush(); err != nil {
		return err
	}

	if len(r.Classes) > 0 {
		_, _ = fmt.Fprintln(out, "\ntop problem classes by degraded GPU-hours:")
		w := tabwriter.NewWriter(out, 2, 4, 2, ' ', 0)
		_, _ = fmt.Fprintln(w, "CLASS\tINCIDENTS\tDEGRADED\tRECOVERED\tNOTHING DONE\tUNATTENDED\tMTTR P50\tMTTR P90")
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

	if len(r.Open) > 0 {
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
	_, _ = fmt.Fprint(out, "\nlegend:\n"+
		"  recovered     = the incident reached RESOLVED *and* a remediation step executed.\n"+
		"                  NEEDS_HUMAN and EXPIRED do not count, and an incident parked for\n"+
		"                  a human keeps accruing degraded time.\n"+
		"  nothing done  = reached RESOLVED with no remediation step ever executed — the\n"+
		"                  incident was observed and closed. Usually a problem class with no\n"+
		"                  policy bound to it: it does not error, it does not alert, and it\n"+
		"                  is not recovered capacity. Check `kubectl get gpuremediationpolicy`\n"+
		"                  against the classes your detectors emit.\n"+
		"  unattended    = recovered without ever asking for an approval (no human decision).\n"+
		"  GPU-hours     = degraded GPU-time clipped to the window, not lost GPU-time:\n"+
		"                  a degraded GPU may still have been serving.\n"+
		"                  1 GPU for a GPU-scoped incident, the node's registered inventory\n"+
		"                  for a node-scoped one.\n"+
		"  MTTR          = full open-to-resolved time of incidents that RECOVERED in the\n"+
		"                  window; an incident nothing repaired contributes no repair time.\n")
	if r.AssumedSingleGPU > 0 {
		_, _ = fmt.Fprintf(out, "\nnote: %d node-scoped incident(s) hit nodes with no registered GPU inventory\n"+
			"and were charged 1 GPU each — the real degraded GPU-hours are higher.\n", r.AssumedSingleGPU)
	}
	if r.DryRunExcluded > 0 {
		// Say this loudly. On a fresh install every incident is dry-run, so a
		// silent zero would read as "nothing happened" when the truth is
		// "everything was watched and nothing was executed, by design".
		_, _ = fmt.Fprintf(out, "\nnote: %d incident(s) excluded because their installation was in dry-run.\n"+
			"Dry-run executes nothing, so it recovers nothing; these are not counted as capacity\n"+
			"returned to service. Switch to executionMode: Enabled (confined) to measure real recovery.\n",
			r.DryRunExcluded)
	}
	if sim := r.Simulated; sim != nil {
		// Printed AFTER the note above and under its own heading, never
		// interleaved with the real numbers. A reader skimming must not be
		// able to carry one of these away as a measurement.
		_, _ = fmt.Fprintf(out, "\nSIMULATED — what dry-run WOULD have done (nothing was executed)\n"+
			"  incidents             %d\n"+
			"  would recover         %d (%d without asking a human)\n"+
			"  no ladder to run      %d   ← closed with no policy bound to the class\n"+
			"  degraded GPU-hours    %s   ← real: the hardware was degraded this long\n"+
			"  would recover         %s   ← hypothetical: no capacity was returned\n"+
			"  ladder decision time  p50 %s  p90 %s\n",
			sim.Incidents,
			sim.WouldRecover, sim.WouldRecoverUnattended,
			sim.ObservedOnly,
			gpuHours(sim.DegradedGPUHours),
			gpuHours(sim.WouldRecoverGPUHours),
			seconds(sim.MTTR.P50Seconds), seconds(sim.MTTR.P90Seconds))
		_, _ = fmt.Fprint(out, "\nThese are decisions, not repairs: every one of them reached RESOLVED against a\n"+
			"synthetic success, so they measure what the policy would have chosen — which is\n"+
			"what a dry-run pilot is for — and not what came back.\n")
	}
	return nil
}

func gpuHours(v float64) string { return strconv.FormatFloat(v, 'f', 1, 64) }

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
