package main

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"text/tabwriter"

	"github.com/google/uuid"
	"github.com/spf13/cobra"

	"github.com/kubeneuron/kubeneuron/internal/operations"
)

func operationKey() string { return "kubeneuronctl-" + uuid.NewString() }

func printOperationJSON(cmd *cobra.Command, value any) error {
	encoder := json.NewEncoder(cmd.OutOrStdout())
	encoder.SetIndent("", "  ")
	return encoder.Encode(value)
}

func cmdReadiness() *cobra.Command {
	return &cobra.Command{
		Use:   "readiness [node]",
		Short: "Explain fleet or node remediation readiness",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := newClient(cmd)
			if err != nil {
				return err
			}
			if len(args) == 1 {
				var item any
				if err := client.do("GET", "/api/v1/nodes/"+url.PathEscape(args[0])+"/readiness", nil, &item); err != nil {
					return err
				}
				return printOperationJSON(cmd, item)
			}
			var response struct {
				Items []struct {
					Node     string `json:"node"`
					Decision struct {
						State       string   `json:"state"`
						ReasonCodes []string `json:"reason_codes"`
					} `json:"decision"`
				} `json:"items"`
			}
			if err := client.do("GET", "/api/v1/readiness", nil, &response); err != nil {
				return err
			}
			w := tabwriter.NewWriter(cmd.OutOrStdout(), 2, 4, 2, ' ', 0)
			_, _ = fmt.Fprintln(w, "NODE\tSTATE\tREASONS")
			for _, item := range response.Items {
				_, _ = fmt.Fprintf(w, "%s\t%s\t%s\n", item.Node, item.Decision.State, strings.Join(item.Decision.ReasonCodes, ","))
			}
			return w.Flush()
		},
	}
}

func cmdCandidates() *cobra.Command {
	root := &cobra.Command{Use: "candidates", Short: "Upload and inspect candidate configuration without applying it"}
	upload := &cobra.Command{
		Use:   "upload <file>",
		Short: "Upload a CandidateConfiguration or native candidate CR",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := newClient(cmd)
			if err != nil {
				return err
			}
			data, err := os.ReadFile(args[0])
			if err != nil {
				return err
			}
			contentType := "application/yaml"
			if strings.EqualFold(filepath.Ext(args[0]), ".json") {
				contentType = "application/json"
			}
			actor, _ := cmd.Flags().GetString("actor")
			query := url.Values{"actor": []string{actorOrLocalUserValue(actor)}}
			for _, flag := range []string{"tenant", "cluster", "expires-at"} {
				if value, _ := cmd.Flags().GetString(flag); strings.TrimSpace(value) != "" {
					query.Set(strings.ReplaceAll(flag, "-", "_"), value)
				}
			}
			path := "/api/v1/candidates?" + query.Encode()
			var candidate operations.CandidateConfiguration
			if err := client.doBytes("POST", path, data, contentType, &candidate, map[string]string{"Idempotency-Key": operationKey()}); err != nil {
				return err
			}
			return printOperationJSON(cmd, candidate)
		},
	}
	upload.Flags().String("actor", "", "audited actor (default: $USER)")
	upload.Flags().String("tenant", "", "optional tenant scope")
	upload.Flags().String("cluster", "", "optional cluster scope")
	upload.Flags().String("expires-at", "", "optional RFC3339 expiry (default: 24h)")
	list := &cobra.Command{Use: "list", Short: "List retained candidate configurations", RunE: func(cmd *cobra.Command, _ []string) error {
		client, err := newClient(cmd)
		if err != nil {
			return err
		}
		var response any
		if err := client.do("GET", "/api/v1/candidates", nil, &response); err != nil {
			return err
		}
		return printOperationJSON(cmd, response)
	}}
	show := &cobra.Command{Use: "show <candidate-id>", Short: "Show one normalized candidate", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		client, err := newClient(cmd)
		if err != nil {
			return err
		}
		var candidate operations.CandidateConfiguration
		if err := client.do("GET", "/api/v1/candidates/"+url.PathEscape(args[0]), nil, &candidate); err != nil {
			return err
		}
		return printOperationJSON(cmd, candidate)
	}}
	revoke := &cobra.Command{Use: "revoke <candidate-id>", Short: "Logically revoke a candidate without deleting its audit trail", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		client, err := newClient(cmd)
		if err != nil {
			return err
		}
		reason, _ := cmd.Flags().GetString("reason")
		if strings.TrimSpace(reason) == "" {
			return fmt.Errorf("--reason is required")
		}
		actor, _ := cmd.Flags().GetString("actor")
		version, _ := cmd.Flags().GetInt("resource-version")
		var candidate operations.CandidateConfiguration
		if err := client.doHeaders("DELETE", "/api/v1/candidates/"+url.PathEscape(args[0]), map[string]any{"actor": actorOrLocalUserValue(actor), "reason": reason, "resource_version": version}, &candidate, map[string]string{"Idempotency-Key": operationKey()}); err != nil {
			return err
		}
		return printOperationJSON(cmd, candidate)
	}}
	revoke.Flags().String("actor", "", "audited actor (default: $USER)")
	revoke.Flags().String("reason", "", "why this candidate is revoked")
	revoke.Flags().Int("resource-version", 0, "optimistic version from candidates show (0 reads current)")
	root.AddCommand(upload, list, show, revoke)
	return root
}

func cmdPreview() *cobra.Command {
	command := &cobra.Command{
		Use:   "preview <candidate-id>",
		Short: "Create a deterministic policy-impact preview for a candidate",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := newClient(cmd)
			if err != nil {
				return err
			}
			actor, _ := cmd.Flags().GetString("actor")
			var preview operations.PolicyImpactPreview
			if err := client.doHeaders("POST", "/api/v1/candidates/"+url.PathEscape(args[0])+"/preview", map[string]string{"actor": actorOrLocalUserValue(actor)}, &preview, map[string]string{"Idempotency-Key": operationKey()}); err != nil {
				return err
			}
			return printOperationJSON(cmd, preview)
		},
	}
	command.Flags().String("actor", "", "audited actor (default: $USER)")
	return command
}

func cmdHealthCheck() *cobra.Command {
	command := &cobra.Command{
		Use:   "health-check <node>",
		Short: "Request a bounded passive, quick, or extended health check",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := newClient(cmd)
			if err != nil {
				return err
			}
			profile, _ := cmd.Flags().GetString("profile")
			reason, _ := cmd.Flags().GetString("reason")
			if reason == "" {
				return fmt.Errorf("--reason is required")
			}
			actor, _ := cmd.Flags().GetString("actor")
			deviceID, _ := cmd.Flags().GetString("device-id")
			body := map[string]any{"actor": actorOrLocalUserValue(actor), "node": args[0], "device_id": deviceID, "profile": profile, "reason": reason}
			var run operations.HealthCheckRun
			if err := client.doHeaders("POST", "/api/v1/health-checks", body, &run, map[string]string{"Idempotency-Key": operationKey()}); err != nil {
				return err
			}
			return printOperationJSON(cmd, run)
		},
	}
	command.Flags().String("profile", string(operations.HealthCheckPassive), "Passive, Quick, or Extended")
	command.Flags().String("reason", "", "why this check is required")
	command.Flags().String("device-id", "", "optional physical device ID")
	command.Flags().String("actor", "", "audited actor (default: $USER)")
	command.AddCommand(
		&cobra.Command{Use: "list", Short: "List retained health-check runs", RunE: func(cmd *cobra.Command, _ []string) error {
			client, err := newClient(cmd)
			if err != nil {
				return err
			}
			var response any
			if err := client.do("GET", "/api/v1/health-checks", nil, &response); err != nil {
				return err
			}
			return printOperationJSON(cmd, response)
		}},
		&cobra.Command{Use: "show <run-id>", Short: "Show one health-check run", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
			client, err := newClient(cmd)
			if err != nil {
				return err
			}
			var run operations.HealthCheckRun
			if err := client.do("GET", "/api/v1/health-checks/"+url.PathEscape(args[0]), nil, &run); err != nil {
				return err
			}
			return printOperationJSON(cmd, run)
		}},
		healthCancelCommand(),
	)
	return command
}

func healthCancelCommand() *cobra.Command {
	command := &cobra.Command{Use: "cancel <run-id>", Short: "Cancel an unleased health-check action", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		client, err := newClient(cmd)
		if err != nil {
			return err
		}
		actor, _ := cmd.Flags().GetString("actor")
		version, _ := cmd.Flags().GetInt("resource-version")
		var run operations.HealthCheckRun
		if err := client.doHeaders("POST", "/api/v1/health-checks/"+url.PathEscape(args[0])+"/cancel", map[string]any{"actor": actorOrLocalUserValue(actor), "resource_version": version}, &run, map[string]string{"Idempotency-Key": operationKey()}); err != nil {
			return err
		}
		return printOperationJSON(cmd, run)
	}}
	command.Flags().String("actor", "", "audited actor (default: $USER)")
	command.Flags().Int("resource-version", 0, "optimistic version from health-check show (0 reads current)")
	return command
}

func cmdSimulate() *cobra.Command {
	command := &cobra.Command{
		Use:   "simulate <node>",
		Short: "Build a no-side-effect remediation simulation from frozen evidence",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := newClient(cmd)
			if err != nil {
				return err
			}
			class, _ := cmd.Flags().GetString("class")
			rationale, _ := cmd.Flags().GetString("rationale")
			if class == "" || rationale == "" {
				return fmt.Errorf("--class and --rationale are required")
			}
			actor, _ := cmd.Flags().GetString("actor")
			action, _ := cmd.Flags().GetString("action")
			scope, _ := cmd.Flags().GetString("scope")
			deviceID, _ := cmd.Flags().GetString("device-id")
			body := map[string]any{"actor": actorOrLocalUserValue(actor), "node": args[0], "class": class, "rationale": rationale,
				"action": action, "scope": scope, "device_id": deviceID}
			var simulation operations.RemediationSimulation
			if err := client.doHeaders("POST", "/api/v1/simulations", body, &simulation, map[string]string{"Idempotency-Key": operationKey()}); err != nil {
				return err
			}
			return printOperationJSON(cmd, simulation)
		},
	}
	command.Flags().String("class", "", "problem class selected by the policy")
	command.Flags().String("rationale", "", "operator rationale recorded in audit")
	command.Flags().String("action", "", "requested semantic accelerator action")
	command.Flags().String("scope", "", "node, physical-device, or partition")
	command.Flags().String("device-id", "", "target device ID")
	command.Flags().String("actor", "", "audited actor (default: $USER)")
	command.AddCommand(
		&cobra.Command{Use: "list", Short: "List frozen remediation simulations", RunE: func(cmd *cobra.Command, _ []string) error {
			client, err := newClient(cmd)
			if err != nil {
				return err
			}
			var response any
			if err := client.do("GET", "/api/v1/simulations", nil, &response); err != nil {
				return err
			}
			return printOperationJSON(cmd, response)
		}},
		&cobra.Command{Use: "show <simulation-id>", Short: "Show one frozen remediation simulation", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
			client, err := newClient(cmd)
			if err != nil {
				return err
			}
			var simulation operations.RemediationSimulation
			if err := client.do("GET", "/api/v1/simulations/"+url.PathEscape(args[0]), nil, &simulation); err != nil {
				return err
			}
			return printOperationJSON(cmd, simulation)
		}},
		simulationIncidentCommand(),
	)
	return command
}

func simulationIncidentCommand() *cobra.Command {
	command := &cobra.Command{Use: "incident <simulation-id>", Short: "Create or attach the incident represented by a permitted simulation", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		client, err := newClient(cmd)
		if err != nil {
			return err
		}
		rationale, _ := cmd.Flags().GetString("rationale")
		if strings.TrimSpace(rationale) == "" {
			return fmt.Errorf("--rationale is required")
		}
		actor, _ := cmd.Flags().GetString("actor")
		version, _ := cmd.Flags().GetInt("resource-version")
		var incident any
		if err := client.doHeaders("POST", "/api/v1/incidents/from-simulation", map[string]any{"actor": actorOrLocalUserValue(actor), "simulation_id": args[0], "rationale": rationale, "resource_version": version}, &incident, map[string]string{"Idempotency-Key": operationKey()}); err != nil {
			return err
		}
		return printOperationJSON(cmd, incident)
	}}
	command.Flags().String("actor", "", "audited actor (default: $USER)")
	command.Flags().String("rationale", "", "why the simulation should become an incident")
	command.Flags().Int("resource-version", 0, "optimistic version from simulate show (0 reads current)")
	return command
}

func cmdAutonomy() *cobra.Command {
	root := &cobra.Command{Use: "autonomy", Short: "Manage bounded GPU autonomy plans"}
	root.AddCommand(
		&cobra.Command{Use: "list", Short: "List autonomy plans", RunE: func(cmd *cobra.Command, _ []string) error {
			client, err := newClient(cmd)
			if err != nil {
				return err
			}
			var response any
			if err := client.do("GET", "/api/v1/autonomy/plans", nil, &response); err != nil {
				return err
			}
			return printOperationJSON(cmd, response)
		}},
		autonomyShowCommand(), autonomyCreateCommand(), autonomyMutationCommand("approve <plan-id>", "Approve an autonomy plan for one required role", "approve", true),
		autonomyAttachSimulationCommand(), autonomyRolloutCommand(),
		autonomyMutationCommand("pause <plan-id>", "Pause an autonomy plan before further effects", "pause", false),
		autonomyMutationCommand("resume <plan-id>", "Resume a paused plan at a fresh canary", "resume", false),
		autonomyMutationCommand("rollback <plan-id>", "Roll back an autonomy plan", "rollback", false),
	)
	return root
}

func autonomyAttachSimulationCommand() *cobra.Command {
	command := &cobra.Command{Use: "attach-simulation <plan-id> <simulation-id>", Short: "Bind a permitted frozen simulation to a draft autonomy plan", Args: cobra.ExactArgs(2), RunE: func(cmd *cobra.Command, args []string) error {
		client, err := newClient(cmd)
		if err != nil {
			return err
		}
		actor, _ := cmd.Flags().GetString("actor")
		version, _ := cmd.Flags().GetInt("resource-version")
		var plan operations.GPUAutonomyPlan
		if err := client.doHeaders("POST", "/api/v1/autonomy/plans/"+url.PathEscape(args[0])+"/simulation", map[string]any{"actor": actorOrLocalUserValue(actor), "simulation_id": args[1], "resource_version": version}, &plan, map[string]string{"Idempotency-Key": operationKey()}); err != nil {
			return err
		}
		return printOperationJSON(cmd, plan)
	}}
	command.Flags().String("actor", "", "audited actor (default: $USER)")
	command.Flags().Int("resource-version", 0, "optimistic version from autonomy show (0 reads current)")
	return command
}

func autonomyRolloutCommand() *cobra.Command {
	return &cobra.Command{Use: "rollout <plan-id>", Short: "Show canary, bake, expansion and effect observations", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		client, err := newClient(cmd)
		if err != nil {
			return err
		}
		var rollout operations.AutonomyRollout
		if err := client.do("GET", "/api/v1/autonomy/plans/"+url.PathEscape(args[0])+"/rollout", nil, &rollout); err != nil {
			return err
		}
		return printOperationJSON(cmd, rollout)
	}}
}

func autonomyShowCommand() *cobra.Command {
	return &cobra.Command{Use: "show <plan-id>", Short: "Show plan, approvals and rollout link", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		client, err := newClient(cmd)
		if err != nil {
			return err
		}
		var plan operations.GPUAutonomyPlan
		if err := client.do("GET", "/api/v1/autonomy/plans/"+url.PathEscape(args[0]), nil, &plan); err != nil {
			return err
		}
		return printOperationJSON(cmd, plan)
	}}
}

func autonomyCreateCommand() *cobra.Command {
	command := &cobra.Command{Use: "create <json-file>", Short: "Create a time-bounded autonomy plan from JSON", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		client, err := newClient(cmd)
		if err != nil {
			return err
		}
		payload, err := os.ReadFile(args[0])
		if err != nil {
			return err
		}
		var raw map[string]any
		if err := json.Unmarshal(payload, &raw); err != nil {
			return fmt.Errorf("autonomy plan must be JSON: %w", err)
		}
		actor, _ := cmd.Flags().GetString("actor")
		if _, present := raw["actor"]; !present {
			raw["actor"] = actorOrLocalUserValue(actor)
		}
		var plan operations.GPUAutonomyPlan
		if err := client.doHeaders("POST", "/api/v1/autonomy/plans", raw, &plan, map[string]string{"Idempotency-Key": operationKey()}); err != nil {
			return err
		}
		return printOperationJSON(cmd, plan)
	}}
	command.Flags().String("actor", "", "audited actor (default: $USER)")
	return command
}

func autonomyMutationCommand(use, short, operation string, needsRole bool) *cobra.Command {
	command := &cobra.Command{Use: use, Short: short, Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		client, err := newClient(cmd)
		if err != nil {
			return err
		}
		actor, _ := cmd.Flags().GetString("actor")
		version, _ := cmd.Flags().GetInt("resource-version")
		body := map[string]any{"actor": actorOrLocalUserValue(actor), "resource_version": version}
		if needsRole {
			role, _ := cmd.Flags().GetString("role")
			if role == "" {
				return fmt.Errorf("--role is required")
			}
			body["role"] = role
		}
		if operation != "approve" {
			reason, _ := cmd.Flags().GetString("reason")
			if reason == "" {
				return fmt.Errorf("--reason is required")
			}
			body["reason"] = reason
		}
		var plan operations.GPUAutonomyPlan
		if err := client.doHeaders("POST", "/api/v1/autonomy/plans/"+url.PathEscape(args[0])+"/"+operation, body, &plan, map[string]string{"Idempotency-Key": operationKey()}); err != nil {
			return err
		}
		return printOperationJSON(cmd, plan)
	}}
	command.Flags().String("actor", "", "audited actor (default: $USER)")
	command.Flags().Int("resource-version", 0, "optimistic version from autonomy show (0 reads current)")
	if needsRole {
		command.Flags().String("role", "", "required approval role")
	} else {
		command.Flags().String("reason", "", "audited lifecycle reason")
	}
	return command
}

func cmdAuditEvents() *cobra.Command {
	command := &cobra.Command{Use: "audit-events", Short: "Query immutable v0.4.0 operational audit events", RunE: func(cmd *cobra.Command, _ []string) error {
		client, err := newClient(cmd)
		if err != nil {
			return err
		}
		query := url.Values{}
		for _, flag := range []string{"kind", "tenant", "cluster", "actor", "resource-id", "decision-id", "request-id", "action", "since", "until", "cursor"} {
			if value, _ := cmd.Flags().GetString(flag); value != "" {
				query.Set(strings.ReplaceAll(flag, "-", "_"), value)
			}
		}
		if limit, _ := cmd.Flags().GetInt("limit"); limit > 0 {
			query.Set("limit", strconv.Itoa(limit))
		}
		var response any
		path := "/api/v1/audit-events"
		if encoded := query.Encode(); encoded != "" {
			path += "?" + encoded
		}
		if err := client.do("GET", path, nil, &response); err != nil {
			return err
		}
		return printOperationJSON(cmd, response)
	}}
	for _, flag := range []string{"kind", "tenant", "cluster", "actor", "resource-id", "decision-id", "request-id", "action", "since", "until", "cursor"} {
		command.Flags().String(flag, "", "audit filter")
	}
	command.Flags().Int("limit", 0, "maximum audit events to return (1-1000)")
	return command
}

func actorOrLocalUserValue(actor string) string {
	if strings.TrimSpace(actor) != "" {
		return actor
	}
	if user := os.Getenv("USER"); user != "" {
		return user
	}
	return "unknown"
}
