package cmd

import (
	"context"
	"errors"
	"fmt"

	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/daemonclient"
	"go.kenn.io/msgvault/internal/emailtags"
)

type messageArchiveBackend interface {
	SupportsAPISchemaVersion(ctx context.Context, version string) (bool, error)
	ArchiveMessage(ctx context.Context, id int64, change emailtags.ArchiveChange) (*emailtags.ArchiveResult, error)
}

func newMessageArchiveCommand(archived bool, backend messageArchiveBackend) *cobra.Command {
	var id int64
	var dryRun, asJSON, wholeChat bool
	name, short := "archive", "Archive a Gmail message or a Beeper chat"
	if !archived {
		name, short = "unarchive", "Restore a Gmail message or a Beeper chat to its inbox"
	}
	cmd := &cobra.Command{
		Use: name + " --message <id>", Short: short, Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if id <= 0 {
				return errors.New("--message must be a positive archived message ID")
			}
			client := backend
			if client == nil {
				st, _, err := OpenHTTPStore(cmd.Context())
				if err != nil {
					return err
				}
				defer func() { _ = st.Close() }()
				client = st
			}
			supported, err := client.SupportsAPISchemaVersion(cmd.Context(), daemonclient.MessageArchiveMinAPISchemaVersion)
			if err != nil {
				return fmt.Errorf("check daemon archive support: %w", err)
			}
			if !supported {
				return fmt.Errorf("upgrade the daemon to API schema %s or newer for message archiving", daemonclient.MessageArchiveMinAPISchemaVersion)
			}
			result, err := client.ArchiveMessage(cmd.Context(), id, emailtags.ArchiveChange{Archived: &archived, DryRun: dryRun, WholeChat: wholeChat})
			if asJSON {
				if writeErr := writeMessageTagJSON(cmd, result, err, emailtags.Failure("request_failed", "Cannot set provider archive state", nil, err), "write message archive JSON"); writeErr != nil {
					return writeErr
				}
				return err
			}
			if err != nil {
				return err
			}
			if result.ChatID != "" {
				_, err = fmt.Fprintf(cmd.OutOrStdout(), "Beeper chat %q: archived=%t changed=%t dry_run=%t\n", result.ChatID, result.Archived, result.Changed, result.DryRun)
				if err != nil {
					return fmt.Errorf("write Beeper archive result: %w", err)
				}
				return nil
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "Message %d: archived=%t changed=%t dry_run=%t\n", id, result.Archived, result.Changed, result.DryRun)
			if err != nil {
				return fmt.Errorf("write message archive result: %w", err)
			}
			return nil
		},
	}
	cmd.Flags().Int64Var(&id, "message", 0, "Archived message ID")
	cmd.Flags().BoolVar(&wholeChat, "whole-chat", false, "Archive the entire Beeper chat containing this message")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "Preview without writing")
	cmd.Flags().BoolVar(&asJSON, flagJSON, false, "Output result as JSON")
	return cmd
}

func init() {
	rootCmd.AddCommand(newMessageArchiveCommand(true, nil), newMessageArchiveCommand(false, nil))
}
