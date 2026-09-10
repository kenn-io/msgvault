package cmd

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/daemonclient"
)

var (
	agentTokenLabel       string
	agentTokenPermissions []string
	agentTokenSourceIDs   string // comma-separated source IDs
	agentTokenJSON        bool
)

var agentTokenCmd = &cobra.Command{
	Use:   "agent-token",
	Short: "Manage restricted agent grant tokens",
	Long: "Create, list, and revoke restricted agent grant tokens.\n\n" +
		"Agent tokens allow delegated callers (e.g. AI agents) to perform a\n" +
		"limited set of operations on behalf of the archive owner without\n" +
		"exposing the full owner API key. Each token declares the permissions\n" +
		"and source IDs it may access. A grant is valid until revoked or until\n" +
		"the daemon restarts.\n\n" +
		"Requires agent_access = true and api_key to be set in config.toml.",
}

var agentTokenIssueCmd = &cobra.Command{
	Use:   "issue",
	Short: "Issue a new agent grant token",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		if agentTokenLabel == "" {
			return fmt.Errorf("--label is required")
		}
		sourceIDs, err := parseAgentTokenSourceIDs(strings.Split(agentTokenSourceIDs, ","))
		if err != nil {
			return err
		}
		client, _, err := OpenHTTPStore(cmd.Context())
		if err != nil {
			return err
		}
		result, err := client.IssueAgentToken(
			cmd.Context(),
			agentTokenLabel,
			agentTokenPermissions,
			sourceIDs,
		)
		if err != nil {
			return err
		}
		if agentTokenJSON {
			return json.NewEncoder(cmd.OutOrStdout()).Encode(result)
		}
		printAgentTokenIssueResult(cmd, result)
		return nil
	},
}

var agentTokenListCmd = &cobra.Command{
	Use:   "list",
	Short: "List active agent grant tokens",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		client, _, err := OpenHTTPStore(cmd.Context())
		if err != nil {
			return err
		}
		tokens, err := client.ListAgentTokens(cmd.Context())
		if err != nil {
			return err
		}
		if agentTokenJSON {
			return json.NewEncoder(cmd.OutOrStdout()).Encode(map[string]any{"tokens": tokens})
		}
		printAgentTokenList(cmd, tokens)
		return nil
	},
}

var agentTokenRevokeCmd = &cobra.Command{
	Use:   "revoke <token-id>",
	Short: "Revoke an agent grant token by ID",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		id := strings.TrimSpace(args[0])
		if id == "" {
			return fmt.Errorf("token ID must not be empty")
		}
		client, _, err := OpenHTTPStore(cmd.Context())
		if err != nil {
			return err
		}
		if err := client.RevokeAgentToken(cmd.Context(), id); err != nil {
			return err
		}
		_, _ = fmt.Fprintln(cmd.OutOrStdout(), "Token revoked.")
		return nil
	},
}

// parseAgentTokenSourceIDs converts a slice of string args into int64 IDs.
func parseAgentTokenSourceIDs(raw []string) ([]int64, error) {
	ids := make([]int64, 0, len(raw))
	for _, s := range raw {
		// Support comma-separated values in a single flag value.
		for _, part := range strings.Split(s, ",") {
			part = strings.TrimSpace(part)
			if part == "" {
				continue
			}
			id, err := strconv.ParseInt(part, 10, 64)
			if err != nil {
				return nil, fmt.Errorf("invalid source ID %q: %w", part, err)
			}
			ids = append(ids, id)
		}
	}
	return ids, nil
}

func printAgentTokenIssueResult(cmd *cobra.Command, r *daemonclient.AgentTokenIssueResult) {
	w := cmd.OutOrStdout()
	_, _ = fmt.Fprintf(w, "ID:          %s\n", r.ID)
	_, _ = fmt.Fprintf(w, "Label:       %s\n", r.Label)
	_, _ = fmt.Fprintf(w, "Permissions: %s\n", strings.Join(r.Permissions, ", "))
	if len(r.Sources) > 0 {
		parts := make([]string, len(r.Sources))
		for i, s := range r.Sources {
			parts[i] = fmt.Sprintf("%d (%s)", s.ID, s.Identifier)
		}
		_, _ = fmt.Fprintf(w, "Sources:     %s\n", strings.Join(parts, ", "))
	}
	_, _ = fmt.Fprintf(w, "Created:     %s\n", r.CreatedAt.Format(time.RFC3339))
	_, _ = fmt.Fprintf(w, "Valid until: revoked or daemon restart\n")
	if r.DaemonURL != "" {
		_, _ = fmt.Fprintf(w, "Daemon URL:  %s\n", r.DaemonURL)
	}
	_, _ = fmt.Fprintf(w, "\nSecret (store immediately — not shown again):\n%s\n", r.Secret)
}

func printAgentTokenList(cmd *cobra.Command, tokens []daemonclient.AgentTokenView) {
	if len(tokens) == 0 {
		_, _ = fmt.Fprintln(cmd.OutOrStdout(), "No active agent tokens.")
		return
	}
	tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "ID\tLABEL\tPERMISSIONS\tSOURCES\tCREATED")
	for _, t := range tokens {
		sourceParts := make([]string, len(t.Sources))
		for i, s := range t.Sources {
			sourceParts[i] = fmt.Sprintf("%d/%s/%s", s.ID, s.Type, s.Identifier)
		}
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n",
			t.ID,
			t.Label,
			strings.Join(t.Permissions, ","),
			strings.Join(sourceParts, ";"),
			t.CreatedAt.Format(time.RFC3339),
		)
	}
	_ = tw.Flush()
}

func init() {
	rootCmd.AddCommand(agentTokenCmd)
	agentTokenCmd.AddCommand(agentTokenIssueCmd, agentTokenListCmd, agentTokenRevokeCmd)

	agentTokenIssueCmd.Flags().StringVar(&agentTokenLabel, "label", "",
		"Human-readable label for the token (required)")
	agentTokenIssueCmd.Flags().StringSliceVar(&agentTokenPermissions, "permissions", nil,
		"Comma-separated list of permissions to grant (e.g. draft.create)")
	agentTokenIssueCmd.Flags().StringVar(&agentTokenSourceIDs, "source-ids", "",
		"Comma-separated list of source IDs the token may access")
	agentTokenIssueCmd.Flags().BoolVar(&agentTokenJSON, flagJSON, false, "Output as JSON")
	agentTokenListCmd.Flags().BoolVar(&agentTokenJSON, flagJSON, false, "Output as JSON")
}
