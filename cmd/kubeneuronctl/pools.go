// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"text/tabwriter"
	"time"

	"github.com/kubeneuron/kubeneuron/pkg/types"
	"github.com/spf13/cobra"
)

func cmdPools() *cobra.Command {
	cmd := &cobra.Command{
		Use: "pools", Short: "Show add-first availability for accelerator node pools", Args: cobra.NoArgs,
		Long: "Show add-first availability for accelerator node pools. Mixed means that different remediation policies can apply to nodes in this pool, depending on fault class and accelerator vendor. Those policies use different replacement orders.",
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
			fmt.Fprintln(w, "POOL\tCLOUD\tACCELERATOR NODES\tREPLACEMENT POLICY\tCHECKED (UTC)\tADD-FIRST")
			for _, row := range view.Pools {
				checked := "—"
				if row.CheckedAt != nil {
					checked = row.CheckedAt.UTC().Format(time.RFC3339)
				}
				status := row.Status
				if row.ReplacementPolicy == "RemoveFirst" {
					status = "not required"
				} else {
					if status == "" {
						status = "unknown"
					}
					if status == "unknown" && row.CheckedAt == nil && row.Reason == "" {
						status += " — not checked yet"
					}
					if row.Reason != "" {
						status += " — " + row.Reason
					}
					if row.Action != "" {
						status += " — " + row.Action
					}
				}
				fmt.Fprintf(w, "%s\t%s\t%d\t%s\t%s\t%s\n", row.Pool, row.Cloud, row.AcceleratorNodes, row.ReplacementPolicy, checked, status)
			}
			return w.Flush()
		},
	}
	cmd.Flags().Bool("json", false, "print the pool assessments as JSON")
	return cmd
}
