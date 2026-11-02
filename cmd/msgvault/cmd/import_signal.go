package cmd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/signalandroid"
)

var (
	importSignalPhone             string
	importSignalDisplayName       string
	importSignalAttachmentsDir    string
	importSignalLimit             int
	noDefaultIdentityImportSignal bool
)

var importSignalCmd = &cobra.Command{
	Use:   "import-signal <database.sqlite | directory>",
	Short: "Import a decrypted Signal Android backup",
	Long: `Import direct and group chats from a Signal Android backup that
signalbackup-tools has decrypted into a directory:

  signalbackup-tools signal-<date>.backup <passphrase> --output <directory>

Pass that directory or its database.sqlite file. Attachment files
(Attachment_*.bin) are read from the same directory unless --attachments-dir
names another one. msgvault reads Signal Android database version 185 and
newer. Run the command again with a newer backup to add new messages; messages
already in the archive are updated in place.

Examples:
  msgvault import-signal --phone "+447700900000" ~/signal-backup
  msgvault import-signal --phone "+447700900000" ~/signal-backup/database.sqlite`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		if !isDaemonCLISubprocess() {
			return runDaemonCLICommandHTTPFromCobra(cmd, args)
		}
		return runSignalImport(cmd, args[0])
	},
}

// resolveSignalInput accepts the directory signalbackup-tools wrote or the
// database.sqlite inside it, and returns the database and attachment paths.
func resolveSignalInput(input, attachmentsDir string) (dbPath, filesDir string, err error) {
	info, err := os.Stat(input)
	if err != nil {
		return "", "", fmt.Errorf("source not found: %w", err)
	}
	dbPath = input
	if info.IsDir() {
		dbPath = filepath.Join(input, "database.sqlite")
		if _, err := os.Stat(dbPath); err != nil {
			return "", "", fmt.Errorf("no database.sqlite in %s: %w", input, err)
		}
	}
	filesDir = attachmentsDir
	if filesDir == "" {
		filesDir = filepath.Dir(dbPath)
	} else if info, err := os.Stat(filesDir); err != nil || !info.IsDir() {
		return "", "", fmt.Errorf("attachments directory not found or not a directory: %s", filesDir)
	}
	return dbPath, filesDir, nil
}

func runSignalImport(cmd *cobra.Command, input string) error {
	state := invocationFromCommand(cmd)
	if state == nil || state.cfg == nil {
		return errors.New("configuration is unavailable")
	}
	cfg := state.cfg
	if importSignalPhone == "" {
		return usageErr(cmd, errors.New("--phone is required for Signal import (E.164 format, e.g., +447700900000)"))
	}
	if len(importSignalPhone) < 2 || importSignalPhone[0] != '+' {
		return usageErr(cmd, fmt.Errorf("phone number must be in E.164 format (starting with +), got %q", importSignalPhone))
	}
	dbPath, filesDir, err := resolveSignalInput(input, importSignalAttachmentsDir)
	if err != nil {
		return err
	}

	s, cleanup, err := openWritableStoreAndInitForIngestInvocation(state)
	if err != nil {
		return err
	}
	defer cleanup()
	archiveDSN := cfg.DatabaseDSN()

	ctx, cancel := context.WithCancel(cmd.Context())
	defer cancel()
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(sigChan)
	go func() {
		select {
		case <-sigChan:
			fmt.Println("\nInterrupted. Stopping...")
			cancel()
		case <-ctx.Done():
		}
	}()

	opts := signalandroid.DefaultOptions()
	opts.Phone = importSignalPhone
	opts.DisplayName = importSignalDisplayName
	opts.AttachmentFilesDir = filesDir
	opts.AttachmentsDir = cfg.AttachmentsDir()
	opts.Limit = importSignalLimit

	fmt.Printf("Importing Signal messages from %s\n", dbPath)
	fmt.Printf("Phone: %s\n", importSignalPhone)
	if importSignalLimit > 0 {
		fmt.Printf("Limit: %d messages\n", importSignalLimit)
	}
	fmt.Println()

	progress := &signalCLIProgress{}
	summary, err := signalandroid.NewImporter(s, progress).Import(ctx, dbPath, opts)
	if err != nil {
		if ctx.Err() != nil {
			fmt.Println("\nImport interrupted. Run again to continue.")
			return rebuildCacheAfterWrite(archiveDSN, state)
		}
		return fmt.Errorf("import failed: %w", err)
	}

	if !noDefaultIdentityImportSignal && summary.SourceID != 0 {
		confirmDefaultIdentity(cmd.OutOrStdout(), s, summary.SourceID, importSignalPhone, importSignalPhone, "phone-e164", state.logger)
	}
	if summary.SourceID != 0 {
		if err := runPostSourceCreateMigrationsForInvocation(s, state); err != nil {
			return fmt.Errorf("post-source-create migrations: %w", err)
		}
	}

	fmt.Println()
	fmt.Println("Import complete!")
	fmt.Printf("  Duration:       %s\n", summary.Duration.Round(time.Second))
	fmt.Printf("  Chats:          %d imported, %d skipped\n", summary.ChatsProcessed, summary.ChatsSkipped)
	fmt.Printf("  Messages:       %d processed, %d added, %d updated, %d skipped\n",
		summary.MessagesProcessed, summary.MessagesAdded, summary.MessagesUpdated, summary.MessagesSkipped)
	if summary.MessagesEdited > 0 {
		fmt.Printf("  Edited:         %d\n", summary.MessagesEdited)
	}
	if summary.DuplicateIDs > 0 {
		fmt.Printf("  Duplicate IDs:  %d (same author and timestamp; first kept)\n", summary.DuplicateIDs)
	}
	fmt.Printf("  Participants:   %d\n", summary.Participants)
	fmt.Printf("  Reactions:      %d\n", summary.ReactionsAdded)
	fmt.Printf("  Replies:        %d linked, %d unresolved\n", summary.RepliesLinked, summary.RepliesUnresolved)
	fmt.Printf("  Attachments:    %d found, %d stored, %d without a file\n",
		summary.AttachmentsFound, summary.AttachmentsStored, summary.AttachmentsMissing)
	if summary.Errors > 0 {
		fmt.Printf("  Errors:         %d\n", summary.Errors)
	}
	return rebuildCacheAfterWrite(archiveDSN, state)
}

// signalCLIProgress reuses the WhatsApp terminal progress line.
type signalCLIProgress struct {
	ImportCLIProgress
}

func (p *signalCLIProgress) OnComplete(*signalandroid.ImportSummary) {
	fmt.Println()
}

func init() {
	importSignalCmd.Flags().StringVar(&importSignalPhone, "phone", "", "your phone number in E.164 format (required)")
	importSignalCmd.Flags().StringVar(&importSignalDisplayName, "display-name", "", "display name for the phone owner")
	importSignalCmd.Flags().StringVar(&importSignalAttachmentsDir, "attachments-dir", "",
		"directory holding the Attachment_*.bin files (default: the database's directory)")
	importSignalCmd.Flags().IntVar(&importSignalLimit, "limit", 0, "limit number of messages (for testing)")
	importSignalCmd.Flags().BoolVar(&noDefaultIdentityImportSignal, "no-default-identity", false, noDefaultIdentityHelp)
	_ = importSignalCmd.MarkFlagRequired("phone")
	rootCmd.AddCommand(importSignalCmd)
}
