package cmd

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"github.com/wesm/msgvault/internal/fbmessenger"
	"github.com/wesm/msgvault/internal/store"
)

var (
	importMessengerMe              string
	importMessengerFormat          string
	importMessengerLimit           int
	importMessengerNoResume        bool
	importMessengerCheckpointEvery int
)

var importMessengerCmd = &cobra.Command{
	Use:   "import-messenger <dyi-export-dir>",
	Short: "Import Facebook Messenger from a Download Your Information export",
	Long: `Import Facebook Messenger conversations from a DYI export (JSON or HTML).

Both JSON and HTML DYI formats are supported. When a thread contains both, the
JSON form wins because it preserves timestamps at millisecond precision and
reactions with relational fidelity. Use --format both to import both copies
into a single conversation with disambiguated source_message_id values.

Participants are synthesized as <slug>@facebook.messenger addresses. Two
participants whose display names produce the same slug are merged with a
warning — DYI exports do not expose stable user IDs, so this is the best we
can do without false-splitting one person into two phantom participants.

Your own identifier is required via --me and must itself be a
<slug>@facebook.messenger address; this value becomes the source identifier
and drives is_from_me on outbound messages.

HTML exports do not expose timezone information; timestamps are stored as
UTC. JSON exports have millisecond-precision timestamps that are preserved
verbatim.

Examples:
  msgvault import-messenger --me test.user@facebook.messenger ~/downloads/facebook-export
  msgvault import-messenger --me test.user@facebook.messenger --format both ./dyi
  msgvault import-messenger --me test.user@facebook.messenger --limit 100 ./dyi
`,
	Args:         cobra.ExactArgs(1),
	SilenceUsage: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		if err := MustBeLocal("import-messenger"); err != nil {
			return err
		}
		return runImportMessenger(cmd, args[0])
	},
}

func runImportMessenger(cmd *cobra.Command, rootDir string) error {
	if info, err := os.Stat(rootDir); err != nil {
		return fmt.Errorf("source directory not found: %w", err)
	} else if !info.IsDir() {
		return fmt.Errorf("source path is not a directory: %s", rootDir)
	}

	dbPath := cfg.DatabaseDSN()
	s, err := store.Open(dbPath)
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer func() { _ = s.Close() }()

	if err := s.InitSchema(); err != nil {
		return fmt.Errorf("init schema: %w", err)
	}

	ctx, cancel := context.WithCancel(cmd.Context())
	defer cancel()

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(sigChan)
	go func() {
		select {
		case <-sigChan:
			_, _ = fmt.Fprintln(cmd.ErrOrStderr(), "\nInterrupted. Saving checkpoint...")
			cancel()
		case <-ctx.Done():
		}
	}()

	opts := fbmessenger.ImportOptions{
		Me:              importMessengerMe,
		RootDir:         rootDir,
		Format:          importMessengerFormat,
		AttachmentsDir:  cfg.AttachmentsDir(),
		Limit:           importMessengerLimit,
		NoResume:        importMessengerNoResume,
		CheckpointEvery: importMessengerCheckpointEvery,
		Logger:          logger,
	}

	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Importing Facebook Messenger DYI from %s\n", rootDir)
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Me: %s\n", importMessengerMe)
	_, _ = fmt.Fprintln(cmd.OutOrStdout())

	summary, err := fbmessenger.ImportDYI(ctx, s, opts)
	if err != nil {
		if ctx.Err() != nil {
			_, _ = fmt.Fprintln(cmd.OutOrStdout(), "\nImport interrupted. Re-run to continue.")
			rebuildCacheAfterWrite(dbPath)
			return nil
		}
		return fmt.Errorf("import failed: %w", err)
	}

	_, _ = fmt.Fprintln(cmd.OutOrStdout())
	if summary.WasResumed {
		_, _ = fmt.Fprintln(cmd.OutOrStdout(), "Resumed from checkpoint.")
	}
	_, _ = fmt.Fprintln(cmd.OutOrStdout(), "Import complete!")
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "  Duration:       %s\n", summary.Duration.Round(time.Millisecond))
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "  Threads:        %d processed, %d skipped\n",
		summary.ThreadsProcessed, summary.ThreadsSkipped)
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "  Files skipped:  %d (unrecognized siblings)\n", summary.FilesSkipped)
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "  Messages:       %d processed, %d added, %d skipped\n",
		summary.MessagesProcessed, summary.MessagesAdded, summary.MessagesSkipped)
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "  Participants:   %d\n", summary.ParticipantsResolved)
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "  Attachments:    %d found, %d stored\n", summary.AttachmentsFound, summary.AttachmentsStored)
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "  Reactions:      %d\n", summary.ReactionsAdded)
	if summary.Errors > 0 {
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "  Errors:         %d\n", summary.Errors)
	}
	if summary.MessagesAdded > 0 && summary.FromMeCount == 0 {
		_, _ = fmt.Fprintf(cmd.OutOrStdout(),
			"\n  Warning: no messages matched --me %q (slug: %q).\n"+
				"  The --me value must match the slug of your display name in the export.\n",
			importMessengerMe, fbmessenger.Slug(fbmessenger.StripDomain(importMessengerMe)))
	}

	rebuildCacheAfterWrite(dbPath)
	return nil
}

func init() {
	importMessengerCmd.Flags().StringVar(&importMessengerMe, "me", "", "your <slug>@facebook.messenger identifier (required)")
	importMessengerCmd.Flags().StringVar(&importMessengerFormat, "format", "auto", "format to import: auto|json|html|both")
	importMessengerCmd.Flags().IntVar(&importMessengerLimit, "limit", 0, "limit number of messages (for testing)")
	importMessengerCmd.Flags().BoolVar(&importMessengerNoResume, "no-resume", false, "ignore any existing checkpoint and start fresh")
	importMessengerCmd.Flags().IntVar(&importMessengerCheckpointEvery, "checkpoint-interval", 200, "checkpoint every N messages")
	_ = importMessengerCmd.MarkFlagRequired("me")
	rootCmd.AddCommand(importMessengerCmd)
}
