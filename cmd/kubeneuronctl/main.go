// Command kubeneuronctl is the operator CLI for KubeNeuron.
package main

import (
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/crypto/bcrypt"
	"golang.org/x/term"

	"github.com/kubeneuron/kubeneuron/pkg/types"
)

var version = "dev"

func main() {
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
		cmdStatus(), cmdNodes(), cmdIncidents(), cmdApprove(), cmdReject(),
		cmdResolve(), cmdRemediate(), cmdPause(), cmdResume(), cmdPasswd(),
	)

	if err := root.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
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
			types.StateNeedsHuman, types.StateResolved, types.StateExpired,
		} {
			if n := byState[state]; n > 0 {
				_, _ = fmt.Fprintf(out, "  %-18s %d\n", state, n)
			}
		}
		return nil
	}}
}

func cmdNodes() *cobra.Command {
	return &cobra.Command{Use: "nodes", Short: "List nodes and their GPU state", RunE: func(cmd *cobra.Command, _ []string) error {
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
			Incident *types.Incident     `json:"incident"`
			Audit    []*types.AuditEntry `json:"audit"`
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
	return c
}

func decisionCommand(use, short, route string) *cobra.Command {
	c := &cobra.Command{Use: use, Short: short, Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		cl, err := newClient(cmd)
		if err != nil {
			return err
		}
		reason, _ := cmd.Flags().GetString("reason")
		body := map[string]string{"actor": actorOrLocalUser(cmd)}
		if reason != "" {
			body["reason"] = reason
		}
		if err := cl.do("POST", "/api/v1/incidents/"+args[0]+route, body, nil); err != nil {
			return err
		}
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "%s: %s\n", strings.TrimSuffix(route, "/"), args[0])
		return nil
	}}
	c.Flags().String("actor", "", "audited actor (default: $USER)")
	c.Flags().String("reason", "", "reason recorded in the audit trail")
	return c
}

func cmdApprove() *cobra.Command {
	return decisionCommand("approve <incident-id>", "Approve a pending risky action", "/approve")
}

func cmdReject() *cobra.Command {
	return decisionCommand("reject <incident-id>", "Reject a pending risky action", "/reject")
}

func cmdResolve() *cobra.Command {
	return decisionCommand("resolve <incident-id>", "Manually resolve an incident", "/resolve")
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
		if err := cl.do("DELETE", "/api/v1/pause?actor="+actorOrLocalUser(cmd), nil, nil); err != nil {
			return err
		}
		_, _ = fmt.Fprintln(cmd.OutOrStdout(), "automation resumed")
		return nil
	}}
	c.Flags().String("actor", "", "audited actor (default: $USER)")
	return c
}
