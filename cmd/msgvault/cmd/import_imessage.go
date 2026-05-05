package cmd

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"github.com/wesm/msgvault/internal/imessage"
	"github.com/wesm/msgvault/internal/store"
)

var (
	importImessageDBPath string
	importImessageBefore string
	importImessageAfter  string
	importImessageLimit  int
	importImessageMe     string
)

var importImessageCmd = &cobra.Command{
	Use:   "import-imessage",
	Short: "Import iMessages from local database",
	Long: `Import iMessages from macOS's local Messages database (chat.db).

Reads messages from ~/Library/Messages/chat.db and stores them in the
msgvault archive. This is a read-only operation that does not modify
the iMessage database.

Requires Full Disk Access permission in System Settings > Privacy & Security.

Date filters:
  --after 2024-01-01     Only messages on or after this date
  --before 2024-12-31    Only messages before this date

Examples:
  msgvault import-imessage
  msgvault import-imessage --after 2024-01-01
  msgvault import-imessage --limit 100
  msgvault import-imessage --db-path /path/to/chat.db`,
	RunE: runImportImessage,
}

func runImportImessage(cmd *cobra.Command, _ []string) error {
	s, err := openStoreAndInitForIngest()
	if err != nil {
		return err
	}
	defer func() { _ = s.Close() }()

	chatDBPath, err := resolveChatDBPath()
	if err != nil {
		return err
	}

	clientOpts, err := buildImessageOpts()
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
	if err := runPostSourceCreateMigrations(s); err != nil {
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
			rebuildCacheAfterWrite(cfg.DatabaseDSN())
			return nil
		}
		return fmt.Errorf("import failed: %w", err)
	}

	printImessageSummary(summary, startTime)
	rebuildCacheAfterWrite(cfg.DatabaseDSN())
	return nil
}

func openStoreAndInit() (*store.Store, error) {
	// Shared by 15 commands (deduplicate, identity, collection,
	// import-imessage/gvoice, delete-deduped, …). store.Open + InitSchema
	// create the database file on first use, which is the right behavior
	// for a freshly-installed CLI: a missing file is not an error here.
	// init-db remains the explicit setup command for users who want to
	// pre-create the DB.
	return openStoreAndInitWith(runStartupMigrations)
}

// openStoreAndInitForIngest is the ingest-command variant of
// openStoreAndInit. It uses runStartupMigrationsForIngest so that, on a
// fresh install with [identity] addresses configured but no source yet,
// the misleading "migration will run on the next command" notice does
// not fire. The post-source-create migration call run by ingest commands
// after GetOrCreateSource emits the accurate "applied" notice once.
func openStoreAndInitForIngest() (*store.Store, error) {
	return openStoreAndInitWith(runStartupMigrationsForIngest)
}

func openStoreAndInitWith(migrate func(*store.Store) error) (*store.Store, error) {
	dbPath := cfg.DatabaseDSN()
	s, err := store.Open(dbPath)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	if err := s.InitSchema(); err != nil {
		_ = s.Close()
		return nil, fmt.Errorf("init schema: %w", err)
	}
	if err := migrate(s); err != nil {
		_ = s.Close()
		return nil, fmt.Errorf("startup migrations: %w", err)
	}
	return s, nil
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

func buildImessageOpts() ([]imessage.ClientOption, error) {
	var opts []imessage.ClientOption
	opts = append(opts, imessage.WithImessageLogger(logger))

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
	if summary.MessagesImported > 0 && elapsed.Seconds() > 0 {
		rate := float64(summary.MessagesImported) / elapsed.Seconds()
		fmt.Printf("  Rate:             %.1f messages/sec\n", rate)
	}
}

// resolveImessageSource finds or creates the apple_messages source.
// Among non-"local" sources, prefers the one with the highest ID
// (most recently created), which is more likely in active use.
func resolveImessageSource(s *store.Store) (*store.Source, error) {
	sources, err := s.ListSources("apple_messages")
	if err == nil && len(sources) > 0 {
		var best *store.Source
		for _, src := range sources {
			if src.Identifier != "local" {
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
	return s.GetOrCreateSource("apple_messages", "local")
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
	rootCmd.AddCommand(importImessageCmd)
}
