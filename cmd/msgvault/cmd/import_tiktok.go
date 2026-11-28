package cmd

import (
	"context"
	"errors"
	"fmt"

	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/tiktokexport"
)

func newImportTikTokCmd() *cobra.Command {
	var jsonOutput bool
	command := &cobra.Command{
		Use:   "import-tiktok <identifier> <archive>",
		Short: "Import TikTok DMs and your own comments from a data export",
		Long: `Import a TikTok "Download your data" ZIP or extracted folder.

Include Profile Information in the export so your own messages count as sent
by you. JSON sections take precedence; TXT fills sections and links JSON lacks.
Shared post and media URLs are kept as links. Repeat an interrupted import to
continue. See the TikTok export guide for accepted layouts.

Examples:
  msgvault import-tiktok tiktok ./tiktok-export.zip
  msgvault import-tiktok tiktok ./tiktok-export --json`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			if !isDaemonCLISubprocess() {
				return runDaemonCLICommandHTTPFromCobraWithLocalFiles(cmd, args)
			}
			return runImportTikTok(cmd, args[0], args[1], jsonOutput)
		},
	}
	command.Flags().BoolVar(&jsonOutput, flagJSON, false, "Output structured JSON")
	return command
}

func runImportTikTok(cmd *cobra.Command, identifier, archive string, jsonOutput bool) error {
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
	summary, importErr := tiktokexport.Import(ctx, st, identifier, archive)
	if summary == nil || summary.SourceID == 0 {
		return importErr
	}
	// The importer confirms the profile identity from the export itself.
	migrationErr := runPostSourceCreateMigrationsForInvocation(context.WithoutCancel(ctx), st, state)
	// Through the daemon, the daemon refreshes the cache once the import
	// returns (see manualSyncCLICommand); a --local run refreshes it here.
	cacheErr := rebuildCacheAfterManualSync(state.cfg.DatabaseDSN(), state)
	if err := errors.Join(importErr, migrationErr, cacheErr); err != nil {
		return err
	}
	if summary.UnknownSections > 0 {
		_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "Warning: skipped %d unknown export sections or files.\n", summary.UnknownSections)
	}
	if jsonOutput {
		return printJSONTo(cmd.OutOrStdout(), summary)
	}
	_, err = fmt.Fprintf(cmd.OutOrStdout(), "Messages: %d added, %d processed\nConversations: %d\nFrom you: %d\n",
		summary.MessagesAdded, summary.MessagesProcessed, summary.Conversations, summary.FromMe)
	if err != nil {
		return fmt.Errorf("write TikTok import summary: %w", err)
	}
	return nil
}

func init() { rootCmd.AddCommand(newImportTikTokCmd()) }
