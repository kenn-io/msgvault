package cmd

import (
	"errors"
	"fmt"

	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/xarchive"
)

type xCLIOptions struct {
	Me                string
	NoDefaultIdentity bool
}

func newImportXCmd() *cobra.Command {
	var opts xCLIOptions
	cmd := &cobra.Command{
		Use: "import-x <archive-dir>", Short: "Import X direct-message archives",
		Long: `Import supplied direct and group DMs from an extracted X account archive.

Accepts direct-messages.js, direct-messages-group.js and numbered parts in
an archive root or its data directory. Uses account.js for the numeric owner
ID; pass --me <numeric-id> when account.js is absent. A conflicting override
is rejected. Available local media is copied; URLs are never fetched.

Re-running safely skips archived messages and retries missing media. Original
message JSON is retained. Service events are skipped as messages.
Adds observed group members without inferring message recipients.
Only supplied archive history is covered, without claims about deleted or
newer encrypted chats. HTML-only exports and ZIP files are not supported.

Examples:
  msgvault import-x ~/Downloads/x-archive
  msgvault import-x --me 100001 ./archive/data
`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if !isDaemonCLISubprocess() {
				return runDaemonCLICommandHTTPFromCobraWithLocalFiles(cmd, args)
			}
			return runImportX(cmd, args[0], opts)
		},
	}
	cmd.Flags().StringVar(&opts.Me, "me", "", "your numeric X account ID (required without account.js)")
	cmd.Flags().BoolVar(&opts.NoDefaultIdentity, "no-default-identity", false, noDefaultIdentityHelp)
	return cmd
}
func runImportX(cmd *cobra.Command, root string, opts xCLIOptions) error {
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
	summary, importErr := xarchive.Import(ctx, st, xarchive.ImportOptions{RootDir: root, Me: opts.Me, AttachmentsDir: state.cfg.AttachmentsDir()})
	migrationErr := runExportPostImportMigrations(ctx, cmd.OutOrStdout(), st, summary.SourceID, opts.NoDefaultIdentity, state)
	var cacheErr error
	if summary.SourceID != 0 {
		cacheErr = rebuildCacheAfterWrite(ctx, state.cfg.DatabaseDSN(), state)
	}
	if summary.SourceID != 0 {
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "X DM import: %d direct and %d group conversation entries\n  Messages: %d added, %d duplicates\n  Skipped: %d events, %d invalid records\n  Media: %d stored, %d missing, unsafe or oversized\nRe-run to safely retry unfinished work and missing media.\n", summary.DirectConversations, summary.GroupConversations, summary.MessagesAdded, summary.MessagesSkipped, summary.EventsSkipped, summary.InvalidRecords, summary.AttachmentsStored, summary.AttachmentsMissing)
	}
	return errors.Join(importErr, migrationErr, cacheErr)
}
func init() { rootCmd.AddCommand(newImportXCmd()) }
