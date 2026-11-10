package cmd

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/googlechat"
)

type googleChatCLIOptions struct {
	Me                string
	NoDefaultIdentity bool
}

func newImportGoogleChatCmd() *cobra.Command {
	var opts googleChatCLIOptions
	command := &cobra.Command{
		Use:   "import-google-chat --me <email> <part-dir>...",
		Short: "Import extracted Google Chat Takeout parts",
		Long: `Import retained Google Chat messages and available attachments from explicitly
supplied, locally extracted Takeout parts. Tested variants use English dates
with a UTC suffix; other date forms remain unknown and are preserved raw.

Supply all parts together. --me selects the exporting account; supplied owner
profiles must agree. Accepted roots contain Takeout/Google Chat, Google Chat,
or Groups, or are Groups themselves. ZIP/TGZ files must be extracted first.

Replays add missing messages and repair missing media. Stored snapshots and
bytes remain unchanged. Only exact export filenames are matched. Missing media,
unsupported records, unknown dates and incomplete membership coverage are
reported; Takeout does not guarantee lifetime history. Rosters establish membership,
never delivery recipients. Conflicting message IDs are counted and skipped while
later records continue. Structural or storage errors stop the run; earlier commits
remain available for replay.

Examples:
  msgvault import-google-chat --me owner@example.com ./part-001 ./part-002
`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if _, err := googlechat.NormalizeOwner(opts.Me); err != nil {
				return err
			}
			if !isDaemonCLISubprocess() {
				return runDaemonCLICommandHTTPFromCobraWithLocalFiles(cmd, args)
			}
			return runImportGoogleChat(cmd, args, opts)
		},
	}
	command.Flags().StringVar(&opts.Me, "me", "", "exporting account email (required)")
	command.Flags().BoolVar(&opts.NoDefaultIdentity, "no-default-identity", false, noDefaultIdentityHelp)
	return command
}

func runImportGoogleChat(cmd *cobra.Command, roots []string, opts googleChatCLIOptions) error {
	state := invocationFromCommand(cmd)
	if state == nil || state.cfg == nil {
		return errors.New("configuration is unavailable")
	}
	st, cleanup, err := openWritableStoreAndInitForIngestInvocation(cmd.Context(), state)
	if err != nil {
		return err
	}
	defer cleanup()
	ctx, stop := withInterruptCancel(cmd, "\nInterrupted. Re-run all supplied parts to continue.")
	defer stop()
	summary, err := googlechat.Import(ctx, st, googlechat.ImportOptions{Me: opts.Me, Roots: roots, AttachmentsDir: state.cfg.AttachmentsDir()})
	// Always print committed partial results before returning any importer error.
	out := cmd.OutOrStdout()
	_, _ = fmt.Fprintf(out, "Google Chat supplied input (coverage is not completeness):\n  Conversations: %d\n  Messages: %d supplied, %d added, %d duplicates, %d unsupported\n  Unknown senders: %d; unparsed dates: %d; unsupported roster members: %d\n  Attachments: %d stored, %d missing (%d unsupported, %d oversized)\n  Memberships without supplied messages: %d\n  Duration: %s\n", summary.Conversations, summary.MessagesSupplied, summary.MessagesAdded, summary.Duplicates, summary.Unsupported, summary.UnknownSenders, summary.DatesUnparsed, summary.UnknownMembers, summary.AttachmentsStored, summary.AttachmentsMissing, summary.AttachmentsUnsupported, summary.AttachmentsOversized, summary.MembershipWithoutMessages, summary.Duration.Round(time.Millisecond))
	err = errors.Join(err, runExportPostImportMigrations(ctx, out, st, summary.SourceID, opts.NoDefaultIdentity, state))
	if summary.SourceID != 0 {
		// Refresh derived files for messages committed before an interruption.
		err = errors.Join(err, rebuildCacheAfterWrite(context.WithoutCancel(ctx), state.cfg.DatabaseDSN(), state))
	}
	if err != nil {
		return fmt.Errorf("google chat import stopped; re-run all supplied parts: %w", err)
	}
	return nil
}

func init() { rootCmd.AddCommand(newImportGoogleChatCmd()) }
