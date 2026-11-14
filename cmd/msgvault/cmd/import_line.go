package cmd

import (
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/line"
)

// lineCLIOptions holds import-line's flags for the closure-built command,
// mirroring the Telegram command's shape.
type lineCLIOptions struct {
	Me                string
	Limit             int
	NoResume          bool
	NoDefaultIdentity bool
}

func newImportLineCmd() *cobra.Command {
	var opts lineCLIOptions
	cmd := &cobra.Command{
		Use:   "import-line <path>",
		Short: "Import LINE chats from iPhone backups",
		Long: `Import LINE chats from the message store the LINE iOS app keeps, as
found in an unencrypted iPhone backup.

<path> is the backup directory (the one holding Manifest.db), the Messages
directory copied out of it, or its Line.sqlite file. UnifiedGroup.sqlite
beside the store supplies group names and members when present, and a
Line.sqlite-wal file beside it is read together with the store. A backup
that holds the stores of several LINE accounts is refused; copy the one
Messages directory you want out of it instead.

Text messages, their senders, and replies are archived for direct chats,
groups, and rooms. The owner is read from the store's Keep Memo chat; pass
--me <id> when the store has none. Photos, videos, files, stickers, calls,
and system notices are counted and skipped: the store holds no media bytes.

Re-running an import adds messages the archive lacks and leaves archived
messages as they were. Messages are keyed by chat and LINE message id. An
interrupted run resumes chat by chat from the same store file.

Examples:
  msgvault import-line "$HOME/Library/Application Support/MobileSync/Backup/<device>"
  msgvault import-line ~/line-backup/Messages
  msgvault import-line --me u0123456789abcdef0123456789abcdef --limit 100 ./Line.sqlite
`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if opts.Limit < 0 {
				return errors.New("--limit must be non-negative")
			}
			if !isDaemonCLISubprocess() {
				return runDaemonCLICommandHTTPFromCobraWithLocalFiles(cmd, args)
			}
			return runImportLine(cmd, args[0], opts)
		},
	}
	cmd.Flags().StringVar(&opts.Me, "me", "", "your LINE user ID (overrides the store's Keep Memo chat)")
	cmd.Flags().IntVar(&opts.Limit, "limit", 0, "maximum messages to add (0 imports all)")
	cmd.Flags().BoolVar(&opts.NoDefaultIdentity, "no-default-identity", false, noDefaultIdentityHelp)
	cmd.Flags().BoolVar(&opts.NoResume, "no-resume", false, "ignore any existing checkpoint and start fresh")
	return cmd
}

func runImportLine(cmd *cobra.Command, path string, opts lineCLIOptions) error {
	state := invocationFromCommand(cmd)
	if state == nil || state.cfg == nil {
		return errors.New("configuration is unavailable")
	}
	cfg := state.cfg
	if _, err := os.Stat(path); err != nil {
		return fmt.Errorf("source path not found: %w", err)
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
		owner = "from store"
	}
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Importing LINE messages from %s\n", path)
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Me: %s\n\n", owner)

	summary, err := line.Import(ctx, s, line.ImportOptions{
		Me:       opts.Me,
		Path:     path,
		Limit:    opts.Limit,
		NoResume: opts.NoResume,
		Logger:   state.logger,
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
		_, _ = fmt.Fprintln(cmd.OutOrStdout(), "Import paused at --limit.")
	} else {
		_, _ = fmt.Fprintln(cmd.OutOrStdout(), "Import complete!")
	}
	if summary.OwnerFromStore {
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "  Owner:            %s (from the store's Keep Memo chat)\n", summary.Owner)
	}
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "  Duration:         %s\n", summary.Duration.Round(time.Millisecond))
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "  Chats:            %d processed, %d skipped\n", summary.ChatsProcessed, summary.ChatsSkipped)
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "  Messages:         %d added, %d skipped\n", summary.MessagesAdded, summary.MessagesSkipped)
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "  Media skipped:    %d (the iOS store holds no media bytes)\n", summary.MediaSkipped)
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "  Stickers skipped: %d\n", summary.StickersSkipped)
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "  Calls skipped:    %d\n", summary.CallsSkipped)
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "  System skipped:   %d\n", summary.SystemSkipped)
	if summary.UnsentSkipped > 0 {
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "  Unsent skipped:   %d\n", summary.UnsentSkipped)
	}
	if summary.UnsupportedSkipped > 0 {
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "  Unsupported skipped: %d\n", summary.UnsupportedSkipped)
	}
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "  Participants:     %d\n", summary.ParticipantsResolved)
	return rebuildCacheAfterWrite(ctx, dbPath, state)
}

func init() {
	rootCmd.AddCommand(newImportLineCmd())
}
