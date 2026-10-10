package cmd

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/mcpevents"
)

func newMCPEventsCommand() *cobra.Command {
	events := &cobra.Command{Use: "events", Short: "Inspect durable MCP webhook subscriptions"}
	var jsonOutput bool
	status := &cobra.Command{Use: "status", Short: "List subscription state and delivery outcomes", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		if isAgentMode(invocationFromCommand(cmd)) {
			return errors.New("MCP Events status requires the daemon owner credential")
		}
		client, _, err := OpenHTTPStore(cmd.Context())
		if err != nil {
			return fmt.Errorf("open daemon: %w", err)
		}
		defer func() { _ = client.Close() }()
		rows, err := client.MCPEventsStatus(cmd.Context())
		if err != nil {
			return err
		}
		return writeMCPEventsStatus(cmd.OutOrStdout(), rows, jsonOutput)
	}}
	status.Flags().BoolVar(&jsonOutput, "json", false, "Print safe subscription status as JSON")
	events.AddCommand(status)
	return events
}
func writeMCPEventsStatus(out io.Writer, rows []mcpevents.SubscriptionStatus, jsonOutput bool) error {
	if rows == nil {
		rows = []mcpevents.SubscriptionStatus{}
	}
	if jsonOutput {
		return json.MarshalEncode(jsontext.NewEncoder(out), rows, json.Deterministic(true))
	}
	if len(rows) == 0 {
		_, err := fmt.Fprintln(out, "No MCP Events subscriptions.")
		if err != nil {
			return fmt.Errorf("write MCP Events status: %w", err)
		}
		return nil
	}
	for _, row := range rows {
		expiry := time.UnixMilli(row.RefreshBefore).UTC().Format(time.RFC3339)
		_, err := fmt.Fprintf(out, "%s %s %s:%s\n  state=%s reason=%s expires=%s cursor=%d:%d\n  delivery=%s pending_attempt=%d dead_letters=%d loop_guard_skips=%d\n", row.ID, row.Name, row.ScopeKind, row.ScopeID, row.State, row.StopReason, expiry, row.CursorEpoch, row.CursorSeq, row.LastOutcome, row.PendingAttempt, row.DeadLetterCount, row.LoopGuardSkips)
		if err != nil {
			return fmt.Errorf("write MCP Events status: %w", err)
		}
	}
	return nil
}
