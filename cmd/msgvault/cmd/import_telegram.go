package cmd

import (
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/telegram"
)

// telegramCLIOptions holds import-telegram's flags for the closure-built
// command, mirroring the Slackdump command's shape.
type telegramCLIOptions struct {
	Me              string
	Limit           int
	NoResume        bool
	CheckpointEvery int
}

func newImportTelegramCmd() *cobra.Command {
	var opts telegramCLIOptions
	cmd := &cobra.Command{
		Use:   "import-telegram <export-dir>",
		Short: "Import a Telegram export",
		Long: `Import Telegram conversations from a Telegram Desktop
"Machine-readable JSON" export directory.

Chats, plain-text messages, senders, and exported media are copied into the
local archive. The exporting user is taken from the export's
personal_information; pass --me <numeric-id> to override it (required for
single-chat exports, which do not identify the owner).

Service messages (membership changes, calls, pins) are not imported.
Imports are append-only: re-running with a newer or overlapping export adds
only messages the archive does not yet carry, keyed on the chat's id and
"<chat-id>:<message-id>", and never changes what an earlier import stored.
An interrupted run resumes chat by chat over the same export bytes.

A Telegram person already known through a Beeper bridge resolves to the same
participant instead of a duplicate.

Examples:
  msgvault import-telegram ~/Downloads/TelegramExport
  msgvault import-telegram --me 482111 ./export
  msgvault import-telegram --limit 100 ./export
`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if !isDaemonCLISubprocess() {
				return runDaemonCLICommandHTTPFromCobraWithLocalFiles(cmd, args, nil)
			}
			return runImportTelegram(cmd, args[0], opts)
		},
	}
	cmd.Flags().StringVar(&opts.Me, "me", "", "your numeric Telegram user ID (overrides the export's owner)")
	cmd.Flags().IntVar(&opts.Limit, "limit", 0, "limit number of messages (for testing)")
	cmd.Flags().BoolVar(&opts.NoResume, "no-resume", false, "ignore any existing checkpoint and start fresh")
	cmd.Flags().IntVar(&opts.CheckpointEvery, "checkpoint-interval", 200, "checkpoint every N messages")
	return cmd
}

func runImportTelegram(cmd *cobra.Command, exportDir string, opts telegramCLIOptions) error {
	state := invocationFromCommand(cmd)
	if state == nil || state.cfg == nil {
		return errors.New("configuration is unavailable")
	}
	cfg := state.cfg
	if info, err := os.Stat(exportDir); err != nil {
		return fmt.Errorf("source directory not found: %w", err)
	} else if !info.IsDir() {
		return fmt.Errorf("source path is not a directory: %s", exportDir)
	}

	dbPath := cfg.DatabaseDSN()
	s, cleanup, err := openWritableStoreAndInitForIngestInvocation(state)
	if err != nil {
		return err
	}
	defer cleanup()

	ctx, stop := withInterruptCancel(cmd, "\nInterrupted. Saving checkpoint...")
	defer stop()

	owner := opts.Me
	if owner == "" {
		owner = "from export"
	}
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Importing Telegram export from %s\n", exportDir)
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Me: %s\n\n", owner)

	summary, err := telegram.Import(ctx, s, telegram.ImportOptions{
		Me:              opts.Me,
		RootDir:         exportDir,
		AttachmentsDir:  cfg.AttachmentsDir(),
		Limit:           opts.Limit,
		NoResume:        opts.NoResume,
		CheckpointEvery: opts.CheckpointEvery,
		Logger:          state.logger,
	})
	if err != nil {
		if ctx.Err() != nil {
			_, _ = fmt.Fprintln(cmd.OutOrStdout(), "\nImport interrupted. Re-run to continue.")
			return rebuildCacheAfterWrite(dbPath, state)
		}
		return fmt.Errorf("import failed: %w", err)
	}

	if err := runPostSourceCreateMigrationsForInvocation(s, state); err != nil {
		return fmt.Errorf("post-source-create migrations: %w", err)
	}

	_, _ = fmt.Fprintln(cmd.OutOrStdout())
	if summary.WasResumed {
		_, _ = fmt.Fprintln(cmd.OutOrStdout(), "Resumed from checkpoint.")
	}
	_, _ = fmt.Fprintln(cmd.OutOrStdout(), "Import complete!")
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "  Duration:         %s\n", summary.Duration.Round(time.Millisecond))
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "  Chats:            %d\n", summary.ChatsProcessed)
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "  Messages:         %d processed, %d added, %d skipped\n",
		summary.MessagesProcessed, summary.MessagesAdded, summary.MessagesSkipped)
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "  Service skipped:  %d\n", summary.ServiceMessagesSkipped)
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "  Participants:     %d\n", summary.ParticipantsResolved)
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "  Attachments:      %d found, %d stored, %d missing\n",
		summary.AttachmentsFound, summary.AttachmentsStored, summary.AttachmentsMissing)
	if summary.Errors > 0 {
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "  Errors:           %d\n", summary.Errors)
	}
	if opts.Me != "" && summary.MessagesAdded > 0 && summary.FromMeCount == 0 {
		_, _ = fmt.Fprintf(cmd.OutOrStdout(),
			"\n  Warning: no messages matched --me %q.\n"+
				"  The --me value must be your numeric Telegram user ID; without a\n"+
				"  match, none of the imported messages are marked as sent by you.\n",
			opts.Me)
	}
	return rebuildCacheAfterWrite(dbPath, state)
}

func init() {
	rootCmd.AddCommand(newImportTelegramCmd())
}
