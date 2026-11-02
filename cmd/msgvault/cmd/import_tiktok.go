package cmd

import (
	"errors"
	"fmt"

	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/tiktokexport"
)

func newImportTikTokCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "tiktok <identifier> <archive>",
		Short: "Import TikTok DMs and own comments from a data export",
		Long: `Import a TikTok Download your data ZIP or extracted directory.

Include Profile Information so the account owner's messages count as me.
JSON takes precedence per chat and comment section; TXT fills missing sections.
Shared post and media URLs are retained as links. Files are read on the daemon
host. This unreleased importer was checked against published field descriptions,
not a real export. See docs/usage/tiktok-export.md for accepted layouts.`,
		PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
			// Preserve the root lifecycle while formatting configuration failures in
			// the same way as this command's argument and import failures.
			root := cmd.Root()
			if root != cmd && root.PersistentPreRunE != nil {
				if err := root.PersistentPreRunE(cmd, args); err != nil {
					return reportTikTokError(cmd, err)
				}
			}
			return nil
		},
		Args: func(cmd *cobra.Command, args []string) error {
			if err := cobra.ExactArgs(2)(cmd, args); err != nil {
				return reportTikTokError(cmd, err)
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			var err error
			if !isDaemonCLISubprocess() {
				err = runDaemonCLICommandHTTPFromCobra(cmd, args)
			} else {
				err = runImportTikTok(cmd, args[0], args[1])
			}
			if err != nil {
				return reportTikTokError(cmd, err)
			}
			return nil
		},
	}
	cmd.Flags().Bool(flagJSON, false, "Output import summary and errors as JSON")
	cmd.SetFlagErrorFunc(func(cmd *cobra.Command, err error) error { return reportTikTokError(cmd, err) })
	return addManualSyncCacheFlags(cmd)
}

func reportTikTokError(cmd *cobra.Command, err error) error {
	if cmd.SilenceErrors {
		return err
	} // A daemon child already streamed its error.
	hint := "use msgvault import tiktok <identifier> <archive>; include Profile Information and request JSON format"
	jsonOutput, _ := cmd.Flags().GetBool(flagJSON)
	cmd.SilenceErrors = true
	cmd.SilenceUsage = true
	if jsonOutput {
		_ = printJSONTo(cmd.ErrOrStderr(), map[string]string{"error": err.Error(), "hint": hint})
	} else {
		_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "error: %s\nhint: %s\n", err, hint)
	}
	return err
}

func runImportTikTok(cmd *cobra.Command, identifier, filename string) error {
	state := invocationFromCommand(cmd)
	if state == nil || state.cfg == nil {
		return errors.New("configuration is unavailable")
	}
	st, cleanup, err := openWritableStoreAndInitForIngestInvocation(state)
	if err != nil {
		return err
	}
	defer cleanup()
	ctx, stop := withInterruptCancel(cmd, "\nInterrupted. Re-run the import to continue.")
	defer stop()
	summary, err := tiktokexport.Import(ctx, st, identifier, filename)
	if err != nil {
		return err
	}
	if err := runPostSourceCreateMigrationsForInvocation(st, state); err != nil {
		return fmt.Errorf("post-source-create migrations: %w", err)
	}
	if err := rebuildCacheAfterManualSync(state.cfg.DatabaseDSN(), state); err != nil {
		return err
	}
	if summary.UnknownSections > 0 {
		_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "Warning: skipped %d unknown export sections or files.\n", summary.UnknownSections)
	}
	jsonOutput, _ := cmd.Flags().GetBool(flagJSON)
	if jsonOutput {
		return printJSONTo(cmd.OutOrStdout(), summary)
	}
	_, err = fmt.Fprintf(cmd.OutOrStdout(), "Imported TikTok: %d messages added, %d processed, %d conversations, %d from me.\n", summary.MessagesAdded, summary.MessagesProcessed, summary.Conversations, summary.FromMe)
	return err
}

func init() {
	importCmd.AddCommand(newImportTikTokCommand())
}
