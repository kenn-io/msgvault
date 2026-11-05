package cmd

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/attachmentpolicy"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/imessage"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/vcard"
)

var (
	importImessageDBPath        string
	importImessageBefore        string
	importImessageAfter         string
	importImessageLimit         int
	importImessageMe            string
	importImessageContacts      string
	importImessageMessagesRoot  string
	importImessageNoAttachments bool

	importImessageMaxMediaMB int64
)

var importImessageCmd = &cobra.Command{
	Use:   "import-imessage",
	Short: "Import iMessages from local database",
	Long: `Import iMessages from macOS's local Messages database (chat.db).

Reads messages from ~/Library/Messages/chat.db and stores them in the
msgvault archive. This is a read-only operation that does not modify
the iMessage database.

Available local attachments are copied with their messages. Missing or
offloaded files remain unavailable; this command does not fetch iCloud media.
Use --messages-root to select a copied Messages folder; only its
Attachments and StickerCache folders are read.

Requires Full Disk Access permission in System Settings > Privacy & Security.

Date filters:
  --after 2024-01-01     Only messages on or after this date
  --before 2024-12-31    Only messages before this date

Contact names:
  chat.db only stores phone/email handles, not contact names. Pass
  --contacts /path/to/contacts.vcf to backfill display names from a
  vCard export (e.g. macOS Contacts.app → File → Export → Export vCard).

Examples:
  msgvault import-imessage
  msgvault import-imessage --after 2024-01-01
  msgvault import-imessage --limit 100
  msgvault import-imessage --db-path /path/to/chat.db
  msgvault import-imessage --contacts ~/contacts.vcf`,
	PreRunE: validateImessageFlags,
	RunE:    runImportImessage,
}

// validateImessageFlags rejects a bad --max-media-mb before the store opens.
func validateImessageFlags(cmd *cobra.Command, _ []string) error {
	if importImessageMaxMediaMB < 0 {
		return usageErr(cmd, errors.New("--max-media-mb must be non-negative"))
	}
	if importImessageMaxMediaMB > math.MaxInt64>>20 {
		return usageErr(cmd, errors.New("--max-media-mb is too large"))
	}
	return nil
}

// imessageMediaPolicy is the media policy for Messages, shared by import and
// purge. It applies the [imessage] config limits and lets a positive
// --max-media-mb override the size cap (import passes 0 for no override).
func imessageMediaPolicy(overrideMB int64, cfg *config.Config) attachmentpolicy.Policy {
	policy := cfg.IMessage.MediaPolicy()
	if overrideMB > 0 {
		policy.MaxBytes = overrideMB << 20
	}
	return policy
}

func runImportImessage(cmd *cobra.Command, args []string) error {
	state := invocationFromCommand(cmd)
	if state == nil || state.cfg == nil {
		return errors.New("configuration is unavailable")
	}
	if !isDaemonCLISubprocess() {
		return runDaemonCLICommandHTTPFromCobra(cmd, args)
	}

	s, cleanup, err := openWritableStoreAndInitForIngestInvocation(state)
	if err != nil {
		return err
	}
	defer cleanup()

	chatDBPath, err := resolveChatDBPath()
	if err != nil {
		return err
	}

	clientOpts, err := buildImessageOpts(
		state.logger, state.cfg.AttachmentsDir(), importImessageMessagesRoot,
		importImessageNoAttachments,
		imessageMediaPolicy(importImessageMaxMediaMB, state.cfg),
	)
	if err != nil {
		return err
	}
	if importImessageMe != "" {
		clientOpts = append(
			clientOpts, imessage.WithOwnerHandle(importImessageMe),
		)
	}

	client, err := imessage.NewClient(chatDBPath, clientOpts...)
	if err != nil {
		return fmt.Errorf("open iMessage database: %w", err)
	}
	defer func() { _ = client.Close() }()

	src, err := resolveImessageSource(s)
	if err != nil {
		return fmt.Errorf("get or create source: %w", err)
	}
	if err := runPostSourceCreateMigrationsForInvocation(s, state); err != nil {
		return fmt.Errorf("post-source-create migrations: %w", err)
	}

	ctx, cancel := context.WithCancel(cmd.Context())
	defer cancel()

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigChan
		fmt.Println("\nInterrupted.")
		cancel()
	}()

	startTime := time.Now()
	totalEstimate := client.CountFilteredMessages(ctx)
	fmt.Printf("Importing iMessages from %s\n", chatDBPath)
	if totalEstimate > 0 {
		fmt.Printf("Messages to import: ~%d\n", totalEstimate)
	}
	printImessageDateFilter()
	if importImessageLimit > 0 {
		fmt.Printf("Limit: %d messages\n", importImessageLimit)
	}
	fmt.Println()

	summary, err := client.Import(ctx, s, src.ID)
	if err != nil {
		if ctx.Err() != nil {
			fmt.Println("\nImport interrupted.")
			printImessageSummary(summary, startTime)
			return finishImessageImport(s, state, summary)
		}
		if errors.Is(err, imessage.ErrAttachmentImport) {
			printImessageSummary(summary, startTime)
			finishErr := finishImessageImport(s, state, summary)
			importErr := fmt.Errorf("import failed: %w", err)
			if finishErr != nil {
				return errors.Join(importErr, fmt.Errorf("finish iMessage import: %w", finishErr))
			}
			return importErr
		}
		return fmt.Errorf("import failed: %w", err)
	}

	printImessageSummary(summary, startTime)
	return finishImessageImport(s, state, summary)
}

// finishImessageImport runs the post-import name backfill, refreshes
// generated chat titles, and rebuilds the analytics cache when those changes
// affect existing rows inside the published message boundary.
func finishImessageImport(s *store.Store, state *invocation, summary *imessage.ImportSummary) error {
	if state == nil || state.cfg == nil {
		return errors.New("configuration is unavailable")
	}
	cfg := state.cfg
	mutated := false

	if importImessageContacts != "" {
		if applyImessageContacts(s, importImessageContacts) {
			mutated = true
		}
	}

	if retitleImessageChats(s) {
		mutated = true
	}

	dbPath := cfg.DatabaseDSN()
	if !store.IsPostgresURL(dbPath) && (mutated || (summary != nil && summary.DatesCleared > 0)) {
		// Title/display_name updates aren't visible to the message-id-keyed
		// staleness check, so the standard rebuildCacheAfterWrite would skip.
		// A date clear also leaves the message ID unchanged, so it must re-export
		// messages.parquet as well as the affected conversation datasets.
		if _, err := buildCache(
			dbPath,
			cfg.AnalyticsDir(),
			true,
			analyticsBuilderOverrides(cfg.Analytics),
		); err != nil {
			return fmt.Errorf("refresh analytics cache: %w", err)
		}
		return nil
	}

	return rebuildCacheAfterWrite(dbPath, state)
}

func retitleImessageChats(s *store.Store) bool {
	n, err := s.RetitleImessageChats()
	if err != nil {
		fmt.Printf("\nWarning: could not refresh iMessage chat titles: %v\n", err)
		return false
	}
	if n > 0 {
		fmt.Printf("iMessage chat titles refreshed: %d\n", n)
		return true
	}
	return false
}

// applyImessageContacts loads a vCard file and backfills display_name
// for participants matched by phone or email. Only updates participants
// that already exist (created during message import) and whose name is
// currently empty — first-writer-wins from earlier sources is preserved.
// Returns true if any participant was updated.
func applyImessageContacts(s *store.Store, vcfPath string) bool {
	contacts, err := vcard.ParseFile(vcfPath)
	if err != nil {
		fmt.Printf("\nWarning: could not read contacts %s: %v\n", vcfPath, err)
		return false
	}

	var phoneMatches, emailMatches int
	var phoneErrors, emailErrors int
	var firstPhoneErr, firstEmailErr error
	for _, c := range contacts {
		if c.FullName == "" {
			continue
		}
		for _, phone := range c.Phones {
			// Use the iMessage-scoped variant so legacy participants
			// whose display_name was poisoned with the raw phone string
			// (older import-imessage runs) get cleared and replaced.
			updated, err := s.UpdateImessageParticipantDisplayNameByPhone(phone, c.FullName)
			if err != nil {
				if firstPhoneErr == nil {
					firstPhoneErr = err
				}
				phoneErrors++
				continue
			}
			if updated {
				phoneMatches++
			}
		}
		for _, email := range c.Emails {
			updated, err := s.UpdateParticipantDisplayNameByEmail(email, c.FullName)
			if err != nil {
				if firstEmailErr == nil {
					firstEmailErr = err
				}
				emailErrors++
				continue
			}
			if updated {
				emailMatches++
			}
		}
	}

	fmt.Println()
	fmt.Println("Contacts applied:")
	fmt.Printf("  Source:           %s (%d entries)\n", vcfPath, len(contacts))
	fmt.Printf("  Names backfilled: %d by phone, %d by email\n",
		phoneMatches, emailMatches)
	if phoneErrors > 0 || emailErrors > 0 {
		fmt.Fprintf(os.Stderr,
			"Warning: %d phone update(s) and %d email update(s) failed.\n",
			phoneErrors, emailErrors)
		if firstPhoneErr != nil {
			fmt.Fprintf(os.Stderr, "  First phone error: %v\n", firstPhoneErr)
		}
		if firstEmailErr != nil {
			fmt.Fprintf(os.Stderr, "  First email error: %v\n", firstEmailErr)
		}
	}
	return phoneMatches > 0 || emailMatches > 0
}

func resolveChatDBPath() (string, error) {
	if importImessageDBPath != "" {
		if _, err := os.Stat(importImessageDBPath); os.IsNotExist(err) {
			return "", fmt.Errorf(
				"iMessage database not found at %s",
				importImessageDBPath,
			)
		}
		return importImessageDBPath, nil
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("get home directory: %w", err)
	}
	path := filepath.Join(home, "Library", "Messages", "chat.db")
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return "", fmt.Errorf(
			"iMessage database not found at %s\n\n"+
				"Make sure you're running on macOS with Messages enabled",
			path,
		)
	}
	return path, nil
}

func buildImessageOpts(logger *slog.Logger, attachmentsDir, messagesRoot string, noAttachments bool, policy attachmentpolicy.Policy) ([]imessage.ClientOption, error) {
	if noAttachments {
		attachmentsDir, messagesRoot = "", ""
	}
	opts := []imessage.ClientOption{
		imessage.WithImessageLogger(logger),
		imessage.WithAttachments(attachmentsDir, messagesRoot, policy),
	}

	if importImessageAfter != "" {
		t, err := time.ParseInLocation(
			"2006-01-02", importImessageAfter, time.Local,
		)
		if err != nil {
			return nil, fmt.Errorf(
				"invalid --after date: %w (use YYYY-MM-DD format)", err,
			)
		}
		opts = append(opts, imessage.WithAfterDate(t))
	}

	if importImessageBefore != "" {
		t, err := time.ParseInLocation(
			"2006-01-02", importImessageBefore, time.Local,
		)
		if err != nil {
			return nil, fmt.Errorf(
				"invalid --before date: %w (use YYYY-MM-DD format)", err,
			)
		}
		opts = append(opts, imessage.WithBeforeDate(t))
	}

	if importImessageLimit > 0 {
		opts = append(opts, imessage.WithLimit(importImessageLimit))
	}

	return opts, nil
}

func printImessageDateFilter() {
	if importImessageAfter == "" && importImessageBefore == "" {
		return
	}
	parts := []string{}
	if importImessageAfter != "" {
		parts = append(parts, "after "+importImessageAfter)
	}
	if importImessageBefore != "" {
		parts = append(parts, "before "+importImessageBefore)
	}
	fmt.Printf("Date filter: %s\n", strings.Join(parts, ", "))
}

func printImessageSummary(
	summary *imessage.ImportSummary,
	startTime time.Time,
) {
	if summary == nil {
		return
	}
	elapsed := time.Since(startTime)
	fmt.Println()
	fmt.Println("iMessage import complete!")
	fmt.Printf("  Duration:         %s\n", elapsed.Round(time.Second))
	fmt.Printf("  Messages:         %d imported\n", summary.MessagesImported)
	fmt.Printf("  Conversations:    %d\n", summary.ConversationsImported)
	fmt.Printf("  Participants:     %d resolved\n", summary.ParticipantsResolved)
	if summary.Skipped > 0 {
		fmt.Printf("  Skipped:          %d\n", summary.Skipped)
	}
	if summary.AttachmentFailures > 0 {
		fmt.Printf("  Attachment errors: %d messages\n", summary.AttachmentFailures)
	}
	if summary.AttachmentsUnavailable > 0 {
		fmt.Printf("  Attachments unavailable: %d (%s)\n", summary.AttachmentsUnavailable, unavailableAttachmentHint())
	}
	if summary.AttachmentsSkipped > 0 {
		fmt.Printf("  Attachments skipped: %d (media policy or path outside the Messages folder%s)\n", summary.AttachmentsSkipped, messagesRootHint())
	}
	if summary.DatesCleared > 0 {
		fmt.Printf("  Dates cleared:    %d\n", summary.DatesCleared)
	}
	if summary.MessagesImported > 0 && elapsed.Seconds() > 0 {
		rate := float64(summary.MessagesImported) / elapsed.Seconds()
		fmt.Printf("  Rate:             %.1f messages/sec\n", rate)
	}
}

// unavailableAttachmentHint names the usual causes of unreadable attachment files.
func unavailableAttachmentHint() string {
	return "file missing or unreadable; check Full Disk Access" + messagesRootHint()
}

// messagesRootHint points at --messages-root when a custom database is read without one.
func messagesRootHint() string {
	if importImessageDBPath != "" && importImessageMessagesRoot == "" {
		return ", or pass --messages-root for a copied Messages folder"
	}
	return ""
}

// resolveImessageSource finds or creates the apple_messages source.
// Among non-"local" sources, prefers the one with the highest ID
// (most recently created), which is more likely in active use.
func resolveImessageSource(s *store.Store) (*store.Source, error) {
	sources, err := s.ListSources(sourceTypeAppleMessages)
	if err == nil && len(sources) > 0 {
		var best *store.Source
		for _, src := range sources {
			if src.Identifier != localValue {
				if best == nil || src.ID > best.ID {
					best = src
				}
			}
		}
		if best != nil {
			return best, nil
		}
		return sources[0], nil
	}
	return s.GetOrCreateSource(sourceTypeAppleMessages, localValue)
}

func init() {
	importImessageCmd.Flags().StringVar(
		&importImessageDBPath, "db-path", "",
		"path to chat.db (default: ~/Library/Messages/chat.db)",
	)
	importImessageCmd.Flags().StringVar(
		&importImessageBefore, "before", "",
		"only messages before this date (YYYY-MM-DD)",
	)
	importImessageCmd.Flags().StringVar(
		&importImessageAfter, "after", "",
		"only messages after this date (YYYY-MM-DD)",
	)
	importImessageCmd.Flags().IntVar(
		&importImessageLimit, "limit", 0,
		"limit number of messages (for testing)",
	)
	importImessageCmd.Flags().StringVar(
		&importImessageMe, "me", "",
		"your phone/email for recipient tracking (default: source identifier 'local')",
	)
	importImessageCmd.Flags().StringVar(
		&importImessageContacts, "contacts", "",
		"path to .vcf file used to backfill participant display names by phone/email",
	)
	importImessageCmd.Flags().StringVar(
		&importImessageMessagesRoot, "messages-root", "",
		"Messages folder to read attachments from (default ~/Library/Messages); rebase copied Mac paths here",
	)
	importImessageCmd.Flags().BoolVar(
		&importImessageNoAttachments, "no-attachments", false,
		"Do not store attachments to disk (messages and attachment metadata are still imported)",
	)
	importImessageCmd.Flags().Int64Var(
		&importImessageMaxMediaMB, "max-media-mb", 0,
		"maximum attachment size in MiB (0 = configured/default limit)",
	)
	rootCmd.AddCommand(importImessageCmd)
}
