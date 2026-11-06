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
	Me                string
	Limit             int
	NoResume          bool
	NoDefaultIdentity bool
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
Re-running an export adds messages the archive lacks and fills media slots
that still lack bytes. Archived text, senders, timestamps, and edit flags stay
as first imported. Messages are keyed by peer and message ID.
An interrupted run resumes chat by chat from the same export directory.

Examples:
  msgvault import-telegram ~/Downloads/TelegramExport
  msgvault import-telegram --me 482111 ./export
  msgvault import-telegram --limit 100 ./export
`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if opts.Limit < 0 {
				return errors.New("--limit must be non-negative")
			}
			if !isDaemonCLISubprocess() {
				return runDaemonCLICommandHTTPFromCobraWithLocalFiles(cmd, args, nil)
			}
			return runImportTelegram(cmd, args[0], opts)
		},
	}
	cmd.Flags().StringVar(&opts.Me, "me", "", "your numeric Telegram user ID (overrides the export's owner)")
	cmd.Flags().IntVar(&opts.Limit, "limit", 0, "maximum messages to add (0 imports all)")
	cmd.Flags().BoolVar(&opts.NoDefaultIdentity, "no-default-identity", false, noDefaultIdentityHelp)
	cmd.Flags().BoolVar(&opts.NoResume, "no-resume", false, "ignore any existing checkpoint and start fresh")
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
	s, cleanup, err := openWritableStoreAndInitForIngestInvocation(cmd.Context(), state)
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
		Me:             opts.Me,
		RootDir:        exportDir,
		AttachmentsDir: cfg.AttachmentsDir(),
		Limit:          opts.Limit,
		NoResume:       opts.NoResume,
		Logger:         state.logger,
	})
	err = errors.Join(err, runExportPostImportMigrations(ctx, cmd.OutOrStdout(), s, summary.SourceID, opts.NoDefaultIdentity, state))
	if err != nil {
		var cacheErr error
		if summary.SourceID != 0 {
			cacheErr = rebuildCacheAfterWrite(ctx, dbPath, state)
		}
		if ctx.Err() != nil {
			_, _ = fmt.Fprintln(cmd.OutOrStdout(), "\nImport interrupted. Re-run to continue.")
		}
		return errors.Join(fmt.Errorf("import failed: %w", err), cacheErr)
	}

	_, _ = fmt.Fprintln(cmd.OutOrStdout())
	if summary.WasResumed {
		_, _ = fmt.Fprintln(cmd.OutOrStdout(), "Resumed from checkpoint.")
	}
	if summary.LimitReached {
		_, _ = fmt.Fprintln(cmd.OutOrStdout(), "Import paused at --limit. More messages remain; re-run to continue.")
	} else {
		_, _ = fmt.Fprintln(cmd.OutOrStdout(), "Import complete!")
	}
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "  Duration:         %s\n", summary.Duration.Round(time.Millisecond))
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "  Chats:            %d processed, %d skipped\n", summary.ChatsProcessed, summary.ChatsSkipped)
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "  Messages:         %d added, %d skipped\n", summary.MessagesAdded, summary.MessagesSkipped)
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "  Service skipped:  %d\n", summary.ServiceMessagesSkipped)
	if summary.UnsupportedEntriesSkipped > 0 {
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "  Unsupported skipped: %d\n", summary.UnsupportedEntriesSkipped)
	}
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "  Participants:     %d\n", summary.ParticipantsResolved)
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "  Attachments:      %d stored, %d missing\n", summary.AttachmentsStored, summary.AttachmentsMissing)
	return rebuildCacheAfterWrite(ctx, dbPath, state)
}

func init() {
	rootCmd.AddCommand(newImportTelegramCmd())
}
