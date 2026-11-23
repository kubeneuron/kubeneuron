// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"io"
	"net/url"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/kubeneuron/kubeneuron/internal/inventory"
	"github.com/kubeneuron/kubeneuron/pkg/types"
)

// cmdHealth prints what the controller last read from every accelerator
// health source (GET /api/v1/accelerator-health).
func cmdHealth() *cobra.Command {
	c := &cobra.Command{
		Use:   "health",
		Short: "Show each node's accelerator health per source (healthy, fault, unknown, ignored, no-signal) with the rule and why",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := newClient(cmd)
			if err != nil {
				return err
			}
			path := types.AcceleratorHealthPath
			if node, _ := cmd.Flags().GetString("node"); node != "" {
				path += "?node=" + url.QueryEscape(node)
			}
			var view types.AcceleratorHealthView
			if err := c.do("GET", path, nil, &view); err != nil {
				return err
			}
			if asJSON, _ := cmd.Flags().GetBool("json"); asJSON {
				return printOperationJSON(cmd, view)
			}
			printHealth(cmd.OutOrStdout(), view)
			return nil
		},
	}
	c.Flags().String("node", "", "show only this node (exact name)")
	c.Flags().Bool("json", false, "print the view as JSON")
	return c
}

// The table explains the missing source; JSON retains the reported condition names.
const missingGPUConditions = "No GPU health condition is reported on this node. This source needs a platform component that publishes one (on EKS, the node monitoring agent; on AKS, node problem detector). Other sources are not affected."

func printHealth(out io.Writer, view types.AcceleratorHealthView) {
	if len(view.Rows) == 0 {
		_, _ = fmt.Fprintln(out, "No accelerator health read yet.")
		return
	}
	w := tabwriter.NewWriter(out, 2, 4, 2, ' ', 0)
	_, _ = fmt.Fprintln(w, "NODE\tSOURCE\tPROFILE\tRULE\tCLASS\tDEVICE\tSTATE\tREASON\tREAD\tWHY")
	for _, r := range view.Rows {
		read := "-"
		if !r.ReadAt.IsZero() {
			read = r.ReadAt.UTC().Format(time.RFC3339)
		}
		why := r.Why
		if r.Source == "kubernetes-node" && r.Profile == "nvidia" && r.Rule == "conditions" && r.State == "unknown" {
			why = missingGPUConditions
		}
		node := r.Node
		if node == "" {
			node = "(all)"
		}
		_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			node, r.Source, dash(r.Profile), dash(r.Rule), dash(string(r.Class)), dash(r.Device), r.State, dash(r.Reason), read, dash(why))
	}
	_ = w.Flush()
	if view.Truncated {
		_, _ = fmt.Fprintf(out, "Truncated at %d %s; use --node to see one node.\n", len(view.Rows), inventory.Plural(len(view.Rows), "row", "rows"))
	}
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
