package cmd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/mattn/go-isatty"
	"github.com/spf13/cobra"
	"github.com/wesm/msgvault/internal/gmail"
	"github.com/wesm/msgvault/internal/oauth"
	"github.com/wesm/msgvault/internal/store"
	"github.com/wesm/msgvault/internal/sync"
	"golang.org/x/oauth2"
)

var syncIncrementalCmd = &cobra.Command{
	Use:     "sync [email]",
	Aliases: []string{"sync-incremental"},
	Short:   "Sync new and changed messages from configured accounts",
	Long: `Perform an incremental synchronization using the Gmail History API.

This is faster than a full sync as it only fetches changes since the last sync.
Requires a prior full sync to establish the history ID baseline.

IMAP accounts do not support incremental sync, so they are automatically
synced using a full sync instead.

If no email is specified, syncs all accounts that have credentials configured.
Accounts without tokens or history IDs are skipped.

If history is too old (Gmail returns 404), falls back to suggesting a full sync.

Examples:
  msgvault sync                 # Sync all accounts
  msgvault sync you@gmail.com   # Sync specific account`,
	Args: cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		// Open database
		dbPath := cfg.DatabaseDSN()
		s, err := store.Open(dbPath)
		if err != nil {
			return fmt.Errorf("open database: %w", err)
		}
		defer func() { _ = s.Close() }()

		if err := s.InitSchema(); err != nil {
			return fmt.Errorf("init schema: %w", err)
		}
		if err := runStartupMigrations(s); err != nil {
			return fmt.Errorf("startup migrations: %w", err)
		}

		// Set up context with cancellation before any sync calls
		// so Ctrl+C always saves checkpoints.
		ctx, cancel := context.WithCancel(cmd.Context())
		defer cancel()

		sigChan := make(chan os.Signal, 1)
		signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
		go func() {
			<-sigChan
			fmt.Println("\nInterrupted. Saving checkpoint...")
			cancel()
		}()

		// Open vector backend (optional) so newly-ingested messages
		// are enqueued for embedding.
		vf, err := setupVectorFeatures(ctx, s.DB(), dbPath)
		if err != nil {
			return fmt.Errorf("vector features: %w", err)
		}
		defer func() {
			if vf != nil && vf.Close != nil {
				if closeErr := vf.Close(); closeErr != nil {
					logger.Warn("closing vectors.db failed", "error", closeErr)
				}
			}
		}()

		getOAuthMgr := oauthManagerCache()

		// Determine which accounts to sync.
		type syncTarget struct {
			source *store.Source
			email  string
		}
		var gmailTargets []syncTarget
		var imapTargets []*store.Source
		var syncErrors []string

		if len(args) == 1 {
			// Resolve all sources for the identifier and route
			// each by type, same as sync-full.
			allMatches, lookupErr := s.GetSourcesByIdentifierOrDisplayName(args[0])
			if lookupErr != nil {
				return fmt.Errorf("look up source: %w", lookupErr)
			}
			for _, src := range allMatches {
				switch src.SourceType {
				case "gmail":
					gmailTargets = append(gmailTargets, syncTarget{source: src, email: src.Identifier})
				case "imap":
					imapTargets = append(imapTargets, src)
				}
			}
			if len(gmailTargets) == 0 && len(imapTargets) == 0 {
				if len(allMatches) > 0 {
					return fmt.Errorf("account %q exists but its source type cannot be synced (only gmail and imap are supported)", args[0])
				}
				// Not in DB — assume Gmail (legacy behaviour)
				gmailTargets = []syncTarget{{email: args[0]}}
			}
		} else {
			// Discover all sources.
			allSources, err := s.ListSources("")
			if err != nil {
				return fmt.Errorf("list sources: %w", err)
			}
			if len(allSources) == 0 {
				return fmt.Errorf("no accounts configured - run 'add-account' or 'add-imap' first")
			}
			for _, src := range allSources {
				switch src.SourceType {
				case "gmail":
					if !cfg.OAuth.HasAnyConfig() {
						fmt.Printf("Skipping %s (OAuth not configured)\n", src.Identifier)
						continue
					}
					appName := sourceOAuthApp(src)
					if !src.SyncCursor.Valid || src.SyncCursor.String == "" {
						fmt.Printf("Skipping %s (no history ID - run 'sync-full' first)\n", src.Identifier)
						continue
					}
					// Service accounts are always ready
					if saKey := cfg.OAuth.ServiceAccountKeyFor(appName); saKey == "" {
						mgr, mgrErr := getOAuthMgr(appName)
						if mgrErr != nil {
							syncErrors = append(syncErrors, fmt.Sprintf("%s: %v", src.Identifier, mgrErr))
							continue
						}
						if !mgr.HasToken(src.Identifier) {
							fmt.Printf("Skipping %s (no OAuth token - run 'add-account' first)\n", src.Identifier)
							continue
						}
					}
					gmailTargets = append(gmailTargets, syncTarget{source: src, email: src.Identifier})
				case "imap":
					skipMsg, parseErr := imapSkipReason(src)
					if parseErr != nil {
						syncErrors = append(syncErrors, fmt.Sprintf("%s: malformed sync_config: %v", src.Identifier, parseErr))
						continue
					}
					if skipMsg != "" {
						fmt.Println(skipMsg)
						continue
					}
					imapTargets = append(imapTargets, src)
				default:
					continue
				}
			}
			if len(gmailTargets) == 0 && len(imapTargets) == 0 {
				if len(syncErrors) > 0 {
					// Surface the collected errors (e.g. broken OAuth config).
					return fmt.Errorf("%s", syncErrors[0])
				}
				return fmt.Errorf("no accounts are ready to sync")
			}
		}

		// Sync IMAP sources via full sync.
		for _, src := range imapTargets {
			if ctx.Err() != nil {
				break
			}
			fmt.Printf("Note: IMAP account %s does not support incremental sync. Running full sync.\n\n", src.Identifier)
			if err := runFullSync(ctx, s, getOAuthMgr, src, vf); err != nil {
				syncErrors = append(syncErrors, fmt.Sprintf("%s: %v", src.Identifier, err))
			}
		}

		// Sync Gmail sources via incremental sync.
		for _, target := range gmailTargets {
			if ctx.Err() != nil {
				break
			}
			if target.source == nil {
				syncErrors = append(syncErrors, fmt.Sprintf("%s: no source found - run 'sync-full' first", target.email))
				continue
			}
			if err := runIncrementalSync(ctx, s, getOAuthMgr, target.source, vf); err != nil {
				syncErrors = append(syncErrors, fmt.Sprintf("%s: %v", target.email, err))
				continue
			}
		}

		// Rebuild analytics cache.
		rebuildCacheAfterWrite(dbPath)

		if len(syncErrors) > 0 {
			fmt.Println()
			fmt.Println("Errors:")
			for _, e := range syncErrors {
				fmt.Printf("  %s\n", e)
			}
			return fmt.Errorf("%d account(s) failed to sync: %s",
				len(syncErrors), strings.Join(syncErrors, "; "))
		}

		return nil
	},
}

func runIncrementalSync(ctx context.Context, s *store.Store, getOAuthMgr func(string) (*oauth.Manager, error), source *store.Source, vf *vectorFeatures) error {
	if !source.SyncCursor.Valid || source.SyncCursor.String == "" {
		return fmt.Errorf("no history ID - run 'sync-full' first")
	}

	email := source.Identifier
	appName := sourceOAuthApp(source)
	var tokenSource oauth2.TokenSource
	var tsErr error

	if saKeyPath := cfg.OAuth.ServiceAccountKeyFor(appName); saKeyPath != "" {
		saMgr, saErr := oauth.NewServiceAccountManager(saKeyPath, oauth.Scopes)
		if saErr != nil {
			return fmt.Errorf("service account: %w", saErr)
		}
		tokenSource, tsErr = saMgr.TokenSource(ctx, email)
		if tsErr != nil {
			return tsErr
		}
	} else {
		oauthMgr, oaErr := getOAuthMgr(appName)
		if oaErr != nil {
			return oaErr
		}
		interactive := isatty.IsTerminal(os.Stdin.Fd()) ||
			isatty.IsCygwinTerminal(os.Stdin.Fd())
		tokenSource, tsErr = getTokenSourceWithReauth(ctx, oauthMgr, email, interactive)
		if tsErr != nil {
			return tsErr
		}
	}

	// Create Gmail client
	rateLimiter := gmail.NewRateLimiter(float64(cfg.Sync.RateLimitQPS))
	client := gmail.NewClient(tokenSource,
		gmail.WithLogger(logger),
		gmail.WithRateLimiter(rateLimiter),
	)
	defer func() { _ = client.Close() }()

	// Set up sync options
	opts := sync.DefaultOptions()
	opts.AttachmentsDir = cfg.AttachmentsDir()

	// Create syncer with progress reporter
	syncer := sync.New(client, s, opts).
		WithLogger(logger).
		WithProgress(&CLIProgress{})
	if vf != nil {
		syncer.SetEmbedEnqueuer(vf.Enqueuer)
	}

	// Run incremental sync
	startTime := time.Now()
	fmt.Printf("Starting incremental sync for %s\n", email)
	fmt.Printf("Last history ID: %s\n\n", source.SyncCursor.String)

	summary, err := syncer.Incremental(ctx, source)
	if err != nil {
		if ctx.Err() != nil {
			fmt.Println("\nSync interrupted. Run again to resume.")
			return nil
		}
		// Check for history expired error
		if errors.Is(err, sync.ErrHistoryExpired) {
			fmt.Println("\nHistory ID has expired. Gmail only keeps ~7 days of history.")
			fmt.Println("Run 'sync-full' to catch up on missed changes.")
			return nil
		}
		return fmt.Errorf("sync failed: %w", err)
	}

	// Print summary
	fmt.Println()
	fmt.Println("Sync complete!")
	fmt.Printf("  Duration:      %s\n", summary.Duration.Round(time.Second))
	fmt.Printf("  Changes:       %d processed, %d added\n",
		summary.MessagesFound, summary.MessagesAdded)
	fmt.Printf("  Downloaded:    %.2f MB\n", float64(summary.BytesDownloaded)/(1024*1024))
	if summary.Errors > 0 {
		fmt.Printf("  Errors:        %d\n", summary.Errors)
	}

	elapsed := time.Since(startTime)
	logger.Info("incremental sync completed",
		"email", email,
		"messages_added", summary.MessagesAdded,
		"elapsed", elapsed,
	)

	return nil
}

func init() {
	rootCmd.AddCommand(syncIncrementalCmd)
}
