package cmd

import (
	"context"
	"errors"
	"fmt"

	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/linkedin"
)

func newImportLinkedInCmd() *cobra.Command {
	var opts linkedin.Options
	var jsonOutput bool
	command := &cobra.Command{
		Use:   "import-linkedin <identifier> <archive>",
		Short: "Import a LinkedIn member data export",
		Long: `Import messages and your own comments and posts from a LinkedIn data export
ZIP or extracted folder. Connections.csv adds people without creating messages.

Use the same identifier for newer exports; repeated rows update in place.
Attachment URLs are kept as links, and no network requests are made. Repeat an
interrupted import to continue.

Examples:
  msgvault import-linkedin linkedin ~/Downloads/Basic_LinkedInDataExport.zip --me https://www.linkedin.com/in/example
  msgvault import-linkedin linkedin ./linkedin-export --me https://www.linkedin.com/in/example --json`,
		Args: cobra.ExactArgs(2),
		PreRunE: func(cmd *cobra.Command, args []string) error {
			if _, err := linkedin.ProfileURL(opts.Me); err != nil {
				return usageErr(cmd, fmt.Errorf("--me: %w", err))
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			if !isDaemonCLISubprocess() {
				return runDaemonCLICommandHTTPFromCobraWithLocalFiles(cmd, args)
			}
			opts.Identifier = args[0]
			return runImportLinkedIn(cmd, args[1], opts, jsonOutput)
		},
	}
	command.Flags().StringVar(&opts.Me, "me", "", "your LinkedIn member profile URL (required)")
	command.Flags().StringVar(&opts.DisplayName, "display-name", "", "your display name for posts and comments")
	command.Flags().BoolVar(&opts.IncludeDrafts, "include-drafts", false, "include unsent message drafts")
	command.Flags().BoolVar(&jsonOutput, flagJSON, false, "Output structured JSON")
	return command
}

func runImportLinkedIn(cmd *cobra.Command, archive string, opts linkedin.Options, jsonOutput bool) error {
	state := invocationFromCommand(cmd)
	if state == nil || state.cfg == nil {
		return errors.New("configuration is unavailable")
	}
	st, cleanup, err := openWritableStoreAndInitForIngestInvocation(cmd.Context(), state)
	if err != nil {
		return err
	}
	defer cleanup()
	ctx, stop := withInterruptCancel(cmd, "\nInterrupted. Re-run to continue.")
	defer stop()
	summary, importErr := linkedin.Import(ctx, st, archive, opts)
	if summary == nil || summary.SourceID == 0 {
		return importErr
	}
	// The importer confirms the --me identity itself; the identifier is a
	// nickname, not an address, so no default identity is added here.
	migrationErr := runPostSourceCreateMigrationsForInvocation(context.WithoutCancel(ctx), st, state)
	// Through the daemon, the daemon refreshes the cache once the import
	// returns (see manualSyncCLICommand); a --local run refreshes it here.
	cacheErr := rebuildCacheAfterManualSync(state.cfg.DatabaseDSN(), state)
	if err := errors.Join(importErr, migrationErr, cacheErr); err != nil {
		return err
	}
	if jsonOutput {
		return printJSONTo(cmd.OutOrStdout(), summary)
	}
	_, err = fmt.Fprintf(cmd.OutOrStdout(), "Messages: %d added, %d updated\nDrafts skipped: %d\nConnections: %d\n",
		summary.MessagesAdded, summary.MessagesUpdated, summary.DraftsSkipped, summary.ConnectionsProcessed)
	if err != nil {
		return fmt.Errorf("write LinkedIn import summary: %w", err)
	}
	return nil
}

func init() { rootCmd.AddCommand(newImportLinkedInCmd()) }
