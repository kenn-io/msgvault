package cmd

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"github.com/wesm/msgvault/internal/store"
	"github.com/wesm/msgvault/internal/textutil"
	"github.com/wesm/msgvault/internal/whatsapp"
)

var (
	importPhone                     string
	importMediaDir                  string
	importContacts                  string
	importLimit                     int
	importDisplayName               string
	noDefaultIdentityImportWhatsApp bool
)

var importWhatsappCmd = &cobra.Command{
	Use:   "import-whatsapp <msgstore.db>",
	Short: "Import WhatsApp messages from decrypted backup",
	Long: `Import messages from a decrypted WhatsApp msgstore.db backup.

Examples:
  msgvault import-whatsapp --phone "+447700900000" /path/to/msgstore.db
  msgvault import-whatsapp --phone "+447700900000" --contacts ~/contacts.vcf /path/to/msgstore.db
  msgvault import-whatsapp --phone "+447700900000" --media-dir /path/to/Media /path/to/msgstore.db`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		if err := MustBeLocal("import-whatsapp"); err != nil {
			return err
		}
		return runWhatsAppImport(cmd, args[0])
	},
}

func runWhatsAppImport(cmd *cobra.Command, sourcePath string) error {
	// Validate source file exists.
	if _, err := os.Stat(sourcePath); err != nil {
		return fmt.Errorf("source file not found: %w", err)
	}

	// Validate phone number.
	if importPhone == "" {
		return fmt.Errorf("--phone is required for WhatsApp import (E.164 format, e.g., +447700900000)")
	}
	if !strings.HasPrefix(importPhone, "+") {
		return fmt.Errorf("phone number must be in E.164 format (starting with +), got %q", importPhone)
	}

	// Validate media dir if provided.
	if importMediaDir != "" {
		if info, err := os.Stat(importMediaDir); err != nil || !info.IsDir() {
			return fmt.Errorf("media directory not found or not a directory: %s", importMediaDir)
		}
	}

	// Open database.
	dbPath := cfg.DatabaseDSN()
	s, err := store.Open(dbPath)
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer func() { _ = s.Close() }()

	if err := s.InitSchema(); err != nil {
		return fmt.Errorf("init schema: %w", err)
	}
	if err := runStartupMigrationsForIngest(s); err != nil {
		return fmt.Errorf("startup migrations: %w", err)
	}

	// Set up context with cancellation.
	ctx, cancel := context.WithCancel(cmd.Context())
	defer cancel()

	// Handle Ctrl+C gracefully.
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigChan
		fmt.Println("\nInterrupted. Saving checkpoint...")
		cancel()
	}()

	// Build import options.
	opts := whatsapp.DefaultOptions()
	opts.Phone = importPhone
	opts.DisplayName = importDisplayName
	opts.MediaDir = importMediaDir
	opts.AttachmentsDir = cfg.AttachmentsDir()
	opts.Limit = importLimit

	// Create importer with CLI progress.
	progress := &ImportCLIProgress{}
	importer := whatsapp.NewImporter(s, progress)

	fmt.Printf("Importing WhatsApp messages from %s\n", sourcePath)
	fmt.Printf("Phone: %s\n", importPhone)
	if importMediaDir != "" {
		fmt.Printf("Media: %s\n", importMediaDir)
	}
	if importLimit > 0 {
		fmt.Printf("Limit: %d messages\n", importLimit)
	}
	fmt.Println()

	summary, err := importer.Import(ctx, sourcePath, opts)
	if err != nil {
		if ctx.Err() != nil {
			fmt.Println("\nImport interrupted. Run again to continue.")
			rebuildCacheAfterWrite(dbPath)
			return nil
		}
		return fmt.Errorf("import failed: %w", err)
	}

	// Auto-default-identity must run BEFORE the legacy migration
	// retry — see comment in account_identity.go.
	if !noDefaultIdentityImportWhatsApp && summary.Errors == 0 && summary.SourceID != 0 {
		confirmDefaultIdentity(cmd.OutOrStdout(), s, summary.SourceID, importPhone, importPhone, "phone-e164")
	}

	if summary.SourceID != 0 {
		if err := runPostSourceCreateMigrations(s); err != nil {
			return fmt.Errorf("post-source-create migrations: %w", err)
		}
	}

	// Import contacts if provided.
	if importContacts != "" {
		fmt.Printf("\nImporting contacts from %s...\n", importContacts)
		matched, total, err := whatsapp.ImportContacts(s, importContacts)
		if err != nil {
			return fmt.Errorf("contact import: %w", err)
		} else {
			fmt.Printf("  Contacts: %d in file, %d phone numbers matched to participants\n", total, matched)
		}
	}

	// Print summary.
	fmt.Println()
	fmt.Println("Import complete!")
	fmt.Printf("  Duration:       %s\n", summary.Duration.Round(time.Second))
	fmt.Printf("  Chats:          %d\n", summary.ChatsProcessed)
	fmt.Printf("  Messages:       %d processed, %d added, %d skipped\n",
		summary.MessagesProcessed, summary.MessagesAdded, summary.MessagesSkipped)
	fmt.Printf("  Participants:   %d\n", summary.Participants)
	fmt.Printf("  Reactions:      %d\n", summary.ReactionsAdded)
	fmt.Printf("  Attachments:    %d found", summary.AttachmentsFound)
	if summary.MediaCopied > 0 {
		fmt.Printf(", %d files copied", summary.MediaCopied)
	}
	fmt.Println()
	if summary.Errors > 0 {
		fmt.Printf("  Errors:         %d\n", summary.Errors)
	}

	if summary.MessagesAdded > 0 {
		rate := float64(summary.MessagesAdded) / summary.Duration.Seconds()
		fmt.Printf("  Rate:           %.0f messages/sec\n", rate)
	}

	rebuildCacheAfterWrite(dbPath)
	return nil
}

// ImportCLIProgress implements whatsapp.ImportProgress for terminal output.
type ImportCLIProgress struct {
	startTime   time.Time
	lastPrint   time.Time
	currentChat string
}

func (p *ImportCLIProgress) OnStart() {
	p.startTime = time.Now()
	p.lastPrint = time.Now()
}

func (p *ImportCLIProgress) OnChatStart(chatJID, chatTitle string, messageCount int) {
	p.currentChat = chatTitle
	// Don't print every chat start — too noisy for 13k+ chats.
}

func (p *ImportCLIProgress) OnProgress(processed, added, skipped int64) {
	// Throttle output to every 2 seconds.
	if time.Since(p.lastPrint) < 2*time.Second {
		return
	}
	p.lastPrint = time.Now()

	elapsed := time.Since(p.startTime)
	rate := 0.0
	if elapsed.Seconds() >= 1 {
		rate = float64(added) / elapsed.Seconds()
	}

	elapsedStr := formatDuration(elapsed)

	chatStr := ""
	if p.currentChat != "" {
		// Truncate long chat names and sanitize to prevent terminal injection.
		name := textutil.SanitizeTerminal(p.currentChat)
		if len(name) > 30 {
			name = name[:27] + "..."
		}
		chatStr = fmt.Sprintf(" | Chat: %s", name)
	}

	fmt.Printf("\r  Processed: %d | Added: %d | Skipped: %d | Rate: %.0f/s | Elapsed: %s%s    ",
		processed, added, skipped, rate, elapsedStr, chatStr)
}

func (p *ImportCLIProgress) OnChatComplete(chatJID string, messagesAdded int64) {
	// Quiet — progress line shows the aggregate.
}

func (p *ImportCLIProgress) OnComplete(summary *whatsapp.ImportSummary) {
	fmt.Println() // Clear the progress line.
}

func (p *ImportCLIProgress) OnError(err error) {
	fmt.Printf("\nWarning: %s\n", textutil.SanitizeTerminal(err.Error()))
}

// Deprecated: "import --type whatsapp" forwards to "import-whatsapp".
// Remove after one release cycle.
var importType string

var importCmd = &cobra.Command{
	Use:        "import [path]",
	Short:      "Import messages (deprecated: use import-whatsapp)",
	Deprecated: "use import-whatsapp instead",
	Hidden:     true,
	Args:       cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		if err := MustBeLocal("import"); err != nil {
			return err
		}
		if strings.ToLower(importType) != "whatsapp" {
			return fmt.Errorf(
				"unsupported import type %q; use import-whatsapp",
				importType,
			)
		}
		fmt.Fprintln(os.Stderr,
			"Warning: 'import --type whatsapp' is deprecated."+
				" Use 'import-whatsapp' instead.")
		return runWhatsAppImport(cmd, args[0])
	},
}

func init() {
	// import-whatsapp (canonical)
	importWhatsappCmd.Flags().StringVar(&importPhone, "phone", "", "your phone number in E.164 format (required)")
	importWhatsappCmd.Flags().StringVar(&importMediaDir, "media-dir", "", "path to decrypted Media folder (optional)")
	importWhatsappCmd.Flags().StringVar(&importContacts, "contacts", "", "path to contacts .vcf file for name resolution (optional)")
	importWhatsappCmd.Flags().IntVar(&importLimit, "limit", 0, "limit number of messages (for testing)")
	importWhatsappCmd.Flags().StringVar(&importDisplayName, "display-name", "", "display name for the phone owner")
	importWhatsappCmd.Flags().BoolVar(&noDefaultIdentityImportWhatsApp, "no-default-identity", false, noDefaultIdentityHelp)
	_ = importWhatsappCmd.MarkFlagRequired("phone")
	rootCmd.AddCommand(importWhatsappCmd)

	// Deprecated "import --type whatsapp" alias
	importCmd.Flags().StringVar(&importType, "type", "", "import source type")
	importCmd.Flags().StringVar(&importPhone, "phone", "", "your phone number in E.164 format")
	importCmd.Flags().StringVar(&importMediaDir, "media-dir", "", "path to decrypted Media folder")
	importCmd.Flags().StringVar(&importContacts, "contacts", "", "path to contacts .vcf file")
	importCmd.Flags().IntVar(&importLimit, "limit", 0, "limit number of messages")
	importCmd.Flags().StringVar(&importDisplayName, "display-name", "", "display name for the phone owner")
	rootCmd.AddCommand(importCmd)
}
