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
	_, _ = fmt.Fprintf(w, "MTTR (resolved, n=%d)\tp50 %s\tp90 %s\tmean %s\n",
		r.MTTR.Samples, seconds(r.MTTR.P50Seconds), seconds(r.MTTR.P90Seconds), seconds(r.MTTR.MeanSeconds))
	if err := w.Flush(); err != nil {
		return err
	}

	if len(r.Classes) > 0 {
		_, _ = fmt.Fprintln(out, "\ntop problem classes by degraded GPU-hours:")
		w := tabwriter.NewWriter(out, 2, 4, 2, ' ', 0)
		_, _ = fmt.Fprintln(w, "CLASS\tINCIDENTS\tDEGRADED\tRECOVERED\tUNATTENDED\tMTTR P50\tMTTR P90")
		for _, class := range r.Classes {
			_, _ = fmt.Fprintf(w, "%s\t%d\t%s\t%s\t%d\t%s\t%s\n",
				class.Class, class.Incidents, gpuHours(class.DegradedGPUHours),
				gpuHours(class.RecoveredGPUHours), class.RecoveredUnattended,
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
		"  recovered   = the incident reached RESOLVED. NEEDS_HUMAN and EXPIRED do not count,\n"+
		"                and an incident parked for a human keeps accruing degraded time.\n"+
		"  unattended  = resolved without ever asking for an approval (no human decision).\n"+
		"  GPU-hours   = degraded GPU-time clipped to the window, not lost GPU-time:\n"+
		"                a degraded GPU may still have been serving.\n"+
		"                1 GPU for a GPU-scoped incident, the node's registered inventory\n"+
		"                for a node-scoped one.\n"+
		"  MTTR        = full open-to-resolved time of incidents that resolved in the window.\n")
	if r.AssumedSingleGPU > 0 {
		_, _ = fmt.Fprintf(out, "\nnote: %d node-scoped incident(s) hit nodes with no registered GPU inventory\n"+
			"and were charged 1 GPU each — the real degraded GPU-hours are higher.\n", r.AssumedSingleGPU)
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
