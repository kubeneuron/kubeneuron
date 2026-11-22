// SPDX-License-Identifier: Apache-2.0

package main

import (
	"cmp"
	"fmt"
	"text/tabwriter"
	"time"

	"github.com/kubeneuron/kubeneuron/pkg/types"
	"github.com/spf13/cobra"
)

func cmdPools() *cobra.Command {
	cmd := &cobra.Command{
		Use: "pools", Short: "Show add-first availability for accelerator node pools", Args: cobra.NoArgs,
		Long: "Show add-first availability for accelerator node pools. Mixed means that different remediation policies can apply to nodes in this pool, depending on fault class and accelerator vendor. Those policies use different replacement orders. Availability reflects the pool configuration at its last check. The cloud determines capacity for the extra node when it receives the request.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			client, err := newClient(cmd)
			if err != nil {
				return err
			}
			var view types.PoolAvailabilityView
			if err := client.do("GET", types.PoolAvailabilityPath, nil, &view); err != nil {
				return err
			}
			if asJSON, _ := cmd.Flags().GetBool("json"); asJSON {
				return printOperationJSON(cmd, view)
			}
			w := tabwriter.NewWriter(cmd.OutOrStdout(), 2, 4, 2, ' ', 0)
			if _, err := fmt.Fprintln(w, "POOL\tCLOUD\tACCELERATOR NODES\tREPLACEMENT POLICY\tCHECKED (UTC)\tREPLACEMENT\tADD-FIRST"); err != nil {
				return err
			}
			for _, row := range view.Pools {
				checked := "—"
				if row.CheckedAt != nil {
					checked = row.CheckedAt.UTC().Format(time.RFC3339)
				}
				replacement := "—"
				if row.ReplacementPolicy != "none" {
					replacement = poolCell(row.ReplacementStatus, row.ReplacementReason, row.ReplacementAction, row.CheckedAt == nil)
				}
				status := "not required"
				switch {
				case row.ReplacementPolicy == "RemoveFirst":
				case row.Reason != "" && row.Reason == row.ReplacementReason && row.ReasonCode == row.ReplacementReasonCode:
					status = cmp.Or(row.Status, "unknown") + " — see REPLACEMENT"
				default:
					status = poolCell(row.Status, row.Reason, row.Action, row.CheckedAt == nil)
				}
				if _, err := fmt.Fprintf(w, "%s\t%s\t%d\t%s\t%s\t%s\t%s\n", row.Pool, row.Cloud, row.AcceleratorNodes, row.ReplacementPolicy, checked, replacement, status); err != nil {
					return err
				}
			}
			return w.Flush()
		},
	}
	cmd.Flags().Bool("json", false, "print the pool assessments as JSON")
	return cmd
}

// poolCell is a status with its reason and action; an unknown status that was
// never checked says so.
func poolCell(status, reason, action string, unchecked bool) string {
	status = cmp.Or(status, "unknown")
	if status == "unknown" && unchecked && reason == "" {
		status += " — not checked yet"
	}
	if reason != "" {
		status += " — " + reason
	}
	if action != "" {
		status += " — " + action
	}
	return status
}
