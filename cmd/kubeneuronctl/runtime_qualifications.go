package main

import (
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

// cmdRuntimeQualifications groups the runtime contract qualification
// workflow: create a bounded, evidence-only qualification of an explicit node
// cohort, list and show qualifications, and record an observation. Every
// subcommand prints the controller's JSON, which carries the read-time
// effective_state, expired, expiry_pending, and ready_for_approval fields, so
// a qualification past its expiry is never mistaken for one that is ready.
//
// There is intentionally no approve, promote, delete, apply, or enable
// subcommand: a ReadyForApproval qualification is evidence for a separate,
// later human decision, and this CLI does not turn it into authority.
func cmdRuntimeQualifications() *cobra.Command {
	root := &cobra.Command{
		Use:   "runtime-qualifications",
		Short: "Create, inspect, and observe evidence-only runtime contract qualifications",
	}
	root.AddCommand(
		runtimeQualificationCreateCommand(),
		runtimeQualificationListCommand(),
		runtimeQualificationShowCommand(),
		runtimeQualificationObserveCommand(),
	)
	return root
}

func runtimeQualificationCreateCommand() *cobra.Command {
	command := &cobra.Command{
		Use:   "create",
		Short: "Freeze an explicit node cohort against its runtime profile and start observing it",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			nodes, _ := cmd.Flags().GetStringSlice("nodes")
			cohort := make([]string, 0, len(nodes))
			for _, node := range nodes {
				if node = strings.TrimSpace(node); node != "" {
					cohort = append(cohort, node)
				}
			}
			if len(cohort) == 0 {
				return fmt.Errorf("--nodes is required (comma-separated or repeated)")
			}
			vendor, _ := cmd.Flags().GetString("vendor")
			if strings.TrimSpace(vendor) == "" {
				return fmt.Errorf("--vendor is required")
			}
			minSamples, _ := cmd.Flags().GetInt("min-samples")
			if minSamples <= 0 {
				return fmt.Errorf("--min-samples must be a positive integer")
			}
			minDuration, _ := cmd.Flags().GetString("min-duration")
			if parsed, err := time.ParseDuration(strings.TrimSpace(minDuration)); err != nil || parsed <= 0 {
				return fmt.Errorf("--min-duration must be a positive Go duration such as 30m or 12h")
			}
			expiresAt, _ := cmd.Flags().GetString("expires-at")
			expiry, err := time.Parse(time.RFC3339, strings.TrimSpace(expiresAt))
			if err != nil {
				return fmt.Errorf("--expires-at must be an RFC3339 timestamp")
			}
			// Everything the client can check cheaply is checked before a
			// request is built; the controller still validates every field.
			client, err := newClient(cmd)
			if err != nil {
				return err
			}
			actor, _ := cmd.Flags().GetString("actor")
			body := map[string]any{
				"actor":  actorOrLocalUserValue(actor),
				"nodes":  cohort,
				"vendor": strings.TrimSpace(vendor),
				"requirements": map[string]any{
					"min_samples":  minSamples,
					"min_duration": strings.TrimSpace(minDuration),
				},
				"expires_at": expiry.UTC().Format(time.RFC3339),
			}
			for _, flag := range []string{"tenant", "cluster"} {
				if value, _ := cmd.Flags().GetString(flag); strings.TrimSpace(value) != "" {
					body[flag] = strings.TrimSpace(value)
				}
			}
			var qualification any
			if err := client.doHeaders("POST", "/api/v1/runtime-contract-qualifications", body, &qualification, map[string]string{"Idempotency-Key": operationKey()}); err != nil {
				return err
			}
			return printOperationJSON(cmd, qualification)
		},
	}
	command.Flags().StringSlice("nodes", nil, "explicit node cohort (comma-separated or repeated); required")
	command.Flags().String("vendor", "", "accelerator vendor (nvidia, amd, intel, or google); required")
	command.Flags().Int("min-samples", 0, "successful Full observations required before ReadyForApproval; required")
	command.Flags().String("min-duration", "", "minimum time since the first successful sample, as a Go duration (e.g. 30m); required")
	command.Flags().String("expires-at", "", "RFC3339 instant after which the qualification is Expired; required")
	command.Flags().String("tenant", "", "optional tenant assertion; must match the kubeneuron.io/tenant label of every node")
	command.Flags().String("cluster", "", "optional cluster assertion; must match the kubeneuron.io/cluster label of every node")
	command.Flags().String("actor", "", "audited actor (default: $USER)")
	return command
}

func runtimeQualificationListCommand() *cobra.Command {
	command := &cobra.Command{
		Use:   "list",
		Short: "List runtime contract qualifications with their effective state",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			client, err := newClient(cmd)
			if err != nil {
				return err
			}
			query := url.Values{}
			for _, flag := range []string{"state", "tenant", "cluster", "since", "until", "cursor"} {
				if value, _ := cmd.Flags().GetString(flag); strings.TrimSpace(value) != "" {
					query.Set(flag, strings.TrimSpace(value))
				}
			}
			if limit, _ := cmd.Flags().GetInt("limit"); limit > 0 {
				query.Set("limit", strconv.Itoa(limit))
			}
			if cmd.Flags().Changed("include-expired") {
				includeExpired, _ := cmd.Flags().GetBool("include-expired")
				query.Set("include_expired", strconv.FormatBool(includeExpired))
			}
			path := "/api/v1/runtime-contract-qualifications"
			if encoded := query.Encode(); encoded != "" {
				path += "?" + encoded
			}
			var response any
			if err := client.do("GET", path, nil, &response); err != nil {
				return err
			}
			return printOperationJSON(cmd, response)
		},
	}
	command.Flags().String("state", "", "stored state filter (Observing, ReadyForApproval, Invalidated, Expired)")
	command.Flags().String("tenant", "", "frozen tenant scope filter")
	command.Flags().String("cluster", "", "frozen cluster scope filter")
	command.Flags().String("since", "", "RFC3339 lower creation-time bound")
	command.Flags().String("until", "", "RFC3339 upper creation-time bound")
	command.Flags().String("cursor", "", "opaque cursor from a previous page")
	command.Flags().Int("limit", 0, "maximum qualifications per page (1-500)")
	command.Flags().Bool("include-expired", true, "include qualifications whose expiry has passed (the controller default)")
	return command
}

func runtimeQualificationShowCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "show <qualification-id>",
		Short: "Show one qualification, its frozen bindings, evidence, and effective state",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := newClient(cmd)
			if err != nil {
				return err
			}
			var qualification any
			if err := client.do("GET", "/api/v1/runtime-contract-qualifications/"+url.PathEscape(args[0]), nil, &qualification); err != nil {
				return err
			}
			return printOperationJSON(cmd, qualification)
		},
	}
}

func runtimeQualificationObserveCommand() *cobra.Command {
	command := &cobra.Command{
		Use:   "observe <qualification-id>",
		Short: "Record one fresh observation of the frozen cohort",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := newClient(cmd)
			if err != nil {
				return err
			}
			actor, _ := cmd.Flags().GetString("actor")
			version, _ := cmd.Flags().GetInt("resource-version")
			if version < 0 {
				return fmt.Errorf("--resource-version must not be negative")
			}
			body := map[string]any{"actor": actorOrLocalUserValue(actor), "resource_version": version}
			var qualification any
			if err := client.doHeaders("POST", "/api/v1/runtime-contract-qualifications/"+url.PathEscape(args[0])+"/observe", body, &qualification, map[string]string{"Idempotency-Key": operationKey()}); err != nil {
				return err
			}
			return printOperationJSON(cmd, qualification)
		},
	}
	command.Flags().String("actor", "", "audited actor (default: $USER)")
	command.Flags().Int("resource-version", 0, "optimistic version from runtime-qualifications show (0 reads current)")
	return command
}
