package cmd

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/discord"
)

func newImportDiscordPackageCmd() *cobra.Command {
	var noDefaultIdentity bool
	command := &cobra.Command{
		Use:   "import-discord-package <package-dir>",
		Short: "Import a Discord sent-message package",
		Long: `Import sent messages from an extracted official Discord data package.

Reads the account ID, channel metadata, and JSON or legacy CSV transcripts.
Coverage is sent-only, including DMs; incoming messages are not in the package.
Attachment URLs remain links. No media is downloaded. Re-importing replaces
messages with the last imported snapshot and safely repeats interrupted runs.
Run on the daemon host with a local extracted directory; ZIP input is unsupported.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if !isDaemonCLISubprocess() {
				return runDaemonCLICommandHTTPFromCobraWithLocalFiles(cmd, args)
			}
			return runImportDiscordPackage(cmd, args[0], noDefaultIdentity)
		},
	}
	command.Flags().BoolVar(&noDefaultIdentity, "no-default-identity", false, noDefaultIdentityHelp)
	return command
}

func runImportDiscordPackage(cmd *cobra.Command, directory string, noDefaultIdentity bool) error {
	state := invocationFromCommand(cmd)
	if state == nil || state.cfg == nil {
		return errors.New("configuration is unavailable")
	}
	st, cleanup, err := openWritableStoreAndInitForIngestInvocation(cmd.Context(), state)
	if err != nil {
		return err
	}
	defer cleanup()
	ctx, stop := withInterruptCancel(cmd, "\nInterrupted. Re-run to import remaining snapshots.")
	defer stop()
	summary, importErr := discord.ImportPackage(ctx, st, directory)
	_, _ = fmt.Fprintln(cmd.OutOrStdout(), "Coverage: sent_only (exporting account's sent messages only)")
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Channels: %d\nMessages: %d processed, %d added, %d updated, %d skipped\nAttachments: %d links, %d missing local media, %d unsupported links\n", summary.Channels, summary.MessagesProcessed, summary.MessagesAdded, summary.MessagesUpdated, summary.MessagesSkipped, summary.AttachmentLinks, summary.AttachmentLinks, summary.UnsupportedAttachmentLinks)
	reasons := make([]string, 0, len(summary.SkipReasons))
	for reason := range summary.SkipReasons {
		reasons = append(reasons, reason)
	}
	sort.Strings(reasons)
	for _, reason := range reasons {
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Skipped %s: %d\n", reason, summary.SkipReasons[reason])
	}
	migrationErr := runExportPostImportMigrations(ctx, cmd.OutOrStdout(), st, summary.SourceID, noDefaultIdentity, state)
	var cacheErr error
	if summary.SourceID != 0 {
		// Cache already committed snapshots even after cancellation or partial failure.
		cacheErr = rebuildCacheAfterWrite(context.WithoutCancel(ctx), state.cfg.DatabaseDSN(), state)
	}
	return errors.Join(importErr, migrationErr, cacheErr)
}

func init() { rootCmd.AddCommand(newImportDiscordPackageCmd()) }
