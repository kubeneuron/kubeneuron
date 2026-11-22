// SPDX-License-Identifier: Apache-2.0

// Command kubeneuronctl is the operator CLI for KubeNeuron.
package main

import (
	"fmt"
	"io"
	"net/url"
	"os"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/crypto/bcrypt"
	"golang.org/x/term"

	"github.com/kubeneuron/kubeneuron/internal/inventory"
	"github.com/kubeneuron/kubeneuron/pkg/types"
)

var version = "dev"

func main() {
	if err := newRootCommand().Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

// newRootCommand assembles the full operator CLI. It is separate from main so
// tests can assert which commands ship without executing any of them.
func newRootCommand() *cobra.Command {
	root := &cobra.Command{
		Use:           "kubeneuronctl",
		Short:         "Operator CLI for KubeNeuron",
		Version:       version,
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.PersistentFlags().String("server", "http://localhost:8080", "controller base URL")
	root.PersistentFlags().String("token", "", "bearer token (prefer --token-file or KUBENEURONCTL_TOKEN)")
	root.PersistentFlags().String("token-file", "", "file containing the bearer token")

	root.AddCommand(
		cmdStatus(), cmdNodes(), cmdIncidents(), cmdReport(), cmdApprove(),
		cmdReject(), cmdAcknowledge(), cmdResolve(), cmdRemediate(), cmdPause(), cmdResume(),
		cmdPasswd(), cmdReadiness(), cmdCandidates(), cmdPreview(), cmdHealthCheck(),
		cmdSimulate(), cmdAutonomy(), cmdAuditEvents(), cmdRuntimeContracts(),
		cmdRuntimeQualifications(), cmdHealth(), cmdPools(),
	)
	return root
}

func cmdPasswd() *cobra.Command {
	cost := 0
	cmd := &cobra.Command{
		Use:   "passwd",
		Short: "Generate a bcrypt password hash for the panel users Secret",
		Long: `Reads a password (without echo when stdin is a terminal) and prints its
bcrypt hash — the value format for spec.auth.users Secret entries:

  kubectl -n <ns> create secret generic kubeneuron-panel-users \
    --from-literal=<username>="$(kubeneuronctl passwd)"`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			password, err := readPassword(cmd)
			if err != nil {
				return err
			}
			if len(password) == 0 {
				return fmt.Errorf("password is empty")
			}
			if len(password) > 72 {
				return fmt.Errorf("bcrypt limits passwords to 72 bytes")
			}
			hash, err := bcrypt.GenerateFromPassword(password, cost)
			if err != nil {
				return err
			}
			fmt.Println(string(hash))
			return nil
		},
	}
	cmd.Flags().IntVar(&cost, "cost", 12, "bcrypt cost factor (10-14 is sensible)")
	return cmd
}

func readPassword(cmd *cobra.Command) ([]byte, error) {
	stdin := int(os.Stdin.Fd())
	if term.IsTerminal(stdin) {
		fmt.Fprint(os.Stderr, "Password: ")
		password, err := term.ReadPassword(stdin)
		fmt.Fprintln(os.Stderr)
		return password, err
	}
	raw, err := io.ReadAll(io.LimitReader(os.Stdin, 4096))
	if err != nil {
		return nil, err
	}
	return []byte(strings.TrimRight(string(raw), "\r\n")), nil
}

func cmdStatus() *cobra.Command {
	return &cobra.Command{Use: "status", Short: "Fleet health summary", RunE: func(cmd *cobra.Command, _ []string) error {
		c, err := newClient(cmd)
		if err != nil {
			return err
		}
		var incidents []*types.Incident
		if err := c.do("GET", "/api/v1/incidents", nil, &incidents); err != nil {
			return err
		}
		var pause struct {
			Paused bool `json:"paused"`
		}
		if err := c.do("GET", "/api/v1/pause", nil, &pause); err != nil {
			return err
		}
		byState := map[types.IncidentState]int{}
		for _, inc := range incidents {
			byState[inc.State]++
		}
		out := cmd.OutOrStdout()
		_, _ = fmt.Fprintf(out, "automation paused: %v\n", pause.Paused)
		_, _ = fmt.Fprintf(out, "incidents: %d total\n", len(incidents))
		for _, state := range []types.IncidentState{
			types.StateOpen, types.StateObserving, types.StateEvaluating,
			types.StateAwaitingApproval, types.StateExecuting, types.StateVerifying,
			types.StateNeedsHuman, types.StateResolved, types.StateExpired, types.StateSuperseded,
		} {
			if n := byState[state]; n > 0 {
				_, _ = fmt.Fprintf(out, "  %-18s %d\n", state, n)
			}
		}
		var data map[string]string
		if err := c.do("GET", "/api/v1/inventory-summary", nil, &data); err != nil {
			_, _ = fmt.Fprintln(out, "GPU inventory unknown: controller summary unavailable")
			return nil
		}
		s, err := inventory.Parse(data, time.Now())
		if err != nil {
			return err
		}
		switch {
		case s.Diagnostic != "":
			_, _ = fmt.Fprintln(out, "GPU inventory:", inventory.DiagnosticMessage(s.Diagnostic))
		case s.ReportingNodes == 0:
			_, _ = fmt.Fprintf(out, "GPU inventory unknown: no fresh agent reports (%d nodes advertise accelerator resources)\n", s.AdvertisedNodes)
		case s.GPUs == 0 && s.AdvertisedNodes == 0:
			_, _ = fmt.Fprintln(out, "GPU inventory: agents report zero GPUs and no node advertises an accelerator resource")
		case s.GPUs == 0:
			_, _ = fmt.Fprintln(out, "GPU inventory: agents report zero GPUs; check host tooling, driver and nvidia-smi")
		default:
			_, _ = fmt.Fprintf(out, "GPU inventory: %d GPUs across %d reporting nodes\n", s.GPUs, s.ReportingNodes)
		}
		if !s.ResetConfirmed {
			_, _ = fmt.Fprintln(out, "Reset unconfirmed: no fresh DCGM reset attestation. See docs/quickstart.md#check-gpu-visibility.")
		} else {
			_, _ = fmt.Fprintln(out, "Reset attested on at least one GPU")
		}

		return nil
	}}
}

func cmdNodes() *cobra.Command {
	nodes := &cobra.Command{Use: "nodes", Short: "List nodes and their GPU state", RunE: func(cmd *cobra.Command, _ []string) error {
		c, err := newClient(cmd)
		if err != nil {
			return err
		}
		var nodes []*types.Node
		if err := c.do("GET", "/api/v1/nodes", nil, &nodes); err != nil {
			return err
		}
		w := tabwriter.NewWriter(cmd.OutOrStdout(), 2, 4, 2, ' ', 0)
		_, _ = fmt.Fprintln(w, "NAME\tPLATFORM\tGPUS\tPAUSED\tAGENT LAST SEEN")
		for _, n := range nodes {
			lastSeen := "never"
			if !n.AgentLastSeen.IsZero() {
				lastSeen = time.Since(n.AgentLastSeen).Round(time.Second).String() + " ago"
			}
			_, _ = fmt.Fprintf(w, "%s\t%s\t%d\t%v\t%s\n", n.Name, n.Platform, len(n.GPUs), n.Paused, lastSeen)
		}
		return w.Flush()
	}}
	nodes.AddCommand(cmdNodeTopology())
	return nodes
}

// cmdNodeTopology prints one node's read-only device topology classification
// (GET /api/v1/nodes/{node}/topology). It is the only `nodes` subcommand and
// it is read-only: nothing here can approve, change or request anything.
func cmdNodeTopology() *cobra.Command {
	topologyCmd := &cobra.Command{
		Use:   "topology <node>",
		Short: "Explain a node's device topology (whole-device, mig, time-sliced, dra-managed, mixed, unknown) and whether device-scoped actions are eligible",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := newClient(cmd)
			if err != nil {
				return err
			}
			var item map[string]any
			if err := c.do("GET", "/api/v1/nodes/"+url.PathEscape(args[0])+"/topology", nil, &item); err != nil {
				return err
			}
			if asJSON, _ := cmd.Flags().GetBool("json"); asJSON {
				return printOperationJSON(cmd, item)
			}
			w := tabwriter.NewWriter(cmd.OutOrStdout(), 2, 4, 2, ' ', 0)
			reasons := "-"
			if list, ok := item["reasons"].([]any); ok && len(list) > 0 {
				parts := make([]string, 0, len(list))
				for _, reason := range list {
					parts = append(parts, fmt.Sprint(reason))
				}
				reasons = strings.Join(parts, ",")
			}
			dra := "-"
			if summary, ok := item["dra"].(map[string]any); ok {
				dra = fmt.Sprint(summary["state"])
				if reason, ok := summary["reason"].(string); ok && reason != "" {
					dra += " (" + reason + ")"
				}
			}
			for _, row := range [][2]string{
				{"NODE", fmt.Sprint(item["node"])},
				{"TOPOLOGY", fmt.Sprint(item["topology"])},
				{"DEVICE ACTIONS", fmt.Sprint(item["device_actions"])},
				{"ATTRIBUTION", fmt.Sprint(item["attribution"])},
				{"REASONS", reasons},
				{"DRA", dra},
				{"OBSERVED AT", fmt.Sprint(item["observed_at"])},
				{"INPUTS DIGEST", fmt.Sprint(item["inputs_digest"])},
				{"VERSION", fmt.Sprint(item["version"])},
			} {
				_, _ = fmt.Fprintf(w, "%s\t%s\n", row[0], row[1])
			}
			return w.Flush()
		},
	}
	topologyCmd.Flags().Bool("json", false, "print the full topology-classification/v1 document as JSON")
	return topologyCmd
}

func cmdIncidents() *cobra.Command {
	c := &cobra.Command{Use: "incidents", Short: "List incidents", RunE: func(cmd *cobra.Command, _ []string) error {
		cl, err := newClient(cmd)
		if err != nil {
			return err
		}
		state, _ := cmd.Flags().GetString("state")
		path := "/api/v1/incidents"
		if state != "" {
			path += "?state=" + strings.ToUpper(state)
		}
		var incidents []*types.Incident
		if err := cl.do("GET", path, nil, &incidents); err != nil {
			return err
		}
		w := tabwriter.NewWriter(cmd.OutOrStdout(), 2, 4, 2, ' ', 0)
		_, _ = fmt.Fprintln(w, "ID\tSTATE\tCLASS\tNODE\tGPU\tPLAYBOOK\tSIGNALS\tAGE")
		for _, inc := range incidents {
			_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%d\t%s\n",
				inc.ID, inc.State, inc.Class, inc.Target.Node, inc.Target.GPUUUID,
				inc.Playbook, inc.SignalSeen, time.Since(inc.OpenedAt).Round(time.Second))
		}
		return w.Flush()
	}}
	c.Flags().StringP("state", "s", "", "filter by state (comma-separated)")

	show := &cobra.Command{Use: "show <id>", Short: "Incident detail with audit trail", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		cl, err := newClient(cmd)
		if err != nil {
			return err
		}
		var detail struct {
			RecoveryAvailability      *types.RecoveryAvailability  `json:"recovery_availability"`
			Incident                  *types.Incident              `json:"incident"`
			Audit                     []*types.AuditEntry          `json:"audit"`
			Evidence                  *types.IncidentEvidence      `json:"evidence"`
			NodeConditions            *types.NodeConditionSnapshot `json:"node_conditions"`
			NodeConditionsUnavailable string                       `json:"node_conditions_unavailable"`
			AffectedJobs              []types.AffectedJob          `json:"affected_jobs"`
			ObserveOnly               string                       `json:"observe_only"`
		}
		if err := cl.do("GET", "/api/v1/incidents/"+args[0], nil, &detail); err != nil {
			return err
		}
		out := cmd.OutOrStdout()
		inc := detail.Incident
		_, _ = fmt.Fprintf(out, "id:        %s\nstate:     %s\nclass:     %s\nnode:      %s\ngpu:       %s\nplaybook:  %s (step %d, attempt %d)\ndry-run:   %v\nsignals:   %d\nopened:    %s\n",
			inc.ID, inc.State, inc.Class, inc.Target.Node, inc.Target.GPUUUID,
			inc.Playbook, inc.StepIndex, inc.Attempt, inc.DryRun, inc.SignalSeen,
			inc.OpenedAt.Format(time.RFC3339))
		// The round is what approve, reject and incidents impact bind to; without
		// a notification channel this is the only place an operator can read it.
		if inc.State == types.StateAwaitingApproval && inc.ApprovalEpoch > 0 {
			_, _ = fmt.Fprintf(out, "approval:  round %d (use --round %d with approve, reject and incidents impact)\n", inc.ApprovalEpoch, inc.ApprovalEpoch)
		}
		// The current reason recovery is held comes first, before the evidence.
		recovery := detail.RecoveryAvailability
		if recovery == nil {
			recovery = types.RecoveryAvailabilityFromAudit(detail.Audit)
		}
		if recovery != nil {
			_, _ = fmt.Fprintf(out, "recovery:  %s [%s] (assessed %s)\nreason:    %s\n", recovery.Status, recovery.Source, recovery.ObservedAt.Format(time.RFC3339), recovery.Reason)
		}
		if snap := detail.NodeConditions; snap != nil {
			for _, condition := range snap.Conditions {
				if condition.Type == "Ready" && condition.Status == "False" {
					_, _ = fmt.Fprintln(out, "Node NotReady")
				}
			}
			_, _ = fmt.Fprintf(out, "Node conditions observed %s\n", snap.ObservedAt.Format(time.RFC3339))
			for _, condition := range snap.Conditions {
				_, _ = fmt.Fprintf(out, "  %s: %s\n", condition.Type, condition.Status)
			}
		} else if detail.NodeConditionsUnavailable == "NodeNotFound" {
			_, _ = fmt.Fprintln(out, "Node conditions: node no longer exists")
		} else if detail.NodeConditionsUnavailable != "" {
			_, _ = fmt.Fprintf(out, "Node conditions unavailable (%s)\n", detail.NodeConditionsUnavailable)
		}
		printEvidence(out, detail.Evidence)
		printTrainingRecovery(out, detail.AffectedJobs)
		if detail.ObserveOnly != "" {
			_, _ = fmt.Fprintf(out, "observe:   %s\n", detail.ObserveOnly)
		}
		for _, line := range authorisations(detail.Audit) {
			_, _ = fmt.Fprintln(out, line)
		}
		for _, line := range successorLines(inc.Successor, time.Now()) {
			_, _ = fmt.Fprintln(out, line)
		}
		if line := replacementLine(detail.Audit); line != "" {
			_, _ = fmt.Fprintln(out, line)
		}
		_, _ = fmt.Fprintln(out, "\naudit trail:")
		w := tabwriter.NewWriter(out, 2, 4, 2, ' ', 0)
		_, _ = fmt.Fprintln(w, "TIME\tFROM\tTO\tACTOR\tACTION\tRESULT")
		for _, e := range detail.Audit {
			_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n",
				e.Time.Format(time.RFC3339), e.FromState, e.ToState, e.Actor, e.Action, e.Result)
		}
		return w.Flush()
	}}
	c.AddCommand(show)
	c.AddCommand(cmdIncidentImpact())
	return c
}

// cmdIncidentImpact reads one approval round's impact preview: what the
// pending step and the disruptions downstream of it would touch, and the
// digest an approve may bind to (v0.8 development).
func cmdIncidentImpact() *cobra.Command {
	c := &cobra.Command{Use: "impact <incident-id>", Short: "Impact preview of the pending approval round", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		cl, err := newClient(cmd)
		if err != nil {
			return err
		}
		round, _ := cmd.Flags().GetInt("round")
		var preview map[string]any
		if err := cl.do("GET", impactPreviewPath(args[0], round), nil, &preview); err != nil {
			return err
		}
		if asJSON, _ := cmd.Flags().GetBool("json"); asJSON {
			return printOperationJSON(cmd, preview)
		}
		printImpactPreview(cmd.OutOrStdout(), preview)
		return nil
	}}
	c.Flags().Int("round", 0, "approval round (default: the incident's current round)")
	c.Flags().Bool("json", false, "print the full impact-preview/v1 document")
	return c
}

func impactPreviewPath(id string, round int) string {
	path := "/api/v1/incidents/" + url.PathEscape(id) + "/impact-preview"
	if round > 0 {
		path += "?round=" + strconv.Itoa(round)
	}
	return path
}

// printImpactPreview renders the summary table: identity, node and topology,
// the counts, groups, warnings, and the digest to approve with.
func printImpactPreview(out io.Writer, p map[string]any) {
	w := tabwriter.NewWriter(out, 2, 4, 2, ' ', 0)
	get := func(path ...string) any {
		var cur any = p
		for _, key := range path {
			m, ok := cur.(map[string]any)
			if !ok {
				return nil
			}
			cur = m[key]
		}
		return cur
	}
	str := func(v any) string {
		switch t := v.(type) {
		case nil:
			return "—"
		case float64:
			return strconv.FormatFloat(t, 'f', -1, 64)
		case bool:
			return strconv.FormatBool(t)
		case []any:
			parts := make([]string, 0, len(t))
			for _, item := range t {
				parts = append(parts, fmt.Sprint(item))
			}
			if len(parts) == 0 {
				return "—"
			}
			return strings.Join(parts, ", ")
		default:
			return fmt.Sprint(t)
		}
	}
	rows := [][2]string{
		{"INCIDENT", str(get("incident"))},
		{"ROUND", str(get("round"))},
		{"COMPUTED AT", str(get("computed_at"))},
		{"DIGEST", str(get("digest"))},
		{"NODE", str(get("node", "name"))},
		{"TOPOLOGY", str(get("node", "topology", "topology")) + " / device actions " + str(get("node", "topology", "device_actions"))},
		{"PENDING STEP", str(get("pending_step"))},
		{"DOWNSTREAM", str(get("downstream_disruptions"))},
		{"WORKLOADS", str(get("workloads", "total")) + " total, " + str(get("workloads", "gpu_holders")) + " GPU holders, " + str(get("workloads", "namespaces")) + " namespaces"},
		{"CHECKPOINT", str(get("workloads", "eligible_for_checkpoint")) + " eligible; ineligible " + str(get("workloads", "ineligible"))},
		{"GROUPS", str(get("workloads", "groups"))},
		{"ATTRIBUTION", str(get("devices", "attribution"))},
		{"WARNINGS", str(get("warnings"))},
	}
	if unavailable := get("unavailable"); unavailable != nil {
		rows = append(rows, [2]string{"UNAVAILABLE", str(get("unavailable", "reason"))})
	}
	for _, row := range rows {
		_, _ = fmt.Fprintf(w, "%s\t%s\n", row[0], row[1])
	}
	_ = w.Flush()
}

func decisionCommand(use, short, route string, withRound bool) *cobra.Command {
	c := &cobra.Command{Use: use, Short: short, Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		cl, err := newClient(cmd)
		if err != nil {
			return err
		}
		reason, _ := cmd.Flags().GetString("reason")
		body := map[string]any{"actor": actorOrLocalUser(cmd)}
		if reason != "" {
			body["reason"] = reason
		}
		if withRound {
			// Pin the decision to the approval round the notification showed:
			// the controller refuses it if a re-park has since replaced the
			// request, instead of binding the click to content the human
			// never saw. Omitted (0) keeps bind-to-current.
			round, _ := cmd.Flags().GetInt("round")
			if round > 0 {
				body["park_epoch"] = round
			}
			// The impact digest pins the decision to the preview the human
			// read (v0.8 development). It is never taken from the server at
			// approve time: an approve that fetched "the current preview"
			// and bound to it would approve whatever is current — a preview
			// another operator recomputed after this one was read — which is
			// the substitution the digest exists to refuse. With --round and
			// no --impact-digest, an approve reads the round's current
			// preview, prints it and STOPS, naming the exact command to
			// re-run; the notification's hint already carries the digest.
			// A controller without the route (404) or a round without a
			// computed preview sends the approve unbound, as before.
			digest, _ := cmd.Flags().GetString("impact-digest")
			if digest == "" && round > 0 && route == "/approve" {
				var preview map[string]any
				if err := cl.do("GET", impactPreviewPath(args[0], round), nil, &preview); err == nil {
					if d, _ := preview["digest"].(string); d != "" && preview["unavailable"] == nil {
						printImpactPreview(cmd.OutOrStdout(), preview)
						return fmt.Errorf("round %d of %s has impact preview %s; read it above and approve exactly that with:\n  kubeneuronctl approve %s --round %d --impact-digest %s",
							round, args[0], d, args[0], round, d)
					}
				}
			}
			if digest != "" {
				body["impact_digest"] = digest
			}
		}
		if err := cl.do("POST", "/api/v1/incidents/"+args[0]+route, body, nil); err != nil {
			return err
		}
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "%s: %s\n", strings.TrimPrefix(route, "/"), args[0])
		return nil
	}}
	c.Flags().String("actor", "", "audited actor (default: $USER)")
	c.Flags().String("reason", "", "reason recorded in the audit trail")
	if withRound {
		c.Flags().Int("round", 0, "approval round from the notification; refused if the request changed since")
		c.Flags().String("impact-digest", "", "digest of the impact preview that was read (from the notification or 'incidents impact'); with --round and a computed preview an approve without it prints the preview and stops")
	}
	return c
}

func cmdApprove() *cobra.Command {
	return decisionCommand("approve <incident-id>", "Approve a pending risky action", "/approve", true)
}

func cmdReject() *cobra.Command {
	return decisionCommand("reject <incident-id>", "Reject a pending risky action", "/reject", true)
}

func cmdAcknowledge() *cobra.Command {
	command := &cobra.Command{
		Use:   "acknowledge <incident-id>",
		Short: "Record ownership of an active incident without changing its state",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := newClient(cmd)
			if err != nil {
				return err
			}
			reason, _ := cmd.Flags().GetString("reason")
			version, _ := cmd.Flags().GetInt("resource-version")
			body := map[string]any{"actor": actorOrLocalUser(cmd), "reason": reason, "resource_version": version}
			var incident types.Incident
			if err := client.doHeaders("POST", "/api/v1/incidents/"+url.PathEscape(args[0])+"/acknowledge", body, &incident, map[string]string{"Idempotency-Key": operationKey()}); err != nil {
				return err
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "acknowledged: %s (resource version %d)\n", incident.ID, incident.Version)
			return err
		},
	}
	command.Flags().String("actor", "", "audited actor (default: $USER)")
	command.Flags().String("reason", "", "acknowledgement rationale recorded in audit")
	command.Flags().Int("resource-version", 0, "optimistic version from incidents show (0 reads current)")
	return command
}

func cmdResolve() *cobra.Command {
	command := &cobra.Command{
		Use:   "resolve <incident-id>",
		Short: "Manually resolve an incident",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := newClient(cmd)
			if err != nil {
				return err
			}
			reason, _ := cmd.Flags().GetString("reason")
			version, _ := cmd.Flags().GetInt("resource-version")
			body := map[string]any{"actor": actorOrLocalUser(cmd), "reason": reason, "resource_version": version}
			settled, _ := cmd.Flags().GetBool("cloud-operation-settled")
			if settled {
				if strings.TrimSpace(reason) == "" {
					return fmt.Errorf("--cloud-operation-settled requires --reason")
				}
				body["cloud_operation_settled"] = true
			}
			var incident types.Incident
			if err := client.doHeaders("POST", "/api/v1/incidents/"+url.PathEscape(args[0])+"/resolve", body, &incident, map[string]string{"Idempotency-Key": operationKey()}); err != nil {
				return err
			}
			if hint := incident.CloudSettlementHint(); hint != "" {
				_, err = fmt.Fprintf(cmd.OutOrStdout(), "Incident %s is closed.\n%s\n", incident.ID, hint)
				return err
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "resolved: %s (resource version %d)\n", incident.ID, incident.Version)
			return err
		},
	}
	command.Flags().Bool("cloud-operation-settled", false, "Record that you verified the cloud operation completed or was canceled, and release the node for repair. KubeNeuron does not verify this. Include what you checked in --reason.")
	command.Flags().String("actor", "", "audited actor (default: $USER)")
	command.Flags().String("reason", "", "resolution rationale recorded in audit")
	command.Flags().Int("resource-version", 0, "optimistic version from incidents show (0 reads current)")
	return command
}

func cmdRemediate() *cobra.Command {
	c := &cobra.Command{Use: "remediate <node>", Short: "Manually open an incident for a node", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		cl, err := newClient(cmd)
		if err != nil {
			return err
		}
		class, _ := cmd.Flags().GetString("class")
		if class == "" {
			return fmt.Errorf("--class is required (the policy for that class selects the playbook)")
		}
		gpuUUID, _ := cmd.Flags().GetString("gpu-uuid")
		gpu, _ := cmd.Flags().GetInt("gpu")
		body := map[string]any{
			"node": args[0], "class": class, "actor": actorOrLocalUser(cmd),
		}
		if gpuUUID != "" {
			body["gpu_uuid"] = gpuUUID
		}
		if gpu >= 0 {
			body["gpu_index"] = gpu
		}
		if err := cl.do("POST", "/api/v1/incidents", body, nil); err != nil {
			return err
		}
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "manual signal accepted for %s (class %s)\n", args[0], class)
		return nil
	}}
	c.Flags().String("class", "", "problem class, e.g. ecc-dbe (required)")
	c.Flags().String("gpu-uuid", "", "GPU UUID (for gpu-scoped playbooks)")
	c.Flags().Int("gpu", -1, "GPU index (for gpu-scoped playbooks)")
	c.Flags().String("actor", "", "audited actor (default: $USER)")
	return c
}

func cmdPause() *cobra.Command {
	c := &cobra.Command{Use: "pause", Short: "Pause all automated remediation (big red button)", RunE: func(cmd *cobra.Command, _ []string) error {
		cl, err := newClient(cmd)
		if err != nil {
			return err
		}
		if err := cl.do("POST", "/api/v1/pause", map[string]string{"actor": actorOrLocalUser(cmd)}, nil); err != nil {
			return err
		}
		_, _ = fmt.Fprintln(cmd.OutOrStdout(), "automation paused")
		return nil
	}}
	c.Flags().String("actor", "", "audited actor (default: $USER)")
	return c
}

func cmdResume() *cobra.Command {
	c := &cobra.Command{Use: "resume", Short: "Resume automated remediation", RunE: func(cmd *cobra.Command, _ []string) error {
		cl, err := newClient(cmd)
		if err != nil {
			return err
		}
		if err := cl.do("DELETE", "/api/v1/pause?actor="+url.QueryEscape(actorOrLocalUser(cmd)), nil, nil); err != nil {
			return err
		}
		_, _ = fmt.Fprintln(cmd.OutOrStdout(), "automation resumed")
		return nil
	}}
	c.Flags().String("actor", "", "audited actor (default: $USER)")
	return c
}

// authorisations says who let each executed step run: a person's approval or
// an automatic policy. A step started by the controller on its own (one that
// needs no approval) is not listed.
func authorisations(trail []*types.AuditEntry) []string {
	var lines []string
	for _, e := range trail {
		if e.ToState != types.StateExecuting || e.FromState == types.StateExecuting || e.Actor == "" || e.Actor == "system" {
			continue
		}
		if name, ok := strings.CutPrefix(e.Actor, "policy:"); ok {
			lines = append(lines, fmt.Sprintf("step %-12s authorised by policy %s", e.Action, name))
			continue
		}
		lines = append(lines, fmt.Sprintf("step %-12s approved by %s", e.Action, e.Actor))
	}
	return lines
}

// successorLines describes an add-first successor: its node, where its
// operation stands, and how long two nodes have run (both are billed).
func successorLines(s *types.SuccessorRecord, now time.Time) []string {
	if s == nil {
		return nil
	}
	status := "open"
	if s.Outcome != "" {
		status = s.Outcome
	}
	node := s.Node
	if node == "" {
		node = "(not registered yet)"
	}
	lines := []string{fmt.Sprintf("successor: %s in pool %s, phase %s, %s (operation %s)", node, s.Pool, s.Phase, status, s.Operation)}
	if s.ReadyAt != nil {
		end := now
		if s.EndedAt != nil {
			end = *s.EndedAt
		}
		lines = append(lines, fmt.Sprintf("overlap:   two nodes ran for %s from %s", end.Sub(*s.ReadyAt).Round(time.Second), s.ReadyAt.Format(time.RFC3339)))
	}
	if s.Reason != "" {
		lines = append(lines, "why:       "+s.Reason)
	}
	return lines
}

// replacementLine names the node that replaced the incident's target, from
// the resolution that confirmed it, and how it was tied to the replacement.
func replacementLine(audit []*types.AuditEntry) string {
	for i := len(audit) - 1; i >= 0; i-- {
		e := audit[i]
		if e == nil || e.FromState == e.ToState || e.ToState == "" {
			continue
		}
		if e.Action != "resolve" || e.ToState != types.StateResolved || e.Params["infrastructure_evidence"] != "verified_replacement_node" {
			return ""
		}
		how := "named by the cloud operation"
		if e.Params["replacement_correlation"] == "pool" {
			how = "the only node that joined its pool after the removal"
		}
		return fmt.Sprintf("replaced:  node %s (uid %s), %s", e.Params["replacement_node"], e.Params["replacement_node_uid"], how)
	}
	return ""
}

// printEvidence prints the evidence block of an incident. No block is
// printed as unknown: a missing observation is never shown as healthy.
func printEvidence(out io.Writer, ev *types.IncidentEvidence) {
	if ev == nil {
		_, _ = fmt.Fprintln(out, "fault:     unknown (no evidence recorded)")
		return
	}
	loc := ev.Location
	_, _ = fmt.Fprintf(out, "fault:     %s; rule: %s\n", loc.Location, loc.Rule)
	if loc.Group != "" {
		_, _ = fmt.Fprintf(out, "group:     %s; incidents: %s\n", loc.Group, strings.Join(loc.Members, ", "))
	}
	for _, line := range loc.Evidence {
		_, _ = fmt.Fprintf(out, "  %s\n", line)
	}
	_, _ = fmt.Fprintf(out, "software:  %s%s\n", ev.Software.State, reasonSuffix(ev.Software.Reason))
	for _, r := range ev.Software.Resources {
		_, _ = fmt.Fprintf(out, "  %s: %d of %d allocatable\n", r.Name, r.Allocatable, r.Capacity)
	}
	for _, comp := range ev.Software.Components {
		_, _ = fmt.Fprintf(out, "  %s: %s\n", comp.Name, comp.State)
	}
	_, _ = fmt.Fprintf(out, "network:   %s%s\n", ev.Network.State, reasonSuffix(ev.Network.Reason))
	for _, r := range ev.Network.Resources {
		_, _ = fmt.Fprintf(out, "  %s: %d of %d allocatable\n", r.Name, r.Allocatable, r.Capacity)
	}
	if p := ev.Network.Plugin; p != nil {
		_, _ = fmt.Fprintf(out, "  %s: %s\n", p.Name, p.State)
	}
}

func reasonSuffix(reason string) string {
	if reason == "" {
		return ""
	}
	return " (" + reason + ")"
}
