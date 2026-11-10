package cmd

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"

	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/daemonclient"
	"go.kenn.io/msgvault/internal/emailtags"
)

type messageArchiveBackend interface {
	SupportsAPISchemaVersion(context.Context, string) (bool, error)
	ArchiveMessage(context.Context, int64, emailtags.ArchiveChange) (*emailtags.ArchiveResult, error)
}

func newMessageArchiveCommand(archived bool, backend messageArchiveBackend) *cobra.Command {
	var id int64
	var dryRun, asJSON bool
	name, short := "archive", "Archive one Gmail message"
	if !archived {
		name, short = "unarchive", "Restore one Gmail message to its inbox"
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
			result, err := client.ArchiveMessage(cmd.Context(), id, emailtags.ArchiveChange{Archived: &archived, DryRun: dryRun})
			if asJSON {
				var value any = result
				if err != nil {
					if failure, ok := errors.AsType[*emailtags.MessageTagError](err); ok {
						value = failure
					} else {
						value = emailtags.Failure("request_failed", "Cannot set provider archive state", nil, err)
					}
				}
				data, encodeErr := json.Marshal(value)
				if encodeErr != nil {
					return encodeErr
				}
				if _, writeErr := fmt.Fprintln(cmd.OutOrStdout(), string(data)); writeErr != nil {
					return writeErr
				}
				return err
			}
			if err != nil {
				return err
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "Message %d: archived=%t changed=%t dry_run=%t\n", id, result.Archived, result.Changed, result.DryRun)
			return err
		},
	}
	cmd.Flags().Int64Var(&id, "message", 0, "Archived message ID")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "Preview without writing")
	cmd.Flags().BoolVar(&asJSON, "json", false, "Output result as JSON")
	return cmd
}

func init() {
	rootCmd.AddCommand(newMessageArchiveCommand(true, nil), newMessageArchiveCommand(false, nil))
}
